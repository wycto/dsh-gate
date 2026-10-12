package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/wycto/dsh-gate/internal/config"
)

// rootWriteGuard 拦下「以 root（sudo）身份把数据写进别人的目录」。
//
// 为什么需要它：为了把命令装进 /usr/local/bin，用户很自然会
// `sudo dsh-gate setup`（我们自己的 README/QUICKSTART 以前也正是这么写的）。
// 但 macOS 的 sudo 会保留 $HOME，于是 `~/.dsh` 仍然展开成**用户自己的**实例目录，
// 而写进去的 settings.yaml / .credentials.yaml 属于 root（0600）。DSH 桌面端
// 以那个用户身份运行，从此读不了也改不动自己的配置——现场就留下过
// `settings.yaml.root-owned-<时间戳>` 这种残骸，只能手工 chown 收拾。
//
// 判定用的是「目标目录（或它最近的已存在祖先）的属主 == 发起 sudo 的用户」：
// 这才是真正危险的那件事——root 往别人的目录里写。以 root 正常跑的服务器
// （--dsh-home /var/lib/dsh 之类，属主本来就是 root）不受影响。
type rootWriteGuard struct {
	// EUID 是当前有效用户 id（sudo 下是 0）。
	EUID int
	// InvokingUID / InvokingName 是发起 sudo 的那个用户（SUDO_UID / SUDO_USER）。
	InvokingUID  string
	InvokingName string
	// Allowed 对应 --allow-root-write：用户明确表示「我知道后果」。
	Allowed bool
}

// newRootWriteGuard 从当前进程取一次环境快照。
func newRootWriteGuard(allow bool) rootWriteGuard {
	return rootWriteGuard{
		EUID:         os.Geteuid(),
		InvokingUID:  os.Getenv("SUDO_UID"),
		InvokingName: os.Getenv("SUDO_USER"),
		Allowed:      allow,
	}
}

// check 在写任何东西之前检查一个目录，返回给人看的错误；可以写就返回 nil。
//
// 目录还不存在时沿途向上找最近的已存在祖先：`sudo setup` 面对一个还没建出来的
// `~/.dsh` 时，照样要能认出「这是别人的家目录」。
func (g rootWriteGuard) check(label, dir string) error {
	if g.Allowed || g.EUID != 0 || strings.TrimSpace(dir) == "" {
		return nil
	}
	uid, err := strconv.Atoi(strings.TrimSpace(g.InvokingUID))
	if err != nil || uid == 0 {
		// 没有 SUDO_UID（或它指向 root）：这台机器上本来就只有 root 一份数据，
		// 不存在「写坏别人的目录」这回事。
		return nil
	}
	owner, ok := dirOwnerUID(dir)
	if !ok || owner != uid {
		return nil
	}

	who := strings.TrimSpace(g.InvokingName)
	if who == "" {
		who = fmt.Sprintf("uid %d", uid)
	}
	return fmt.Errorf(`%s %s 属于用户 %s，而当前是以 root（sudo）运行。

以 root 写进去的 settings.yaml 与 .credentials.yaml 会变成 root 属主（0600），
%s 自己的 dsh（包括 DSH 桌面端）从此读不了也改不动，得手工 chown 才能恢复。

两条安全的做法：
  ① 去掉 sudo，以 %s 自己的身份重跑（macOS 装开机自启并不需要 root）
  ② 只是想把命令装进 PATH：先 sudo install -m755 dsh-gate /usr/local/bin/，
     再以自己身份跑 dsh-gate setup

确实要让 root 拥有这些数据时，加 --allow-root-write 明确表示知情。`,
		label, dir, who, who, who)
}

// guardRootWrites 对一份配置里的关键目录逐个检查，拦下时返回非零退出码。
//
// 检查放在「建目录、建账号表」之前：先把危险说出来，而不是先动完手再报错。
func guardRootWrites(configs []config.Instance, allow bool) int {
	guard := newRootWriteGuard(allow)
	for _, cfg := range configs {
		items := []struct{ label, dir string }{
			{"dsh 实例目录", cfg.DSHHome},
			{"网关状态目录", cfg.StateDirPath()},
		}
		for _, item := range items {
			if err := guard.check(item.label, item.dir); err != nil {
				return fatal("%v", err)
			}
		}
	}
	return 0
}
