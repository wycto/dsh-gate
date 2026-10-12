package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
)

// stringList 是一个可以重复出现的字符串选项。
//
// 用来实现 `--trusted-host a --trusted-host b`：flag 包的 StringVar 只会保留
// 最后一次出现的值，而 dsh 的 --trusted-host 恰恰需要多个值，所以这里自己实现
// 一个 flag.Value。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// instanceFlags 持有需要特殊处理的选项值。
type instanceFlags struct {
	trustedHosts stringList
}

// registerInstanceFlags 注册所有对应到 config.Instance 的命令行选项。
//
// 选项名与 config.Apply 里接受的键一一对应，两个函数必须同步改——这正是把
// 它们写成两处手写表的原因：新增选项时编译器不会提醒，但 grep "trusted-host"
// 一定能找到。
//
// 默认值全部取自 config.Default()，这样 `--help` 打出来的就是真实默认值，
// 而不是另一份需要同步的数字。
func registerInstanceFlags(fs *flag.FlagSet) *instanceFlags {
	hold := &instanceFlags{}
	def := config.Default()

	fs.String("name", def.Name, "实例名（多实例运行时用于区分与日志前缀）")
	fs.String("host", def.Host, "网关监听地址，如 0.0.0.0 表示接受局域网所有设备的连接")
	fs.Int("port", def.Port, "网关监听端口（对外访问用的端口）")

	fs.String("dsh-host", def.DSHHost, "dsh 监听地址。必须是某个具体地址，0.0.0.0 会被 dsh 拒绝")
	fs.Int("dsh-port", def.DSHPort, "dsh 监听端口（仅本机访问）")
	fs.String("dsh-home", "", "dsh 实例目录（DSH_HOME）。指向不同目录即两套互不干扰的 dsh")
	fs.String("dsh-cmd", "", "启动 dsh 的命令，支持占位符 {host} {port} {home} {workspace} {url} {trusted}；留空自动探测")
	fs.String("dsh-token", "", "已知的 dsh 启动令牌；仅接管外部 dsh 且拿不到令牌时才需要")
	fs.String("npm-registry", "", "拉取 dsh 用的 npm 源；留空用 npm 默认，连不上时自动改用镜像源重试")

	fs.String("workspace", "", "dsh 子进程的工作目录（决定会话默认能访问哪些文件），留空沿用当前目录")
	fs.String("state-dir", config.DefaultStateDir(), "网关自己的状态目录（放账号表等），不会写进 dsh 的实例目录")
	fs.String("users", "", "账号表路径，留空则放在 <状态目录>/users.json")
	fs.String("public-url", "", "对外公告地址（--public-url），留空按局域网 IP 自动推导")
	fs.Var(&hold.trustedHosts, "trusted-host", "额外信任的地址，可重复。局域网 IP 会自动加入，一般不用手工指定")

	fs.String("tls-cert", "", "HTTPS 证书文件，与 --tls-key 成对提供")
	fs.String("tls-key", "", "HTTPS 私钥文件，与 --tls-cert 成对提供")

	fs.Bool("auto-start", def.AutoStart, "dsh 端口空闲时由网关拉起 dsh")
	fs.Bool("adopt", def.Adopt, "dsh 端口已被占用时直接接管那个实例，而不是报错")
	fs.Bool("restart", def.Restart, "dsh 意外退出后自动重新拉起")

	fs.Duration("session-ttl", def.SessionTTL, "登录会话有效期")
	fs.Int("max-login-fails", def.MaxLoginFails, "同一 IP 连续登录失败多少次后开始锁定")
	fs.Duration("lockout", def.LockoutBase, "登录失败锁定的基础时长，每次再失败翻倍")

	fs.Duration("dsh-start-timeout", def.DSHStartTimeout, "拉起 dsh 后等待其就绪的最长时间")
	return hold
}

// isRunModeFlag 判断某个选项属于「怎么启动」而不是「启动什么」。
//
// 日志级别、后台化、配置文件、等待时长都属于前者：它们决定网关以什么姿态运行，
// 与实例配置无关。这个名单被两处共用——一处决定哪些选项要转成配置键，另一处
// 决定用 --config 时哪些选项仍然允许出现。两处各写一份必然会漏（漏过一次：
// `--config --background` 被当成「混用了实例选项」而直接拒绝）。
//
// --allow-root-write 也在这里：它不是实例配置（不能喂给 config.Apply、也不该
// 写进服务单元），但属于「怎么启动」，后台化时要原样交给子进程——真正落盘的是
// 那个子进程，它得知道用户已经明确表达过知情。
func isRunModeFlag(name string) bool {
	switch name {
	case "log-level", "log-json", "config", "background", "d", "wait", "run-dir",
		"allow-root-write":
		return true
	}
	return false
}

// overrides 收集用户**显式写出**的选项。
//
// 用 fs.Visit 而不是遍历全部选项：没写的选项必须保持默认值，否则「配置来自
// 环境变量」这条优先级就永远不会生效。log-level / log-json / config 这几个
// 不属于实例配置的选项在这里被过滤掉。
func overrides(fs *flag.FlagSet, trustedHosts []string) (map[string]string, error) {
	result := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		// 这些不是实例配置，别喂给 config.Apply。
		if !isRunModeFlag(f.Name) {
			result[f.Name] = f.Value.String()
		}
	})
	if len(trustedHosts) > 0 {
		result["trusted-host"] = strings.Join(trustedHosts, ",")
	}
	return result, nil
}

// parseLevel 解析日志级别。
func parseLevel(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug", "info", "warn", "warning", "error", "":
		return raw, nil
	default:
		return "", fmt.Errorf("无法识别的日志级别 %q，可用：debug / info / warn / error", raw)
	}
}

// defaultDSHHome 返回默认实例目录，供帮助文本展示。
func defaultDSHHome() string {
	return "~/.dsh"
}

// formatTimeout 把时长渲染成秒数，便于在提示里说人话。
func formatTimeout(d time.Duration) string { return d.String() }

// parseFlexible 解析命令行，但允许位置参数出现在选项之前。
//
// 标准 flag 包遇到第一个非选项参数就停，于是 `user add admin --dsh-home X`
// 里的 --dsh-home 会被当成多余参数丢弃——而这恰恰是大多数人顺手写出来的顺序。
// 这里把选项与位置参数分开收集，再用 `--` 终止选项解析，两种顺序都能用。
func parseFlexible(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && strings.HasPrefix(arg, "-") {
			options = append(options, arg)
			name := strings.TrimLeft(arg, "-")
			if strings.Contains(name, "=") {
				continue // 值已经内联
			}
			// 取值型选项后面紧跟的那个词属于它，得一起搬过去；布尔选项没有值。
			flagDef := fs.Lookup(name)
			if flagDef == nil {
				continue // 交给 flag 包报「未知选项」
			}
			if boolFlag, ok := flagDef.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
				continue
			}
			if i+1 < len(args) {
				i++
				options = append(options, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return fs.Parse(append(append(options, "--"), positional...))
}
