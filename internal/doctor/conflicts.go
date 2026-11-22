package doctor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DetectActiveConflicts checks for currently running tools that actively
// interfere with UnlimitedCode's embedded MASQUE egress (e.g. Cloudflare WARP
// in VPN/proxy mode causing WARP-in-WARP 502s, or another OpenCode proxy/bypass
// listening on loopback). Safe, read-only, and fast (~150ms when idle).
func DetectActiveConflicts(ctx context.Context) []string {
	var out []string
	if isWARPActive(ctx) {
		out = append(out, "Cloudflare WARP is connected")
	}
	if isOtherBypassActive() {
		out = append(out, "Another OpenCode proxy/bypass tool is running")
	}
	return out
}

// ResolveActiveConflicts cleanly disconnects Cloudflare WARP (via official
// warp-cli without elevation) and stops conflicting user-level OpenCode proxy
// processes when the user explicitly opts in from the UI.
func ResolveActiveConflicts(ctx context.Context) (actions []string, remaining []string) {
	if isWARPActive(ctx) {
		if warpCLI := findWARPCLI(); warpCLI != "" {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cmd := exec.CommandContext(cctx, warpCLI, "disconnect")
			hideWindow(cmd)
			err := cmd.Run()
			cancel()
			if err == nil {
				actions = append(actions, "Disconnected Cloudflare WARP")
			}
		}
	}
	if isOtherBypassActive() {
		if stopOtherBypassProcesses(ctx) {
			actions = append(actions, "Stopped conflicting proxy process")
		}
	}
	if len(actions) > 0 {
		time.Sleep(400 * time.Millisecond)
	}
	return actions, DetectActiveConflicts(ctx)
}

func isWARPActive(ctx context.Context) bool {
	if tcpListening("127.0.0.1:40000") {
		return true
	}
	warpCLI := findWARPCLI()
	if warpCLI == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(cctx, warpCLI, "status")
	hideWindow(cmd)
	raw, err := cmd.Output()
	if err != nil {
		return false
	}
	low := strings.ToLower(string(raw))
	if strings.Contains(low, "disconnected") {
		return false
	}
	return strings.Contains(low, "connected") || strings.Contains(low, "connecting")
}

func findWARPCLI() string {
	if p, err := exec.LookPath("warp-cli"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files\Cloudflare\Cloudflare WARP\warp-cli.exe`,
			`C:\Program Files (x86)\Cloudflare\Cloudflare WARP\warp-cli.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	} else if runtime.GOOS == "darwin" {
		p := filepath.Join("/Applications", "Cloudflare WARP.app", "Contents", "Resources", "warp-cli")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func isOtherBypassActive() bool {
	// Known loopback proxy ports used by third-party OpenCode bypass tools.
	for _, addr := range []string{"127.0.0.1:41987"} {
		if tcpListening(addr) {
			return true
		}
	}
	return hasConflictingBypassProcess()
}

func tcpListening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 150*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
