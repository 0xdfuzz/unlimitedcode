//go:build windows

package doctor

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNoWindow,
		HideWindow:    true,
	}
}

var conflictingExeNames = map[string]bool{
	"openhop.exe":                 true,
	"opencodeunlimited-multi.exe": true,
	"opencode-unlimited.exe":      true,
}

func hasConflictingBypassProcess() bool {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(snap)

	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = syscall.Process32First(snap, &entry); err == nil; err = syscall.Process32Next(snap, &entry) {
		name := strings.ToLower(syscall.UTF16ToString(entry.ExeFile[:]))
		if conflictingExeNames[name] {
			return true
		}
	}
	return false
}

func stopOtherBypassProcesses(ctx context.Context) bool {
	stopped := false
	// Terminate known standalone conflicting executables directly via Win32 API.
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err == nil {
		defer syscall.CloseHandle(snap)
		var entry syscall.ProcessEntry32
		entry.Size = uint32(unsafe.Sizeof(entry))
		for err = syscall.Process32First(snap, &entry); err == nil; err = syscall.Process32Next(snap, &entry) {
			name := strings.ToLower(syscall.UTF16ToString(entry.ExeFile[:]))
			if conflictingExeNames[name] {
				if p, err := os.FindProcess(int(entry.ProcessID)); err == nil {
					if p.Kill() == nil {
						stopped = true
					}
				}
			}
		}
	}
	// Also stop background Python proxy workers belonging to other OpenCode bypass tools.
	if tcpListening("127.0.0.1:41987") {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		script := `Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object { ($_.Name -ieq 'python.exe' -or $_.Name -ieq 'pythonw.exe') -and $_.CommandLine -and ($_.CommandLine -like '*zen_proxy.py*' -or $_.CommandLine -like '*warp_keeper.py*' -or $_.CommandLine -like '*zen_guard.py*') } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`
		cmd := exec.CommandContext(cctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
		hideWindow(cmd)
		if cmd.Run() == nil {
			stopped = true
		}
	}
	return stopped
}
