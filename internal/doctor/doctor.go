package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/control"
	"unlimitedcode/internal/opencode"
	"unlimitedcode/internal/upstream"
	"unlimitedcode/plugin"
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
				add(item.name, OK, item.addr+" (in use by UnlimitedCode)")
			} else {
				add(item.name, Fail, fmt.Sprintf("%s: %v", item.addr, err))
			}
			continue
		}
		add(item.name, OK, item.addr+" (free)")
	}

	if cfg.Egress.Mode == config.ModeMasque || cfg.Egress.Mode == config.ModeUsque {
		add("egress", OK, fmt.Sprintf("masque -> embedded pool (size=%d, state=%s)", cfg.Egress.PoolSize, config.MasqueDir()))
	} else {
		dialer, err := upstream.Build(cfg)
		if err != nil {
			add("egress", Fail, err.Error())
		} else {
			add("egress", OK, fmt.Sprintf("%s -> %s", cfg.Egress.Mode, dialer.Name()))
			checkGate(ctx, dialer, add)
		}
	}

	if path, err := opencode.Find(); err != nil {
		add("opencode", Warn, "CLI not found on PATH (Desktop-only install uses ~/.config/opencode)")
	} else {
		detail := path
		if v, err := opencode.Version(ctx, path); err == nil && v != "" {
			detail += " (" + v + ")"
		}
		add("opencode", OK, detail)
	}

	for _, shim := range opencode.Shims() {
		if opencode.IsOpenhopShim(shim) {
			add("path", Warn, shim+" is an openhop shim; remove it from PATH so official OpenCode runs cleanly")
			break
		}
	}

	checkRotate(cfg, add)
	checkPlugin(add)

	return checks
}

func checkRotate(cfg config.Config, add func(string, string, string)) {
	if !cfg.Rotate.Enabled {
		add("rotate", OK, "disabled by config")
		return
	}
	if cfg.Egress.Mode == config.ModeMasque || cfg.Egress.Mode == config.ModeUsque {
		add("rotate", OK, fmt.Sprintf("embedded MASQUE pool=%d timeout=%ds cooldown=%ds", cfg.Egress.PoolSize, cfg.Rotate.TimeoutSeconds, cfg.Rotate.FailureCooldownSecs))
		return
	}
	add("rotate", Warn, "requires egress.mode=masque")
}

func checkPlugin(add func(string, string, string)) {
	dir, err := plugin.Dir()
	if err != nil {
		add("plugin", Warn, err.Error())
		return
	}
	files := []struct {
		name string
		want []byte
	}{
		{plugin.Filename, plugin.Source},
		{plugin.TUIFilename, plugin.TUISource},
		{plugin.GUIFilename, plugin.GUISource},
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		current, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			add("plugin", Warn, "not installed (missing "+f.name+"): run unlimitedcode install")
			return
		}
		if err != nil {
			add("plugin", Fail, err.Error())
			return
		}
		if !bytes.Equal(current, f.want) {
			add("plugin", Warn, path+" differs from embedded bundle (rerun unlimitedcode install)")
			return
		}
	}
	add("plugin", OK, dir)
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
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
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
