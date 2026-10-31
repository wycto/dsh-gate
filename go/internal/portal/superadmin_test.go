package portal

import (
	"errors"
	"testing"
)

// TestSuperAdminIsFirstRegistered 锁住超管的判定：id 最小的账号，与角色无关。
func TestSuperAdminIsFirstRegistered(t *testing.T) {
	s := newStore(t)
	first := mustCreateUser(t, s, "alice", "password123", RoleUser) // 首位哪怕只是普通用户
	second := mustCreateUser(t, s, "admin2", "password123", RoleAdmin)

	id, err := s.SuperAdminID()
	if err != nil {
		t.Fatalf("SuperAdminID 失败: %v", err)
	}
	if id != first.ID {
		t.Errorf("超管应是第一个注册的 %s(id=%d)，得到 id=%d", first.Name, first.ID, id)
	}
	if !s.IsSuperAdmin("alice") {
		t.Error("alice 应被判定为超管")
	}
	if s.IsSuperAdmin(second.Name) {
		t.Error("后注册的管理员不该是超管")
	}
}

// TestSuperAdminProtectedOps 超管不可改角色、停用、删除；
// 改口令、展示名与登录名必须放行——忘了口令就把管理端锁死，
// 而默认账号名（admin）不合心意时改名是唯一安全的出路。
func TestSuperAdminProtectedOps(t *testing.T) {
	s := newStore(t)
	super := mustCreateUser(t, s, "root", "password123", RoleAdmin)
	mustCreateUser(t, s, "other", "password123", RoleAdmin) // 凑一个非最后管理员的场景

	if err := s.SetRole(super.Name, RoleUser); !errors.Is(err, ErrSuperAdmin) {
		t.Errorf("改超管角色应报 ErrSuperAdmin，得到 %v", err)
	}
	if err := s.SetDisabled(super.Name, true); !errors.Is(err, ErrSuperAdmin) {
		t.Errorf("停用超管应报 ErrSuperAdmin，得到 %v", err)
	}
	if err := s.DeleteUser(super.Name); !errors.Is(err, ErrSuperAdmin) {
		t.Errorf("删除超管应报 ErrSuperAdmin，得到 %v", err)
	}
	// 放行项：口令与展示名。
	if err := s.SetPassword(super.Name, "newpassword1"); err != nil {
		t.Errorf("超管改口令不该被拦: %v", err)
	}
	if err := s.SetDisplayName(super.Name, "初始账号"); err != nil {
		t.Errorf("超管改展示名不该被拦: %v", err)
	}
	// 别人不受影响：other 停用、改角色都正常。
	if err := s.SetDisabled("other", true); err != nil {
		t.Errorf("普通管理员停用不该被拦: %v", err)
	}
	if err := s.SetRole("other", RoleUser); err != nil {
		t.Errorf("普通管理员改角色不该被拦: %v", err)
	}
}

// TestSuperAdminRenameKeepsIdentity 锁住「超管可以改名，且改完还是超管」。
// 这是默认 admin 账号换成自己名字的正路：身份依据是 id，不是登录名。
func TestSuperAdminRenameKeepsIdentity(t *testing.T) {
	s := newStore(t)
	super := mustCreateUser(t, s, "admin", "password123", RoleAdmin)
	other := mustCreateUser(t, s, "bob", "password123", RoleUser)

	if err := s.RenameUser("admin", "weiyi"); err != nil {
		t.Fatalf("超管改名应放行，得到 %v", err)
	}
	if _, err := s.UserByName("admin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("旧名应已不可查，得到 %v", err)
	}
	u, err := s.UserByName("weiyi")
	if err != nil {
		t.Fatalf("新名查不到: %v", err)
	}
	if u.ID != super.ID {
		t.Errorf("改名不该换 id：期望 %d，得到 %d", super.ID, u.ID)
	}
	if !s.IsSuperAdmin("weiyi") {
		t.Error("改名后 weiyi 应仍是超级管理员")
	}
	if s.IsSuperAdmin("bob") {
		t.Error("bob 不该因超管改名而变成超管")
	}
	// 展示名创建时回退成旧登录名，改名要跟着走，否则页面还显示 admin。
	if u.DisplayName != "weiyi" {
		t.Errorf("展示名应跟着改成 weiyi，得到 %q", u.DisplayName)
	}
	// 口令不随改名丢失：还是原来那个。
	if _, err := s.Authenticate("weiyi", "password123"); err != nil {
		t.Errorf("改名后应能用新名登录，得到 %v", err)
	}
	// 撞名要拦。
	if err := s.RenameUser("weiyi", other.Name); err == nil {
		t.Error("改名撞到已存在的账号应报错")
	}
	// 非法新名要拦（中文登录名不允许）。
	if err := s.RenameUser("weiyi", "中文名"); err == nil {
		t.Error("非法登录名应报错")
	}
	// 被拦下的非法改名不能把展示名改坏。
	if cur, err := s.UserByName("weiyi"); err != nil || cur.DisplayName != "weiyi" {
		t.Errorf("失败的改名不该改坏记录，得到 %+v err=%v", cur, err)
	}
}

// TestRenameUserNonSuper 非超管改名照旧可用，且不会动到超管身份。
func TestRenameUserNonSuper(t *testing.T) {
	s := newStore(t)
	mustCreateUser(t, s, "admin", "password123", RoleAdmin)
	carol := mustCreateUser(t, s, "carol", "password123", RoleUser)

	if err := s.RenameUser("carol", "kate"); err != nil {
		t.Fatalf("普通用户改名应放行: %v", err)
	}
	if _, err := s.UserByName("kate"); err != nil {
		t.Errorf("改名后查不到 kate: %v", err)
	}
	if s.IsSuperAdmin("kate") {
		t.Error("普通用户改名后不该变成超管")
	}
	if !s.IsSuperAdmin("admin") {
		t.Error("超管身份不该因他人改名而漂移")
	}
	// 同名的原地改名是 no-op（幂等，不报错）。
	if err := s.RenameUser("admin", "admin"); err != nil {
		t.Errorf("原地改名应为无操作: %v", err)
	}
	if carol.Name != "carol" {
		t.Errorf("测试前置数据异常: %+v", carol)
	}
}

// TestSuperAdminSurvivesDeletionOfOthers 删掉别的账号不改变超管身份；
// 超管本身永远删不掉——它不存在了才谈得上「下一个成为超管」。
func TestSuperAdminSurvivesDeletionOfOthers(t *testing.T) {
	s := newStore(t)
	super := mustCreateUser(t, s, "root", "password123", RoleAdmin)
	a2 := mustCreateUser(t, s, "b", "password123", RoleAdmin)
	if err := s.DeleteUser(a2.Name); err != nil {
		t.Fatalf("删除第二个管理员失败: %v", err)
	}
	id, err := s.SuperAdminID()
	if err != nil || id != super.ID {
		t.Errorf("超管身份不该随他人删除而改变: id=%d err=%v", id, err)
	}
	// 只剩超管自己：依然删不掉（ErrSuperAdmin 先于 ErrLastAdmin 拦下）。
	if err := s.DeleteUser(super.Name); !errors.Is(err, ErrSuperAdmin) {
		t.Fatalf("超管删不掉，得到 %v", err)
	}
}
