//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

func sysProcAttrNewPGroup() *syscall.SysProcAttr {
	// CREATE_NEW_PROCESS_GROUP — prevents Ctrl+C from propagating from us
	// to the child unintentionally. We can't signal an arbitrary tree on
	// Windows, so cancellation is best-effort.
	return &syscall.SysProcAttr{CreationFlags: 0x00000200}
}

func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
