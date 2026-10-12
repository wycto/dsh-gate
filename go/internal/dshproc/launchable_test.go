package dshproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckLaunchable 在 fork 之前拦住「路径记下了但文件没了」。
//
// 用户反馈的现象是 `fork/exec …/node/bin/npx: no such file or directory`：一句话
// 只说现象，用户不知道该删哪个目录、也不知道重启有没有用。
func TestCheckLaunchable(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "npx")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	// 绝对路径且存在 → 放行。
	if err := checkLaunchable(real); err != nil {
		t.Fatalf("存在的文件被拦了: %v", err)
	}

	// 绝对路径但已消失 → 拦下，并说清怎么办。
	gone := filepath.Join(dir, "gone", "bin", "npx")
	err := checkLaunchable(gone)
	if err == nil {
		t.Fatal("文件不存在时应当拦下")
	}
	if !strings.Contains(err.Error(), gone) {
		t.Errorf("错误应带上那条路径，实际: %v", err)
	}
	for _, want := range []string{"删除", "node/", "restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应说明「%s」怎么办，实际: %v", want, err)
		}
	}

	// PATH 里的相对命令不归我们查（exec 自己解析），别误伤。
	if err := checkLaunchable("npx"); err != nil {
		t.Errorf("相对命令不该被拦: %v", err)
	}
}

// TestRuntimeUsableRejectsBrokenSymlink 官方包里 npx 是软链：链接在、目标不在，
// 不能算「装好了」——否则会拿一份残缺运行时去启动 dsh。
func TestRuntimeUsableRejectsBrokenSymlink(t *testing.T) {
	binDir := t.TempDir()
	nodeName := "node"
	if os.PathSeparator == '\\' {
		nodeName = "node.exe"
	}
	if err := os.WriteFile(filepath.Join(binDir, nodeName), []byte("ELF"), 0o755); err != nil {
		t.Fatalf("准备 node 失败: %v", err)
	}
	// 指向一个不存在的目标。
	if err := os.Symlink(filepath.Join(binDir, "missing.js"), nodeNPXPath(binDir)); err != nil {
		t.Fatalf("准备软链失败: %v", err)
	}
	if runtimeUsable(binDir) {
		t.Error("软链目标缺失时不该判定为可用")
	}
}

// TestRuntimeUsableNeedsNode 只有 npx 没有 node 也不算数：npx 的 shebang 是
// #!/usr/bin/env node，起不来。
func TestRuntimeUsableNeedsNode(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(nodeNPXPath(binDir), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("准备 npx 失败: %v", err)
	}
	if runtimeUsable(binDir) {
		t.Error("缺少 node 时不该判定为可用")
	}
}
