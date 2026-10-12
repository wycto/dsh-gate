package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestInstallSelfToBinDirCopiesAndReplaces 锁住命令拷贝的三条行为：
// 首次拷进去、内容一致时不重复拷、内容变了要覆盖。
func TestInstallSelfToBinDirCopiesAndReplaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不走 PATH 目录拷贝")
	}
	dir := t.TempDir()

	path, changed, err := installSelfToBinDir(dir)
	if err != nil {
		t.Fatalf("首次拷贝失败: %v", err)
	}
	if !changed {
		t.Error("首次拷贝应报告发生了改动")
	}
	if filepath.Base(path) != serviceName {
		t.Errorf("落点 = %q，期望文件名 %s", path, serviceName)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("拷贝结果不存在: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("拷贝结果不可执行，权限 = %v", info.Mode().Perm())
	}
	if _, changed, err := installSelfToBinDir(dir); err != nil || changed {
		t.Errorf("内容一致时应静默复用：changed=%v err=%v", changed, err)
	}

	// 目标被换成旧版本（内容不同）时必须被覆盖，否则升级完敲到的还是老程序。
	if err := os.WriteFile(path, []byte("旧的二进制"), 0o755); err != nil {
		t.Fatalf("准备旧版本失败: %v", err)
	}
	path, changed, err = installSelfToBinDir(dir)
	if err != nil {
		t.Fatalf("覆盖失败: %v", err)
	}
	if !changed {
		t.Error("内容不同时应报告发生了改动")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取落点失败: %v", err)
	}
	want, err := os.ReadFile(mustExecutable(t))
	if err != nil {
		t.Fatalf("读取自身失败: %v", err)
	}
	if len(got) != len(want) {
		t.Errorf("覆盖后的内容大小 = %d，期望 %d", len(got), len(want))
	}
}

// TestInstallSelfToBinDirEmptyDirSkips 传空目录表示明确不拷贝。
func TestInstallSelfToBinDirEmptyDirSkips(t *testing.T) {
	path, changed, err := installSelfToBinDir("")
	if err != nil || changed || path != "" {
		t.Fatalf("空目录应什么都不做，得到 path=%q changed=%v err=%v", path, changed, err)
	}
}

// mustExecutable 返回当前测试二进制路径。
func mustExecutable(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("无法确定自身路径: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}
