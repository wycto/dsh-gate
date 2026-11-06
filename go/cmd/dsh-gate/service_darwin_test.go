//go:build darwin

package main

import (
	"strings"
	"testing"
)

// TestPlistBooleansAreSelfClosing launchd 只认自闭合的布尔标签。
//
// 这个是**真机验证出来的**：Go 的 xml 编码器把 bool 写成 <true></true>，这种形式
// plutil -lint 判它合法，而 launchctl bootstrap 直接失败——
//
//	Bootstrap failed: 5: Input/output error
//
// （EIO 就是 launchd 说「这份 plist 我解析不了」的方式）。当时我正把
// <string>true</string> 改成「更正确的」真布尔，如果没有真的跑一次 launchctl，
// 这次改动会把 macOS 上的开机自启**全部弄坏**。
func TestPlistBooleansAreSelfClosing(t *testing.T) {
	text, err := serviceUnit(serviceOptions{Binary: "/usr/local/bin/dsh-gate", Overrides: map[string]string{"workspace": "/srv"}})
	if err != nil {
		t.Fatalf("生成 plist 失败: %v", err)
	}
	for _, bad := range []string{"<true></true>", "<false></false>"} {
		if strings.Contains(text, bad) {
			t.Errorf("plist 里出现了 launchd 不接受的非自闭合布尔 %s:\n%s", bad, text)
		}
	}
	for _, key := range []string{"KeepAlive", "RunAtLoad"} {
		idx := strings.Index(text, "<key>"+key+"</key>")
		if idx < 0 {
			t.Fatalf("plist 里没有 %s", key)
		}
		rest := text[idx:]
		if !strings.HasPrefix(strings.TrimSpace(rest[len("<key>"+key+"</key>"):]), "<true/>") {
			t.Errorf("%s 应当是自闭合的真布尔 <true/>，附近内容：%s", key, firstLine(rest))
		}
	}
}

// TestPlistHasNoStringBooleans 反向也钉住：布尔键不能退回 <string>true</string>。
func TestPlistHasNoStringBooleans(t *testing.T) {
	text, err := serviceUnit(serviceOptions{Binary: "/usr/local/bin/dsh-gate"})
	if err != nil {
		t.Fatalf("生成 plist 失败: %v", err)
	}
	if strings.Contains(text, "<string>true</string>") {
		t.Errorf("布尔键被写成了字符串:\n%s", text)
	}
}

// firstLine 取一小段上下文，便于测试失败时定位。
func firstLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > 2 {
		lines = lines[:2]
	}
	return strings.Join(lines, " | ")
}

// TestServiceUnitIncludesExtraPath 断言 --extra-path 的目录进了 plist 的 PATH。
//
// 回归的是「服务起来了、实例起不来」：服务管理器的 PATH 只有系统目录，而实例的
// 启动命令（例如 pnpm）常装在 ~/.local/node/bin 这类用户目录里。自备运行时不在
// 那几条固定目录中，没有这条就找不到命令——现象是管理端正常、每个实例都启动失败，
// 而且报错只说「找不到 pnpm」，与「服务配错了」看不出关系。
func TestServiceUnitIncludesExtraPath(t *testing.T) {
	const extra = "/Users/demo/.local/node/bin"
	text, err := serviceUnit(serviceOptions{
		Binary:    "/Users/demo/.local/bin/dsh-gate",
		ExtraPath: extra,
		Overrides: map[string]string{"workspace": "/srv"},
	})
	if err != nil {
		t.Fatalf("生成 plist 失败: %v", err)
	}

	idx := strings.Index(text, "<key>PATH</key>")
	if idx < 0 {
		t.Fatalf("plist 里没有 PATH:\n%s", text)
	}
	rest := text[idx:]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		t.Fatalf("PATH 没有闭合:\n%s", text)
	}
	value := rest[strings.Index(rest, "<string>")+len("<string>") : end]

	// 必须在最前面：用户明确给的目录优先于系统目录（例如要让自己的 node 压过
	// /usr/local/bin 里的旧版本）。
	if !strings.HasPrefix(value, extra+":") {
		t.Errorf("--extra-path 应当排在 PATH 最前面，实际 %q", value)
	}
	// 空段会让 PATH 里出现「当前目录」，那是安全风险，不能因为拼装留下空元素。
	for _, dir := range strings.Split(value, ":") {
		if strings.TrimSpace(dir) == "" {
			t.Errorf("PATH 里有空段（等价于当前目录）: %q", value)
		}
	}

	// 没有 --extra-path 时不能凭空多出空段。
	plain, err := serviceUnit(serviceOptions{Binary: "/usr/local/bin/dsh-gate"})
	if err != nil {
		t.Fatalf("生成 plist 失败: %v", err)
	}
	if strings.Contains(plain, "::") || strings.Contains(plain, ":<") {
		t.Errorf("没有 --extra-path 时 PATH 不该有多余分隔符:\n%s", plain)
	}
}

// TestExtraPathDirs 查拆分的边角：空串、空段、首尾分隔符都要被丢掉。
func TestExtraPathDirs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{":", nil},
		{"/a", []string{"/a"}},
		{"/a:/b", []string{"/a", "/b"}},
		{" /a : /b ", []string{"/a", "/b"}},
		{"/a::/b", []string{"/a", "/b"}},
		{" /a :", []string{"/a"}},
	}
	for _, c := range cases {
		got := extraPathDirs(c.in)
		if len(got) != len(c.want) {
			t.Errorf("extraPathDirs(%q) = %v，期望 %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("extraPathDirs(%q) = %v，期望 %v", c.in, got, c.want)
				break
			}
		}
	}
}
