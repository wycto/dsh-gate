//go:build !linux && !darwin && !windows

package dshproc

// findPID 在剩下的平台上一律返回 0。
//
// Linux 走 /proc，macOS 走 lsof，Windows 走 iphlpapi（见 probe_pid_windows.go）。
// 再往下的平台（*BSD 之类）要拿到同样的信息得调各系统专有 API，代码量与
// 平台矩阵都不划算。PID 用于两件事：「check」里的一句诊断提示，以及停掉
// 「接管来的、网关没有子进程句柄的」那个 dsh。拿不到就写「未知」，并在停止
// 时如实报错——不会假装停成功了。
func findPID(port int) int { return 0 }
