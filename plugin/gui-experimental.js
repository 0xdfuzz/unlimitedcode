"use strict"

const fs = require("node:fs")
const os = require("node:os")
const path = require("node:path")
const { Schema } = require("effect")
const sdk = require("@opencode/plugin/gui")
const { createSignal, onCleanup, onMount } = require("solid-js")

const DEFAULT_CONTROL = "http://127.0.0.1:42988"

function stateHome() {
  if (process.env.UNLIMITEDCODE_HOME) return process.env.UNLIMITEDCODE_HOME
  if (process.platform === "win32" && process.env.LOCALAPPDATA) {
    return path.join(process.env.LOCALAPPDATA, "UnlimitedCode")
  }
  if (process.env.XDG_STATE_HOME) return path.join(process.env.XDG_STATE_HOME, "unlimitedcode")
  return path.join(os.homedir(), ".local", "state", "unlimitedcode")
}

function controlURL() {
  const raw = process.env.UNLIMITEDCODE_CONTROL || DEFAULT_CONTROL
  return raw.startsWith("http") ? raw.replace(/\/+$/, "") : `http://${raw}`
}

function controlToken() {
  try {
    return fs.readFileSync(path.join(stateHome(), "control.token"), "utf8").trim()
  } catch {
    return ""
  }
}

function shortenIPv6(ip) {
  if (!ip || !ip.includes(":")) return ip || "-"
  const parts = ip.split(":").filter((p) => p.length > 0)
  if (parts.length <= 4) return ip
  return `${parts[0]}:${parts[1]}::${parts[parts.length - 2]}:${parts[parts.length - 1]}`
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
    if (!res.ok) return null
    return await res.json().catch(() => null)
  } catch {
    return null
  } finally {
    clearTimeout(timer)
  }
}

const StatusSchema = Schema.Struct({
  online: Schema.Boolean,
  paused: Schema.Boolean,
  ip: Schema.String,
  ready: Schema.Number,
  total: Schema.Number,
  rotations: Schema.Number,
  burnt: Schema.Number,
})

const RotateResultSchema = Schema.Struct({
  ok: Schema.Boolean,
  ip: Schema.String,
  durationMs: Schema.Number,
})

const UnlimitedIpc = sdk.Ipc.define({
  id: "unlimitedcode.daemon",
  methods: {
    status: { output: StatusSchema },
    rotate: { output: RotateResultSchema },
    setPaused: { input: Schema.Boolean, output: StatusSchema },
  },
})

async function fetchSummary() {
  const st = await controlCall("/status", "GET", undefined, 2000)
  if (!st) {
    return { online: false, paused: false, ip: "offline", ready: 0, total: 0, rotations: 0, burnt: 0 }
  }
  const slots = Array.isArray(st.pool?.slots) ? st.pool.slots : []
  const ready = slots.filter((s) => s.state === "ready").length
  return {
    online: true,
    paused: Boolean(st.hop?.paused),
    ip: shortenIPv6(st.pool?.active_ip || "?"),
    ready,
    total: slots.length,
    rotations: Number(st.hop?.count ?? 0),
    burnt: Number(st.pool?.burnt_count ?? 0),
  }
}

const DOT_SVG =
  "<svg viewBox='0 0 16 16' fill='none' xmlns='http://www.w3.org/2000/svg'><circle cx='8' cy='8' r='4.5' fill='currentColor'/></svg>"

exports.default = sdk.Plugin.define({
  icons: {
    dot: DOT_SVG,
  },
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
      async setPaused(paused) {
        await controlCall("/pause", "POST", { paused: Boolean(paused) }, 4000)
        return await fetchSummary()
      },
    })
  },
  setup(ctx) {
    const [summary, setSummary] = createSignal({
      online: true,
      paused: false,
      ip: "…",
      ready: 0,
      total: 0,
      rotations: 0,
      burnt: 0,
    })

    const refresh = async () => {
      try {
        const next = await ctx.main(UnlimitedIpc).status()
        if (next) setSummary(next)
      } catch {}
    }

    onMount(() => {
      void refresh()
      const timer = setInterval(() => {
        void refresh()
      }, 15000)
      onCleanup(() => clearInterval(timer))
    })

    const runRotate = async () => {
      try {
        await ctx.main(UnlimitedIpc).rotate()
        await refresh()
      } catch {}
    }

    const togglePause = async () => {
      try {
        const next = await ctx.main(UnlimitedIpc).setPaused(!summary().paused)
        if (next) setSummary(next)
      } catch {}
    }

    ctx.add(sdk.ComposerPill, {
      id: "status",
      order: 50,
      show() {
        const s = summary()
        const tone = !s.online ? "danger" : s.paused ? "warning" : "success"
        const stateLabel = !s.online ? "OFFLINE" : s.paused ? "PAUSED" : "AUTO"
        return {
          label: "unlimited",
          icon: ctx.icons.dot,
          tone,
          title: `UnlimitedCode · ${stateLabel}`,
          rows: [
            {
              id: "egress",
              label: "Egress IP",
              detail: s.online ? `${s.ip} (${s.ready}/${s.total})` : "offline",
              tone,
            },
            {
              id: "counters",
              label: "Rotations",
              detail: `${s.rotations} · burnt ${s.burnt}`,
            },
            {
              id: "rotate",
              label: "Rotate egress IP now",
              run() {
                void runRotate()
              },
            },
            {
              id: "pause",
              label: s.paused ? "Resume auto-rotate" : "Pause auto-rotate",
              run() {
                void togglePause()
              },
            },
          ],
        }
      },
    })

    ctx.add(sdk.Command, () => ({
      id: "rotate",
      title: "UnlimitedCode: Rotate egress IP",
      description: "Switch to a fresh IPv6 /64 MASQUE slot immediately",
      group: "UnlimitedCode",
      slash: { name: "rotate" },
      run() {
        void runRotate()
      },
    }))
  },
})
