//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// Windows 上「脱离终端」用创建标志表达：
//
//   - CREATE_NEW_PROCESS_GROUP 让子进程有自己的控制组，restart 命令退出时不会
//     把控制台的关闭事件连带送给它；
//   - DETACHED_PROCESS 让它不继承当前控制台窗口，否则双击或关掉那个终端窗口
//     时新起的网关也会一起消失。
func applyDetach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // 0x8 = DETACHED_PROCESS
	}
}
