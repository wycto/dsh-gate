package portal

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestOpenClearsOrphanShm 锁住「硬死留下的孤立索引」这条自愈。
//
// 现场：管理端被强杀后 <db>-shm 留在磁盘、<db>-wal 已经没了；下一个管理端
// Open 成功（所以 Open 里的失败重试轮不到），但第一次真读表就报
// disk I/O error (1802)，表现成「实例列表是空的、重启实例报找不到实例」。
func TestOpenClearsOrphanShm(t *testing.T) {
	if runtime.GOOS != "windows" {
		// 自愈只在 Windows 上生效：POSIX 上删别人的 -shm 可能伤到正在跑的进程。
		t.Skip("孤立索引清理只在 Windows 上生效")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "portal.db")

	// 先建一个真实可用的库并关掉。
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	// 制造孤立索引：只有 -shm，没有 -wal。
	if err := os.WriteFile(dbPath+"-shm", make([]byte, 32768), 0o600); err != nil {
		t.Fatalf("写孤立索引失败: %v", err)
	}
	_ = os.Remove(dbPath + "-wal")

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("有孤立索引时打开失败: %v", err)
	}
	defer s2.Close()
	if _, err := s2.ListInstances(); err != nil {
		t.Fatalf("孤立索引之后读实例列表失败: %v", err)
	}
	if _, err := os.Stat(dbPath + "-shm"); err == nil {
		t.Log("打开后 SQLite 又建了新的 -shm，属正常（索引可重建）")
	}
}
