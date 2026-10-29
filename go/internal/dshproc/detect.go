// dsh 运行环境检测：回答「这台机器现在能装哪种 dsh」。
//
// 管理台里创建实例之前要知道三件事：本机有没有 Node/npm/pnpm/git、各自什么
// 版本、已经装好的 dsh 在哪儿。检测本身是只读的——它不下载、不安装，只报告
// 事实与建议，装不装由用户在管理台上选。
//
// 四种安装方式对工具的要求不同，检测结果直接决定哪几个选项能选：
//
//	npx   —— 需要 Node（自带 npm 与 npx）
//	git   —— 需要 Node + git + pnpm（从源码构建）
//	pnpm  —— 需要 Node + pnpm（装到本地或全局）
//	local —— 需要 Node + pnpm（启动你自己构建好的源码，用 `pnpm dsh web`）
//
// 检测失败不返回错误：探测不到就是探测不到，管理台据此把那个选项标成不可用，
// 比抛错让整个页面挂掉有用得多。
package dshproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// ToolInfo 是一个工具的检测结果。
type ToolInfo struct {
	// Name 是工具名（node / npm / npx / pnpm / git / dsh）。
	Name string
	// Found 表示 PATH 里找得到它。
	Found bool
	// Path 是解析到的可执行文件路径（Found 为真时才有值）。
	Path string
	// Version 是版本号文本（探测不到时为空）。探测失败不影响 Found——
	// 一个跑不起来但存在的二进制，与「没装」是两件事。
	Version string
	// Err 记录探测版本时的报错（便于用户在管理台上看到「装是装了但跑不起来」）。
	Err string
}

// Available 表示这个工具既有、又能执行。
func (t ToolInfo) Available() bool { return t.Found && t.Err == "" }

// EnvReport 是本机 dsh 运行环境的检测报告。
type EnvReport struct {
	Tools []ToolInfo

	// SelfNodeBin 是网关自备的 Node 运行时目录（空表示没有自备、用的是系统 Node）。
	SelfNodeBin string
	// SelfPnpmBin 是网关自备的 pnpm 目录（空表示没有自备）。
	SelfPnpmBin string

	// ExistingDsh 是本机已有的 dsh 可执行文件（空表示没有）。
	ExistingDsh string
	// ExistingDshVersion 是它的版本号。
	ExistingDshVersion string

	// Platform 是 GOOS/GOARCH（用于排查平台相关的问题）。
	Platform string
}

// Tool 按名字取一个工具的检测结果。
func (r EnvReport) Tool(name string) ToolInfo {
	for _, t := range r.Tools {
		if t.Name == name {
			return t
		}
	}
	return ToolInfo{Name: name}
}

// CanNpx 表示能不能用官方 npx 方式。
func (r EnvReport) CanNpx() bool { return r.Tool("node").Available() }

// CanGit 表示能不能从源码 clone 构建。
func (r EnvReport) CanGit() bool {
	return r.Tool("node").Available() && r.Tool("git").Available()
}

// CanPnpm 表示能不能用 pnpm 方式。
//
// 只要 Node 在就算「能」：pnpm 可以现装（EnsurePnpm 会装到网关状态目录），
// 不必用户自己先装好。自备的 pnpm 也算数。
func (r EnvReport) CanPnpm() bool {
	return r.Tool("node").Available() || r.SelfNodeBin != ""
}

// NodeBinDir 返回应当前置进 PATH 的 Node bin 目录（空表示用系统 Node）。
func (r EnvReport) NodeBinDir() string { return r.SelfNodeBin }

// DetectEnv 检测本机 dsh 运行环境。
//
// stateDir 用于发现网关自备的 Node / pnpm（它们放在状态目录里）。
// 只读、不改任何东西；探测单个工具有超时，避免一个卡住的二进制把整页拖死。
func DetectEnv(ctx context.Context, stateDir string) EnvReport {
	report := EnvReport{Platform: runtime.GOOS + "/" + runtime.GOARCH}

	// 自备运行时先查：有自备的话，PATH 里可能没有 node，但实例照样能跑。
	if stateDir != "" {
		if bin, ok := LocalNodeBin(stateDir); ok {
			report.SelfNodeBin = bin
		}
		if bin := localPnpmBin(stateDir); bin != "" {
			report.SelfPnpmBin = bin
		}
	}

	report.Tools = []ToolInfo{
		detectTool(ctx, "node", "node", "--version"),
		detectTool(ctx, "npm", "npm", "--version"),
		detectTool(ctx, "npx", "npx", "--version"),
		detectTool(ctx, "pnpm", "pnpm", "--version"),
		detectTool(ctx, "git", "git", "--version"),
	}

	// 自备运行时存在但 PATH 里没有对应命令：把它的 bin 目录算进工具探测，
	// 否则管理台会显示「没装 Node」，而实例其实跑得起来。
	//
	// 自备 pnpm 同样要算：`EnsurePnpm` 会把它装到状态目录，可它不在 PATH 上。
	// 只把 pnpm 的存在写进摘要行、表格里却写「缺失」，用户会以为摘要行在说谎。
	if report.SelfNodeBin != "" {
		for i := range report.Tools {
			name := report.Tools[i].Name
			if name != "node" && name != "npm" && name != "npx" {
				continue
			}
			if report.Tools[i].Found {
				continue
			}
			path := filepath.Join(report.SelfNodeBin, binaryName(name))
			if info := probeVersion(ctx, path, "--version"); info != "" {
				report.Tools[i].Found = true
				report.Tools[i].Path = path
				report.Tools[i].Version = info
			}
		}
	}
	if report.SelfPnpmBin != "" {
		for i := range report.Tools {
			if report.Tools[i].Name != "pnpm" || report.Tools[i].Found {
				continue
			}
			path := filepath.Join(report.SelfPnpmBin, binaryName("pnpm"))
			if info := probeVersion(ctx, path, "--version"); info != "" {
				report.Tools[i].Found = true
				report.Tools[i].Path = path
				report.Tools[i].Version = info
			}
		}
	}

	// 已有 dsh：先看 PATH，再看网关自备的 pnpm / node 目录。
	if path := lookPathAny("dsh", "dsh.cmd"); path != "" {
		report.ExistingDsh = path
		report.ExistingDshVersion = probeVersion(ctx, path, "--version")
	}
	report.Tools = append(report.Tools, ToolInfo{
		Name:    "dsh",
		Found:   report.ExistingDsh != "",
		Path:    report.ExistingDsh,
		Version: report.ExistingDshVersion,
	})

	return report
}

// localPnpmBin 返回网关自备 pnpm 的 bin 目录（没有则空串）。
func localPnpmBin(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	binDir := pnpmBinDir(filepath.Join(stateDir, pnpmDirName))
	if binDir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(binDir, pnpmBinaryName())); err != nil {
		return ""
	}
	return binDir
}

// binaryName 给工具名补上平台后缀。
func binaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".cmd"
	}
	return name
}

// detectTool 检测一个工具：先看 PATH，再探版本。
func detectTool(ctx context.Context, name, cmd string, versionArgs ...string) ToolInfo {
	info := ToolInfo{Name: name}
	path, err := exec.LookPath(cmd)
	if err != nil {
		return info
	}
	info.Found = true
	info.Path = path
	info.Version = probeVersion(ctx, path, versionArgs...)
	return info
}

// probeVersion 跑一次 `cmd <args>` 取版本号。失败时返回空串（不记为错误——
// 调用方另有 Err 字段来记录「跑不起来」）。
func probeVersion(ctx context.Context, path string, args ...string) string {
	return probeVersionEnv(ctx, nil, path, args...)
}

// probeVersionEnv 与 probeVersion 相同，但用给定的环境变量跑（nil 表示继承进程环境）。
//
// 装出来的 dsh 是个 `#!/usr/bin/env node` 脚本：PATH 里没有 node 时它连版本都报
// 不出来。安装时既然已经把自备运行时的 bin 目录前置进了环境，探测版本就该用同一份
// 环境，否则会出现「刚装好却说探测不到版本」。
func probeVersionEnv(ctx context.Context, env []string, path string, args ...string) string {
	probeCtx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, path, args...)
	applyProbeAttrs(cmd)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		// 失败时的输出是**报错信息**，不是版本号。直接把它当版本返回会让管理台
		// 显示「版本：<...>: exec: node: not found」这种把错误当版本的东西——
		// 实测 pnpm 方式装出来的 dsh 在 PATH 没有 node 时就是这样。
		// 版本探测失败只说明「现在探测不到」，用空串表示即可。
		return ""
	}
	return firstLine(text)
}

// toolProbeTimeout 是单个工具探测的上限：够慢机器冷启动，又不至于把整页拖住。
const toolProbeTimeout = 5 * time.Second

// firstLine 取文本第一行（版本号通常在第一行）。
func firstLine(text string) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		text = text[:idx]
	}
	return strings.TrimSpace(text)
}

// versionNumber 从版本文本里抠出纯版本号（"v20.12.0" -> "20.12.0"）。
//
// 允许只有主版本号（"v20" -> "20"）：有些工具或预发布构建就这么报版本，
// 要求必须带点号会把这些全部解析失败，进而让「已安装」显示成「探测不到版本」。
var versionNumberPattern = regexp.MustCompile(`\d+(\.\d+)*`)

func versionNumber(text string) string {
	if m := versionNumberPattern.FindString(text); m != "" {
		return m
	}
	return strings.TrimSpace(text)
}

// majorVersion 取版本文本的主版本号（解析不出来返回 0）。
func majorVersion(text string) int {
	num := versionNumber(text)
	if idx := strings.IndexByte(num, '.'); idx > 0 {
		num = num[:idx]
	}
	major := 0
	for _, r := range num {
		if r < '0' || r > '9' {
			return 0
		}
		major = major*10 + int(r-'0')
	}
	return major
}
