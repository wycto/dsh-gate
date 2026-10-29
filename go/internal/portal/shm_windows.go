//go:build windows

package portal

import (
	"os"
	"path/filepath"
)

// clearStaleShmIndex 删掉过期的 WAL 索引文件（<db>-shm），返回是否真的删掉了。
//
// 只在 Windows 上做：计划任务被 `schtasks /End` 强制结束时进程不会走 sqlite 的
// 正常收尾，<db>-shm 会留在磁盘上；下一个进程（重装后的管理端）映射到这份过期
// 索引后，读有的表直接报 disk I/O error (266)，其它进程连库都打不开（1546）。
// 删掉它是安全的——索引可以从 <db>-wal 重建——前提是此刻没有进程开着它。
//
// Windows 上「删除正被打开的文件」本身就会失败（sqlite 打开 -shm 时不允许
// FILE_SHARE_DELETE），所以 os.Remove 成功就等于没人占用，不用另外判断。
func clearStaleShmIndex(dbPath string) bool {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return false
	}
	if err := os.Remove(abs + "-shm"); err != nil {
		return false
	}
	return true
}

// clearOrphanShmIndex 在打开库之前清掉「没有 WAL 却留着索引」的孤立 <db>-shm。
//
// 与 clearStaleShmIndex 的分工：那个是**打开失败之后**的自愈；而这个针对一种
// 更刁的残留——库能打开、Open 返回成功，可第一次真读表时才炸成
// `disk I/O error (1802)`（1802 = SQLITE_IOERR_FSTAT，索引里记着「有 WAL」，
// 去 stat 那个已被删掉的 <db>-wal 就失败）。实现在 Open 的自愈重试根本轮不到，
// 现场看到的是「管理端起来了、实例列表却是空的、重启实例报找不到实例」
// （0.3.39 强杀管理端后实测）。
//
// 判据取「有 -shm、没 -wal」：WAL 模式下的连接一定同时开着 <db>-wal，没有它
// 就说明这份索引没有对应的日志可以重建，纯属上个进程硬死的残骸。删除本身仍然
// 受 Windows 的共享语义保护：真被别的进程开着就删不掉，返回 false 不会误伤。
func clearOrphanShmIndex(dbPath string) bool {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return false
	}
	if _, err := os.Stat(abs + "-shm"); err != nil {
		return false
	}
	if _, err := os.Stat(abs + "-wal"); err == nil {
		// 有 WAL 就交给 sqlite 正常恢复，别动。
		return false
	}
	if err := os.Remove(abs + "-shm"); err != nil {
		return false
	}
	return true
}
