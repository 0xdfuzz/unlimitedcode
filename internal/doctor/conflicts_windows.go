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

var conflictingScheduledTasks = []string{
	"openhop-Keeper",
	"openhop-Guard",
	"OpencodeUnlimited-Keeper",
	"OpencodeUnlimited-Guard",
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

	// Stop and disable known user-level background Scheduled Tasks first so Task
	// Scheduler does not immediately respawn killed proxy workers.
	for _, task := range conflictingScheduledTasks {
		tctx, tcancel := context.WithTimeout(ctx, 2*time.Second)
		endCmd := exec.CommandContext(tctx, "schtasks", "/End", "/TN", task)
		hideWindow(endCmd)
		if endCmd.Run() == nil {
			stopped = true
		}
		disCmd := exec.CommandContext(tctx, "schtasks", "/Change", "/TN", task, "/DISABLE")
		hideWindow(disCmd)
		if disCmd.Run() == nil {
			stopped = true
		}
		tcancel()
	}

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

	// Stop background Python/Tor/wireproxy helper workers explicitly launched by
	// other OpenCode bypass tools without touching unrelated user apps.
	needWorkerCleanup := false
	for _, addr := range knownBypassPorts {
		if tcpListening(addr) {
			needWorkerCleanup = true
			break
		}
	}
	if needWorkerCleanup {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		script := `$hit = 0; Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object {
  $n = [string]$_.Name
  $c = [string]$_.CommandLine
  $e = [string]$_.ExecutablePath
  (($n -ieq 'python.exe' -or $n -ieq 'pythonw.exe') -and ($c -like '*zen_proxy.py*' -or $c -like '*warp_keeper.py*' -or $c -like '*zen_guard.py*')) -or
  (($n -ieq 'tor.exe' -or $n -ieq 'wireproxy.exe') -and ($e -like '*opencode-infinite*' -or $c -like '*opencode-infinite*' -or $e -like '*olliecode*' -or $c -like '*olliecode*'))
} | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue; $hit++ }; if ($hit -gt 0) { Write-Output 'stopped' }`
		cmd := exec.CommandContext(cctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
		hideWindow(cmd)
		if out, err := cmd.Output(); err == nil && strings.Contains(string(out), "stopped") {
			stopped = true
		}
	}
	return stopped
}
