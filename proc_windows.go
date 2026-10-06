//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

func ownGroup(cmd *exec.Cmd) {}

// killGroup ends the process and its children with taskkill /T.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
