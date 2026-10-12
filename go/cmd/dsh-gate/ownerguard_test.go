package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestRootWriteGuard 锁定「sudo 往用户目录里写」这条路的拦截规则。
//
// 现场踩过的坑：`sudo dsh-gate setup` 在 macOS 上保留 $HOME，于是 settings.yaml
// 与 .credentials.yaml 被写成了 root 属主（0600），DSH 桌面端从此读不了自己的
// 配置，只能手工 chown 恢复。这个守卫要保证那种调用在任何写入之前就被拒绝。
func TestRootWriteGuard(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("以 root 跑测试时无法模拟 sudo 场景")
	}
	me := strconv.Itoa(os.Getuid())
	other := strconv.Itoa(os.Getuid() + 12345)

	cases := []struct {
		name    string
		guard   rootWriteGuard
		label   string
		dir     string
		wantErr bool
	}{
		{
			name:  "非 root 直接放行",
			guard: rootWriteGuard{EUID: os.Getuid(), InvokingUID: me, InvokingName: "me"},
			dir:   t.TempDir(),
		},
		{
			name:    "sudo 写自己的家目录目录被拦",
			guard:   rootWriteGuard{EUID: 0, InvokingUID: me, InvokingName: "me"},
			dir:     t.TempDir(),
			wantErr: true,
		},
		{
			name:    "还没建出来的子目录也拦（看最近的已存在祖先）",
			guard:   rootWriteGuard{EUID: 0, InvokingUID: me, InvokingName: "me"},
			dir:     filepath.Join(t.TempDir(), "not-yet", "deeper"),
			wantErr: true,
		},
		{
			name:  "目录属于别人时不是这条守卫的事",
			guard: rootWriteGuard{EUID: 0, InvokingUID: other, InvokingName: "other"},
			dir:   t.TempDir(),
		},
		{
			name:  "没有 SUDO_UID：真的是 root 在跑，放行",
			guard: rootWriteGuard{EUID: 0},
			dir:   t.TempDir(),
		},
		{
			name:  "SUDO_UID=0：sudo 到了 root 自己的家目录，放行",
			guard: rootWriteGuard{EUID: 0, InvokingUID: "0"},
			dir:   t.TempDir(),
		},
		{
			name:  "空路径不拦",
			guard: rootWriteGuard{EUID: 0, InvokingUID: me, InvokingName: "me"},
			dir:   "",
		},
		{
			name:  "--allow-root-write 明确表达了知情",
			guard: rootWriteGuard{EUID: 0, InvokingUID: me, InvokingName: "me", Allowed: true},
			dir:   t.TempDir(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.guard.check("dsh 实例目录", tc.dir)
			if tc.wantErr && err == nil {
				t.Fatalf("期望被拦下，实际放行（dir=%s）", tc.dir)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不该拦下，实际报错：%v", err)
			}
			if !tc.wantErr {
				return
			}
			// 错误必须能让人照着改：说清是谁的目录、为什么危险、怎么办。
			for _, want := range []string{"root", "sudo", "--allow-root-write", "chown", tc.dir} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息里缺少 %q：\n%v", want, err)
				}
			}
		})
	}
}

// TestProcessAliveSelf 验证进程存活探测的最基本两态：自己一定在，离谱的 pid 一定不在。
func TestProcessAliveSelf(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("当前进程应当被判为存活")
	}
	if processAlive(1 << 30) {
		t.Error("不存在的进程号不应被判为存活")
	}
	if processAlive(0) || processAlive(-1) {
		t.Error("非法进程号必须返回 false")
	}
}
