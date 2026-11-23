//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a process group of its own, so the window outlives
// the node.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
