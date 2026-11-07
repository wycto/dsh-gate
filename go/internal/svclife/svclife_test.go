package svclife

import (
	"strings"
	"testing"
	"time"
)

func newTestSup(t *testing.T) *Supervisor {
	t.Helper()
	return New(Options{StateDir: t.TempDir(), StartTimeout: 2 * time.Second})
}

func TestStartStatusLogsStop(t *testing.T) {
	s := newTestSup(t)
	spec := Spec{ID: "s1", Name: "demo", StartCmd: "echo hello; sleep 30"}
	if err := s.Start(spec); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	st := s.Status("s1")
	if !st.Running || st.PID == 0 || st.State != "running" {
		t.Fatalf("应运行中: %+v", st)
	}
	time.Sleep(200 * time.Millisecond)
	logs, err := s.Logs("s1", 50)
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	if !strings.Contains(logs, "hello") {
		t.Errorf("日志应含 hello，实际 %q", logs)
	}
	if err := s.Stop("s1"); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if got := s.Status("s1"); got.Running {
		t.Errorf("停止后不应运行: %+v", got)
	}
}

func TestStartIdempotentAndRestart(t *testing.T) {
	s := newTestSup(t)
	spec := Spec{ID: "s2", Name: "demo2", StartCmd: "sleep 30"}
	if err := s.Start(spec); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	pid1 := s.Status("s2").PID
	if err := s.Start(spec); err != nil {
		t.Fatalf("重复启动应幂等: %v", err)
	}
	if s.Status("s2").PID != pid1 {
		t.Error("重复启动不应换进程")
	}
	if err := s.Restart(spec); err != nil {
		t.Fatalf("重启失败: %v", err)
	}
	st := s.Status("s2")
	if !st.Running || st.PID == pid1 {
		t.Errorf("重启后应是新进程且运行中: %+v (old pid %d)", st, pid1)
	}
	_ = s.Stop("s2")
}

func TestStopAll(t *testing.T) {
	s := newTestSup(t)
	for _, id := range []string{"a", "b"} {
		if err := s.Start(Spec{ID: id, Name: id, StartCmd: "sleep 30"}); err != nil {
			t.Fatalf("启动 %s 失败: %v", id, err)
		}
	}
	s.StopAll()
	for _, id := range []string{"a", "b"} {
		if s.Status(id).Running {
			t.Errorf("%s 应已停止", id)
		}
	}
}

func TestUnknownStatusAndLogs(t *testing.T) {
	s := newTestSup(t)
	if got := s.Status("nope"); got.State != "stopped" || got.Running {
		t.Errorf("未知服务应 stopped: %+v", got)
	}
	logs, err := s.Logs("nope", 0)
	if err != nil || logs != "" {
		t.Errorf("未知服务日志应为空且无错: %q %v", logs, err)
	}
}
