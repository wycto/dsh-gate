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

// TestSuperAdminProtectedOps 超管不可改角色、停用、改名、删除；
// 改口令与展示名必须放行（否则忘了口令就把管理端锁死）。
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
	if err := s.RenameUser(super.Name, "boss"); !errors.Is(err, ErrSuperAdmin) {
		t.Errorf("改超管登录名应报 ErrSuperAdmin，得到 %v", err)
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
	// 别人不受影响：other 停用、删除都正常。
	if err := s.SetDisabled("other", true); err != nil {
		t.Errorf("普通管理员停用不该被拦: %v", err)
	}
	if err := s.SetRole("other", RoleUser); err != nil {
		t.Errorf("普通管理员改角色不该被拦: %v", err)
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
