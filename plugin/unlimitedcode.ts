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

async function setup(ctx: any): Promise<void> {
  await ensureDaemonRunning()

  if (typeof ctx?.session?.hook !== "function") return

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

    const st = await controlCall("/status", "GET", undefined, 1_500)
    if (st?.hop?.paused) return { retry: false, delay: 0 }

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

export default {
  id: "unlimitedcode",
  setup,
}
