package portal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// 首位注册自动成为超级管理员（见 CreateUser）；停用测试要用非超管账号。
	mustCreateUser(t, s, "root", "correct horse battery", RoleUser)
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
	// 账号不支持中文（中文名走「姓名」字段）。
	if _, err := s.CreateUser("张三", "correct horse battery", RoleUser); err == nil {
		t.Fatal("中文账号应被拒绝")
	}
	if _, err := s.CreateUser("carol", "correct horse battery", Role("root")); err == nil {
		t.Fatal("非法角色应被拒绝")
	}
}

// TestCreateUserWithDisplay 姓名入库与回退：留空回退登录名，非空原样存。
func TestCreateUserWithDisplay(t *testing.T) {
	s := newStore(t)
	// 首位账号（超管）不带姓名：display 回退登录名。
	first := mustCreateUser(t, s, "root", "correct horse battery", RoleAdmin)
	if first.DisplayName != "root" {
		t.Errorf("姓名留空应回退登录名，实际 %q", first.DisplayName)
	}
	u, err := s.CreateUserWithDisplay("li.si", "correct horse battery", RoleUser, "李四 ")
	if err != nil {
		t.Fatalf("带姓名建号失败: %v", err)
	}
	if u.DisplayName != "李四" {
		t.Errorf("姓名应去空白后原样入库，实际 %q", u.DisplayName)
	}
	// 姓名里塞控制字符要拦住（账号合法、姓名非法）。
	if _, err := s.CreateUserWithDisplay("wang.wu", "correct horse battery", RoleUser, "坏\x00名"); err == nil {
		t.Fatal("非法姓名应被拒绝")
	}
}

func TestLastAdminProtection(t *testing.T) {
	s := newStore(t)
	mustCreateUser(t, s, "root", "correct horse battery", RoleAdmin)
	// 构造「超管不是管理员」的旧库形态：首位账号在新规则下自动是超管且
	// 受超管保护，超管保护先于最后管理员保护拦下一切——要让后者仍然可达，
	// 只能像迁移过来的老数据那样，把首位降成普通用户（直接改库模拟）。
	if err := s.updateUser("root", `UPDATE users SET role = ? WHERE name = ?`, string(RoleUser), "root"); err != nil {
		t.Fatalf("模拟旧库失败: %v", err)
	}

	mustCreateUser(t, s, "root2", "correct horse battery", RoleAdmin)
	if err := s.DeleteUser("root2"); err == nil {
		t.Fatal("删除最后一个管理员应被拒绝")
	}
	if err := s.SetRole("root2", RoleUser); err == nil {
		t.Fatal("降级最后一个管理员应被拒绝")
	}
	if err := s.SetDisabled("root2", true); err == nil {
		t.Fatal("停用最后一个管理员应被拒绝")
	}

	// 有备用管理员后：降级与删除都放行——保护看的是「还剩几个管理员」。
	mustCreateUser(t, s, "root3", "correct horse battery", RoleAdmin)
	if err := s.SetRole("root2", RoleUser); err != nil {
		t.Fatalf("有备用管理员后应允许降级: %v", err)
	}
	if err := s.DeleteUser("root2"); err != nil {
		t.Fatalf("有备用管理员后应允许删除: %v", err)
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

	// 名字不能重复。
	if err := s.CreateInstance(&Instance{Name: "alice-main", DshHome: "/x"}); err == nil {
		t.Fatal("重名实例应被拒绝")
	}
	// dsh-home 留空不是错误：落到 dsh 自己的默认目录（~/.dsh 展开后的绝对路径）。
	noHome := &Instance{Name: "no-home"}
	if err := s.CreateInstance(noHome); err != nil {
		t.Fatalf("dsh-home 留空应接受并补默认值，实际 %v", err)
	}
	if want := DefaultDshHome(); noHome.DshHome != want {
		t.Errorf("dsh-home 应补成默认值 %q，实际 %q", want, noHome.DshHome)
	}
	if strings.HasPrefix(noHome.DshHome, "~") {
		t.Errorf("默认值必须是展开后的绝对路径（子进程的 DSH_HOME 不经过 shell，`~` 不会被展开），实际 %q", noHome.DshHome)
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

// TestInstanceHomeDefaultsToDshDefault 验证「实例目录留空就用 dsh 的默认目录」。
//
// 这是用户直接提的需求：建实例时那一栏空着应该照常能建，落到 dsh 自己的
// ~/.dsh，而不是被一串「必填」拦住。
//
// 关键约束：落库的必须是**展开后的绝对路径**。这个值稍后会被原样塞进子进程
// 环境变量（DSH_HOME=...），环境变量不经过 shell，`~` 不会被展开——存字面量
// `~/.dsh` 会让 dsh 在当前工作目录下建出一个名字就叫 `~` 的目录，数据写错地方。
func TestInstanceHomeDefaultsToDshDefault(t *testing.T) {
	s := newStore(t)

	want := DefaultDshHome()
	if want == "" {
		t.Skip("本机取不到家目录，默认值不可用")
	}
	if !filepath.IsAbs(want) {
		t.Fatalf("默认实例目录必须是绝对路径，实际 %q", want)
	}

	inst := &Instance{Name: "default-home"}
	if err := s.CreateInstance(inst); err != nil {
		t.Fatalf("实例目录留空应能建实例，实际 %v", err)
	}
	if inst.DshHome != want {
		t.Errorf("实例目录应为 %q，实际 %q", want, inst.DshHome)
	}
	// 读回来也必须是同一个值：默认值在读写两条路上不一致的话，管理台显示的
	// 与实际使用的会是两个目录。
	got, err := s.InstanceByID(inst.ID)
	if err != nil {
		t.Fatalf("读回实例失败: %v", err)
	}
	if got.DshHome != want {
		t.Errorf("读回的实例目录应为 %q，实际 %q", want, got.DshHome)
	}
	// 留空时工作目录也跟着落到 <默认实例目录>/workspace。
	if w := filepath.Join(want, "workspace"); got.Workspace != w {
		t.Errorf("工作目录应为 %q，实际 %q", w, got.Workspace)
	}

	// 显式指定时不得被默认值覆盖。
	explicit := &Instance{Name: "explicit-home", DshHome: "/srv/dsh/mine"}
	if err := s.CreateInstance(explicit); err != nil {
		t.Fatalf("显式指定实例目录应成功: %v", err)
	}
	if explicit.DshHome != "/srv/dsh/mine" {
		t.Errorf("显式指定的实例目录被改写成了 %q", explicit.DshHome)
	}
}

// TestUpdateInstanceHomeBlankFallsBack 验证改实例时把目录清空也回落到默认值。
func TestUpdateInstanceHomeBlankFallsBack(t *testing.T) {
	s := newStore(t)
	want := DefaultDshHome()
	if want == "" {
		t.Skip("本机取不到家目录，默认值不可用")
	}
	inst := mustCreateInstance(t, s, "clear-home", "/srv/dsh/before")

	inst.DshHome = ""
	if err := s.UpdateInstance(*inst); err != nil {
		t.Fatalf("清空实例目录应回落到默认值，实际报错 %v", err)
	}
	got, err := s.InstanceByID(inst.ID)
	if err != nil {
		t.Fatalf("读回实例失败: %v", err)
	}
	if got.DshHome != want {
		t.Errorf("清空后应为默认值 %q，实际 %q", want, got.DshHome)
	}
}

// TestOpenRecoversFromStaleShmIndex 验证留下一份过期的 WAL 索引时仍能打开库。
//
// 真实场景：重装时计划任务被 `schtasks /End` 强制结束，进程没走 sqlite 的正常
// 收尾，<db>-shm 留在磁盘上；新进程映射这份过期索引后，读有的表报 disk I/O error
// (266)，页面显示「服务内部错误」。Open 应当把它当缓存删掉重来。
func TestOpenRecoversFromStaleShmIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portal.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	mustCreateUser(t, first, "keeper", "secret-pass-123", RoleAdmin)
	if err := first.Close(); err != nil {
		t.Fatalf("关闭库失败: %v", err)
	}
	// 正常关闭时 sqlite 会删掉 -shm；这里故意留一份全是垃圾字节的索引，
	// 模拟被强制结束的进程留下的状态。
	junk := make([]byte, 32*1024)
	for i := range junk {
		junk[i] = 0xAA
	}
	shmPath := path + "-shm"
	if err := os.WriteFile(shmPath, junk, 0o600); err != nil {
		t.Fatalf("写入过期索引失败: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("有过期 WAL 索引时也应能打开库: %v", err)
	}
	u, err := s.UserByName("keeper")
	if err != nil {
		t.Fatalf("打开后读账号失败: %v", err)
	}
	if u.Name != "keeper" {
		t.Errorf("读回的账号应为 keeper，实际 %q", u.Name)
	}
	_ = s.Close()

	// 再把索引文件换成一个目录：第一次打开必然失败，只有「删掉过期索引再重来」
	// 这条自愈路径能救回来——这是真正被验证的代码路径。
	if err := os.Remove(shmPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("清理索引文件失败: %v", err)
	}
	if err := os.Mkdir(shmPath, 0o700); err != nil {
		t.Fatalf("占住索引路径失败: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("索引路径被占住时应能自愈并打开库: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if _, err := s2.UserByName("keeper"); err != nil {
		t.Fatalf("自愈后读账号失败: %v", err)
	}
}
