//go:build !windows

package main

// hideServiceConsole 在 Linux / macOS 上什么都不用做：systemd 单元没有控制台，
// launchd 的进程也不连终端，服务管理器停了就是停了，不存在「关窗口把服务关掉」。
func hideServiceConsole() {}

// detachServiceConsole 在 Linux / macOS 上永远是 false：那里的服务管理器本来就
// 要求被托管的进程留在前台（systemd 的 Type=simple 靠这个进程判断服务在不在），
// 派子进程自己退出会被当成服务退出。
func detachServiceConsole() bool { return false }
