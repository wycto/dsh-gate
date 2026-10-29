package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
)

// pluginCommand 实现 `dsh-gate plugin`：直接管理 dsh 配置档（profile）里的插件。
//
// 为什么要网关来管：dsh 自己的「添加插件」按钮走 pnpm，而干净的服务器上既没有
// Node.js 也可能连不上 npm 官方源——实测的报错就是 `spawn pnpm ENOENT` 与
// 180 秒超时。网关这边已经有自备 Node、自备 pnpm 与自动换源，把同一套能力拿来
// 装插件，就不必先让浏览器里的插件页能用。
//
// 做的事与 dsh 插件管理器完全一致（本机 `~/.dsh/profiles/web/.plugin-manager/logs`
// 里的 pnpm 日志可以对照）：
//
//  1. 在 <dsh-home>/profiles/<profile>/ 里 `pnpm add <包名>`；
//  2. 把包名写进该目录 package.json 的 dsh.profile.bundles（dsh 靠这份清单决定
//     加载哪些 bundle）。
//
// 之后需要重启 dsh 才会加载（dsh 的 profile 解析在进程启动时建立）。
func pluginCommand(args []string) int {
	if len(args) == 0 {
		printPluginUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return pluginListCommand(args[1:])
	case "add":
		return pluginAddCommand(args[1:])
	case "-h", "--help", "help":
		fs := flag.NewFlagSet("plugin", flag.ContinueOnError)
		var opts pluginOptions
		registerPluginFlags(fs, &opts)
		printPluginUsage(os.Stdout)
		fs.PrintDefaults()
		return 0
	default:
		return usageError("未知的 plugin 子命令 %q，可用：list / add", args[0])
	}
}

// printPluginUsage 打印 plugin 用法。
func printPluginUsage(w *os.File) {
	fmt.Fprint(w, `用法: dsh-gate plugin <list|add> --instance <实例名|id> [参数] [选项]

管理某个 dsh 实例的配置档（profile）里的插件：加依赖、装包、写进 bundle 清单。
与 dsh 自己的「添加插件」做的是同一件事，但会用上网关自备的 Node/pnpm 与
自动换源——服务器上缺这些时，浏览器里的插件页是装不动的。

实例用 --instance 指定（dsh-gate dsh list 查看）；插件装在实例自己的 DSH_HOME 下。

  dsh-gate plugin list --instance 演示实例
  dsh-gate plugin add dsh-dock --instance 演示实例
  dsh-gate plugin add dsh-dock @deepseek-ai/dsh-web-app --instance 演示实例

选项:
`)
}

// pluginOptions 是 list / add 共用的输入。
type pluginOptions struct {
	Instance  string
	DB        string
	DSHHome   string
	Profile   string
	Registry  string
	StateDir  string
	Timeout   time.Duration
	WithSetup bool
}

// registerPluginFlags 注册两个子命令共用的选项。
func registerPluginFlags(fs *flag.FlagSet, opts *pluginOptions) {
	fs.StringVar(&opts.Instance, "instance", "", "dsh 实例名或 id（必填；dsh-gate dsh list 查看）")
	fs.StringVar(&opts.Instance, "i", "", "同 --instance")
	fs.StringVar(&opts.DB, "db", "", "管理端数据库路径（默认 <状态目录>/portal.db）")
	fs.StringVar(&opts.DSHHome, "dsh-home", "", "直接指定 dsh 实例目录（兼容/高级用法，优先于 --instance）")
	fs.StringVar(&opts.Profile, "profile", "web", "dsh 配置档名；网关跑的是 dsh web，所以默认 web")
	fs.StringVar(&opts.Registry, "npm-registry", "", "拉包用的 npm 源；留空自动（官方失败退镜像）")
	fs.StringVar(&opts.StateDir, "state-dir", "", "网关状态目录（自备 pnpm 与 portal.db 都放这里）；留空用默认")
	fs.DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "安装超时")
}

// resolvedDSHHome 返回 dsh 实例目录：显式 --dsh-home 优先，否则按 --instance 从管理端库解析。
func (o pluginOptions) resolvedDSHHome() (string, error) {
	if strings.TrimSpace(o.DSHHome) != "" {
		return config.ExpandPath(o.DSHHome), nil
	}
	home, _, err := instanceHomeFromFlag(o.Instance, o.StateDir, o.DB)
	return home, err
}

// profilePath 返回配置档目录。
func (o pluginOptions) profilePath() (string, error) {
	home, err := o.resolvedDSHHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "profiles", o.Profile), nil
}

// pluginListCommand 打印配置档里已装的插件与 bundle。
func pluginListCommand(args []string) int {
	fs := flag.NewFlagSet("plugin list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts pluginOptions
	registerPluginFlags(fs, &opts)
	fs.Usage = func() {
		printPluginUsage(os.Stderr)
		fs.PrintDefaults()
	}
	if err := parseFlexible(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if rest := fs.Args(); len(rest) > 0 {
		return usageError("plugin list 不接受位置参数（收到 %q）", strings.Join(rest, " "))
	}

	profileDir, err := opts.profilePath()
	if err != nil {
		return usageError("%v", err)
	}
	pkg, path, err := loadProfilePackage(profileDir)
	if err != nil {
		return fatal("%v", err)
	}
	fmt.Printf("配置档目录  %s\n", path)

	names := make([]string, 0, len(pkg.Dependencies))
	for name := range pkg.Dependencies {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Println("已装插件    无")
	} else {
		for i, name := range names {
			label := "已装插件    "
			if i > 0 {
				label = "            "
			}
			fmt.Printf("%s%s %s\n", label, name, pkg.Dependencies[name])
		}
	}

	bundles := pkg.DSH.Profile.Bundles
	if len(bundles) == 0 {
		fmt.Println("bundle 清单 空（dsh 会加载不了任何插件）")
	} else {
		for i, name := range bundles {
			label := "bundle 清单 "
			if i > 0 {
				label = "            "
			}
			fmt.Printf("%s%s\n", label, name)
		}
	}

	// 浏览器里的「模型 / 凭据」设置页由 dsh-web-app 挂载：缺了它，页面上只会
	// 显示 settings are unavailable in this browser。这条检查值得直接说出来。
	if !containsString(bundles, webAppBundle) {
		fmt.Println()
		fmt.Printf("提示        bundle 清单里没有 %s：浏览器里的设置页（模型、凭据）由它挂载，\n", webAppBundle)
		fmt.Println("            缺了会报 settings are unavailable in this browser。")
		fmt.Println("            它随 dsh 本体一起安装，不在 npm 公开包源里，装不了；")
		fmt.Println("            先升级/重装 dsh，或用 dsh-dock 自带的「模型设置」页：")
		fmt.Println("              dsh-gate plugin add dsh-dock && dsh-gate dsh restart <实例名>")
	}
	return 0
}

// webAppBundle 是挂载浏览器设置面（模型 / 凭据）的那个 bundle。
const webAppBundle = "@deepseek-ai/dsh-web-app"

// pluginAddCommand 把插件装进配置档。
func pluginAddCommand(args []string) int {
	fs := flag.NewFlagSet("plugin add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts pluginOptions
	registerPluginFlags(fs, &opts)
	fs.Usage = func() {
		printPluginUsage(os.Stderr)
		fs.PrintDefaults()
	}
	if err := parseFlexible(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	specs := fs.Args()
	if len(specs) == 0 {
		return usageError("plugin add 需要至少一个包名，例如 dsh-gate plugin add dsh-dock")
	}
	return runPluginAdd(context.Background(), opts, specs)
}

// runPluginAdd 是 plugin add 的安装流程。
func runPluginAdd(parent context.Context, opts pluginOptions, specs []string) int {
	profileDir, err := opts.profilePath()
	if err != nil {
		return usageError("%v", err)
	}
	if _, _, err := loadProfilePackage(profileDir); err != nil {
		return fatal("%v", err)
	}
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	defer cancel()

	// 先把 pnpm 准备好：系统里没有就用网关自备的那份（与 run 里同一条路径）。
	registry := opts.Registry
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = config.DefaultStateDir()
	} else {
		stateDir = config.ExpandPath(stateDir)
	}
	pnpmDir, err := dshproc.EnsurePnpm(ctx, slog.Default(), stateDir, registry)
	if err != nil {
		return fatal("准备 pnpm 失败：%v", err)
	}
	if pnpmDir != "" {
		os.Setenv("PATH", dshproc.PrependPATH(pnpmDir, os.Getenv("PATH")))
		fmt.Printf("使用网关自备的 pnpm：%s\n", pnpmDir)
	}
	pnpm, _ := exec.LookPath("pnpm")
	if pnpm == "" {
		return fatal("找不到 pnpm，无法安装插件")
	}

	added := make([]string, 0, len(specs))
	for _, spec := range specs {
		if err := pnpmAdd(ctx, pnpm, profileDir, spec, registry); err != nil {
			return fatal("%v", err)
		}
		name := packageNameOf(spec)
		if err := registerBundle(profileDir, name); err != nil {
			return fatal("%v", err)
		}
		fmt.Printf("已安装并注册 %s\n", name)
		added = append(added, name)
	}

	if len(added) > 0 {
		fmt.Println()
		fmt.Println("重启 dsh 后生效（profile 解析在进程启动时建立）：")
		fmt.Println("  dsh-gate dsh restart <实例名>   # 重启该实例让插件生效")
	}
	return 0
}

// pnpmAdd 在配置档目录里执行一次 pnpm add。
//
// 先按用户给的源（留空＝npm 默认）装；失败再试镜像一次——服务器上装不动插件
// 最常见的原因就是官方源拉不动。
func pnpmAdd(ctx context.Context, pnpm, dir, spec, registry string) error {
	sources := []string{registry}
	if registry == "" {
		sources = []string{"", dshproc.RegistryMirror}
	}
	var lastErr error
	for i, source := range sources {
		args := []string{"add", spec}
		if source != "" {
			args = append(args, "--registry="+source)
		}
		cmd := exec.CommandContext(ctx, pnpm, args...)
		cmd.Dir = dir
		cmd.Env = npmEnvForPlugin(os.Environ(), source)
		output, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("pnpm add %s 失败：%w\n%s", spec, err, truncateOutput(string(output), 400))
		if i+1 < len(sources) {
			fmt.Fprintf(os.Stderr, "dsh-gate: 这个源装不动 %s，改用镜像源重试\n", spec)
		}
	}
	return lastErr
}

// npmEnvForPlugin 注入 npm 源（与 dshproc 里同一套规则，这里不导出那个内部函数）。
func npmEnvForPlugin(env []string, registry string) []string {
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

// profilePackage 是 dsh 配置档的 package.json 里我们关心的那部分。
type profilePackage struct {
	Name         string            `json:"name,omitempty"`
	Private      bool              `json:"private,omitempty"`
	Dependencies map[string]string `json:"dependencies"`
	DSH          struct {
		Profile struct {
			Bundles []string `json:"bundles"`
		} `json:"profile"`
	} `json:"dsh"`
}

// loadProfilePackage 读取配置档 package.json。
//
// 目录不存在时给的是「先启动一次 dsh」的指引：配置档由 dsh 自己创建，我们
// 凭空造一个可能少挂载点，反而让 dsh 起不来。
func loadProfilePackage(dir string) (profilePackage, string, error) {
	path := filepath.Join(dir, "package.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return profilePackage{}, path, fmt.Errorf("找不到 dsh 配置档 %s：先启动一次 dsh（在管理台启动对应实例）让它生成，再回来装插件", path)
	}
	if err != nil {
		return profilePackage{}, path, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	var pkg profilePackage
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return profilePackage{}, path, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return pkg, path, nil
}

// registerBundle 把包名写进 dsh.profile.bundles（已存在则原样返回）。
func registerBundle(dir, name string) error {
	pkg, path, err := loadProfilePackage(dir)
	if err != nil {
		return err
	}
	if containsString(pkg.DSH.Profile.Bundles, name) {
		return nil
	}
	pkg.DSH.Profile.Bundles = append(pkg.DSH.Profile.Bundles, name)
	return saveProfilePackage(path, pkg)
}

// saveProfilePackage 原子写回 package.json。
//
// 先写临时文件再 rename：这份文件是 dsh 启动时的加载清单，写坏一半等于让
// dsh 起不来。
func saveProfilePackage(path string, pkg profilePackage) error {
	raw, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s 失败: %w", path, err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".dsh-gate-package-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return nil
}

// packageNameOf 从「包名@版本」里取出包名（scope 包要认对 @）。
func packageNameOf(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(spec, "@")
	// scope 包：@scope/name@version——版本分隔符是 name 之后的第一个 @。
	slash := strings.Index(trimmed, "/")
	rest := trimmed
	if slash >= 0 {
		rest = trimmed[slash+1:]
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		spec = spec[:len(spec)-len(rest)+at]
	}
	return spec
}

// containsString 判断列表里有没有某项。
func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// truncateOutput 截断命令输出，避免把整段 pnpm 日志塞进一行错误里。
func truncateOutput(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[len(text)-limit:]
}
