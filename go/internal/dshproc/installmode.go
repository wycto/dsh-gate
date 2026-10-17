// dsh 的三种安装方式。
//
// 官方给了三条路，管理台让用户自己选——它们对机器与网络的要求不同，没有哪一
// 条在所有场景下都最好：
//
//	InstallNpx  —— npx --yes @deepseek-ai/dsh
//	              官方推荐方式，每次启动对照 npm 检查版本；无需预先安装任何
//	              东西，只要有 Node。适合「只想赶紧跑起来」。
//	InstallGit  —— git clone + pnpm install + pnpm run build
//	              从源码构建（官方 README「Run from source」）。构建耗时、占
//	              空间，但拿到的是本仓库源码，可改、可跟分支。需要 git + pnpm。
//	InstallPnpm —— pnpm add -g @deepseek-ai/dsh（或装到实例自己的目录）
//	              装成一份真实可执行文件，之后启动不再走 npx 的版本检查，冷启
//	              动更快，也便于固定版本。
//
// 三者最终都归结为一条「启动命令」（Instance.DshCmd）：supervisor 会展开它并
// 接上 web --host --port。所以安装方式的差别只在「怎么把那条命令准备好」，
// 不在运行时。
package dshproc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// InstallMode 是 dsh 的安装方式。
type InstallMode string

const (
	// InstallNpx 用官方 npx 命令（默认，无需预装）。
	InstallNpx InstallMode = "npx"
	// InstallGit 从源码 clone 并构建。
	InstallGit InstallMode = "git"
	// InstallPnpm 用 pnpm 装一份可执行文件。
	InstallPnpm InstallMode = "pnpm"
)

// ParseInstallMode 把文本解析成安装方式；空串按默认（npx）处理。
func ParseInstallMode(text string) (InstallMode, error) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", string(InstallNpx):
		return InstallNpx, nil
	case string(InstallGit):
		return InstallGit, nil
	case string(InstallPnpm):
		return InstallPnpm, nil
	default:
		return "", fmt.Errorf("不认识的安装方式 %q（可选：npx、git、pnpm）", text)
	}
}

// Label 返回给人看的中文名。
func (m InstallMode) Label() string {
	switch m {
	case InstallGit:
		return "从源码构建（git clone + pnpm build）"
	case InstallPnpm:
		return "pnpm 安装"
	default:
		return "npx 官方命令"
	}
}

// Valid 表示这是不是一个已支持的安装方式。
func (m InstallMode) Valid() bool {
	switch m {
	case InstallNpx, InstallGit, InstallPnpm:
		return true
	}
	return false
}

// InstallRequest 是一次安装请求。
type InstallRequest struct {
	// Mode 是安装方式。
	Mode InstallMode
	// Dir 是安装目录。git 方式会 clone 到 <Dir>/deepseek-harness；
	// pnpm 方式把包装进 <Dir>/dsh。npx 方式不用它（留空即可）。
	Dir string
	// DshHome 是实例的 DSH_HOME（写进命令的环境变量，见 InstallResult.Env）。
	DshHome string
	// Registry 是 npm 源（空表示 npm 默认）。git/pnpm 方式都用它。
	Registry string
	// GitURL 是源码仓库地址（空表示官方仓库）。
	GitURL string
	// GitRef 是要检出的分支或标签（空表示仓库默认分支）。
	GitRef string
	// NodeBin 是应当前置进 PATH 的 Node bin 目录（空表示用系统 Node）。
	NodeBin string
	// PnpmBin 是应当前置进 PATH 的 pnpm bin 目录（空表示用系统 pnpm）。
	PnpmBin string
}

// InstallResult 是一次安装的结果。
//
// Dir 是**用户指定的安装根目录**（req.Dir），不是内部推导出来的子目录
// （git 的 <Dir>/deepseek-harness、pnpm 的 <Dir>/dsh）。这个区别很重要：管理台
// 会把 Dir 写回实例记录、下次重装再读出来当 req.Dir 用。若在这里回填推导出的
// 子目录，每重装一次就多嵌一层（实测 /srv/pnpm/inst → .../dsh → .../dsh/dsh）。
type InstallResult struct {
	// Mode 是实际用的安装方式。
	Mode InstallMode
	// Command 是装好之后的 dsh 启动命令前缀（不带 web 子命令；supervisor
	// 会自己接上 web --host --port）。例如：
	//   npx  -> "npx --yes @deepseek-ai/dsh"
	//   git  -> "node /path/deepseek-harness/apps/cli/lib/bin.js"
	//   pnpm -> "/path/dsh/node_modules/.bin/dsh"
	Command string
	// Dir 是安装目录（npx 方式为空）。
	Dir string
	// Version 是安装到的 dsh 版本（探测不到为空）。
	Version string
	// Env 是要注入实例进程的额外环境变量（如 DSH_HOME）。
	Env map[string]string
	// Log 是安装过程的要点（便于在管理台上回显「做了什么」）。
	Log []string
}

// installTimeout 是单条安装命令的上限。
//
// git 方式要 pnpm install + build 整个 monorepo，实测在慢网络下要十几分钟，
// 所以给得比 pnpm 安装宽。超时不代表失败得莫名其妙——错误里会说清是哪一步。
const gitInstallTimeout = 30 * time.Minute

// InstallDSH 按指定方式把 dsh 准备好，返回可用的启动命令。
//
// 每种方式都先做前置检查（工具在不在），缺什么就说清缺什么、怎么补——管理台
// 上那些选项的可用性就是据此算出来的，这里再复核一次，避免界面判断与执行
// 判断漂移。
func InstallDSH(ctx context.Context, log *slog.Logger, req InstallRequest) (InstallResult, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if !req.Mode.Valid() {
		return InstallResult{}, fmt.Errorf("不认识的安装方式 %q", req.Mode)
	}
	switch req.Mode {
	case InstallNpx:
		return installViaNpx(ctx, log, req)
	case InstallGit:
		return installViaGit(ctx, log, req)
	case InstallPnpm:
		return installViaPnpm(ctx, log, req)
	}
	return InstallResult{}, fmt.Errorf("不认识的安装方式 %q", req.Mode)
}

// installViaNpx 用官方 npx 命令：不预装任何东西，交给 EnsureDSH 冒烟验证。
func installViaNpx(ctx context.Context, log *slog.Logger, req InstallRequest) (InstallResult, error) {
	if lookPathAny("npx", "npx.cmd") == "" && req.NodeBin == "" {
		return InstallResult{}, errors.New("找不到 npx：npx 方式需要 Node.js（自带 npm 与 npx）；" +
			"请安装 Node.js（https://nodejs.org/），或改用其他安装方式")
	}
	// 把自备运行时的 bin 目录交下去：机器上没有系统级 npx 时，npx 就在那里。
	res, err := EnsureDSHWithBin(ctx, log, req.Registry, req.NodeBin)
	if err != nil {
		return InstallResult{}, err
	}
	log.Info("npx 方式已就绪", "command", redact(res.Command))
	return InstallResult{
		Mode:    InstallNpx,
		Command: res.Command,
		Env:     map[string]string{},
		Log:     []string{"用官方 npx 命令，已通过冒烟验证"},
	}, nil
}

// installViaGit 从源码构建：git clone -> pnpm install -> pnpm run build。
//
// 目录布局：<Dir>/deepseek-harness。启动命令指向构建产物
// apps/cli/lib/bin.js，用 node 直跑——不用 pnpm dsh，因为 supervisor 展开命令
// 时不做 shell 解析，pnpm 这类包管理器包装会引入一层它无法可靠处理的环境依赖。
func installViaGit(ctx context.Context, log *slog.Logger, req InstallRequest) (InstallResult, error) {
	if strings.TrimSpace(req.Dir) == "" {
		return InstallResult{}, errors.New("git 方式需要指定安装目录")
	}
	if lookPathAny("git", "git.exe") == "" {
		return InstallResult{}, errors.New("找不到 git：从源码构建需要 git；" +
			"请先安装 git（https://git-scm.com/），或改用 npx / pnpm 方式")
	}
	node := lookPathAny("node", "node.exe")
	if node == "" && req.NodeBin == "" {
		return InstallResult{}, errors.New("找不到 node：从源码构建需要 Node.js 20+；请安装 Node.js（https://nodejs.org/）")
	}
	pnpm := lookPathAny("pnpm", "pnpm.cmd")
	if pnpm == "" && req.PnpmBin == "" {
		return InstallResult{}, errors.New("找不到 pnpm：从源码构建需要 pnpm（官方 README 的 Run from source 步骤）；" +
			"可用 npm install -g pnpm 安装，或改用 npx 方式")
	}

	repoDir := filepath.Join(req.Dir, gitRepoDirName)
	gitURL := strings.TrimSpace(req.GitURL)
	if gitURL == "" {
		gitURL = defaultGitURL
	}

	// clone：目录已存在就当成「已经 clone 过」，直接进下一步（幂等，重跑
	// 安装不该把用户改过的源码删掉重来）。
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		log.Info("源码目录已存在，跳过 clone（如需更新请手工 git pull）", "dir", repoDir)
	} else {
		if err := os.MkdirAll(req.Dir, 0o755); err != nil {
			return InstallResult{}, fmt.Errorf("创建安装目录 %s 失败: %w", req.Dir, err)
		}
		args := []string{"clone", "--depth", "1"}
		if ref := strings.TrimSpace(req.GitRef); ref != "" {
			args = append(args, "--branch", ref)
		}
		args = append(args, gitURL, repoDir)
		log.Info("正在 clone dsh 源码", "url", gitURL, "dir", repoDir)
		if out, err := runInstallStep(ctx, log, req, req.Dir, gitInstallTimeout, "git", args...); err != nil {
			return InstallResult{}, fmt.Errorf("clone 源码失败：%w。原始输出：%s", err, tailText(out, 300))
		}
	}

	// pnpm install
	log.Info("正在安装依赖（pnpm install）", "dir", repoDir)
	installArgs := append([]string{"install", "--frozen-lockfile"}, allowBuildArgs()...)
	if out, err := runInstallStep(ctx, log, req, repoDir, gitInstallTimeout, "pnpm", installArgs...); err != nil {
		// --frozen-lockfile 只在 lockfile 与 package.json 不一致时失败；那是上游
		// 源码分支漂移的常态，退回普通 install 再试一次即可。
		//
		// 但**网络类失败不能这么退**：普通 install 会重新去拉同一批包，结局一样，
		// 用户白等一轮（实测上游 monorepo 一轮 install 十几分钟）。而且报错会从
		// 「网络拉不动」变成「安装依赖失败」，反而看不清真正的原因。
		if !lockfileMismatch(out) {
			return InstallResult{}, fmt.Errorf("安装依赖失败：%w。原始输出：%s", err, tailText(out, 300))
		}
		log.Warn("pnpm install --frozen-lockfile 因 lockfile 不一致失败，改用普通 install 重试", "err", errText(err))
		retryArgs := append([]string{"install"}, allowBuildArgs()...)
		if out2, err2 := runInstallStep(ctx, log, req, repoDir, gitInstallTimeout, "pnpm", retryArgs...); err2 != nil {
			return InstallResult{}, fmt.Errorf("安装依赖失败：%w。原始输出：%s", err2, tailText(out2, 300))
		}
	}

	// pnpm run build
	log.Info("正在构建（pnpm run build）", "dir", repoDir)
	if out, err := runInstallStep(ctx, log, req, repoDir, gitInstallTimeout, "pnpm", "run", "build"); err != nil {
		return InstallResult{}, fmt.Errorf("构建失败：%w。原始输出：%s", err, tailText(out, 300))
	}

	bin := filepath.Join(repoDir, "apps", "cli", "lib", "bin.js")
	if _, err := os.Stat(bin); err != nil {
		return InstallResult{}, fmt.Errorf("构建完成但没找到 dsh 入口（%s）：上游构建布局可能变了，请查看构建日志", bin)
	}

	// 用 node 直跑入口：比 pnpm dsh 少一层包装，supervisor 能可靠拆分命令。
	nodeExe := node
	if nodeExe == "" {
		nodeExe = filepath.Join(req.NodeBin, binaryName("node"))
	}
	command := nodeExe + " " + bin
	version := probeVersionEnv(ctx, installEnv(req), bin, "--version")
	log.Info("git 方式已就绪", "command", command, "version", version)
	return InstallResult{
		Mode:    InstallGit,
		Command: command,
		// 回填 req.Dir（见 InstallResult 的说明）：回填 repoDir 会让重装嵌套。
		Dir:     req.Dir,
		Version: version,
		Env:     map[string]string{},
		Log:     []string{"git clone + pnpm install + pnpm run build", "入口：" + bin},
	}, nil
}

// installViaPnpm 用 pnpm 装一份 dsh 可执行文件。
//
// 装到实例自己的目录（<Dir>/dsh）而不是全局：管理端可能管着多个实例，各自的
// dsh 版本不一定相同；全局安装会互相覆盖，也让「删实例」带不走自己那份。
func installViaPnpm(ctx context.Context, log *slog.Logger, req InstallRequest) (InstallResult, error) {
	if strings.TrimSpace(req.Dir) == "" {
		return InstallResult{}, errors.New("pnpm 方式需要指定安装目录")
	}
	if lookPathAny("pnpm", "pnpm.cmd") == "" && req.PnpmBin == "" {
		return InstallResult{}, errors.New("找不到 pnpm：请先安装 pnpm（npm install -g pnpm），" +
			"或改用 npx 方式（无需预装）")
	}

	prefix := filepath.Join(req.Dir, "dsh")
	if err := os.MkdirAll(prefix, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("创建安装目录 %s 失败: %w", prefix, err)
	}

	log.Info("正在用 pnpm 安装 dsh", "dir", prefix, "registry", req.Registry)
	args := []string{"add", dshPackage, "--dir", prefix}
	args = append(args, allowBuildArgs()...)
	out, err := runInstallStep(ctx, log, req, prefix, pnpmInstallTimeout, "pnpm", args...)
	if err != nil {
		return InstallResult{}, fmt.Errorf("pnpm 安装 dsh 失败：%w。原始输出：%s", err, tailText(out, 300))
	}

	// pnpm 把可执行文件放在 node_modules/.bin 下（Windows 是 .cmd）。
	bin := filepath.Join(prefix, "node_modules", ".bin", binaryName("dsh"))
	if _, err := os.Stat(bin); err != nil {
		return InstallResult{}, fmt.Errorf("pnpm 安装完成但没找到 dsh 可执行文件（%s）：上游包布局可能变了", bin)
	}
	version := probeVersionEnv(ctx, installEnv(req), bin, "--version")
	log.Info("pnpm 方式已就绪", "command", bin, "version", version)
	return InstallResult{
		Mode:    InstallPnpm,
		Command: bin,
		// 回填 req.Dir（见 InstallResult 的说明）：回填 prefix 会让重装嵌套。
		Dir:     req.Dir,
		Version: version,
		Env:     map[string]string{},
		Log:     []string{"pnpm add " + dshPackage, "可执行文件：" + bin},
	}, nil
}

// gitRepoDirName 是 clone 到安装目录下的子目录名。
const gitRepoDirName = "deepseek-harness"

// defaultGitURL 是官方源码仓库。
const defaultGitURL = "https://github.com/deepseek-ai/deepseek-harness"

// runInstallStep 跑一条安装命令，把 Node/pnpm 的 bin 目录前置进 PATH，
// 并按 registry 注入 npm 源。
//
// 返回合并输出（供错误里附证据）；出错时也返回输出，成功时返回空串。
func runInstallStep(ctx context.Context, log *slog.Logger, req InstallRequest,
	dir string, timeout time.Duration, name string, args ...string) (string, error) {
	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	exe := resolveToolPath(req, name)
	cmd := exec.CommandContext(stepCtx, exe, args...)
	cmd.Dir = dir
	cmd.Env = installEnv(req)

	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if stepCtx.Err() != nil {
			return text, fmt.Errorf("%s 超时（%s）", name, timeout)
		}
		return text, fmt.Errorf("%s 失败: %w", name, err)
	}
	if text != "" {
		log.Debug("安装步骤输出", "step", name, "output", tailText(text, 400))
	}
	return "", nil
}

// dshBuildScriptPackages 是安装 dsh 时必须允许执行构建脚本的原生依赖。
//
// 为什么需要：pnpm 10 起默认**拒绝**依赖的 postinstall 脚本（供应链安全考虑），
// 而这些包正是靠它准备平台相关的产物——
//
//	node-pty / koffi / @deepseek-ai/dsh-subprocess-local  编译或搬运原生绑定
//	protobufjs / @google/genai                            生成运行时代码
//
// 不放行的话 pnpm 会报 "Ignored build scripts" 并**以失败退出**（不是警告），
// dsh 装不起来。逐个点名而不是 `--allow-build=*`：只放行 dsh 真正需要的那几个，
// 不把整条依赖链的脚本都放开——这正是 pnpm 默认拒绝的初衷。
//
// 这个列表由实测得出（pnpm 报的 "Ignored build scripts" 名单）。上游若新增
// 原生依赖，报错会直接点名缺哪个包，照名单加即可。
var dshBuildScriptPackages = []string{
	"@deepseek-ai/dsh-subprocess-local",
	"@google/genai",
	"koffi",
	"node-pty",
	"protobufjs",
}

// allowBuildArgs 生成放行构建脚本的 pnpm 参数。
func allowBuildArgs() []string {
	args := make([]string, 0, len(dshBuildScriptPackages))
	for _, name := range dshBuildScriptPackages {
		args = append(args, "--allow-build="+name)
	}
	return args
}

// lockfileMismatch 判断 pnpm 的失败是不是「lockfile 与 package.json 不一致」。
//
// 只有这一种失败值得退回普通 install 重试：其它原因（网络超时、源不可用、
// 磁盘满）重试同一批包不会变好，只会让用户多等一轮。
func lockfileMismatch(out string) bool {
	for _, marker := range []string{
		"ERR_PNPM_OUTDATED_LOCKFILE",
		"frozen-lockfile",
		"lockfile is not up to date",
		"lockfile is not up-to-date",
	} {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}

// resolveToolPath 找到要执行的工具：优先用请求里给的 bin 目录，其次 PATH。
func resolveToolPath(req InstallRequest, name string) string {
	switch name {
	case "pnpm":
		if req.PnpmBin != "" {
			if p := filepath.Join(req.PnpmBin, binaryName("pnpm")); fileExists(p) {
				return p
			}
		}
	case "git":
		if p := lookPathAny("git", "git.exe"); p != "" {
			return p
		}
		return "git"
	}
	if p := lookPathAny(name, name+".cmd"); p != "" {
		return p
	}
	return name
}

// installEnv 构造安装子进程的环境：Node/pnpm bin 前置进 PATH，npm 源注入。
func installEnv(req InstallRequest) []string {
	env := os.Environ()
	if req.NodeBin != "" {
		env = setEnv(env, "PATH", prependPath(req.NodeBin, envValue(env, "PATH")))
	}
	if req.PnpmBin != "" {
		env = setEnv(env, "PATH", prependPath(req.PnpmBin, envValue(env, "PATH")))
	}
	env = npmEnv(env, req.Registry)
	// pnpm 在 CI 之外会问「要不要重建 lockfile」之类的问题；非交互环境里
	// 这类提问只会挂住。
	env = setEnv(env, "CI", "1")
	return env
}

// prependPath 把目录前置到 PATH 前面（幂等：已经在最前面就不重复加）。
func prependPath(dir, path string) string {
	if dir == "" {
		return path
	}
	if strings.HasPrefix(path, dir+string(os.PathListSeparator)) {
		return path
	}
	if path == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + path
}

// envValue 从 "K=V" 切片里取 K 的值。
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

// setEnv 设置一个环境变量（已存在则替换）。
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

// fileExists 判断路径存在且不是目录。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// EnsureGit 保证本机有 git，没有就说清怎么装。
//
// 与 EnsureNodeRuntime / EnsurePnpm 不同，git 不自动下载：各平台的 git 分发
// 差异太大（源码编译、包管理器、Xcode 命令行工具），自动装比让用户自己装
// 更容易把机器搞乱。所以这里只检测 + 给指引。
func EnsureGit() error {
	if runtime.GOOS == "windows" {
		if lookPathAny("git", "git.exe") == "" {
			return errors.New("找不到 git：从源码构建需要 git；请从 https://git-scm.com/ 安装")
		}
		return nil
	}
	if lookPathAny("git") == "" {
		return errors.New("找不到 git：从源码构建需要 git；请用系统包管理器安装（如 apt install git / brew install git）")
	}
	return nil
}
