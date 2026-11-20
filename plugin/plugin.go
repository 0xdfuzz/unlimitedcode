// Package plugin ships the unified OpenCode V2 plugin bundle (server retry bridge,
// native TUI commands/badge, and Desktop GUI composer pill).
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

//go:embed tui.tsx
var TUISource []byte

//go:embed gui-experimental.js
var GUISource []byte

const (
	PluginID    = "unlimitedcode"
	Filename    = "server.ts"
	TUIFilename = "tui.tsx"
	GUIFilename = "gui-experimental.js"

	legacyFilename = "unlimitedcode.ts"
)

// Dir is the unified OpenCode V2 plugin directory (~/.config/opencode/plugins/unlimitedcode).
// OpenCode follows XDG_CONFIG_HOME when set and falls back to ~/.config.
func Dir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "plugins", PluginID), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode", "plugins", PluginID), nil
}

func legacyServerPath(bundleDir string) string {
	// bundleDir is <base>/plugins/unlimitedcode -> <base>/plugin/unlimitedcode.ts
	return filepath.Join(filepath.Dir(filepath.Dir(bundleDir)), "plugin", legacyFilename)
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

	// Remove legacy single-file plugin and old non-JSX tui.ts so OpenCode never double-loads.
	_ = os.Remove(legacyServerPath(dir))
	_ = os.Remove(filepath.Join(dir, "tui.ts"))

	serverPath := filepath.Join(dir, Filename)
	if err := writeIfChanged(serverPath, Source); err != nil {
		return "", fmt.Errorf("write server plugin: %w", err)
	}
	if err := writeIfChanged(filepath.Join(dir, TUIFilename), TUISource); err != nil {
		return "", fmt.Errorf("write tui plugin: %w", err)
	}
	if err := writeIfChanged(filepath.Join(dir, GUIFilename), GUISource); err != nil {
		return "", fmt.Errorf("write gui plugin: %w", err)
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
	_ = os.Remove(legacyServerPath(dir))
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
