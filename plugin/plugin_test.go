package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallAndUninstall(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "plugins", PluginID)

	// Seed a legacy single-file plugin to verify Install cleans it up.
	legacyDir := filepath.Join(base, "plugin")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyFile := filepath.Join(legacyDir, legacyFilename)
	if err := os.WriteFile(legacyFile, []byte("// legacy"), 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, Filename) {
		t.Fatalf("path = %q", path)
	}
	if _, err := os.Stat(legacyFile); !os.IsNotExist(err) {
		t.Fatalf("legacy plugin was not removed: %v", err)
	}

	for name, want := range map[string][]byte{
		Filename:    Source,
		TUIFilename: TUISource,
		GUIFilename: GUISource,
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("installed %s differs from embedded source", name)
		}
	}

	again, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != path {
		t.Fatalf("second install path = %q", again)
	}

	if err := Uninstall(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("plugin dir still present: %v", err)
	}
	if err := Uninstall(dir); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

func TestSourceHasContract(t *testing.T) {
	for _, want := range []string{`hook("retry"`, "provider.free-limit", "control.token", `"/event"`} {
		if !bytes.Contains(Source, []byte(want)) {
			t.Fatalf("server plugin source missing %q", want)
		}
	}
	for _, want := range []string{`unlimited-stats`, `dialog.show`, `dialog.select`, `toast.show`, `Free Stats`, `prompt.footer`} {
		if !bytes.Contains(TUISource, []byte(want)) {
			t.Fatalf("tui plugin source missing %q", want)
		}
	}
	for _, want := range []string{`session.header`, `unlimitedcode.daemon`, `Rotate Egress IP`} {
		if !bytes.Contains(GUISource, []byte(want)) {
			t.Fatalf("gui plugin source missing %q", want)
		}
	}
}

func TestDirHonorsXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "opencode", "plugins", PluginID)
	if dir != want {
		t.Fatalf("Dir() = %q, want %q", dir, want)
	}
}
