import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

const DEFAULT_CONTROL = "http://127.0.0.1:42988"
const ROTATE_TIMEOUT_MS = 55_000

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

function bar(fraction: number, width = 12): string {
  const clamped = Math.max(0, Math.min(1, fraction))
  const filled = Math.round(clamped * width)
  return "█".repeat(filled) + "░".repeat(width - filled)
}

function padR(s: string, n: number): string {
  return s.length >= n ? s.slice(0, n) : s + " ".repeat(n - s.length)
}

function padL(s: string, n: number): string {
  return s.length >= n ? s : " ".repeat(n - s.length) + s
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

async function formatTokenStats(): Promise<string> {
  const dataDir = process.env.XDG_DATA_HOME
    ? join(process.env.XDG_DATA_HOME, "opencode")
    : join(homedir(), ".local", "share", "opencode")
  const dbPath = join(dataDir, "opencode.db")
  if (!existsSync(dbPath)) {
    return "No local OpenCode database found yet."
  }

  try {
    const mod: any = await import("bun:sqlite")
    const Database = mod.Database || mod.default
    if (!Database) return "bun:sqlite is unavailable in this runtime."

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
      const b24 = empty()
      const b7 = empty()
      const b30 = empty()
      const bAll = empty()
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
        return "No token usage recorded in opencode.db yet."
      }

      const fmtRow = (label: string, b: Bucket) => {
        const tot = b.inTok + b.outTok + b.cacheTok
        return (
          padR(label, 10) +
          padL(String(b.turns), 7) +
          padL(fmtNum(b.inTok), 10) +
          padL(fmtNum(b.outTok), 10) +
          padL(fmtNum(b.cacheTok), 11) +
          padL(fmtNum(tot), 11)
        )
      }

      const cacheRatio =
        bAll.inTok + bAll.cacheTok > 0
          ? Math.round((bAll.cacheTok / (bAll.inTok + bAll.cacheTok)) * 100)
          : 0
      const avgTps = speedSec > 0 ? (speedTok / speedSec).toFixed(1) : "-"
      const streak = computeStreakDays(activeDays)
      let peakHour = 0
      for (let h = 1; h < 24; h++) {
        if (hourCounts[h] > hourCounts[peakHour]) peakHour = h
      }

      const allTotal = bAll.inTok + bAll.outTok + bAll.cacheTok
      const models = [...byModel.entries()].sort((a, b) => b[1].total - a[1].total).slice(0, 6)
      const maxModelTok = models[0]?.[1].total || 1
      const modelLines = models.map(([name, info]) => {
        const pct = Math.round((info.total / allTotal) * 100)
        return (
          padR(name, 24) +
          " " +
          bar(info.total / maxModelTok, 10) +
          padL(`${pct}%`, 5) +
          padL(fmtNum(info.total), 10) +
          padL(`(${info.turns})`, 8)
        )
      })

      const status = await controlCall("/status", "GET", undefined, 2_000)
      const proxyLine = status
        ? `Proxy: ${status.hop?.paused ? "PAUSED" : "AUTO"} · ${status.pool?.active_ip || "?"} · rotations: ${status.hop?.count ?? 0} · burnt: ${status.pool?.burnt_count ?? 0}`
        : "Proxy: offline"

      return [
        padR("Window", 10) +
          padL("Turns", 7) +
          padL("Input", 10) +
          padL("Output", 10) +
          padL("Cache", 11) +
          padL("Total", 11),
        "─".repeat(59),
        fmtRow("24h", b24),
        fmtRow("7d", b7),
        fmtRow("30d", b30),
        fmtRow("All time", bAll),
        "",
        `Cache Hit: ${cacheRatio}%   Avg Speed: ${avgTps} tok/s   Streak: ${streak}d   Peak: ${String(peakHour).padStart(2, "0")}:00`,
        "",
        "Top Models",
        ...modelLines,
        "",
        proxyLine,
      ].join("\n")
    } finally {
      db.close()
    }
  } catch (err: any) {
    return `Failed to read local DB: ${err?.message || err}`
  }
}

async function formatStatusDetails(status: any): Promise<string> {
  if (!status) {
    return `Daemon is not reachable on ${controlURL()}.`
  }
  const paused = Boolean(status.hop?.paused)
  const lines: string[] = [
    `Version:   v${status.version} (${paused ? "PAUSED" : "AUTO"})`,
    `Egress:    ${status.egress?.mode || "masque"}`,
    `Active IP: ${status.pool?.active_ip || "?"}`,
    `Rotations: ${status.hop?.count ?? 0}   Burnt today: ${status.pool?.burnt_count ?? 0}`,
  ]
  if (status.pool?.slots?.length) {
    lines.push("")
    lines.push(padR("Slot", 8) + padR("State", 10) + padR("IPv6 /64 Egress", 26) + padR("PoP", 6) + padL("Recycles", 8))
    lines.push("─".repeat(58))
    for (const sl of status.pool.slots) {
      const icon = sl.active ? "● active" : sl.state === "ready" ? "○ warm" : `◌ ${sl.state}`
      lines.push(
        padR(`slot-${sl.id}`, 8) +
          padR(icon, 10) +
          padR(sl.ip || "-", 26) +
          padR(sl.colo || "-", 6) +
          padL(String(sl.recycles ?? 0), 8),
      )
    }
  }
  return lines.join("\n")
}

async function showAlert(context: any, title: string, message: string): Promise<void> {
  const done = context.ui.dialog.alert({ title, message })
  try {
    context.ui.dialog.set({ size: "large" })
  } catch {}
  await done
}

async function doRotate(context: any): Promise<void> {
  context.ui.toast.show({
    title: "UnlimitedCode",
    message: "Rotating egress IP…",
    variant: "info",
    duration: 2000,
  })
  const res = await controlCall("/rotate", "POST", {}, ROTATE_TIMEOUT_MS)
  const st = await controlCall("/status", "GET", undefined, 2_000)
  const ip = st?.pool?.active_ip || "?"
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
              description: "Local token usage, cache hit rate, speed, streak, and top models",
              group: "UnlimitedCode",
              slash: { name: "unlimited-stats", aliases: ["ustats"] },
              palette: true,
              async run() {
                const msg = await formatTokenStats()
                await showAlert(context, "UnlimitedCode — Usage Stats", msg)
              },
            },
            {
              id: "unlimitedcode.menu",
              title: "UnlimitedCode Controls",
              description: "Status, usage stats, rotate egress IP, pause/resume",
              group: "UnlimitedCode",
              slash: { name: "unlimited" },
              palette: true,
              async run() {
                const status = await controlCall("/status", "GET", undefined, 2_000)
                const paused = Boolean(status?.hop?.paused)
                const ip = status?.pool?.active_ip || "offline"
                const ready = status?.pool?.slots?.filter((s: any) => s.state === "ready").length ?? 0
                const total = status?.pool?.slots?.length ?? 0
                const badge = !status ? "OFFLINE" : paused ? "PAUSED" : "AUTO"

                const choice = await context.ui.dialog.select({
                  title: `UnlimitedCode — ${badge}`,
                  placeholder: "Choose an action",
                  options: [
                    {
                      title: "Usage stats",
                      value: "stats",
                      description: "Token usage, cache hit, speed, streak & top models",
                    },
                    {
                      title: "Rotate egress IP now",
                      value: "rotate",
                      description: status ? `Active: ${ip} · pool ${ready}/${total}` : "Daemon offline",
                    },
                    {
                      title: paused ? "Resume auto-rotate" : "Pause auto-rotate",
                      value: paused ? "resume" : "pause",
                      description: paused
                        ? "Re-enable automatic 429 recovery"
                        : "Pause automatic 429 recovery",
                    },
                    {
                      title: "Pool & proxy details",
                      value: "details",
                      description: status
                        ? `Rotations: ${status.hop?.count ?? 0} · burnt today: ${status.pool?.burnt_count ?? 0}`
                        : "Daemon offline",
                    },
                  ],
                })

                if (choice === "stats") {
                  await showAlert(context, "UnlimitedCode — Usage Stats", await formatTokenStats())
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
                  await showAlert(context, "UnlimitedCode — Status", await formatStatusDetails(status))
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
