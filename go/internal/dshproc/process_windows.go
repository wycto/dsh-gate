//go:build windows

package dshproc

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// terminate 请求子进程退出。
//
// Windows 没有可供任意进程接收的「优雅终止」信号，os.Process.Signal 只支持
// Kill 与部分控制台事件，所以这里直接结束进程树。dsh 的会话索引在运行期就
// 持续落盘（SQLite），因此强制结束不会像 SIGKILL 那样损坏数据。
func terminate(cmd *exec.Cmd) error {
	return killTree(cmd)
}

// kill 强制结束子进程树。
func kill(cmd *exec.Cmd) error {
	return killTree(cmd)
}

// terminatePID 结束一个「不是网关启动」的进程。
//
// Windows 没有优雅终止信号可发，所以 terminatePID 与 killPID 是同一件事。
// 从这里开始只按进程号动手，因为我们手里没有 *exec.Cmd——那个 dsh 是接管来的。
func terminatePID(pid int) error {
	return killPID(pid)
}

// killPID 按进程号结束整棵进程树。
//
// 仍然带上 /T：接管到的往往是 `npx` 底下那一层真正的 node，它的子孙还得跟着
// 一起收，否则停完端口空了、桌上却留下一串孤儿。
func killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	k := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(pid))
	applyProbeAttrs(k) // 停实例不该在桌面上闪一个黑框
	err := k.Run()
	if err == nil {
		return nil
	}
	// 128 = 「没有找到该进程」：它已经自己退了，这正是我们要的结果。
	// 把它当失败会让「停止一个已经死掉的接管实例」报成操作失败。
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 128 {
		return nil
	}
	// 精简系统里可能根本没有 taskkill（127/找不到可执行文件）：退回普通 API，
	// 只能结束它自己一个进程，聊胜于无。
	proc, findErr := os.FindProcess(pid)
	if findErr != nil {
		return err
	}
	if killErr := proc.Kill(); killErr != nil {
		return err
	}
	return nil
}

// killTree 结束子进程以及它名下的整棵进程树。
//
// 只结束直接子进程是不够的，这一点在 Windows 上尤其致命：dsh 的启动命令是
// `npx --yes @deepseek-ai/dsh web …`，npx 自己只是个壳，真正监听端口的 node
// 在它下面一层。壳被结束、node 还活着，于是「重启实例」实际做成了「停掉壳、
// 再接管自己留下的孤儿」——实例看着还在跑（端口有人听），令牌却随着旧进程一起
// 丢了，浏览器一对话就要求重新登录；每重启一次还多留一个 node 进程。0.3.39
// 实测：连续重启后 32100 上挂着两代孤儿，日志里一串「接管已在运行的 dsh 实例」。
//
// taskkill /T 按父子关系整棵收掉，语义与 POSIX 上 kill 进程组等价。
func killTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	k := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(pid))
	applyProbeAttrs(k) // 别让收尸动作自己弹一个黑框
	if err := k.Run(); err != nil {
		// 精简系统里可能没有 taskkill：退回只结束直接子进程，聊胜于无。
		return cmd.Process.Kill()
	}
	// taskkill 已经把壳收掉了；这里再确认一次，顺带让 Job/句柄进入收尾状态。
	_ = cmd.Process.Kill()
	return nil
}

// createNoWindow 对应 CreateProcess 的 CREATE_NO_WINDOW：给控制台程序一个
// 不显示的控制台。
//
// 为什么不能只靠 HideWindow：HideWindow 写的是 STARTUPINFO 的 wShowWindow，
// 只对 **GUI 子系统** 程序有效。dsh 这一串全是控制台程序（node.exe，以及包着
// 它的 cmd.exe / npx.cmd）——管理端自己跑在没有控制台的进程里（计划任务形态见
// cmd/dsh-gate 的 detachServiceConsole），Windows 于是会为每个控制台子进程新分配
// 一个控制台，也就是桌面上一闪而过的黑框。CREATE_NO_WINDOW 才是对控制台子进程有效
// 的那个开关（0.3.39 实测：实例重启一次，桌面闪一个框，刷新页面触发重启就是
// 「弹多次终端」）。
const createNoWindow = 0x08000000

// applyProcessAttrs 让 dsh 子进程不弹控制台窗口。
//
// 两个开关一起给：HideWindow 管 GUI 子系统，CREATE_NO_WINDOW 管控制台子系统，
// 而 dsh 是后者。
func applyProcessAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// applyProbeAttrs 让「探测 / 安装」这类一次性子进程同样不弹控制台窗口。
//
// 这些命令的输出都走管道（CombinedOutput），不依赖控制台；不隐藏的话，管理台
// 每刷新一次「运行环境」页、每次装 dsh，桌面上就会闪一串黑框。跟
// applyProcessAttrs 分开是因为语义不同：那个是长驻的 dsh，这个是跑完就退的探测。
func applyProbeAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
