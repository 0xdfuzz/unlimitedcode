"use strict"

const fs = require("node:fs")
const os = require("node:os")
const path = require("node:path")
const { Schema } = require("effect")
const sdk = require("@opencode/plugin/gui")
const { createSignal, onCleanup, onMount } = require("solid-js")

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const RATE_INPUT_PER_M = 0.5
const RATE_OUTPUT_PER_M = 2.0
const RATE_CACHE_PER_M = 0.05

function stateHome() {
  if (process.env.UNLIMITEDCODE_HOME) return process.env.UNLIMITEDCODE_HOME
  if (process.platform === "win32" && process.env.LOCALAPPDATA) {
    return path.join(process.env.LOCALAPPDATA, "UnlimitedCode")
  }
  if (process.env.XDG_STATE_HOME) return path.join(process.env.XDG_STATE_HOME, "unlimitedcode")
  return path.join(os.homedir(), ".local", "state", "unlimitedcode")
}

function controlURL() {
  const raw = process.env.UNLIMITEDCODE_CONTROL || DEFAULT_CONTROL
  return raw.startsWith("http") ? raw.replace(/\/+$/, "") : `http://${raw}`
}

function controlToken() {
  try {
    return fs.readFileSync(path.join(stateHome(), "control.token"), "utf8").trim()
  } catch {
    return ""
  }
}

function shortenIPv6(ip) {
  if (!ip || !ip.includes(":")) return ip || "-"
  const parts = ip.split(":").filter((p) => p.length > 0)
  if (parts.length <= 4) return ip
  return `${parts[0]}:${parts[1]}::${parts[parts.length - 2]}:${parts[parts.length - 1]}`
}

function fmtNum(n) {
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(2) + "B"
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M"
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k"
  return String(Math.round(n))
}

function fmtSaved(b) {
  const usd =
    (b.inTok / 1_000_000) * RATE_INPUT_PER_M +
    (b.outTok / 1_000_000) * RATE_OUTPUT_PER_M +
    (b.cacheTok / 1_000_000) * RATE_CACHE_PER_M
  return "~$" + (usd < 0.01 ? "0.00" : usd.toFixed(2))
}

function isFreeEntry(parsed, modelId) {
  const low = modelId.toLowerCase()
  if (low.endsWith("-free") || low.includes(":free") || low === "big-pickle") return true
  if (typeof parsed?.cost === "number" && parsed.cost === 0) return true
  if (typeof parsed?.cost?.total === "number" && parsed.cost.total === 0) return true
  const prov = String(parsed?.providerID || parsed?.model?.providerID || "").toLowerCase()
  if (prov === "opencode" && !parsed?.cost) return true
  return false
}

function computeStreakDays(dayKeys) {
  let streak = 0
  const cursor = new Date()
  for (let i = 0; i < 365; i++) {
    const key = cursor.toISOString().slice(0, 10)
    if (dayKeys.has(key)) streak++
    else if (i > 0) break
    cursor.setDate(cursor.getDate() - 1)
  }
  return streak
}

function computeStatsJSON() {
  const dataDir = process.env.XDG_DATA_HOME
    ? path.join(process.env.XDG_DATA_HOME, "opencode")
    : path.join(os.homedir(), ".local", "share", "opencode")
  const dbPath = path.join(dataDir, "opencode.db")
  if (!fs.existsSync(dbPath)) {
    return JSON.stringify({ error: "No local OpenCode database found yet." })
  }
  try {
    const { DatabaseSync } = require("node:sqlite")
    const db = new DatabaseSync(dbPath, { readOnly: true })
    try {
      const now = Date.now()
      const dayAgo = now - 86_400_000
      const weekAgo = now - 7 * 86_400_000
      const monthAgo = now - 30 * 86_400_000
      const rows = db
        .prepare("SELECT time_created, data FROM session_message WHERE type = 'assistant'")
        .all()

      const makeAcc = () => ({
        b: {
          "24h": { inTok: 0, outTok: 0, cacheTok: 0, turns: 0 },
          "7d": { inTok: 0, outTok: 0, cacheTok: 0, turns: 0 },
          "30d": { inTok: 0, outTok: 0, cacheTok: 0, turns: 0 },
          All: { inTok: 0, outTok: 0, cacheTok: 0, turns: 0 },
        },
        models: new Map(),
        days: new Set(),
        hours: new Array(24).fill(0),
        speedTok: 0,
        speedSec: 0,
      })

      const add = (acc, wins, inp, out, cache, mid, dt, sec) => {
        const tot = inp + out + cache
        for (const w of wins) {
          const b = acc.b[w]
          b.inTok += inp
          b.outTok += out
          b.cacheTok += cache
          b.turns++
        }
        const prev = acc.models.get(mid) || { total: 0, turns: 0, speedTok: 0, speedSec: 0 }
        prev.total += tot
        prev.turns++
        if (sec > 0 && out > 0) {
          prev.speedTok += out
          prev.speedSec += sec
        }
        acc.models.set(mid, prev)
        acc.days.add(dt.toISOString().slice(0, 10))
        acc.hours[dt.getHours()]++
        if (sec > 0 && out > 0) {
          acc.speedTok += out
          acc.speedSec += sec
        }
      }

      const freeAcc = makeAcc()
      const allAcc = makeAcc()

      for (const row of rows) {
        let parsed
        try {
          parsed = JSON.parse(row.data)
        } catch {
          continue
        }
        const usage = parsed?.usage || parsed?.tokens
        if (!usage) continue
        const inp = Number(usage.input ?? usage.inputTokens ?? usage.promptTokens ?? 0)
        const out = Number(usage.output ?? usage.outputTokens ?? usage.completionTokens ?? 0)
        const cache = Number(usage.cache?.read ?? usage.cacheRead ?? usage.cachedInputTokens ?? 0)
        if (inp + out + cache <= 0) continue

        const wins = ["All"]
        if (row.time_created >= monthAgo) wins.push("30d")
        if (row.time_created >= weekAgo) wins.push("7d")
        if (row.time_created >= dayAgo) wins.push("24h")

        const dt = new Date(row.time_created)
        const started = Number(parsed?.time?.created ?? 0)
        const completed = Number(parsed?.time?.completed ?? 0)
        const sec =
          out > 0 && completed > started && completed - started < 300_000 ? (completed - started) / 1000 : 0
        const mid = String(parsed?.model?.id || parsed?.modelID || "unknown")

        add(allAcc, wins, inp, out, cache, mid, dt, sec)
        if (isFreeEntry(parsed, mid)) add(freeAcc, wins, inp, out, cache, mid, dt, sec)
      }

      const pack = (acc) => {
        const all = acc.b.All
        const grand = Math.max(1, all.inTok + all.outTok + all.cacheTok)
        let peakHour = 0
        for (let h = 1; h < 24; h++) if (acc.hours[h] > acc.hours[peakHour]) peakHour = h
        const models = [...acc.models.entries()]
          .sort((a, b) => b[1].total - a[1].total)
          .slice(0, 6)
          .map(([name, m]) => ({
            name,
            total: m.total,
            turns: m.turns,
            pct: Math.round((m.total / grand) * 100),
            tps: m.speedSec > 0 ? Math.round(m.speedTok / m.speedSec) : 0,
          }))
        return {
          buckets: acc.b,
          cacheRatio:
            all.inTok + all.cacheTok > 0
              ? Math.round((all.cacheTok / (all.inTok + all.cacheTok)) * 100)
              : 0,
          avgTps: acc.speedSec > 0 ? (acc.speedTok / acc.speedSec).toFixed(1) : "-",
          streak: computeStreakDays(acc.days),
          peakHour,
          models,
        }
      }

      return JSON.stringify({ free: pack(freeAcc), all: pack(allAcc) })
    } finally {
      db.close()
    }
  } catch (err) {
    return JSON.stringify({ error: String(err?.message || err) })
  }
}

async function controlCall(endpoint, method = "GET", body, timeoutMs = 8000) {
  const token = controlToken()
  if (!token) return null
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const res = await fetch(controlURL() + endpoint, {
      method,
      headers: {
        authorization: "Bearer " + token,
        ...(body !== undefined ? { "content-type": "application/json" } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })
    if (!res.ok) return null
    return await res.json().catch(() => null)
  } catch {
    return null
  } finally {
    clearTimeout(timer)
  }
}

const StatusSchema = Schema.Struct({
  online: Schema.Boolean,
  paused: Schema.Boolean,
  ip: Schema.String,
  ready: Schema.Number,
  total: Schema.Number,
  rotations: Schema.Number,
  burnt: Schema.Number,
})

const RotateResultSchema = Schema.Struct({
  ok: Schema.Boolean,
  ip: Schema.String,
  durationMs: Schema.Number,
})

const UnlimitedIpc = sdk.Ipc.define({
  id: "unlimitedcode.daemon",
  methods: {
    status: { output: StatusSchema },
    rotate: { output: RotateResultSchema },
    setPaused: { input: Schema.Boolean, output: StatusSchema },
    stats: { output: Schema.String },
  },
})

async function fetchSummary() {
  const st = await controlCall("/status", "GET", undefined, 2000)
  if (!st) {
    return { online: false, paused: false, ip: "offline", ready: 0, total: 0, rotations: 0, burnt: 0 }
  }
  const slots = Array.isArray(st.pool?.slots) ? st.pool.slots : []
  const ready = slots.filter((s) => s.state === "ready").length
  return {
    online: true,
    paused: Boolean(st.hop?.paused),
    ip: shortenIPv6(st.pool?.active_ip || "?"),
    ready,
    total: slots.length,
    rotations: Number(st.hop?.count ?? 0),
    burnt: Number(st.pool?.burnt_count ?? 0),
  }
}

const DOT_SVG =
  "<svg viewBox='0 0 16 16' fill='none' xmlns='http://www.w3.org/2000/svg'><circle cx='8' cy='8' r='4.5' fill='currentColor'/></svg>"

function buildStatsElement(statsData, summary, dialog, actions) {
  const root = document.createElement("div")
  root.style.cssText =
    "padding:20px 24px;min-width:560px;max-width:680px;font-family:var(--font-mono,ui-monospace,monospace);font-size:13px;color:var(--text-base,#e5e5e5);display:flex;flex-direction:column;gap:14px;"

  let freeOnly = Boolean(statsData?.free?.buckets?.All?.turns > 0)

  const render = () => {
    root.innerHTML = ""
    const header = document.createElement("div")
    header.style.cssText =
      "display:flex;justify-content:space-between;align-items:center;padding-bottom:10px;border-bottom:1px solid var(--border-base,rgba(255,255,255,0.12));"

    const left = document.createElement("div")
    left.innerHTML = `<strong>UnlimitedCode</strong> <span style="opacity:0.6">· Usage Stats</span>`
    header.appendChild(left)

    const right = document.createElement("div")
    right.style.cssText = "display:flex;gap:8px;align-items:center;"

    const scopeBtn = document.createElement("button")
    scopeBtn.textContent = freeOnly ? "Free" : "All"
    scopeBtn.style.cssText =
      "background:rgba(255,255,255,0.08);border:1px solid rgba(255,255,255,0.16);color:inherit;border-radius:6px;padding:2px 10px;cursor:pointer;font:inherit;font-size:12px;"
    scopeBtn.onclick = () => {
      freeOnly = !freeOnly
      render()
    }
    right.appendChild(scopeBtn)

    const closeBtn = document.createElement("button")
    closeBtn.textContent = "esc"
    closeBtn.style.cssText =
      "background:none;border:none;color:inherit;opacity:0.6;cursor:pointer;font:inherit;font-size:12px;"
    closeBtn.onclick = () => dialog.close()
    right.appendChild(closeBtn)

    header.appendChild(right)
    root.appendChild(header)

    if (statsData?.error) {
      const err = document.createElement("div")
      err.style.cssText = "opacity:0.75;"
      err.textContent = statsData.error
      root.appendChild(err)
    } else {
      const sc = freeOnly ? statsData.free : statsData.all
      const table = document.createElement("table")
      table.style.cssText = "width:100%;border-collapse:collapse;font-size:12.5px;"
      table.innerHTML = `
        <thead>
          <tr style="opacity:0.55;text-align:right;">
            <th style="text-align:left;font-weight:500;padding-bottom:6px;">Window</th>
            <th style="font-weight:500;padding-bottom:6px;">Turns</th>
            <th style="font-weight:500;padding-bottom:6px;">Input</th>
            <th style="font-weight:500;padding-bottom:6px;">Output</th>
            <th style="font-weight:500;padding-bottom:6px;">Cache</th>
            <th style="font-weight:500;padding-bottom:6px;">Total</th>
            <th style="font-weight:500;padding-bottom:6px;">Est. Saved</th>
          </tr>
        </thead>
        <tbody>
          ${[
            ["24h", "24h"],
            ["7d", "7d"],
            ["30d", "30d"],
            ["All", "All time"],
          ]
            .map(([k, lbl]) => {
              const b = sc.buckets[k]
              const tot = b.inTok + b.outTok + b.cacheTok
              return `<tr style="text-align:right;">
                <td style="text-align:left;opacity:0.7;padding:2px 0;">${lbl}</td>
                <td>${b.turns.toLocaleString("en-US")}</td>
                <td style="opacity:0.65;">${fmtNum(b.inTok)}</td>
                <td style="opacity:0.65;">${fmtNum(b.outTok)}</td>
                <td style="opacity:0.65;">${fmtNum(b.cacheTok)}</td>
                <td><strong>${fmtNum(tot)}</strong></td>
                <td style="color:#4ade80;">${fmtSaved(b)}</td>
              </tr>`
            })
            .join("")}
        </tbody>
      `
      root.appendChild(table)

      const meta = document.createElement("div")
      meta.style.cssText = "display:flex;justify-content:space-between;font-size:12px;opacity:0.85;"
      meta.innerHTML = `
        <span><span style="opacity:0.6">Cache Hit</span> ${sc.cacheRatio}% · <span style="opacity:0.6">Avg</span> ${sc.avgTps} tok/s · <span style="opacity:0.6">Streak</span> ${sc.streak}d · <span style="opacity:0.6">Peak</span> ${String(sc.peakHour).padStart(2, "0")}:00</span>
        <span><span style="opacity:0.6">429s</span> ${summary.rotations}</span>
      `
      root.appendChild(meta)

      const modelsWrap = document.createElement("div")
      modelsWrap.style.cssText = "display:flex;flex-direction:column;gap:5px;"
      const mTitle = document.createElement("div")
      mTitle.style.cssText = "opacity:0.55;font-size:12px;"
      mTitle.textContent = `Top Models (${freeOnly ? "free" : "all"})`
      modelsWrap.appendChild(mTitle)

      const maxTok = sc.models[0]?.total || 1
      for (const m of sc.models) {
        const row = document.createElement("div")
        row.style.cssText = "display:grid;grid-template-columns:1fr 110px 40px 72px 54px 66px;align-items:center;gap:8px;font-size:12px;"
        const pctWidth = Math.max(2, Math.round((m.total / maxTok) * 100))
        row.innerHTML = `
          <span style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">${m.name}</span>
          <div style="height:4px;background:rgba(255,255,255,0.12);border-radius:2px;overflow:hidden;">
            <div style="height:100%;width:${pctWidth}%;background:var(--accent,#a78bfa);"></div>
          </div>
          <span style="text-align:right;">${m.pct}%</span>
          <span style="text-align:right;">${fmtNum(m.total)}</span>
          <span style="text-align:right;opacity:0.65;">${m.turns}t</span>
          <span style="text-align:right;opacity:0.65;">${m.tps ? m.tps + " t/s" : "—"}</span>
        `
        modelsWrap.appendChild(row)
      }
      root.appendChild(modelsWrap)
    }

    const footer = document.createElement("div")
    footer.style.cssText =
      "display:flex;justify-content:space-between;align-items:center;padding-top:10px;border-top:1px solid var(--border-base,rgba(255,255,255,0.12));font-size:12px;"
    const dotColor = !summary.online ? "#f87171" : summary.paused ? "#fbbf24" : "#4ade80"
    const stLabel = !summary.online ? "OFFLINE" : summary.paused ? "PAUSED" : "AUTO"
    const fLeft = document.createElement("div")
    fLeft.innerHTML = `<span style="color:${dotColor}">●</span> Proxy ${stLabel} <span style="opacity:0.65">· ${summary.ip} · rot ${summary.rotations} · burnt ${summary.burnt}</span>`
    footer.appendChild(fLeft)

    const fRight = document.createElement("div")
    fRight.style.cssText = "display:flex;gap:8px;"

    const rotBtn = document.createElement("button")
    rotBtn.textContent = "Rotate IP"
    rotBtn.style.cssText =
      "background:rgba(255,255,255,0.08);border:1px solid rgba(255,255,255,0.16);color:inherit;border-radius:6px;padding:3px 10px;cursor:pointer;font:inherit;font-size:12px;"
    rotBtn.onclick = async () => {
      rotBtn.textContent = "Rotating…"
      const next = await actions.rotate()
      if (next) summary = next
      render()
    }
    fRight.appendChild(rotBtn)

    const pauseBtn = document.createElement("button")
    pauseBtn.textContent = summary.paused ? "Resume" : "Pause"
    pauseBtn.style.cssText =
      "background:rgba(255,255,255,0.08);border:1px solid rgba(255,255,255,0.16);color:inherit;border-radius:6px;padding:3px 10px;cursor:pointer;font:inherit;font-size:12px;"
    pauseBtn.onclick = async () => {
      const next = await actions.togglePause()
      if (next) summary = next
      render()
    }
    fRight.appendChild(pauseBtn)

    footer.appendChild(fRight)
    root.appendChild(footer)
  }

  render()
  return root
}

exports.default = sdk.Plugin.define({
  icons: {
    dot: DOT_SVG,
  },
  main(ctx) {
    ctx.provide(UnlimitedIpc, {
      async status() {
        return await fetchSummary()
      },
      async rotate() {
        const res = await controlCall("/rotate", "POST", {}, 55000)
        const summary = await fetchSummary()
        return {
          ok: res?.status === "ok",
          ip: summary.ip,
          durationMs: Number(res?.duration_ms ?? 0),
        }
      },
      async setPaused(paused) {
        await controlCall("/pause", "POST", { paused: Boolean(paused) }, 4000)
        return await fetchSummary()
      },
      async stats() {
        return computeStatsJSON()
      },
    })
  },
  setup(ctx) {
    const [summary, setSummary] = createSignal({
      online: true,
      paused: false,
      ip: "…",
      ready: 0,
      total: 0,
      rotations: 0,
      burnt: 0,
    })

    const refresh = async () => {
      try {
        const next = await ctx.main(UnlimitedIpc).status()
        if (next) setSummary(next)
        return next
      } catch {
        return null
      }
    }

    onMount(() => {
      void refresh()
      const timer = setInterval(() => {
        void refresh()
      }, 15000)
      onCleanup(() => clearInterval(timer))
    })

    const runRotate = async () => {
      try {
        const res = await ctx.main(UnlimitedIpc).rotate()
        const next = await refresh()
        ctx.toasts.show({
          title: "UnlimitedCode",
          description: res?.ok
            ? `Rotated in ${res.durationMs}ms → ${res.ip}`
            : "Rotate failed: daemon unreachable",
          variant: res?.ok ? "success" : "error",
          duration: 4000,
        })
        return next
      } catch {
        return null
      }
    }

    const togglePause = async () => {
      try {
        const next = await ctx.main(UnlimitedIpc).setPaused(!summary().paused)
        if (next) {
          setSummary(next)
          ctx.toasts.show({
            title: "UnlimitedCode",
            description: next.paused ? "Auto-rotate paused" : "Auto-rotate resumed",
            variant: "default",
            duration: 3000,
          })
        }
        return next
      } catch {
        return null
      }
    }

    const openStatsDialog = async () => {
      const [raw, cur] = await Promise.all([
        ctx.main(UnlimitedIpc).stats().catch(() => JSON.stringify({ error: "Unable to load stats." })),
        refresh(),
      ])
      let parsed
      try {
        parsed = JSON.parse(raw)
      } catch {
        parsed = { error: "Invalid stats payload." }
      }
      ctx.dialogs.open((dialog) =>
        buildStatsElement(parsed, cur || summary(), dialog, {
          rotate: runRotate,
          togglePause,
        }),
      )
    }

    ctx.add(sdk.TitlebarItem, () => {
      const s = summary()
      const stateLabel = !s.online ? "OFFLINE" : s.paused ? "PAUSED" : "AUTO"
      return {
        id: "unlimited",
        label: "unlimited",
        title: `UnlimitedCode · ${stateLabel} (${s.ip})`,
        icon: ctx.icons.dot,
        run() {
          void openStatsDialog()
        },
      }
    })

    ctx.add(sdk.ComposerPill, {
      id: "status",
      order: 50,
      show() {
        const s = summary()
        const tone = !s.online ? "danger" : s.paused ? "warning" : "success"
        const stateLabel = !s.online ? "OFFLINE" : s.paused ? "PAUSED" : "AUTO"
        return {
          label: "unlimited",
          icon: ctx.icons.dot,
          tone,
          title: `UnlimitedCode · ${stateLabel}`,
          rows: [
            {
              id: "stats",
              label: "Usage stats",
              detail: "tokens & savings",
              run() {
                void openStatsDialog()
              },
            },
            {
              id: "egress",
              label: "Egress IP",
              detail: s.online ? `${s.ip} (${s.ready}/${s.total})` : "offline",
              tone,
            },
            {
              id: "counters",
              label: "Rotations",
              detail: `${s.rotations} · burnt ${s.burnt}`,
            },
            {
              id: "rotate",
              label: "Rotate egress IP now",
              run() {
                void runRotate()
              },
            },
            {
              id: "pause",
              label: s.paused ? "Resume auto-rotate" : "Pause auto-rotate",
              run() {
                void togglePause()
              },
            },
          ],
        }
      },
    })

    ctx.add(sdk.Command, () => ({
      id: "stats",
      title: "UnlimitedCode: Usage Stats",
      description: "Free-tier tokens, speed, savings & top models",
      group: "UnlimitedCode",
      slash: { name: "unlimited-stats" },
      run() {
        void openStatsDialog()
      },
    }))

    ctx.add(sdk.Command, () => ({
      id: "menu",
      title: "UnlimitedCode: Status & Controls",
      description: "Open UnlimitedCode stats, IP rotation & pause controls",
      group: "UnlimitedCode",
      slash: { name: "unlimited" },
      run() {
        void openStatsDialog()
      },
    }))

    ctx.add(sdk.Command, () => ({
      id: "rotate",
      title: "UnlimitedCode: Rotate Egress IP",
      description: "Switch to a fresh IPv6 /64 MASQUE slot immediately",
      group: "UnlimitedCode",
      slash: { name: "rotate" },
      run() {
        void runRotate()
      },
    }))
  },
})
