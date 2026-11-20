//go:build !windows

package resume

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
