package main

import (
	"os"
	"path/filepath"
	"testing"
)

// swapStdin 把标准输入换成一段文本，返回还原函数。
//
// setup 在非交互路径上通过 --password-stdin 从标准输入读口令，测试里用
// 常规文件冒充管道即可；term.IsTerminal 对普通文件一定返回 false，
// 因此整条向导都会走「不提问」的分支。
func swapStdin(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("准备标准输入失败: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开标准输入失败: %v", err)
	}
	original := os.Stdin
	os.Stdin = file
	t.Cleanup(func() {
		os.Stdin = original
		file.Close()
	})
}

// TestSetupCreatesFirstAccount 走通非交互主路径：建账号、不动其它东西。
