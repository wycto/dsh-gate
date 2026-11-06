package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/registry"
)

// TestStatusExitCodeIsAHealthProbe 钉住 status 的退出码语义：管理端没在跑返回 1，
// 在跑返回 0。
//
// 为什么值得单独钉一条测试：这条退出码是「脚本能不能把它当探针用」的全部依据。
// 改之前它恒为 0，于是 `dsh-gate status || dsh-gate start` 永远不会触发，
// 而这类守卫正是 README 里给无 root 自启推荐的写法——一旦有人为了「报告成功」
// 把 return 改回去，自启就会静悄悄地失效，且没有任何输出提示。
func TestStatusExitCodeIsAHealthProbe(t *testing.T) {
	isolateRunDir(t)

	t.Run("未运行返回 1", func(t *testing.T) {
		stateDir := t.TempDir()
		if code := statusCommand([]string{"--state-dir", stateDir, "--db", filepath.Join(stateDir, "portal.db")}); code != 1 {
			t.Errorf("管理端未运行时 status 退出码应为 1，实际 %d", code)
		}
	})

	t.Run("运行中返回 0", func(t *testing.T) {
		stateDir := t.TempDir()

		// 造一个「活着」的管理端：登记里的端口必须真的能连上（Record.Alive 是 TCP 探测）。
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("占端口失败: %v", err)
		}
		defer listener.Close()
		port := listener.Addr().(*net.TCPAddr).Port

		if err := registry.DefaultDir().Save(registry.Record{
			Name: portalRecordName,
			PID:  os.Getpid(),
			Port: port,
		}); err != nil {
			t.Fatalf("写运行登记失败: %v", err)
		}

		if code := statusCommand([]string{"--state-dir", stateDir, "--db", filepath.Join(stateDir, "portal.db")}); code != 0 {
			t.Errorf("管理端运行中时 status 退出码应为 0，实际 %d", code)
		}
	})
}

// TestStatusReportsInstancesButStaysUnhealthy 确认「能读出实例定义」不等于「健康」。
//
// 管理端停着时数据库照旧可读，实例列表照旧打印——但退出码仍必须是 1。这两件事
// 容易在实现里被混成一个：读数据库成功就 return 0，探针语义就丢了。
func TestStatusReportsInstancesButStaysUnhealthy(t *testing.T) {
	isolateRunDir(t)
	stateDir := t.TempDir()
	dbPath := filepath.Join(stateDir, "portal.db")

	store, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	inst := portal.Instance{
		ID:      "abc123",
		Name:    "demo",
		DshHome: filepath.Join(t.TempDir(), "dsh"),
		DshPort: 32100,
	}
	if err := store.CreateInstance(&inst); err != nil {
		t.Fatalf("写实例失败: %v", err)
	}
	store.Close()

	if code := statusCommand([]string{"--state-dir", stateDir, "--db", dbPath}); code != 1 {
		t.Errorf("管理端未运行时，即使能列出实例，退出码也应为 1，实际 %d", code)
	}
}
