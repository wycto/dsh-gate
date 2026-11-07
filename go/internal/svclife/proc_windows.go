//go:build windows

package svclife

import "os/exec"

// configureProc 在 Windows 上不需要额外设置。
func configureProc(cmd *exec.Cmd) {}

// terminateProc 在 Windows 上直接结束进程（hard 参数忽略）。
func terminateProc(cmd *exec.Cmd, hard bool) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
