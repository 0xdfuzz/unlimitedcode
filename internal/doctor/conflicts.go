package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"unlimitedcode/internal/setup"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// knownBypassPorts lists loopback ports uniquely opened by third-party OpenCode
// proxy/bypass tools (service proxies, HTTP CONNECT gates, Tor/SOCKS bridges).
var knownBypassPorts = []string{
	"127.0.0.1:41987",
	"127.0.0.1:18120",
	"127.0.0.1:18118",
	"127.0.0.1:18119",
}

// conflictingServicePorts lists loopback ports that indicate another proxy tool
// hijacked OpenCode's service.json HTTPS_PROXY.
var conflictingServicePorts = []string{
	":40000",
	":41987",
	":18120",
	":18118",
	":18119",
	":8118",
}

const (
	// ConflictWARPConnected is reported when Cloudflare WARP's tunnel/VPN mode is
	// actively blocking UnlimitedCode's embedded MASQUE egress.
	ConflictWARPConnected = "Cloudflare WARP is connected"
	// ConflictOtherBypassActive is reported when another OpenCode proxy/bypass
	// tool or hijacked service.json is competing with UnlimitedCode.
	ConflictOtherBypassActive = "Another OpenCode proxy/bypass tool is active"
)

// DetectProxyConflicts checks strictly for competing OpenCode proxy/bypass
// tools, active configs, or hijacked service.json entries. Unlike WARP tunnel
// mode (which may coexist cleanly on consumer setups), these directly fight
// UnlimitedCode for OpenCode traffic and should always be surfaced.
func DetectProxyConflicts(expectedProxy string) []string {
	if isOtherBypassActive(expectedProxy) {
		return []string{ConflictOtherBypassActive}
	}
	return nil
}

// DetectWARPConflict reports whether Cloudflare WARP is connected in a tunnel
// or VPN mode that can intercept UnlimitedCode's MASQUE UDP traffic. Local-only
// WarpProxy mode (:40000) does not capture system UDP routes and is ignored
// unless OpenCode's service.json was hijacked to :40000 (handled by
// DetectProxyConflicts).
func DetectWARPConflict(ctx context.Context) bool {
	return isWARPTunnelActive(ctx)
}

// DetectActiveConflicts checks for both active WARP tunnel mode and competing
// OpenCode proxy/bypass tools. Safe, read-only, and fast (~150ms when idle).
func DetectActiveConflicts(ctx context.Context, expectedProxy string) []string {
	var out []string
	if isWARPTunnelActive(ctx) {
		out = append(out, ConflictWARPConnected)
	}
	if isOtherBypassActive(expectedProxy) {
		out = append(out, ConflictOtherBypassActive)
	}
	return out
}

// ResolveActiveConflicts cleanly disconnects Cloudflare WARP (via official
// warp-cli without elevation), disables conflicting user-level OpenCode proxy
// configs/tasks/processes, and restores OpenCode's service.json when hijacked.
func ResolveActiveConflicts(ctx context.Context, expectedProxy string) (actions []string, remaining []string) {
	if isWARPConnected(ctx) {
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
	if disableActiveBypassConfigs(bypassConfigPaths()) {
		actions = append(actions, "Disabled conflicting OpenCode proxy config")
	}
	if isOtherBypassProcessOrPortActive() {
		if stopOtherBypassProcesses(ctx) {
			actions = append(actions, "Stopped conflicting proxy process")
		}
	}
	if expectedProxy != "" && hasHijackedServiceProxy(expectedProxy) {
		if _, err := setup.SetOpenCodeServiceEnv(expectedProxy); err == nil {
			actions = append(actions, "Restored OpenCode proxy settings")
		}
	}
	if len(actions) > 0 {
		time.Sleep(400 * time.Millisecond)
	}
	return actions, DetectActiveConflicts(ctx, expectedProxy)
}

func isWARPConnected(ctx context.Context) bool {
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

func isWARPTunnelActive(ctx context.Context) bool {
	if !isWARPConnected(ctx) {
		return false
	}
	// If WARP is in local-only WarpProxy mode (SOCKS/HTTP proxy on :40000),
	// it does not install a system TUN route and cannot block MASQUE UDP 443.
	warpCLI := findWARPCLI()
	if warpCLI == "" {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(cctx, warpCLI, "settings")
	hideWindow(cmd)
	raw, err := cmd.Output()
	if err != nil {
		return true
	}
	return !isWarpProxySettings(string(raw))
}

func isWarpProxySettings(settingsOutput string) bool {
	for _, line := range strings.Split(settingsOutput, "\n") {
		low := strings.ToLower(strings.TrimSpace(line))
		if strings.Contains(low, "mode:") && strings.Contains(low, "warpproxy") {
			return true
		}
	}
	return false
}

func findWARPCLI() string {
	if p, err := exec.LookPath("warp-cli"); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		candidates := []string{
			`C:\Program Files\Cloudflare\Cloudflare WARP\warp-cli.exe`,
			`C:\Program Files (x86)\Cloudflare\Cloudflare WARP\warp-cli.exe`,
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			candidates = append(candidates, filepath.Join(local, "Cloudflare", "Cloudflare WARP", "warp-cli.exe"))
		}
		for _, p := range candidates {
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

func isOtherBypassActive(expectedProxy string) bool {
	if isOtherBypassProcessOrPortActive() {
		return true
	}
	if hasActiveBypassConfig(bypassConfigPaths()) {
		return true
	}
	if expectedProxy != "" && hasHijackedServiceProxy(expectedProxy) {
		return true
	}
	return false
}

func isOtherBypassProcessOrPortActive() bool {
	for _, addr := range knownBypassPorts {
		if tcpListening(addr) {
			return true
		}
	}
	return hasConflictingBypassProcess()
}

func bypassConfigPaths() []string {
	var paths []string
	switch runtime.GOOS {
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			paths = append(paths,
				filepath.Join(appdata, "OpenCode", "infinite.json"),
				filepath.Join(appdata, "ai.opencode.desktop", "infinite.json"),
			)
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			base := filepath.Join(home, "Library", "Application Support")
			paths = append(paths,
				filepath.Join(base, "OpenCode", "infinite.json"),
				filepath.Join(base, "ai.opencode.desktop", "infinite.json"),
			)
		}
	default:
		cfgRoot := os.Getenv("XDG_CONFIG_HOME")
		if cfgRoot == "" {
			if home, err := os.UserHomeDir(); err == nil {
				cfgRoot = filepath.Join(home, ".config")
			}
		}
		if cfgRoot != "" {
			paths = append(paths,
				filepath.Join(cfgRoot, "OpenCode", "infinite.json"),
				filepath.Join(cfgRoot, "ai.opencode.desktop", "infinite.json"),
			)
		}
	}
	return paths
}

func hasActiveBypassConfig(paths []string) bool {
	for _, p := range paths {
		if isBypassConfigActive(p) {
			return true
		}
	}
	return false
}

func isBypassConfigActive(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return false
	}
	raw = bytes.TrimPrefix(raw, utf8BOM)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	if mode, ok := doc["mode"].(string); ok {
		mode = strings.TrimSpace(mode)
		if mode != "" && !strings.EqualFold(mode, "NONE") {
			return true
		}
	}
	if auto, ok := doc["auto"].(bool); ok && auto {
		return true
	}
	return false
}

func disableActiveBypassConfigs(paths []string) bool {
	changed := false
	for _, p := range paths {
		if !isBypassConfigActive(p) {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		raw = bytes.TrimPrefix(raw, utf8BOM)
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		doc["mode"] = "NONE"
		doc["auto"] = false
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			continue
		}
		out = append(out, '\n')
		if writeFileAtomic(p, out, 0o600) == nil {
			changed = true
		}
	}
	return changed
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".uc-fix-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	_ = os.Chmod(tmpName, perm)
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return os.WriteFile(path, data, perm)
	}
	return nil
}

func opencodeServiceFiles() []string {
	var dir string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dir = filepath.Join(xdg, "opencode")
	} else if home, err := os.UserHomeDir(); err == nil {
		dir = filepath.Join(home, ".config", "opencode")
	}
	if dir == "" {
		return nil
	}
	files := []string{filepath.Join(dir, "service.json")}
	if matches, err := filepath.Glob(filepath.Join(dir, "service-*.json")); err == nil {
		files = append(files, matches...)
	}
	return files
}

func hasHijackedServiceProxy(expectedProxy string) bool {
	return hasHijackedServiceProxyInFiles(opencodeServiceFiles(), expectedProxy)
}

func hasHijackedServiceProxyInFiles(files []string, expectedProxy string) bool {
	want := strings.ToLower(strings.TrimRight(strings.TrimSpace(expectedProxy), "/"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil || len(raw) == 0 {
			continue
		}
		raw = bytes.TrimPrefix(raw, utf8BOM)
		var doc struct {
			Env map[string]any `json:"env"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || doc.Env == nil {
			continue
		}
		for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			val, ok := doc.Env[k].(string)
			if !ok {
				continue
			}
			low := strings.ToLower(strings.TrimRight(strings.TrimSpace(val), "/"))
			if low == "" || low == want {
				continue
			}
			if !strings.Contains(low, "127.0.0.1") && !strings.Contains(low, "localhost") {
				continue
			}
			for _, port := range conflictingServicePorts {
				if strings.HasSuffix(low, port) {
					return true
				}
			}
		}
	}
	return false
}

func tcpListening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 150*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
