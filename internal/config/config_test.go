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
	if cfg.Egress.Mode != ModeMasque {
		t.Fatalf("default mode = %q, want %q", cfg.Egress.Mode, ModeMasque)
	}
	if !cfg.Egress.PassthroughEnabled() {
		t.Fatal("passthrough should default to true")
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
	cfg.Egress.Mode = ModeHTTPConnect
	cfg.Egress.Upstream = "ftp://127.0.0.1:40000"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unsupported scheme must fail")
	}
}

func TestIgnoresUnknownLegacyField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:42987","resume":{"enabled":false,"timeout_seconds":300}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("legacy keys should be ignored cleanly: %v", err)
	}
	if cfg.Listen != "127.0.0.1:42987" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("UNLIMITEDCODE_ALLOW", "example.com,*.example.net")
	t.Setenv("UNLIMITEDCODE_ALLOW_PORTS", "443,8443")
	t.Setenv("UNLIMITEDCODE_EGRESS_MODE", "direct")

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

func TestRotateConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{
		"listen": "127.0.0.1:42987",
		"control": "127.0.0.1:42988",
		"allow": ["opencode.ai"],
		"allow_ports": [443],
		"rotate": {
			"enabled": false,
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
	if cfg.Rotate.Enabled {
		t.Fatal("rotate.enabled = true, want false")
	}
	if cfg.Rotate.TimeoutSeconds != 30 || cfg.Rotate.FailureCooldownSecs != 2 {
		t.Fatalf("rotate = %+v", cfg.Rotate)
	}
}

func TestRotateEnvOverride(t *testing.T) {
	t.Setenv("UNLIMITEDCODE_ROTATE_ENABLED", "false")
	t.Setenv("UNLIMITEDCODE_ROTATE_TIMEOUT_SECONDS", "20")
	t.Setenv("UNLIMITEDCODE_ROTATE_FAILURE_COOLDOWN_SECONDS", "1")
	t.Setenv("UNLIMITEDCODE_ROTATE_MAX_PER_HOUR", "5")
	t.Setenv("UNLIMITEDCODE_ROTATE_MAX_PER_DAY", "50")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rotate.Enabled || cfg.Rotate.TimeoutSeconds != 20 || cfg.Rotate.FailureCooldownSecs != 1 || cfg.Rotate.MaxPerHour != 5 || cfg.Rotate.MaxPerDay != 50 {
		t.Fatalf("rotate = %+v", cfg.Rotate)
	}
}

func TestRotateValidation(t *testing.T) {
	cfg := Default()
	cfg.Rotate.TimeoutSeconds = 4
	if err := cfg.Validate(); err == nil {
		t.Fatal("timeout below five seconds must fail")
	}
	cfg = Default()
	cfg.Rotate.FailureCooldownSecs = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative cooldown must fail")
	}
	cfg = Default()
	cfg.Rotate.MaxPerHour = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative hourly limit must fail")
	}
	cfg = Default()
	cfg.Rotate.MaxPerDay = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative daily limit must fail")
	}
}
