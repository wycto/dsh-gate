package term

import (
	"bytes"
	"errors"
	"testing"
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
