package dshproc

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestReapBackoffBoundsInstantCrash 锁定崩溃重启的退避节奏。
//
// 曾经的缺陷：每次 spawn 都另起一个 reap goroutine，「秒退」的子进程（Node
// 版本过旧导致 dsh 起不来）会让新旧重启循环并存，各自的退避都从 1 秒重新
// 数起——四分钟里重试了四千多次，把容器拖出一千多个进程。修复后重启循环
// 全局唯一，退避跨代累计（1s→2s→4s…封顶 30s），这里用计数桩验证节奏。
func TestReapBackoffBoundsInstantCrash(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	bin := t.TempDir()
	counter := filepath.Join(bin, "count")
	// --help 时输出选项清单（探测一次后被缓存），真正运行则计数后立刻退出，
	// 模拟「Node 版本过旧导致 dsh 秒退」。
	writeExec(t, bin, "stub-dsh", "if [ \"$2\" = \"--help\" ]; then echo --host; echo --port; echo --no-open; exit 0; fi\n"+
		"echo x >> "+counter+"\nexit 1\n")

	s := New(Options{
		Host:         "127.0.0.1",
		Port:         0,
		Home:         t.TempDir(),
		Workspace:    t.TempDir(),
		Restart:      true,
		Command:      filepath.Join(bin, "stub-dsh"),
		StartTimeout: 300 * time.Millisecond,
	})

	_, waitCh, exited, err := s.spawn([]string{filepath.Join(bin, "stub-dsh"), "web"})
	if err != nil {
		t.Fatalf("首次拉起失败: %v", err)
	}
	go s.reap(waitCh, exited, time.Second)

	time.Sleep(5 * time.Second)
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()

	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("读取计数失败: %v", err)
	}
	runs := strings.Count(string(raw), "x")
	// 5 秒内的期望节奏：t=0 首启，随后 1s、3s 两次重启（下次在 7s 之后）。
	// 若退避失效（回归为并发循环），5 秒内会是两位数。
	if runs < 2 || runs > 4 {
		t.Fatalf("5 秒内子进程运行了 %d 次，退避应把它压在 2~4 次", runs)
	}
}

// TestDiagnoseCrashNodeVersion 单测崩溃诊断：旧 Node 的崩溃特征要给出
// 升级指引，新 Node 的崩溃不能被冤枉成版本问题。
func TestDiagnoseCrashNodeVersion(t *testing.T) {
	build := func() (*Supervisor, *bytes.Buffer) {
		buf := &bytes.Buffer{}
		s := New(Options{Host: "127.0.0.1", Port: 0})
		s.log = slog.New(slog.NewTextHandler(buf, nil))
		return s, buf
	}

	s, buf := build()
	s.recent = []string{"SyntaxError: The requested module 'node:util' does not provide an export named 'parseEnv'", "Node.js v18.20.4"}
	s.diagnoseCrash()
	if !strings.Contains(buf.String(), "Node.js v18.20") || !strings.Contains(buf.String(), "升级 Node.js") {
		t.Fatalf("崩溃横幅应给出 Node 版本与升级指引，实际日志: %s", buf.String())
	}

	// 崩溃横幅只证明「崩了」，不能断言一定是版本问题：别的错误也该照实记录。
	s, buf = build()
	s.recent = []string{"某些别的错误", "Node.js v20.19.0"}
	s.diagnoseCrash()
	if strings.Contains(buf.String(), "说明这版 dsh 与当前 Node.js 不匹配") {
		t.Fatalf("非版本类崩溃不该被断言成运行时不匹配，实际日志: %s", buf.String())
	}
}
