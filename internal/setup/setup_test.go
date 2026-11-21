package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"unlimitedcode/internal/config"
)

func TestEnsureConfigCreates(t *testing.T) {
	t.Setenv("UNLIMITEDCODE_HOME", t.TempDir())
	path := config.ConfigPath()
	got, created, err := EnsureConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path || !created {
		t.Fatalf("path=%q created=%v", got, created)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:42987" || !cfg.Rotate.Enabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestEnsureConfigDoesNotOverwrite(t *testing.T) {
	t.Setenv("UNLIMITEDCODE_HOME", t.TempDir())
	path := config.ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"listen":"127.0.0.1:41000"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, created, err := EnsureConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("reported created")
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("config changed: %s", current)
	}
}

func TestSetAndClearOpenCodeServiceEnv(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	ocDir := filepath.Join(xdg, "opencode")
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mikoshi := filepath.Join(ocDir, "service-mikoshi.json")
	if err := os.WriteFile(mikoshi, []byte(`{"password":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	updated, err := SetOpenCodeServiceEnv("http://127.0.0.1:42987")
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 2 {
		t.Fatalf("updated = %v, want 2 files", updated)
	}
	raw, err := os.ReadFile(mikoshi)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"HTTPS_PROXY": "http://127.0.0.1:42987"`) || !strings.Contains(string(raw), `"password": "secret"`) {
		t.Fatalf("service-mikoshi.json = %s", raw)
	}

	if _, err := ClearOpenCodeServiceEnv("http://127.0.0.1:42987"); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(mikoshi)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "HTTPS_PROXY") || !strings.Contains(string(raw), `"password": "secret"`) {
		t.Fatalf("after clear service-mikoshi.json = %s", raw)
	}

	// Foreign user proxy must never be touched on uninstall.
	if err := os.WriteFile(mikoshi, []byte(`{"password":"secret","env":{"HTTPS_PROXY":"http://corp:8080","CUSTOM":"keep"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearOpenCodeServiceEnv("http://127.0.0.1:42987"); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(mikoshi)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "http://corp:8080") || !strings.Contains(string(raw), `"CUSTOM":"keep"`) {
		t.Fatalf("foreign proxy was altered: %s", raw)
	}
}
