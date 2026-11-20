//go:build windows

package main

import (
	"context"
	"syscall"
)

const (
	processSynchronize = 0x00100000
	infiniteTimeout    = 0xFFFFFFFF
)

// watchParent blocks until the given parent PID exits (or ctx is cancelled),
// then calls cancel so the daemon exits cleanly without leaving zombies.
func watchParent(ctx context.Context, pid int, cancel context.CancelFunc) {
	if pid <= 0 {
		return
	}
	handle, err := syscall.OpenProcess(processSynchronize, false, uint32(pid))
	if err != nil {
		// Parent already exited before we could attach.
		cancel()
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = syscall.WaitForSingleObject(handle, infiniteTimeout)
		_ = syscall.CloseHandle(handle)
	}()
	select {
	case <-done:
		cancel()
	case <-ctx.Done():
		_ = syscall.CloseHandle(handle)
	}
}
