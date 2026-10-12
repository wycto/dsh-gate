//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// applyDetach 让子进程脱离当前终端。
//
// restart 拉起的新进程必须比这条命令活得久：命令自己很快就会返回、终端随即
// 交还给用户，不脱离的话新进程会跟着终端一起被收走。Setsid 之后它进入新的
// 会话，收终端发来的 SIGHUP 也收不到。
func applyDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
