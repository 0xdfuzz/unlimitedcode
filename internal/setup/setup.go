package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"unlimitedcode/internal/config"
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// EnsureConfig writes the default config when none exists. It never overwrites
// an existing file, so a hand-edited config survives a repeated setup.
func EnsureConfig(path string) (string, bool, error) {
	if path == "" {
		path = config.ConfigPath()
	}
	EnsureInstalledAt(filepath.Dir(path))
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
	if err := writeAtomic(path, raw, 0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	_ = os.Chmod(tmpName, perm)
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return os.WriteFile(path, data, perm)
	}
	return nil
}

// EnsureInstalledAt writes a one-time Unix millisecond timestamp marking when
// UnlimitedCode was first installed so usage stats only count free-tier tokens
// used after installation. Preserves the existing timestamp across upgrades
// and repairs, and seeds from config.json mtime when upgrading from older releases.
func EnsureInstalledAt(dir string) {
	if dir == "" {
		dir = config.Home()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	marker := filepath.Join(dir, "installed_at")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	ts := time.Now().UnixMilli()
	if info, err := os.Stat(filepath.Join(dir, "config.json")); err == nil && !info.ModTime().IsZero() {
		ts = info.ModTime().UnixMilli()
	}
	_ = writeAtomic(marker, []byte(strconv.FormatInt(ts, 10)+"\n"), 0o600)
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
			raw = bytes.TrimPrefix(raw, utf8BOM)
			if err := json.Unmarshal(raw, &doc); err != nil {
				return updated, err
			}
		}
		envMap, ok := doc["env"].(map[string]any)
		if !ok || envMap == nil {
			envMap = map[string]any{}
		}
		envMap["HTTPS_PROXY"] = proxyURL
		envMap["HTTP_PROXY"] = proxyURL
		envMap["https_proxy"] = proxyURL
		envMap["http_proxy"] = proxyURL
		envMap["NO_PROXY"] = "localhost,127.0.0.1,::1"
		envMap["no_proxy"] = "localhost,127.0.0.1,::1"
		doc["env"] = envMap

		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return updated, err
		}
		out = append(out, '\n')
		if err := writeAtomic(file, out, 0o600); err != nil {
			return updated, err
		}
		updated = append(updated, file)
	}
	return updated, nil
}

// ClearOpenCodeServiceEnv removes HTTPS_PROXY, HTTP_PROXY, and NO_PROXY from
// OpenCode's service.json and any service-*.json files ONLY when they point to
// UnlimitedCode's proxyURL, leaving any user-configured proxy or other keys intact.
func ClearOpenCodeServiceEnv(proxyURL string) ([]string, error) {
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
		raw = bytes.TrimPrefix(raw, utf8BOM)
		doc := map[string]any{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		envMap, ok := doc["env"].(map[string]any)
		if !ok || envMap == nil {
			continue
		}
		removedProxy := false
		for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
			if val, ok := envMap[key].(string); ok && (proxyURL == "" || val == proxyURL) {
				delete(envMap, key)
				removedProxy = true
			}
		}
		if removedProxy {
			for _, key := range []string{"NO_PROXY", "no_proxy"} {
				if val, ok := envMap[key].(string); ok && val == "localhost,127.0.0.1,::1" {
					delete(envMap, key)
				}
			}
		}
		if !removedProxy {
			continue
		}
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
		if err := writeAtomic(file, out, 0o600); err == nil {
			updated = append(updated, file)
		}
	}
	return updated, nil
}
