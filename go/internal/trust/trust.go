// Package trust 把本地 CA 装进操作系统的信任库。
//
// 为什么必须做这一步：浏览器只对「证书链能验证通过」的 HTTPS 站点开放麦克风
// 这类敏感能力。只点「继续访问」跳过警告时，页面虽然能打开，但安全上下文
// 的判定并不稳定——同一台机器上，装进信任库之后才真正可靠。
//
// 边界很重要：这个包只做「尝试」，不把失败当成安装失败。用户可能在无图形界面
// 的服务器上、可能没有管理员权限、也可能用的浏览器有自己独立的信任库。因此
// 每个平台都返回一段**能照着敲的命令**，让用户自己决定要不要执行——这比
// 替用户静默改系统状态更符合「开源项目装到别人机器上」的分寸。
package trust

import (
	"fmt"
	"runtime"
	"strings"
)

// Status 是一次信任安装尝试的结果。
type Status struct {
	// Attempted 表示确实尝试过（平台受支持、且用户同意了）。
	Attempted bool
	// Trusted 表示命令执行成功。
	Trusted bool
	// Message 是给人看的说明，失败时要能照着做。
	Message string
}

// Options 是一次信任安装的输入。
type Options struct {
	// CACert 是要导入信任库的 CA 证书路径。
	CACert string
	// StateDir 用于在 Linux 上按发行版挑选导入方式（如是否需要 sudo）。
	StateDir string
}

// Install 尝试把 CA 装进当前平台的信任库。
//
// 各平台的实现放在带 build tag 的文件里（trust_darwin.go / trust_linux.go /
// trust_windows.go / trust_other.go）：它们的差别不是「命令名不同」，而是
// 信任模型完全不同——macOS 走钥匙串、Linux 走发行版各自的 CA 目录、Windows
// 走证书存储区。硬凑成一个通用实现只会让每一处都别扭。
func Install(opts Options) Status {
	if strings.TrimSpace(opts.CACert) == "" {
		return Status{Message: "没有给出 CA 证书路径"}
	}
	return install(opts)
}

// ManualCommand 返回在指定平台手动装信任时应当执行的命令。
//
// 无论自动安装成功与否，都会把它打印给用户：成功了是一份可复核的记录，
// 失败了是能直接照抄的补救方式。平台用 runtime.GOOS 的取值。
func ManualCommand(goos, caPath string) string {
	switch goos {
	case "darwin":
		// macOS 用 login 钥匙串（不需要 sudo），并把信任策略设为「始终信任」。
		// 不加 -r trustRoot 的话，用户仍会在第一次连接时被问一次。
		return fmt.Sprintf(
			`security add-trusted-cert -r trustRoot -k "$HOME/Library/Keychains/login.keychain-db" %s`,
			shellQuote(caPath))
	case "windows":
		// certutil 是 Windows 自带的，-addstore -f 表示强制写入当前用户存储。
		return fmt.Sprintf(`certutil -addstore -f "ROOT" %s`, shellQuote(caPath))
	default:
		// 大多数 Linux 发行版都能用 update-ca-certificates；RHEL 系另有命令，
		// 见 linuxHint。
		return fmt.Sprintf(
			"sudo cp %s /usr/local/share/ca-certificates/dsh-gate-ca.crt && sudo update-ca-certificates",
			shellQuote(caPath))
	}
}

// BrowserHint 返回「装完系统信任之后还要做什么」的提示。
//
// 这一步最容易被漏掉：Firefox 在多数平台用自己独立的信任库，装进系统信任
// 对它无效；而所有浏览器都要求重启一次才重新读信任库。
func BrowserHint() string {
	switch runtime.GOOS {
	case "windows":
		return "装好后请重启浏览器（Windows 的 Edge / Chrome 用系统证书存储，重启后即生效）。"
	case "darwin":
		return "装好后请重启浏览器；Safari 与 Chrome / Edge 用系统钥匙串，重启后即生效。"
	default:
		return "装好后请重启浏览器；若用 Firefox，它有自己的信任库，需要在其设置里单独导入这个 CA。"
	}
}

// shellQuote 给路径加引号，避免空格与特殊字符把命令拆散。
//
// 用单引号并转义内部的单引号：这是 POSIX shell 里唯一稳妥的写法，双引号里
// 的 $ 与反引号仍会被展开。
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
