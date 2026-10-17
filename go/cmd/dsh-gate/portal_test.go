package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/portal"
)

// withStdin 把标准输入临时替换成给定的内容（口令来自 stdin 的那几条路）。
func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatalf("造 stdin 文件失败: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("写 stdin 失败: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("回绕 stdin 失败: %v", err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

// captureStdout 跑一段代码并捕获它写到 stdout 的内容。
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("造管道失败: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(r)
		done <- string(raw)
	}()
	code := fn()
	_ = w.Close()
	os.Stdout = old
	return <-done, code
}

// newPortalDB 造一个空的隔离管理端库路径。
func newPortalDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "portal.db")
}

func openStore(t *testing.T, path string) *portal.Store {
	t.Helper()
	store, err := portal.Open(path)
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestExtractLang(t *testing.T) {
	cases := []struct {
		argv []string
		lang string
		rest []string
	}{
		{[]string{"--lang", "en", "portal", "user", "list"}, "en", []string{"portal", "user", "list"}},
		{[]string{"--lang=en", "status"}, "en", []string{"status"}},
		{[]string{"portal", "user", "list"}, "", []string{"portal", "user", "list"}},
		{[]string{"--lang"}, "", []string{"--lang"}}, // 缺取值：原样留下，交给子命令报错
	}
	for _, c := range cases {
		lang, rest := extractLang(c.argv)
		if lang != c.lang || strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("extractLang(%v) = (%q, %v)，期望 (%q, %v)", c.argv, lang, rest, c.lang, c.rest)
		}
	}
}

func TestPortalCLIUserLifecycle(t *testing.T) {
	db := newPortalDB(t)
	withStdin(t, "root-password-123\n")

	if code := run([]string{"portal", "user", "add", "root", "--role", "admin", "--db", db, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员应成功，退出码 %d", code)
	}
	withStdin(t, "alice-password-123\n")
	if code := run([]string{"portal", "user", "add", "alice", "--db", db, "--password-stdin"}); code != 0 {
		t.Fatalf("建普通用户应成功，退出码 %d", code)
	}

	store := openStore(t, db)
	root, err := store.UserByName("root")
	if err != nil || root.Role != portal.RoleAdmin {
		t.Fatalf("root 应是管理员: %+v %v", root, err)
	}
	alice, err := store.UserByName("alice")
	if err != nil || alice.Role != portal.RoleUser {
		t.Fatalf("alice 应是普通用户: %+v %v", alice, err)
	}
	// 命令行建出来的账号能通过管理端的认证。
	if _, err := store.Authenticate("alice", "alice-password-123"); err != nil {
		t.Fatalf("新账号应能登录: %v", err)
	}

	// 改角色。
	if code := run([]string{"portal", "user", "role", "alice", "admin", "--db", db}); code != 0 {
		t.Fatalf("改角色应成功，退出码 %d", code)
	}
	alice, _ = store.UserByName("alice")
	if alice.Role != portal.RoleAdmin {
		t.Fatalf("alice 应已是管理员，实际 %q", alice.Role)
	}

	// 停用 / 启用。
	if code := run([]string{"portal", "user", "disable", "alice", "--db", db}); code != 0 {
		t.Fatalf("停用应成功，退出码 %d", code)
	}
	alice, _ = store.UserByName("alice")
	if !alice.Disabled {
		t.Fatal("alice 应已停用")
	}
	if code := run([]string{"portal", "user", "enable", "alice", "--db", db}); code != 0 {
		t.Fatalf("启用应成功，退出码 %d", code)
	}

	// 改口令：旧口令失效。
	withStdin(t, "brand-new-password\n")
	if code := run([]string{"portal", "user", "passwd", "alice", "--db", db, "--password-stdin"}); code != 0 {
		t.Fatalf("改口令应成功，退出码 %d", code)
	}
	if _, err := store.Authenticate("alice", "brand-new-password"); err != nil {
		t.Fatalf("新口令应可用: %v", err)
	}

	// 删除。
	if code := run([]string{"portal", "user", "rm", "alice", "--db", db}); code != 0 {
		t.Fatalf("删除应成功，退出码 %d", code)
	}
	if _, err := store.UserByName("alice"); err == nil {
		t.Fatal("alice 应已被删除")
	}
}

func TestPortalCLILastAdminProtected(t *testing.T) {
	db := newPortalDB(t)
	withStdin(t, "root-password-123\n")
	if code := run([]string{"portal", "user", "add", "root", "--role", "admin", "--db", db, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败: %d", code)
	}

	// 降级 / 停用 / 删除唯一的管理员都必须失败（退出码 1），且状态不变。
	for _, args := range [][]string{
		{"portal", "user", "role", "root", "user", "--db", db},
		{"portal", "user", "disable", "root", "--db", db},
		{"portal", "user", "rm", "root", "--db", db},
	} {
		if code := run(args); code != 1 {
			t.Errorf("%v 应被拒绝（退出码 1），实际 %d", args, code)
		}
	}
	store := openStore(t, db)
	root, err := store.UserByName("root")
	if err != nil || root.Disabled || root.Role != portal.RoleAdmin {
		t.Fatalf("最后一个管理员不该被改动: %+v %v", root, err)
	}
}

func TestPortalCLIInstancesAndGrants(t *testing.T) {
	db := newPortalDB(t)
	withStdin(t, "root-password-123\n")
	if code := run([]string{"portal", "user", "add", "root", "--role", "admin", "--db", db, "--password-stdin"}); code != 0 {
		t.Fatalf("建管理员失败: %d", code)
	}
	withStdin(t, "alice-password-123\n")
	if code := run([]string{"portal", "user", "add", "alice", "--db", db, "--password-stdin"}); code != 0 {
		t.Fatalf("建普通用户失败: %d", code)
	}
	home := filepath.Join(t.TempDir(), "dsh-alice")

	if code := run([]string{"portal", "instance", "add", "alice-main", "--dsh-home", home, "--owner", "root", "--db", db}); code != 0 {
		t.Fatalf("建实例应成功，退出码 %d", code)
	}
	store := openStore(t, db)
	inst, err := store.InstanceByName("alice-main")
	if err != nil {
		t.Fatalf("实例没建出来: %v", err)
	}
	if inst.ID == "" || inst.DshPort == 0 || inst.DshHome != home {
		t.Fatalf("实例字段不对: %+v", inst)
	}
	if inst.OwnerID == nil {
		t.Error("--owner 应被记录")
	}

	// 授权用**普通用户**验证：管理员天然能进所有实例，拿管理员测等于没测。
	alice, err := store.UserByName("alice")
	if err != nil {
		t.Fatalf("读 alice 失败: %v", err)
	}
	if _, err := store.Accessible(alice, inst.ID); err == nil {
		t.Fatal("授权前 alice 不该能访问")
	}
	if code := run([]string{"portal", "grant", "add", "alice", "alice-main", "--db", db}); code != 0 {
		t.Fatalf("授权应成功，退出码 %d", code)
	}
	if _, err := store.Accessible(alice, inst.ID); err != nil {
		t.Fatalf("授权后 alice 应可访问: %v", err)
	}
	// 取消授权时用不透明 id 再走一遍（两种写法都要认）。
	if code := run([]string{"portal", "grant", "rm", "alice", inst.ID, "--db", db}); code != 0 {
		t.Fatalf("取消授权应成功，退出码 %d", code)
	}
	if _, err := store.Accessible(alice, inst.ID); err == nil {
		t.Fatal("取消授权后 alice 不该还能访问")
	}

	// 实例列表与删除。
	if code := run([]string{"portal", "instance", "list", "--db", db}); code != 0 {
		t.Fatalf("实例列表应成功，退出码 %d", code)
	}
	if code := run([]string{"portal", "instance", "rm", "alice-main", "--db", db}); code != 0 {
		t.Fatalf("删除实例应成功，退出码 %d", code)
	}
	if _, err := store.InstanceByName("alice-main"); err == nil {
		t.Error("实例应已被删除")
	}
}

func TestPortalCLIUsageErrors(t *testing.T) {
	db := newPortalDB(t)
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"portal"}, 2},                                          // 缺子命令
		{[]string{"portal", "nope"}, 2},                                  // 未知子命令
		{[]string{"portal", "user"}, 2},                                  // 缺 user 子命令
		{[]string{"portal", "user", "add", "--db", db}, 2},               // 缺用户名
		{[]string{"portal", "user", "role", "x", "root", "--db", db}, 2}, // 角色非法
		{[]string{"portal", "instance", "add", "x", "--db", db}, 2},      // 缺 --dsh-home
		{[]string{"portal", "instance", "rm", "--db", db}, 2},            // 缺实例名
		{[]string{"portal", "grant"}, 2},                                 // 缺 grant 子命令
		{[]string{"nope"}, 2},                                            // 顶层未知子命令
	}
	for _, c := range cases {
		if code := run(c.args); code != c.code {
			t.Errorf("%v 退出码应为 %d，实际 %d", c.args, c.code, code)
		}
	}
}

func TestPortalCLIEnglishOutput(t *testing.T) {
	db := newPortalDB(t)
	withStdin(t, "root-password-123\n")

	out, code := captureStdout(t, func() int {
		return run([]string{"--lang", "en", "portal", "user", "add", "root", "--role", "admin", "--db", db, "--password-stdin"})
	})
	if code != 0 {
		t.Fatalf("建用户应成功，退出码 %d", code)
	}
	if !strings.Contains(out, "created user") {
		t.Errorf("--lang en 时输出应为英文，实际 %q", out)
	}
	if strings.Contains(out, "已创建") {
		t.Errorf("--lang en 时不该出现中文，实际 %q", out)
	}

	// 中文（默认）：同一动作输出中文。
	db2 := newPortalDB(t)
	withStdin(t, "root-password-123\n")
	out, code = captureStdout(t, func() int {
		return run([]string{"--lang", "zh-CN", "portal", "user", "add", "root", "--role", "admin", "--db", db2, "--password-stdin"})
	})
	if code != 0 || !strings.Contains(out, "已创建") {
		t.Errorf("默认应输出中文，实际退出码 %d、输出 %q", code, out)
	}
}

func TestPortalCLIEnglishHelp(t *testing.T) {
	out, code := captureStdout(t, func() int { return run([]string{"--lang", "en", "portal", "--help"}) })
	if code != 0 {
		t.Fatalf("portal --help 应回 0，实际 %d", code)
	}
	if !strings.Contains(out, "Usage: dsh-gate manage") {
		t.Errorf("英文帮助没出来，实际 %q", out)
	}
	if !strings.Contains(out, "grant access") {
		t.Errorf("英文帮助缺少子命令说明，实际 %q", out)
	}

	outZH, _ := captureStdout(t, func() int { return run([]string{"portal", "--help"}) })
	if !strings.Contains(outZH, "用法: dsh-gate manage") {
		t.Errorf("中文帮助没出来，实际 %q", outZH)
	}
}
