//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive 判断某个进程号现在是否还在。
//
// restart 用它等上一代网关**真正退出**。只看网关端口关没关是不够的：
// Instance.Shutdown 先关 HTTP 监听、再收尾 dsh，而 dsh 的收尾要等它自己的
// 宽限期（默认 10 秒），实测里端口关掉之后 dsh 还能活十几秒（见 awaitStopped）。
//
// 信号 0 不发送任何东西，只做权限与存在性检查：
//   - nil：进程在；
//   - EPERM：进程在，只是不属于我们（sudo 下的实例）；
//   - ESRCH：进程已经没了。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
