package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsValidate(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

func TestNormalizeHost(t *testing.T) {
	valid := map[string]string{
		"Example.COM.":  "example.com",
		"*.Example.COM": "*.example.com",
		"127.0.0.1":     "127.0.0.1",
	}
	for raw, want := range valid {
		got, err := NormalizeHost(raw)
		if err != nil {
			t.Fatalf("NormalizeHost(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("NormalizeHost(%q) = %q, want %q", raw, got, want)
		}
	}
	invalid := []string{"", "http://example.com", "example.com:443", "a/b", "*.127.0.0.1"}
	for _, raw := range invalid {
		if _, err := NormalizeHost(raw); err == nil {
			t.Errorf("NormalizeHost(%q) should fail", raw)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allow := []string{"opencode.ai", "*.example.com"}
	cases := []struct {
		host string
		want bool
	}{
		{"opencode.ai", true},
		{"OPENCODE.AI.", true},
		{"api.opencode.ai", false},
		{"example.com", false},
		{"api.example.com", true},
		{"a.b.example.com", true},
		{"notexample.com", false},
		{"example.com.evil", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := HostAllowed(tc.host, allow); got != tc.want {
			t.Errorf("HostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestRejectsNonLoopback(t *testing.T) {
	cfg := Default()
	cfg.Listen = "0.0.0.0:42987"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-loopback listen must fail")
	}
}

func TestRejectsBadEgress(t *testing.T) {
	cfg := Default()
	cfg.Egress.Mode = "socks5"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown mode must fail")
	}
	cfg = Default()
	cfg.Egress.Mode = ModeWarp
	cfg.Egress.WarpProxy = "ftp://127.0.0.1:40000"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported scheme must fail")
	}
}

func TestRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:42987","policy":{"mode":"full"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config key must fail")
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("KESTREL_ALLOW", "example.com,*.example.net")
	t.Setenv("KESTREL_ALLOW_PORTS", "443,8443")
	t.Setenv("KESTREL_EGRESS_MODE", "direct")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Allow) != 2 || cfg.Allow[0] != "example.com" || cfg.Allow[1] != "*.example.net" {
		t.Fatalf("allow = %v", cfg.Allow)
	}
	if len(cfg.AllowPorts) != 2 || cfg.AllowPorts[1] != 8443 {
		t.Fatalf("allow ports = %v", cfg.AllowPorts)
	}
	if cfg.Egress.Mode != ModeDirect {
		t.Fatalf("mode = %q", cfg.Egress.Mode)
	}
}

func TestHopConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{
		"listen": "127.0.0.1:42987",
		"control": "127.0.0.1:42988",
		"allow": ["opencode.ai"],
		"allow_ports": [443],
		"hop": {
			"enabled": false,
			"warp_cli": "C:\\Tools\\warp-cli.exe",
			"timeout_seconds": 30,
			"failure_cooldown_seconds": 2
		}
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hop.Enabled {
		t.Fatal("hop.enabled = true, want false")
	}
	if cfg.Hop.WarpCLI != `C:\Tools\warp-cli.exe` {
		t.Fatalf("warp_cli = %q", cfg.Hop.WarpCLI)
	}
	if cfg.Hop.TimeoutSeconds != 30 || cfg.Hop.FailureCooldownSecs != 2 {
		t.Fatalf("hop = %+v", cfg.Hop)
	}
}

func TestHopEnvOverride(t *testing.T) {
	t.Setenv("KESTREL_HOP_ENABLED", "false")
	t.Setenv("KESTREL_WARP_CLI", "warp-test")
	t.Setenv("KESTREL_HOP_TIMEOUT_SECONDS", "20")
	t.Setenv("KESTREL_HOP_FAILURE_COOLDOWN_SECONDS", "1")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hop.Enabled || cfg.Hop.WarpCLI != "warp-test" || cfg.Hop.TimeoutSeconds != 20 || cfg.Hop.FailureCooldownSecs != 1 {
		t.Fatalf("hop = %+v", cfg.Hop)
	}
}

func TestHopValidation(t *testing.T) {
	cfg := Default()
	cfg.Hop.TimeoutSeconds = 4
	if err := cfg.Validate(); err == nil {
		t.Fatal("timeout below five seconds must fail")
	}
	cfg = Default()
	cfg.Hop.FailureCooldownSecs = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative cooldown must fail")
	}
}

func TestConfigRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:42987","typo":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown field must fail")
	}
}

func TestPhase2bDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Observer.Enabled || cfg.Observer.PollSeconds != 2 || cfg.Observer.DedupSeconds != 60 || cfg.Observer.PluginGraceSeconds != 15 {
		t.Fatalf("observer defaults = %+v", cfg.Observer)
	}
	if cfg.Resume.Enabled || cfg.Resume.TimeoutSeconds != 300 {
		t.Fatalf("resume defaults = %+v", cfg.Resume)
	}
	if cfg.Hop.MaxPerHour != 30 || cfg.Hop.MaxPerDay != 300 {
		t.Fatalf("hop limits = %+v", cfg.Hop)
	}
}

func TestPhase2bEnvOverride(t *testing.T) {
	t.Setenv("KESTREL_HOP_MAX_PER_HOUR", "5")
	t.Setenv("KESTREL_HOP_MAX_PER_DAY", "50")
	t.Setenv("KESTREL_OBSERVER_ENABLED", "true")
	t.Setenv("KESTREL_OBSERVER_POLL_SECONDS", "3")
	t.Setenv("KESTREL_OBSERVER_DEDUP_SECONDS", "30")
	t.Setenv("KESTREL_OBSERVER_PLUGIN_GRACE_SECONDS", "0")
	t.Setenv("KESTREL_RESUME_ENABLED", "true")
	t.Setenv("KESTREL_RESUME_BINARY", "opencode-test")
	t.Setenv("KESTREL_RESUME_TIMEOUT_SECONDS", "120")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hop.MaxPerHour != 5 || cfg.Hop.MaxPerDay != 50 {
		t.Fatalf("hop = %+v", cfg.Hop)
	}
	if !cfg.Observer.Enabled || cfg.Observer.PollSeconds != 3 || cfg.Observer.DedupSeconds != 30 || cfg.Observer.PluginGraceSeconds != 0 {
		t.Fatalf("observer = %+v", cfg.Observer)
	}
	if !cfg.Resume.Enabled || cfg.Resume.Binary != "opencode-test" || cfg.Resume.TimeoutSeconds != 120 {
		t.Fatalf("resume = %+v", cfg.Resume)
	}
}

func TestPhase2bValidation(t *testing.T) {
	cfg := Default()
	cfg.Hop.MaxPerHour = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative hourly limit must fail")
	}

	cfg = Default()
	cfg.Hop.MaxPerDay = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative daily limit must fail")
	}

	cfg = Default()
	cfg.Observer.PollSeconds = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("zero poll must fail")
	}

	cfg = Default()
	cfg.Resume.Enabled = true
	cfg.Observer.Enabled = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("resume without observer must fail")
	}

	cfg = Default()
	cfg.Resume.Enabled = true
	cfg.Resume.TimeoutSeconds = 5
	if err := cfg.Validate(); err == nil {
		t.Fatal("resume timeout below ten seconds must fail")
	}
}

func TestObserverAndResumeEnv(t *testing.T) {
	t.Setenv("KESTREL_OBSERVER_ENABLED", "false")
	t.Setenv("KESTREL_OBSERVER_LOG_PATH", `C:\logs\opencode.log`)
	t.Setenv("KESTREL_OBSERVER_POLL_SECONDS", "5")
	t.Setenv("KESTREL_OBSERVER_DEDUP_SECONDS", "90")
	t.Setenv("KESTREL_OBSERVER_PLUGIN_GRACE_SECONDS", "7")
	t.Setenv("KESTREL_RESUME_ENABLED", "true")
	t.Setenv("KESTREL_RESUME_BINARY", `C:\opencode.exe`)
	t.Setenv("KESTREL_RESUME_TIMEOUT_SECONDS", "600")
	t.Setenv("KESTREL_HOP_MAX_PER_HOUR", "5")
	t.Setenv("KESTREL_HOP_MAX_PER_DAY", "50")

	// Resume requires the observer, so leave the observer enabled here.
	t.Setenv("KESTREL_OBSERVER_ENABLED", "true")
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Observer.LogPath != `C:\logs\opencode.log` ||
		cfg.Observer.PollSeconds != 5 ||
		cfg.Observer.DedupSeconds != 90 ||
		cfg.Observer.PluginGraceSeconds != 7 {
		t.Fatalf("observer = %+v", cfg.Observer)
	}
	if !cfg.Resume.Enabled || cfg.Resume.Binary != `C:\opencode.exe` || cfg.Resume.TimeoutSeconds != 600 {
		t.Fatalf("resume = %+v", cfg.Resume)
	}
	if cfg.Hop.MaxPerHour != 5 || cfg.Hop.MaxPerDay != 50 {
		t.Fatalf("hop = %+v", cfg.Hop)
	}
}

func TestResumeRequiresObserver(t *testing.T) {
	cfg := Default()
	cfg.Observer.Enabled = false
	cfg.Resume.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("resume without observer must fail")
	}
}

func TestHopLimitsValidation(t *testing.T) {
	cfg := Default()
	cfg.Hop.MaxPerHour = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative hourly limit must fail")
	}
	cfg = Default()
	cfg.Hop.MaxPerDay = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative daily limit must fail")
	}
}

func TestObserverValidation(t *testing.T) {
	cfg := Default()
	cfg.Observer.PollSeconds = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("zero poll must fail")
	}
	cfg = Default()
	cfg.Observer.DedupSeconds = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative dedup must fail")
	}
	cfg = Default()
	cfg.Observer.PluginGraceSeconds = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative grace must fail")
	}
}
