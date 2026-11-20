package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	ModeMasque      = "masque"
	ModeUsque       = "usque" // alias for ModeMasque
	ModeHTTPConnect = "http-connect"
	ModeDirect      = "direct"
)

type Config struct {
	Listen     string   `json:"listen"`
	Control    string   `json:"control"`
	Allow      []string `json:"allow"`
	AllowPorts []int    `json:"allow_ports"`
	Egress     Egress   `json:"egress"`
	Rotate     Rotate   `json:"rotate"`
	Observer   Observer `json:"observer"`
	Resume     Resume   `json:"resume"`
	Limits     Limits   `json:"limits"`

	Path string `json:"-"`
}

type Egress struct {
	Mode           string `json:"mode"`
	PoolSize       int    `json:"pool_size,omitempty"`
	Upstream       string `json:"upstream,omitempty"`
	DirectFallback bool   `json:"direct_fallback"`
	Passthrough    *bool  `json:"passthrough,omitempty"`
}

func (e Egress) PassthroughEnabled() bool {
	if e.Passthrough == nil {
		return true
	}
	return *e.Passthrough
}

type Rotate struct {
	Enabled             bool `json:"enabled"`
	TimeoutSeconds      int  `json:"timeout_seconds"`
	FailureCooldownSecs int  `json:"failure_cooldown_seconds"`
	MaxPerHour          int  `json:"max_per_hour"`
	MaxPerDay           int  `json:"max_per_day"`
}

type Observer struct {
	Enabled            bool   `json:"enabled"`
	LogPath            string `json:"log_path"`
	PollSeconds        int    `json:"poll_seconds"`
	DedupSeconds       int    `json:"dedup_seconds"`
	PluginGraceSeconds int    `json:"plugin_grace_seconds"`
}

type Resume struct {
	Enabled        bool   `json:"enabled"`
	Binary         string `json:"binary"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type Limits struct {
	MaxTunnels         int `json:"max_tunnels"`
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	DialTimeoutSeconds int `json:"dial_timeout_seconds"`
}

func Default() Config {
	return Config{
		Listen:     "127.0.0.1:42987",
		Control:    "127.0.0.1:42988",
		Allow:      []string{"opencode.ai", "*.opencode.ai"},
		AllowPorts: []int{443, 80},
		Egress: Egress{
			Mode:     ModeMasque,
			PoolSize: 2,
		},
		Rotate: Rotate{
			Enabled:             true,
			TimeoutSeconds:      45,
			FailureCooldownSecs: 5,
			MaxPerHour:          30,
			MaxPerDay:           300,
		},
		Observer: Observer{
			Enabled:            true,
			PollSeconds:        2,
			DedupSeconds:       60,
			PluginGraceSeconds: 15,
		},
		Resume: Resume{
			Enabled:        false,
			TimeoutSeconds: 300,
		},
		Limits: Limits{
			MaxTunnels:         128,
			IdleTimeoutSeconds: 300,
			DialTimeoutSeconds: 10,
		},
	}
}

func Home() string {
	if v := os.Getenv("UNLIMITEDCODE_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "UnlimitedCode")
		}
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "UnlimitedCode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".UnlimitedCode"
	}
	return filepath.Join(home, ".local", "state", "UnlimitedCode")
}

func ConfigPath() string { return filepath.Join(Home(), "config.json") }
func LogPath() string    { return filepath.Join(Home(), "logs", "unlimitedcode.log") }
func TokenPath() string  { return filepath.Join(Home(), "control.token") }
func MasqueDir() string  { return filepath.Join(Home(), "masque") }

// ObserverStatePath stores the log tail offset so a restart does not replay
// old quota errors.
func ObserverStatePath() string { return filepath.Join(Home(), "observer.json") }

func Load(path string) (Config, error) {
	if path == "" {
		path = ConfigPath()
	}
	cfg := Default()
	cfg.Path = path

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("UNLIMITEDCODE_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("UNLIMITEDCODE_CONTROL"); v != "" {
		c.Control = v
	}
	if v := os.Getenv("UNLIMITEDCODE_ALLOW"); v != "" {
		c.Allow = splitList(v)
	}
	if v := os.Getenv("UNLIMITEDCODE_ALLOW_PORTS"); v != "" {
		if ports, err := parsePorts(v); err == nil {
			c.AllowPorts = ports
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_EGRESS_MODE"); v != "" {
		c.Egress.Mode = v
	}
	if v := os.Getenv("UNLIMITEDCODE_UPSTREAM"); v != "" {
		c.Egress.Upstream = v
	}
	if v := os.Getenv("UNLIMITEDCODE_DIRECT_FALLBACK"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Egress.DirectFallback = b
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Egress.PoolSize = n
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_MAX_TUNNELS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Limits.MaxTunnels = n
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Limits.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_DIAL_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Limits.DialTimeoutSeconds = n
		}
	}
	if v := envFirst("UNLIMITEDCODE_ROTATE_ENABLED", "UNLIMITEDCODE_HOP_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Rotate.Enabled = b
		}
	}
	if v := envFirst("UNLIMITEDCODE_ROTATE_TIMEOUT_SECONDS", "UNLIMITEDCODE_HOP_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Rotate.TimeoutSeconds = n
		}
	}
	if v := envFirst("UNLIMITEDCODE_ROTATE_FAILURE_COOLDOWN_SECONDS", "UNLIMITEDCODE_HOP_FAILURE_COOLDOWN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Rotate.FailureCooldownSecs = n
		}
	}
	if v := envFirst("UNLIMITEDCODE_ROTATE_MAX_PER_HOUR", "UNLIMITEDCODE_HOP_MAX_PER_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Rotate.MaxPerHour = n
		}
	}
	if v := envFirst("UNLIMITEDCODE_ROTATE_MAX_PER_DAY", "UNLIMITEDCODE_HOP_MAX_PER_DAY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Rotate.MaxPerDay = n
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_OBSERVER_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Observer.Enabled = b
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_OBSERVER_LOG_PATH"); v != "" {
		c.Observer.LogPath = v
	}
	if v := os.Getenv("UNLIMITEDCODE_OBSERVER_POLL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Observer.PollSeconds = n
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_OBSERVER_DEDUP_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Observer.DedupSeconds = n
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_OBSERVER_PLUGIN_GRACE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Observer.PluginGraceSeconds = n
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_RESUME_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Resume.Enabled = b
		}
	}
	if v := os.Getenv("UNLIMITEDCODE_RESUME_BINARY"); v != "" {
		c.Resume.Binary = v
	}
	if v := os.Getenv("UNLIMITEDCODE_RESUME_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Resume.TimeoutSeconds = n
		}
	}
}

func (c *Config) Validate() error {
	if err := validateLoopback(c.Listen, "listen"); err != nil {
		return err
	}
	if err := validateLoopback(c.Control, "control"); err != nil {
		return err
	}
	if c.Listen == c.Control {
		return errors.New("listen and control must differ")
	}

	switch c.Egress.Mode {
	case ModeMasque, ModeUsque:
		c.Egress.Mode = ModeMasque
		if c.Egress.PoolSize <= 0 {
			c.Egress.PoolSize = 2
		}
		if c.Egress.PoolSize > 8 {
			return errors.New("egress.pool_size must be between 1 and 8")
		}
	case ModeHTTPConnect:
		if strings.TrimSpace(c.Egress.Upstream) == "" {
			return errors.New("egress.upstream is required in http-connect mode")
		}
		if err := validateProxyURL(c.Egress.Upstream); err != nil {
			return fmt.Errorf("egress.upstream: %w", err)
		}
	case ModeDirect:
	default:
		return fmt.Errorf("egress.mode must be %q, %q or %q", ModeMasque, ModeHTTPConnect, ModeDirect)
	}

	if len(c.Allow) == 0 {
		return errors.New("allow must contain at least one host")
	}
	normalized := make([]string, 0, len(c.Allow))
	for _, entry := range c.Allow {
		host, err := NormalizeHost(entry)
		if err != nil {
			return fmt.Errorf("allow %q: %w", entry, err)
		}
		normalized = append(normalized, host)
	}
	c.Allow = normalized

	if len(c.AllowPorts) == 0 {
		return errors.New("allow_ports must contain at least one port")
	}
	for _, port := range c.AllowPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("allow_ports: invalid port %d", port)
		}
	}

	if c.Limits.MaxTunnels < 1 {
		return errors.New("limits.max_tunnels must be positive")
	}
	if c.Limits.IdleTimeoutSeconds < 1 {
		return errors.New("limits.idle_timeout_seconds must be positive")
	}
	if c.Limits.DialTimeoutSeconds < 1 {
		return errors.New("limits.dial_timeout_seconds must be positive")
	}

	if c.Rotate.TimeoutSeconds < 5 {
		return errors.New("rotate.timeout_seconds must be at least 5")
	}
	if c.Rotate.FailureCooldownSecs < 0 {
		return errors.New("rotate.failure_cooldown_seconds must not be negative")
	}
	if c.Rotate.MaxPerHour < 0 {
		return errors.New("rotate.max_per_hour must not be negative")
	}
	if c.Rotate.MaxPerDay < 0 {
		return errors.New("rotate.max_per_day must not be negative")
	}

	if c.Observer.PollSeconds < 1 {
		return errors.New("observer.poll_seconds must be positive")
	}
	if c.Observer.DedupSeconds < 0 {
		return errors.New("observer.dedup_seconds must not be negative")
	}
	if c.Observer.PluginGraceSeconds < 0 {
		return errors.New("observer.plugin_grace_seconds must not be negative")
	}
	if c.Resume.Enabled && !c.Observer.Enabled {
		return errors.New("resume.enabled requires observer.enabled")
	}
	if c.Resume.TimeoutSeconds < 10 {
		return errors.New("resume.timeout_seconds must be at least 10")
	}
	return nil
}

func (c Config) UpstreamName() string {
	switch c.Egress.Mode {
	case ModeMasque, ModeUsque:
		size := c.Egress.PoolSize
		if size <= 0 {
			size = 2
		}
		return fmt.Sprintf("masque-pool(%d) in-process", size)
	case ModeHTTPConnect:
		return c.Egress.Upstream
	default:
		return "direct"
	}
}

func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// NormalizeHost lowercases a host or wildcard pattern, strips a trailing dot,
// and rejects anything that is not a plain DNS name, wildcard or IP literal.
func NormalizeHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	host = strings.TrimSuffix(host, ".")
	wildcard := strings.HasPrefix(host, "*.")
	if wildcard {
		host = strings.TrimPrefix(host, "*.")
	}
	if host == "" {
		return "", errors.New("empty host")
	}
	if strings.ContainsAny(host, "/\\@ :?#[]") {
		return "", errors.New("host must not contain scheme, port or path")
	}
	if wildcard && net.ParseIP(host) != nil {
		return "", errors.New("wildcard cannot be an IP literal")
	}
	if wildcard {
		return "*." + host, nil
	}
	return host, nil
}

// HostAllowed matches a host against exact entries and "*." suffix entries.
func HostAllowed(host string, allow []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pattern := range allow {
		pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:]
			base := pattern[2:]
			if host != base && strings.HasSuffix(host, suffix) {
				return true
			}
			continue
		}
		if host == pattern {
			return true
		}
	}
	return false
}

func validateProxyURL(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return errors.New("empty URL")
	}
	u, err := url.Parse(value)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("path is not supported, got %q", u.Path)
	}
	return nil
}

func validateLoopback(addr, name string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("%s: invalid port %q", name, port)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s must be a loopback address, got %q", name, host)
	}
	return nil
}

func parseProxy(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("empty URL")
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("path is not supported, got %q", u.Path)
	}
	return value, nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parsePorts(v string) ([]int, error) {
	parts := splitList(v)
	ports := make([]int, 0, len(parts))
	for _, part := range parts {
		port, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, nil
}
