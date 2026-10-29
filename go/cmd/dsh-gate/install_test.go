package main

import (
	"flag"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/registry"
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
	if !strings.Contains(out, "dsh-gate start") {
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

// TestCheckPortOccupiedIgnoresOwnPortal 锁住重装场景：端口被自己的管理端占着
// 不算冲突（install 会把它停下换成新版），被别的程序占着才拦。
//
// Windows 上 dshproc.Inspect 反查不到 PID，只能靠 ~/.dsh-gate/run 里的登记记录
// 认人，所以这条对 Windows 尤其重要。测试用临时 HOME 隔离真实登记目录。
func TestCheckPortOccupiedIgnoresOwnPortal(t *testing.T) {
	home := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	runDir := resolveRunDir("")
	// 记一条「自己正在跑」的登记：PID 用当前测试进程，它当然活着。
	if err := runDir.Save(registry.Record{Name: portalRecordName, PID: os.Getpid(), Port: port}); err != nil {
		t.Fatalf("写登记失败: %v", err)
	}
	if err := checkPortOccupied("管理端端口", port, false); err != nil {
		t.Errorf("端口被自己的管理端占着不该算冲突，实际 %v", err)
	}

	// 登记里的端口对不上（占用者是别人）时仍要拦。
	if err := runDir.Save(registry.Record{Name: portalRecordName, PID: os.Getpid(), Port: port + 1}); err != nil {
		t.Fatalf("改写登记失败: %v", err)
	}
	if err := checkPortOccupied("管理端端口", port, false); err == nil {
		t.Error("占用者不是自己的管理端时应报冲突")
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
	fs.Int("port", 12020, "")
	fs.String("state-dir", "", "")
	fs.String("admin", "admin", "")
	fs.String("display", "", "")
	fs.Bool("start", false, "")
	fs.Bool("service", false, "")
	_ = fs.Parse([]string{"--port", strconv.Itoa(free), "--start"})

	visited := map[string]bool{"port": true, "start": true}
	s, err := collectInstallSettings(fs, visited, "0.0.0.0", free, "/tmp/state", "admin", "魏一", false, false, nil, 0, false, false)
	if err != nil {
		t.Fatalf("脚本模式不应报错: %v", err)
	}
	if s.port != free || !s.start || s.stateDir != "/tmp/state" {
		t.Fatalf("脚本模式应原样采用选项值，实际 %+v", s)
	}
	if s.display != "魏一" {
		t.Errorf("姓名应原样采用选项值，实际 %q", s.display)
	}

	// 被占用的端口：提前失败。
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	defer ln2.Close()
	busy := ln2.Addr().(*net.TCPAddr).Port
	if _, err := collectInstallSettings(fs, visited, "0.0.0.0", busy, "", "admin", "", false, false, nil, 0, false, false); err == nil {
		t.Fatal("端口被占用时脚本模式应提前报错")
	}
}

func TestManageCommandAcceptsLegacyPortalName(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "")

	// 新名：manage user list
	if code := run([]string{"manage", "user", "list", "--state-dir", dir}); code != 0 {
		t.Fatalf("manage user list 应成功，退出码 %d", code)
	}
	// 旧名建号：portal user add（写的是同一份 portal.db）
	withStdin(t, "alice-password-123\n")
	if code := run([]string{"manage", "user", "add", "alice", "--state-dir", dir, "--password-stdin"}); code != 0 {
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
		"dsh", "add", "team-main",
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
		return run([]string{"dsh", "list", "--state-dir", dir})
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
		"dsh", "add", "bad",
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
	code := run([]string{"dsh", "install", "nope", "--state-dir", dir})
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
		"dsh", "add", "alice-main",
		"--state-dir", dir, "--dsh-home", "/srv/dsh/alice",
	}); code != 0 {
		t.Fatalf("建实例失败")
	}
	// 授权要显式建：--owner 只是「默认拥有者」这个展示字段，访问判定只看 grants。
	if code := run([]string{"manage", "grant", "add", "alice", "alice-main", "--state-dir", dir}); code != 0 {
		t.Fatalf("授权失败")
	}

	out, code := captureStdout(t, func() int {
		return run([]string{"dsh", "rename", "alice-main", "爱丽丝的主实例", "--state-dir", dir})
	})
	if code != 0 {
		t.Fatalf("改名应成功，退出码 %d，输出 %s", code, out)
	}
	if !strings.Contains(out, "爱丽丝的主实例") {
		t.Errorf("改名结果里应出现新名，实际 %s", out)
	}

	after, code := captureStdout(t, func() int {
		return run([]string{"dsh", "list", "--state-dir", dir})
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
			"dsh", "add", name, "--state-dir", dir, "--dsh-home", "/srv/dsh/" + name,
		}); code != 0 {
			t.Fatalf("建实例 %s 失败", name)
		}
	}

	// 非法名字（开头是 -，会被 CLI 当成选项）必须被拒。
	if code := run([]string{"dsh", "rename", "first", "-bad", "--state-dir", dir}); code == 0 {
		t.Error("以 - 开头的实例名应被拒绝")
	}
	// 撞上已有名字必须被拒。
	if code := run([]string{"dsh", "rename", "first", "second", "--state-dir", dir}); code == 0 {
		t.Error("改成已存在的实例名应被拒绝")
	}
	// 参数不足必须给出用法而不是 panic。
	if code := run([]string{"dsh", "rename", "first", "--state-dir", dir}); code == 0 {
		t.Error("缺少新名时应给出用法")
	}
}

// TestPrintExistingAccountsMarksSuperAdmin 验证「已有账号」清单把类型说清楚：
// 谁是超级管理员、谁是普通用户、谁被停用。重装时这段输出就是用户判断
// 「我的账号还在不在、该动哪个」的唯一依据，只说「未创建新账号」不够。
func TestPrintExistingAccountsMarksSuperAdmin(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, filepath.Join(dir, "portal.db"))
	if _, err := store.CreateUser("root", "root-password-123", portal.RoleAdmin); err != nil {
		t.Fatalf("建超级管理员失败: %v", err)
	}
	if _, err := store.CreateUser("alice", "alice-password-123", portal.RoleUser); err != nil {
		t.Fatalf("建普通用户失败: %v", err)
	}
	if err := store.SetDisabled("alice", true); err != nil {
		t.Fatalf("停用 alice 失败: %v", err)
	}
	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("读账号列表失败: %v", err)
	}
	superID, err := store.SuperAdminID()
	if err != nil {
		t.Fatalf("取超级管理员失败: %v", err)
	}

	out, _ := captureStdout(t, func() int {
		printExistingAccounts(users, superID)
		return 0
	})
	// 表头先说清每列是什么，否则「超级管理员」落在角色列里也看不出含义。
	for _, want := range []string{"账号", "姓名", "角色", "状态"} {
		if !strings.Contains(out, want) {
			t.Errorf("清单表头缺少 %q，实际输出：%s", want, out)
		}
	}
	rootLine, aliceLine := "", ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " root ") || strings.HasPrefix(strings.TrimSpace(line), "root ") {
			rootLine = line
		}
		if strings.Contains(line, " alice ") || strings.HasPrefix(strings.TrimSpace(line), "alice ") {
			aliceLine = line
		}
	}
	if !strings.Contains(rootLine, tr(i18n.KeyAdminSuperBadge)) {
		t.Errorf("root 是超级管理员，行里应标出来，实际行：%q", rootLine)
	}
	if !strings.Contains(aliceLine, tr(i18n.KeyRoleUser)) {
		t.Errorf("alice 是普通用户，行里应显示普通用户，实际行：%q", aliceLine)
	}
	if strings.Contains(aliceLine, tr(i18n.KeyAdminSuperBadge)) {
		t.Errorf("超级管理员只应标在 root 上，alice 行：%q", aliceLine)
	}
	if !strings.Contains(aliceLine, tr(i18n.KeyCLIStatusDisabled)) {
		t.Errorf("alice 已停用，行里应显示停用，实际行：%q", aliceLine)
	}
}

// TestManageUserListMarksSuperAdmin 验证 `manage user list` 也能一眼看出谁是
// 超级管理员：CLI 上没有页面徽标，只能在角色列里标。
func TestManageUserListMarksSuperAdmin(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建超级管理员失败，退出码 %d", code)
	}
	withStdin(t, "alice-password-123\n")
	if code := run([]string{"manage", "user", "add", "alice", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建普通用户失败，退出码 %d", code)
	}

	out, code := captureStdout(t, func() int {
		return run([]string{"manage", "user", "list", "--state-dir", dir})
	})
	if code != 0 {
		t.Fatalf("manage user list 应成功，退出码 %d，输出：%s", code, out)
	}
	rootLine, aliceLine := "", ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " root ") {
			rootLine = line
		}
		if strings.Contains(line, " alice ") {
			aliceLine = line
		}
	}
	if !strings.Contains(rootLine, tr(i18n.KeyAdminSuperBadge)) {
		t.Errorf("root 是超级管理员，列表行应标出来，实际行：%q", rootLine)
	}
	if !strings.Contains(aliceLine, tr(i18n.KeyRoleUser)) {
		t.Errorf("alice 是普通用户，列表行应显示普通用户，实际行：%q", aliceLine)
	}
}

// TestCollectInstallSettingsExistingAccountsSkipsAccountQuestions 验证重装时
// 向导不再问账号与姓名（问了也不建号），改为在确认页列出已有账号及其类型。
func TestCollectInstallSettingsExistingAccountsSkipsAccountQuestions(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, filepath.Join(dir, "portal.db"))
	if _, err := store.CreateUser("root", "root-password-123", portal.RoleAdmin); err != nil {
		t.Fatalf("建超级管理员失败: %v", err)
	}
	if _, err := store.CreateUser("alice", "alice-password-123", portal.RoleUser); err != nil {
		t.Fatalf("建普通用户失败: %v", err)
	}
	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("读账号列表失败: %v", err)
	}
	superID, err := store.SuperAdminID()
	if err != nil {
		t.Fatalf("取超级管理员失败: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	free := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("host", "0.0.0.0", "")
	fs.Int("port", 12020, "")
	fs.String("state-dir", "", "")
	fs.String("admin", "admin", "")
	fs.String("display", "", "")
	fs.Bool("start", false, "")
	fs.Bool("service", false, "")
	// 环境项都算「已给过」，只留下确认页这一问。
	visited := map[string]bool{"host": true, "port": true, "bin-dir": true, "state-dir": true, "start": true, "service": true}
	withStdin(t, "y\n")

	var settings installSettings
	out, code := captureStdout(t, func() int {
		var err error
		settings, err = collectInstallSettings(fs, visited, "0.0.0.0", free, dir, "admin", "", true, false, users, superID, false, false)
		if err != nil {
			t.Errorf("已有账号时向导不应报错: %v", err)
			return 1
		}
		return 0
	})
	if code != 0 {
		t.Fatalf("向导应走完确认，输出：%s", out)
	}
	// 不再问账号与口令：问了也没用，反而让人觉得刚才填的账号生效了。
	if strings.Contains(out, "管理员账号 [") {
		t.Errorf("已有账号时不应再问管理员账号，实际输出：%s", out)
	}
	if strings.Contains(out, "口令（输入显示为 *）") {
		t.Errorf("已有账号时不应再问口令，实际输出：%s", out)
	}
	// 确认页要把已有账号列出来，并标出超级管理员。
	if !strings.Contains(out, tr(i18n.KeyAdminSuperBadge)) {
		t.Errorf("确认页应标出超级管理员，实际输出：%s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("确认页应列出已有账号，实际输出：%s", out)
	}
	if settings.admin != "admin" {
		t.Errorf("没有问账号时不应改动默认账号，实际 %q", settings.admin)
	}
}

// TestCollectInstallSettingsAsksBinDirWithPlatformDefault 锁住「安装目录」一问：
// 默认值必须来自 binDirDefault()，用户直接回车就采纳它。
//
// 这条曾经是「Windows 上不问安装目录」（那时 Windows 不拷命令副本）。现在
// Windows 也会拷到 %LOCALAPPDATA%\dsh-gate\bin，所以两个平台都要问、都要有
// 平台默认值——计划任务 / 服务单元指向这份拷贝，原目录被清理也不影响自启。
func TestCollectInstallSettingsAsksBinDirWithPlatformDefault(t *testing.T) {
	want := binDirDefault()
	if want == "" {
		t.Skip("当前环境没有可用的默认安装目录")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	free := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("host", "0.0.0.0", "")
	fs.Int("port", 12020, "")
	fs.String("state-dir", "", "")
	fs.String("admin", "admin", "")
	fs.String("display", "", "")
	fs.Bool("start", false, "")
	fs.Bool("service", false, "")
	fs.String("bin-dir", binDirDefault(), "")
	// 其余项都算「已给过」，只留下安装目录与确认页这两问：第一行回车取默认，
	// 第二行 y 确认。
	visited := map[string]bool{"host": true, "port": true, "state-dir": true, "start": true, "service": true}
	withStdin(t, "\ny\n")

	var settings installSettings
	out, code := captureStdout(t, func() int {
		var err error
		settings, err = collectInstallSettings(fs, visited, "0.0.0.0", free, t.TempDir(), "admin", "", true, false, nil, 0, false, false)
		if err != nil {
			t.Errorf("向导不应报错: %v", err)
			return 1
		}
		return 0
	})
	if code != 0 {
		t.Fatalf("向导应走完确认，输出：%s", out)
	}
	if !strings.Contains(out, "安装目录 [") {
		t.Errorf("应该问一次安装目录，实际输出：%s", out)
	}
	if settings.binDir != want {
		t.Errorf("回车应采纳默认安装目录 %q，实际 %q", want, settings.binDir)
	}
	if !strings.Contains(out, want) {
		t.Errorf("确认页应列出安装目录 %q，实际输出：%s", want, out)
	}
	if strings.Contains(out, "不拷贝") {
		t.Errorf("有默认安装目录时不该说「不拷贝」，实际输出：%s", out)
	}
}

// readInstance 从状态目录里的管理端库读回一条实例，用于核对 CLI 写进去的字段。
func readInstance(t *testing.T, stateDir, name string) portal.Instance {
	t.Helper()
	dbPath, err := portalDBPath(stateDir)
	if err != nil {
		t.Fatalf("定位管理端库失败: %v", err)
	}
	store, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	defer store.Close()
	inst, err := store.InstanceByName(name)
	if err != nil {
		t.Fatalf("读实例 %q 失败: %v", name, err)
	}
	return *inst
}

// TestInstanceAddPinsDshVersion CLI 钉住 dsh 版本：add 落库、edit 改版本、
// 显式给空串取消钉住。
func TestInstanceAddPinsDshVersion(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败，退出码 %d", code)
	}
	code := run([]string{
		"dsh", "add", "pinned", "--state-dir", dir,
		"--dsh-home", "/srv/dsh/pin", "--install-mode", "pnpm",
		"--install-dir", "/srv/dsh/pin/pkg", "--dsh-version", "0.2.2-alpha.2",
	})
	if code != 0 {
		t.Fatalf("带 --dsh-version 的 dsh add 应成功，退出码 %d", code)
	}
	if got := readInstance(t, dir, "pinned").DshVersion; got != "0.2.2-alpha.2" {
		t.Errorf("钉住的版本应落库，实际 %q", got)
	}

	if code := run([]string{"dsh", "edit", "pinned", "--state-dir", dir, "--dsh-version", "next"}); code != 0 {
		t.Fatalf("dsh edit 改版本应成功，退出码 %d", code)
	}
	if got := readInstance(t, dir, "pinned").DshVersion; got != "next" {
		t.Errorf("版本应改成 next，实际 %q", got)
	}

	// 显式给空串 = 取消钉住。不写这一栏时 edit 不该动它（只改显式给出的字段）。
	if code := run([]string{"dsh", "edit", "pinned", "--state-dir", dir, "--dsh-home", "/srv/dsh/pin2"}); code != 0 {
		t.Fatalf("dsh edit 应成功，退出码 %d", code)
	}
	if got := readInstance(t, dir, "pinned").DshVersion; got != "next" {
		t.Errorf("没给 --dsh-version 时不该动钉住的版本，实际 %q", got)
	}
	if code := run([]string{"dsh", "edit", "pinned", "--state-dir", dir, "--dsh-version", ""}); code != 0 {
		t.Fatalf("清空版本应成功，退出码 %d", code)
	}
	if got := readInstance(t, dir, "pinned").DshVersion; got != "" {
		t.Errorf("清空后应跟随 latest，实际 %q", got)
	}
}

// TestInstanceAddRejectsBadDshVersion 非法版本名被挡下：这一串会拼进启动命令。
func TestInstanceAddRejectsBadDshVersion(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败")
	}
	code := run([]string{
		"dsh", "add", "badver", "--state-dir", dir, "--dsh-home", "/srv/x",
		"--dsh-version", "1.0.0 --registry=http://evil.example",
	})
	if code == 0 {
		t.Fatal("非法版本名应被拒绝")
	}
}

// TestInstanceAddRejectsGitWithPinnedVersion 源码方式 + 钉版本是一对做不到的组合，
// CLI 与页面在同一个地方拦：装到一半才失败最费时间。
func TestInstanceAddRejectsGitWithPinnedVersion(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, "root-password-123\n")
	if code := run([]string{"manage", "user", "add", "root", "--state-dir", dir, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败")
	}
	for _, mode := range []string{"git", "local"} {
		code := run([]string{
			"dsh", "add", "gitver", "--state-dir", dir, "--dsh-home", "/srv/x",
			"--install-mode", mode, "--dsh-version", "next",
		})
		if code == 0 {
			t.Errorf("%s + 钉版本应被拒绝", mode)
		}
	}
}
