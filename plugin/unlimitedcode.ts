import { spawn } from "node:child_process"
import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const DEFAULT_LISTEN = "127.0.0.1:42987"
const DEFAULT_CONTROL = "http://127.0.0.1:42988"
// OpenCode's ModelRetry policy emits attempt = 2..11 (10 retries per step).
const MAX_ATTEMPTS = 11
const MAX_SESSION_ALIASES = 200
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
  if (sessionID) {
    if (sessionAliases.size >= MAX_SESSION_ALIASES && !sessionAliases.has(sessionID)) {
      const oldest = sessionAliases.keys().next().value
      if (oldest !== undefined) sessionAliases.delete(oldest)
    }
    sessionAliases.set(sessionID, alias)
  }
  return alias
}

function ensureSessionAlias(sessionID: string): string {
  if (!sessionID) return ""
  const existing = sessionAliases.get(sessionID)
  if (existing) return existing
  return rotateSessionAlias(sessionID)
}

function applySessionAlias(headers: Record<string, string> | Headers, sessionID: string): void {
  const alias = ensureSessionAlias(sessionID)
  if (!alias) return
  const keys = [
    "x-opencode-session",
    "x-opencode-session-id",
    "x-opencode-parent-session-id",
    "x-parent-session-id",
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
      low === "x-opencode-parent-session-id" ||
      low === "x-parent-session-id" ||
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

let cachedControlURL = ""
let cachedControlToken = ""

function readConfigJSON(): any {
  try {
    return JSON.parse(readFileSync(join(stateHome(), "config.json"), "utf8"))
  } catch {
    return null
  }
}

function controlURL(): string {
  if (cachedControlURL) return cachedControlURL
  const raw = process.env.UNLIMITEDCODE_CONTROL || readConfigJSON()?.control || DEFAULT_CONTROL
  cachedControlURL = raw.startsWith("http") ? raw.replace(/\/+$/, "") : `http://${raw}`
  return cachedControlURL
}

function controlToken(forceReload = false): string {
  if (!forceReload && cachedControlToken) return cachedControlToken
  try {
    cachedControlToken = readFileSync(join(stateHome(), "control.token"), "utf8").trim()
  } catch {
    cachedControlToken = ""
  }
  return cachedControlToken
}

async function isDaemonHealthy(): Promise<boolean> {
  try {
    const res = await fetch(controlURL() + "/health", { signal: AbortSignal.timeout(800) })
    return res.ok
  } catch {
    return false
  }
}

let daemonBootPromise: Promise<boolean> | null = null

// Launch the local daemon tied to this OpenCode PID so it exits automatically
// when all OpenCode processes close. Single-flight so concurrent calls share one boot.
function ensureDaemonRunning(): Promise<boolean> {
  if (daemonBootPromise) return daemonBootPromise
  daemonBootPromise = (async () => {
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
    const deadline = Date.now() + 25_000
    while (Date.now() < deadline) {
      if (await isDaemonHealthy()) return true
      await new Promise((r) => setTimeout(r, 150))
    }
    return false
  })().finally(() => {
    daemonBootPromise = null
  })
  return daemonBootPromise
}

function isLoopbackProxy(val: string | undefined): boolean {
  if (!val) return false
  return /^https?:\/\/(127\.0\.0\.1|localhost|\[::1\]):/i.test(val.trim())
}

function ensureProxyEnv(): void {
  const listen = process.env.UNLIMITEDCODE_LISTEN || readConfigJSON()?.listen || DEFAULT_LISTEN
  const proxyURL = listen.startsWith("http") ? listen : `http://${listen}`
  // Always take over if empty or pointing to a stale loopback proxy (e.g. leftover
  // WARP :40000, OpenHop :41987, or OpenCode-Infinite gate) — and keep lowercase
  // https_proxy/http_proxy in sync since Bun checks lowercase before uppercase.
  for (const key of ["HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"] as const) {
    if (!process.env[key] || isLoopbackProxy(process.env[key])) {
      process.env[key] = proxyURL
    }
  }
  if (!process.env.NO_PROXY) process.env.NO_PROXY = "localhost,127.0.0.1,::1"
  if (!process.env.no_proxy) process.env.no_proxy = "localhost,127.0.0.1,::1"
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

function isOpenCodeProvider(event: any): boolean {
  const prov = String(event?.model?.providerID ?? event?.providerID ?? "").toLowerCase()
  if (prov) return prov === "opencode"
  const mid = modelID(event?.model).toLowerCase()
  return mid.startsWith("opencode/") || mid.endsWith("-free") || mid === "big-pickle"
}

function errorText(err: any): string {
  if (!err) return ""
  if (typeof err === "string") return err
  return [err.type, err.message, err.cause?.message, err.data?.message]
    .filter(Boolean)
    .map(String)
    .join(" ")
}

async function requestRotate(event: any, type = "provider.free-limit"): Promise<{ retry: boolean; delay: number } | null> {
  const attempt = Number(event?.attempt ?? 1)
  if (attempt > MAX_ATTEMPTS) return null
  const result = await controlCall(
    "/event",
    "POST",
    {
      type,
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

function desktopDraftsDBPath(): string {
  if (process.platform === "win32" && process.env.APPDATA) {
    return join(process.env.APPDATA, "ai.opencode.desktop", "drafts.sqlite")
  }
  if (process.platform === "darwin") {
    return join(homedir(), "Library", "Application Support", "ai.opencode.desktop", "drafts.sqlite")
  }
  return join(process.env.XDG_CONFIG_HOME || join(homedir(), ".config"), "ai.opencode.desktop", "drafts.sqlite")
}

// Clears OpenCode Desktop's global "free-limit" composer lockout (stored in
// drafts.sqlite -> state['opencode.global.dat','free-limit']) without touching
// opencode.db or chat history. Called only at startup and on free-limit events
// (zero background polling).
async function clearDesktopFreeLimit(): Promise<void> {
  const dbPath = desktopDraftsDBPath()
  if (!existsSync(dbPath)) return
  try {
    const mod: any = await import("bun:sqlite")
    const db = new mod.Database(dbPath)
    try {
      db.exec("PRAGMA busy_timeout = 1000")
      db.run(
        "UPDATE state SET value = '{\"resetAt\":null}', updated_at = ? WHERE name = 'opencode.global.dat' AND key = 'free-limit' AND value != '{\"resetAt\":null}'",
        [Date.now()],
      )
    } finally {
      db.close()
    }
  } catch {
    // Non-fatal: Desktop may not be installed or sqlite may be unavailable.
  }
}

async function setup(ctx: any): Promise<void> {
  // Set proxy env and register hooks immediately (0ms) so OpenCode startup and
  // new-chat initialization never block on daemon boot.
  ensureProxyEnv()
  void ensureDaemonRunning()
  void clearDesktopFreeLimit()

  if (typeof ctx?.session?.hook !== "function") return

  try {
    // Only hook model.request (not http.request): OpenCode already passes all
    // session & parent-session headers here, and omitting http.request keeps
    // native zero-copy SSE streaming without Response re-wrapping overhead.
    await ctx.session.hook("model.request", (event: any) => {
      const sid = String(event?.sessionID ?? "")
      if (sid && event?.headers) {
        applySessionAlias(event.headers, sid)
      }
    })
  } catch {}

  await ctx.session.hook("retry", async (event: any) => {
    const errType = String(event?.error?.type ?? "")
    const errMsg = errorText(event?.error)
    const status = Number(event?.error?.status ?? event?.error?.statusCode ?? 0)
    const sid = String(event?.sessionID ?? "")
    const attempt = Number(event?.attempt ?? 2)

    const isFreeLimit =
      errType === "provider.free-limit" ||
      (isOpenCodeProvider(event) &&
        (status === 429 || /FreeUsageLimitError|free.?usage.?limit|free.?limit|today's free messages/i.test(errMsg)))

    if (!isFreeLimit) {
      // Self-heal proxy/tunnel failures invisibly. On the first transport reset
      // (attempt <= 2), retry cleanly without rotating: when a sibling sub-agent
      // just rotated, CloseTunnels() resets active sockets so they move onto the
      // new slot; rotating again would burn the brand-new slot with 0 bytes sent.
      // Only trigger a transport rotate if the failure persists on attempt >= 3.
      const isTransport =
        errType === "provider.transport" ||
        /ECONNREFUSED|Unable to connect|ERR_PROXY_TUNNEL|502 Bad Gateway|ECONNRESET|socket connection was closed/i.test(
          errMsg,
        )
      if (attempt <= 4 && isTransport) {
        const wasUp = await isDaemonHealthy()
        const up = wasUp || (await ensureDaemonRunning())
        if (up) {
          ensureProxyEnv()
          if (wasUp && attempt >= 3 && isOpenCodeProvider(event)) {
            if (sid) rotateSessionAlias(sid)
            await requestRotate(event, "proxy.transport-error")
          }
          const decision = { retry: true, delay: 150 }
          event.decision = decision
          return decision
        }
      }
      return
    }

    if (attempt > MAX_ATTEMPTS) return { retry: false, delay: 0 }

    await ensureDaemonRunning()
    ensureProxyEnv()
    if (sid) rotateSessionAlias(sid)
    const decision = await requestRotate(event, "provider.free-limit")
    if (!decision) {
      if (attempt < MAX_ATTEMPTS) {
        // Give the MASQUE pool / network a brief window to recover before
        // letting OpenCode emit session.step.failed and lock the Desktop UI.
        const backoff = { retry: true, delay: 3000 }
        event.decision = backoff
        return backoff
      }
      return { retry: false, delay: 0 }
    }
    void clearDesktopFreeLimit()
    event.decision = decision
    return decision
  })
}

export default {
  id: "unlimitedcode",
  setup,
}
