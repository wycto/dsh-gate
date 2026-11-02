package trust

import (
	"strings"
	"testing"
)

// TestManualCommandPerPlatform 验证每个平台的手动命令里都带上了证书路径，
// 且路径被正确引号包裹——路径里有空格时，没引号的命令会当场失败。
func TestManualCommandPerPlatform(t *testing.T) {
	const ca = "/tmp/my certs/ca.crt"
	cases := []struct {
		goos string
		want string
	}{
		{"darwin", "security add-trusted-cert"},
		{"windows", "certutil -addstore"},
		{"linux", "update-ca-certificates"},
	}
	for _, c := range cases {
		got := ManualCommand(c.goos, ca)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s 的命令应包含 %q，实际: %s", c.goos, c.want, got)
		}
		// 路径必须被引起来：含空格的路径不加引号会被 shell 拆成两个参数。
		if !strings.Contains(got, "'/tmp/my certs/ca.crt'") {
			t.Errorf("%s 的命令没有正确引用路径，实际: %s", c.goos, got)
		}
	}
}

// TestShellQuote 验证引号处理对单引号也安全。
//
// 单引号是 POSIX shell 里唯一能阻止所有展开的引号，但路径里若有单引号，
// 就必须用 '"'"' 这种写法闭合再续开，否则命令会被截断。
func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/plain/path", "'/plain/path'"},
		{"/with space/ca.crt", "'/with space/ca.crt'"},
		{"/with'quote/ca.crt", `'/with'\''quote/ca.crt'`},
		{"/with$dollar/ca.crt", "'/with$dollar/ca.crt'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestInstallRejectsEmptyPath 验证没有 CA 路径时不会去执行任何命令。
func TestInstallRejectsEmptyPath(t *testing.T) {
	status := Install(Options{CACert: "  "})
	if status.Attempted {
		t.Fatal("没有 CA 路径时不该尝试安装")
	}
	if status.Trusted {
		t.Fatal("没有 CA 路径时不该报告成功")
	}
	if strings.TrimSpace(status.Message) == "" {
		t.Fatal("应当说明为什么没做")
	}
}

// TestBrowserHintMentionsRestart 验证提示里一定要有「重启浏览器」。
//
// 这是最容易漏掉的一步：证书装进信任库后，已经打开的浏览器不会重新读取，
// 用户会以为装失败了。
func TestBrowserHintMentionsRestart(t *testing.T) {
	hint := BrowserHint()
	if !strings.Contains(hint, "重启") {
		t.Fatalf("提示里应包含「重启浏览器」，实际: %s", hint)
	}
}
