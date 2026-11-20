package doctor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kestrel/internal/config"
	"kestrel/internal/control"
	"kestrel/internal/opencode"
	"kestrel/internal/upstream"
	"kestrel/internal/warp"
	"kestrel/plugin"
)

const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
)

type Check struct {
	Name   string
	Level  string
	Detail string
}

func Run(ctx context.Context, cfg config.Config, version string) []Check {
	var checks []Check
	add := func(name, level, detail string) {
		checks = append(checks, Check{Name: name, Level: level, Detail: detail})
	}

	if _, err := os.Stat(cfg.Path); err == nil {
		add("config", OK, cfg.Path)
	} else if os.IsNotExist(err) {
		add("config", Warn, cfg.Path+" (missing, using defaults)")
	} else {
		add("config", Fail, err.Error())
	}

	home := config.Home()
	if err := writable(home); err != nil {
		add("home", Fail, fmt.Sprintf("%s: %v", home, err))
	} else {
		add("home", OK, home)
	}

	daemonUp := false
	if detail, err := health(ctx, cfg.Control); err == nil {
		daemonUp = true
		add("daemon", OK, detail)
	} else {
		add("daemon", Warn, "not running")
	}

	for _, item := range []struct {
		name string
		addr string
	}{
		{"listen", cfg.Listen},
		{"control", cfg.Control},
	} {
		if err := portFree(item.addr); err != nil {
			if daemonUp {
				add(item.name, OK, item.addr+" (in use by kestrel)")
			} else {
				add(item.name, Fail, fmt.Sprintf("%s: %v", item.addr, err))
			}
			continue
		}
		add(item.name, OK, item.addr+" (free)")
	}

	if cfg.Egress.Mode == config.ModeUsque {
		add("egress", OK, fmt.Sprintf("usque -> embedded MASQUE pool (size=%d, state=%s)", cfg.Egress.PoolSize, config.UsqueDir()))
	} else {
		dialer, err := upstream.Build(cfg)
		if err != nil {
			add("egress", Fail, err.Error())
		} else {
			add("egress", OK, fmt.Sprintf("%s -> %s", cfg.Egress.Mode, dialer.Name()))
			checkGate(ctx, dialer, add)
			if cfg.Egress.Mode == config.ModeWarp || cfg.Egress.Mode == config.ModeHTTPConnect {
				checkWarp(ctx, cfg, add)
			}
		}
	}

	if path, err := opencode.Find(); err != nil {
		add("opencode", Fail, err.Error())
	} else {
		detail := path
		if v, err := opencode.Version(ctx, path); err == nil && v != "" {
			detail += " (" + v + ")"
		}
		add("opencode", OK, detail)
	}

	for _, shim := range opencode.Shims() {
		if opencode.IsShim(shim) {
			add("path", Warn, shim+" is a shim; kestrel run resolves the real binary")
			break
		}
	}

	checkHop(ctx, cfg, add)
	checkObserver(cfg, add)
	checkResume(cfg, add)
	checkPlugin(add)

	return checks
}

func checkHop(ctx context.Context, cfg config.Config, add func(string, string, string)) {
	if !cfg.Hop.Enabled {
		add("hop", OK, "disabled by config")
		return
	}
	if cfg.Egress.Mode == config.ModeUsque {
		add("hop", OK, fmt.Sprintf("embedded MASQUE pool=%d timeout=%ds cooldown=%ds", cfg.Egress.PoolSize, cfg.Hop.TimeoutSeconds, cfg.Hop.FailureCooldownSecs))
		return
	}
	if cfg.Egress.Mode != config.ModeWarp {
		add("hop", Warn, "requires egress.mode=warp or usque")
		return
	}
	path, err := warp.Find(cfg.Hop.WarpCLI)
	if err != nil {
		add("hop", Fail, err.Error())
		return
	}
	detail := fmt.Sprintf("%s timeout=%ds cooldown=%ds", path, cfg.Hop.TimeoutSeconds, cfg.Hop.FailureCooldownSecs)
	statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	state, err := warp.New(path).Status(statusCtx)
	if err != nil {
		add("hop", Warn, detail+"; status: "+err.Error())
		return
	}
	if strings.EqualFold(state, "Connected") {
		add("hop", OK, detail+"; warp="+state)
		return
	}
	add("hop", Warn, detail+"; warp="+state)
}

func checkObserver(cfg config.Config, add func(string, string, string)) {
	if !cfg.Observer.Enabled {
		add("observer", OK, "disabled by config")
		return
	}
	path := cfg.Observer.LogPath
	if path == "" {
		path = opencode.LogFile()
	}
	if path == "" {
		add("observer", Fail, "cannot resolve the OpenCode log path")
		return
	}
	detail := fmt.Sprintf("%s poll=%ds dedup=%ds grace=%ds",
		path, cfg.Observer.PollSeconds, cfg.Observer.DedupSeconds, cfg.Observer.PluginGraceSeconds)
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		add("observer", Warn, detail+"; log not present yet")
	case err != nil:
		add("observer", Fail, err.Error())
	case info.IsDir():
		add("observer", Fail, path+" is a directory")
	default:
		add("observer", OK, detail)
	}
}

func checkResume(cfg config.Config, add func(string, string, string)) {
	if !cfg.Resume.Enabled {
		add("resume", OK, "disabled (opt-in fallback)")
		return
	}
	binary := cfg.Resume.Binary
	if binary == "" {
		found, err := opencode.Find()
		if err != nil {
			add("resume", Fail, err.Error())
			return
		}
		binary = found
	}
	info, err := os.Stat(binary)
	if err != nil {
		add("resume", Fail, fmt.Sprintf("%s: %v", binary, err))
		return
	}
	if info.IsDir() {
		add("resume", Fail, binary+" is a directory")
		return
	}
	if opencode.IsShim(binary) {
		add("resume", Fail, binary+" is the openhop shim or a launcher")
		return
	}
	add("resume", OK, fmt.Sprintf("%s timeout=%ds sends \"continue\"", binary, cfg.Resume.TimeoutSeconds))
}

func checkPlugin(add func(string, string, string)) {
	dir, err := plugin.Dir()
	if err != nil {
		add("plugin", Warn, err.Error())
		return
	}
	path := filepath.Join(dir, plugin.Filename)
	current, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		add("plugin", Warn, "not installed: run kestrel plugin install")
		return
	}
	if err != nil {
		add("plugin", Fail, err.Error())
		return
	}
	if bytes.Equal(current, plugin.Source) {
		add("plugin", OK, path)
		return
	}
	add("plugin", Warn, path+" differs from the embedded plugin (rerun kestrel plugin install)")
}

func checkGate(ctx context.Context, dialer upstream.Dialer, add func(string, string, string)) {
	start := time.Now()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := dialer.DialContext(dialCtx, "tcp", "opencode.ai:443")
	if err != nil {
		add("gate", Fail, fmt.Sprintf("opencode.ai:443 via %s: %v", dialer.Name(), err))
		return
	}
	conn.Close()
	add("gate", OK, fmt.Sprintf("opencode.ai:443 via %s in %s", dialer.Name(), time.Since(start).Round(time.Millisecond)))
}

func checkWarp(ctx context.Context, cfg config.Config, add func(string, string, string)) {
	dialer, err := upstream.Build(cfg)
	if err != nil {
		add("warp", Fail, err.Error())
		return
	}
	start := time.Now()
	trace, err := fetchTrace(ctx, dialer)
	if err != nil {
		add("warp", Fail, err.Error())
		return
	}
	detail := fmt.Sprintf("warp=%s ip=%s colo=%s in %s",
		trace["warp"], trace["ip"], trace["colo"], time.Since(start).Round(time.Millisecond))
	switch {
	case trace["warp"] == "on":
		add("warp", OK, detail)
	case cfg.Egress.Mode == config.ModeWarp:
		add("warp", Fail, detail+" (WARP proxy mode required)")
	default:
		add("warp", Warn, detail)
	}
}

func fetchTrace(ctx context.Context, dialer upstream.Dialer) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	raw, err := dialer.DialContext(ctx, "tcp", "www.cloudflare.com:443")
	if err != nil {
		return nil, fmt.Errorf("CONNECT www.cloudflare.com:443: %w", err)
	}
	defer raw.Close()

	tlsConn := tls.Client(raw, &tls.Config{ServerName: "www.cloudflare.com", MinVersion: tls.VersionTLS12})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	deadline := time.Now().Add(12 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	tlsConn.SetDeadline(deadline)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return nil, err
	}
	if err := req.Write(tlsConn); err != nil {
		return nil, fmt.Errorf("write trace request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		return nil, fmt.Errorf("read trace response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trace status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	if values["warp"] == "" {
		return nil, fmt.Errorf("trace did not report warp state")
	}
	return values, nil
}

func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func portFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

func health(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, control.URL(addr)+"/health", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("health status %d", resp.StatusCode)
	}
	var payload struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&payload); err != nil {
		return "", err
	}
	if !payload.OK {
		return "", fmt.Errorf("health not ok")
	}
	return fmt.Sprintf("running %s at %s", payload.Version, addr), nil
}
