package dshproc

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTokenFileRoundTrip 锁住落盘格式与权限：令牌文件必须 0600，重启后能读回。
func TestTokenFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", ".dsh-gate-token")
	if err := writeTokenFile(path, "tok-123"); err != nil {
		t.Fatalf("写入令牌文件失败: %v", err)
	}
	if got := readTokenFile(path); got != "tok-123" {
		t.Errorf("读回的令牌 = %q，期望 %q", got, "tok-123")
	}
	if runtime.GOOS != "windows" {
		// Windows 上 Go 的权限位只是近似，不拿它当断言。
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat 失败: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("令牌文件权限 = %04o，期望 0600", perm)
		}
	}
}

// TestTokenFileIgnoredWhenMissing 文件缺失不该当成错误：读不到就是没有令牌。
func TestTokenFileIgnoredWhenMissing(t *testing.T) {
	if got := readTokenFile(filepath.Join(t.TempDir(), "nothing")); got != "" {
		t.Errorf("缺文件时读了 %q，应为空", got)
	}
	if got := readTokenFile(""); got != "" {
		t.Errorf("空路径时读了 %q，应为空", got)
	}
}

// TestNewLoadsTokenFromFile 是这条改动的要点：管理端重启后接手还活着的 dsh，
// 令牌要能从实例目录读回来，否则浏览器一对话就要求重新登录。
func TestNewLoadsTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dsh-gate-token")
	if err := writeTokenFile(path, "restored-token"); err != nil {
		t.Fatalf("写入令牌文件失败: %v", err)
	}
	s := New(Options{Host: "127.0.0.1", Port: 1, TokenFile: path})
	if got := s.Token(); got != "restored-token" {
		t.Errorf("启动时读回的令牌 = %q，期望 %q", got, "restored-token")
	}
}

// TestExplicitTokenWins 显式 --dsh-token 是用户的明确输入，不能被落盘值盖掉。
func TestExplicitTokenWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dsh-gate-token")
	if err := writeTokenFile(path, "from-file"); err != nil {
		t.Fatalf("写入令牌文件失败: %v", err)
	}
	s := New(Options{Host: "127.0.0.1", Port: 1, Token: "from-flag", TokenFile: path})
	if got := s.Token(); got != "from-flag" {
		t.Errorf("令牌 = %q，期望显式传入的 %q", got, "from-flag")
	}
}

// TestSetTokenPersists 学到令牌（从启动输出或用户粘的地址）时要落盘。
func TestSetTokenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", ".dsh-gate-token")
	s := New(Options{Host: "127.0.0.1", Port: 1, TokenFile: path})
	s.SetToken("learned-token")
	if got := readTokenFile(path); got != "learned-token" {
		t.Errorf("落盘的令牌 = %q，期望 %q", got, "learned-token")
	}
	// 再起一个 supervisor：模拟管理端重启。
	if got := New(Options{Host: "127.0.0.1", Port: 1, TokenFile: path}).Token(); got != "learned-token" {
		t.Errorf("重启后读回的令牌 = %q，期望 %q", got, "learned-token")
	}
}

// TestSetTokenDoesNotLogToken 锁住「令牌明文不进日志」这条承诺。
//
// 真实日志（portal.log）里曾出现完整地址 http://127.0.0.1:32100/?token=<明文>：
// 那条日志是给人拿去直连 dsh 的，但把万能钥匙写进日志等于把承诺写反了。现在只
// 打地址并把令牌换成 ***，明文要读实例目录的 0600 文件。
func TestSetTokenDoesNotLogToken(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	path := filepath.Join(t.TempDir(), ".dsh-gate-token")
	s := New(Options{Host: "127.0.0.1", Port: 32100, TokenFile: path, Logger: logger})
	s.SetToken("super-secret-token")

	out := buf.String()
	if strings.Contains(out, "super-secret-token") {
		t.Errorf("日志里出现了令牌明文:\n%s", out)
	}
	if !strings.Contains(out, "token=***") {
		t.Errorf("日志里应保留打码后的地址，实际:\n%s", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("日志里应指出令牌文件位置 %q，实际:\n%s", path, out)
	}
}

// TestSetTokenEmptyDoesNotWipe 空令牌不该清掉已有的落盘值。
func TestSetTokenEmptyDoesNotWipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".dsh-gate-token")
	if err := writeTokenFile(path, "keep-me"); err != nil {
		t.Fatalf("写入令牌文件失败: %v", err)
	}
	s := New(Options{Host: "127.0.0.1", Port: 1, TokenFile: path})
	s.SetToken("")
	if got := readTokenFile(path); got != "keep-me" {
		t.Errorf("落盘的令牌 = %q，空令牌不该覆盖它", got)
	}
}

// TestAdoptReportsPersistedToken 接管时报告令牌要读「解析后」的值，不能只看 Options.Token。
//
// 现场故障（0.3.40 之前）：管理端崩掉后被计划任务重新拉起，接管还活着的 dsh，日志
// 却报「接管的实例没有已知启动令牌」，浏览器一进实例就被要求重新登录——令牌其实
// 就在实例目录的 .dsh-gate-token 里。这条测试盯住「接管走的是读回来的令牌」。
func TestAdoptReportsPersistedToken(t *testing.T) {
	port := freePort(t)
	token := "persisted-adopt-token"
	startFakeDSH(t, port, token)

	firstOpts := launchOptions(t, port)
	firstOpts.TokenFile = filepath.Join(firstOpts.Home, ".dsh-gate-token")
	first := New(firstOpts)
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("拉起 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = first.Shutdown(t.Context()) })

	// 模拟管理端重启：新 supervisor 只有 TokenFile，内存里没有令牌。
	secondOpts := launchOptions(t, port)
	secondOpts.TokenFile = firstOpts.TokenFile
	second := New(secondOpts)
	if err := second.Start(t.Context()); err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	t.Cleanup(func() { _ = second.Shutdown(t.Context()) })

	if !second.Adopted() {
		t.Fatal("端口被占时应当接管，而不是重新拉起")
	}
	if got := second.Token(); got != token {
		t.Errorf("接管后的令牌 = %q，期望从落盘文件读回 %q", got, token)
	}
}
