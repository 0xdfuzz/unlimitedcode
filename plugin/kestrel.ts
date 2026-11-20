import { spawn } from "node:child_process"
import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const MAX_ATTEMPTS = 4
const HOP_TIMEOUT_MS = 55_000

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
  if (process.env.KESTREL_HOME) return process.env.KESTREL_HOME
  if (process.platform === "win32" && process.env.LOCALAPPDATA) {
    return join(process.env.LOCALAPPDATA, "Kestrel")
  }
  if (process.env.XDG_STATE_HOME) return join(process.env.XDG_STATE_HOME, "kestrel")
  return join(homedir(), ".local", "state", "kestrel")
}

function installedBinary(): string {
  const name = process.platform === "win32" ? "kestrel.exe" : "kestrel"
  return join(stateHome(), name)
}

function controlURL(): string {
  const raw = process.env.KESTREL_CONTROL || DEFAULT_CONTROL
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

// Start kestrel daemon on demand tied to this OpenCode process PID so it exits
// automatically when OpenCode closes (no logon autostart required).
async function ensureDaemonRunning(): Promise<boolean> {
  if (await isDaemonHealthy()) return true
  const bin = installedBinary()
  if (!existsSync(bin)) return false
  try {
    const child = spawn(bin, ["daemon", "--parent-pid", String(process.pid)], {
      detached: true,
      stdio: "ignore",
      windowsHide: true,
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
    HOP_TIMEOUT_MS,
  )
  if (result?.status === "ok") return { retry: true, delay: 0 }
  return null
}

function fmtNum(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M"
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "k"
  return String(n)
}

async function readLocalTokenStats(): Promise<string> {
  const dataDir = process.env.XDG_DATA_HOME
    ? join(process.env.XDG_DATA_HOME, "opencode")
    : join(homedir(), ".local", "share", "opencode")
  const dbPath = join(dataDir, "opencode.db")
  if (!existsSync(dbPath)) return ""

  try {
    const mod: any = await import("bun:sqlite")
    const Database = mod.Database || mod.default
    if (!Database) return ""
    const db = new Database(dbPath, { readonly: true })
    try {
      const now = Date.now()
      const dayAgo = now - 86_400_000
      const weekAgo = now - 7 * 86_400_000
      const rows = db
        .query("SELECT time_created, data FROM session_message WHERE type = 'assistant'")
        .all() as Array<{ time_created: number; data: string }>

      let totalIn = 0, totalOut = 0, totalCache = 0, totalMsgs = 0
      let dayTok = 0, weekTok = 0
      const byModel = new Map<string, number>()

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
        const sum = inp + out
        if (sum <= 0 && cache <= 0) continue

        totalMsgs++
        totalIn += inp
        totalOut += out
        totalCache += cache
        if (row.time_created >= dayAgo) dayTok += sum + cache
        if (row.time_created >= weekAgo) weekTok += sum + cache

        const mid = String(parsed?.model?.id || parsed?.modelID || "unknown")
        byModel.set(mid, (byModel.get(mid) || 0) + sum + cache)
      }

      if (totalMsgs === 0) return ""
      const top = [...byModel.entries()]
        .sort((a, b) => b[1] - a[1])
        .slice(0, 3)
        .map(([m, t]) => `${m} (${fmtNum(t)})`)
        .join(", ")

      return [
        `Tokens (local DB, ${totalMsgs} turns):`,
        `  24h: ${fmtNum(dayTok)} · 7d: ${fmtNum(weekTok)} · Total: ${fmtNum(totalIn + totalOut + totalCache)} (in ${fmtNum(totalIn)} / out ${fmtNum(totalOut)} / cache ${fmtNum(totalCache)})`,
        top ? `  Top models: ${top}` : "",
      ]
        .filter(Boolean)
        .join("\n")
    } finally {
      db.close()
    }
  } catch {
    return ""
  }
}

async function formatStatusReport(): Promise<string> {
  const status = await controlCall("/status")
  if (!status) {
    return "Kestrel daemon is not reachable on " + controlURL()
  }
  const lines: string[] = []
  const paused = Boolean(status.hop?.paused || localPaused)
  lines.push(`Kestrel ${status.version} · auto-rotate: ${paused ? "PAUSED" : "ON"} · egress: ${status.egress?.mode}`)
  if (status.pool) {
    lines.push(`Active IP: ${status.pool.active_ip || "?"} (slot-${status.pool.active_slot}, burnt today: ${status.pool.burnt_count})`)
    for (const sl of status.pool.slots || []) {
      const mark = sl.active ? "*" : " "
      lines.push(`  ${mark} slot-${sl.id} [${sl.state}] ${sl.ip || "-"} (${sl.colo || "-"}) recycles=${sl.recycles}`)
    }
  }
  lines.push(`Rotations: ${status.hop?.count ?? 0} (failed=${status.hop?.failures ?? 0}) · Tunnels: ${status.proxy?.total ?? 0}`)
  const tokenStats = await readLocalTokenStats()
  if (tokenStats) {
    lines.push("")
    lines.push(tokenStats)
  }
  lines.push("")
  lines.push("Commands: /kestrel [status|rotate|pause|resume] · /rotate")
  return lines.join("\n")
}

async function sendNotice(ctx: any, sessionID: string, text: string): Promise<void> {
  if (typeof ctx?.session?.synthetic === "function" && sessionID) {
    try {
      await ctx.session.synthetic({
        sessionID,
        text,
        metadata: { source: "kestrel" },
      })
      return
    } catch {
      // fall through
    }
  }
  throw new Error(text)
}

async function setup(ctx: any): Promise<void> {
  // Launch the local daemon on demand if not already running (non-blocking for fast startup).
  void ensureDaemonRunning()

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

      // First free-limit hit: rotate the wire session ID without burning the current IP.
      if (sid && attempt <= 2) {
        rotateSessionAlias(sid)
        const decision = { retry: true, delay: 0 }
        event.decision = decision
        return decision
      }

      // Subsequent hit: rotate session alias + request a 0ms MASQUE pool swap.
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
          description: "rotate Kestrel egress IP and session alias immediately",
          execute: async (input: any) => {
            const sid = String(input?.sessionID ?? "")
            if (sid) rotateSessionAlias(sid)
            const res = await controlCall("/rotate", "POST", {}, HOP_TIMEOUT_MS)
            const st = await controlCall("/status")
            const ip = st?.pool?.active_ip || "?"
            const msg = res?.status === "ok"
              ? `Kestrel rotated in ${res.duration_ms ?? 0}ms -> ${ip}`
              : `Kestrel rotate failed: ${res?.error || "daemon unreachable"}`
            await sendNotice(ctx, sid, msg)
          },
        })

        editor.add({
          name: "kestrel",
          description: "show Kestrel status & local token stats [status|rotate|pause|resume]",
          execute: async (input: any) => {
            const sid = String(input?.sessionID ?? "")
            const arg = String(input?.prompt?.text ?? "").trim().toLowerCase()
            if (arg === "pause") {
              localPaused = true
              await controlCall("/pause", "POST", { paused: true })
              await sendNotice(ctx, sid, "Kestrel auto-rotate paused. Use `/kestrel resume` to re-enable.")
              return
            }
            if (arg === "resume") {
              localPaused = false
              await controlCall("/pause", "POST", { paused: false })
              await sendNotice(ctx, sid, "Kestrel auto-rotate resumed.")
              return
            }
            if (arg === "rotate") {
              if (sid) rotateSessionAlias(sid)
              const res = await controlCall("/rotate", "POST", {}, HOP_TIMEOUT_MS)
              const st = await controlCall("/status")
              const ip = st?.pool?.active_ip || "?"
              const msg = res?.status === "ok"
                ? `Kestrel rotated in ${res.duration_ms ?? 0}ms -> ${ip}`
                : `Kestrel rotate failed: ${res?.error || "daemon unreachable"}`
              await sendNotice(ctx, sid, msg)
              return
            }
            const report = await formatStatusReport()
            await sendNotice(ctx, sid, report)
          },
        })
      })
    } catch {}
  }
}

export default {
  id: "kestrel",
  setup,
}
