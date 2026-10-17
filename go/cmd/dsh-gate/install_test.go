package main

import (
	"flag"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/portal"
)

// TestInstallCreatesAdminAndKeepsExisting 验证 install 的账号语义：
// 空库里建出管理员；重复执行不动已有账号。
func TestInstallCreatesAdminAndKeepsExisting(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "portal.db")

	// 第一次：非交互 + --random，应当建出 admin 且角色是 admin。
	withStdin(t, "")
	out, code := captureStdout(t, func() int {
		return run([]string{"install", "--state-dir", dir, "--bin-dir", "", "--random"})
	})
	if code != 0 {
		t.Fatalf("install 应成功，退出码 %d，输出：%s", code, out)
	}
	if !strings.Contains(out, "admin") {
		t.Fatalf("输出里应出现管理员名，实际：%s", out)
	}
	// 不带 --start 时必须明说「还没启动」，不能让人以为服务已经在跑。
	if !strings.Contains(out, "manage run --background") {
		t.Fatalf("应提示如何启动管理端，实际：%s", out)
	}

	store := openStore(t, db)
	user, err := store.UserByName("admin")
	if err != nil {
		t.Fatalf("admin 应已创建: %v", err)
	}
	if user.Role != portal.RoleAdmin {
		t.Fatalf("install 建的应是管理员，实际角色 %q", user.Role)
	}
	_ = store.Close()

	// 第二次：库非空，口令选项被忽略、不新建账号。
	out2, code2 := captureStdout(t, func() int {
		return run([]string{"install", "--state-dir", dir, "--bin-dir", "", "--random"})
	})
	if code2 != 0 {
		t.Fatalf("重复 install 应成功，退出码 %d，输出：%s", code2, out2)
	}
	store2 := openStore(t, db)
	count, err := store2.CountUsers()
	if err != nil {
		t.Fatalf("数账号失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("重复执行不应新增账号，实际 %d 个", count)
	}
	_ = store2.Close()
}

// TestInstallRefusesUnknownFlags 验证 install 不接受实例选项——那些属于
// portal instance add，混进来只会配到一个谁也不看的角落。
func TestInstallRefusesUnknownFlags(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "")
	_, code := captureStdout(t, func() int {
		return run([]string{"install", "--state-dir", dir, "--bin-dir", "", "--random", "--dsh-home", dir})
	})
	if code != 2 {
		t.Fatalf("传了不属于 install 的选项应报用法错误（退出码 2），实际 %d", code)
	}
}

// TestInstallRejectsBadPort 验证端口范围当场校验。
func TestInstallRejectsBadPort(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "")
	for _, port := range []string{"0", "70000", "-1"} {
		_, code := captureStdout(t, func() int {
			return run([]string{"install", "--state-dir", dir, "--bin-dir", "", "--random", "--port", port})
		})
		if code != 2 {
			t.Fatalf("端口 %s 应被拒（退出码 2），实际 %d", port, code)
		}
	}
}

// TestInstallNeedsPasswordSource 验证非交互且没给口令来源时提前失败。
func TestInstallNeedsPasswordSource(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "")
	_, code := captureStdout(t, func() int {
		return run([]string{"install", "--state-dir", dir, "--bin-dir", ""})
	})
	if code != 2 {
		t.Fatalf("没有口令来源时应报用法错误（退出码 2），实际 %d", code)
	}
}

// TestPortalDBPathFollowsStateDir 验证管理端数据库的推导与 portal run 一致。
func TestPortalDBPathFollowsStateDir(t *testing.T) {
	got, err := portalDBPath("")
	if err != nil {
		t.Fatalf("默认推导失败: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("取家目录失败: %v", err)
	}
	if want := filepath.Join(home, ".dsh-gate", "portal.db"); got != want {
		t.Fatalf("默认管理端库应为 %s，实际 %s", want, got)
	}
	if got, err := portalDBPath("/tmp/elsewhere"); err != nil || got != "/tmp/elsewhere/portal.db" {
		t.Fatalf("指定 state-dir 时应推导出其中的 portal.db，实际 %q %v", got, err)
	}
}

// TestCheckPortalAccounts 验证后台化前的账号自检：空库报错、非空库放行。
func TestCheckPortalAccounts(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "portal.db")

	if err := checkPortalAccounts(db, dir); err == nil {
		t.Fatal("空管理端库应被自检拒绝（管理端是 fail closed 的）")
	}

	store, err := portal.Open(db)
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	if _, err := store.CreateUser("root", "root-password-123", portal.RoleAdmin); err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	_ = store.Close()

	if err := checkPortalAccounts(db, dir); err != nil {
		t.Fatalf("有账号的自检应放行，实际 %v", err)
	}
}

// TestCheckPortOccupiedFreePort 验证空闲端口直接放行。
func TestCheckPortOccupiedFreePort(t *testing.T) {
	// 先占住一个端口，再要一个没人用的端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port

	if err := checkPortOccupied("管理端端口", busy, false); err == nil {
		t.Fatal("已被占用的端口应被 checkPortOccupied 拒绝")
	}

	free := busy%60000 + 30000
	if free == busy {
		free++
	}
	if err := checkPortOccupied("管理端端口", free, false); err != nil {
		t.Fatalf("空闲端口应放行，实际 %v", err)
	}
}

// TestCollectInstallSettingsScriptMode 验证脚本模式：不提问，直接用选项值，
// 端口被占用时提前报错。
func TestCollectInstallSettingsScriptMode(t *testing.T) {
	// 空闲端口：原样返回选项值。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	free := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("host", "0.0.0.0", "")
	fs.Int("port", 3082, "")
	fs.String("state-dir", "", "")
	fs.String("admin", "admin", "")
	fs.Bool("start", false, "")
	fs.Bool("service", false, "")
	_ = fs.Parse([]string{"--port", strconv.Itoa(free), "--start"})

	visited := map[string]bool{"port": true, "start": true}
	s, err := collectInstallSettings(fs, visited, "0.0.0.0", free, "/tmp/state", "admin", false, false, false, false)
	if err != nil {
		t.Fatalf("脚本模式不应报错: %v", err)
	}
	if s.port != free || !s.start || s.stateDir != "/tmp/state" {
		t.Fatalf("脚本模式应原样采用选项值，实际 %+v", s)
	}

	// 被占用的端口：提前失败。
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln2.Close()
	busy := ln2.Addr().(*net.TCPAddr).Port
	if _, err := collectInstallSettings(fs, visited, "0.0.0.0", busy, "", "admin", false, false, false, false); err == nil {
		t.Fatal("端口被占用时脚本模式应提前报错")
	}
}

// TestManageCommandAcceptsLegacyPortalName 验证旧名 portal 仍然可用：
// 改名不能把老脚本和用户的肌肉记忆一次性废掉。
func TestManageCommandAcceptsLegacyPortalName(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "")

	// 新名：manage user list
	if code := run([]string{"manage", "user", "list", "--state-dir", dir}); code != 0 {
		t.Fatalf("manage user list 应成功，退出码 %d", code)
	}
	// 旧名建号：portal user add（写的是同一份 portal.db）
	withStdin(t, "alice-password-123\n")
	if code := run([]string{"portal", "user", "add", "alice", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("旧名 portal user add 应成功，退出码 %d", code)
	}
	// 新名应看得到旧名建的账号：证明两个命令名共用同一套数据。
	out, code := captureStdout(t, func() int {
		return run([]string{"manage", "user", "list", "--state-dir", dir})
	})
	if code != 0 {
		t.Fatalf("manage user list 应成功，退出码 %d", code)
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("新名应看到旧名建的账号，实际 %s", out)
	}
}

// TestInstanceAddAcceptsInstallOptions 验证 CLI 的实例新增能带上安装相关选项，
// 并与管理台页面写进同一批列。
func TestInstanceAddAcceptsInstallOptions(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败，退出码 %d", code)
	}

	code := run([]string{
		"manage", "instance", "add", "team-main",
		"--state-dir", dir,
		"--dsh-home", "/srv/dsh/team",
		"--config-dir", "/srv/dsh/team/config",
		"--workspace", "/srv/work",
		"--listen-host", "0.0.0.0",
		"--install-mode", "git",
		"--install-dir", "/srv/dsh/team/src",
		"--on-demand",
	})
	if code != 0 {
		t.Fatalf("instance add 应成功，退出码 %d", code)
	}

	// 读回来核对：新列都要如实落库。
	out, code := captureStdout(t, func() int {
		return run([]string{"manage", "instance", "list", "--state-dir", dir})
	})
	if code != 0 {
		t.Fatalf("instance list 应成功，退出码 %d", code)
	}
	if !strings.Contains(out, "team-main") {
		t.Fatalf("列表里应看到新实例，实际 %s", out)
	}
}

// TestInstanceAddRejectsBadInstallMode 验证非法安装方式被挡下。
func TestInstanceAddRejectsBadInstallMode(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败")
	}
	code := run([]string{
		"manage", "instance", "add", "bad",
		"--state-dir", dir, "--dsh-home", "/srv/x", "--install-mode", "rpm",
	})
	if code == 0 {
		t.Fatal("非法安装方式应被拒绝")
	}
}

// TestInstanceInstallUnknownInstance 验证对不存在的实例触发安装会报错，
// 而不是静默成功。
func TestInstanceInstallUnknownInstance(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败")
	}
	code := run([]string{"manage", "instance", "install", "nope", "--state-dir", dir})
	if code == 0 {
		t.Fatal("对不存在的实例安装应失败")
	}
}

// TestInstanceRenameChangesDisplayNameOnly 验证改名只动展示名。
//
// 这条命令的存在理由就是「名字起错了别逼人删实例重建」：删除会连带丢掉授权与
// 登记关系。所以测试要证明 id 不变（授权挂在 id 上）而名字确实变了，并且中文名
// 可以正常落库、在列表与授权里正常显示。
func TestInstanceRenameChangesDisplayNameOnly(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败")
	}
	withStdin(t, "alice-password-123\n")
	if code := run([]string{"manage", "user", "add", "alice", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建普通用户失败")
	}
	if code := run([]string{
		"manage", "instance", "add", "alice-main",
		"--state-dir", dir, "--dsh-home", "/srv/dsh/alice",
	}); code != 0 {
		t.Fatalf("建实例失败")
	}
	// 授权要显式建：--owner 只是「默认拥有者」这个展示字段，访问判定只看 grants。
	if code := run([]string{"manage", "grant", "add", "alice", "alice-main", "--state-dir", dir}); code != 0 {
		t.Fatalf("授权失败")
	}

	out, code := captureStdout(t, func() int {
		return run([]string{"manage", "instance", "rename", "alice-main", "爱丽丝的主实例", "--state-dir", dir})
	})
	if code != 0 {
		t.Fatalf("改名应成功，退出码 %d，输出 %s", code, out)
	}
	if !strings.Contains(out, "爱丽丝的主实例") {
		t.Errorf("改名结果里应出现新名，实际 %s", out)
	}

	after, code := captureStdout(t, func() int {
		return run([]string{"manage", "instance", "list", "--state-dir", dir})
	})
	if code != 0 {
		t.Fatalf("instance list 应成功")
	}
	if !strings.Contains(after, "爱丽丝的主实例") {
		t.Errorf("列表里应看到中文新名，实际 %s", after)
	}
	if strings.Contains(after, "alice-main") {
		t.Errorf("列表里不应再有旧名，实际 %s", after)
	}

	// 授权必须还在：它挂在 id 上，改名不该动它。按新名核对。
	grants, code := captureStdout(t, func() int {
		return run([]string{"manage", "grant", "list", "alice", "--state-dir", dir})
	})
	if code != 0 {
		t.Fatalf("grant list 应成功")
	}
	if !strings.Contains(grants, "爱丽丝的主实例") {
		t.Errorf("改名后授权应仍然有效并显示新名，实际 %s", grants)
	}
}

// TestInstanceRenameRejectsBadNames 验证改名同样要过名字校验，且不允许撞名。
func TestInstanceRenameRejectsBadNames(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败")
	}
	for _, name := range []string{"first", "second"} {
		if code := run([]string{
			"manage", "instance", "add", name, "--state-dir", dir, "--dsh-home", "/srv/dsh/" + name,
		}); code != 0 {
			t.Fatalf("建实例 %s 失败", name)
		}
	}

	// 非法名字（开头是 -，会被 CLI 当成选项）必须被拒。
	if code := run([]string{"manage", "instance", "rename", "first", "-bad", "--state-dir", dir}); code == 0 {
		t.Error("以 - 开头的实例名应被拒绝")
	}
	// 撞上已有名字必须被拒。
	if code := run([]string{"manage", "instance", "rename", "first", "second", "--state-dir", dir}); code == 0 {
		t.Error("改成已存在的实例名应被拒绝")
	}
	// 参数不足必须给出用法而不是 panic。
	if code := run([]string{"manage", "instance", "rename", "first", "--state-dir", dir}); code == 0 {
		t.Error("缺少新名时应给出用法")
	}
}
