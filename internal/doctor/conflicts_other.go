//go:build !windows

package doctor

import (
	"context"
	"os/exec"
	"time"
)

func hideWindow(_ *exec.Cmd) {}

func hasConflictingBypassProcess() bool {
	return false
}

func stopOtherBypassProcesses(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stopped := false
	for _, pat := range []string{
		"zen_proxy.py",
		"warp_keeper.py",
		"zen_guard.py",
		"openhop",
		"opencode-infinite/tor",
		"opencode-infinite/wireguard",
	} {
		if exec.CommandContext(cctx, "pkill", "-f", pat).Run() == nil {
			stopped = true
		}
	}
	return stopped
}
