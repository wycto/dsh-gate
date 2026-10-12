package dshproc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExec 写一个可执行的 shell 脚本到目录里，用于伪造 PATH 上的命令。
func writeExec(t *testing.T, dir, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skipf("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

// fakeNpx 生成一个可编程的假 npx 脚本：按调用形态返回不同输出。
// 冒烟调用是 `npx --yes @deepseek-ai/dsh web --help`（$3=web $4=--help）。
func fakeNpx(webHelpOutput string) string {
	return "if [ \"$3\" = web ] && [ \"$4\" = \"--help\" ]; then\n" + webHelpOutput + "\nfi\nexit 0\n"
}

// TestEnsureDSHWithoutNpx 锁定「没有 npx」时的指引：dsh 官方启动方式依赖
// npx，这是必须说清楚的第一道门槛。
func TestEnsureDSHWithoutNpx(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	t.Setenv("PATH", t.TempDir())

	_, err := EnsureDSH(context.Background(), nil, "")
	if err == nil {
		t.Fatal("没有 npx 时应当返回错误")
	}
	if !strings.Contains(err.Error(), "npx") || !strings.Contains(err.Error(), "Node.js") {
		t.Fatalf("错误应指路 npx 与 Node.js，实际: %v", err)
	}
}

// TestEnsureDSHSmokePass 冒烟通过时返回官方命令形态。
func TestEnsureDSHSmokePass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	bin := t.TempDir()
	writeExec(t, bin, "npx", fakeNpx("echo --host; echo --port; echo --no-open"))
	t.Setenv("PATH", bin)

	resolved, err := EnsureDSH(context.Background(), nil, "")
	if err != nil {
		t.Fatalf("EnsureDSH 失败: %v", err)
	}
	if !strings.Contains(resolved.Command, "--yes") || !strings.Contains(resolved.Command, dshPackage) {
		t.Fatalf("EnsureDSH = %q，期望 npx --yes 官方形态", resolved.Command)
	}
	if resolved.Registry != "" {
		t.Errorf("官方源就用默认源，不该换成 %q", resolved.Registry)
	}
}

// TestEnsureDSHSilentExitGuidance 入口静默不执行（exit 0 无输出）时的指引
// 必须带证据：说清是 bin.js 的 import.meta.main 守门与 Node 版本的关系。
func TestEnsureDSHSilentExitGuidance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	bin := t.TempDir()
	writeExec(t, bin, "npx", fakeNpx(":")) // 静默退出
	t.Setenv("PATH", bin)

	_, err := EnsureDSH(context.Background(), nil, "")
	if err == nil {
		t.Fatal("入口静默退出时应当返回错误")
	}
	if !strings.Contains(err.Error(), "import.meta.main") || !strings.Contains(err.Error(), "静默退出") {
		t.Fatalf("错误应包含证据（import.meta.main 守门）与现象，实际: %v", err)
	}
}

// TestEnsureDSHCrashBannerGuidance 旧 Node 崩溃时指引要引用横幅里的版本。
func TestEnsureDSHCrashBannerGuidance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	bin := t.TempDir()
	writeExec(t, bin, "npx", fakeNpx("echo SyntaxError: parseEnv >&2; echo Node.js v18.20.4 >&2; exit 1"))
	t.Setenv("PATH", bin)

	_, err := EnsureDSH(context.Background(), nil, "")
	if err == nil {
		t.Fatal("崩溃时应当返回错误")
	}
	if !strings.Contains(err.Error(), "Node.js v18.20") || !strings.Contains(err.Error(), "parseEnv") {
		t.Fatalf("错误应引用崩溃证据（版本横幅与报错），实际: %v", err)
	}
}

// TestEnsureDSHBindingGuidance 缺平台原生绑定时指引要引用原始报错，
// 并说明这是上游分发问题、网关修不了。
func TestEnsureDSHBindingGuidance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	bin := t.TempDir()
	writeExec(t, bin, "npx", fakeNpx("echo 'dsh: fatal uncaught exception: Error: dsh: host preparation failed: "+
		"No usable native binding found for node-addon-require-builtin-linux-x64-gnu (auto)' >&2; exit 1"))
	t.Setenv("PATH", bin)

	_, err := EnsureDSH(context.Background(), nil, "")
	if err == nil {
		t.Fatal("缺绑定时应当返回错误")
	}
	if !strings.Contains(err.Error(), "原生绑定") ||
		!strings.Contains(err.Error(), "linux-x64-gnu") ||
		!strings.Contains(err.Error(), "dsh 上游") {
		t.Fatalf("错误应引用绑定缺失证据并指向上游，实际: %v", err)
	}
}
