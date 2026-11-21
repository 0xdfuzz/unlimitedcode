# AGENTS.md

Engineering rules and architecture contract for **UnlimitedCode**.

## 1. Package Map
- `cmd/unlimitedcode/`: CLI entrypoint split by command (`main.go`, `daemon.go`, `install.go`, `update.go`, `status.go`, `parent_windows.go`, `parent_other.go`).
- `internal/masque/`: in-process Cloudflare WARP MASQUE L4 client (`client.go`) and warm rotation pool (`pool.go`) with IPv6-first DoH resolution and burned-prefix tracking.
- `internal/proxy/`: transparent HTTP `CONNECT` proxy with split routing (routed hosts via MASQUE egress, other hosts via direct passthrough when enabled) and pause/close gate for clean rotation.
- `internal/rotate/`: single-flight rotation coordinator with failure cooldown and rolling hourly/daily budgets.
- `internal/control/`: loopback-only authenticated HTTP control API (`/health`, `/status`, `/rotate`, `/event`, `/shutdown`).
- `internal/config/`: JSON config loading, defaults, env overrides, and validation.
- `internal/setup/`: first-run config creation (`installed_at` marker), OpenCode `service*.json` env updater/cleaner, binary swap, and legacy logon autostart cleanup.
- `internal/update/`: opt-in GitHub Releases checker with shared cache and SHA-256 checksum verification.
- `internal/doctor/`: environment and connectivity diagnostics.
- `internal/opencode/`: official OpenCode CLI + Desktop binary locator.
- `internal/upstream/`: `Dialer` interface and `direct` / `http-connect` implementations.
- `internal/logx/`: rotating file logger.
- `plugin/`: embedded OpenCode Plugin V2 bundle (`unlimitedcode.ts`, `tui.tsx`, `gui-experimental.js`) and installer (`plugin.go`).

## 2. Go Style & Minimalism
- **Stdlib first:** only `github.com/quic-go/quic-go` is used outside Go's standard library (for HTTP/3 MASQUE).
- **Comments:** explain invariants and non-obvious protocol constraints; never restate code.
- **Consumer-owned interfaces:** keep interfaces small (1–3 methods) and define them where consumed.
- **English everywhere:** all code, comments, logs, CLI output, and docs must be in English.

## 3. Lifecycle, Concurrency & Zero Leaks
- Every I/O, network, or worker function accepts `ctx context.Context` as its first parameter.
- Track background goroutines with `sync.WaitGroup` and tie cancellation to `ctx.Done()`.
- Graceful shutdown via `signal.NotifyContext`: close listeners, active tunnels, and all QUIC/UDP sockets within a bounded timeout (5s).

## 4. Local Security (IPC & Proxy)
- Bind strictly to loopback (`127.0.0.1` / `::1`); never `0.0.0.0`.
- Authenticate all control API endpoints (except `/health`) with a 32-byte random bearer token using constant-time comparison and loopback `Host` validation.
- Use explicit typed structs for all JSON control payloads.

## 5. Windows Portability
- Always use `filepath.Join` and `filepath.Clean`.
- Hide console windows on child processes with `//go:build windows` (`SysProcAttr{HideWindow: true}`).
- Use atomic writes (write `.tmp` -> close -> `os.Rename`) for configs and state files.

## 6. Verification Before Merging
Always run from `UnlimitedCode/`:
```sh
go vet ./...
go test -race ./...
go build -trimpath -ldflags "-s -w" -o unlimitedcode.exe ./cmd/unlimitedcode
```
