import { spawn } from "node:child_process"
import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const MAX_ATTEMPTS = 4
const ROTATE_TIMEOUT_MS = 55_000

const BASE62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
const sessionAliases = new Map<string, string>()

let lastIDTs = 0
let idCounter = 0
let localPaused = false

// Matches OpenCode's Identifier.descending() contract ("ses_" + 12 hex of ~(ms*0x1000+counter) + 14 Base62).
function generateSessionID(): string {
  const ts = Date.now()
  if (ts !== lastIDTs) {
    lastIDTs = ts
    idCounter = 0
  }
  idCounter++
  const current = BigInt(ts) * 0x1000n + BigInt(idCounter)
  const value = ~current
  let hex = ""
  for (let i = 0; i < 6; i++) {
    hex += Number((value >> BigInt(40 - 8 * i)) & 0xffn)
      .toString(16)
      .padStart(2, "0")
  }
  const bytes = new Uint8Array(14)
  crypto.getRandomValues(bytes)
  let tail = ""
  for (let i = 0; i < bytes.length; i++) {
    tail += BASE62[bytes[i] % 62]
  }
  return "ses_" + hex + tail
}

function rotateSessionAlias(sessionID: string): string {
  const alias = generateSessionID()
  if (sessionID) sessionAliases.set(sessionID, alias)
  return alias
}

function applySessionAlias(headers: Record<string, string> | Headers, sessionID: string): void {
  const alias = sessionAliases.get(sessionID)
  if (!alias) return
  const keys = [
    "x-opencode-session",
    "x-opencode-session-id",
    "x-session-affinity",
    "x-session-id",
    "X-Session-Id",
  ]
  if (typeof (headers as Headers).set === "function") {
    const h = headers as Headers
    for (const key of keys) {
      if (h.has(key)) h.set(key, alias)
    }
    return
  }
  const record = headers as Record<string, string>
  for (const existing of Object.keys(record)) {
    const low = existing.toLowerCase()
    if (
      low === "x-opencode-session" ||
      low === "x-opencode-session-id" ||
      low === "x-session-affinity" ||
      low === "x-session-id"
    ) {
      record[existing] = alias
    }
  }
}

function stateHome(): string {
  if (process.env.UNLIMITEDCODE_HOME) return process.env.UNLIMITEDCODE_HOME
  if (process.platform === "win32" && process.env.LOCALAPPDATA) {
    return join(process.env.LOCALAPPDATA, "UnlimitedCode")
  }
  if (process.env.XDG_STATE_HOME) return join(process.env.XDG_STATE_HOME, "unlimitedcode")
  return join(homedir(), ".local", "state", "unlimitedcode")
}

function installedBinary(): string {
  const name = process.platform === "win32" ? "unlimitedcode.exe" : "unlimitedcode"
  return join(stateHome(), name)
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

async function isDaemonHealthy(): Promise<boolean> {
  try {
    const res = await fetch(controlURL() + "/health", { signal: AbortSignal.timeout(800) })
    return res.ok
  } catch {
    return false
  }
}

// Launch the local daemon tied to this OpenCode PID so it exits automatically
// when OpenCode closes.
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
  const deadline = Date.now() + 12_000
  while (Date.now() < deadline) {
    if (await isDaemonHealthy()) return true
    await new Promise((r) => setTimeout(r, 250))
  }
  return false
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

function nameOf(value: any): string {
  if (!value) return ""
  if (typeof value === "string") return value
  return String(value.name || value.id || "")
}

function modelID(model: any): string {
  if (!model) return ""
  if (typeof model === "string") return model
  return String(model.modelID || model.id || model.name || "")
}

async function requestRotate(event: any): Promise<{ retry: boolean; delay: number } | null> {
  const attempt = Number(event?.attempt ?? 1)
  if (attempt > MAX_ATTEMPTS) return null
  const result = await controlCall(
    "/event",
    "POST",
    {
      type: "provider.free-limit",
      session: String(event?.sessionID ?? ""),
      model: modelID(event?.model),
      agent: nameOf(event?.agent),
      attempt,
    },
    ROTATE_TIMEOUT_MS,
  )
  if (result?.status === "ok") return { retry: true, delay: 0 }
  return null
}

function fmtNum(n: number): string {
  if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(2) + "B"
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M"
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k"
  return String(Math.round(n))
}

function bar(fraction: number, width = 12): string {
  const clamped = Math.max(0, Math.min(1, fraction))
  const filled = Math.round(clamped * width)
  return "█".repeat(filled) + "░".repeat(width - filled)
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

async function formatTokenStatsReport(): Promise<string> {
  const dataDir = process.env.XDG_DATA_HOME
    ? join(process.env.XDG_DATA_HOME, "opencode")
    : join(homedir(), ".local", "share", "opencode")
  const dbPath = join(dataDir, "opencode.db")
  if (!existsSync(dbPath)) {
    return "### UnlimitedCode Stats\n\nNo local OpenCode database found yet."
  }

  try {
    const mod: any = await import("bun:sqlite")
    const Database = mod.Database || mod.default
    if (!Database) return "### UnlimitedCode Stats\n\n`bun:sqlite` is unavailable in this runtime."

    const db = new Database(dbPath, { readonly: true })
    try {
      const now = Date.now()
      const dayAgo = now - 86_400_000
      const weekAgo = now - 7 * 86_400_000
      const monthAgo = now - 30 * 86_400_000

      const rows = db
        .query("SELECT time_created, data FROM session_message WHERE type = 'assistant'")
        .all() as Array<{ time_created: number; data: string }>

      type Bucket = { inTok: number; outTok: number; cacheTok: number; turns: number }
      const empty = (): Bucket => ({ inTok: 0, outTok: 0, cacheTok: 0, turns: 0 })
      const b24 = empty(), b7 = empty(), b30 = empty(), bAll = empty()
      const byModel = new Map<string, { total: number; turns: number }>()
      const activeDays = new Set<string>()
      const hourCounts = new Array<number>(24).fill(0)
      let speedTok = 0
      let speedSec = 0

      const add = (b: Bucket, i: number, o: number, c: number) => {
        b.inTok += i
        b.outTok += o
        b.cacheTok += c
        b.turns++
      }

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
        const total = inp + out + cache
        if (total <= 0) continue

        add(bAll, inp, out, cache)
        if (row.time_created >= dayAgo) add(b24, inp, out, cache)
        if (row.time_created >= weekAgo) add(b7, inp, out, cache)
        if (row.time_created >= monthAgo) add(b30, inp, out, cache)

        const dt = new Date(row.time_created)
        activeDays.add(dt.toISOString().slice(0, 10))
        hourCounts[dt.getHours()]++

        const started = Number(parsed?.time?.created ?? 0)
        const completed = Number(parsed?.time?.completed ?? 0)
        if (out > 0 && completed > started && completed - started < 300_000) {
          speedTok += out
          speedSec += (completed - started) / 1000
        }

        const mid = String(parsed?.model?.id || parsed?.modelID || "unknown")
        const prev = byModel.get(mid) || { total: 0, turns: 0 }
        prev.total += total
        prev.turns++
        byModel.set(mid, prev)
      }

      if (bAll.turns === 0) {
        return "### UnlimitedCode Stats\n\nNo token usage recorded in `opencode.db` yet."
      }

      const rowLine = (label: string, b: Bucket) => {
        const tot = b.inTok + b.outTok + b.cacheTok
        return `| ${label} | ${b.turns} | ${fmtNum(b.inTok)} | ${fmtNum(b.outTok)} | ${fmtNum(b.cacheTok)} | **${fmtNum(tot)}** |`
      }

      const cacheRatio = bAll.inTok + bAll.cacheTok > 0
        ? Math.round((bAll.cacheTok / (bAll.inTok + bAll.cacheTok)) * 100)
        : 0
      const avgTps = speedSec > 0 ? (speedTok / speedSec).toFixed(1) : "—"
      const streak = computeStreakDays(activeDays)
      let peakHour = 0
      for (let h = 1; h < 24; h++) {
        if (hourCounts[h] > hourCounts[peakHour]) peakHour = h
      }

      const models = [...byModel.entries()].sort((a, b) => b[1].total - a[1].total).slice(0, 6)
      const maxModelTok = models[0]?.[1].total || 1
      const modelLines = models.map(([name, info]) => {
        const pct = Math.round((info.total / (bAll.inTok + bAll.outTok + bAll.cacheTok)) * 100)
        return `| \`${name}\` | ${bar(info.total / maxModelTok)} ${pct}% | ${fmtNum(info.total)} | ${info.turns} |`
      })

      const status = await controlCall("/status")
      const rotSummary = status
        ? `Rotations this session: **${status.hop?.count ?? 0}** · Active IP: \`${status.pool?.active_ip || "?"}\``
        : "Daemon offline"

      return [
        "### UnlimitedCode — Local Usage Stats",
        "",
        "| Window | Turns | Input | Output | Cache Read | Total |",
        "|---|---:|---:|---:|---:|---:|",
        rowLine("24h", b24),
        rowLine("7d", b7),
        rowLine("30d", b30),
        rowLine("All time", bAll),
        "",
        `**Cache Hit:** ${cacheRatio}% · **Avg Speed:** ${avgTps} tok/s · **Streak:** ${streak}d · **Peak Hour:** ${String(peakHour).padStart(2, "0")}:00`,
        "",
        "| Model | Share | Tokens | Turns |",
        "|---|---|---:|---:|",
        ...modelLines,
        "",
        rotSummary,
      ].join("\n")
    } finally {
      db.close()
    }
  } catch (err: any) {
    return `### UnlimitedCode Stats\n\nFailed to read local DB: ${err?.message || err}`
  }
}

async function formatStatusReport(): Promise<string> {
  const status = await controlCall("/status")
  if (!status) {
    return "### UnlimitedCode\n\nDaemon is not reachable on `" + controlURL() + "`."
  }
  const paused = Boolean(status.hop?.paused || localPaused)
  const badge = paused ? "⏸ PAUSED" : "● AUTO"
  const lines: string[] = [
    `### UnlimitedCode \`v${status.version}\` — ${badge}`,
    "",
    `**Egress:** \`${status.egress?.mode}\` · **Active IP:** \`${status.pool?.active_ip || "?"}\` · **Rotations:** ${status.hop?.count ?? 0} · **Burnt today:** ${status.pool?.burnt_count ?? 0}`,
  ]
  if (status.pool?.slots?.length) {
    lines.push("")
    lines.push("| Slot | State | IPv6 /64 Egress | PoP | Recycles |")
    lines.push("|---|---|---|---|---:|")
    for (const sl of status.pool.slots) {
      const icon = sl.active ? "● active" : sl.state === "ready" ? "○ warm" : `◌ ${sl.state}`
      lines.push(`| slot-${sl.id} | ${icon} | \`${sl.ip || "-"}\` | ${sl.colo || "-"} | ${sl.recycles} |`)
    }
  }
  lines.push("")
  lines.push("Commands: `/unlimited [status|rotate|pause|resume]` · `/unlimited-stats` · `/rotate`")
  return lines.join("\n")
}

async function sendNotice(ctx: any, sessionID: string, text: string): Promise<void> {
  if (typeof ctx?.session?.synthetic === "function" && sessionID) {
    try {
      await ctx.session.synthetic({
        sessionID,
        text,
        metadata: { source: "unlimitedcode" },
      })
      return
    } catch {}
  }
  throw new Error(text)
}

async function setup(ctx: any): Promise<void> {
  await ensureDaemonRunning()

  if (typeof ctx?.session?.hook === "function") {
    try {
      await ctx.session.hook("model.request", (event: any) => {
        const sid = String(event?.sessionID ?? "")
        if (sid && event?.headers) {
          applySessionAlias(event.headers, sid)
        }
      })
    } catch {}

    try {
      await ctx.session.hook("http.request", (event: any) => {
        const sid = String(event?.sessionID ?? "")
        if (sid && event?.request?.headers) {
          applySessionAlias(event.request.headers, sid)
        }
      })
    } catch {}

    await ctx.session.hook("retry", async (event: any) => {
      if (event?.error?.type !== "provider.free-limit") return
      if (localPaused) return { retry: false, delay: 0 }

      const sid = String(event?.sessionID ?? "")
      const attempt = Number(event?.attempt ?? 1)
      if (attempt > MAX_ATTEMPTS) return { retry: false, delay: 0 }

      if (sid && attempt <= 2) {
        rotateSessionAlias(sid)
        const decision = { retry: true, delay: 0 }
        event.decision = decision
        return decision
      }

      if (sid) rotateSessionAlias(sid)
      const decision = await requestRotate(event)
      if (!decision) return { retry: false, delay: 0 }
      event.decision = decision
      return decision
    })
  }

  if (typeof ctx?.command?.transform === "function") {
    try {
      await ctx.command.transform((editor: any) => {
        editor.add({
          name: "rotate",
          description: "rotate UnlimitedCode egress IP and session alias immediately",
          execute: async (input: any) => {
            const sid = String(input?.sessionID ?? "")
            if (sid) rotateSessionAlias(sid)
            const res = await controlCall("/rotate", "POST", {}, ROTATE_TIMEOUT_MS)
            const st = await controlCall("/status")
            const ip = st?.pool?.active_ip || "?"
            const msg = res?.status === "ok"
              ? `● Rotated in **${res.duration_ms ?? 0}ms** → \`${ip}\``
              : `Rotate failed: ${res?.error || "daemon unreachable"}`
            await sendNotice(ctx, sid, msg)
          },
        })

        editor.add({
          name: "unlimited-stats",
          description: "show local OpenCode token usage, speed, streak, and model breakdown",
          execute: async (input: any) => {
            const sid = String(input?.sessionID ?? "")
            const report = await formatTokenStatsReport()
            await sendNotice(ctx, sid, report)
          },
        })

        editor.add({
          name: "unlimited",
          description: "UnlimitedCode status & controls [status|rotate|pause|resume|stats]",
          execute: async (input: any) => {
            const sid = String(input?.sessionID ?? "")
            const arg = String(input?.prompt?.text ?? "").trim().toLowerCase()
            if (arg === "stats") {
              await sendNotice(ctx, sid, await formatTokenStatsReport())
              return
            }
            if (arg === "pause") {
              localPaused = true
              await controlCall("/pause", "POST", { paused: true })
              await sendNotice(ctx, sid, "⏸ UnlimitedCode auto-rotate **paused**. Use `/unlimited resume` to re-enable.")
              return
            }
            if (arg === "resume") {
              localPaused = false
              await controlCall("/pause", "POST", { paused: false })
              await sendNotice(ctx, sid, "● UnlimitedCode auto-rotate **resumed**.")
              return
            }
            if (arg === "rotate") {
              if (sid) rotateSessionAlias(sid)
              const res = await controlCall("/rotate", "POST", {}, ROTATE_TIMEOUT_MS)
              const st = await controlCall("/status")
              const ip = st?.pool?.active_ip || "?"
              const msg = res?.status === "ok"
                ? `● Rotated in **${res.duration_ms ?? 0}ms** → \`${ip}\``
                : `Rotate failed: ${res?.error || "daemon unreachable"}`
              await sendNotice(ctx, sid, msg)
              return
            }
            await sendNotice(ctx, sid, await formatStatusReport())
          },
        })
      })
    } catch {}
  }
}

export default {
  id: "unlimitedcode",
  setup,
}
