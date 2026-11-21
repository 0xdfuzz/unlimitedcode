//go:build windows

package setup

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"unlimitedcode/internal/config"
)

// InstalledPath is where install keeps a stable copy of the binary.
func InstalledPath() string {
	return filepath.Join(config.Home(), "unlimitedcode.exe")
}

// CopyBinary installs a copy of self at dest using an atomic rename.
func CopyBinary(self, dest string) error {
	return copyBinary(self, dest)
}

// RemoveLegacyAutostart deletes the Startup VBS launcher written by older versions.
func RemoveLegacyAutostart() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", nil
	}
	vbs := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "UnlimitedCode.vbs")
	if err := os.Remove(vbs); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return vbs, nil
}

func copyBinary(self, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	if same, err := sameFile(self, dest); err == nil && same {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".UnlimitedCode-*.exe")
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
	if err := os.Rename(tmpName, dest); err != nil {
		// Windows allows renaming a running executable aside even though it
		// blocks overwriting it in place. Move the live binary to .old, swap
		// the new one in, and best-effort remove .old.
		oldPath := dest + ".old"
		_ = os.Remove(oldPath)
		if rerr := os.Rename(dest, oldPath); rerr != nil {
			return fmt.Errorf("replace %s (stop the daemon first): %w", dest, err)
		}
		if err2 := os.Rename(tmpName, dest); err2 != nil {
			_ = os.Rename(oldPath, dest)
			return fmt.Errorf("replace %s: %w", dest, err2)
		}
		_ = os.Remove(oldPath)
	}
	return nil
}

func sameFile(a, b string) (bool, error) {
	left, err := hashFile(a)
	if err != nil {
		return false, err
	}
	right, err := hashFile(b)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return left == right, nil
}

func hashFile(path string) ([32]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], hash.Sum(nil))
	return out, nil
}
