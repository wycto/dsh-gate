//go:build windows

package dshproc

import (
	"os/exec"
	"syscall"
)

// terminate 请求子进程退出。
//
// Windows 没有可供任意进程接收的「优雅终止」信号，os.Process.Signal 只支持
// Kill 与部分控制台事件，所以这里直接结束进程。dsh 的会话索引在运行期就
// 持续落盘（SQLite），因此强制结束不会像 SIGKILL 那样损坏数据。
func terminate(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// kill 强制结束子进程。
func kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// applyProcessAttrs 隐藏子进程的控制台窗口。
//
// dsh-gate 常常是个双击启动的控制台程序；不隐藏的话，每次拉起 dsh 都会在
// 桌面上多闪一个黑框。HideWindow 只影响 GUI 程序的窗口显示，不影响真正的
// 控制台宿主（我们自己仍是控制台程序）。
func applyProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
