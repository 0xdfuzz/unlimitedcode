//go:build !windows

package setup

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"kestrel/internal/config"
)

func InstalledPath() string {
	return filepath.Join(config.Home(), "kestrel")
}

func CopyBinary(self, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".kestrel-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func InstallAutostart(string) (string, error) {
	return "", errors.New("autostart is only implemented on Windows")
}

func RemoveAutostart() (string, error) {
	return "", nil
}

func StartDetached(string) error { return errors.New("detached start is only implemented on Windows") }
