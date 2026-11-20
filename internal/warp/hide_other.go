//go:build !windows

package warp

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
