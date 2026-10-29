package term

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestReadPasswordMaskedNeedsTerminal 非终端输入不能走这条路：它的前提是能控制
// 终端回显，管道没有终端可控制。
func TestReadPasswordMaskedNeedsTerminal(t *testing.T) {
	var out bytes.Buffer
	if _, err := ReadPasswordMasked(bytes.NewReader([]byte("x\n")), &out, ">"); err == nil {
		t.Fatal("非终端输入应当返回错误")
	}
}

// TestMaskedEchoHelper 回显规则单独测：这段逻辑不依赖终端，可以按字节验证。
//
// 逐字符状态机是本功能的核心——星号、退格、回车、Ctrl-C——而真终端在测试里
// 拿不到，所以把规则抽出来测，避免只能靠人眼在终端里试。
func TestMaskedEchoHelper(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		wantValue  string
		wantEcho   string
		wantCancel bool
	}{
		{name: "普通输入", input: "sk-abc\n", wantValue: "sk-abc", wantEcho: "******\n"},
		{name: "退格改错", input: "sk-abX\x7f\n", wantValue: "sk-ab", wantEcho: "******\b \b\n"},
		{name: "退格在空行上无害", input: "\x7f\x7fsk\n", wantValue: "sk", wantEcho: "**\n"},
		{name: "回车结束（CR）", input: "abc\r", wantValue: "abc", wantEcho: "***\n"},
		{name: "Ctrl-C 取消", input: "ab\x03", wantCancel: true, wantEcho: "**\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var echo bytes.Buffer
			value, cancelled, err := feedMasked([]byte(tc.input), &echo)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if cancelled != tc.wantCancel {
				t.Fatalf("cancelled = %v，期望 %v", cancelled, tc.wantCancel)
			}
			if !tc.wantCancel && string(value) != tc.wantValue {
				t.Errorf("值 = %q，期望 %q", value, tc.wantValue)
			}
			if echo.String() != tc.wantEcho {
				t.Errorf("回显 = %q，期望 %q", echo.String(), tc.wantEcho)
			}
		})
	}
}

// TestMaskedNeverEchoesTheSecret 星号之外绝不能漏出原文。
func TestMaskedNeverEchoesTheSecret(t *testing.T) {
	const secret = "sk-super-secret-value"
	var echo bytes.Buffer
	if _, _, err := feedMasked([]byte(secret+"\n"), &echo); err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if bytes.Contains(echo.Bytes(), []byte(secret)) || bytes.Contains(echo.Bytes(), []byte("super")) {
		t.Errorf("回显里出现了原文: %q", echo.String())
	}
	if got := echo.String(); got != "*********************\n" {
		t.Errorf("回显应当全是星号（%d 个）加一个换行，实际 %q", len(secret), got)
	}
}

var errStop = errors.New("stop")

// TestMaskedCountsRunesNotBytes 锁定「按字符打星号」。
//
// 口令允许中文（users.ValidatePassword 按 rune 数校验「至少 8 个字符」），
// 但逐字节打星号会让 8 个汉字显示成 24 个星号——用户会以为自己敲多了，
// 退格改错时也只删掉一个字节，把 UTF-8 截成非法序列，存进去就再也登不上。
func TestMaskedCountsRunesNotBytes(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantVal  string
		wantStar int
	}{
		{name: "8 个汉字打 8 个星号", input: "中文口令测试八位\n", wantVal: "中文口令测试八位", wantStar: 8},
		{name: "英文不受影响", input: "abc12345\n", wantVal: "abc12345", wantStar: 8},
		{name: "汉字与英文混排", input: "abc中文\n", wantVal: "abc中文", wantStar: 5},
		{name: "emoji", input: "🔒🔑\n", wantVal: "🔒🔑", wantStar: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var echo bytes.Buffer
			value, cancelled, err := feedMasked([]byte(tc.input), &echo)
			if err != nil || cancelled {
				t.Fatalf("意外结果 err=%v cancelled=%v", err, cancelled)
			}
			if string(value) != tc.wantVal {
				t.Errorf("值 = %q，期望 %q", string(value), tc.wantVal)
			}
			if n := strings.Count(echo.String(), "*"); n != tc.wantStar {
				t.Errorf("星号数 = %d，期望 %d（每个字符一个星号）", n, tc.wantStar)
			}
		})
	}
}

// TestMaskedBackspaceDropsWholeRune 退格要删掉整个字符，不能只删一个字节。
func TestMaskedBackspaceDropsWholeRune(t *testing.T) {
	// 「中文」 + 一个退格 => 只剩「中」
	var echo bytes.Buffer
	value, cancelled, err := feedMasked([]byte("中文\x7f\n"), &echo)
	if err != nil || cancelled {
		t.Fatalf("意外结果 err=%v cancelled=%v", err, cancelled)
	}
	if string(value) != "中" {
		t.Fatalf("退格后应只剩「中」，实际 %q", string(value))
	}
	if !utf8.Valid(value) {
		t.Fatalf("退格后不是合法 UTF-8：% x", value)
	}
	// 两次退格把最后一个汉字也删掉，再输入新内容仍要合法。
	echo.Reset()
	value, _, _ = feedMasked([]byte("中文\x7f\x7f新\n"), &echo)
	if string(value) != "新" {
		t.Fatalf("两次退格后应为「新」，实际 %q", string(value))
	}
}

// TestMaskedBackspaceOnEmptyIsNoop 空行上按退格不能 panic，也不能回显乱码。
func TestMaskedBackspaceOnEmptyIsNoop(t *testing.T) {
	var echo bytes.Buffer
	value, _, err := feedMasked([]byte("\x7f\x7f\n"), &echo)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if len(value) != 0 {
		t.Fatalf("空行退格后应为空，实际 %q", value)
	}
}
