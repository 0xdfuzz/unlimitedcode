package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"unlimitedcode/internal/config"
)

func TestStripOpenhop(t *testing.T) {
	separator := string(os.PathListSeparator)
	npmDir := filepath.Join("demo", "AppData", "Roaming", "npm")
	input := strings.Join([]string{
		filepath.Join("demo", "AppData", "Local", "openhop", "bin"),
		npmDir,
		filepath.Join("tools", "OpenHop", "bin"),
	}, separator)
	got, removed := StripOpenhop(input)
	if got != npmDir {
		t.Fatalf("kept %q, want %q", got, npmDir)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
}

func TestStripOpenhopKeepsSibling(t *testing.T) {
	separator := string(os.PathListSeparator)
	value := strings.Join([]string{
		filepath.Join("tools", "openhopkit", "bin"),
		filepath.Join("tools", "openhop", "bin"),
	}, separator)
	got, removed := StripOpenhop(value)
	if !strings.Contains(got, "openhopkit") {
		t.Fatalf("sibling removed: %q", got)
	}
	if len(removed) != 1 {
		t.Fatalf("removed %v", removed)
	}
}

func TestLauncherVBS(t *testing.T) {
	got := launcherVBS(`C:\Users\demo\UnlimitedCode\unlimitedcode.exe`)
	if !strings.Contains(got, `"""C:\Users\demo\UnlimitedCode\unlimitedcode.exe"" daemon"`) {
		t.Fatalf("bad command: %q", got)
	}
	if !strings.Contains(got, ", 0, False") {
		t.Fatalf("not hidden: %q", got)
	}
}

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
	if cfg.Listen != "127.0.0.1:42987" || !cfg.Rotate.Enabled || !cfg.Observer.Enabled {
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

	if _, err := ClearOpenCodeServiceEnv(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(mikoshi)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "HTTPS_PROXY") || !strings.Contains(string(raw), `"password": "secret"`) {
		t.Fatalf("after clear service-mikoshi.json = %s", raw)
	}
}
