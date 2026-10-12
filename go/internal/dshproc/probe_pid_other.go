//go:build !linux && !darwin

package dshproc

// findPID 在非 Linux 平台上一律返回 0。
//
// 拿端口占用进程的 PID 在这些平台上需要调用 lsof/netstat 之类的外部命令，
// 或者走各系统的专有 API；前者会被精简过的容器镜像卡住，后者的代码量与
// 平台矩阵都不划算。PID 只用于「check」命令里的一句诊断提示，拿不到就写
// 「未知」，不影响端口探测与接管逻辑。
func findPID(port int) int { return 0 }
