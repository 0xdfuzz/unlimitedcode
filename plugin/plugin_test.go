package plugin

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallAndUninstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plugin")

	path, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, Filename) {
		t.Fatalf("path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, Source) {
		t.Fatal("installed server plugin differs from embedded source")
	}

	tuiDir, err := TUIDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	tuiPath := filepath.Join(tuiDir, TUIFilename)
	tuiData, err := os.ReadFile(tuiPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tuiData, TUISource) {
		t.Fatal("installed tui plugin differs from embedded source")
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
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("server plugin still present: %v", err)
	}
	if _, err := os.Stat(tuiDir); !os.IsNotExist(err) {
		t.Fatalf("tui plugin dir still present: %v", err)
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
	for _, want := range []string{`unlimited-stats`, `dialog.show`, `dialog.select`, `toast.show`, `Est. Saved`} {
		if !bytes.Contains(TUISource, []byte(want)) {
			t.Fatalf("tui plugin source missing %q", want)
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
	want := filepath.Join(xdg, "opencode", "plugin")
	if dir != want {
		t.Fatalf("Dir() = %q, want %q", dir, want)
	}
}
