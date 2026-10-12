//go:build windows

package main

import (
	"fmt"
	"os"
)

// terminateProcess 在 Windows 上终止进程。
//
// Windows 没有 SIGTERM 这一说：os.Process.Kill 走的是 TerminateProcess，
// 也就是直接结束，没有「优雅退出」的中间档。
func terminateProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("找不到进程 %d: %w", pid, err)
	}
	return proc.Kill()
}
