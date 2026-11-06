package main

// 本文件是「这次操作到底属于哪个用户」的共享实现：Linux / macOS 都用到它。
//
// 为什么不能只看 os.Getuid()：用户为了把 dsh-gate 装进 /usr/local/bin 很自然地
// 会用 sudo 跑安装（旧提示里也这么建议），那时 euid 是 0，真正的「人」是发起
// sudo 的那个用户。macOS 的 launchd 必须装上 gui/<uid>（gui/0 不存在，bootstrap
// 直接失败）；Linux 的 systemd 单元则要写 User=<真实用户>，否则 dsh 以 root 运行，
// 读写用户家目录里的实例数据会遇到权限问题。两个平台都从 SUDO_UID / SUDO_USER
// 解析真实用户。

import (
	"fmt"
	"os"
	"os/user"
	"strings"
)

// launchingUser 返回「这次操作真正属于哪个用户」。
//
// 返回的 elevated 表示「当前是以提权身份（euid=0）在跑」：调用方据此决定要不要
// 提醒用户、或按真实用户去装服务单元。
func launchingUser() (uid string, home string, elevated bool) {
	return resolveLaunchingUser(os.Geteuid(), os.Getenv("SUDO_UID"), os.Getenv("SUDO_USER"))
}

// resolveLaunchingUser 是 launchingUser 的纯函数版本：把 euid 与 SUDO_* 摊成参数，
// 这样 sudo 与 root 两个分支都能在测试里覆盖到——否则它们只有在真机上以 root
// 跑一次才走得到，而「以 root 跑」恰恰是最容易出问题、又最难复现的场景。
func resolveLaunchingUser(euid int, sudoUID, sudoUser string) (uid string, home string, elevated bool) {
	if euid == 0 {
		if trimmed := strings.TrimSpace(sudoUID); trimmed != "" {
			return trimmed, homeOfUser(sudoUser), true
		}
		// 没有 SUDO_UID：真的以 root 登录（不做图形登录的场景）。这时没有可用的
		// GUI domain，由调用方给出说明。
		return "0", homeOfUser(""), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return fmt.Sprintf("%d", euid), home, false
}

// homeOfUser 查某个用户的家目录；查不到时退回当前进程的家目录。
func homeOfUser(name string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		if account, err := user.Lookup(name); err == nil && account.HomeDir != "" {
			return account.HomeDir
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// lookupUserIdentity 查用户的 uid / gid / 家目录（都是字符串，查不到返回空）。
//
// 用字符串是为了和 user.User 的字段口径一致；需要整数（chown）时由调用方用
// parseID 转。gid 为空表示查不到，chown 时按「不改所属组」处理。
func lookupUserIdentity(name string) (uid, gid, home string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", ""
	}
	if account, err := user.Lookup(name); err == nil {
		return account.Uid, account.Gid, account.HomeDir
	}
	return "", "", ""
}

// effectiveInstallTarget 决定这次安装「归属哪个用户」——即 sudo 安装时，安装期
// 创建的文件该 chown 给谁、systemd 单元的 User= 该写谁。
//
// 纯函数，便于测试三态：
//   - sudo 安装（euid=0 且 SUDO_USER 指向真实用户）：返回该用户、其家目录与 gid，
//     elevatedIntoUser=true；
//   - 真的是 root 登录（euid=0 且没有 SUDO_USER）：退回 root，elevatedIntoUser=false
//     （调用方应 warn，但不做任何 chown/换用户）；
//   - 普通用户直接运行（euid!=0）：返回自己，elevatedIntoUser=false（行为不变）。
func effectiveInstallTarget(euid int, sudoUID, sudoUser string) (userOut, home, gid string, elevatedIntoUser bool) {
	if euid == 0 {
		if name := strings.TrimSpace(sudoUser); name != "" {
			_, g, h := lookupUserIdentity(name)
			if h == "" {
				h = homeOfUser(name)
			}
			return name, h, g, true
		}
		// 真 root 登录：保持现状（以 root 跑），由调用方给出警告。
		_, _, h := lookupUserIdentity("root")
		if h == "" {
			h = homeOfUser("root")
		}
		return "root", h, "", false
	}
	name := currentUser()
	if name == "" {
		// 退化成 uid 字符串，与 resolveLaunchingUser 的普通用户分支一致。
		name = fmt.Sprintf("%d", euid)
	}
	_, g, h := lookupUserIdentity(name)
	if h == "" {
		h = homeOfUser("")
	}
	return name, h, g, false
}
