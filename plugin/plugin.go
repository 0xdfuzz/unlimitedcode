// Package plugin ships the OpenCode server retry bridge and native TUI commands.
package plugin

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed unlimitedcode.ts
var Source []byte

//go:embed tui.ts
var TUISource []byte

const (
	Filename    = "unlimitedcode.ts"
	TUIFilename = "tui.ts"
	PluginID    = "unlimitedcode"
)

// Dir is the global OpenCode server plugin directory. OpenCode follows
// XDG_CONFIG_HOME when it is set and falls back to ~/.config.
func Dir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "plugin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode", "plugin"), nil
}

// TUIDir returns the OpenCode V2 TUI plugin directory sibling to the server plugin dir
// (~/.config/opencode/plugins/unlimitedcode).
func TUIDir(serverDir string) (string, error) {
	if serverDir == "" {
		var err error
		serverDir, err = Dir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(filepath.Dir(serverDir), "plugins", PluginID), nil
}

func writeIfChanged(path string, content []byte) error {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, content) {
		return nil
	}
	return os.WriteFile(path, content, 0o600)
}

func Install(dir string) (string, error) {
	if dir == "" {
		var err error
		dir, err = Dir()
		if err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create plugin dir: %w", err)
	}
	serverPath := filepath.Join(dir, Filename)
	if err := writeIfChanged(serverPath, Source); err != nil {
		return "", fmt.Errorf("write server plugin: %w", err)
	}

	tuiDir, err := TUIDir(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(tuiDir, 0o700); err != nil {
		return "", fmt.Errorf("create tui plugin dir: %w", err)
	}
	tuiPath := filepath.Join(tuiDir, TUIFilename)
	if err := writeIfChanged(tuiPath, TUISource); err != nil {
		return "", fmt.Errorf("write tui plugin: %w", err)
	}

	return serverPath, nil
}

func Uninstall(dir string) error {
	if dir == "" {
		var err error
		dir, err = Dir()
		if err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dir, Filename)); err != nil && !os.IsNotExist(err) {
		return err
	}
	tuiDir, err := TUIDir(dir)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(tuiDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
