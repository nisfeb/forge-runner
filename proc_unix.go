//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// ownGroup puts the command in its own process group, so a kill reaches
// everything a build script started.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
