//go:build unix

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// sysProcAttrNewPGroup returns SysProcAttr that puts the child in its own
// process group, so killGroup can SIGTERM the entire tree.
func sysProcAttrNewPGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killGroup sends SIGTERM to the child's process group, waits a short grace
// period, then sends SIGKILL if the process is still alive. This catches
// `docker build`'s buildkit helper subprocesses too.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// fall back to single-process kill
		_ = cmd.Process.Signal(syscall.SIGTERM)
		time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(5*time.Second, func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
}
