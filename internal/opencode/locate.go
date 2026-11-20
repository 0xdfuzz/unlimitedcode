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

// IsShim reports whether path belongs to the openhop PATH shim or another
// launcher that must not be treated as the official binary.
func IsShim(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	if strings.Contains(lower, "/openhop/") {
		return true
	}
	ext := strings.ToLower(filepath.Ext(lower))
	return ext == ".cmd" || ext == ".bat"
}

func candidates() []string {
	var out []string
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			out = append(out, filepath.Join(appdata, "npm", "node_modules", "@opencode", "cli", "bin", "opencode.exe"))
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			out = append(out, filepath.Join(local, "npm", "node_modules", "@opencode", "cli", "bin", "opencode.exe"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".local", "bin", "opencode"))
		out = append(out, filepath.Join(home, ".bun", "bin", "opencode"))
	}
	out = append(out, "/usr/local/bin/opencode", "/usr/bin/opencode", "/opt/homebrew/bin/opencode")
	return out
}

func Find() (string, error) {
	for _, candidate := range candidates() {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && !IsShim(candidate) {
			return candidate, nil
		}
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, name := range Names() {
			candidate := filepath.Join(dir, name)
			info, err := os.Stat(candidate)
			if err == nil && !info.IsDir() && !IsShim(candidate) {
				return candidate, nil
			}
		}
	}
	return "", errors.New("official opencode binary not found")
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
