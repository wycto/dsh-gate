package main

import (
	"strings"
	"testing"
)

// TestQuoteForTaskScheduler 覆盖 Windows 计划任务那一段最易错的引号处理。
//
// 这段代码跑在 Windows 上，而开发与 CI 通常在 macOS/Linux，实机验证成本很高；
// 抽成不依赖平台的小函数就是为了能在任何机器上把它钉死。真出错的后果很具体：
// 路径含空格（Windows 上几乎必然）时任务创建成功但一启动就失败。
func TestQuoteForTaskScheduler(t *testing.T) {
	args := []string{
		`C:\Program Files\dsh-gate\dsh-gate.exe`,
		"run", "--log-json",
		"--dsh-home", `C:\Users\wei yi\.dsh`,
		"--port", "3082",
	}
	got := quoteForTaskScheduler(args)

	if want := `\"C:\Program Files\dsh-gate\dsh-gate.exe\"`; got[0] != want {
		t.Errorf("程序路径必须被转义引号包住：got %q want %q", got[0], want)
	}
	if want := `\"C:\Users\wei yi\.dsh\"`; got[4] != want {
		t.Errorf("含空格的参数值必须引起来：got %q want %q", got[4], want)
	}
	// 不含空格的参数保持原样：多余的引号会被当成值的一部分传下去。
	for _, i := range []int{1, 2, 3, 5, 6} {
		if got[i] != args[i] {
			t.Errorf("第 %d 项不该被改动：got %q want %q", i, got[i], args[i])
		}
	}
	if strings.ContainsAny(got[1], `"`) {
		t.Errorf("无空格参数不该加引号：%q", got[1])
	}
}
