package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"unlimitedcode/internal/config"
)

// EnsureConfig writes the default config when none exists. It never overwrites
// an existing file, so a hand-edited config survives a repeated setup.
func EnsureConfig(path string) (string, bool, error) {
	if path == "" {
		path = config.ConfigPath()
	}
	if _, err := os.Stat(path); err == nil {
		return path, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	raw, err := json.MarshalIndent(config.Default(), "", "  ")
	if err != nil {
		return "", false, err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}
