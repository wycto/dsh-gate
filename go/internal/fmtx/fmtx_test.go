package fmtx_test

import (
	"testing"

	"github.com/wycto/dsh-gate/internal/fmtx"
)

func TestWidth(t *testing.T) {
	cases := map[string]int{
		"":         0,
		"abc":      3,
		"账号":       4,
		"dsh 端口":   3 + 1 + 4,
		"HTTPS":    5,
		"局域网访问":    10,
		"混合 mixed": 4 + 1 + 5,
	}
	for input, want := range cases {
		if got := fmtx.Width(input); got != want {
			t.Errorf("Width(%q) = %d，期望 %d", input, got, want)
		}
	}
}

func TestPad(t *testing.T) {
	// 对齐的目标是**显示宽度**，不是字符数：汉字两列，英文一列。
	if got := fmtx.Pad("账号", 12); fmtx.Width(got) != 12 {
		t.Errorf("Pad(账号, 12) 宽度 = %d，期望 12", fmtx.Width(got))
	}
	if got := fmtx.Pad("dsh 端口", 12); fmtx.Width(got) != 12 {
		t.Errorf("Pad(dsh 端口, 12) 宽度 = %d，期望 12", fmtx.Width(got))
	}
	// 超长时不截断：截断会丢掉信息，顶开整列更可接受。
	if got := fmtx.Pad("一个非常非常长的标签", 4); got != "一个非常非常长的标签" {
		t.Errorf("超长文本不应被截断，实际 %q", got)
	}
	if got := fmtx.Pad("", 3); got != "   " {
		t.Errorf("空串应补成 3 列，实际 %q", got)
	}
}

func TestPadLeft(t *testing.T) {
	if got := fmtx.PadLeft("7", 3); got != "  7" {
		t.Errorf("PadLeft = %q", got)
	}
}
