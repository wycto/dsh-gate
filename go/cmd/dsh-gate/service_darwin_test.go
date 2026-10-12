//go:build darwin

package main

import (
	"os/user"
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

// TestLaunchingUserUnderSudo 用 sudo 跑时，自启要装给**发起 sudo 的用户**。
//
// 这是用户实际踩到的坑：他按我们自己的提示 `sudo dsh-gate setup`（为了把命令装进
// /usr/local/bin），结果 euid 变成 0，launchd domain 成了不存在的 gui/0，bootstrap
// 报 `125: Domain does not support specified action`；就算侥幸装上，plist 也会写进
// root 的家目录，登录用户的自启里根本看不到。
func TestLaunchingUserUnderSudo(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("拿不到当前用户")
	}

	// 核心场景：sudo 跑（euid=0）但 SUDO_UID 指向登录用户 → domain 必须是 gui/501
	// 这种真实存在的 domain，而不是 gui/0。
	uid, home, elevated := resolveLaunchingUser(0, current.Uid, current.Username)
	if uid != current.Uid {
		t.Errorf("sudo 下 uid = %q，期望发起 sudo 的用户 %q（否则 domain 会是不存在的 gui/0）", uid, current.Uid)
	}
	if home != current.HomeDir {
		t.Errorf("sudo 下家目录 = %q，期望 %q（plist 要写进登录用户的家目录）", home, current.HomeDir)
	}
	if !elevated {
		t.Error("sudo 下应当报告为 elevated，好给出提示")
	}

	// root 且没有 SUDO_UID：只能给 uid 0，由调用方拦住并说明。
	uid, _, elevated = resolveLaunchingUser(0, "", "")
	if uid != "0" || !elevated {
		t.Errorf("root 无 SUDO_UID 时 = %q/%v，期望 0/true", uid, elevated)
	}

	// 普通用户：用自己的 uid 与家目录，且不报 elevated。
	uid, home, elevated = resolveLaunchingUser(501, current.Uid, current.Username)
	if uid != "501" || elevated {
		t.Errorf("普通用户 = %q/%v，期望 501/false", uid, elevated)
	}
	if home == "" {
		t.Error("普通用户的家目录不该为空")
	}

	// SUDO_UID 带空格也要能用（有些环境的 sudo 配置会带进来）。
	if uid, _, _ := resolveLaunchingUser(0, " "+current.Uid+" ", current.Username); uid != current.Uid {
		t.Errorf("SUDO_UID 带空格时应被清理，实际 %q", uid)
	}
}

// TestHomeOfUserFallsBack 查不到用户时退回当前进程的家目录，而不是空字符串
// （空会被拼成相对路径，plist 就写到奇怪的地方去了）。
func TestHomeOfUserFallsBack(t *testing.T) {
	if got := homeOfUser("这个用户肯定不存在-zzz"); got == "" {
		t.Error("查不到用户时应当退回当前家目录，而不是空")
	}
	if got := homeOfUser(""); got == "" {
		t.Error("空用户名时应当退回当前家目录")
	}
}
