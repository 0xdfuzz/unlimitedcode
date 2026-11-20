//go:build windows

package setup

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"kestrel/internal/config"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

// InstalledPath is where setup keeps a stable copy of the binary for autostart.
func InstalledPath() string {
	return filepath.Join(config.Home(), "kestrel.exe")
}

// InstallAutostart copies the binary to the state home and writes a hidden
// logon launcher in the user's Startup folder.
func InstallAutostart(self string) (string, error) {
	dest := InstalledPath()
	if err := copyBinary(self, dest); err != nil {
		return "", err
	}
	startup := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	if startup == "" || os.Getenv("APPDATA") == "" {
		return "", errors.New("APPDATA is not set")
	}
	if err := os.MkdirAll(startup, 0o700); err != nil {
		return "", err
	}
	vbs := filepath.Join(startup, "Kestrel.vbs")
	if err := os.WriteFile(vbs, []byte(launcherVBS(dest)), 0o600); err != nil {
		return "", err
	}
	return vbs, nil
}

// RemoveAutostart deletes the logon launcher. The installed binary stays.
func RemoveAutostart() (string, error) {
	vbs := filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "Kestrel.vbs")
	if err := os.Remove(vbs); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return vbs, nil
}

// StartDetached starts the daemon without a console window.
func StartDetached(exe string) error {
	cmd := exec.Command(exe, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup,
	}
	return cmd.Start()
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

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".kestrel-*.exe")
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
		return fmt.Errorf("replace %s (stop the daemon first): %w", dest, err)
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
