import { spawn } from "node:child_process"
import { existsSync, readFileSync, statSync, writeFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js"

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const ROTATE_TIMEOUT_MS = 55_000
const RELEASES_URL = "https://api.github.com/repos/0xdfuzz/unlimitedcode/releases/latest"
const UPDATE_CHECK_TTL_MS = 24 * 60 * 60 * 1000

const WINDOWS = ["24h", "7d", "30d", "All"] as const
type WindowKey = (typeof WINDOWS)[number]

type Bucket = {
  inTok: number
  outTok: number
  cacheTok: number
  turns: number
}

type ModelAgg = {
  total: number
  turns: number
  speedTok: number
  speedSec: number
}

type FullStats = {
  buckets: Record<WindowKey, Bucket>
  byModel: Record<WindowKey, Map<string, ModelAgg>>
  cacheRatio: number
  avgTps: string
  streak: number
  peakHour: number
  sinceLabel: string
  error?: string
}

function stateHome(): string {
  if (process.env.UNLIMITEDCODE_HOME) return process.env.UNLIMITEDCODE_HOME
  if (process.platform === "win32" && process.env.LOCALAPPDATA) {
    return join(process.env.LOCALAPPDATA, "UnlimitedCode")
  }
  if (process.env.XDG_STATE_HOME) return join(process.env.XDG_STATE_HOME, "unlimitedcode")
  return join(homedir(), ".local", "state", "unlimitedcode")
}

function readConfigJSON(): any {
  try {
    return JSON.parse(readFileSync(join(stateHome(), "config.json"), "utf8"))
  } catch {
    return null
  }
}

function controlURL(): string {
  const raw = process.env.UNLIMITEDCODE_CONTROL || readConfigJSON()?.control || DEFAULT_CONTROL
  return raw.startsWith("http") ? raw.replace(/\/+$/, "") : `http://${raw}`
}

function controlToken(): string {
  try {
    return readFileSync(join(stateHome(), "control.token"), "utf8").trim()
  } catch {
    return ""
  }
}

function installedBinary(): string {
  const name = process.platform === "win32" ? "unlimitedcode.exe" : "unlimitedcode"
  return join(stateHome(), name)
}

async function isDaemonHealthy(): Promise<boolean> {
  try {
    const res = await fetch(controlURL() + "/health", { signal: AbortSignal.timeout(800) })
    return res.ok
  } catch {
    return false
  }
}

async function ensureDaemonRunning(): Promise<boolean> {
  if (await isDaemonHealthy()) return true
  const bin = installedBinary()
  if (!existsSync(bin)) return false
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
  const deadline = Date.now() + 20_000
  while (Date.now() < deadline) {
    if (await isDaemonHealthy()) return true
    await new Promise((r) => setTimeout(r, 250))
  }
  return false
}

async function controlCall(path: string, method = "GET", body?: any, timeoutMs = 8_000): Promise<any | null> {
  let token = controlToken()
  if (!token) {
    if (await ensureDaemonRunning()) {
      token = controlToken()
    }
    if (!token) return null
  }
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const res = await fetch(controlURL() + path, {
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

function fmtNum(n: number): string {
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(2) + "B"
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M"
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k"
  return String(Math.round(n))
}

function fmtTurns(n: number): string {
  return n.toLocaleString("en-US")
}

function installedSinceMs(): number {
  const home = stateHome()
  try {
    const raw = readFileSync(join(home, "installed_at"), "utf8").trim()
    const ts = Number(raw)
    if (Number.isFinite(ts) && ts > 0) return ts
  } catch {}
  try {
    const st = statSync(join(home, "config.json"))
    if (st.birthtimeMs > 0) return Math.floor(st.birthtimeMs)
    if (st.mtimeMs > 0) return Math.floor(st.mtimeMs)
  } catch {}
  return 0
}

function fmtSinceDate(ts: number): string {
  if (!ts) return ""
  try {
    return new Date(ts).toLocaleDateString("en-US", { month: "short", day: "numeric" })
  } catch {
    return ""
  }
}

function padR(s: string, n: number): string {
  return s.length >= n ? s.slice(0, n) : s + " ".repeat(n - s.length)
}

function padL(s: string, n: number): string {
  return s.length >= n ? s : " ".repeat(n - s.length) + s
}

// Shortens full IPv6 addresses for clean display & screenshot privacy:
// "2a09:bac5:b91:37cd::58f:34" -> "2a09:bac5::58f:34"
function shortenIPv6(ip: string): string {
  if (!ip || !ip.includes(":")) return ip || "-"
  const parts = ip.split(":").filter((p) => p.length > 0)
  if (parts.length <= 4) return ip
  return `${parts[0]}:${parts[1]}::${parts[parts.length - 2]}:${parts[parts.length - 1]}`
}

// Strict OpenCode free-tier filter: only provider "opencode" models with zero cost.
function isOpenCodeFree(parsed: any, modelId: string): boolean {
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

function computeStreakDays(dayKeys: Set<string>): number {
  let streak = 0
  const cursor = new Date()
  for (let i = 0; i < 365; i++) {
    const key = cursor.toISOString().slice(0, 10)
    if (dayKeys.has(key)) {
      streak++
    } else if (i > 0) {
      break
    }
    cursor.setDate(cursor.getDate() - 1)
  }
  return streak
}

function createScopeAcc() {
  const empty = (): Bucket => ({ inTok: 0, outTok: 0, cacheTok: 0, turns: 0 })
  return {
    buckets: {
      "24h": empty(),
      "7d": empty(),
      "30d": empty(),
      All: empty(),
    } as Record<WindowKey, Bucket>,
    byModel: {
      "24h": new Map<string, ModelAgg>(),
      "7d": new Map<string, ModelAgg>(),
      "30d": new Map<string, ModelAgg>(),
      All: new Map<string, ModelAgg>(),
    } as Record<WindowKey, Map<string, ModelAgg>>,
    activeDays: new Set<string>(),
    hourCounts: new Array<number>(24).fill(0),
    speedTok: 0,
    speedSec: 0,
  }
}

function addToScope(
  acc: ReturnType<typeof createScopeAcc>,
  windows: WindowKey[],
  inp: number,
  out: number,
  cache: number,
  mid: string,
  dt: Date,
  turnSpeedSec: number,
) {
  const total = inp + out + cache
  for (const w of windows) {
    const b = acc.buckets[w]
    b.inTok += inp
    b.outTok += out
    b.cacheTok += cache
    b.turns++

    const map = acc.byModel[w]
    const prev = map.get(mid) || { total: 0, turns: 0, speedTok: 0, speedSec: 0 }
    prev.total += total
    prev.turns++
    if (turnSpeedSec > 0 && out > 0) {
      prev.speedTok += out
      prev.speedSec += turnSpeedSec
    }
    map.set(mid, prev)
  }
  acc.activeDays.add(dt.toISOString().slice(0, 10))
  acc.hourCounts[dt.getHours()]++
  if (turnSpeedSec > 0 && out > 0) {
    acc.speedTok += out
    acc.speedSec += turnSpeedSec
  }
}

function finalizeScope(acc: ReturnType<typeof createScopeAcc>, sinceLabel: string): FullStats {
  const all = acc.buckets.All
  const cacheRatio =
    all.inTok + all.cacheTok > 0 ? Math.round((all.cacheTok / (all.inTok + all.cacheTok)) * 100) : 0
  const avgTps = acc.speedSec > 0 ? (acc.speedTok / acc.speedSec).toFixed(1) : "-"
  let peakHour = 0
  for (let h = 1; h < 24; h++) {
    if (acc.hourCounts[h] > acc.hourCounts[peakHour]) peakHour = h
  }
  return {
    buckets: acc.buckets,
    byModel: acc.byModel,
    cacheRatio,
    avgTps,
    streak: computeStreakDays(acc.activeDays),
    peakHour,
    sinceLabel,
  }
}

async function loadTokenStats(): Promise<FullStats> {
  const sinceMs = installedSinceMs()
  const sinceLabel = fmtSinceDate(sinceMs)
  const emptyStats = (error: string): FullStats => ({
    ...finalizeScope(createScopeAcc(), sinceLabel),
    error,
  })

  const dataDir = process.env.XDG_DATA_HOME
    ? join(process.env.XDG_DATA_HOME, "opencode")
    : join(homedir(), ".local", "share", "opencode")
  const dbPath = join(dataDir, "opencode.db")
  if (!existsSync(dbPath)) {
    return emptyStats("No local OpenCode database found yet.")
  }

  try {
    const mod: any = await import("bun:sqlite")
    const Database = mod.Database || mod.default
    if (!Database) {
      return emptyStats("bun:sqlite is unavailable in this runtime.")
    }

    const db = new Database(dbPath, { readonly: true })
    try {
      const now = Date.now()
      const dayAgo = now - 86_400_000
      const weekAgo = now - 7 * 86_400_000
      const monthAgo = now - 30 * 86_400_000

      const rows = db
        .query("SELECT time_created, data FROM session_message WHERE type = 'assistant' AND time_created >= ?")
        .all(sinceMs) as Array<{ time_created: number; data: string }>

      const freeAcc = createScopeAcc()

      for (const row of rows) {
        let parsed: any
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

        const windows: WindowKey[] = ["All"]
        if (row.time_created >= monthAgo) windows.push("30d")
        if (row.time_created >= weekAgo) windows.push("7d")
        if (row.time_created >= dayAgo) windows.push("24h")

        const dt = new Date(row.time_created)
        const started = Number(parsed?.time?.created ?? 0)
        const completed = Number(parsed?.time?.completed ?? 0)
        const turnSpeedSec =
          out > 0 && completed > started && completed - started < 300_000 ? (completed - started) / 1000 : 0

        addToScope(freeAcc, windows, inp, out, cache, mid, dt, turnSpeedSec)
      }

      if (freeAcc.buckets.All.turns === 0) {
        return emptyStats(
          sinceLabel
            ? `No OpenCode free-tier turns recorded since ${sinceLabel}.`
            : "No OpenCode free-tier turns recorded yet.",
        )
      }

      return finalizeScope(freeAcc, sinceLabel)
    } finally {
      db.close()
    }
  } catch (err: any) {
    return emptyStats(`Failed to read local DB: ${err?.message || err}`)
  }
}

function parseSemver(raw: string): [number, number, number] | null {
  const clean = raw.trim().replace(/^v/i, "").split(/[-+]/)[0]
  const parts = clean.split(".").map((n) => Number(n))
  if (parts.length < 2 || parts.length > 3 || parts.some((n) => !Number.isInteger(n) || n < 0)) {
    return null
  }
  return [parts[0] || 0, parts[1] || 0, parts[2] || 0]
}

function isNewerVersion(current: string, candidate: string): boolean {
  const c = parseSemver(current)
  const n = parseSemver(candidate)
  if (!c || !n) return false
  for (let i = 0; i < 3; i++) {
    if (n[i] !== c[i]) return n[i] > c[i]
  }
  return false
}

async function checkAvailableUpdate(currentVersion: string): Promise<string> {
  if (!currentVersion) return ""
  const cachePath = join(stateHome(), "update_check.json")
  const now = Date.now()
  try {
    const cached = JSON.parse(readFileSync(cachePath, "utf8"))
    if (cached && now - Number(cached.checked_at || 0) < UPDATE_CHECK_TTL_MS) {
      const tag = String(cached.tag || "")
      return tag && isNewerVersion(currentVersion, tag) ? tag : ""
    }
  } catch {}
  try {
    const res = await fetch(RELEASES_URL, {
      headers: { accept: "application/vnd.github+json", "user-agent": `unlimitedcode/${currentVersion}` },
      signal: AbortSignal.timeout(4_000),
    })
    if (!res.ok) return ""
    const data: any = await res.json().catch(() => null)
    const tag = String(data?.tag_name || "").trim()
    const version = tag.replace(/^v/i, "")
    try {
      writeFileSync(
        cachePath,
        JSON.stringify({ checked_at: now, tag, version, html_url: String(data?.html_url || "") }) + "\n",
      )
    } catch {}
    return tag && isNewerVersion(currentVersion, tag) ? tag : ""
  } catch {
    return ""
  }
}

async function doApplyUpdate(context: any, tag: string): Promise<void> {
  const bin = installedBinary()
  if (!existsSync(bin)) {
    context.ui.toast.show({
      title: "UnlimitedCode",
      message: "Installed binary not found (run unlimitedcode install)",
      variant: "error",
      duration: 4500,
    })
    return
  }
  context.ui.toast.show({
    title: "UnlimitedCode",
    message: `Updating to ${tag}…`,
    variant: "info",
    duration: 4000,
  })
  const ok = await new Promise<boolean>((resolve) => {
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
    context.ui.toast.show({
      title: "UnlimitedCode",
      message: `Updated to ${tag} — restart OpenCode to reload UI`,
      variant: "success",
      duration: 6000,
    })
  } else {
    context.ui.toast.show({
      title: "UnlimitedCode",
      message: `Update failed — run "unlimitedcode update" in terminal`,
      variant: "error",
      duration: 5000,
    })
  }
}

async function doRotate(context: any): Promise<any | null> {
  context.ui.toast.show({
    title: "UnlimitedCode",
    message: "Rotating egress IP…",
    variant: "info",
    duration: 2000,
  })
  const res = await controlCall("/rotate", "POST", {}, ROTATE_TIMEOUT_MS)
  const st = await controlCall("/status", "GET", undefined, 2_000)
  const ip = shortenIPv6(st?.pool?.active_ip || "?")
  if (res?.status === "ok") {
    context.ui.toast.show({
      title: "UnlimitedCode",
      message: `Rotated in ${res.duration_ms ?? 0}ms → ${ip}`,
      variant: "success",
      duration: 4000,
    })
  } else {
    context.ui.toast.show({
      title: "UnlimitedCode",
      message: `Rotate failed: ${res?.error || "daemon unreachable"}`,
      variant: "error",
      duration: 4500,
    })
  }
  return st
}

async function doRestartDaemon(context: any): Promise<any | null> {
  context.ui.toast.show({
    title: "UnlimitedCode",
    message: "Restarting daemon…",
    variant: "info",
    duration: 2500,
  })
  await controlCall("/shutdown", "POST", {}, 2_000)
  const stopDeadline = Date.now() + 2_500
  while (Date.now() < stopDeadline) {
    if (!(await isDaemonHealthy())) break
    await new Promise((r) => setTimeout(r, 150))
  }
  const ok = await ensureDaemonRunning()
  const st = ok ? await controlCall("/status", "GET", undefined, 2_000) : null
  if (st) {
    context.ui.toast.show({
      title: "UnlimitedCode",
      message: `Daemon online · ${shortenIPv6(st.pool?.active_ip || "?")}`,
      variant: "success",
      duration: 4000,
    })
  } else {
    context.ui.toast.show({
      title: "UnlimitedCode",
      message: "Daemon failed to start (run unlimitedcode doctor)",
      variant: "error",
      duration: 4500,
    })
  }
  return st
}

function themeColors(context: any) {
  const root = context.theme
  const surface = typeof root?.surface === "function" ? root.surface("dialog") : root
  const accent =
    root?.categorical?.[0]?.[200] ??
    surface?.text?.action?.primary?.base ??
    surface?.text?.base
  return {
    base: surface.text.base,
    muted: surface.text.muted,
    accent,
    border: surface.border?.base ?? surface.text.muted,
    ok: surface.text.feedback.success.base,
    warn: surface.text.feedback.warning.base,
    err: surface.text.feedback.error.base,
  }
}

function StatsDialog(props: { context: any; stats: FullStats; initialStatus: any }) {
  const [winIdx, setWinIdx] = createSignal(3) // Default: All

  const activeWindow = createMemo(() => WINDOWS[winIdx()])

  const topModels = createMemo(() => {
    const w = activeWindow()
    const map = props.stats.byModel[w]
    const b = props.stats.buckets[w]
    const grandTotal = Math.max(1, b.inTok + b.outTok + b.cacheTok)
    const entries = [...map.entries()].sort((a, b2) => b2[1].total - a[1].total).slice(0, 6)
    const maxTok = entries[0]?.[1].total || 1
    return entries.map(([name, info]) => {
      const pct = Math.round((info.total / grandTotal) * 100)
      const filled = Math.max(0, Math.min(14, Math.round((info.total / maxTok) * 14)))
      const tps = info.speedSec > 0 ? `${Math.round(info.speedTok / info.speedSec)} tok/s` : "—"
      return {
        name,
        filled,
        empty: 14 - filled,
        pct,
        total: fmtNum(info.total),
        turns: `${info.turns}t`,
        tps,
      }
    })
  })

  props.context.keymap.layer(() => ({
    mode: "modal",
    commands: [
      {
        bind: "tab",
        title: "Cycle window",
        group: "UnlimitedCode",
        run() {
          setWinIdx((i) => (i + 1) % WINDOWS.length)
        },
      },
    ],
  }))

  const c = () => themeColors(props.context)

  const proxyState = createMemo(() => {
    const st = props.initialStatus
    if (!st) return { label: "OFFLINE", color: c().err, ip: "-", rot: 0, burnt: 0 }
    return {
      label: "AUTO",
      color: c().ok,
      ip: shortenIPv6(st.pool?.active_ip || "?"),
      rot: Number(st.hop?.count ?? 0),
      burnt: Number(st.pool?.burnt_count ?? 0),
    }
  })

  const tableRows: Array<{ key: WindowKey; label: string }> = [
    { key: "24h", label: "24h" },
    { key: "7d", label: "7d" },
    { key: "30d", label: "30d" },
    { key: "All", label: "All time" },
  ]

  return (
    <box paddingLeft={3} paddingRight={3} paddingTop={1} paddingBottom={1} flexDirection="column" gap={1} width="100%">
      <box flexDirection="column" width="100%">
        <box flexDirection="row" justifyContent="space-between" width="100%">
          <text fg={c().base}>
            <b>UnlimitedCode</b>
            <span style={{ fg: c().muted }}>
              {props.stats.sinceLabel ? ` · Free Stats (since ${props.stats.sinceLabel})` : " · Free Stats"}
            </span>
          </text>
          <box flexDirection="row" gap={1}>
            <For each={WINDOWS}>
              {(w, idx) => (
                <text fg={winIdx() === idx() ? c().accent : c().muted}>
                  {winIdx() === idx() ? `[${w}]` : w}
                </text>
              )}
            </For>
            <text fg={c().muted}>{"  esc"}</text>
          </box>
        </box>
        <box width="100%" height={1} border={["bottom"]} borderColor={c().border} />
      </box>

      <Show
        when={!props.stats.error}
        fallback={<text fg={c().warn}>{props.stats.error}</text>}
      >
        <box flexDirection="column" width="100%">
          <box flexDirection="row" justifyContent="space-between" width="100%">
            <text fg={c().muted}>{padR("Window", 10)}</text>
            <text fg={c().muted}>
              {padL("Turns", 9) +
                padL("Input", 11) +
                padL("Output", 11) +
                padL("Cache", 12) +
                padL("Total", 12)}
            </text>
          </box>
          <For each={tableRows}>
            {(row) => {
              const b = () => props.stats.buckets[row.key]
              const tot = () => b().inTok + b().outTok + b().cacheTok
              const selected = () => activeWindow() === row.key
              return (
                <box flexDirection="row" justifyContent="space-between" width="100%">
                  <text fg={selected() ? c().accent : c().muted}>{padR(row.label, 10)}</text>
                  <text fg={c().base}>
                    <span style={{ fg: c().base }}>{padL(fmtTurns(b().turns), 9)}</span>
                    <span style={{ fg: c().muted }}>{padL(fmtNum(b().inTok), 11)}</span>
                    <span style={{ fg: c().muted }}>{padL(fmtNum(b().outTok), 11)}</span>
                    <span style={{ fg: c().muted }}>{padL(fmtNum(b().cacheTok), 12)}</span>
                    <span style={{ fg: c().base }}>
                      <b>{padL(fmtNum(tot()), 12)}</b>
                    </span>
                  </text>
                </box>
              )
            }}
          </For>
        </box>

        <box flexDirection="row" justifyContent="space-between" width="100%">
          <text fg={c().muted}>
            {"Cache Hit "}
            <span style={{ fg: c().base }}>{`${props.stats.cacheRatio}%`}</span>
            {"  ·  Avg "}
            <span style={{ fg: c().base }}>{`${props.stats.avgTps} tok/s`}</span>
            {"  ·  Streak "}
            <span style={{ fg: c().base }}>{`${props.stats.streak}d`}</span>
            {"  ·  Peak "}
            <span style={{ fg: c().base }}>{`${String(props.stats.peakHour).padStart(2, "0")}:00`}</span>
          </text>
          <text fg={c().muted}>
            {"429s "}
            <span style={{ fg: proxyState().rot > 0 ? c().warn : c().ok }}>{String(proxyState().rot)}</span>
          </text>
        </box>

        <box flexDirection="column" width="100%">
          <text fg={c().muted}>{`Top Free Models (${activeWindow()})`}</text>
          <Show
            when={topModels().length > 0}
            fallback={<text fg={c().muted}>{"No turns in this window."}</text>}
          >
            <For each={topModels()}>
              {(m) => (
                <box flexDirection="row" justifyContent="space-between" width="100%">
                  <text fg={c().base}>{padR(m.name, 26)}</text>
                  <text fg={c().base}>
                    <span style={{ fg: c().accent }}>{"━".repeat(m.filled)}</span>
                    <span style={{ fg: c().muted }}>{"─".repeat(m.empty)}</span>
                    <span style={{ fg: c().base }}>{padL(`${m.pct}%`, 5)}</span>
                    <span style={{ fg: c().base }}>{padL(m.total, 9)}</span>
                    <span style={{ fg: c().muted }}>{padL(m.turns, 7)}</span>
                    <span style={{ fg: c().muted }}>{padL(m.tps, 10)}</span>
                  </text>
                </box>
              )}
            </For>
          </Show>
        </box>
      </Show>

      <box flexDirection="column" width="100%">
        <box width="100%" height={1} border={["top"]} borderColor={c().border} />
        <box flexDirection="row" justifyContent="space-between" width="100%">
          <text fg={c().muted}>
            <span style={{ fg: proxyState().color }}>{"● "}</span>
            <span style={{ fg: c().base }}>{`Proxy ${proxyState().label}`}</span>
            {` · ${proxyState().ip} · rot `}
            <span style={{ fg: proxyState().rot > 0 ? c().warn : c().base }}>{String(proxyState().rot)}</span>
            {" · burnt "}
            <span style={{ fg: proxyState().burnt > 0 ? c().err : c().base }}>{String(proxyState().burnt)}</span>
          </text>
          <text fg={c().muted}>{"tab window"}</text>
        </box>
      </box>
    </box>
  )
}

function StatusDialog(props: { context: any; initialStatus: any }) {
  const [status, setStatus] = createSignal<any>(props.initialStatus)

  const c = () => themeColors(props.context)

  props.context.keymap.layer(() => ({
    mode: "modal",
    commands: [
      {
        bind: "r",
        title: "Rotate proxy",
        group: "UnlimitedCode",
        async run() {
          const next = await doRotate(props.context)
          if (next) setStatus(next)
        },
      },
      {
        bind: "x",
        title: "Restart daemon",
        group: "UnlimitedCode",
        async run() {
          const next = await doRestartDaemon(props.context)
          if (next) setStatus(next)
        },
      },
    ],
  }))

  const slots = () => (Array.isArray(status()?.pool?.slots) ? status().pool.slots : [])

  return (
    <box paddingLeft={3} paddingRight={3} paddingTop={1} paddingBottom={1} flexDirection="column" gap={1} width="100%">
      <box flexDirection="column" width="100%">
        <box flexDirection="row" justifyContent="space-between" width="100%">
          <text fg={c().base}>
            <b>UnlimitedCode</b>
            <span style={{ fg: c().muted }}>{" · Pool & Proxy Status"}</span>
          </text>
          <text fg={c().muted}>{"esc"}</text>
        </box>
        <box width="100%" height={1} border={["bottom"]} borderColor={c().border} />
      </box>

      <Show
        when={status()}
        fallback={<text fg={c().err}>{`Daemon is not reachable on ${controlURL()}.`}</text>}
      >
        <box flexDirection="column" width="100%">
          <box flexDirection="row" justifyContent="space-between" width="100%">
            <text fg={c().muted}>
              {"State      "}
              <span style={{ fg: c().ok }}>{"● AUTO"}</span>
            </text>
            <text fg={c().muted}>{`v${status().version || "?"}`}</text>
          </box>
          <box flexDirection="row" justifyContent="space-between" width="100%">
            <text fg={c().muted}>
              {"Egress     "}
              <span style={{ fg: c().base }}>{status().egress?.mode || "masque"}</span>
            </text>
            <text fg={c().muted}>
              {"Active IP "}
              <span style={{ fg: c().accent }}>{shortenIPv6(status().pool?.active_ip || "?")}</span>
            </text>
          </box>
          <box flexDirection="row" justifyContent="space-between" width="100%">
            <text fg={c().muted}>
              {"Rotations  "}
              <span style={{ fg: (status().hop?.count ?? 0) > 0 ? c().warn : c().base }}>
                {String(status().hop?.count ?? 0)}
              </span>
            </text>
            <text fg={c().muted}>
              {"Burnt today "}
              <span style={{ fg: (status().pool?.burnt_count ?? 0) > 0 ? c().err : c().base }}>
                {String(status().pool?.burnt_count ?? 0)}
              </span>
            </text>
          </box>
        </box>

        <Show when={slots().length > 0}>
          <box flexDirection="column" width="100%">
            <box flexDirection="row" justifyContent="space-between" width="100%">
              <text fg={c().muted}>{padR("Slot", 10) + padR("State", 12)}</text>
              <text fg={c().muted}>{padR("IPv6 /64 Egress", 24) + padR("PoP", 8) + padL("Recycles", 10)}</text>
            </box>
            <For each={slots()}>
              {(sl: any) => {
                const icon = sl.active ? "● active" : sl.state === "ready" ? "○ warm" : `◌ ${sl.state}`
                const color = sl.active ? c().ok : sl.state === "ready" ? c().base : c().warn
                return (
                  <box flexDirection="row" justifyContent="space-between" width="100%">
                    <text fg={c().muted}>
                      <span style={{ fg: c().base }}>{padR(`slot-${sl.id}`, 10)}</span>
                      <span style={{ fg: color }}>{padR(icon, 12)}</span>
                    </text>
                    <text fg={c().muted}>
                      <span style={{ fg: sl.active ? c().accent : c().base }}>
                        {padR(shortenIPv6(sl.ip || "-"), 24)}
                      </span>
                      <span style={{ fg: c().muted }}>{padR(sl.colo || "-", 8)}</span>
                      <span style={{ fg: c().base }}>{padL(String(sl.recycles ?? 0), 10)}</span>
                    </text>
                  </box>
                )
              }}
            </For>
          </box>
        </Show>
      </Show>

      <box flexDirection="column" width="100%">
        <box width="100%" height={1} border={["top"]} borderColor={c().border} />
        <box flexDirection="row" justifyContent="space-between" width="100%">
          <text fg={c().muted}>{`Control ${controlURL()}`}</text>
          <text fg={c().muted}>{"r rotate · x restart"}</text>
        </box>
      </box>
    </box>
  )
}

async function openStatsModal(context: any): Promise<void> {
  const [stats, status] = await Promise.all([
    loadTokenStats(),
    controlCall("/status", "GET", undefined, 2_000),
  ])
  context.ui.dialog.show(() => <StatsDialog context={context} stats={stats} initialStatus={status} />)
  try {
    context.ui.dialog.set({ size: "large" })
  } catch {}
}

async function openStatusModal(context: any, status?: any): Promise<void> {
  const st = status !== undefined ? status : await controlCall("/status", "GET", undefined, 2_000)
  context.ui.dialog.show(() => <StatusDialog context={context} initialStatus={st} />)
  try {
    context.ui.dialog.set({ size: "large" })
  } catch {}
}

function PromptBadge(props: { context: any; onClick: () => void }) {
  const [state, setState] = createSignal<"auto" | "offline">("auto")
  const [hovered, setHovered] = createSignal(false)

  const refresh = async () => {
    let st = await controlCall("/status", "GET", undefined, 1_500)
    if (!st) {
      if (await ensureDaemonRunning()) {
        st = await controlCall("/status", "GET", undefined, 2_000)
      }
    }
    setState(st ? "auto" : "offline")
  }

  let notifiedUpdate = false

  onMount(() => {
    void (async () => {
      await refresh()
      const st = await controlCall("/status", "GET", undefined, 1_500)
      if (!notifiedUpdate && st?.version) {
        const tag = await checkAvailableUpdate(String(st.version))
        if (tag) {
          notifiedUpdate = true
          props.context.ui.toast.show({
            title: "UnlimitedCode",
            message: `Update available: ${tag} — open /unlimited to install`,
            variant: "info",
            duration: 5000,
          })
        }
      }
    })()
    const timer = setInterval(() => {
      void refresh()
    }, 15_000)
    onCleanup(() => clearInterval(timer))
  })

  const theme = () => props.context.theme
  const labelColor = () =>
    hovered()
      ? theme().text.base
      : state() === "auto"
        ? theme().text.feedback.success.base
        : theme().text.feedback.error.base

  return (
    <box
      flexShrink={0}
      onMouseOver={() => setHovered(true)}
      onMouseOut={() => setHovered(false)}
      onMouseUp={() => props.onClick()}
    >
      <text fg={labelColor()} wrapMode="none">
        {"unlimited"}
      </text>
    </box>
  )
}

export default {
  id: "unlimitedcode",
  setup(context: any) {
    context.ui.slot({
      append: "prompt.footer",
      render() {
        return <PromptBadge context={context} onClick={() => context.keymap.dispatch("unlimitedcode.menu")} />
      },
    })

    context.ui.slot({
      append: "app",
      render() {
        context.keymap.layer(() => ({
          mode: "global",
          commands: [
            {
              id: "unlimitedcode.stats",
              title: "UnlimitedCode Usage Stats",
              description: "OpenCode free-tier tokens, speed & top models since install",
              group: "UnlimitedCode",
              slash: { name: "unlimited-stats", aliases: ["ustats"] },
              palette: true,
              async run() {
                await openStatsModal(context)
              },
            },
            {
              id: "unlimitedcode.menu",
              title: "UnlimitedCode Controls",
              description: "Stats, rotate egress IP, pool details & restart daemon",
              group: "UnlimitedCode",
              slash: { name: "unlimited" },
              palette: true,
              async run() {
                const status = await controlCall("/status", "GET", undefined, 2_000)
                const ip = shortenIPv6(status?.pool?.active_ip || "offline")
                const ready = status?.pool?.slots?.filter((s: any) => s.state === "ready").length ?? 0
                const total = status?.pool?.slots?.length ?? 0
                const badge = !status ? "OFFLINE" : "AUTO"
                const updateTag = status?.version ? await checkAvailableUpdate(String(status.version)) : ""

                const options = [
                  ...(updateTag
                    ? [
                        {
                          title: `Update to ${updateTag}`,
                          value: "update",
                          description: `Install latest release (currently v${status.version})`,
                        },
                      ]
                    : []),
                  {
                    title: "Usage stats",
                    value: "stats",
                    description: "Free tokens, speed & top models",
                  },
                  {
                    title: "Rotate egress IP",
                    value: "rotate",
                    description: status ? `${ip} · pool ${ready}/${total}` : "Daemon offline",
                  },
                  {
                    title: "Pool & proxy details",
                    value: "details",
                    description: status
                      ? `rot ${status.hop?.count ?? 0} · burnt ${status.pool?.burnt_count ?? 0}`
                      : "Daemon offline",
                  },
                  {
                    title: "Restart daemon",
                    value: "restart",
                    description: "Reconnect local proxy & MASQUE pool",
                  },
                ]

                const choice = await context.ui.dialog.select({
                  title: `UnlimitedCode · ${badge}`,
                  placeholder: "Choose an action",
                  options,
                })

                if (choice === "update" && updateTag) {
                  await doApplyUpdate(context, updateTag)
                } else if (choice === "stats") {
                  await openStatsModal(context)
                } else if (choice === "rotate") {
                  await doRotate(context)
                } else if (choice === "restart") {
                  await doRestartDaemon(context)
                } else if (choice === "details") {
                  await openStatusModal(context, status)
                }
              },
            },
            {
              id: "unlimitedcode.rotate",
              title: "UnlimitedCode Rotate Egress",
              description: "Immediately switch to a fresh IPv6 /64 slot",
              group: "UnlimitedCode",
              slash: { name: "rotate" },
              palette: true,
              async run() {
                await doRotate(context)
              },
            },
          ],
        }))
        return null
      },
    })
  },
}
