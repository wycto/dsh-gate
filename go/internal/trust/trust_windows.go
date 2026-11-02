//go:build windows

package trust

import (
	"os/exec"
	"strings"
)

// install 在 Windows 上把 CA 加进当前用户的「受信任的根证书颁发机构」存储区。
//
// 用 -addstore "ROOT" 而不是本机存储（LocalMachine）：后者要管理员权限，而
// 当前用户存储只影响这个账号，install 用普通用户跑就能生效，也符合「装到自己
// 账号下」的预期。
func install(opts Options) Status {
	if _, err := exec.LookPath("certutil"); err != nil {
		return Status{
			Attempted: true,
			Message:   "找不到 Windows 自带的 certutil，请手动导入这个 CA 证书。",
		}
	}

	cmd := exec.Command("certutil", "-addstore", "-f", "ROOT", opts.CACert)
	output, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(output))
		if text == "" {
			text = err.Error()
		}
		return Status{Attempted: true, Message: text}
	}
	return Status{
		Attempted: true,
		Trusted:   true,
		Message:   "已加入当前用户的受信任根证书存储区。",
	}
}
