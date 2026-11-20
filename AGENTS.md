# AGENTS.md

Engineering rules and architecture contract for **Kestrel**.

## 1. Package Map
- `cmd/kestrel/`: CLI entrypoint split by command (`main.go`, `daemon.go`, `setup.go`, `status.go`, `run.go`).
- `internal/usque/`: in-process Cloudflare WARP MASQUE L4 client (`masque.go`) and warm rotation pool (`pool.go`) with IPv6-first DoH resolution and burned-prefix tracking.
- `internal/proxy/`: transparent HTTP `CONNECT` proxy with split routing (routed hosts via MASQUE egress, other hosts via direct passthrough when enabled) and pause/close gate for clean rotation.
- `internal/hop/`: single-flight rotation coordinator with failure cooldown and rolling hourly/daily budgets.
- `internal/control/`: loopback-only authenticated HTTP control API (`/health`, `/status`, `/rotate`, `/event`).
- `internal/config/`: JSON config loading, defaults, env overrides, and validation.
- `internal/observer/`: OpenCode service log tail fallback when the plugin is absent.
- `internal/resume/`: opt-in `opencode run --session <id> continue` fallback.
- `internal/setup/`: first-run config creation, Windows logon autostart (`Kestrel.vbs`), and optional `openhop` PATH takeover/restore.
- `internal/doctor/`: environment and connectivity diagnostics.
- `internal/opencode/`: official OpenCode binary and log locator.
- `internal/upstream/`: `Dialer` interface and `direct` / `http-connect` implementations.
- `internal/activity/`: plugin/observer cross-suppression timestamp tracker.
- `internal/logx/`: rotating file logger.
- `plugin/`: embedded OpenCode Plugin V2 (`kestrel.ts`) and installer (`plugin.go`).

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
Always run from `kestrel/`:
```sh
go vet ./...
go test -race ./...
go build -trimpath -ldflags "-s -w" -o kestrel.exe ./cmd/kestrel
```
