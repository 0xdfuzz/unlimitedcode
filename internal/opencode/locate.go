package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func Names() []string {
	if runtime.GOOS == "windows" {
		return []string{"opencode.exe", "opencode.cmd", "opencode.bat"}
	}
	return []string{"opencode"}
}

// IsOpenhopShim reports whether path belongs to an openhop/opencode-unlimited PATH shim.
func IsOpenhopShim(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	return strings.Contains(lower, "/openhop/") || strings.Contains(lower, "/opencode-unlimited/")
}

// IsShim reports whether path is a wrapper script (.cmd/.bat) or an openhop
// shim that must not be treated as the native opencode binary.
func IsShim(path string) bool {
	if IsOpenhopShim(path) {
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".cmd" || ext == ".bat"
}

func candidates() []string {
	var out []string
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			out = append(out, filepath.Join(appdata, "npm", "node_modules", "@opencode", "cli", "bin", "opencode.exe"))
			// OpenCode Desktop stages its bundled CLI under %APPDATA%\OpenCode\cli\<stage>\opencode*.exe
			if matches, err := filepath.Glob(filepath.Join(appdata, "OpenCode", "cli", "*", "opencode*.exe")); err == nil {
				out = append(out, matches...)
			}
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			out = append(out,
				filepath.Join(local, "npm", "node_modules", "@opencode", "cli", "bin", "opencode.exe"),
				filepath.Join(local, "Programs", "OpenCode", "resources", "opencode-cli.exe"),
				filepath.Join(local, "Programs", "OpenCode", "resources", "opencode.exe"),
				filepath.Join(local, "OpenCode", "resources", "opencode-cli.exe"),
				filepath.Join(local, "OpenCode", "resources", "opencode.exe"),
			)
			if matches, err := filepath.Glob(filepath.Join(local, "OpenCode", "cli", "*", "opencode*.exe")); err == nil {
				out = append(out, matches...)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".opencode", "bin", "opencode"),
			filepath.Join(home, ".local", "bin", "opencode"),
			filepath.Join(home, ".bun", "bin", "opencode"),
		)
		if runtime.GOOS == "darwin" {
			if matches, err := filepath.Glob(filepath.Join(home, "Library", "Application Support", "OpenCode", "cli", "*", "opencode*")); err == nil {
				out = append(out, matches...)
			}
		} else if runtime.GOOS == "linux" {
			if matches, err := filepath.Glob(filepath.Join(home, ".config", "OpenCode", "cli", "*", "opencode*")); err == nil {
				out = append(out, matches...)
			}
		}
	}
	out = append(out,
		"/Applications/OpenCode.app/Contents/Resources/opencode-cli",
		"/Applications/OpenCode.app/Contents/Resources/opencode",
		"/usr/local/bin/opencode",
		"/usr/bin/opencode",
		"/opt/homebrew/bin/opencode",
	)
	return out
}

func Find() (string, error) {
	all := FindAll()
	if len(all) > 0 {
		return all[0], nil
	}
	return "", errors.New("official opencode binary not found")
}

// FindAll returns every distinct official opencode binary found across known
// CLI and Desktop locations and PATH (excluding shims).
func FindAll() []string {
	var out []string
	seen := map[string]bool{}
	add := func(candidate string) {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || IsShim(candidate) {
			return
		}
		key := candidate
		if runtime.GOOS == "windows" {
			key = strings.ToLower(filepath.Clean(candidate))
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, candidate)
	}
	for _, candidate := range candidates() {
		add(candidate)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, name := range Names() {
			add(filepath.Join(dir, name))
		}
	}
	return out
}

// Shims returns every opencode launcher visible on PATH, including shims.
func Shims() []string {
	var out []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, name := range Names() {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				out = append(out, candidate)
			}
		}
	}
	return out
}

// First returns the launcher a shell would run first, including a shim.
func First() string {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, name := range Names() {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate
			}
		}
	}
	return ""
}

func Version(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// DataDir mirrors the OpenCode data directory used by 2.2.5.
func DataDir() string {
	if value := os.Getenv("XDG_DATA_HOME"); value != "" {
		return filepath.Join(value, "opencode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// LogFile is the service log path. Standalone runs use opencode-local.log.
func LogFile() string {
	dir := DataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "log", "opencode.log")
}
