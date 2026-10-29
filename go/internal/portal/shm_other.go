//go:build !windows

package portal

// clearStaleShmIndex 在非 Windows 平台不做事，返回 false。
//
// Unix 上进程退出（包括被杀）时由内核回收共享内存文件，正常情况下不会留下
// 需要人工清理的过期索引；真遇到读错误时就在这里原样报错，不自作主张删文件
// （POSIX 允许删除别人正在映射的 -shm，贸然删反而会伤到正在跑的管理端）。
func clearStaleShmIndex(string) bool { return false }

// clearOrphanShmIndex 在非 Windows 平台不做事，返回 false。
//
// 理由同上：POSIX 上允许删除别人正在映射的 -shm，猜错就要伤到正在跑的管理端，
// 这个自愈只在 Windows 那个「硬死留下索引」的特定缺陷上才值得做。
func clearOrphanShmIndex(string) bool { return false }
