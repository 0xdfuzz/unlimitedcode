//go:build windows

package setup

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"unlimitedcode/internal/config"
	"unlimitedcode/internal/opencode"
)

const (
	runKey  = `HKCU:\Software\Microsoft\Windows\CurrentVersion\Run`
	runName = "openhop"
)

type backup struct {
	Path string `json:"path"`
	Run  string `json:"run,omitempty"`
}

func UserPath() (string, error) {
	out, err := powershell("[Environment]::GetEnvironmentVariable('Path','User')")
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\r\n"), nil
}

func SetUserPath(value string) error {
	_, err := powershellWithEnv("[Environment]::SetEnvironmentVariable('Path', $env:UNLIMITEDCODE_NEW_VALUE, 'User')", value)
	return err
}

// TakeoverPlan reports what --takeover would remove without changing anything.
func TakeoverPlan() (removed []string, run string, err error) {
	current, err := UserPath()
	if err != nil {
		return nil, "", err
	}
	_, removed = StripOpenhop(current)
	run, err = openhopRun()
	return removed, run, err
}

// TakeoverPath removes openhop from the user PATH and the logon Run key,
// saves both values first and makes sure an official launcher is reachable.
// Files and running processes are left alone.
func TakeoverPath(official string) (removed, added []string, runRemoved bool, err error) {
	current, err := UserPath()
	if err != nil {
		return nil, nil, false, err
	}
	run, err := openhopRun()
	if err != nil {
		return nil, nil, false, err
	}
	next, removed := StripOpenhop(current)
	if len(removed) == 0 && run == "" {
		return nil, nil, false, nil
	}
	if !hasLauncher(next) {
		dir := launcherDir(official)
		if dir == "" {
			return nil, nil, false, errors.New("official opencode launcher is not on PATH and no npm bin directory was found")
		}
		next = dir + string(os.PathListSeparator) + next
		added = append(added, dir)
	}
	if err := os.MkdirAll(filepath.Dir(backupPath()), 0o700); err != nil {
		return nil, nil, false, err
	}
	raw, err := json.Marshal(backup{Path: current, Run: run})
	if err != nil {
		return nil, nil, false, err
	}
	if err := os.WriteFile(backupPath(), append(raw, '\n'), 0o600); err != nil {
		return nil, nil, false, err
	}
	if len(removed) > 0 {
		if err := SetUserPath(next); err != nil {
			return nil, nil, false, err
		}
	}
	if run != "" {
		if err := removeRun(); err != nil {
			return nil, nil, false, err
		}
		runRemoved = true
	}
	return removed, added, runRemoved, nil
}

// Restore puts back the PATH and Run value saved by TakeoverPath.
func Restore() (pathRestored, runRestored bool, err error) {
	raw, err := os.ReadFile(backupPath())
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, errors.New("no backup found")
		}
		return false, false, err
	}
	var saved backup
	if err := json.Unmarshal(raw, &saved); err != nil {
		return false, false, err
	}
	if saved.Path != "" {
		if err := SetUserPath(saved.Path); err != nil {
			return false, false, err
		}
		pathRestored = true
	}
	if saved.Run != "" {
		if err := restoreRun(saved.Run); err != nil {
			return false, false, err
		}
		runRestored = true
	}
	if err := os.Remove(backupPath()); err != nil {
		return false, false, err
	}
	return pathRestored, runRestored, nil
}

func backupPath() string {
	return filepath.Join(config.Home(), "path-backup.json")
}

func openhopRun() (string, error) {
	out, err := powershell(`(Get-ItemProperty -Path '` + runKey + `' -Name ` + runName + ` -ErrorAction SilentlyContinue).` + runName)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func removeRun() error {
	_, err := powershell(`Remove-ItemProperty -Path '` + runKey + `' -Name ` + runName + ` -ErrorAction SilentlyContinue`)
	return err
}

func restoreRun(value string) error {
	_, err := powershellWithEnv(`New-ItemProperty -Path '`+runKey+`' -Name `+runName+` -PropertyType String -Value $env:UNLIMITEDCODE_NEW_VALUE -Force | Out-Null`, value)
	return err
}

func powershell(script string) (string, error) {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	return string(out), err
}

func powershellWithEnv(script, value string) (string, error) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "UNLIMITEDCODE_NEW_VALUE="+value)
	out, err := cmd.Output()
	return string(out), err
}

func hasLauncher(pathValue string) bool {
	for _, entry := range filepath.SplitList(pathValue) {
		if entry == "" || IsOpenhopEntry(entry) {
			continue
		}
		for _, name := range opencode.Names() {
			if info, err := os.Stat(filepath.Join(entry, name)); err == nil && !info.IsDir() {
				return true
			}
		}
	}
	return false
}

func launcherDir(official string) string {
	var dirs []string
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		dirs = append(dirs, filepath.Join(appdata, "npm"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		dirs = append(dirs, filepath.Join(local, "npm"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".local", "bin"))
	}
	for _, dir := range dirs {
		for _, name := range opencode.Names() {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
				return dir
			}
		}
	}
	return filepath.Dir(official)
}
