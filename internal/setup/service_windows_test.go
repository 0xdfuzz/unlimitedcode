//go:build windows

package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyBinary(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.exe")
	dest := filepath.Join(dir, "dest", "unlimitedcode.exe")
	want := []byte("binary-content")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyBinary(src, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("dest content %q", got)
	}
	if err := copyBinary(src, dest); err != nil {
		t.Fatalf("second copy: %v", err)
	}
}

func TestInstallAutostart(t *testing.T) {
	root := t.TempDir()
	t.Setenv("UNLIMITEDCODE_HOME", filepath.Join(root, "home"))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	vbs, err := InstallAutostart(self)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(vbs)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != launcherVBS(InstalledPath()) {
		t.Fatalf("launcher %q", raw)
	}
	if _, err := os.Stat(InstalledPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vbs); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveAutostart(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vbs); !os.IsNotExist(err) {
		t.Fatalf("launcher still there: %v", err)
	}
}
