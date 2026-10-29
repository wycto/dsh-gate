//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	user32                    = syscall.NewLazyDLL("user32.dll")
	procShowWindow            = user32.NewProc("ShowWindow")
)

// swHide 是 ShowWindow 的 SW_HIDE。
const swHide = 0

// detachServiceConsole 报告这次 `service run` 是不是计划任务亲手开出来的那个控制台
// 进程；是的话它不该自己当管理端，而是派一个没有控制台的子进程去当。
//
// 为什么必须派子进程：控制台宿主在窗口被关时会把仍挂在这个控制台上的进程一律结束
// 掉——CTRL_CLOSE_EVENT 的处理器只有几秒钟，超时就是 TerminateProcess。实测 0.3.43
// 「藏窗口 + 吞掉 SIGTERM」这一版：关掉页签两秒后进程就没了，日志里连一行收尾都没
// 有。屏蔽信号救不了它，唯一的出路是根本不在那个控制台上。
//
// 也不 FreeConsole 摘掉自己的控制台：控制台归自己独占时 FreeConsole 会销毁它，此后
// Go 运行时在 Windows 上会以致命错误当场死掉（recover 不住），现场抓到过两种：
//
//	fatal error: runtime.preemptM: duplicatehandle failed   （errno=6，0.3.39）
//	fatal error: runtime: netpoll failed                    （errno=735，0.3.40）
//
// 而 DETACHED_PROCESS 是「从出生起就没有控制台」，跟「把控制台拆掉」不是一回事，
// 运行时不会碰那些失效的句柄。
//
// 从命令行手动跑 `service run` 时控制台还归用户的 shell 所有（独占判断见
// ownsConsole），此时就地当管理端：那条终端里的 Ctrl-C 仍然停得下来，日志也仍然
// 打在终端上，与 `start --foreground` 行为一致。
func detachServiceConsole() bool { return ownsConsole() }

// hideServiceConsole 尽量让那个窗口别露脸：只在控制台归自己独有时藏窗口。
//
// Windows 11 的默认终端应用是 Windows Terminal 时这一步不够看：可见的那个页签属于
// WindowsTerminal.exe，`GetConsoleWindow()` 拿到的是伪控制台的隐藏句柄，藏的是个
// 本来就不显示的东西，页签还是会闪出来。但只闪一下——派完管理端这个进程就退出了，
// 终端随之把页签关掉，不再有「留着一个窗口、关掉它服务就没了」这回事。
//
// 从命令行跑时控制台属于用户的终端（旁边还挂着 shell），藏窗口会连用户的终端一起
// 藏掉，那是不能干的事。
func hideServiceConsole() {
	if !ownsConsole() {
		return
	}
	if hwnd, _, _ := procGetConsoleWindow.Call(); hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

// ownsConsole 判断当前控制台是不是只有我们这个进程在用。
//
// 计划任务拉起时只有自己一个客户端；从终端里跑时至少还有 shell。没有控制台、
// 或者查询失败，都当作「不是独占」，什么都不做最安全。
func ownsConsole() bool {
	var pids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}
