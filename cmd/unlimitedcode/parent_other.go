//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// watchParent monitors the watched OpenCode PID and adopts any surviving
// OpenCode process before shutting down so closing one window never cuts off
// another running OpenCode instance.
func watchParent(ctx context.Context, pid int, cancel context.CancelFunc, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
				next := findSurvivingOpenCodePID(pid)
				if next <= 0 {
					cancel()
					return
				}
				if logf != nil {
					logf("parent pid=%d exited; adopting surviving opencode pid=%d", pid, next)
				}
				pid = next
			}
		}
	}
}

func findSurvivingOpenCodePID(excludePID int) int {
	self := os.Getpid()
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return 0
		}
		fallback := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p, err := strconv.Atoi(e.Name())
			if err != nil || p == self || p == excludePID {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
			if err != nil {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(string(raw)))
			if name == "opencode-cli" {
				return p
			}
			if name == "opencode" && fallback == 0 {
				fallback = p
			}
		}
		return fallback
	}
	for _, procName := range []string{"opencode-cli", "opencode", "OpenCode"} {
		out, err := exec.Command("pgrep", "-x", procName).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Fields(string(out)) {
			if p, err := strconv.Atoi(line); err == nil && p > 0 && p != self && p != excludePID {
				return p
			}
		}
	}
	return 0
}
