// Command dsh-gate 让局域网里的任意设备使用你的 DSH。
//
// 一句话定位：dsh 只监听 127.0.0.1，dsh-gate 在局域网可达的端口上做一层
// 带登录页的反向代理，需要时还会顺手把 dsh 一起拉起来。
//
// 子命令：
//
//	dsh-gate setup [选项]       引导式初始化：建账号，可顺带启动与自启
//	dsh-gate uninstall <实例名|all>  停掉指定实例；--service/--purge 另外两步
//	dsh-gate plugin <list|add>  管理 dsh 配置档里的插件（自带 Node/pnpm 与换源）
//	dsh-gate model <list|key|provider|default>  配模型（dsh-dock 装不上时的出路）
//	dsh-gate run [选项]         启动网关
//	dsh-gate status [选项]      列出本机正在运行的实例
//	dsh-gate stop [实例名...]   停掉实例
//	dsh-gate restart [实例名...] 重启实例
//	dsh-gate user add|passwd|list|rm  管理登录账号
//	dsh-gate check [选项]       体检：端口占用、dsh 状态、访问地址
//	dsh-gate service unit       生成 systemd 服务单元（Linux）
//	dsh-gate version            打印版本（--version 同义）
//
// 退出码：0 成功，1 运行失败，2 参数用法错误。
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/wycto/dsh-gate/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 是真正的入口，单独抽出来是为了能在测试里直接调用。
func run(argv []string) int {
	command := "run"
	rest := argv
	switch {
	// 光敲一个 `dsh-gate` 就把用法摆出来，而不是替用户猜一个端口开始监听。
	// 起服务是有副作用的（占用端口、拉起 dsh），值得让人先看一眼要干什么。
	case len(argv) == 0:
		printHelp(os.Stdout)
		return 0
	case argv[0] == "run" || argv[0] == "stop" || argv[0] == "restart" ||
		argv[0] == "status" || argv[0] == "user" || argv[0] == "check" ||
		argv[0] == "service" || argv[0] == "version" || argv[0] == "setup" ||
		argv[0] == "uninstall" || argv[0] == "plugin" || argv[0] == "model":
		command, rest = argv[0], argv[1:]
	case argv[0] == "help" || argv[0] == "-h" || argv[0] == "--help":
		printHelp(os.Stdout)
		return 0
	case argv[0] == "version" || argv[0] == "-version" || argv[0] == "--version":
		// 放在「带横线就当 run」那条规则之前，否则 --version 会被当成
		// run 的选项，报一句莫名其妙的「flag provided but not defined」。
		fmt.Printf("dsh-gate %s\n", version.Version)
		return 0
	case strings.HasPrefix(argv[0], "-"):
		// 带选项但没写子命令时仍按 run 处理：`dsh-gate --port 8080` 这类
		// 「我就是想改个端口启动」的写法不必再补一个 run。
		command, rest = "run", argv
	default:
		fmt.Fprintf(os.Stderr, "dsh-gate: 未知子命令 %q\n\n", argv[0])
		printHelp(os.Stderr)
		return 2
	}

	switch command {
	case "run":
		return runCommand(rest)
	case "setup":
		return setupCommand(rest)
	case "uninstall":
		return uninstallCommand(rest)
	case "plugin":
		return pluginCommand(rest)
	case "model":
		return modelCommand(rest)
	case "stop":
		return stopCommand(rest)
	case "restart":
		return restartCommand(rest)
	case "status":
		return statusCommand(rest)
	case "user":
		return userCommand(rest)
	case "check":
		return checkCommand(rest)
	case "service":
		return serviceCommand(rest)
	case "version":
		fmt.Printf("dsh-gate %s\n", version.Version)
		return 0
	}
	return 2
}

// printHelp 打印总帮助。
func printHelp(w *os.File) {
	fmt.Fprint(w, `dsh-gate —— 让局域网里的任意设备使用你的 DSH

  dsh 自带的 Web UI 只监听 127.0.0.1，别的电脑访问不了。dsh-gate 在一个
  局域网可达的端口上提供登录页，并把请求转发给本机的 dsh；dsh 还没起来的话，
  它会顺手把 dsh 拉起来。

用法:
  dsh-gate setup [选项]          引导式初始化：建账号，可顺带启动与自启
  dsh-gate run [选项]            启动网关
  dsh-gate status [选项]         列出本机正在运行的实例（名字、端口、各自目录）
  dsh-gate stop <实例名|all>    停掉指定实例（必须写明目标）
  dsh-gate restart <实例名|all> 按原参数重启指定实例
  dsh-gate user add <用户名>     添加或修改账号（交互输入口令）
  dsh-gate user passwd <用户名>  修改已有账号的口令（改完立即生效）
  dsh-gate user list             列出账号
  dsh-gate user rm <用户名>      删除账号
  dsh-gate check [选项]          体检：端口占用、dsh 状态、访问地址
  dsh-gate service install       设为开机（登录）自启，并立即启动（默认把命令拷进 /usr/local/bin）
  dsh-gate service uninstall     只取消开机自启（机器级的一个服务单元）
  dsh-gate service status        查看托管状态
  dsh-gate service unit          打印将要安装的配置内容
  dsh-gate uninstall <实例名|all>  停掉指定实例；--service 取消自启、--purge 删数据
  dsh-gate plugin list           查看 dsh 配置档里已装的插件
  dsh-gate plugin add dsh-dock   装插件（自带 pnpm 与镜像源，服务器上也能装）
  dsh-gate model list             看当前服务商、密钥与默认模型
  dsh-gate model key set DEEPSEEK_API_KEY --stdin   配官方 DeepSeek 密钥
  dsh-gate model default deepseek-official deepseek-chat   设默认模型
  dsh-gate version               打印版本号

一条命令就能用起来:
  $ dsh-gate setup               # 建账号，按需顺带启动与设自启
或者分步来:
  $ dsh-gate user add admin      # 1. 创建一个登录账号
  $ dsh-gate run                 # 2. 启动网关，按提示在浏览器登录
  $ dsh-gate check               # 3. 随时确认端口与访问地址

看状态、停止、重启:
  dsh-gate status              现在跑着哪几个实例、分别在哪个目录
  dsh-gate stop personal       停掉 personal；写 all 才是全部，不写只列清单
  dsh-gate restart personal    按原参数重启 personal（后台运行，日志在
                               ~/.dsh-gate/run/logs/<实例名>.log）
  dsh-gate run --background    关掉终端也要继续跑时用（也可写作 -d）
  dsh-gate service install     设为开机（登录）自启，三个平台都支持
  Ctrl-C                      前台运行时直接停，走的是同一条收尾流程

不再用了:
  dsh-gate uninstall test      停掉 test 并注销它的登记（数据默认留着）
  dsh-gate uninstall all --service --purge   整机收尾：停全部 + 取消自启 + 删数据

文件放在哪:
  dsh 的数据     ~/.dsh                      （会话、插件、配置；--dsh-home 可改）
  网关自己的数据 ~/.dsh-gate                  （账号表；--state-dir 可改）
  自备 Node.js   ~/.dsh-gate/node             （本机没有 Node.js 时自动下载，随 --purge 删除）
  两者刻意分开：备份、清理、迁移时互不牵扯。

第一次运行:
  本机没有 Node.js 也不用手工装：网关会把官方 Node.js 运行时下载到状态目录
  （不改系统、不需要 root），再用它拉起 dsh。dsh 首次启动要下插件，慢一点正常。
  用 setup 或 service install 装过一次之后，dsh-gate 命令就在 PATH 里了。

典型用法:
  # 家里一台 macOS，既跑个人 dsh，又代理公司的 dsh
  dsh-gate run --name personal --port 3082 --dsh-port 3080 --dsh-home ~/.dsh
  dsh-gate run --name company   --port 8080 --dsh-port 8081 --dsh-home ~/.dsh-company

  # 无界面 Linux 服务器，开机自启
  sudo dsh-gate service install --dsh-home /var/lib/dsh

选项说明:
  dsh-gate run --help
  dsh-gate user --help
  dsh-gate check --help
  dsh-gate status --help
  dsh-gate restart --help

文档与示例见仓库根目录 README.md。
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
