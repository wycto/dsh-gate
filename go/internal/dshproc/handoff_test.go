package dshproc

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestSetResolvedCommandTakesEffectAfterStart 锁住「首次启动的准备工作放在后台」
// 这条路径：网关启动时命令还没准备好（要先下载 Node.js、再由 npx 拉 dsh），
// 备好之后用 SetResolvedCommand 交进来，下一次拉起就必须用它——不必重建
// supervisor，更不必重启网关。
func TestSetResolvedCommandTakesEffectAfterStart(t *testing.T) {
	// PATH 指向空目录，模拟「本机没有 npx」；否则开发机上装着 npx，
	// 盲探测会成功，这个用例就测不到「命令还没准备好」的状态。
	t.Setenv("PATH", t.TempDir())

	supervisor := New(Options{
		Host:   "127.0.0.1",
		Port:   1,
		Home:   t.TempDir(),
		Logger: slog.New(slog.DiscardHandler),
	})
	if _, err := supervisor.buildCommand(context.Background()); err == nil {
		t.Fatal("既没有命令、PATH 里也没有 npx 时应当报错")
	}

	fake := writeFakeDSH(t)
	supervisor.SetResolvedCommand(Resolution{Command: quoteCommandArg(fake) + " web"})

	args, err := supervisor.buildCommand(context.Background())
	if err != nil {
		t.Fatalf("交进命令后仍然解析失败: %v", err)
	}
	if args[0] != fake {
		t.Errorf("可执行文件 = %q，期望 %q", args[0], fake)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "web") || !strings.Contains(joined, "--port 1") {
		t.Errorf("拼出来的命令不对: %s", joined)
	}

	// 交空命令不该把已经就绪的命令抹掉：后台准备可能重试，失败的那次不能
	// 把上一次成功的状态毁掉。
	supervisor.SetResolvedCommand(Resolution{Command: "   "})
	args, err = supervisor.buildCommand(context.Background())
	if err != nil {
		t.Fatalf("空命令把已有命令清掉了: %v", err)
	}
	if args[0] != fake {
		t.Errorf("空命令之后可执行文件 = %q，期望仍是 %q", args[0], fake)
	}

	// 用 --dsh-cmd 显式指定的实例不受影响：那是用户自己的决定。
	explicit := New(Options{
		Command: "my-dsh {port}",
		Host:    "127.0.0.1",
		Port:    2,
		Home:    t.TempDir(),
		Logger:  slog.New(slog.DiscardHandler),
	})
	explicit.SetResolvedCommand(Resolution{Command: fake + " web"})
	args, err = explicit.buildCommand(context.Background())
	if err != nil {
		t.Fatalf("显式命令解析失败: %v", err)
	}
	if !strings.Contains(strings.Join(args, " "), "my-dsh") {
		t.Errorf("显式 --dsh-cmd 被后台交进来的命令覆盖了: %v", args)
	}
}
