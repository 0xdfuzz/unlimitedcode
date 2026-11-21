//go:build !windows

package setup

import (
	"io"
	"os"
	"path/filepath"

	"unlimitedcode/internal/config"
)

// InstalledPath is where install keeps a stable copy of the binary.
func InstalledPath() string {
	return filepath.Join(config.Home(), "unlimitedcode")
}

// CopyBinary installs a copy of self at dest using an atomic rename.
func CopyBinary(self, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if si, err := os.Stat(self); err == nil {
		if di, err := os.Stat(dest); err == nil && os.SameFile(si, di) {
			return nil
		}
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".unlimitedcode-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// RemoveLegacyAutostart is a no-op on non-Windows platforms.
func RemoveLegacyAutostart() (string, error) {
	return "", nil
}
