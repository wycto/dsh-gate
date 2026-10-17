package portal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wycto/dsh-gate/internal/users"
)

// newStore 在临时目录里开一个全新的管理端库。
func newStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// mustCreateUser 建账号并在失败时直接失败测试。
func mustCreateUser(t *testing.T, s *Store, name, password string, role Role) *User {
	t.Helper()
	u, err := s.CreateUser(name, password, role)
	if err != nil {
		t.Fatalf("创建账号 %q 失败: %v", name, err)
	}
	return u
}

func mustCreateInstance(t *testing.T, s *Store, name, home string) *Instance {
	t.Helper()
	inst := &Instance{Name: name, DshHome: home}
	if err := s.CreateInstance(inst); err != nil {
		t.Fatalf("创建实例 %q 失败: %v", name, err)
	}
	return inst
}

func TestOpenCreatesSchemaAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portal.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	mustCreateUser(t, s, "alice", "correct horse battery", RoleAdmin)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	// 重新打开：结构已存在，数据还在，口令仍可校验。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("再次打开失败: %v", err)
	}
	defer s2.Close()
	u, err := s2.Authenticate("alice", "correct horse battery")
	if err != nil {
		t.Fatalf("重新打开后认证失败: %v", err)
	}
	if !u.IsAdmin() {
		t.Fatalf("alice 应是管理员，实际角色 %q", u.Role)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat 数据库失败: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("管理端数据库权限应为 0600，实际 %o", perm)
		}
	}
}

func TestSchemaVersionGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("写版本号失败: %v", err)
	}
	_ = s.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("结构版本比程序更高时必须拒绝打开，实际成功")
	}
}

func TestAuthenticate(t *testing.T) {
	s := newStore(t)
	mustCreateUser(t, s, "alice", "correct horse battery", RoleUser)

	if _, err := s.Authenticate("alice", "correct horse battery"); err != nil {
		t.Fatalf("正确口令应通过: %v", err)
	}
	if _, err := s.Authenticate("alice", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("错误口令应返回 ErrBadCredentials，实际 %v", err)
	}
	if _, err := s.Authenticate("nobody", "whatever"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("不存在的账号应返回 ErrBadCredentials，实际 %v", err)
	}

	// 停用后：口令对 → ErrDisabled；口令错 → 仍是 ErrBadCredentials（不泄漏停用状态）。
	if err := s.SetDisabled("alice", true); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, err := s.Authenticate("alice", "correct horse battery"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("停用账号应返回 ErrDisabled，实际 %v", err)
	}
	if _, err := s.Authenticate("alice", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("停用账号+错口令应仍返回 ErrBadCredentials，实际 %v", err)
	}
}

func TestUserValidationAndUniqueness(t *testing.T) {
	s := newStore(t)
	mustCreateUser(t, s, "alice", "correct horse battery", RoleUser)

	if _, err := s.CreateUser("alice", "another good password", RoleUser); err == nil {
		t.Fatal("重名应被拒绝")
	}
	if _, err := s.CreateUser("bob", "short", RoleUser); err == nil {
		t.Fatal("口令过短应被拒绝")
	}
	if _, err := s.CreateUser("bad name", "correct horse battery", RoleUser); err == nil {
		t.Fatal("非法用户名应被拒绝")
	}
	if _, err := s.CreateUser("carol", "correct horse battery", Role("root")); err == nil {
		t.Fatal("非法角色应被拒绝")
	}
}

func TestLastAdminProtection(t *testing.T) {
	s := newStore(t)
	mustCreateUser(t, s, "root", "correct horse battery", RoleAdmin)

	if err := s.DeleteUser("root"); err == nil {
		t.Fatal("删除最后一个管理员应被拒绝")
	}
	if err := s.SetRole("root", RoleUser); err == nil {
		t.Fatal("降级最后一个管理员应被拒绝")
	}
	if err := s.SetDisabled("root", true); err == nil {
		t.Fatal("停用最后一个管理员应被拒绝")
	}

	// 有第二个管理员后：允许降级 root；但此时 root2 成了唯一管理员，
	// 删它仍应被拒绝——保护看的是「还剩几个管理员」，不是「谁」。
	mustCreateUser(t, s, "root2", "correct horse battery", RoleAdmin)
	if err := s.SetRole("root", RoleUser); err != nil {
		t.Fatalf("有备用管理员后应允许降级: %v", err)
	}
	if err := s.DeleteUser("root2"); err == nil {
		t.Fatal("root2 已是唯一管理员，删除应被拒绝")
	}
	// 把 root 提回管理员，再删 root2 就应当放行。
	if err := s.SetRole("root", RoleAdmin); err != nil {
		t.Fatalf("提回管理员失败: %v", err)
	}
	if err := s.DeleteUser("root2"); err != nil {
		t.Fatalf("有主管理员时应允许删除另一个管理员: %v", err)
	}
}

// TestInstanceWorkspaceDefaultsUnderInstanceDir 验证「工作目录留空」时的默认推导。
//
// 为什么必须有这条默认值：dsh 自己的默认工作区是系统的「文档」目录，无头 Linux
// 上要靠 xdg-user-dir 解析，容器与服务器通常没有这个命令——实例第一次进去只会
// 看到「无法创建默认工作区」。网关的预置能力（把工作目录写进 dsh 的工作区注册表）
// 又要求工作目录非空。所以留空必须推导出一个能用的目录，而不是空串。
func TestInstanceWorkspaceDefaultsUnderInstanceDir(t *testing.T) {
	s := newStore(t)

	// 未显式指定：默认 <DSH_HOME>/workspace。
	inst := mustCreateInstance(t, s, "default-ws", "/srv/dsh/default-ws")
	want := filepath.Join("/srv/dsh/default-ws", "workspace")
	if inst.Workspace != want {
		t.Fatalf("工作目录应默认为 %q，实际 %q", want, inst.Workspace)
	}
	// 读回来也应当是同一个值（默认值在读写两条路上必须一致，否则管理台与
	// 运行时看到的不是同一个目录）。
	got, err := s.InstanceByID(inst.ID)
	if err != nil {
		t.Fatalf("读回实例失败: %v", err)
	}
	if got.Workspace != want {
		t.Errorf("读回的默认工作目录应为 %q，实际 %q", want, got.Workspace)
	}

	// 显式指定时不得覆盖。
	explicit := &Instance{Name: "explicit-ws", DshHome: "/srv/dsh/explicit", Workspace: "/srv/work/x"}
	if err := s.CreateInstance(explicit); err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}
	if explicit.Workspace != "/srv/work/x" {
		t.Errorf("显式指定的工作目录被改写成了 %q", explicit.Workspace)
	}

	// 指定了配置目录时，工作区跟着配置目录走（与运行时 homeOf 一致）。
	withConfig := &Instance{Name: "cfg-ws", DshHome: "/srv/dsh/cfg", ConfigDir: "/srv/cfg"}
	if err := s.CreateInstance(withConfig); err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}
	if want := filepath.Join("/srv/cfg", "workspace"); withConfig.Workspace != want {
		t.Errorf("有配置目录时工作目录应为 %q，实际 %q", want, withConfig.Workspace)
	}
}

func TestInstanceLifecycle(t *testing.T) {
	s := newStore(t)

	inst := mustCreateInstance(t, s, "alice-main", "/srv/dsh/alice")
	if inst.ID == "" || !InstanceIDPattern.MatchString(inst.ID) {
		t.Fatalf("自动生成的实例 id 不合规: %q", inst.ID)
	}
	if inst.DshPort <= 0 {
		t.Fatalf("应自动分配端口，实际 %d", inst.DshPort)
	}
	if !inst.Enabled {
		t.Fatal("新建实例应为启用状态")
	}

	// 端口不重复。
	second := mustCreateInstance(t, s, "bob-main", "/srv/dsh/bob")
	third := mustCreateInstance(t, s, "carol-main", "/srv/dsh/carol")
	if inst.DshPort == second.DshPort || second.DshPort == third.DshPort {
		t.Fatalf("自动分配的端口重复: %d %d %d", inst.DshPort, second.DshPort, third.DshPort)
	}

	// 名字不能重复、dsh-home 必填。
	if err := s.CreateInstance(&Instance{Name: "alice-main", DshHome: "/x"}); err == nil {
		t.Fatal("重名实例应被拒绝")
	}
	if err := s.CreateInstance(&Instance{Name: "no-home"}); err == nil {
		t.Fatal("缺少 dsh-home 应被拒绝")
	}

	// 更新：改端口、停用。
	inst.DshPort = 32999
	inst.Enabled = false
	inst.Workspace = "/srv/work/alice"
	if err := s.UpdateInstance(*inst); err != nil {
		t.Fatalf("更新实例失败: %v", err)
	}
	got, err := s.InstanceByID(inst.ID)
	if err != nil {
		t.Fatalf("读回实例失败: %v", err)
	}
	if got.DshPort != 32999 || got.Enabled || got.Workspace != "/srv/work/alice" {
		t.Fatalf("实例更新未生效: %+v", got)
	}

	// 删除，并验证授权级联删除。
	u := mustCreateUser(t, s, "alice", "correct horse battery", RoleUser)
	if err := s.Grant("alice", inst.ID, "root"); err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	if err := s.DeleteInstance(inst.ID); err != nil {
		t.Fatalf("删除实例失败: %v", err)
	}
	if _, err := s.InstanceByID(inst.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应查不到，实际 %v", err)
	}
	list, err := s.InstancesForUser(u.ID)
	if err != nil {
		t.Fatalf("读用户实例失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("实例删除后授权应级联清空，实际还有 %d 条", len(list))
	}
}

func TestGrantsAndAccess(t *testing.T) {
	s := newStore(t)
	admin := mustCreateUser(t, s, "root", "correct horse battery", RoleAdmin)
	alice := mustCreateUser(t, s, "alice", "correct horse battery", RoleUser)

	mine := mustCreateInstance(t, s, "alice-main", "/srv/dsh/alice")
	other := mustCreateInstance(t, s, "bob-main", "/srv/dsh/bob")

	if err := s.Grant("alice", mine.ID, "root"); err != nil {
		t.Fatalf("授权失败: %v", err)
	}

	if _, err := s.Accessible(alice, mine.ID); err != nil {
		t.Fatalf("被授权的实例应可访问: %v", err)
	}
	// 未授权：返回 ErrNotFound（与不存在同形，不泄漏存在性）。
	if _, err := s.Accessible(alice, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未授权实例应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := s.Accessible(admin, other.ID); err != nil {
		t.Fatalf("管理员应可访问任意实例: %v", err)
	}
	if _, err := s.Accessible(alice, "zzzzzzzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的实例应返回 ErrNotFound，实际 %v", err)
	}

	vis, err := s.VisibleInstances(alice)
	if err != nil || len(vis) != 1 || vis[0].ID != mine.ID {
		t.Fatalf("普通用户应只看到被授权的实例: %v %v", vis, err)
	}
	all, err := s.VisibleInstances(admin)
	if err != nil || len(all) != 2 {
		t.Fatalf("管理员应看到全部实例: %v %v", all, err)
	}

	// 矩阵式整表设置。
	if err := s.SetUserGrants("alice", []string{other.ID}, "root"); err != nil {
		t.Fatalf("整表设置授权失败: %v", err)
	}
	if _, err := s.Accessible(alice, mine.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("整表设置后旧授权应被清掉")
	}
	names, err := s.InstanceGrants(other.ID)
	if err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatalf("实例授权名单不对: %v %v", names, err)
	}
	// 取消授权是幂等的。
	if err := s.Revoke("alice", other.ID); err != nil {
		t.Fatalf("取消授权失败: %v", err)
	}
	if err := s.Revoke("alice", other.ID); err != nil {
		t.Fatalf("重复取消授权应幂等: %v", err)
	}
}

func TestAuditAppendAndRead(t *testing.T) {
	s := newStore(t)
	for _, action := range []string{"login.ok", "instance.enter", "user.add"} {
		if err := s.Audit(AuditEntry{Actor: "root", Action: action, Target: "alice", IP: "127.0.0.1", OK: true}); err != nil {
			t.Fatalf("写审计失败: %v", err)
		}
	}
	entries, err := s.RecentAudit(10)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("应有 3 条审计，实际 %d", len(entries))
	}
	if entries[0].Action != "user.add" {
		t.Fatalf("审计应按时间倒序（最新在前），实际首条 %q", entries[0].Action)
	}
	n, err := s.AuditCount()
	if err != nil || n != 3 {
		t.Fatalf("审计计数不对: %d %v", n, err)
	}
}

func TestImportLegacyUsers(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "users.json")

	hash, err := users.HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("造哈希失败: %v", err)
	}
	doc := map[string]any{
		"version": 1,
		"users": map[string]any{
			"alice": map[string]any{"hash": hash},
			"bob":   map[string]any{"hash": hash},
		},
	}
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(legacy, raw, 0o600); err != nil {
		t.Fatalf("写旧账号表失败: %v", err)
	}

	res, err := s.ImportLegacyUsers(legacy)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	if res.Imported != 2 || res.Admin != "alice" {
		t.Fatalf("导入结果不对: %+v（首个（按名字排序）应被提升为管理员）", res)
	}
	// 原口令必须继续可用（原样搬运哈希）。
	if _, err := s.Authenticate("alice", "correct horse battery"); err != nil {
		t.Fatalf("迁移后的账号应能用原口令登录: %v", err)
	}

	// 幂等：再跑一次全都跳过。
	res2, err := s.ImportLegacyUsers(legacy)
	if err != nil {
		t.Fatalf("重复导入失败: %v", err)
	}
	if res2.Imported != 0 || res2.Skipped != 2 || res2.Admin != "" {
		t.Fatalf("重复导入应全部跳过: %+v", res2)
	}

	// 文件不存在不是错误。
	res3, err := s.ImportLegacyUsers(filepath.Join(dir, "nope.json"))
	if err != nil || !res3.Missing {
		t.Fatalf("文件不存在应标记 Missing: %+v %v", res3, err)
	}
}

func TestNewInstanceIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewInstanceID()
		if err != nil {
			t.Fatalf("生成 id 失败: %v", err)
		}
		if !InstanceIDPattern.MatchString(id) {
			t.Fatalf("id %q 不合规", id)
		}
		if seen[id] {
			t.Fatalf("200 次生成出现重复 id %q，随机性可疑", id)
		}
		seen[id] = true
	}
}
