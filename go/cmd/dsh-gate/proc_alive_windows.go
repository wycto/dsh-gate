//go:build windows

package main

import "syscall"

// processAlive 判断某个进程号现在是否还在（Windows）。
//
// 标准库在 Windows 上发不了「信号 0」，所以用 OpenProcess 试探：
//   - 打开成功：进程在（顺手把句柄关掉，别漏内核对象）；
//   - ERROR_ACCESS_DENIED：进程在，只是我们没权限（例如它以管理员身份起的）；
//   - 其它错误（ERROR_INVALID_PARAMETER 等）：进程已经没了。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// PROCESS_QUERY_LIMITED_INFORMATION 是目前唯一在受保护进程上也可能成功的
	// 查询权限，比 QUERY_INFORMATION 宽松。
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err == nil {
		_ = syscall.CloseHandle(handle)
		return true
	}
	return err == syscall.ERROR_ACCESS_DENIED
}
