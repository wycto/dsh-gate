//go:build !windows

package dshproc

import (
	"os/exec"
	"syscall"
)

// terminate 请求 dsh 进程组优雅退出（SIGTERM）。
//
// 信号发给**进程组**而不是单个进程：npx 这类启动器会再 fork 出真正的 node
// 进程，只杀直接子进程会留下孤儿——SIGKILL 之后「仍未收到退出通知」、
// 资源被僵尸孙进程拖着，都是这么来的（容器里踩过）。dsh 的子进程以独立
// 进程组启动（见 applyProcessAttrs），这里对整组发信号。
func terminate(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

// kill 强制结束整个 dsh 进程组。
func kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// applyProcessAttrs 让 dsh 子进程独立成组（Setpgid）。
//
// 这是组信号的前置条件：npx 启动的 node 孙进程与直接子进程同组，整组才能
// 一次杀干净。代价是前台运行时终端的 Ctrl-C 不再直达 dsh——不过网关收到
// 后本就会经 stopChild 主动转发 SIGTERM，优雅退出不受影响。
func applyProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
