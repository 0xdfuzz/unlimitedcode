# UnlimitedCode

Single-binary local egress plugin & in-process Cloudflare WARP MASQUE pool for
the official OpenCode client. One Go executable (~9.5 MB), zero external
binaries, zero `app.asar` patches, zero database writes, no PATH shims, and no
logon autostart required.

UnlimitedCode lives inside OpenCode as a native Plugin V2 extension: when you
open OpenCode, the plugin starts the local daemon on demand (tied to OpenCode's
PID so it exits automatically when OpenCode closes). `opencode.ai` traffic is
forwarded over in-process HTTP/3 MASQUE tunnels (`quic-go`), while all other
hosts (browser, `webfetch`, docs, localhost) pass through directly.

## Highlights

- **Single binary (`unlimitedcode`, ~9.5 MB)**: embedded MASQUE L4 client
  (`internal/masque`) with only `quic-go` outside Go's standard library. No
  `warp-cli`, no `warp-svc`, no TUN driver, no admin rights.
- **Plugin-first lifecycle**: no tray icon, no boot autostart needed. OpenCode
  starts `unlimitedcode daemon --parent-pid <pid>` on demand and the daemon
  exits cleanly when OpenCode closes.
- **Instant (`0ms`) IPv6 `/64` rotation**: keeps a warm pool (default 2 slots)
  on distinct IPv6 `/64` prefixes (IPv6-first via OS DNS + Cloudflare DoH). On
  rotation, swaps immediately to a warm slot and recycles the burned slot
  asynchronously via key re-enrollment, blacklisting burned `/64` prefixes until
  the daily `01:05 UTC` reset.
- **Session-first invisible retry (`plugin/unlimitedcode.ts`)**: on the first
  `provider.free-limit` 429, rotates the wire `ses_...` header in place via
  OpenCode's `model.request` / `http.request` hooks without burning a clean IP
  or losing the current chat. Only swaps egress IPs if the IP itself is out of
  quota.
- **Native OpenCode TUI & Desktop UI + local token stats (`plugin/tui.tsx`, `plugin/gui-experimental.js`)**:
  - `/unlimited` — interactive control menu & MASQUE pool inspector (active IPv6, slots, recycles, rotate, daemon restart).
  - `/unlimited-stats` (alias `/ustats`) — 100% local read-only token stats modal from `opencode.db` for OpenCode free models since installation, with `24h` / `7d` / `30d` / `All` window switching, Cache Hit %, per-model `tok/s`, streak, and peak hour (zero LLM turn, works without an OpenCode account).
  - `/rotate` — immediate `0ms` egress IP rotation with native toast feedback.
  - Subtle clickable `unlimited` status indicator in both the TUI prompt footer and the Desktop session header.
- **Split routing**: `opencode.ai` / `*.opencode.ai` always route fail-closed
  through the MASQUE pool; all other hosts pass through directly so agent web
  tools keep working.
- **Loopback security**: strict `127.0.0.1` bind, 32-byte random bearer token
  on the control API with constant-time comparison and `Host` validation.

## Install (Quick Start)

**Windows (PowerShell — skips SmartScreen prompt):**

```powershell
$r = irm https://api.github.com/repos/0xdfuzz/unlimitedcode/releases/latest
$a = $r.assets | Where-Object name -like "*_windows_amd64.exe" | Select-Object -First 1
iwr $a.browser_download_url -OutFile "$env:TEMP\unlimitedcode.exe"
& "$env:TEMP\unlimitedcode.exe" install
```

Or download `unlimitedcode_*_windows_amd64.exe` from [Releases](https://github.com/0xdfuzz/unlimitedcode/releases/latest), double-click it, and pick **`[1] Install`** (if Windows SmartScreen prompts for an unsigned binary, click *More info → Run anyway*).

**Linux / macOS:**

```sh
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
URL=$(curl -s https://api.github.com/repos/0xdfuzz/unlimitedcode/releases/latest | grep -o "https://[^\"]*_${OS}_${ARCH}\"" | tr -d '"' | head -n1)
curl -fsSL "$URL" -o /tmp/unlimitedcode && chmod +x /tmp/unlimitedcode && /tmp/unlimitedcode install
```

Restart OpenCode and use `/unlimited`, `/unlimited-stats`, or `/rotate`.

Running `unlimitedcode` again opens the maintenance menu (`Repair /
Reinstall`, `Status & Diagnostics`, `Uninstall`). To uninstall from the CLI:

```sh
unlimitedcode uninstall
```

## Build from Source

Requires Go 1.25 or newer.

```sh
go vet ./...
go test ./...
go build -trimpath -ldflags "-s -w -X main.version=0.3.2" -o unlimitedcode.exe ./cmd/unlimitedcode
```

## CLI Commands

```text
unlimitedcode              interactive menu
unlimitedcode install      install binary, plugin & OpenCode proxy env
unlimitedcode update       check & install latest GitHub release
unlimitedcode uninstall    remove plugin, proxy env, and state
unlimitedcode status       show running daemon status (--json)
unlimitedcode rotate       rotate active egress IP now (--json)
unlimitedcode doctor       check environment & connectivity
unlimitedcode version      print version
```

## Zero Configuration

UnlimitedCode works out of the box with no configuration file needed (built-in
2-slot IPv6 MASQUE pool on `127.0.0.1:42987`, direct passthrough for all
non-OpenCode hosts). Advanced overrides can optionally be placed in
`config.json` inside the state directory (`%LOCALAPPDATA%\UnlimitedCode` on
Windows, `~/.local/state/unlimitedcode` on Linux/macOS — see
[config.example.json](config.example.json)).

## Acknowledgments

- [Diniboy1123/usque](https://github.com/Diniboy1123/usque) — Cloudflare WARP MASQUE registration, key enrollment, and HTTP/3 L4 `CONNECT` protocol research.
- [quic-go/quic-go](https://github.com/quic-go/quic-go) — pure-Go QUIC and HTTP/3 transport powering the embedded MASQUE client.
- [OpenCode](https://opencode.ai) — official client and Plugin V2 hook architecture.

## License

MIT. See [LICENSE](LICENSE).
