package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/registry"
)

// TestWithName 验证重启时实例名会被对齐到登记里的名字。
//
// 为什么需要这一步：用 `--config` 启动时，一次登记的是整份配置。若重启时不
// 把名字钉回去，被拉起的进程可能因为「实例名冲突」而拒绝启动，而 restart
// 只会报一句「端口没就绪」，很难往这儿想。
func TestWithName(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{
			// 本来就没有 --name：不能补，补了会与 --config 打架。
			name: "原本没有名字时不追加",
			argv: []string{"/usr/local/bin/dsh-gate", "run", "--port", "8080"},
			want: "",
		},
		{
			name: "空格式带值",
			argv: []string{"/usr/local/bin/dsh-gate", "run", "--name", "work", "--port", "8080"},
			want: "--name personal",
		},
		{
			name: "等号写法",
			argv: []string{"/usr/local/bin/dsh-gate", "run", "--name=work"},
			want: "--name=personal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withName(tc.argv, "personal")
			joined := strings.Join(got, " ")
			if tc.want == "" {
				// 期望「原样返回」：逐项比对，确保没有偷偷追加或丢东西。
				if len(got) != len(tc.argv) {
					t.Fatalf("withName 不该改变参数个数：%q", got)
				}
			} else if !strings.Contains(joined, tc.want) {
				t.Errorf("withName = %q，期望包含 %q", joined, tc.want)
			}
			// 第一个元素（程序名）必须原样保留：它决定执行哪个二进制。
			if got[0] != tc.argv[0] {
				t.Errorf("程序名被改动了: %q", got[0])
			}
			// 旧的实例名不能残留，否则会出现两个 --name。
			if strings.Contains(joined, "work") {
				t.Errorf("旧实例名没有替换干净: %q", joined)
			}
		})
	}
}

func TestHumanSince(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{5 * time.Second, "5 秒"},
		{3 * time.Minute, "3 分钟"},
		{2*time.Hour + 10*time.Minute, "2 小时 10 分钟"},
		{50 * time.Hour, "2 天 2 小时"},
	}
	for _, tc := range cases {
		if got := humanSince(time.Now().Add(-tc.age)); got != tc.want {
			t.Errorf("humanSince(-%s) = %q，期望 %q", tc.age, got, tc.want)
		}
	}
	if got := humanSince(time.Time{}); got != "未知" {
		t.Errorf("零值时间应显示未知，实际 %q", got)
	}
}

// TestAwaitStopped 锁定「等旧实例真的消失」这条规则的两个边界。
//
// 现场踩过的坑：restart 只看网关端口关没关就拉新网关，而旧网关的 dsh 还在收尾，
// 于是新网关把一个正在死的 dsh「接管」过来；接管的实例不由它看守，dsh 一死就
// 再没人拉起，页面永远停在「连接被拒绝」。
func TestAwaitStopped(t *testing.T) {
	t.Run("进程已经不在时立刻返回", func(t *testing.T) {
		record := registry.Record{PID: 1 << 30}
		start := time.Now()
		if err := awaitStopped(record, 5*time.Second); err != nil {
			t.Fatalf("旧进程已经退出时不该报错：%v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("旧进程已经退出时应当立刻返回，实际等了 %s", elapsed)
		}
	})

	t.Run("进程还活着时等到超时并报错", func(t *testing.T) {
		record := registry.Record{PID: os.Getpid()}
		err := awaitStopped(record, 300*time.Millisecond)
		if err == nil {
			t.Fatal("旧进程一直不退时应当报错，而不是照常重启")
		}
		// 报错要带上进程号，用户才知道该去看谁。
		if !strings.Contains(err.Error(), "进程号") {
			t.Errorf("错误里应当点明进程号：%v", err)
		}
	})

	t.Run("没有登记 dsh 端口时不额外等待", func(t *testing.T) {
		record := registry.Record{PID: 1 << 30}
		start := time.Now()
		if err := awaitStopped(record, 5*time.Second); err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("没有 dsh 端口可等，不该耗时间，实际 %s", elapsed)
		}
	})
}

// 曾经允许「不写实例名 = 全部」，代价是一次手抖就能把包括公司那套正在用的实例
// 在内的一切都停掉。现在 nil（取全部）只有出现 all 关键字时才成立。
// TestTargetsRequiresExplicitAll 锁定「全部」必须是写出来的意图。
//
// 曾经允许「不写实例名 = 全部」，代价是一次手抖就能把包括公司那套正在用的实例
// 在内的一切都停掉。现在 nil（取全部）只有出现 all 关键字时才成立。
func TestTargetsRequiresExplicitAll(t *testing.T) {
	cases := []struct {
		names []string
		want  []string // nil 表示「全部」
	}{
		{[]string{"personal"}, []string{"personal"}},
		{[]string{"personal", "work"}, []string{"personal", "work"}},
		{[]string{"all"}, nil},
		// 混着写也按「全部」理解，不必为此报错。
		{[]string{"all", "personal"}, nil},
		{[]string{"personal", "all"}, nil},
		{[]string{"ALL"}, []string{"ALL"}}, // 大写不是关键字，当作不存在的实例名
	}
	for _, tc := range cases {
		got := targets(tc.names)
		if len(got) != len(tc.want) {
			t.Errorf("targets(%v) = %v，期望 %v", tc.names, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("targets(%v) = %v，期望 %v", tc.names, got, tc.want)
				break
			}
		}
	}
}
