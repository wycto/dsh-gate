//go:build darwin

package trust

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// install 在 macOS 上把 CA 加进登录钥匙串并设为始终信任。
//
// 用 login 钥匙串而不是系统钥匙串：后者要 sudo，而登录钥匙串只属于当前用户，
// 不需要提权就能改。install 通常就是普通用户跑的，不该为了装个证书要求 root。
//
// 命令里的 -r trustRoot 是关键：不加它，证书只是「在钥匙串里」，连接时浏览器
// 仍会弹一次警告；加了才是「始终信任」。
func install(opts Options) Status {
	if _, err := exec.LookPath("security"); err != nil {
		return Status{
			Attempted: true,
			Message:   "找不到 macOS 自带的 security 命令，请手动执行下面这条命令。",
		}
	}

	keychain := userKeychain()
	args := []string{
		"add-trusted-cert",
		"-r", "trustRoot",
		"-k", keychain,
		opts.CACert,
	}
	cmd := exec.Command("security", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return Status{
			Attempted: true,
			Message:   trimOutput(output, err),
		}
	}
	return Status{
		Attempted: true,
		Trusted:   true,
		Message:   "已加入钥匙串并设为始终信任。",
	}
}

// userKeychain 返回当前用户的登录钥匙串路径。
//
// 不写死 ~/Library/Keychains/login.keychain-db：有些用户改过钥匙串位置，
// 而 security 的 -k 参数要的是一个真实存在的文件。取不到时退回默认路径，
// 那条路径在绝大多数机器上都是对的。
func userKeychain() string {
	if out, err := exec.Command("security", "default-keychain", "-d", "user").Output(); err == nil {
		if path := strings.Trim(strings.TrimSpace(string(out)), `"`); path != "" {
			return path
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "login.keychain-db"
	}
	return filepath.Join(home, "Library", "Keychains", "login.keychain-db")
}

// trimOutput 把命令输出压成一行给人看的说明。
func trimOutput(output []byte, err error) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return err.Error()
	}
	// 只取最后一行：security 的报错前面往往还有一串无关的输出。
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[i+1:])
	}
	return text
}
