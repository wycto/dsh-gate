//go:build !windows

package dshproc

import (
	"errors"
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

// terminatePID 请一个「不是网关启动」的进程退出（SIGTERM）。
//
// 与 terminate 的关键区别：这里只发给**那一个进程**，不发进程组。接管来的
// dsh 多半是用户在自己终端里跑的，它的进程组就是那个终端的——对组发信号会
// 连带打死他的 shell 和同组的一切。网关不拥有那个组，就没权动它。
func terminatePID(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil // 进程已经不在，这正是我们要的结果
	}
	return err
}

// killPID 强制结束那一个进程（不带负号，理由同 terminatePID）。
func killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// applyProcessAttrs 让 dsh 子进程独立成组（Setpgid）。
//
// 这是组信号的前置条件：npx 启动的 node 孙进程与直接子进程同组，整组才能
// 一次杀干净。代价是前台运行时终端的 Ctrl-C 不再直达 dsh——不过网关收到
// 后本就会经 stopChild 主动转发 SIGTERM，优雅退出不受影响。
func applyProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// applyProbeAttrs 在 Unix 上什么都不用做：探测子进程本来就跟着当前终端走，
// 没有「新开一个窗口」这回事（Windows 上才有，见 process_windows.go）。
func applyProbeAttrs(cmd *exec.Cmd) {}
