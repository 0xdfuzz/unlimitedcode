import { readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const MAX_ATTEMPTS = 4
const HOP_TIMEOUT_MS = 55_000

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
  if (process.env.KESTREL_HOME) return process.env.KESTREL_HOME
  if (process.platform === "win32" && process.env.LOCALAPPDATA) {
    return join(process.env.LOCALAPPDATA, "Kestrel")
  }
  if (process.env.XDG_STATE_HOME) return join(process.env.XDG_STATE_HOME, "kestrel")
  return join(homedir(), ".local", "state", "kestrel")
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

async function requestHop(event: any): Promise<{ retry: boolean; delay: number } | null> {
  const token = controlToken()
  if (!token) return null

  const attempt = Number(event?.attempt ?? 1)
  if (attempt > MAX_ATTEMPTS) return null

  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), HOP_TIMEOUT_MS)
  try {
    const response = await fetch(controlURL() + "/event", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        authorization: "Bearer " + token,
      },
      body: JSON.stringify({
        type: "provider.free-limit",
        session: String(event?.sessionID ?? ""),
        model: modelID(event?.model),
        agent: nameOf(event?.agent),
        attempt,
      }),
      signal: controller.signal,
    })
    if (!response.ok) return null
    const result: any = await response.json().catch(() => null)
    if (result?.status === "ok") return { retry: true, delay: 0 }
    return null
  } catch {
    return null
  } finally {
    clearTimeout(timer)
  }
}

async function setup(ctx: any): Promise<void> {
  if (typeof ctx?.session?.hook !== "function") return

  // Apply any rotated session alias on both request preparation and outgoing HTTP
  // requests (the latter also runs on retries of the same prepared step).
  try {
    await ctx.session.hook("model.request", (event: any) => {
      const sid = String(event?.sessionID ?? "")
      if (sid && event?.headers) {
        applySessionAlias(event.headers, sid)
      }
    })
  } catch {
    // Older OpenCode builds may not expose model.request
  }

  try {
    await ctx.session.hook("http.request", (event: any) => {
      const sid = String(event?.sessionID ?? "")
      if (sid && event?.request?.headers) {
        applySessionAlias(event.request.headers, sid)
      }
    })
  } catch {
    // Older OpenCode builds may not expose http.request
  }

  await ctx.session.hook("retry", async (event: any) => {
    if (event?.error?.type !== "provider.free-limit") return
    const sid = String(event?.sessionID ?? "")
    const attempt = Number(event?.attempt ?? 1)
    if (attempt > MAX_ATTEMPTS) return { retry: false, delay: 0 }

    // On the first free-limit hit (attempt <= 2), rotate the wire session ID
    // first WITHOUT burning the current egress IP. If the limit was per-session
    // (like a long chat), the retry succeeds immediately on the same IP.
    if (sid && attempt <= 2) {
      rotateSessionAlias(sid)
      const decision = { retry: true, delay: 0 }
      event.decision = decision
      return decision
    }

    // If the rotated session ID still hits 429 (attempt >= 3), the egress IP
    // itself is out of quota: rotate the session alias again and ask Kestrel
    // for a fresh WARP /64 hop.
    if (sid) rotateSessionAlias(sid)
    const decision = await requestHop(event)
    if (!decision) return { retry: false, delay: 0 }
    event.decision = decision
    return decision
  })
}

export default {
  id: "kestrel",
  setup,
}
