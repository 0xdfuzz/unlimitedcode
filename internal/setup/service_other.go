//go:build !windows

package setup

import "errors"

func InstalledPath() string { return "" }

func InstallAutostart(string) (string, error) {
	return "", errors.New("autostart is only implemented on Windows")
}

func RemoveAutostart() (string, error) {
	return "", errors.New("autostart is only implemented on Windows")
}

func StartDetached(string) error { return errors.New("detached start is only implemented on Windows") }
