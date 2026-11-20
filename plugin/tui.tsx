import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { createMemo, createSignal, For, onCleanup, onMount, Show } from "solid-js"

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
  const [freeOnly, setFreeOnly] = createSignal(props.stats.free.buckets.All.turns > 0)

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
    ],
  }))

  const c = () => themeColors(props.context)

  const proxyState = createMemo(() => {
    const st = props.initialStatus
    if (!st) return { label: "OFFLINE", color: c().err, ip: "-", rot: 0, burnt: 0 }
    const paused = Boolean(st.hop?.paused)
    return {
      label: paused ? "PAUSED" : "AUTO",
      color: paused ? c().warn : c().ok,
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
            <span style={{ fg: c().muted }}>{" · Usage Stats  "}</span>
            <span style={{ fg: freeOnly() ? c().accent : c().base }}>
              {freeOnly() ? "[Free]" : "[All]"}
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
              {padL("Turns", 8) +
                padL("Input", 10) +
                padL("Output", 10) +
                padL("Cache", 11) +
                padL("Total", 11) +
                padL("Est. Saved", 12)}
            </text>
          </box>
          <For each={tableRows}>
            {(row) => {
              const b = () => scope().buckets[row.key]
              const tot = () => b().inTok + b().outTok + b().cacheTok
              const selected = () => activeWindow() === row.key
              return (
                <box flexDirection="row" justifyContent="space-between" width="100%">
                  <text fg={selected() ? c().accent : c().muted}>{padR(row.label, 10)}</text>
                  <text fg={c().base}>
                    <span style={{ fg: c().base }}>{padL(fmtTurns(b().turns), 8)}</span>
                    <span style={{ fg: c().muted }}>{padL(fmtNum(b().inTok), 10)}</span>
                    <span style={{ fg: c().muted }}>{padL(fmtNum(b().outTok), 10)}</span>
                    <span style={{ fg: c().muted }}>{padL(fmtNum(b().cacheTok), 11)}</span>
                    <span style={{ fg: c().base }}>
                      <b>{padL(fmtNum(tot()), 11)}</b>
                    </span>
                    <span style={{ fg: c().ok }}>{padL(fmtSaved(b()), 12)}</span>
                  </text>
                </box>
              )
            }}
          </For>
        </box>

        <box flexDirection="row" justifyContent="space-between" width="100%">
          <text fg={c().muted}>
            {"Cache Hit "}
            <span style={{ fg: c().base }}>{`${scope().cacheRatio}%`}</span>
            {"  ·  Avg "}
            <span style={{ fg: c().base }}>{`${scope().avgTps} tok/s`}</span>
            {"  ·  Streak "}
            <span style={{ fg: c().base }}>{`${scope().streak}d`}</span>
            {"  ·  Peak "}
            <span style={{ fg: c().base }}>{`${String(scope().peakHour).padStart(2, "0")}:00`}</span>
          </text>
          <text fg={c().muted}>
            {"429s "}
            <span style={{ fg: proxyState().rot > 0 ? c().warn : c().ok }}>{String(proxyState().rot)}</span>
          </text>
        </box>

        <box flexDirection="column" width="100%">
          <text fg={c().muted}>{`Top Models (${activeWindow()} · ${freeOnly() ? "free" : "all"})`}</text>
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
          <text fg={c().muted}>{"tab window · f scope"}</text>
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
              <span style={{ fg: paused() ? c().warn : c().ok }}>
                {paused() ? "⏸ PAUSED" : "● AUTO"}
              </span>
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
          <text fg={c().muted}>{"r rotate · p pause/resume"}</text>
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
  const [state, setState] = createSignal<"auto" | "paused" | "offline">("auto")
  const [hovered, setHovered] = createSignal(false)

  const refresh = async () => {
    const st = await controlCall("/status", "GET", undefined, 1_500)
    if (!st) {
      setState("offline")
      return
    }
    setState(st.hop?.paused ? "paused" : "auto")
  }

  onMount(() => {
    void refresh()
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
        : state() === "paused"
          ? theme().text.feedback.warning.base
          : theme().text.feedback.error.base

  return (
    <box
      flexShrink={0}
      onMouseOver={() => setHovered(true)}
      onMouseOut={() => setHovered(false)}
      onMouseUp={() => props.onClick()}
    >
      <text fg={labelColor()} wrapMode="none">
        <span style={{ fg: theme().text.muted }}>{"· "}</span>
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
