"use strict"

const fs = require("node:fs")
const os = require("node:os")
const path = require("node:path")
const { Schema } = require("effect")
const sdk = require("@opencode/plugin/gui")
const { createSignal, onCleanup, onMount } = require("solid-js")

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const WINDOWS = ["24h", "7d", "30d", "All"]
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
        modelsByWin: {
          "24h": new Map(),
          "7d": new Map(),
          "30d": new Map(),
          All: new Map(),
        },
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

          const map = acc.modelsByWin[w]
          const prev = map.get(mid) || { total: 0, turns: 0, speedTok: 0, speedSec: 0 }
          prev.total += tot
          prev.turns++
          if (sec > 0 && out > 0) {
            prev.speedTok += out
            prev.speedSec += sec
          }
          map.set(mid, prev)
        }
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
        let peakHour = 0
        for (let h = 1; h < 24; h++) if (acc.hours[h] > acc.hours[peakHour]) peakHour = h
        const modelsByWin = {}
        for (const w of WINDOWS) {
          const bw = acc.b[w]
          const grand = Math.max(1, bw.inTok + bw.outTok + bw.cacheTok)
          modelsByWin[w] = [...acc.modelsByWin[w].entries()]
            .sort((a, b) => b[1].total - a[1].total)
            .slice(0, 6)
            .map(([name, m]) => ({
              name,
              total: m.total,
              turns: m.turns,
              pct: Math.round((m.total / grand) * 100),
              tps: m.speedSec > 0 ? Math.round(m.speedTok / m.speedSec) : 0,
            }))
        }
        return {
          buckets: acc.b,
          cacheRatio:
            all.inTok + all.cacheTok > 0
              ? Math.round((all.cacheTok / (all.inTok + all.cacheTok)) * 100)
              : 0,
          avgTps: acc.speedSec > 0 ? (acc.speedTok / acc.speedSec).toFixed(1) : "-",
          streak: computeStreakDays(acc.days),
          peakHour,
          modelsByWin,
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

const SlotSchema = Schema.Struct({
  id: Schema.Number,
  active: Schema.Boolean,
  state: Schema.String,
  ip: Schema.String,
  colo: Schema.String,
  recycles: Schema.Number,
})

const StatusSchema = Schema.Struct({
  online: Schema.Boolean,
  paused: Schema.Boolean,
  version: Schema.String,
  ip: Schema.String,
  ready: Schema.Number,
  total: Schema.Number,
  rotations: Schema.Number,
  burnt: Schema.Number,
  slots: Schema.Array(SlotSchema),
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
    return {
      online: false,
      paused: false,
      version: "",
      ip: "offline",
      ready: 0,
      total: 0,
      rotations: 0,
      burnt: 0,
      slots: [],
    }
  }
  const rawSlots = Array.isArray(st.pool?.slots) ? st.pool.slots : []
  const slots = rawSlots.map((s) => ({
    id: Number(s.id ?? 0),
    active: Boolean(s.active),
    state: String(s.state || "unknown"),
    ip: shortenIPv6(String(s.ip || "-")),
    colo: String(s.colo || "-"),
    recycles: Number(s.recycles ?? 0),
  }))
  const ready = slots.filter((s) => s.state === "ready").length
  return {
    online: true,
    paused: Boolean(st.hop?.paused),
    version: String(st.version || ""),
    ip: shortenIPv6(st.pool?.active_ip || "?"),
    ready,
    total: slots.length,
    rotations: Number(st.hop?.count ?? 0),
    burnt: Number(st.pool?.burnt_count ?? 0),
    slots,
  }
}

function buildStatsElement(statsData, initialSummary, dialog, actions) {
  let summary = initialSummary
  let freeOnly = Boolean(statsData?.free?.buckets?.All?.turns > 0)
  let activeWin = "All"
  let showPool = false

  const root = document.createElement("div")
  root.tabIndex = -1
  root.style.cssText = [
    "background:var(--surface-raised-stronger-non-alpha,var(--background-base,#141417))",
    "border:1px solid var(--border-weak-base,rgba(255,255,255,0.12))",
    "border-radius:12px",
    "box-shadow:0 24px 64px rgba(0,0,0,0.65)",
    "padding:20px 24px",
    "width:650px",
    "max-width:calc(100vw - 48px)",
    "font-family:var(--font-mono,ui-monospace,monospace)",
    "font-size:13px",
    "color:var(--text-base,#d4d4d8)",
    "display:flex",
    "flex-direction:column",
    "gap:14px",
    "outline:none",
  ].join(";")

  const pillBtn = (text, active, onClick) => {
    const b = document.createElement("button")
    b.type = "button"
    b.textContent = text
    b.style.cssText = [
      "border-radius:6px",
      "padding:2px 9px",
      "cursor:pointer",
      "font:inherit",
      "font-size:12px",
      "outline:none",
      "transition:background 120ms ease,color 120ms ease,border-color 120ms ease",
      active
        ? "background:var(--surface-raised-base-hover,rgba(255,255,255,0.12));border:1px solid var(--border-base,rgba(255,255,255,0.22));color:var(--text-strong,#fff);font-weight:600;"
        : "background:transparent;border:1px solid transparent;color:var(--text-weak,rgba(255,255,255,0.55));",
    ].join(";")
    b.onmouseenter = () => {
      if (!active) b.style.background = "var(--surface-raised-base-hover,rgba(255,255,255,0.06))"
    }
    b.onmouseleave = () => {
      if (!active) b.style.background = "transparent"
    }
    b.onclick = onClick
    return b
  }

  const actionBtn = (text, onClick) => {
    const b = document.createElement("button")
    b.type = "button"
    b.textContent = text
    b.style.cssText = [
      "background:var(--surface-base,rgba(255,255,255,0.06))",
      "border:1px solid var(--border-weak-base,rgba(255,255,255,0.14))",
      "color:var(--text-strong,#e5e5e5)",
      "border-radius:6px",
      "padding:3px 10px",
      "cursor:pointer",
      "font:inherit",
      "font-size:12px",
      "outline:none",
      "transition:background 120ms ease",
    ].join(";")
    b.onmouseenter = () => {
      b.style.background = "var(--surface-raised-base-hover,rgba(255,255,255,0.12))"
    }
    b.onmouseleave = () => {
      b.style.background = "var(--surface-base,rgba(255,255,255,0.06))"
    }
    b.onclick = onClick
    return b
  }

  const onKeyDown = (e) => {
    if (e.key === "Tab") {
      e.preventDefault()
      const idx = WINDOWS.indexOf(activeWin)
      activeWin = WINDOWS[(idx + 1) % WINDOWS.length]
      render()
    } else if (e.key === "f" || e.key === "F") {
      e.preventDefault()
      freeOnly = !freeOnly
      render()
    }
  }
  // Prevent Kobalte's outside-click detector from closing the dialog when
  // re-rendering detaches the clicked child element.
  root.addEventListener("pointerdown", (e) => e.stopPropagation())
  root.addEventListener("mousedown", (e) => e.stopPropagation())
  root.addEventListener("click", (e) => e.stopPropagation())
  root.addEventListener("keydown", onKeyDown)

  const render = () => {
    root.innerHTML = ""

    // Header
    const header = document.createElement("div")
    header.style.cssText =
      "display:flex;justify-content:space-between;align-items:center;padding-bottom:10px;border-bottom:1px solid var(--border-weak-base,rgba(255,255,255,0.1));"

    const left = document.createElement("div")
    left.style.cssText = "display:flex;align-items:center;gap:8px;"
    const titleSpan = document.createElement("span")
    titleSpan.innerHTML = `<strong style="color:var(--text-strong,#fff)">UnlimitedCode</strong> <span style="color:var(--text-weak,rgba(255,255,255,0.5))">· Usage Stats</span>`
    left.appendChild(titleSpan)
    left.appendChild(
      pillBtn(freeOnly ? "Free" : "All", true, () => {
        freeOnly = !freeOnly
        render()
      }),
    )
    header.appendChild(left)

    const right = document.createElement("div")
    right.style.cssText = "display:flex;gap:3px;align-items:center;"
    for (const w of WINDOWS) {
      right.appendChild(
        pillBtn(w, activeWin === w, () => {
          activeWin = w
          render()
        }),
      )
    }
    const closeBtn = document.createElement("button")
    closeBtn.type = "button"
    closeBtn.textContent = "esc"
    closeBtn.style.cssText =
      "background:none;border:none;color:var(--text-weak,rgba(255,255,255,0.5));cursor:pointer;font:inherit;font-size:12px;margin-left:8px;outline:none;"
    closeBtn.onclick = () => dialog.close()
    right.appendChild(closeBtn)
    header.appendChild(right)
    root.appendChild(header)

    if (statsData?.error) {
      const err = document.createElement("div")
      err.style.cssText = "color:var(--text-weak);"
      err.textContent = statsData.error
      root.appendChild(err)
    } else {
      const sc = freeOnly ? statsData.free : statsData.all

      // Window table (clickable rows)
      const table = document.createElement("table")
      table.style.cssText = "width:100%;border-collapse:collapse;font-size:12.5px;"
      const thead = document.createElement("thead")
      thead.innerHTML = `
        <tr style="color:var(--text-weak,rgba(255,255,255,0.5));text-align:right;">
          <th style="text-align:left;font-weight:500;padding:0 6px 6px 6px;">Window</th>
          <th style="font-weight:500;padding-bottom:6px;">Turns</th>
          <th style="font-weight:500;padding-bottom:6px;">Input</th>
          <th style="font-weight:500;padding-bottom:6px;">Output</th>
          <th style="font-weight:500;padding-bottom:6px;">Cache</th>
          <th style="font-weight:500;padding-bottom:6px;">Total</th>
          <th style="font-weight:500;padding:0 6px 6px 0;">Est. Saved</th>
        </tr>
      `
      table.appendChild(thead)

      const tbody = document.createElement("tbody")
      for (const [k, lbl] of [
        ["24h", "24h"],
        ["7d", "7d"],
        ["30d", "30d"],
        ["All", "All time"],
      ]) {
        const b = sc.buckets[k]
        const tot = b.inTok + b.outTok + b.cacheTok
        const selected = activeWin === k
        const tr = document.createElement("tr")
        tr.style.cssText = [
          "text-align:right",
          "cursor:pointer",
          "transition:background 100ms ease",
          selected ? "background:var(--surface-raised-base-hover,rgba(255,255,255,0.06));" : "",
        ].join(";")
        tr.onmouseenter = () => {
          if (!selected) tr.style.background = "var(--surface-base,rgba(255,255,255,0.03))"
        }
        tr.onmouseleave = () => {
          if (!selected) tr.style.background = "transparent"
        }
        tr.onclick = () => {
          activeWin = k
          render()
        }
        tr.innerHTML = `
          <td style="text-align:left;padding:3px 6px;border-radius:6px 0 0 6px;color:${selected ? "var(--text-strong,#fff)" : "var(--text-weak,rgba(255,255,255,0.65))"};font-weight:${selected ? "600" : "400"};">${lbl}</td>
          <td style="color:var(--text-strong,#fff);">${b.turns.toLocaleString("en-US")}</td>
          <td style="color:var(--text-weak,rgba(255,255,255,0.6));">${fmtNum(b.inTok)}</td>
          <td style="color:var(--text-weak,rgba(255,255,255,0.6));">${fmtNum(b.outTok)}</td>
          <td style="color:var(--text-weak,rgba(255,255,255,0.6));">${fmtNum(b.cacheTok)}</td>
          <td style="color:var(--text-strong,#fff);"><strong>${fmtNum(tot)}</strong></td>
          <td style="padding:3px 6px 3px 0;border-radius:0 6px 6px 0;color:var(--icon-success-base,#4ade80);">${fmtSaved(b)}</td>
        `
        tbody.appendChild(tr)
      }
      table.appendChild(tbody)
      root.appendChild(table)

      // Summary strip
      const meta = document.createElement("div")
      meta.style.cssText =
        "display:flex;justify-content:space-between;font-size:12px;color:var(--text-weak,rgba(255,255,255,0.6));padding:0 6px;"
      const rotColor = summary.rotations > 0 ? "var(--icon-warning-base,#fbbf24)" : "var(--icon-success-base,#4ade80)"
      meta.innerHTML = `
        <span>Cache Hit <strong style="color:var(--text-strong,#fff)">${sc.cacheRatio}%</strong> · Avg <strong style="color:var(--text-strong,#fff)">${sc.avgTps} tok/s</strong> · Streak <strong style="color:var(--text-strong,#fff)">${sc.streak}d</strong> · Peak <strong style="color:var(--text-strong,#fff)">${String(sc.peakHour).padStart(2, "0")}:00</strong></span>
        <span>429s <strong style="color:${rotColor}">${summary.rotations}</strong></span>
      `
      root.appendChild(meta)

      // Top Models for selected window
      const modelsWrap = document.createElement("div")
      modelsWrap.style.cssText = "display:flex;flex-direction:column;gap:5px;padding:0 6px;"
      const mTitle = document.createElement("div")
      mTitle.style.cssText = "color:var(--text-weak,rgba(255,255,255,0.5));font-size:12px;"
      mTitle.textContent = `Top Models (${activeWin} · ${freeOnly ? "free" : "all"})`
      modelsWrap.appendChild(mTitle)

      const list = sc.modelsByWin?.[activeWin] || []
      if (list.length === 0) {
        const empty = document.createElement("div")
        empty.style.cssText = "color:var(--text-weak,rgba(255,255,255,0.45));font-size:12px;"
        empty.textContent = "No turns in this window."
        modelsWrap.appendChild(empty)
      } else {
        const maxTok = list[0]?.total || 1
        for (const m of list) {
          const row = document.createElement("div")
          row.style.cssText =
            "display:grid;grid-template-columns:1fr 120px 38px 72px 54px 66px;align-items:center;gap:8px;font-size:12px;"
          const pctWidth = Math.max(2, Math.round((m.total / maxTok) * 100))
          row.innerHTML = `
            <span style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text-strong,#fff);">${m.name}</span>
            <div style="height:4px;background:var(--surface-base,rgba(255,255,255,0.1));border-radius:2px;overflow:hidden;">
              <div style="height:100%;width:${pctWidth}%;background:var(--text-interactive-base,var(--icon-active,var(--text-strong,#e5e5e5)));"></div>
            </div>
            <span style="text-align:right;color:var(--text-strong,#fff);">${m.pct}%</span>
            <span style="text-align:right;color:var(--text-strong,#fff);">${fmtNum(m.total)}</span>
            <span style="text-align:right;color:var(--text-weak,rgba(255,255,255,0.55));">${m.turns}t</span>
            <span style="text-align:right;color:var(--text-weak,rgba(255,255,255,0.55));">${m.tps ? m.tps + " t/s" : "—"}</span>
          `
          modelsWrap.appendChild(row)
        }
      }
      root.appendChild(modelsWrap)
    }

    // Optional expandable Pool Slots section
    if (showPool && summary.slots?.length) {
      const poolBox = document.createElement("div")
      poolBox.style.cssText =
        "background:var(--surface-base,rgba(255,255,255,0.03));border:1px solid var(--border-weak-base,rgba(255,255,255,0.08));border-radius:8px;padding:10px 12px;display:flex;flex-direction:column;gap:4px;font-size:12px;"
      poolBox.innerHTML = `
        <div style="display:grid;grid-template-columns:70px 90px 1fr 60px 70px;color:var(--text-weak,rgba(255,255,255,0.5));padding-bottom:3px;">
          <span>Slot</span><span>State</span><span>IPv6 /64 Egress</span><span>PoP</span><span style="text-align:right;">Recycles</span>
        </div>
        ${summary.slots
          .map((sl) => {
            const stCol = sl.active
              ? "var(--icon-success-base,#4ade80)"
              : sl.state === "ready"
                ? "var(--text-base,#d4d4d8)"
                : "var(--icon-warning-base,#fbbf24)"
            const stText = sl.active ? "● active" : sl.state === "ready" ? "○ warm" : `◌ ${sl.state}`
            return `<div style="display:grid;grid-template-columns:70px 90px 1fr 60px 70px;">
              <span style="color:var(--text-strong,#fff);">slot-${sl.id}</span>
              <span style="color:${stCol};">${stText}</span>
              <span style="color:var(--text-base,#d4d4d8);">${sl.ip}</span>
              <span style="color:var(--text-weak,rgba(255,255,255,0.55));">${sl.colo}</span>
              <span style="text-align:right;color:var(--text-strong,#fff);">${sl.recycles}</span>
            </div>`
          })
          .join("")}
      `
      root.appendChild(poolBox)
    }

    // Footer
    const footer = document.createElement("div")
    footer.style.cssText =
      "display:flex;justify-content:space-between;align-items:center;padding-top:10px;border-top:1px solid var(--border-weak-base,rgba(255,255,255,0.1));font-size:12px;"
    const dotColor = !summary.online
      ? "var(--icon-critical-base,#f87171)"
      : summary.paused
        ? "var(--icon-warning-base,#fbbf24)"
        : "var(--icon-success-base,#4ade80)"
    const stLabel = !summary.online ? "OFFLINE" : summary.paused ? "PAUSED" : "AUTO"

    const fLeft = document.createElement("button")
    fLeft.type = "button"
    fLeft.style.cssText =
      "background:none;border:none;padding:2px 4px;margin:0;cursor:pointer;font:inherit;color:var(--text-weak,rgba(255,255,255,0.6));outline:none;border-radius:6px;transition:background 120ms ease;"
    fLeft.title = "Click to toggle MASQUE pool slots"
    fLeft.onmouseenter = () => {
      fLeft.style.background = "var(--surface-raised-base-hover,rgba(255,255,255,0.06))"
    }
    fLeft.onmouseleave = () => {
      fLeft.style.background = "transparent"
    }
    fLeft.onclick = () => {
      showPool = !showPool
      render()
    }
    fLeft.innerHTML = `<span style="color:${dotColor};font-weight:600;">Proxy ${stLabel}</span> · ${summary.ip} · pool ${summary.ready}/${summary.total} · burnt <span style="color:${summary.burnt > 0 ? "var(--icon-critical-base,#f87171)" : "var(--text-strong,#fff)"}">${summary.burnt}</span> ${showPool ? "▾" : "▸"}`
    footer.appendChild(fLeft)

    const fRight = document.createElement("div")
    fRight.style.cssText = "display:flex;gap:8px;"

    const rotBtn = actionBtn("Rotate IP", async () => {
      rotBtn.textContent = "Rotating…"
      const next = await actions.rotate()
      if (next) summary = next
      render()
    })
    fRight.appendChild(rotBtn)

    const pauseBtn = actionBtn(summary.paused ? "Resume" : "Pause", async () => {
      const next = await actions.togglePause()
      if (next) summary = next
      render()
    })
    fRight.appendChild(pauseBtn)

    footer.appendChild(fRight)
    root.appendChild(footer)
  }

  render()
  queueMicrotask(() => {
    try {
      root.focus()
    } catch {}
  })
  return root
}

exports.default = sdk.Plugin.define({
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
      version: "",
      ip: "…",
      ready: 0,
      total: 0,
      rotations: 0,
      burnt: 0,
      slots: [],
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

    ctx.add(sdk.Slot, {
      at: "session.header",
      order: 10,
      render() {
        const btn = document.createElement("button")
        btn.type = "button"
        btn.style.cssText = [
          "background:transparent",
          "border:none",
          "outline:none",
          "padding:2px 8px",
          "border-radius:6px",
          "cursor:pointer",
          "font-family:var(--font-sans,system-ui,sans-serif)",
          "font-size:12px",
          "font-weight:500",
          "letter-spacing:0.01em",
          "transition:background 120ms ease,color 120ms ease",
        ].join(";")

        const paint = () => {
          const s = summary()
          const color = !s.online
            ? "var(--icon-critical-base,#f87171)"
            : s.paused
              ? "var(--icon-warning-base,#fbbf24)"
              : "var(--icon-success-base,#4ade80)"
          const stLabel = !s.online ? "OFFLINE" : s.paused ? "PAUSED" : "AUTO"
          btn.textContent = "unlimited"
          btn.style.color = color
          btn.title = `UnlimitedCode · ${stLabel} · ${s.ip} · pool ${s.ready}/${s.total}`
        }

        btn.onmouseenter = () => {
          btn.style.background = "var(--surface-raised-base-hover,rgba(255,255,255,0.06))"
        }
        btn.onmouseleave = () => {
          btn.style.background = "transparent"
        }
        btn.onclick = () => {
          btn.blur()
          void openStatsDialog()
        }

        paint()
        const sync = setInterval(paint, 3000)
        onCleanup(() => clearInterval(sync))
        return btn
      },
    })

    ctx.add(sdk.Command, () => ({
      id: "stats",
      title: "UnlimitedCode: Usage Stats",
      description: "Free-tier tokens, speed, savings & top models",
      group: "UnlimitedCode",
      featured: true,
      suggested: true,
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
      featured: true,
      suggested: true,
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
