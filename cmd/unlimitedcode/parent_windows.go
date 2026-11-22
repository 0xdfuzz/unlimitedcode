//go:build windows

package main

import (
	"context"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const (
	processSynchronize = 0x00100000
	infiniteTimeout    = 0xFFFFFFFF
	waitObject0        = 0x00000000
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procCreateEventW         = kernel32.NewProc("CreateEventW")
	procSetEvent             = kernel32.NewProc("SetEvent")
	procWaitForMultipleObjs  = kernel32.NewProc("WaitForMultipleObjects")
)

// watchParent blocks at 0% CPU until the watched OpenCode process exits.
// If another OpenCode process (Desktop GUI, CLI TUI, or background service) is
// still running, it adopts that PID instead of shutting down the proxy early.
func watchParent(ctx context.Context, pid int, cancel context.CancelFunc, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	for {
		if !waitProcessExit(ctx, pid) {
			return
		}
		if ctx.Err() != nil {
			return
		}
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

// waitProcessExit blocks at 0% CPU until either the target process exits or
// ctx is cancelled, using a Win32 manual-reset event + WaitForMultipleObjects
// so handles are never closed while waited on.
func waitProcessExit(ctx context.Context, pid int) bool {
	handle, err := syscall.OpenProcess(processSynchronize, false, uint32(pid))
	if err != nil {
		return true
	}
	defer syscall.CloseHandle(handle)

	evRaw, _, _ := procCreateEventW.Call(0, 1, 0, 0)
	if evRaw == 0 {
		return ctx.Err() == nil
	}
	ev := syscall.Handle(evRaw)
	wakeDone := make(chan struct{})
	stopWake := context.AfterFunc(ctx, func() {
		defer close(wakeDone)
		_, _, _ = procSetEvent.Call(uintptr(ev))
	})
	defer func() {
		if !stopWake() {
			<-wakeDone
		}
		_ = syscall.CloseHandle(ev)
	}()

	handles := [2]syscall.Handle{handle, ev}
	r, _, _ := procWaitForMultipleObjs.Call(
		uintptr(len(handles)),
		uintptr(unsafe.Pointer(&handles[0])),
		0, // bWaitAll = FALSE
		infiniteTimeout,
	)
	return r == waitObject0
}

func findSurvivingOpenCodePID(excludePID int) int {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(snap)

	self := uint32(os.Getpid())
	excl := uint32(excludePID)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	fallback := 0
	for err = syscall.Process32First(snap, &entry); err == nil; err = syscall.Process32Next(snap, &entry) {
		if entry.ProcessID == self || entry.ProcessID == excl {
			continue
		}
		name := strings.ToLower(syscall.UTF16ToString(entry.ExeFile[:]))
		// Prefer the long-lived opencode-cli service backend if present.
		if name == "opencode-cli.exe" {
			return int(entry.ProcessID)
		}
		if name == "opencode.exe" && fallback == 0 {
			fallback = int(entry.ProcessID)
		}
	}
	return fallback
}
