# Kestrel

Single-binary local egress daemon for the official OpenCode client. One Go
executable, embedded in-process Cloudflare WARP MASQUE pool, zero external
binaries, zero `app.asar` patches, zero database writes, no PATH shims.

Kestrel exposes a loopback HTTP `CONNECT` proxy for `opencode.ai` and forwards
TCP streams directly over in-process HTTP/3 MASQUE tunnels (`quic-go`).
OpenCode keeps full ownership of TLS, headers, prompts, and the free-tier gate;
Kestrel only routes the TCP bytes, rotates wire session IDs on per-session
limits, and swaps warm IPv6 `/64` egress slots in `0ms` when an IP hits quota.

## Highlights

- **Single binary (`kestrel`)**: embedded MASQUE L4 client (`internal/usque`)
  with only `quic-go` outside Go's standard library. No `warp-cli`, no
  `warp-svc`, no `usque.exe`, no TUN driver, no admin rights.
- **Instant (`0ms`) egress rotation**: keeps a warm pool (default 2 slots) on
  distinct IPv6 `/64` prefixes (IPv6-first via OS DNS + Cloudflare DoH). On
  rotation, Kestrel swaps immediately to a warm slot and recycles the burned
  slot asynchronously via key re-enrollment, blacklisting burned `/64` prefixes
  until the daily `01:05 UTC` reset.
- **Session-first retry plugin (`plugin/kestrel.ts`)**: on the first
  `provider.free-limit` 429, rotates the wire `ses_...` header in place via
  OpenCode's Plugin V2 hooks (`model.request` / `http.request`) without burning
  a clean egress IP or losing the current chat. Only requests an egress
  rotation if the IP itself is out of quota.
- **Loopback security**: strict `127.0.0.1` bind, host/port allowlist
  (`opencode.ai`, `*.opencode.ai`, port `443`), 32-byte random bearer token on
  the control API with constant-time comparison and `Host` validation.

## Build

Requires Go 1.25 or newer.

```sh
go vet ./...
go test ./...
go build -trimpath -ldflags "-s -w -X main.version=0.2.0" -o kestrel.exe ./cmd/kestrel
```

## Quick Start

```sh
# 1. Write default config, install the OpenCode plugin, configure proxy env & autostart (Windows)
kestrel setup --service --proxy

# 2. Check health
kestrel doctor
kestrel status

# 3. Manual rotation
kestrel rotate
```

Coming from an existing `openhop` shim on Windows:

```sh
kestrel setup --takeover --service --proxy
```

`--takeover` removes `openhop` entries from the user `PATH` and logon Run key
(saving a backup in `path-backup.json`; restore with `kestrel setup --restore`).

## Commands

```text
kestrel daemon   [flags]        run the proxy, MASQUE pool and control server
kestrel status   [flags] [--json]
kestrel doctor   [flags]        verify config, ports, egress, plugin and opencode
kestrel setup    [flags]        write config, install plugin, optional service/proxy
kestrel rotate   [flags] [--json]  (alias: hop)
kestrel env      [flags] [--format plain|powershell|json]
kestrel plugin   install|uninstall|path [--dir DIR]
kestrel run      [flags] -- <command> [args...]
kestrel version
```

## Configuration

State directory:
- **Windows:** `%LOCALAPPDATA%\Kestrel`
- **Linux/macOS:** `$XDG_STATE_HOME/kestrel` or `~/.local/state/kestrel`

Key settings (`config.json`, see [config.example.json](config.example.json)):
- `egress.mode`: `usque` (default, embedded MASQUE pool), `warp` (`warp-cli` local proxy), `http-connect`, or `direct`.
- `egress.pool_size`: number of warm in-process MASQUE slots (`1`–`8`, default `2`).
- `hop.max_per_hour` / `hop.max_per_day`: rolling rotation budgets (`30` / `300`, `0` disables).
- `observer.enabled`: log-tail fallback if the plugin is not installed (default `true`).

## License

MIT. See [LICENSE](LICENSE).
