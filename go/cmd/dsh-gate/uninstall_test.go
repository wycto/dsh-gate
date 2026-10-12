package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/registry"
)

// TestUninstallTargetsDedupesNested 运行登记默认就在状态目录里，
// 不该被「删两次」——重复的 RemoveAll 只会让人怀疑删错了东西。
func TestUninstallTargetsDedupesNested(t *testing.T) {
	cases := []struct {
		name     string
		stateDir string
		runDir   string
		want     []string
	}{
		{
			name:     "登记在状态目录内",
			stateDir: "/srv/dsh-gate",
			runDir:   "/srv/dsh-gate/run",
			want:     []string{"/srv/dsh-gate"},
		},
		{
			name:     "两者分开",
			stateDir: "/srv/dsh-gate",
			runDir:   "/home/u/.dsh-gate/run",
			want:     []string{"/srv/dsh-gate", "/home/u/.dsh-gate/run"},
		},
		{
			name:     "指向同一处",
			stateDir: "/srv/dsh-gate",
			runDir:   "/srv/dsh-gate",
			want:     []string{"/srv/dsh-gate"},
		},
		{
			name:     "状态目录缺失时只删登记",
			stateDir: "",
			runDir:   "/home/u/.dsh-gate/run",
			want:     []string{"/home/u/.dsh-gate/run"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uninstallTargets(tc.stateDir, tc.runDir)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("uninstallTargets = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestUninstallRemoveAllGuardsDangerousPaths 锁住安全阀：--purge 的路径来自
// 命令行，写错一个字符就可能是家目录或根，宁可不删也不能删错。
func TestUninstallRemoveAllGuardsDangerousPaths(t *testing.T) {
	for _, path := range []string{"/", ".", "", string(filepath.Separator)} {
		if err := uninstallRemoveAll(path); err == nil {
			t.Errorf("uninstallRemoveAll(%q) 没有拒绝，期望报错", path)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if err := uninstallRemoveAll(home); err == nil {
			t.Errorf("uninstallRemoveAll(家目录) 没有拒绝，期望报错")
		}
	}

	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}
	if err := uninstallRemoveAll(dir); err != nil {
		t.Fatalf("删除普通状态目录失败: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("目录仍然存在: %v", err)
	}
}

// TestUninstallStopsInstancesOnlyTouchesNamedOnes 卸载只动点名的实例：
// 残留登记（进程早没了）要清掉，而且「本来就没在跑」不算失败；没点名的
// 实例连登记文件都不该碰——多实例机器上这条就是「别把别人的收走」。
func TestUninstallStopsInstancesOnlyTouchesNamedOnes(t *testing.T) {
	dir := registry.Dir(t.TempDir())
	// 端口 1 不会有网关在听，Alive() 因此为 false，正好模拟残留登记。
	if err := dir.Save(registry.Record{Name: "ghost", Port: 1, Listen: "0.0.0.0:1"}); err != nil {
		t.Fatalf("写登记失败: %v", err)
	}
	if err := dir.Save(registry.Record{Name: "keep", Port: 2, Listen: "0.0.0.0:2"}); err != nil {
		t.Fatalf("写登记失败: %v", err)
	}

	picked, err := dir.Find([]string{"ghost"})
	if err != nil {
		t.Fatalf("按名字取登记失败: %v", err)
	}
	if failed := uninstallStopsInstances(dir, picked, time.Second); failed {
		t.Error("清理残留登记被当成失败")
	}

	records, problems := dir.List()
	if len(problems) != 0 {
		t.Fatalf("读取登记报错: %v", problems)
	}
	if len(records) != 1 || records[0].Name != "keep" {
		t.Errorf("只该留下没点名的 keep，实际 %v", records)
	}
}

// TestOtherAliveInstancesBlocksPurge 还有别人在跑时，删数据这一步必须被拦下：
// 登记目录是机器级共享的，删掉它，那些实例就丢了身份、再也管不着了。
func TestOtherAliveInstancesBlocksPurge(t *testing.T) {
	dir := registry.Dir(t.TempDir())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("准备监听失败: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	if err := dir.Save(registry.Record{Name: "other", Port: port, Listen: listener.Addr().String()}); err != nil {
		t.Fatalf("写登记失败: %v", err)
	}
	if err := dir.Save(registry.Record{Name: "target", Port: 1, Listen: "0.0.0.0:1"}); err != nil {
		t.Fatalf("写登记失败: %v", err)
	}

	blockers := otherAliveInstances(dir, []registry.Record{{Name: "target"}})
	if len(blockers) != 1 || blockers[0].Name != "other" {
		t.Fatalf("应当只把还在跑的 other 列为阻碍，实际 %v", blockers)
	}
	if names := recordNames(blockers); strings.Join(names, ",") != "other" {
		t.Errorf("recordNames = %v", names)
	}
}

// TestUninstallRequiresTarget 不写目标时必须是空操作：列出在跑的实例、返回 2。
// 「默认动全部」在多实例机器上一次手抖就能把别人正在用的实例收走。
func TestUninstallRequiresTarget(t *testing.T) {
	code := run([]string{"uninstall", "--run-dir", t.TempDir()})
	if code != 2 {
		t.Errorf("不写目标时退出码 = %d，期望 2（用法错误）", code)
	}
}

// TestUninstallUnknownTargetFails 点名了不存在的实例要当场报错，而不是默默成功。
func TestUninstallUnknownTargetFails(t *testing.T) {
	code := run([]string{"uninstall", "nope", "--run-dir", t.TempDir()})
	if code != 1 {
		t.Errorf("点名不存在的实例时退出码 = %d，期望 1", code)
	}
}

// TestDecidePurgeDefaultsToKeep 锁住「默认不删数据」：账号表删掉不可恢复，
// 非交互场景里没有回答的机会，更不能替用户猜。
//
// 这里只测决策，不测整条 uninstall 命令：命令会去动真实的服务管理器
// （macOS 的 launchctl bootout、Linux 的 systemctl），在开发机上跑测试时
// 那是有副作用的操作。
func TestDecidePurgeDefaultsToKeep(t *testing.T) {
	for _, tc := range []struct {
		name        string
		purge       bool
		interactive bool
		want        bool
	}{
		{name: "没给选项且非交互", purge: false, interactive: false, want: false},
		{name: "显式 --purge", purge: true, interactive: false, want: true},
		{name: "显式 --purge 且交互", purge: true, interactive: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decidePurge(tc.purge, tc.interactive, "/srv/dsh-gate", []string{"/srv/dsh-gate"})
			if err != nil {
				t.Fatalf("decidePurge 报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("decidePurge = %v，期望 %v", got, tc.want)
			}
		})
	}
}
