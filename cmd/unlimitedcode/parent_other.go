//go:build !windows

package main

import (
	"context"
	"errors"
	"syscall"
	"time"
)

func watchParent(ctx context.Context, pid int, cancel context.CancelFunc) {
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
				cancel()
				return
			}
		}
	}
}
