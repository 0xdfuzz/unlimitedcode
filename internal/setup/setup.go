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

func opencodeConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

func serviceConfigFiles() ([]string, error) {
	dir, err := opencodeConfigDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	primary := filepath.Join(dir, "service.json")
	files := []string{primary}
	seen[primary] = true

	if matches, err := filepath.Glob(filepath.Join(dir, "service-*.json")); err == nil {
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	return files, nil
}

// SetOpenCodeServiceEnv writes HTTPS_PROXY, HTTP_PROXY, and NO_PROXY directly
// into OpenCode's service.json (and any channel service-*.json files such as
// service-mikoshi.json) without requiring the opencode CLI on PATH.
func SetOpenCodeServiceEnv(proxyURL string) ([]string, error) {
	files, err := serviceConfigFiles()
	if err != nil {
		return nil, err
	}
	var updated []string
	for _, file := range files {
		doc := map[string]any{}
		if raw, err := os.ReadFile(file); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &doc)
		}
		envMap, ok := doc["env"].(map[string]any)
		if !ok || envMap == nil {
			envMap = map[string]any{}
		}
		envMap["HTTPS_PROXY"] = proxyURL
		envMap["HTTP_PROXY"] = proxyURL
		envMap["NO_PROXY"] = "localhost,127.0.0.1,::1"
		doc["env"] = envMap

		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return updated, err
		}
		out = append(out, '\n')
		if err := os.WriteFile(file, out, 0o600); err != nil {
			return updated, err
		}
		updated = append(updated, file)
	}
	return updated, nil
}

// ClearOpenCodeServiceEnv removes HTTPS_PROXY, HTTP_PROXY, and NO_PROXY from
// OpenCode's service.json and any service-*.json files.
func ClearOpenCodeServiceEnv() ([]string, error) {
	files, err := serviceConfigFiles()
	if err != nil {
		return nil, err
	}
	var updated []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		doc := map[string]any{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		envMap, ok := doc["env"].(map[string]any)
		if !ok || envMap == nil {
			continue
		}
		delete(envMap, "HTTPS_PROXY")
		delete(envMap, "HTTP_PROXY")
		delete(envMap, "NO_PROXY")
		if len(envMap) == 0 {
			delete(doc, "env")
		} else {
			doc["env"] = envMap
		}
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			continue
		}
		out = append(out, '\n')
		if err := os.WriteFile(file, out, 0o600); err == nil {
			updated = append(updated, file)
		}
	}
	return updated, nil
}
