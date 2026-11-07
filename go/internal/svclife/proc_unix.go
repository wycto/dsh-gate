//go:build !windows

package svclife

import (
	"os/exec"
	"syscall"
)

// configureProc 让子进程自成进程组，停止时能整组终止、不留孤儿。
func configureProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProc 向整个进程组发信号：hard=false 用 SIGTERM，true 用 SIGKILL。
func terminateProc(cmd *exec.Cmd, hard bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-cmd.Process.Pid, sig) // 负 pid = 整个进程组
}
