// dsh 的可用性保障：dsh 的官方启动命令是 npx @deepseek-ai/dsh web。EnsureDSH
// 在启动前把这件事做稳：
//
//   - 确认 npx 存在（它是 Node.js 自带的）；
//   - 用官方命令做冒烟验证（web --help 真的能执行），把「装了但跑不起来」
//     的情况在网关启动前就现出原形；
//   - 失败时按证据给出指引：每一条都引用 dsh 的原始报错，不说没有依据的话。
//
// --yes 是官方命令的必要补丁：包不存在或有新版本时，npx 会询问
// 「Ok to proceed? (y)」，而网关拉起的进程没有终端可回答——不加它只会收到
// EOF 然后失败；加上它，npx 自动确认安装与更新。
//
// 冒烟/启动失败不视为致命错误：网关照常启动（登录页能开），dsh 的启动循环
// 会继续重试并在日志里给出原因——「网关一直在线、dsh 慢慢总能起来」是本
// 项目对无界面服务器的基本承诺。
package dshproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dshPackage 是 npm 上的包名，官方命令里的包。
const dshPackage = "@deepseek-ai/dsh"

// fallbackVersions 是「latest 在当前环境跑不起来」时的候选版本，从新到旧。
// 只在 latest 冒烟失败时逐个尝试，且必须通过同一个冒烟验证才会被采用；
// 上游修复 latest 后，这里的回退自然不再触发。
//
// 实测（2026-10，Debian + Node 22，linux-x64）：latest 0.2.0-rc.2 因上游未
// 发布平台原生绑定无法启动，而 0.1.7-rc.2 与 0.1.5-rc.3 可正常启动。
var fallbackVersions = []string{"0.1.7-rc.2", "0.1.5-rc.3"}

// dshVersionPattern 是「版本或 dist-tag」允许的字符。
//
// 为什么要卡字符：这个值会被拼进启动命令（`npx --yes @deepseek-ai/dsh@<值>`）
// 并原样存进实例记录，之后由 SplitCommand 拆开直接 exec。允许空格或引号就等于
// 允许往 npx 的参数表里塞东西——比如 `1.0.0 --registry=http://evil` 会让网关从
// 别人指定的源装包。字母数字与 . - + ~ ^ * 已经覆盖版本号、预发布号、构建元数据
// 与 dist-tag（next / alpha）以及 `^1.2` `~0.2` 这类范围写法，够用。
var dshVersionPattern = regexp.MustCompile(`^[0-9A-Za-z.*+~^-]+$`)

// ValidateDshVersion 校验用户填的 dsh 版本 / dist-tag，返回去掉首尾空白的值；
// 空串是合法值，表示「跟随 latest」。
func ValidateDshVersion(text string) (string, error) {
	value := strings.TrimSpace(text)
	if value == "" {
		return "", nil
	}
	if !dshVersionPattern.MatchString(value) {
		return "", fmt.Errorf("dsh 版本 %q 不合法：只能包含字母、数字与 . - + ~ ^ *"+
			"（版本号如 0.2.2-alpha.2，或 dist-tag 如 next、alpha）；留空表示跟随 latest", text)
	}
	// 以 - 开头单独再拦一次：字符集里有 -（0.2.1-rc.4 要用），但版本名永远不会
	// 以它开头。而 `-` 开头的串在命令行里是选项的样子——这一串最终会拼进启动
	// 命令并被拆开执行，长得像选项就该拒绝，不赌它这次是被拼在包名后面。
	if strings.HasPrefix(value, "-") {
		return "", fmt.Errorf("dsh 版本 %q 不合法：版本名或 dist-tag 不以 - 开头（如 0.2.2-alpha.2、next、alpha）；留空表示跟随 latest", text)
	}
	return value, nil
}

// nodeVersionPattern 从「v20.12.0」这类纯净版本文本里取主次版本。
var nodeVersionPattern = regexp.MustCompile(`v?(\d+)\.(\d+)`)

// nodeBannerPattern 匹配 Node 崩溃输出末尾的运行时横幅（Node.js v18.20.4）。
// 不能用宽松的版本模式在整段输出里乱抓——堆栈里的任意「数字.数字」都会被
// 误认成 Node 版本（实测把 esm_loader 帧里的数字当成了 v15.0）。
var nodeBannerPattern = regexp.MustCompile(`Node\.js v(\d+)\.(\d+)`)

// parseNodeVersion 从任意文本里解析主次版本号，解析失败 ok=false。
func parseNodeVersion(text string) (major, minor int, ok bool) {
	match := nodeVersionPattern.FindStringSubmatch(text)
	if match == nil {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(match[1])
	minor, err2 := strconv.Atoi(match[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// RegistryMirror 是默认源连不上时改用的 npm 镜像。
//
// 选 npmmirror（原淘宝源）是因为它完整镜像 npm 元数据与包体，国内实测比
// registry.npmjs.org 快一个数量级；只在「默认源明确失败」时才用，用户显式
// 给了 --npm-registry 就完全以用户的为准，不做任何自动替换。
const RegistryMirror = "https://registry.npmmirror.com"

// ErrSmokeTimeout 表示冒烟验证超时——多数时候是网络或 npm 源的问题，
// 而不是 dsh 本身的兼容性问题，因此值得换一个源重试。
var ErrSmokeTimeout = errors.New("dsh web --help 长时间无响应（网络或 npm 源问题）")

// errSmokeNoOutput 表示命令退出 0 却没有帮助输出：入口被跳过，属于运行时
// 兼容问题（见 smokeGuidance），换源解决不了。
var errSmokeNoOutput = errors.New("dsh web --help 退出码为 0 但没有输出帮助（入口没有执行）")

// Resolution 是冒烟验证过的 dsh 启动方式。
type Resolution struct {
	// Command 是验证过的 npx 命令（可能钉住某个固定版本）。
	Command string
	// Registry 是这条命令要用的 npm 源；空表示用 npm 自己的默认。
	// 以 npm_config_registry 注入子进程，dsh 自己拉插件时也会走同一个源。
	Registry string
}

// EnsureDSH 返回经冒烟验证的官方 dsh 启动命令，并在网络不通时自动换源。
//
// 官方命令是 npx @deepseek-ai/dsh web：npx 每次启动都会对照 npm 检查版本，
// 包不存在或有新版本时需要交互确认安装/更新——--yes 把这个确认自动化。
// 候选命令必须通过冒烟验证（web --help 打出帮助且不带运行时错误）。
//
// 两种失败要分开处理，这是本函数的核心：
//
//   - **网络 / 源的问题**（超时、连不上）：换源重试，并把镜像写进返回值，
//     这样拉起来之后 dsh 自己装插件也走同一个源。实测踩过：国内服务器上
//     registry.npmjs.org 慢到 180 秒冒烟超时，用户看到的只是一句
//     「长时间无响应」，然后就没下文了。
//   - **运行时兼容问题**（入口静默不执行、原生绑定缺失、旧 Node 崩溃）：
//     换源没有意义，直接退到实测可用的固定版本，仍不行就给出带证据的指引。
//
// registry 为空表示用 npm 默认（含用户 ~/.npmrc），失败才自动试镜像；
// 非空表示用户点名了源，只认这一个。
func EnsureDSH(ctx context.Context, log *slog.Logger, registry string) (Resolution, error) {
	return EnsureDSHWithBin(ctx, log, registry, "")
}

// EnsureDSHWithBin 与 EnsureDSH 相同，但把 extraBin 目录前置到子进程的 PATH 上。
//
// 为什么需要它：网关会把官方 Node.js 运行时下载到状态目录（`EnsureNodeRuntime`），
// 那台机器上就**没有**系统级 npx。此时只看进程自己的 PATH 会得出「找不到 npx」，
// 而实际上 npx 就在自备运行时里——用户看到的是「明明刚下好 Node 却说没装」。
// 传进来的是目录（NodeBin 的父目录），不是 npx 文件本身：npx 的 shebang 是
// `#!/usr/bin/env node`，只把 npx 塞进命令行、不把它的 bin 目录放进 PATH，
// 子进程照样找不到 node。
func EnsureDSHWithBin(ctx context.Context, log *slog.Logger, registry, extraBin string) (Resolution, error) {
	return EnsureDSHVersion(ctx, log, registry, extraBin, "")
}

// EnsureDSHVersion 与 EnsureDSHWithBin 相同，但把要装的 dsh 版本钉在 version。
//
// version 可以是版本号（`0.2.2-alpha.2`）或 dist-tag（`next`、`alpha`），空表示
// 跟随 latest。**钉住时不再回退**：候选只有用户点名的那一个，装不上就直接报装不上。
// 这是与「latest 跑不起来就退回实测可用版本」刻意相反的两条路——自动回退的前提是
// 「用户没说要哪个版本，能跑就行」；用户已经点名了版本，悄悄换成另一个版本装上去，
// 他会拿着一个自己以为没有装的版本在跑，比装不上更难查。
func EnsureDSHVersion(ctx context.Context, log *slog.Logger, registry, extraBin, version string) (Resolution, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pinned, err := ValidateDshVersion(version)
	if err != nil {
		return Resolution{}, err
	}
	npx := lookPathAny("npx", "npx.cmd")
	if npx == "" && extraBin == "" {
		return Resolution{}, errors.New("找不到 npx：dsh 通过官方命令 npx --yes @deepseek-ai/dsh 启动；" +
			"网关会先把官方 Node.js 运行时下载到状态目录，这次没成功（原因见上面的下载报错）。" +
			"也可以手工安装 Node.js（https://nodejs.org/），或用 --dsh-cmd 指定启动命令")
	}
	// 自备运行时里的 npx 用绝对路径调，不依赖 PATH 查找。
	if npx == "" {
		npx = filepath.Join(extraBin, "npx")
	}

	versions := versionPlan(pinned)
	registries := registryPlan(registry)

	var (
		lastOutput   string
		lastErr      error
		sourceFailed bool // 这个源上出现过网络类失败：值得换源
	)
	for i, source := range registries {
		// 换源只对「源拉不动」有意义。全是运行时兼容失败时再换一个源去试
		// 一遍是白费几分钟，还会把最终报错换成另一个源的现象，误导排查。
		if i > 0 && !sourceFailed {
			break
		}
		sourceFailed = false
		if i > 0 {
			log.Warn("上一个 npm 源没有成功，改用镜像源重试", "registry", source)
		}
		for _, candidate := range versions {
			command := npxCommand(npx, candidate, source)
			output, err := smokeTestWithBin(ctx, command, source, extraBin)
			if err == nil {
				if i > 0 {
					log.Info("dsh 已从镜像源拿到并验证通过", "registry", source, "command", redact(command))
				}
				if pinned == "" && candidate != "" {
					log.Info("最新版 dsh 在当前环境无法启动，已自动改用实测可用的固定版本（仍是官方 npx 命令，仅固定版本；上游修复后重启网关即恢复 latest）",
						"version", candidate, "command", redact(command))
				}
				return Resolution{Command: command, Registry: source}, nil
			}
			lastOutput, lastErr = output, err
			if isNetworkFailure(err, output) {
				// 源不通是整源的问题，换版本没用：直接换下一个源。
				log.Warn("这个源拉不动 dsh，准备换源", "registry", source, "version", candidate, "err", errText(err))
				sourceFailed = true
				break
			}
			if pinned != "" {
				// 钉住时没有下一个候选可试，说清楚「就是这个版本」。
				log.Warn("指定版本冒烟未通过", "version", pinned, "command", redact(command), "err", errText(err))
				continue
			}
			log.Warn("冒烟未通过，尝试实测可用的固定版本",
				"version", candidate, "command", redact(command), "err", errText(err))
		}
	}
	if pinned != "" {
		return Resolution{}, pinnedGuidance(pinned, lastOutput, lastErr)
	}
	return Resolution{}, smokeGuidance(lastOutput, lastErr)
}

// versionPlan 给出要依次尝试的版本。空串表示 latest（npx 不写 @ 时就是 latest）。
func versionPlan(pinned string) []string {
	if pinned != "" {
		return []string{pinned}
	}
	return append([]string{""}, fallbackVersions...)
}

// pinnedGuidance 是「用户点名要的版本装不上/跑不起来」的指引。
//
// 不复用 smokeGuidance 的运行时兼容分支：那些结论是按「网关自己挑的版本」写的，
// 而这里第一个要排除的可能性是版本本身不存在（打错字、那个 dist-tag 在这个源上
// 没有）。所以把原始报错原样交出去，并给出核对版本的命令。
func pinnedGuidance(version, output string, err error) error {
	text := strings.TrimSpace(output)
	if text == "" && err != nil {
		text = strings.TrimSpace(err.Error())
	}
	return fmt.Errorf("指定的 dsh 版本 %s 没能通过验证（%v）。原始输出（证据）：%s。"+
		"先核对版本名：npm view %s dist-tags 看有哪些标签，npm view %s versions --json 看有哪些版本号；"+
		"版本留空则跟随 latest",
		version, errText(err), tailText(text, 300), dshPackage, dshPackage)
}

// registryPlan 给出要依次尝试的源。
//
// 用户点名了源就只用它——静默替换用户指定的源是不可接受的；没点名时先用
// npm 默认（""），失败了再试镜像。
func registryPlan(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	return []string{"", RegistryMirror}
}

// packageSpec 返回带版本的包名（`@deepseek-ai/dsh@next`）；version 为空时就是
// 裸包名（npm 自己按 latest 解）。pnpm 安装用的是这个形式。
func packageSpec(version string) string {
	if version = strings.TrimSpace(version); version != "" {
		return dshPackage + "@" + version
	}
	return dshPackage
}

// npxCommand 拼一条 npx 命令；version 为空表示 latest。
//
// 源写在包名之前：npx 自己的配置项放前面，后面留干净的位置给要传给 dsh 的
// 参数（冒烟验证会再接 web --help，拉起时会再接 web --host …）。
func npxCommand(npx, version, registry string) string {
	command := npx + " --yes"
	if registry != "" {
		// 写进命令行而不是只靠环境变量：命令本身要能被人复制出去手工跑一遍，
		// 看到现象才算证据（环境变量不会出现在日志的那条命令里）。
		command += " --registry=" + registry
	}
	command += " " + dshPackage
	if version != "" {
		command += "@" + version
	}
	return command
}

// isNetworkFailure 判断冒烟失败是不是「网络 / 源」这一类。
//
// 超时是主要判据；npm 自己的网络错误码与文案也一并认，因为它们会在远不到
// 超时的时候就把失败返回回来。
func isNetworkFailure(err error, output string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSmokeTimeout) {
		return true
	}
	text := strings.ToLower(output + " " + err.Error())
	for _, marker := range []string{
		"etimedout", "econnreset", "econnrefused", "enotfound", "eai_again",
		"err_socket_timeout", "network", "socket hang up", "request to http",
		"fetch failed", "certificate", "tls",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// smokeTest 冒烟验证启动命令真的能执行：web --help 要打出 web 应用的帮助、
// 且不带运行时错误才算数。用 --version 不够——0.2.0-rc 系列的 --version 在
// 兼容性坏掉的运行时下照样「成功」，而 web profile 一启动就现原形：
//   - 旧 Node.js：崩溃，输出带 Node.js v… 横幅；
//   - 某些运行时：入口静默不执行（exit 0 无输出）；
//   - 缺平台原生绑定：boot 报 No usable native binding。
func smokeTest(ctx context.Context, command, registry string) (string, error) {
	return smokeTestWithBin(ctx, command, registry, "")
}

// smokeTestWithBin 与 smokeTest 相同，但把 extraBin 目录前置到 PATH 上。
//
// npx 的 shebang 是 `#!/usr/bin/env node`：命令用自备运行时的绝对路径调，但
// npx 脚本自己要找 node，所以 PATH 里必须有那个 bin 目录，否则会以
// "env: node: No such file or directory" 失败。
func smokeTestWithBin(ctx context.Context, command, registry, extraBin string) (string, error) {
	args, err := SplitCommand(command)
	if err != nil || len(args) == 0 {
		return "", fmt.Errorf("解析 dsh 启动命令 %q 失败: %w", command, err)
	}
	args = append(args, "web", "--help")
	// 首次运行 npx 要下载包及其依赖，给足时间。
	smokeCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(smokeCtx, args[0], args[1:]...)
	applyProbeAttrs(cmd)
	env := npmEnv(os.Environ(), registry)
	if extraBin != "" {
		env = setEnv(env, "PATH", prependPath(extraBin, envValue(env, "PATH")))
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err == nil && smokeOK(text) {
		return text, nil
	}
	if err == nil {
		return text, errSmokeNoOutput
	}
	if smokeCtx.Err() != nil {
		return text, ErrSmokeTimeout
	}
	return text, fmt.Errorf("%w：%s", err, tailText(text, 300))
}

// npmEnv 在环境变量里指定 npm 源。
//
// 用环境变量而不是只把 --registry 写进命令行：npm/npx 会把 npm_config_* 传给
// 自己拉起的进程，dsh 启动后再用 npm 装插件时也走同一个源。显式给空串表示
// 用 npm 自己的默认，此时不动环境。
func npmEnv(env []string, registry string) []string {
	if registry == "" {
		return env
	}
	const key = "npm_config_registry="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, key) {
			out = append(out, item)
		}
	}
	return append(out, key+registry)
}

// smokeOK 判断冒烟输出是否健康：有帮助文本、且不带已知的运行时不兼容标志。
func smokeOK(text string) bool {
	return text != "" &&
		!strings.Contains(text, "host preparation failed") &&
		!strings.Contains(text, "fatal uncaught exception") &&
		!strings.Contains(text, "Node.js v")
}

// smokeGuidance 把冒烟失败翻译成带证据的指引。
//
// 每一条都引用 dsh 的原始报错（标注「证据」），只陈述可验证的事实；
// 涉及运行时兼容性的结论都附了实测来源（哪个 Node 版本、什么现象）。
func smokeGuidance(output string, err error) error {
	text := strings.TrimSpace(output)
	switch {
	case strings.Contains(text, "No usable native binding"):
		return fmt.Errorf("dsh 无法在当前平台启动：它报告缺少运行所需的原生绑定。原始报错（证据）：%s。"+
			"绑定按平台发布（node-addon-require-builtin-<平台>，由 dsh 上游维护），缺失或未发布就不是网关"+
			"能修复的——请向 dsh 上游反馈，或换用有绑定的平台/版本，或用 --dsh-cmd 指定启动命令",
			tailText(text, 200))
	case strings.Contains(text, "Node.js v"):
		version := "未知"
		if match := nodeBannerPattern.FindStringSubmatch(text); match != nil {
			version = fmt.Sprintf("v%s.%s", match[1], match[2])
		}
		return fmt.Errorf("dsh 在 Node.js %s 下启动即崩溃，原始输出（证据）：%s。"+
			"崩溃原因可从输出定位——例如引用 node:util.parseEnv 这类新 API，说明 Node.js 版本过旧"+
			"（parseEnv 需要 20.12+）；请升级 Node.js 后重试，或用 --dsh-cmd 指定启动命令",
			version, tailText(text, 200))
	case err != nil && strings.Contains(err.Error(), "没有输出帮助"):
		return fmt.Errorf("dsh 入口没有执行：web --help 静默退出 0、无任何输出。" +
			"证据：这版 dsh 的 bin.js 以「if (import.meta.main)」守门，而实测 Node 20/22 没有该特性" +
			"（入口被跳过）、26 有——请升级 Node.js 到最新版后重试，或用 --dsh-cmd 指定启动命令")
	default:
		if err == nil {
			return nil
		}
		return fmt.Errorf("%v。若怀疑是 dsh 与运行时的兼容问题，可尝试升级 Node.js 后重试，"+
			"或用 --dsh-cmd 指定启动命令", err)
	}
}

// lookPathAny 依次尝试多个候选名，返回第一个在 PATH 里的；都找不到返回空串。
func lookPathAny(names ...string) string {
	for _, name := range names {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}

// tailText 取文本的末尾若干字符。npm/dsh 的报错结论几乎总在输出的最后。
func tailText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}
