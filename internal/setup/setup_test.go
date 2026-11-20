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
	input := strings.Join([]string{
		`C:\Users\demo\AppData\Local\openhop\bin`,
		`C:\Users\demo\AppData\Roaming\npm`,
		`C:\tools\OpenHop\bin`,
	}, separator)
	got, removed := StripOpenhop(input)
	if got != `C:\Users\demo\AppData\Roaming\npm` {
		t.Fatalf("kept %q", got)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
}

func TestStripOpenhopKeepsSibling(t *testing.T) {
	separator := string(os.PathListSeparator)
	value := strings.Join([]string{
		`C:\tools\openhopkit\bin`,
		`C:\tools\openhop\bin`,
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
