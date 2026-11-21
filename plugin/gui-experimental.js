"use strict"

const { spawn } = require("node:child_process")
const fs = require("node:fs")
const os = require("node:os")
const path = require("node:path")
const { Schema } = require("effect")
const sdk = require("@opencode/plugin/gui")
const { createEffect, createSignal, onCleanup, onMount } = require("solid-js")

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const WINDOWS = ["24h", "7d", "30d", "All"]

let cachedControlURL = ""
let cachedControlToken = ""
let lastSeenUpdateTag = ""

function stateHome() {
  if (process.env.UNLIMITEDCODE_HOME) return process.env.UNLIMITEDCODE_HOME
  if (process.platform === "win32" && process.env.LOCALAPPDATA) {
    return path.join(process.env.LOCALAPPDATA, "UnlimitedCode")
  }
  if (process.env.XDG_STATE_HOME) return path.join(process.env.XDG_STATE_HOME, "unlimitedcode")
  return path.join(os.homedir(), ".local", "state", "unlimitedcode")
}

function readConfigJSON() {
  try {
    return JSON.parse(fs.readFileSync(path.join(stateHome(), "config.json"), "utf8"))
  } catch {
    return null
  }
}

function controlURL() {
  if (cachedControlURL) return cachedControlURL
  const raw = process.env.UNLIMITEDCODE_CONTROL || readConfigJSON()?.control || DEFAULT_CONTROL
  cachedControlURL = raw.startsWith("http") ? raw.replace(/\/+$/, "") : `http://${raw}`
  return cachedControlURL
}

function controlToken(forceReload = false) {
  if (!forceReload && cachedControlToken) return cachedControlToken
  try {
    cachedControlToken = fs.readFileSync(path.join(stateHome(), "control.token"), "utf8").trim()
  } catch {
    cachedControlToken = ""
  }
  return cachedControlToken
}

function installedBinary() {
  const name = process.platform === "win32" ? "unlimitedcode.exe" : "unlimitedcode"
  return path.join(stateHome(), name)
}

async function isDaemonHealthy() {
  try {
    const res = await fetch(controlURL() + "/health", { signal: AbortSignal.timeout(800) })
    return res.ok
  } catch {
    return false
  }
}

async function ensureDaemonRunning() {
  if (await isDaemonHealthy()) return true
  // Give the OpenCode server plugin a brief head start so it owns --parent-pid.
  await new Promise((r) => setTimeout(r, 300))
  if (await isDaemonHealthy()) return true
  const bin = installedBinary()
  if (!fs.existsSync(bin)) return false
  try {
    const childEnv = { ...process.env }
    delete childEnv.HTTPS_PROXY
    delete childEnv.HTTP_PROXY
    delete childEnv.https_proxy
    delete childEnv.http_proxy
    const child = spawn(bin, ["daemon", "--parent-pid", String(process.pid)], {
      detached: true,
      stdio: "ignore",
      windowsHide: true,
      env: childEnv,
    })
    child.unref()
  } catch {
    return false
  }
  const deadline = Date.now() + 20000
  while (Date.now() < deadline) {
    if (await isDaemonHealthy()) return true
    await new Promise((r) => setTimeout(r, 250))
  }
  return false
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

function installedSinceMs() {
  const home = stateHome()
  try {
    const raw = fs.readFileSync(path.join(home, "installed_at"), "utf8").trim()
    const ts = Number(raw)
    if (Number.isFinite(ts) && ts > 0) return ts
  } catch {}
  try {
    const st = fs.statSync(path.join(home, "config.json"))
    if (st.birthtimeMs > 0) return Math.floor(st.birthtimeMs)
    if (st.mtimeMs > 0) return Math.floor(st.mtimeMs)
  } catch {}
  return 0
}

function fmtSinceDate(ts) {
  if (!ts) return ""
  try {
    return new Date(ts).toLocaleDateString("en-US", { month: "short", day: "numeric" })
  } catch {
    return ""
  }
}

function isOpenCodeFree(parsed, modelId) {
  const prov = String(parsed?.providerID || parsed?.model?.providerID || "").toLowerCase()
  if (prov && prov !== "opencode") return false
  const low = modelId.toLowerCase()
  if (low.endsWith("-free") || low === "big-pickle") return true
  if (prov === "opencode") {
    if (typeof parsed?.cost === "number") return parsed.cost === 0
    if (typeof parsed?.cost?.total === "number") return parsed.cost.total === 0
    if (!parsed?.cost) return true
  }
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
  const sinceMs = installedSinceMs()
  const sinceLabel = fmtSinceDate(sinceMs)
  const dataDir = process.env.XDG_DATA_HOME
    ? path.join(process.env.XDG_DATA_HOME, "opencode")
    : path.join(os.homedir(), ".local", "share", "opencode")
  const dbPath = path.join(dataDir, "opencode.db")
  if (!fs.existsSync(dbPath)) {
    return JSON.stringify({ sinceLabel, error: "No local OpenCode database found yet." })
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
        .prepare("SELECT time_created, data FROM session_message WHERE type = 'assistant' AND time_created >= ?")
        .all(sinceMs)

      const acc = {
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
      }

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

        const mid = String(parsed?.model?.id || parsed?.modelID || "unknown")
        if (!isOpenCodeFree(parsed, mid)) continue

        const wins = ["All"]
        if (row.time_created >= monthAgo) wins.push("30d")
        if (row.time_created >= weekAgo) wins.push("7d")
        if (row.time_created >= dayAgo) wins.push("24h")

        const dt = new Date(row.time_created)
        const started = Number(parsed?.time?.created ?? 0)
        const completed = Number(parsed?.time?.completed ?? 0)
        const sec =
          out > 0 && completed > started && completed - started < 300_000 ? (completed - started) / 1000 : 0
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

      if (acc.b.All.turns === 0) {
        return JSON.stringify({
          sinceLabel,
          error: sinceLabel
            ? `No OpenCode free-tier turns recorded since ${sinceLabel}.`
            : "No OpenCode free-tier turns recorded yet.",
        })
      }

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
      return JSON.stringify({
        sinceLabel,
        buckets: acc.b,
        cacheRatio:
          all.inTok + all.cacheTok > 0
            ? Math.round((all.cacheTok / (all.inTok + all.cacheTok)) * 100)
            : 0,
        avgTps: acc.speedSec > 0 ? (acc.speedTok / acc.speedSec).toFixed(1) : "-",
        streak: computeStreakDays(acc.days),
        peakHour,
        modelsByWin,
      })
    } finally {
      db.close()
    }
  } catch (err) {
    return JSON.stringify({ sinceLabel, error: String(err?.message || err) })
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
    if (res.status === 401) {
      cachedControlToken = ""
      return null
    }
    if (!res.ok) return null
    return await res.json().catch(() => null)
  } catch {
    return null
  } finally {
    clearTimeout(timer)
  }
}

function readSkippedVersion() {
  try {
    return fs.readFileSync(path.join(stateHome(), "update_skip"), "utf8").trim()
  } catch {
    return ""
  }
}

function writeSkippedVersion(tag) {
  try {
    fs.writeFileSync(path.join(stateHome(), "update_skip"), String(tag || "").trim() + "\n")
  } catch {}
}

async function runUpdateBinary() {
  const bin = installedBinary()
  if (!fs.existsSync(bin)) {
    return { ok: false, message: "Installed binary not found (run unlimitedcode install)" }
  }
  const ok = await new Promise((resolve) => {
    try {
      const childEnv = { ...process.env }
      delete childEnv.HTTPS_PROXY
      delete childEnv.HTTP_PROXY
      delete childEnv.https_proxy
      delete childEnv.http_proxy
      const child = spawn(bin, ["update", "--yes"], {
        stdio: "ignore",
        windowsHide: true,
        env: childEnv,
      })
      child.on("close", (code) => resolve(code === 0))
      child.on("error", () => resolve(false))
    } catch {
      resolve(false)
    }
  })
  if (ok) {
    await ensureDaemonRunning()
    return { ok: true, message: "Updated! Restart OpenCode to reload the plugin." }
  }
  return { ok: false, message: 'Update failed — run "unlimitedcode update" in terminal.' }
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
  version: Schema.String,
  updateTag: Schema.String,
  promptUpdate: Schema.Boolean,
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

const UpdateResultSchema = Schema.Struct({
  ok: Schema.Boolean,
  message: Schema.String,
})

const UnlimitedIpc = sdk.Ipc.define({
  id: "unlimitedcode.daemon",
  methods: {
    status: { output: StatusSchema },
    rotate: { output: RotateResultSchema },
    restart: { output: StatusSchema },
    applyUpdate: { output: UpdateResultSchema },
    skipUpdate: { output: Schema.Boolean },
    stats: { output: Schema.String },
  },
})

async function fetchSummary() {
  let st = await controlCall("/status", "GET", undefined, 2000)
  if (!st) {
    if (await ensureDaemonRunning()) {
      st = await controlCall("/status", "GET", undefined, 2000)
    }
  }
  if (!st) {
    return {
      online: false,
      version: "",
      updateTag: "",
      promptUpdate: false,
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
  const version = String(st.version || "")
  const updateTag = String(st.update_available || "")
  lastSeenUpdateTag = updateTag
  const promptUpdate = Boolean(updateTag && updateTag !== readSkippedVersion())
  return {
    online: true,
    version,
    updateTag,
    promptUpdate,
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
  let activeWin = "All"
  let showPool = false

  const root = document.createElement("div")
  root.tabIndex = -1
  root.style.cssText = [
    "pointer-events:auto",
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
    const sinceText = statsData?.sinceLabel ? ` · Free Stats (since ${statsData.sinceLabel})` : " · Free Stats"
    titleSpan.innerHTML = `<strong style="color:var(--text-strong,#fff)">UnlimitedCode</strong><span style="color:var(--text-weak,rgba(255,255,255,0.5))">${sinceText}</span>`
    left.appendChild(titleSpan)
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
      const sc = statsData

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
          <th style="font-weight:500;padding:0 6px 6px 0;">Total</th>
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
          <td style="padding:3px 6px 3px 0;border-radius:0 6px 6px 0;color:var(--text-strong,#fff);"><strong>${fmtNum(tot)}</strong></td>
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

      // Top Free Models for selected window
      const modelsWrap = document.createElement("div")
      modelsWrap.style.cssText = "display:flex;flex-direction:column;gap:5px;padding:0 6px;"
      const mTitle = document.createElement("div")
      mTitle.style.cssText = "color:var(--text-weak,rgba(255,255,255,0.5));font-size:12px;"
      mTitle.textContent = `Top Free Models (${activeWin})`
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
      : "var(--icon-success-base,#4ade80)"
    const stLabel = !summary.online ? "OFFLINE" : "AUTO"

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

    if (summary.updateTag && actions.applyUpdate) {
      const updBtn = actionBtn(`Update ${summary.updateTag}`, async () => {
        updBtn.textContent = "Updating…"
        const next = await actions.applyUpdate()
        if (next) summary = next
        render()
      })
      updBtn.style.borderColor = "var(--icon-success-base,#4ade80)"
      fRight.appendChild(updBtn)
    }

    const rotBtn = actionBtn("Rotate IP", async () => {
      rotBtn.textContent = "Rotating…"
      const next = await actions.rotate()
      if (next) summary = next
      render()
    })
    fRight.appendChild(rotBtn)

    const restartBtn = actionBtn("Restart", async () => {
      restartBtn.textContent = "Restarting…"
      const next = await actions.restart()
      if (next) summary = next
      render()
    })
    fRight.appendChild(restartBtn)

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

function buildUpdatePromptElement(currentVersion, tag, dialog, actions) {
  const root = document.createElement("div")
  root.tabIndex = -1
  root.style.cssText = [
    "pointer-events:auto",
    "background:var(--surface-raised-stronger-non-alpha,var(--background-base,#141417))",
    "border:1px solid var(--border-weak-base,rgba(255,255,255,0.12))",
    "border-radius:12px",
    "box-shadow:0 24px 64px rgba(0,0,0,0.65)",
    "padding:20px 22px",
    "width:420px",
    "max-width:calc(100vw - 48px)",
    "font-family:var(--font-sans,system-ui,sans-serif)",
    "font-size:13px",
    "color:var(--text-base,#d4d4d8)",
    "display:flex",
    "flex-direction:column",
    "gap:14px",
    "outline:none",
  ].join(";")
  root.addEventListener("pointerdown", (e) => e.stopPropagation())
  root.addEventListener("mousedown", (e) => e.stopPropagation())
  root.addEventListener("click", (e) => e.stopPropagation())

  const title = document.createElement("div")
  title.style.cssText = "font-weight:600;font-size:14px;color:var(--text-strong,#fff);"
  title.textContent = "UnlimitedCode update available"
  root.appendChild(title)

  const body = document.createElement("div")
  body.style.cssText = "color:var(--text-weak,rgba(255,255,255,0.7));line-height:1.45;"
  body.textContent = `v${currentVersion || "?"} → ${tag} is ready. Download, verify SHA-256, and install now?`
  root.appendChild(body)

  const mkBtn = (label, primary, onClick) => {
    const b = document.createElement("button")
    b.type = "button"
    b.textContent = label
    b.style.cssText = [
      "border-radius:6px",
      "padding:6px 12px",
      "cursor:pointer",
      "font:inherit",
      "font-size:12.5px",
      "outline:none",
      primary
        ? "background:var(--surface-raised-base-hover,rgba(255,255,255,0.14));border:1px solid var(--icon-success-base,#4ade80);color:var(--text-strong,#fff);font-weight:600;"
        : "background:transparent;border:1px solid var(--border-weak-base,rgba(255,255,255,0.14));color:var(--text-weak,rgba(255,255,255,0.7));",
    ].join(";")
    b.onclick = onClick
    return b
  }

  const row = document.createElement("div")
  row.style.cssText = "display:flex;justify-content:flex-end;gap:8px;padding-top:4px;"

  const skipBtn = mkBtn(`Skip ${tag}`, false, async () => {
    await actions.skip()
    dialog.close()
  })
  const laterBtn = mkBtn("Later", false, () => dialog.close())
  const installBtn = mkBtn("Update now", true, async () => {
    installBtn.textContent = "Updating…"
    installBtn.disabled = true
    await actions.applyUpdate()
    dialog.close()
  })
  row.appendChild(skipBtn)
  row.appendChild(laterBtn)
  row.appendChild(installBtn)
  root.appendChild(row)
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
      async restart() {
        await controlCall("/shutdown", "POST", {}, 2000)
        const deadline = Date.now() + 2500
        while (Date.now() < deadline) {
          if (!(await isDaemonHealthy())) break
          await new Promise((r) => setTimeout(r, 150))
        }
        await ensureDaemonRunning()
        return await fetchSummary()
      },
      async applyUpdate() {
        return await runUpdateBinary()
      },
      async skipUpdate() {
        if (lastSeenUpdateTag) writeSkippedVersion(lastSeenUpdateTag)
        return true
      },
      async stats() {
        return computeStatsJSON()
      },
    })
  },
  setup(ctx) {
    const [summary, setSummary] = createSignal({
      online: true,
      version: "",
      updateTag: "",
      promptUpdate: false,
      ip: "…",
      ready: 0,
      total: 0,
      rotations: 0,
      burnt: 0,
      slots: [],
    })
    let promptedUpdate = false

    const refresh = async () => {
      try {
        const next = await ctx.main(UnlimitedIpc).status()
        if (next) setSummary(next)
        return next
      } catch {
        return null
      }
    }

    const maybePromptUpdate = (st) => {
      if (promptedUpdate || !st?.promptUpdate || !st?.updateTag) return
      promptedUpdate = true
      ctx.dialogs.open((dialog) =>
        buildUpdatePromptElement(st.version, st.updateTag, dialog, {
          applyUpdate: runApplyUpdate,
          skip: async () => {
            await ctx.main(UnlimitedIpc).skipUpdate().catch(() => {})
          },
        }),
      )
    }

    onMount(() => {
      let followup
      void (async () => {
        const st = await refresh()
        if (st?.promptUpdate) {
          maybePromptUpdate(st)
        } else {
          // Wait briefly for the daemon's one-shot startup check; never poll or interrupt mid-session.
          followup = setTimeout(() => {
            void refresh().then((next) => {
              if (next?.promptUpdate) maybePromptUpdate(next)
            })
          }, 2500)
        }
      })()
      onCleanup(() => {
        if (followup) clearTimeout(followup)
      })
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

    const runRestart = async () => {
      try {
        const next = await ctx.main(UnlimitedIpc).restart()
        if (next) {
          setSummary(next)
          ctx.toasts.show({
            title: "UnlimitedCode",
            description: next.online
              ? `Daemon online · ${next.ip}`
              : "Daemon failed to start",
            variant: next.online ? "success" : "error",
            duration: 4000,
          })
        }
        return next
      } catch {
        return null
      }
    }

    const runApplyUpdate = async () => {
      try {
        const res = await ctx.main(UnlimitedIpc).applyUpdate()
        ctx.toasts.show({
          title: "UnlimitedCode",
          description: res?.message || "Update finished.",
          variant: res?.ok ? "success" : "error",
          duration: 6000,
        })
        return await refresh()
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
          restart: runRestart,
          applyUpdate: runApplyUpdate,
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
            : "var(--icon-success-base,#4ade80)"
          const stLabel = !s.online ? "OFFLINE" : "AUTO"
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

        createEffect(paint)
        return btn
      },
    })

    ctx.add(sdk.Command, () => ({
      id: "menu",
      title: "UnlimitedCode",
      description: "Free usage stats, MASQUE pool status, IP rotation & daemon controls",
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
