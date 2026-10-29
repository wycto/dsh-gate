//go:build !windows

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processAlive 判断某个进程号现在是否还在。
//
// restart 用它等上一代网关**真正退出**。只看网关端口关没关是不够的：
// Instance.Shutdown 先关 HTTP 监听、再收尾 dsh，而 dsh 的收尾要等它自己的
// 宽限期（默认 10 秒），实测里端口关掉之后 dsh 还能活十几秒（见 restart 的等待逻辑）。
//
// 信号 0 不发送任何东西，只做权限与存在性检查：
//   - nil：进程在；
//   - EPERM：进程在，只是不属于我们（sudo 下的实例）；
//   - ESRCH：进程已经没了。
//
// 僵尸进程要算「已经没了」：容器里 PID 1 常常不是 init（不回收孤儿），于是
// 退出后的进程会以 Z 状态挂着，对信号 0 仍然返回成功——restart 会一直等一个
// 早就退出的进程，直到超时报「旧进程没有退出」。实测在 Alpine 容器里必现。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !processIsZombie(pid)
}

// processIsZombie 读 /proc/<pid>/stat 判断进程是否处于 Z（僵尸）状态。
//
// 读不到文件时返回 false：那种情况进程大概真的没了（或平台不提供 /proc），
// 交由调用方按信号 0 的结果判断，不要在这里替它下结论。
func processIsZombie(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// stat 的第二个字段是括号里的 comm，里面可能带空格或括号；
	// 状态字符在**最后一个** ')' 之后，所以从右往左找才稳。
	text := string(raw)
	idx := strings.LastIndex(text, ")")
	if idx < 0 || idx+2 > len(text) {
		return false
	}
	fields := strings.Fields(text[idx+1:])
	if len(fields) == 0 {
		return false
	}
	return fields[0] == "Z"
}
