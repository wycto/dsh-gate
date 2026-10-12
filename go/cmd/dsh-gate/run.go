package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/registry"
	"github.com/wycto/dsh-gate/internal/server"
)

// runCommand 实现 `dsh-gate run`，也是不带子命令时的默认命令。
func runCommand(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate run [选项]

启动网关：监听一个局域网可达的端口，显示登录页，把请求转发给本机的 dsh。
dsh 端口空闲时会把 dsh 一起拉起来；已被占用且是 dsh 时直接接管。

`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
示例:
  dsh-gate run
      默认监听 0.0.0.0:3082，转发到 127.0.0.1:3080，实例目录 ~/.dsh

  dsh-gate run --port 8080 --dsh-port 8081 --dsh-home ~/.dsh-company --name company
      代理公司的 dsh：对外 8080，本机 8081，实例目录独立

  dsh-gate run --config dsh-gate.json
      按配置文件同时启动多个实例
`)
	}

	hold := registerInstanceFlags(fs)
	var (
		configPath string
		logLevel   string
		logJSON    bool
		runDir     string
	)
	fs.StringVar(&runDir, "run-dir", string(registry.DefaultDir()),
		"运行登记目录：status / stop / restart 从这里找正在运行的实例")
	background := fs.Bool("background", false,
		"放到后台运行：关掉终端也继续，日志写入 <登记目录>/logs/<实例名>.log")
	fs.BoolVar(background, "d", false, "同 --background")
	fs.StringVar(&configPath, "config", "", "多实例配置文件（JSON）；使用时不要再指定任何实例选项")
	fs.StringVar(&logLevel, "log-level", "info", "日志级别：debug / info / warn / error")
	fs.BoolVar(&logJSON, "log-json", false, "以 JSON 输出日志")
	allowRootWrite := fs.Bool("allow-root-write", false,
		"允许以 root 身份把数据写进别的用户的目录（默认拒绝：会造出 root 属主的配置，那个用户改不动）")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	if _, err := parseLevel(logLevel); err != nil {
		return usageError("%v", err)
	}
	if err := setupLogger(logLevel, logJSON); err != nil {
		return usageError("%v", err)
	}

	registryDir := resolveRunDir(runDir)

	configs, code := resolveConfigs(fs, configPath, hold)
	if code != 0 {
		return code
	}

	// 以 root 写进别人的目录会造出 root 属主的 settings.yaml / .credentials.yaml，
	// 那个用户自己的 dsh（含桌面端）就再也读不了、改不动。建目录之前先拦。
	if code := guardRootWrites(configs, *allowRootWrite); code != 0 {
		return code
	}

	// 该有的目录在这里先建齐：状态目录、dsh 实例目录、会话工作目录。
	//
	// 以前谁用谁建（账号表建状态目录、下载运行时建 <状态目录>/node），于是
	// `--dsh-home` 与 `--workspace` 指向一个新目录时**没人创建**，dsh 起来后
	// 才以各种面目失败（写不进配置、cwd 不存在）。用户反馈的原话是「如果输入的
	// 目录不存在，要自动创建」——就放在这里，一次做全，并且把建了什么说出来。
	if code := prepareDirectories(configs); code != 0 {
		return code
	}

	// Ctrl-C 从这里开始就有效：准备 dsh 可能耗时数分钟，期间应当可以取消。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *background {
		return runInBackground(configs, os.Args, configPath, registryDir)
	}

	runner, err := server.NewRunner(configs, server.RunOptions{
		Subtitle: subtitleFor(configs),
		Registry: registryDir,
		Argv:     os.Args,
	})
	if err != nil {
		return fatal("%v", err)
	}

	// 先把端口打开、把服务跑起来，再在后台准备 dsh。反过来（先同步准备）的话，
	// 首次启动那几分钟里端口没开、登录页打不开、`status` 也看不到实例——实测
	// 就有人以为「服务没起来」。
	if err := runner.Start(ctx); err != nil {
		return fatal("%v", err)
	}
	fmt.Print(runner.Banner())
	fmt.Println("按 Ctrl-C 停止。")
	awaitNativeURLs(ctx, runner)

	// dsh 的准备不阻塞服务：备好后命令会交到各实例手上，下一次重试就用它。
	go prepareDSHForRun(ctx, configs, runner)

	// Serve 返回的时机由信号决定；dsh 启动失败不会让网关退出——那正是
	// 「网关在线、dsh 慢慢总能起来」想要的。
	//
	// 两条停机路径：本地信号（Ctrl-C / systemctl stop）与 `dsh-gate stop` 发来的
	// 控制请求。后者会让 Serve 先返回，所以**谁读到 channel 谁负责收尾**——
	// 曾经在 select 之后又统一 `<-serveErr` 等一次，结果控制请求这条路把它读走之后
	// 卡在第二次读上，进程永远退不出（端口关了、进程还在，`ps` 里越积越多）。
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- runner.Serve(ctx) }()

	var serveErr error
	select {
	case <-ctx.Done():
		shutdownRunner(runner)
		serveErr = <-serveErrCh
	case serveErr = <-serveErrCh:
		shutdownRunner(runner)
	}
	if serveErr != nil {
		return fatal("%v", serveErr)
	}
	fmt.Println("已停止。")
	return 0
}

// prepareDSHForRun 在后台确保 dsh 可用，并把冒烟验证过的命令交给运行中的实例。
//
// 为什么放到后台：首次启动要先下载 Node.js 运行时、再由 npx 拉 dsh，可能好几分钟。
// 同步做的话这几分钟里网关端口没打开、登录页打不开、`status` 也看不到实例——
// 「网关一直在线、dsh 慢慢总能起来」是本项目对无界面服务器的承诺，准备期间也该
// 兑现。准备好了就调 runner.SetDSHCommand，各实例下一次重试 dsh 时自动用上。
//
// 失败不拦启动：原因打在 stderr（systemd 下进 journal），等待页面继续显示
// supervisor 给出的最近一次失败原因，dsh 会按退避继续重试。
func prepareDSHForRun(ctx context.Context, configs []config.Instance, runner *server.Runner) {
	instance := -1
	for i, cfg := range configs {
		if cfg.AutoStart && cfg.DSHCommand == "" {
			instance = i
			break
		}
	}
	if instance < 0 {
		return
	}

	// 没有 Node.js 就先自备一个：dsh 的官方启动方式是 npx，而「服务器上没装
	// Node.js」是第一次使用最常见的拦路虎。运行时放在网关状态目录里——不改
	// 系统、不需要 root，卸载时随 --purge 一起消失。
	binDir, err := dshproc.EnsureNodeRuntime(ctx, slog.Default(), configs[instance].StateDirPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsh-gate: %v\n", err)
	} else if binDir != "" {
		// 必须前置到 PATH：npx 的 shebang 是 `#!/usr/bin/env node`，只把它
		// 自己塞进命令行，子进程照样找不到 node。
		os.Setenv("PATH", dshproc.PrependPATH(binDir, os.Getenv("PATH")))
		fmt.Fprintf(os.Stderr, "已自备 Node.js 运行时：%s（随 dsh-gate uninstall --purge 一并删除）\n", binDir)
	}
	if ctx.Err() != nil {
		return // 网关已经停了，没必要再去拉 dsh
	}

	// dsh 装插件走的是 pnpm，Node.js 自带的只有 npm/npx；缺了它，页面上点
	// 「添加插件」只会得到 `spawn pnpm ENOENT`。**先于 dsh 准备**：supervisor
	// 的盲探测可能在后台准备完成之前就把 dsh 拉起来了，而 dsh 子进程的环境是
	// 拉起那一刻定下的——晚一步装好的 pnpm 它看不见。
	registry := configs[instance].NPMRegistry
	pnpmFailed := false
	if pnpmDir, err := dshproc.EnsurePnpm(ctx, slog.Default(), configs[instance].StateDirPath(), registry); err != nil {
		pnpmFailed = true
		fmt.Fprintf(os.Stderr, "dsh-gate: %v\n（dsh 本身不受影响；只是插件安装会用不了。）\n", err)
	} else if pnpmDir != "" {
		os.Setenv("PATH", dshproc.PrependPATH(pnpmDir, os.Getenv("PATH")))
		fmt.Fprintf(os.Stderr, "已自备 pnpm：%s（随 dsh-gate uninstall --purge 一并删除）\n", pnpmDir)
	}

	command, err := dshproc.EnsureDSH(ctx, slog.Default(), registry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsh-gate: %v\n网关会照常运行；dsh 就绪前登录页能打开但暂时无法使用。\n", err)
		return
	}
	// pnpm 那一步失败、而 dsh 这边换过源（说明是源的问题）时，用实际通的源
	// 再试一次——装不上 pnpm 的常见原因就是依赖的那个源拉不动。
	if pnpmFailed && command.Registry != registry {
		if pnpmDir, err := dshproc.EnsurePnpm(ctx, slog.Default(), configs[instance].StateDirPath(), command.Registry); err != nil {
			fmt.Fprintf(os.Stderr, "dsh-gate: %v\n（dsh 本身不受影响；只是插件安装会用不了。）\n", err)
		} else if pnpmDir != "" {
			os.Setenv("PATH", dshproc.PrependPATH(pnpmDir, os.Getenv("PATH")))
			fmt.Fprintf(os.Stderr, "已自备 pnpm：%s（随 dsh-gate uninstall --purge 一并删除）\n", pnpmDir)
		}
	}

	runner.SetDSHCommand(command)
	fmt.Fprintf(os.Stderr, "dsh 启动命令已就绪（%s），等待页面稍后会自动进入登录页。\n", command.Command)
}

// awaitNativeURLs 在前台运行时盯着各实例的 dsh 启动令牌。
//
// 横幅打印的那一刻 dsh 往往还在启动，带令牌的本机直连地址要等 dsh 把自己
// 的启动地址吐出来才学得到——此前它只会出现在结构化日志里，人得去翻。
// 这里逐个实例等待，谁的令牌先就绪就立刻用横幅同款格式补打到 stdout；
// 横幅里已经带直连地址的实例（dsh 本来就在跑）直接跳过。
func awaitNativeURLs(ctx context.Context, runner *server.Runner) {
	for _, instance := range runner.Instances() {
		if instance.NativeURL() != "" {
			continue
		}
		go func(instance *server.Instance) {
			ticker := time.NewTicker(300 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				if url := instance.NativeURL(); url != "" {
					printNativeURLReady(instance.Name(), url)
					return
				}
			}
		}(instance)
	}
}

// printNativeURLReady 把就绪的本机直连地址打成一 个横幅续块。
func printNativeURLReady(name, url string) {
	fmt.Printf("\n实例 %s · dsh 已就绪\n", name)
	fmt.Printf("  本机直连  %s（绕过网关，仅本机可用）\n", url)
}

// runInBackground 把网关放到后台启动，然后返回。
//
// 为什么要有这个：前台运行时关掉终端（或 macOS 终端的「关闭标签页」）网关就没了，
// 很多人第一次用都会踩。前台该看日志时用默认行为，要长期挂着就加 --background。
//
// 做法是**重新执行自己**、去掉 --background 后脱离终端拉起，而不是在当前进程里
// fork：当前进程要打印结果然后退出，而网关必须活到它之后。
func runInBackground(configs []config.Instance, argv []string, configPath string, dir registry.Dir) int {
	logDir := logDirFor(dir)
	exe, err := os.Executable()
	if err != nil {
		return fatal("无法确定自身路径: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// 每个实例起**独立的一个进程**，而不是「一个进程托管全部」。
	//
	// 理由是可管理性：`stop` / `restart` 都是按实例来的，而一个进程只能整体停。
	// 后台跑的时候给每个实例一个进程，三条命令的语义就一致了。
	// 走配置文件时靠 `--config FILE --name <实例名>` 把整份配置收窄成一个实例。
	baseArgv := stripBackgroundFlags(argv)
	failed := 0
	for _, cfg := range configs {
		// 已经在跑就别再起一个：同名实例会抢端口，报错比让用户看两个进程互相
		// 打架要好。
		if record, ok, _ := dir.Load(cfg.Name); ok && record.Alive() {
			fmt.Fprintf(os.Stderr, "实例 %s 已经在运行（网关端口 %s），要重启请用 dsh-gate restart %s\n",
				cfg.Name, record.Listen, cfg.Name)
			failed++
			continue
		}

		spawnArgv := withName(baseArgv, cfg.Name)
		if configPath != "" {
			// 命令行里没有实例参数（它们在配置文件里），因此单独拼一条：
			// 只带上「跑哪个配置文件、跑其中哪个实例」加上启动方式相关的选项。
			spawnArgv = []string{exe, "run", "--config", configPath, "--name", cfg.Name}
			spawnArgv = append(spawnArgv, runModeArgs(argv)...)
		}
		pid, err := spawnDetached(exe, spawnArgv, logDir, cfg.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "实例 %s 启动失败：%v\n", cfg.Name, err)
			failed++
			continue
		}

		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
		if !waitListening(addr, 20*time.Second) {
			fmt.Fprintf(os.Stderr, "实例 %s 已拉起（进程号 %d），但网关端口 %d 还没就绪，看 %s\n",
				cfg.Name, pid, cfg.Port, filepath.Join(logDir, cfg.Name+".log"))
			continue
		}
		fmt.Printf("实例 %s 已在后台运行（进程号 %d），访问 http://127.0.0.1:%d/\n", cfg.Name, pid, cfg.Port)
		fmt.Printf("  日志 %s\n", filepath.Join(logDir, cfg.Name+".log"))
		fmt.Printf("  停止 dsh-gate stop %s    状态 dsh-gate status\n", cfg.Name)
	}

	if failed > 0 {
		return 1
	}
	fmt.Println()
	fmt.Println("提示：开机自启用 dsh-gate service install（当前平台会生成对应的托管配置）")
	return 0
}

// stripBackgroundFlags 从命令行里去掉后台开关。
//
// 子进程必须**不带** --background，否则它会再 forks 一个自己没有监听的网关，
// 然后打印「已在后台运行」退出——父进程等端口就绪时永远等不到。
func stripBackgroundFlags(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		switch arg {
		case "--background", "-d", "--background=true":
			continue
		case "--background=false":
			continue
		}
		out = append(out, arg)
	}
	return out
}

// resolveConfigs 组装本次要运行的实例配置。
func resolveConfigs(fs *flag.FlagSet, configPath string, hold *instanceFlags) ([]config.Instance, int) {
	if configPath != "" {
		// 配置文件已经是一份完整的多实例定义，再叠加命令行只会让「到底哪个
		// 值生效」变得难以回答。直接拒绝，并把提示说清楚。
		var extra []string
		fs.Visit(func(f *flag.Flag) {
			// 只拦「实例选项」；--background、--log-level 这类启动方式
			// 与配置文件不冲突，应当照常可用。
			//
			// --name 是唯一的例外：它在这里不是「配置实例」，而是**从配置里挑一个**。
			// 后台化时要给每个实例起一个独立进程，就靠它把整份配置收窄成一个实例。
			if !isRunModeFlag(f.Name) && f.Name != "name" {
				extra = append(extra, "--"+f.Name)
			}
		})
		if len(extra) > 0 {
			return nil, usageError("使用 --config 时不能再指定 %s；请把它们写进配置文件",
				strings.Join(extra, " "))
		}
		instances, err := config.LoadFile(configPath)
		if err != nil {
			return nil, fatal("%v", err)
		}
		// 没显式写 --name 就不筛：那种情况是「按配置跑全部实例」。
		if flagDef := fs.Lookup("name"); flagDef != nil {
			if wanted := strings.TrimSpace(flagDef.Value.String()); wanted != "" && wanted != config.DefaultName {
				for _, instance := range instances {
					if instance.Name == wanted {
						return []config.Instance{instance}, 0
					}
				}
				return nil, usageError("配置文件里没有名为 %q 的实例", wanted)
			}
		}
		return instances, 0
	}

	cfg := config.Default()
	if err := config.ApplyEnv(&cfg, os.Getenv); err != nil {
		return nil, usageError("%v", err)
	}
	values, err := overrides(fs, hold.trustedHosts)
	if err != nil {
		return nil, usageError("%v", err)
	}
	if err := config.Apply(&cfg, values); err != nil {
		return nil, usageError("%v", err)
	}
	if err := cfg.Normalize(); err != nil {
		return nil, usageError("%v", err)
	}
	return []config.Instance{cfg}, 0
}

// subtitleFor 生成登录页副标题。
func subtitleFor(configs []config.Instance) string {
	if len(configs) != 1 {
		return "请登录后访问你的 DSH 实例"
	}
	return "请登录后访问你的 DSH 实例 · " + configs[0].Name
}

// shutdownRunner 优雅停机。
//
// 先给 dsh 一点时间自己收尾（dsh 收到 SIGTERM 后会 dispose 插件树，最多 5 秒），
// 超时才强杀。对 systemd 来说这点宽限很关键：否则每次 restart 都会留下一个
// 半写的会话索引。
func shutdownRunner(runner *server.Runner) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := runner.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "dsh-gate: 停止时出现问题: %v\n", err)
	}
}

// runModeArgs 从原始命令行里挑出「怎么启动」那一类选项，供后台化的子进程沿用。
//
// 实例参数不能带过去：走 --config 时它们必须写进配置文件，而子进程只认
// 「跑哪份配置、跑哪个实例」。
func runModeArgs(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		name, inline, hasInline := strings.Cut(strings.TrimLeft(argv[i], "-"), "=")
		if !strings.HasPrefix(argv[i], "-") || !isRunModeFlag(name) {
			continue
		}
		switch name {
		case "config", "background", "d":
			continue // 这两个由调用方决定，不照抄
		}
		if hasInline {
			out = append(out, "--"+name+"="+inline)
			continue
		}
		out = append(out, "--"+name)
		if i+1 < len(argv) {
			i++
			out = append(out, argv[i])
		}
	}
	return out
}

// preparedDir 是一个要预先建好的目录。
type preparedDir struct {
	label string
	path  string
	mode  os.FileMode
}

// prepareDirectories 建齐本次运行需要的目录：网关状态目录、dsh 实例目录，
// 以及（若指定了）会话工作目录。
//
// 只在缺失时创建，已存在则一个字节都不动；建不出来直接报错退出——目录不存在，
// 后面每一步都会以更难懂的方式失败。
func prepareDirectories(configs []config.Instance) int {
	for _, cfg := range configs {
		dirs := []preparedDir{
			{"网关状态目录", cfg.StateDirPath(), 0o700},
			{"dsh 实例目录", cfg.DSHHome, 0o700},
		}
		if cfg.Workspace != "" {
			dirs = append(dirs, preparedDir{"会话工作目录", cfg.Workspace, 0o755})
		}
		for _, item := range dirs {
			if item.path == "" {
				continue
			}
			if _, err := os.Stat(item.path); err == nil {
				continue
			}
			if err := os.MkdirAll(item.path, item.mode); err != nil {
				return fatal("创建 %s %s 失败: %v", item.label, item.path, err)
			}
			fmt.Printf("已创建 %s %s\n", item.label, item.path)
		}
	}
	return 0
}
