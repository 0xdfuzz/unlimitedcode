import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { createMemo, createSignal, For, Show } from "solid-js"

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const ROTATE_TIMEOUT_MS = 55_000

const WINDOWS = ["24h", "7d", "30d", "All"] as const
type WindowKey = (typeof WINDOWS)[number]

// Mid-tier frontier API reference rates ($ per 1M tokens) for Est. Saved.
const RATE_INPUT_PER_M = 0.5
const RATE_OUTPUT_PER_M = 2.0
const RATE_CACHE_PER_M = 0.05

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

type ScopeStats = {
  buckets: Record<WindowKey, Bucket>
  byModel: Record<WindowKey, Map<string, ModelAgg>>
  cacheRatio: number
  avgTps: string
  streak: number
  peakHour: number
}

type FullStats = {
  free: ScopeStats
  all: ScopeStats
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

function controlURL(): string {
  const raw = process.env.UNLIMITEDCODE_CONTROL || DEFAULT_CONTROL
  return raw.startsWith("http") ? raw.replace(/\/+$/, "") : `http://${raw}`
}

function controlToken(): string {
  try {
    return readFileSync(join(stateHome(), "control.token"), "utf8").trim()
  } catch {
    return ""
  }
}

async function controlCall(path: string, method = "GET", body?: any, timeoutMs = 8_000): Promise<any | null> {
  const token = controlToken()
  if (!token) return null
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

function fmtSaved(b: Bucket): string {
  const usd =
    (b.inTok / 1_000_000) * RATE_INPUT_PER_M +
    (b.outTok / 1_000_000) * RATE_OUTPUT_PER_M +
    (b.cacheTok / 1_000_000) * RATE_CACHE_PER_M
  if (usd < 0.01) return "~$0.00"
  return "~$" + usd.toFixed(2)
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

function isFreeEntry(parsed: any, modelId: string): boolean {
  const low = modelId.toLowerCase()
  if (low.endsWith("-free") || low.includes(":free") || low === "big-pickle") return true
  if (typeof parsed?.cost === "number" && parsed.cost === 0) return true
  if (typeof parsed?.cost?.total === "number" && parsed.cost.total === 0) return true
  const prov = String(parsed?.providerID || parsed?.model?.providerID || "").toLowerCase()
  if (prov === "opencode" && !parsed?.cost) return true
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

function finalizeScope(acc: ReturnType<typeof createScopeAcc>): ScopeStats {
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
  }
}

async function loadTokenStats(): Promise<FullStats> {
  const emptyFree = finalizeScope(createScopeAcc())
  const emptyAll = finalizeScope(createScopeAcc())

  const dataDir = process.env.XDG_DATA_HOME
    ? join(process.env.XDG_DATA_HOME, "opencode")
    : join(homedir(), ".local", "share", "opencode")
  const dbPath = join(dataDir, "opencode.db")
  if (!existsSync(dbPath)) {
    return { free: emptyFree, all: emptyAll, error: "No local OpenCode database found yet." }
  }

  try {
    const mod: any = await import("bun:sqlite")
    const Database = mod.Database || mod.default
    if (!Database) {
      return { free: emptyFree, all: emptyAll, error: "bun:sqlite is unavailable in this runtime." }
    }

    const db = new Database(dbPath, { readonly: true })
    try {
      const now = Date.now()
      const dayAgo = now - 86_400_000
      const weekAgo = now - 7 * 86_400_000
      const monthAgo = now - 30 * 86_400_000

      const rows = db
        .query("SELECT time_created, data FROM session_message WHERE type = 'assistant'")
        .all() as Array<{ time_created: number; data: string }>

      const freeAcc = createScopeAcc()
      const allAcc = createScopeAcc()

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

        const windows: WindowKey[] = ["All"]
        if (row.time_created >= monthAgo) windows.push("30d")
        if (row.time_created >= weekAgo) windows.push("7d")
        if (row.time_created >= dayAgo) windows.push("24h")

        const dt = new Date(row.time_created)
        const started = Number(parsed?.time?.created ?? 0)
        const completed = Number(parsed?.time?.completed ?? 0)
        const turnSpeedSec =
          out > 0 && completed > started && completed - started < 300_000 ? (completed - started) / 1000 : 0

        const mid = String(parsed?.model?.id || parsed?.modelID || "unknown")
        addToScope(allAcc, windows, inp, out, cache, mid, dt, turnSpeedSec)
        if (isFreeEntry(parsed, mid)) {
          addToScope(freeAcc, windows, inp, out, cache, mid, dt, turnSpeedSec)
        }
      }

      if (allAcc.buckets.All.turns === 0) {
        return { free: emptyFree, all: emptyAll, error: "No token usage recorded in opencode.db yet." }
      }

      return {
        free: finalizeScope(freeAcc),
        all: finalizeScope(allAcc),
      }
    } finally {
      db.close()
    }
  } catch (err: any) {
    return { free: emptyFree, all: emptyAll, error: `Failed to read local DB: ${err?.message || err}` }
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

function StatsDialog(props: { context: any; stats: FullStats; initialStatus: any }) {
  const [winIdx, setWinIdx] = createSignal(3) // Default: All
  const [freeOnly, setFreeOnly] = createSignal(props.stats.free.buckets.All.turns > 0)
  const [status, setStatus] = createSignal<any>(props.initialStatus)

  const activeWindow = createMemo(() => WINDOWS[winIdx()])
  const scope = createMemo(() => (freeOnly() ? props.stats.free : props.stats.all))

  const topModels = createMemo(() => {
    const w = activeWindow()
    const map = scope().byModel[w]
    const b = scope().buckets[w]
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
      {
        bind: "f",
        title: "Toggle Free / All models",
        group: "UnlimitedCode",
        run() {
          setFreeOnly((v) => !v)
        },
      },
      {
        bind: "r",
        title: "Rotate proxy",
        group: "UnlimitedCode",
        async run() {
          const next = await doRotate(props.context)
          if (next) setStatus(next)
        },
      },
    ],
  }))

  const theme = () => props.context.theme
  const fgBase = () => theme().text.base
  const fgMuted = () => theme().text.muted
  const fgOk = () => theme().text.feedback.success.base
  const fgWarn = () => theme().text.feedback.warning.base
  const fgErr = () => theme().text.feedback.error.base

  const proxyState = createMemo(() => {
    const st = status()
    if (!st) return { label: "OFFLINE", color: fgErr(), ip: "-", rot: 0, burnt: 0 }
    const paused = Boolean(st.hop?.paused)
    return {
      label: paused ? "PAUSED" : "AUTO",
      color: paused ? fgWarn() : fgOk(),
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
    <box paddingLeft={2} paddingRight={2} paddingTop={1} paddingBottom={1} flexDirection="column" gap={1}>
      <box flexDirection="row" justifyContent="space-between" width="100%">
        <text fg={fgBase()}>
          <b>UnlimitedCode</b>
          <span style={{ fg: fgMuted() }}>{" · Usage Stats  "}</span>
          <span style={{ fg: freeOnly() ? fgOk() : fgBase() }}>{freeOnly() ? "[Free]" : "[All]"}</span>
        </text>
        <box flexDirection="row" gap={1}>
          <For each={WINDOWS}>
            {(w, idx) => (
              <text fg={winIdx() === idx() ? fgBase() : fgMuted()}>
                {winIdx() === idx() ? `[${w}]` : w}
              </text>
            )}
          </For>
          <text fg={fgMuted()}>{"  esc"}</text>
        </box>
      </box>

      <text fg={fgMuted()}>{"─".repeat(72)}</text>

      <Show
        when={!props.stats.error}
        fallback={<text fg={fgWarn()}>{props.stats.error}</text>}
      >
        <box flexDirection="column">
          <text fg={fgMuted()}>
            {padR("Window", 10) +
              padL("Turns", 8) +
              padL("Input", 10) +
              padL("Output", 10) +
              padL("Cache", 11) +
              padL("Total", 11) +
              padL("Est. Saved", 12)}
          </text>
          <For each={tableRows}>
            {(row) => {
              const b = () => scope().buckets[row.key]
              const tot = () => b().inTok + b().outTok + b().cacheTok
              const selected = () => activeWindow() === row.key
              return (
                <text fg={selected() ? fgBase() : fgMuted()}>
                  <span style={{ fg: selected() ? fgBase() : fgMuted() }}>{padR(row.label, 10)}</span>
                  <span style={{ fg: fgBase() }}>{padL(fmtTurns(b().turns), 8)}</span>
                  <span style={{ fg: fgMuted() }}>{padL(fmtNum(b().inTok), 10)}</span>
                  <span style={{ fg: fgMuted() }}>{padL(fmtNum(b().outTok), 10)}</span>
                  <span style={{ fg: fgMuted() }}>{padL(fmtNum(b().cacheTok), 11)}</span>
                  <span style={{ fg: fgBase() }}>
                    <b>{padL(fmtNum(tot()), 11)}</b>
                  </span>
                  <span style={{ fg: fgOk() }}>{padL(fmtSaved(b()), 12)}</span>
                </text>
              )
            }}
          </For>
        </box>

        <text fg={fgMuted()}>
          {"Cache Hit "}
          <span style={{ fg: fgBase() }}>{`${scope().cacheRatio}%`}</span>
          {"  ·  Avg "}
          <span style={{ fg: fgBase() }}>{`${scope().avgTps} tok/s`}</span>
          {"  ·  Streak "}
          <span style={{ fg: fgBase() }}>{`${scope().streak}d`}</span>
          {"  ·  Peak "}
          <span style={{ fg: fgBase() }}>{`${String(scope().peakHour).padStart(2, "0")}:00`}</span>
          {"  ·  429s "}
          <span style={{ fg: proxyState().rot > 0 ? fgWarn() : fgOk() }}>{String(proxyState().rot)}</span>
        </text>

        <box flexDirection="column">
          <text fg={fgMuted()}>{`Top Models (${activeWindow()}${freeOnly() ? " · free" : " · all"})`}</text>
          <Show
            when={topModels().length > 0}
            fallback={<text fg={fgMuted()}>{"  No turns in this window."}</text>}
          >
            <For each={topModels()}>
              {(m) => (
                <text fg={fgBase()}>
                  <span style={{ fg: fgBase() }}>{padR(m.name, 26)}</span>
                  {" "}
                  <span style={{ fg: fgOk() }}>{"━".repeat(m.filled)}</span>
                  <span style={{ fg: fgMuted() }}>{"─".repeat(m.empty)}</span>
                  <span style={{ fg: fgBase() }}>{padL(`${m.pct}%`, 5)}</span>
                  <span style={{ fg: fgBase() }}>{padL(m.total, 9)}</span>
                  <span style={{ fg: fgMuted() }}>{padL(m.turns, 7)}</span>
                  <span style={{ fg: fgMuted() }}>{padL(m.tps, 10)}</span>
                </text>
              )}
            </For>
          </Show>
        </box>
      </Show>

      <text fg={fgMuted()}>{"─".repeat(72)}</text>

      <box flexDirection="row" justifyContent="space-between" width="100%">
        <text fg={fgMuted()}>
          <span style={{ fg: proxyState().color }}>{"● "}</span>
          <span style={{ fg: fgBase() }}>{`Proxy ${proxyState().label}`}</span>
          {` · ${proxyState().ip} · rot `}
          <span style={{ fg: proxyState().rot > 0 ? fgWarn() : fgBase() }}>{String(proxyState().rot)}</span>
          {" · burnt "}
          <span style={{ fg: proxyState().burnt > 0 ? fgErr() : fgBase() }}>{String(proxyState().burnt)}</span>
        </text>
        <text fg={fgMuted()}>{"tab window · f scope · r rot"}</text>
      </box>
    </box>
  )
}

function StatusDialog(props: { context: any; initialStatus: any }) {
  const [status, setStatus] = createSignal<any>(props.initialStatus)

  const theme = () => props.context.theme
  const fgBase = () => theme().text.base
  const fgMuted = () => theme().text.muted
  const fgOk = () => theme().text.feedback.success.base
  const fgWarn = () => theme().text.feedback.warning.base
  const fgErr = () => theme().text.feedback.error.base

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
        bind: "p",
        title: "Toggle pause",
        group: "UnlimitedCode",
        async run() {
          const cur = Boolean(status()?.hop?.paused)
          const res = await controlCall("/pause", "POST", { paused: !cur })
          if (res) {
            const refreshed = await controlCall("/status", "GET", undefined, 2_000)
            if (refreshed) setStatus(refreshed)
            props.context.ui.toast.show({
              title: "UnlimitedCode",
              message: !cur ? "Auto-rotate paused" : "Auto-rotate resumed",
              variant: "info",
              duration: 3000,
            })
          }
        },
      },
    ],
  }))

  const paused = () => Boolean(status()?.hop?.paused)
  const slots = () => (Array.isArray(status()?.pool?.slots) ? status().pool.slots : [])

  return (
    <box paddingLeft={2} paddingRight={2} paddingTop={1} paddingBottom={1} flexDirection="column" gap={1}>
      <box flexDirection="row" justifyContent="space-between" width="100%">
        <text fg={fgBase()}>
          <b>UnlimitedCode</b>
          <span style={{ fg: fgMuted() }}>{" · Pool & Proxy Status"}</span>
        </text>
        <text fg={fgMuted()}>{"esc"}</text>
      </box>

      <text fg={fgMuted()}>{"─".repeat(66)}</text>

      <Show
        when={status()}
        fallback={<text fg={fgErr()}>{`Daemon is not reachable on ${controlURL()}.`}</text>}
      >
        <box flexDirection="column">
          <text fg={fgMuted()}>
            {"State      "}
            <span style={{ fg: paused() ? fgWarn() : fgOk() }}>
              {paused() ? "⏸ PAUSED" : "● AUTO"}
            </span>
            <span style={{ fg: fgMuted() }}>{`   v${status().version || "?"}`}</span>
          </text>
          <text fg={fgMuted()}>
            {"Egress     "}
            <span style={{ fg: fgBase() }}>{status().egress?.mode || "masque"}</span>
            <span style={{ fg: fgMuted() }}>{"   Active IP "}</span>
            <span style={{ fg: fgBase() }}>{shortenIPv6(status().pool?.active_ip || "?")}</span>
          </text>
          <text fg={fgMuted()}>
            {"Rotations  "}
            <span style={{ fg: (status().hop?.count ?? 0) > 0 ? fgWarn() : fgBase() }}>
              {String(status().hop?.count ?? 0)}
            </span>
            <span style={{ fg: fgMuted() }}>{"   Burnt today "}</span>
            <span style={{ fg: (status().pool?.burnt_count ?? 0) > 0 ? fgErr() : fgBase() }}>
              {String(status().pool?.burnt_count ?? 0)}
            </span>
          </text>
        </box>

        <Show when={slots().length > 0}>
          <box flexDirection="column">
            <text fg={fgMuted()}>
              {padR("Slot", 8) +
                padR("State", 12) +
                padR("IPv6 /64 Egress", 24) +
                padR("PoP", 8) +
                padL("Recycles", 10)}
            </text>
            <For each={slots()}>
              {(sl: any) => {
                const icon = sl.active ? "● active" : sl.state === "ready" ? "○ warm" : `◌ ${sl.state}`
                const color = sl.active ? fgOk() : sl.state === "ready" ? fgBase() : fgWarn()
                return (
                  <text fg={fgMuted()}>
                    <span style={{ fg: fgBase() }}>{padR(`slot-${sl.id}`, 8)}</span>
                    <span style={{ fg: color }}>{padR(icon, 12)}</span>
                    <span style={{ fg: fgBase() }}>{padR(shortenIPv6(sl.ip || "-"), 24)}</span>
                    <span style={{ fg: fgMuted() }}>{padR(sl.colo || "-", 8)}</span>
                    <span style={{ fg: fgBase() }}>{padL(String(sl.recycles ?? 0), 10)}</span>
                  </text>
                )
              }}
            </For>
          </box>
        </Show>
      </Show>

      <text fg={fgMuted()}>{"─".repeat(66)}</text>

      <box flexDirection="row" justifyContent="space-between" width="100%">
        <text fg={fgMuted()}>{`Control ${controlURL()}`}</text>
        <text fg={fgMuted()}>{"r rotate · p pause/resume · esc close"}</text>
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

export default {
  id: "unlimitedcode",
  setup(context: any) {
    context.ui.slot({
      append: "app",
      render() {
        context.keymap.layer(() => ({
          mode: "global",
          commands: [
            {
              id: "unlimitedcode.stats",
              title: "UnlimitedCode Usage Stats",
              description: "Free-tier tokens, speed, savings & top models",
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
              description: "Stats, rotate egress IP, pause/resume & pool details",
              group: "UnlimitedCode",
              slash: { name: "unlimited" },
              palette: true,
              async run() {
                const status = await controlCall("/status", "GET", undefined, 2_000)
                const paused = Boolean(status?.hop?.paused)
                const ip = shortenIPv6(status?.pool?.active_ip || "offline")
                const ready = status?.pool?.slots?.filter((s: any) => s.state === "ready").length ?? 0
                const total = status?.pool?.slots?.length ?? 0
                const badge = !status ? "OFFLINE" : paused ? "PAUSED" : "AUTO"

                const choice = await context.ui.dialog.select({
                  title: `UnlimitedCode · ${badge}`,
                  placeholder: "Choose an action",
                  options: [
                    {
                      title: "Usage stats",
                      value: "stats",
                      description: "Tokens, speed, savings & models",
                    },
                    {
                      title: "Rotate egress IP",
                      value: "rotate",
                      description: status ? `${ip} · pool ${ready}/${total}` : "Daemon offline",
                    },
                    {
                      title: paused ? "Resume auto-rotate" : "Pause auto-rotate",
                      value: paused ? "resume" : "pause",
                      description: "Automatic 429 recovery",
                    },
                    {
                      title: "Pool & proxy details",
                      value: "details",
                      description: status
                        ? `rot ${status.hop?.count ?? 0} · burnt ${status.pool?.burnt_count ?? 0}`
                        : "Daemon offline",
                    },
                  ],
                })

                if (choice === "stats") {
                  await openStatsModal(context)
                } else if (choice === "rotate") {
                  await doRotate(context)
                } else if (choice === "pause" || choice === "resume") {
                  const nextPaused = choice === "pause"
                  const res = await controlCall("/pause", "POST", { paused: nextPaused })
                  if (res) {
                    context.ui.toast.show({
                      title: "UnlimitedCode",
                      message: nextPaused ? "Auto-rotate paused" : "Auto-rotate resumed",
                      variant: "info",
                      duration: 3000,
                    })
                  } else {
                    context.ui.toast.show({
                      title: "UnlimitedCode",
                      message: "Daemon unreachable",
                      variant: "error",
                      duration: 3500,
                    })
                  }
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
