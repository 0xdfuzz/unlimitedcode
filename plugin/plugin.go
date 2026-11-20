// Package plugin ships the OpenCode retry bridge. The daemon works without it;
// the plugin only removes the visible 429 by asking the daemon for a hop.
package plugin

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed kestrel.ts
var Source []byte

const Filename = "kestrel.ts"

// Dir is the global OpenCode plugin directory. OpenCode follows
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
	path := filepath.Join(dir, Filename)
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, Source) {
		return path, nil
	}
	if err := os.WriteFile(path, Source, 0o600); err != nil {
		return "", fmt.Errorf("write plugin: %w", err)
	}
	return path, nil
}

func Uninstall(dir string) error {
	if dir == "" {
		var err error
		dir, err = Dir()
		if err != nil {
			return err
		}
	}
	err := os.Remove(filepath.Join(dir, Filename))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
