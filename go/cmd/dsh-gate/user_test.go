package main

import (
	"path/filepath"
	"testing"

	"github.com/wycto/dsh-gate/internal/users"
)

// TestUserPasswdUpdatesExistingAccount 走通改密主路径：旧口令失效、新口令生效。
func TestUserPasswdUpdatesExistingAccount(t *testing.T) {
	stateDir := t.TempDir()
	usersPath := filepath.Join(stateDir, "users.json")

	store, err := users.Open(usersPath)
	if err != nil {
		t.Fatalf("准备账号表失败: %v", err)
	}
	if err := store.Set("admin", "旧口令-abcd1234", users.DefaultIterations); err != nil {
		t.Fatalf("准备账号失败: %v", err)
	}

	swapStdin(t, "新口令-abcd1234\n")
	code := run([]string{"user", "passwd", "admin", "--password-stdin", "--state-dir", stateDir})
	if code != 0 {
		t.Fatalf("user passwd 退出码 = %d，期望 0", code)
	}

	after, err := users.Open(usersPath)
	if err != nil {
		t.Fatalf("重新打开账号表失败: %v", err)
	}
	if after.Verify("admin", "旧口令-abcd1234") {
		t.Error("改密后旧口令仍然有效")
	}
	if !after.Verify("admin", "新口令-abcd1234") {
		t.Error("改密后新口令没有生效")
	}
}

// TestUserPasswdRefusesMissingAccount 锁定 passwd 与 add 的语义边界：
// 改密只更新已有账号，用户名打错要当场失败，而不是悄悄建一个新号。
func TestUserPasswdRefusesMissingAccount(t *testing.T) {
	stateDir := t.TempDir()
	swapStdin(t, "口令-abcd1234\n")

	code := run([]string{"user", "passwd", "ghost", "--password-stdin", "--state-dir", stateDir})
	if code != 1 {
		t.Fatalf("user passwd 退出码 = %d，期望 1（运行失败）", code)
	}

	store, err := users.Open(filepath.Join(stateDir, "users.json"))
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}
	if store.Exists("ghost") {
		t.Error("passwd 为不存在的账号创建了记录")
	}
}
