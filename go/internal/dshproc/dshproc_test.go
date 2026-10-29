package dshproc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/dshtest"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"dsh web", []string{"dsh", "web"}},
		{"  dsh   web  ", []string{"dsh", "web"}},
		{`dsh web --port 8080`, []string{"dsh", "web", "--port", "8080"}},
		{`dsh web --patch "my config.yml"`, []string{"dsh", "web", "--patch", "my config.yml"}},
		{`dsh web --patch 'my config.yml'`, []string{"dsh", "web", "--patch", "my config.yml"}},
		{`echo "say \"hi\""`, []string{"echo", `say "hi"`}},
		{`echo a\ b`, []string{"echo", "a b"}},
		{`echo ''`, []string{"echo", ""}},
		{`echo "a'b"`, []string{"echo", "a'b"}},
		{"", nil},
	}
	for _, tc := range cases {
		got, err := SplitCommand(tc.in)
		if err != nil {
			t.Errorf("SplitCommand(%q) 报错: %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("SplitCommand(%q) = %q，期望 %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitCommand(%q)[%d] = %q，期望 %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}

	// 引号没闭合几乎一定是用户写错了，必须报错而不是静默吞掉。
	for _, bad := range []string{`echo "unclosed`, `echo 'unclosed`, `echo \`} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("SplitCommand(%q) 应当报错", bad)
		}
	}
}

func TestSplitToken(t *testing.T) {
	cases := map[string]string{
		"dsh web: http://192.168.1.20:12020/?token=abc123": "abc123",
		"dsh web: http://127.0.0.1:3080/?token=abc&x=1":    "abc",
		`printed "http://127.0.0.1:3080/?token=abc"`:       "abc",
		"no token here":                   "",
		"dsh web: http://127.0.0.1:3080/": "",
	}
	for line, want := range cases {
		if got := SplitToken(line); got != want {
			t.Errorf("SplitToken(%q) = %q，期望 %q", line, got, want)
		}
	}
}

func TestRedactToken(t *testing.T) {
	line := "dsh web: http://192.168.1.20:12020/?token=SECRET123"
	got := RedactToken(line)
	if strings.Contains(got, "SECRET123") {
		t.Fatalf("令牌没有被脱敏: %q", got)
	}
	if !strings.Contains(got, "token=***") {
		t.Errorf("应当保留可读的痕迹，实际: %q", got)
	}
	// 脱敏后其它内容必须原样保留，否则日志会失去诊断价值。
	if !strings.Contains(got, "dsh web:") || !strings.Contains(got, "12020") {
		t.Errorf("脱敏破坏了原有内容: %q", got)
	}
	if RedactToken("nothing to redact") != "nothing to redact" {
		t.Error("没有令牌时不应改动内容")
	}
}

func TestExpandPlaceholders(t *testing.T) {
	supervisor := New(Options{
		Host:         "127.0.0.1",
		Port:         3080,
		Home:         "/home/me/.dsh-work",
		Workspace:    "/home/me/code",
		PublicURL:    "http://192.168.1.20:12020/",
		TrustedHosts: []string{"192.168.1.20", "box.local"},
	})

	got, err := supervisor.expand(
		"dsh web --host {host} --port {port} --url {url} --trusted-host {trusted} --trusted-host {trusted} --dir '{workspace}'")
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	want := "dsh web --host 127.0.0.1 --port 3080 --url http://192.168.1.20:12020/ " +
		"--trusted-host 192.168.1.20 --trusted-host box.local --dir '/home/me/code'"
	if got != want {
		t.Errorf("展开结果:\n got %q\nwant %q", got, want)
	}

	if _, err := supervisor.expand("dsh {unknown}"); err == nil {
		t.Error("未知占位符应当报错")
	}
	bare := New(Options{Host: "127.0.0.1", Port: 3080})
	if _, err := bare.expand("dsh web --trusted-host {trusted}"); err == nil {
		t.Error("没有可信地址时使用 {trusted} 应当报错")
	}
}

func TestBuildCommandAutoDetects(t *testing.T) {
	// 自动探测出来的命令必须带 --no-open：网关拉起的 dsh 不该再去开浏览器，
	// 尤其是在无界面服务器上，那只会打印一条让人误以为出错的诊断。
	supervisor := New(Options{
		Host:         "127.0.0.1",
		Port:         3080,
		Home:         t.TempDir(),
		Workspace:    t.TempDir(),
		PublicURL:    "http://192.168.1.20:12020/",
		TrustedHosts: []string{"192.168.1.20"},
	})
	binary, err := detectBinary("")
	if err != nil {
		t.Skipf("本机没有可用的 dsh 命令: %v", err)
	}
	args, err := supervisor.buildCommand(t.Context())
	if err != nil {
		t.Fatalf("生成命令失败: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{"web", "--host 127.0.0.1", "--port 3080", "--no-open",
		"--trusted-host 192.168.1.20"} {
		if !strings.Contains(joined, want) {
			t.Errorf("命令缺少 %q，实际: %s", want, joined)
		}
	}
	// --public-url 只在 dsh 认识它时才传：不同版本支持情况不同，
	// 传一个不认识的选项会让 dsh 直接退出。
	supported := supervisor.supportedFlags(t.Context(), binary)
	if supported["--public-url"] && !strings.Contains(joined, "--public-url") {
		t.Errorf("dsh 支持 --public-url 时应当传入，实际: %s", joined)
	}
	if !supported["--public-url"] && strings.Contains(joined, "--public-url") {
		t.Errorf("dsh 不支持 --public-url 时不应传入，实际: %s", joined)
	}
	if strings.Contains(joined, "token") {
		t.Errorf("启动命令里不应出现令牌: %s", joined)
	}
}

// TestDetectBinaryCarriesPinnedVersion 实例钉了版本又没装出可执行文件时，自动探测
// 出来的那条 npx 命令要带着版本——否则「填了版本但没点安装」的实例会静默跑 latest。
func TestDetectBinaryCarriesPinnedVersion(t *testing.T) {
	pinned, err := detectBinary("next")
	if err != nil {
		t.Skipf("本机没有可用的 npx: %v", err)
	}
	if !strings.HasSuffix(pinned, dshPackage+"@next") {
		t.Errorf("detectBinary(next) = %q，期望包名带上 @next", pinned)
	}
	plain, err := detectBinary("")
	if err != nil {
		t.Skipf("本机没有可用的 npx: %v", err)
	}
	if pinned == plain {
		t.Errorf("钉住与留空的探测结果不该一样: %q", pinned)
	}
}

// TestBuildCommandRejectsBadPinnedVersion 非法版本名在探测之前就拒绝：这一串会
// 拼进启动命令，而命令会被拆开直接执行。
func TestBuildCommandRejectsBadPinnedVersion(t *testing.T) {
	supervisor := New(Options{
		Host: "127.0.0.1", Port: 3081, Home: t.TempDir(), Workspace: t.TempDir(),
		DshVersion: "1.0.0 --registry=http://evil.example",
	})
	_, err := supervisor.buildCommand(t.Context())
	if err == nil || !strings.Contains(err.Error(), "不合法") {
		t.Fatalf("非法版本名应当被拒绝，实际: %v", err)
	}
}

func TestFilterSupportedDropsUnknownFlags(t *testing.T) {
	supervisor := New(Options{Home: t.TempDir()})
	supervisor.mu.Lock()
	supervisor.supported = map[string]bool{"--trusted-host": true}
	supervisor.mu.Unlock()

	got := supervisor.filterSupported(t.Context(), "dsh", []string{
		"--public-url", "http://x/",
		"--trusted-host", "1.2.3.4",
	})
	want := []string{"--trusted-host", "1.2.3.4"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("filterSupported = %q，期望 %q", got, want)
	}
}

func TestSupportedFlagsLearnsFromHelp(t *testing.T) {
	// 用一个假的 dsh 验证「问它自己」的解析逻辑：本机的 dsh 版本未必支持
	// --public-url，测试不能依赖它。
	fake := writeFakeDSH(t)
	supervisor := New(Options{Home: t.TempDir()})

	supported := supervisor.supportedFlags(t.Context(), fake)
	for _, want := range []string{"--host", "--port", "--no-open", "--trusted-host"} {
		if !supported[want] {
			t.Errorf("应当从帮助里学到 %s，实际学到 %v", want, supported)
		}
	}
	if supported["--public-url"] {
		t.Error("帮助里没有的选项不应被认成支持")
	}
}

func TestSupportedFlagsFallsBackConservatively(t *testing.T) {
	// 探测失败时按保守清单来：宁可少传，也不要因为一个陌生选项让 dsh 起不来。
	supervisor := New(Options{Home: t.TempDir()})
	supported := supervisor.supportedFlags(t.Context(), filepath.Join(t.TempDir(), "nonexistent-dsh"))

	for _, want := range []string{"--host", "--port", "--no-open", "--trusted-host"} {
		if !supported[want] {
			t.Errorf("探测失败时应当保留 %s，实际 %v", want, supported)
		}
	}
	if supported["--public-url"] {
		t.Error("探测失败时不应假定支持 --public-url")
	}
}

// writeFakeDSH 写一个只会打印帮助文本的假 dsh。
func writeFakeDSH(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-dsh")
	help := `Usage: dsh --profile web [options]

Options:
  --host <host>                  bind host
  --no-open                      do not open the Web UI in the default browser
  --port <port>                  listen port
  --trusted-host <authority...>  extra authority (repeatable)
  -h, --help                     show this help
`
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'EOF'\n"+help+"EOF\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectIdentifiesDSH(t *testing.T) {
	fake := dshtest.Start()
	defer fake.Close()

	state := Inspect(fake.Addr(), 2*time.Second)
	if !state.InUse {
		t.Fatal("应当探测到端口被占用")
	}
	if !state.DSH {
		t.Errorf("应当识别为 dsh，实际: %s", state.Detail)
	}
	if !strings.Contains(state.Detail, fake.Addr()) && !strings.Contains(state.Detail, "dsh") {
		t.Errorf("说明里应当含判断依据: %s", state.Detail)
	}
}

func TestInspectRejectsOtherService(t *testing.T) {
	// 一个普通的静态站点不是 dsh。误判会让网关把流量默默转发给不相干的服务，
	// 而用户看到的只是一堆莫名其妙的页面。
	plain := startPlainServer(t)
	defer plain.Close()

	state := Inspect(strings.TrimPrefix(plain.URL, "http://"), 2*time.Second)
	if !state.InUse {
		t.Fatal("应当探测到端口被占用")
	}
	if state.DSH {
		t.Errorf("普通网页服务不应被识别为 dsh: %s", state.Detail)
	}
	if !strings.Contains(state.Detail, "不是 dsh") {
		t.Errorf("说明里应当说清「不是 dsh」，实际: %s", state.Detail)
	}
}

// startPlainServer 起一个与 dsh 无关的普通 HTTP 服务。
func startPlainServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<!DOCTYPE html><html><body>hello</body></html>")
	}))
	t.Cleanup(server.Close)
	return server
}

func TestInspectFreePort(t *testing.T) {
	state := Inspect("127.0.0.1:1", 500*time.Millisecond)
	if state.InUse {
		t.Fatalf("端口 1 不应被判定为占用: %s", state.Detail)
	}
	if !strings.Contains(state.Detail, "空闲") {
		t.Errorf("说明应当写明端口空闲，实际: %s", state.Detail)
	}
}

func TestWaitPort(t *testing.T) {
	fake := dshtest.Start()
	defer fake.Close()

	done := make(chan error, 1)
	go func() { done <- WaitPort(t.Context(), fake.Addr(), 50*time.Millisecond) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("等待就绪失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待就绪超时")
	}
}

func TestNativeURL(t *testing.T) {
	// 没有令牌时不给出半截地址。
	supervisor := New(Options{Host: "127.0.0.1", Port: 3080})
	if got := supervisor.NativeURL(); got != "" {
		t.Errorf("令牌未知时 NativeURL 应为空，实际 %q", got)
	}

	supervisor.SetToken("abc-123_XYZ")
	want := "http://127.0.0.1:3080/?token=abc-123_XYZ"
	if got := supervisor.NativeURL(); got != want {
		t.Errorf("NativeURL = %q，期望 %q", got, want)
	}
}
