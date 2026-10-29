// Command dsh-gate 是本机 dsh 的局域网门户：一个管理端 + 多个 dsh 实例。
//
// 一句话定位：dsh 只监听 127.0.0.1，dsh-gate 在局域网可达的端口上提供登录页
// 与管理台，把请求转发给各实例的 dsh，并负责实例的安装、启停与回收。
//
// 子命令：
//
//	dsh-gate install [选项]      安装管理端：建管理员账号，可选后台启动/开机自启
//	dsh-gate start [选项]        启动管理端（默认后台；--foreground 前台）
//	dsh-gate stop [选项]         停止管理端
//	dsh-gate restart [选项]      按保存的配置重启管理端
//	dsh-gate status [选项]       管理端运行状态 + 已登记实例
//	dsh-gate uninstall [选项]    卸载管理端（--service 取消自启，--purge 删状态目录）
//	dsh-gate dsh <list|add|edit|rename|start|stop|restart|rm|install>  管理 dsh 实例
//	dsh-gate manage <user|grant>  用户与授权
//	dsh-gate service <unit|install|uninstall|status>  开机自启（只托管管理端）
//	dsh-gate plugin / model       dsh 配置档与模型的辅助命令（--instance 指定实例）
//
// 退出码：0 成功，1 运行失败，2 参数用法错误。
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 是真正的入口，单独抽出来是为了能在测试里直接调用。
func run(argv []string) int {
	// 全局 --lang 先摘出来：它属于整个进程的输出语言，不属于某个子命令。
	explicitLang, argv := extractLang(argv)
	setCLILang(explicitLang)

	var command string
	var rest []string
	switch {
	case len(argv) == 0:
		printHelp(os.Stdout)
		return 0
	case isCommand(argv[0]):
		command, rest = argv[0], argv[1:]
	case argv[0] == "help" || argv[0] == "-h" || argv[0] == "--help":
		printHelp(os.Stdout)
		return 0
	case argv[0] == "version" || argv[0] == "-version" || argv[0] == "--version":
		fmt.Printf("dsh-gate %s\n", version.Version)
		return 0
	case strings.HasPrefix(argv[0], "-"):
		// 没有子命令、只给了选项：旧版本会把它当成 run。管理端单形态下没有
		// 「默认启动什么」可言，直接把用法摆出来，不猜。
		fmt.Fprintln(os.Stderr, "dsh-gate: 请先写子命令（例如 dsh-gate start 或 dsh-gate install）")
		printHelp(os.Stderr)
		return 2
	default:
		fmt.Fprintf(os.Stderr, "dsh-gate: %s\n\n", tr(i18n.KeyCLIUnknownCommand, argv[0]))
		printHelp(os.Stderr)
		return 2
	}

	switch command {
	case "install":
		return installCommand(rest)
	case "start":
		return startCommand(rest)
	case "stop":
		return stopCommand(rest)
	case "restart":
		return restartCommand(rest)
	case "status":
		return statusCommand(rest)
	case "uninstall":
		return uninstallCommand(rest)
	case "dsh":
		return dshCommand(rest)
	case "manage":
		return manageCommand(rest)
	case "service":
		return serviceCommand(rest)
	case "plugin":
		return pluginCommand(rest)
	case "model":
		return modelCommand(rest)
	case "version":
		fmt.Printf("dsh-gate %s\n", version.Version)
		return 0
	}
	return 2
}

// isCommand 判断参数是不是一个顶层子命令。
func isCommand(name string) bool {
	switch name {
	case "install", "start", "stop", "restart", "status", "uninstall",
		"dsh", "manage", "service", "plugin", "model", "version":
		return true
	}
	return false
}

// printHelp 打印总帮助。
func printHelp(w *os.File) {
	fmt.Fprint(w, `dsh-gate —— 把本机的 dsh 变成局域网可访问的多实例门户

  dsh 自带的 Web UI 只监听 127.0.0.1。dsh-gate 起一个管理端：登录页、管理台、
  多实例反向代理；dsh 实例由管理端安装并按需拉起，每个实例有自己的 DSH_HOME。

管理端:
  dsh-gate install [选项]        安装管理端：建管理员账号（--start 立即后台启动，--service 开机自启）
  dsh-gate start [选项]          启动管理端（默认后台；--foreground 前台运行）
  dsh-gate stop [选项]           停止管理端
  dsh-gate restart [选项]        按保存的配置重启管理端
  dsh-gate status [选项]         管理端运行状态；顺带列出已登记的 dsh 实例
  dsh-gate uninstall [选项]      卸载管理端（--service 取消自启，--purge 删账号库与状态目录）
  dsh-gate service <unit|install|uninstall|status>  把管理端交给系统服务管理器

dsh 实例（与 /admin 页面能力对等）:
  dsh-gate dsh list                                        列出实例
  dsh-gate dsh add <名字> --dsh-home <目录> [--dsh-port ...]  新增
  dsh-gate dsh edit <名字> [字段选项]                        修改（只改显式给出的字段）
  dsh-gate dsh rename <旧名> <新名>                          改名（id、授权、会话不变）
  dsh-gate dsh start|stop|restart <名字>                     启停单个实例
  dsh-gate dsh install <名字> [--install-mode npx|git|local|pnpm]   安装/重装 dsh
  dsh-gate dsh rm <名字> [--purge] [--purge-install]         删除实例（可连带删数据/安装目录）

用户与授权:
  dsh-gate manage user add|passwd|role|disable|enable|list|rm <用户名>
  dsh-gate manage grant add|rm <用户名> <实例名或 id>
  dsh-gate manage grant list [用户名]

辅助（模型与插件都按实例管理，--instance 指定实例）:
  dsh-gate plugin <list|add> --instance <实例>                 管理某个实例的插件
  dsh-gate model <list|key|provider|default> --instance <实例>  配某个实例的模型与密钥
  dsh-gate version                                            打印版本号

一条命令用起来:
  $ dsh-gate install --start        # 建管理员、后台启动管理端
  # 打开提示里的 http://<本机>:12020/admin 创建实例并安装 dsh

文件放在哪:
  管理端数据库   ~/.dsh-gate/portal.db   （账号、实例、授权、审计）
  本机控制令牌   ~/.dsh-gate/portal.token（dsh 子命令用；每次启动刷新）
  dsh 实例数据   各实例自己的 DSH_HOME（默认 ~/.dsh；删实例时可选是否删除）

选项说明:
  dsh-gate install --help
  dsh-gate start --help
  dsh-gate dsh --help
  dsh-gate service --help
`)
}

// setupLogger 配置全局日志。
//
// 默认级别是 info：dsh 的启动输出走 debug，跑网关本身的信息（接管、启动、
// 登录、转发失败）走 info。日志一律写 stderr，stdout 留给命令的正经输出，
// 这样 `dsh-gate check > report.txt` 得到的是干净的结果而不是混着日志。
func setupLogger(level string, asJSON bool) error {
	var parsed slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "", "info":
		parsed = slog.LevelInfo
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		return fmt.Errorf("无法识别的日志级别 %q，可用：debug / info / warn / error", level)
	}

	options := &slog.HandlerOptions{Level: parsed}
	var handler slog.Handler
	if asJSON {
		handler = slog.NewJSONHandler(os.Stderr, options)
	} else {
		handler = slog.NewTextHandler(os.Stderr, options)
	}
	slog.SetDefault(slog.New(handler))
	return nil
}

// fatal 打印错误并返回退出码。
func fatal(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "dsh-gate: "+format+"\n", args...)
	return 1
}

// usageError 打印用法错误。
func usageError(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "dsh-gate: "+format+"\n", args...)
	return 2
}
