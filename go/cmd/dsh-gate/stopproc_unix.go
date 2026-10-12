//go:build !windows

package main

import "syscall"

// terminateProcess 请一个进程正常退出（SIGTERM）。
//
// 用 SIGTERM 而不是 SIGKILL：端口占用者可能是用户自己的另一个服务，得给它
// 收拾现场的机会（dsh 收到信号会注销自己、清掉运行记录）。
func terminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
