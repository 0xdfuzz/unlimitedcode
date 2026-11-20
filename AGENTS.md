# AGENTS.md

Engineering rules and architecture contract for **Kestrel**.

## 1. Architecture
- Single Go binary (`cmd/kestrel`) with private packages under `internal/` and the embedded OpenCode plugin under `plugin/`.
- Default egress is an **in-process Cloudflare WARP MASQUE pool** (`internal/usque`) over HTTP/3 `CONNECT` streams (`quic-go`). No external `usque.exe`, no `warp-svc`, no TUN driver, no admin privileges.
- Transparent TCP `CONNECT` proxy (`internal/proxy`): OpenCode owns TLS, headers, prompts, and the free-tier gate. Kestrel never terminates TLS or inspects request bodies.
- Plugin (`plugin/kestrel.ts`): rotates the wire session ID (`Identifier.descending()`) on the first `provider.free-limit` hit without burning the egress IP, and requests a 0ms pool rotation from the control API only if the IP itself is out of quota.

## 2. Go Style & Minimalism
- **Stdlib first:** prefer `net/http`, `context`, `encoding/json`, `crypto/*`, `os/exec` over third-party dependencies. Only `github.com/quic-go/quic-go` is used externally for HTTP/3 MASQUE.
- **Keep comments sparse and high-signal:** explain invariants and non-obvious protocol constraints, never restate code.
- **Consumer-owned interfaces:** keep interfaces small (1–3 methods) and define them where they are consumed.
- **English everywhere:** all code, comments, logs, CLI output, and docs must be in English.

## 3. Lifecycle, Concurrency & Zero Leaks
- Every I/O, network, or worker function accepts `ctx context.Context` as its first parameter.
- Never spawn orphan goroutines: track background work with `sync.WaitGroup` and tie cancellation to `ctx.Done()`.
- Graceful shutdown via `signal.NotifyContext`: close listeners, active tunnels, and all QUIC/UDP sockets within a bounded timeout (5s).

## 4. Local Security (IPC & Proxy)
- Bind strictly to loopback (`127.0.0.1` / `::1`); never `0.0.0.0`.
- Enforce host and port allowlists (`opencode.ai`, `*.opencode.ai`, port `443`).
- Authenticate all control API endpoints (except `/health`) with a 32-byte random bearer token using constant-time comparison and loopback `Host` header validation.
- Use explicit typed structs for all JSON control payloads.

## 5. Windows Portability
- Always use `filepath.Join` and `filepath.Clean`.
- Hide console windows on child processes with `//go:build windows` (`SysProcAttr{HideWindow: true}`).
- Use atomic writes (write `.tmp` → close → `os.Rename`) for configs and state files to avoid Windows file-locking corruption.

## 6. Verification Before Merging
Always run from `kestrel/`:
```sh
go vet ./...
go test -race ./...
go build -o kestrel.exe ./cmd/kestrel
```
