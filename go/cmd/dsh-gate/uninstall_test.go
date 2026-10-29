package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wycto/dsh-gate/internal/portal"
)

// TestUninstallRemovesInstancesAndOptionallyData 卸载时先删实例登记；只有选择
// 删数据时才动 DSH_HOME。
func TestUninstallRemovesInstancesAndOptionallyData(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "portal.db")
	store, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	homeA := filepath.Join(dir, "home-a")
	homeB := filepath.Join(dir, "home-b")
	for _, home := range []string{homeA, homeB} {
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	}
	instA := &portal.Instance{Name: "demo-a", DshHome: homeA, DshPort: 32101}
	instB := &portal.Instance{Name: "demo-b", DshHome: homeB, DshPort: 32102}
	for _, inst := range []*portal.Instance{instA, instB} {
		if err := store.CreateInstance(inst); err != nil {
			t.Fatalf("建实例失败: %v", err)
		}
	}
	instances, err := store.ListInstances()
	if err != nil {
		t.Fatalf("列实例失败: %v", err)
	}
	_ = store.Close()

	if code := removeInstancesForUninstall(dir, dbPath, instances, true, false); code != 0 {
		t.Fatalf("删除实例返回非零: %d", code)
	}
	check, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("重开测试库失败: %v", err)
	}
	defer check.Close()
	if got, err := check.ListInstances(); err != nil || len(got) != 0 {
		t.Fatalf("实例登记应已清空，得到 %d 个, err=%v", len(got), err)
	}
	if _, err := os.Stat(homeA); !os.IsNotExist(err) {
		t.Errorf("选择删数据后 home-a 应被删除，stat err=%v", err)
	}
	if _, err := os.Stat(homeB); !os.IsNotExist(err) {
		t.Errorf("选择删数据后 home-b 应被删除，stat err=%v", err)
	}
}

// TestUninstallKeepsDataWhenNotRequested 未选择删数据时只删登记，目录保留。
func TestUninstallKeepsDataWhenNotRequested(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "portal.db")
	store, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	inst := &portal.Instance{Name: "keep-me", DshHome: home, DshPort: 32103}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	instances, err := store.ListInstances()
	if err != nil {
		t.Fatalf("列实例失败: %v", err)
	}
	_ = store.Close()

	if code := removeInstancesForUninstall(dir, dbPath, instances, false, false); code != 0 {
		t.Fatalf("删除实例返回非零: %d", code)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("未选择删数据时目录应保留: %v", err)
	}
}

func TestIsYes(t *testing.T) {
	for _, answer := range []string{"y", "Y", "yes", " 是 "} {
		if !isYes(answer) {
			t.Errorf("%q 应识别为肯定", answer)
		}
	}
	for _, answer := range []string{"", "n", "no", "否", "算了"} {
		if isYes(answer) {
			t.Errorf("%q 不应识别为肯定", answer)
		}
	}
}
