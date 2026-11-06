package main

// 这些是「归属用户解析」的纯函数测试：macOS 与 Linux 共用同一份实现，
// 所以放在无 build tag 的文件里，两个平台都能跑到。

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// TestLaunchingUserUnderSudo 用 sudo 跑时，自启要装给**发起 sudo 的用户**。
//
// 这是用户实际踩到的坑：他按我们自己的提示 sudo dsh-gate install（为了把命令
// 装进 /usr/local/bin），结果 euid 变成 0，launchd domain 成了不存在的 gui/0
// （bootstrap 报 125），Linux 上则把单元写成了 root。两处都必须解析出真实用户。
func TestLaunchingUserUnderSudo(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("拿不到当前用户")
	}

	// 核心场景：sudo 跑（euid=0）但 SUDO_UID 指向登录用户 → uid 必须是真实用户。
	uid, home, elevated := resolveLaunchingUser(0, current.Uid, current.Username)
	if uid != current.Uid {
		t.Errorf("sudo 下 uid = %q，期望发起 sudo 的用户 %q（否则 domain 会是不存在的 gui/0）", uid, current.Uid)
	}
	if home != current.HomeDir {
		t.Errorf("sudo 下家目录 = %q，期望 %q", home, current.HomeDir)
	}
	if !elevated {
		t.Error("sudo 下应当报告为 elevated，好给出提示")
	}

	// root 且没有 SUDO_UID：只能给 uid 0，由调用方拦住并说明。
	uid, _, elevated = resolveLaunchingUser(0, "", "")
	if uid != "0" || !elevated {
		t.Errorf("root 无 SUDO_UID 时 = %q/%v，期望 0/true", uid, elevated)
	}

	// 普通用户：用自己的 uid 与家目录，且不报 elevated。
	uid, home, elevated = resolveLaunchingUser(501, current.Uid, current.Username)
	if uid != "501" || elevated {
		t.Errorf("普通用户 = %q/%v，期望 501/false", uid, elevated)
	}
	if home == "" {
		t.Error("普通用户的家目录不该为空")
	}

	// SUDO_UID 带空格也要能用（有些环境的 sudo 配置会带进来）。
	if uid, _, _ := resolveLaunchingUser(0, " "+current.Uid+" ", current.Username); uid != current.Uid {
		t.Errorf("SUDO_UID 带空格时应被清理，实际 %q", uid)
	}
}

// TestHomeOfUserFallsBack 查不到用户时退回当前进程的家目录，而不是空字符串
// （空会被拼成相对路径，单元/plist 就写到奇怪的地方去了）。
func TestHomeOfUserFallsBack(t *testing.T) {
	if got := homeOfUser("这个用户肯定不存在-zzz"); got == "" {
		t.Error("查不到用户时应当退回当前家目录，而不是空")
	}
	if got := homeOfUser(""); got == "" {
		t.Error("空用户名时应当退回当前家目录")
	}
}

// TestEffectiveInstallTarget 覆盖三态：sudo → 真实用户/home/gid；
// root 裸登录 → root、不 elevated；普通用户 → 自己、不 elevated。
func TestEffectiveInstallTarget(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("拿不到当前用户")
	}

	// sudo：euid=0 且有 SUDO_USER
	gotUser, gotHome, gotGID, elevated := effectiveInstallTarget(0, current.Uid, current.Username)
	if !elevated {
		t.Fatal("sudo 场景应当 elevatedIntoUser=true")
	}
	if gotUser != current.Username {
		t.Errorf("sudo 场景用户 = %q，期望 %q", gotUser, current.Username)
	}
	if gotHome != current.HomeDir {
		t.Errorf("sudo 场景家目录 = %q，期望 %q", gotHome, current.HomeDir)
	}
	if gotGID != current.Gid {
		t.Errorf("sudo 场景 gid = %q，期望 %q", gotGID, current.Gid)
	}

	// root 裸登录：SUDO_USER 为空
	gotUser, _, _, elevated = effectiveInstallTarget(0, "", "")
	if elevated {
		t.Error("root 裸登录不该 elevatedIntoUser")
	}
	if gotUser != "root" {
		t.Errorf("root 裸登录用户 = %q，期望 root", gotUser)
	}

	// 普通用户
	gotUser, gotHome, _, elevated = effectiveInstallTarget(501, current.Uid, current.Username)
	if elevated {
		t.Error("普通用户不该 elevatedIntoUser")
	}
	if gotUser == "" || gotHome == "" {
		t.Errorf("普通用户的用户/家目录不该为空：%q / %q", gotUser, gotHome)
	}
}

// TestChownTree 在临时目录上把属主改成当前用户自己（等价于改成功），
// 断言整棵子树都遍历到、没有失败项。跨 uid 的情形需要 root，这里不测。
func TestChownTree(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("拿不到当前用户")
	}
	uid, ok1 := parseID(current.Uid)
	gid, ok2 := parseID(current.Gid)
	if !ok1 || !ok2 {
		t.Skip("uid/gid 不是数字")
	}

	root := t.TempDir()
	sub := filepath.Join(root, "tls")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	key := filepath.Join(sub, "server.key")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	if failed := chownTree(root, uid, gid); len(failed) != 0 {
		t.Fatalf("chownTree 到自身应全部成功，失败项: %v", failed)
	}
	if info, err := os.Stat(key); err == nil {
		if info.Mode().Perm() != 0o600 {
			t.Errorf("chown 不该改变权限，私钥现在是 %o", info.Mode().Perm())
		}
	}
	if !chownPath(key, uid, gid) {
		t.Error("chownPath 到自身应成功")
	}
}
