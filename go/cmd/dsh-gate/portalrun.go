// 管理端进程的运行时入口：`dsh-gate start` 与 `start --foreground` 都落到这里。
//
// 组装顺序与单实例形态（server.Instance）一致：账号表 → 鉴权网关 → 实例运行时 →
// 管理端 HTTP 层。区别只在「账号表换成 SQLite」「上游从『一个 dsh』变成『按实例查』」。
//
// 门禁仍然是 fail closed：管理端里一个账号都没有就拒绝启动，并直接给出创建账号的命令
// —— 一个「看起来正常但谁都进不去」的服务比启动失败更糟。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/netutil"
	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/portalruntime"
	"github.com/wycto/dsh-gate/internal/registry"
	"github.com/wycto/dsh-gate/internal/version"
)

// portalAssembly 是组装好的管理端：处理器 + 运行时 + 收尾函数。
type portalAssembly struct {
	Handler http.Handler
	Runtime *portalruntime.Runtime
	Store   *portal.Store
	stop    func(context.Context) error
}

// portalRunOptions 是组装管理端所需的事实（测试用它注入假 dsh）。
type portalRunOptions struct {
	DBPath   string
	StateDir string
	// NPMRegistry 是管理台安装 dsh 时用的 npm 源（--npm-registry）。
	NPMRegistry string
	// StartTimeout 是每个实例等它自己的 dsh 端口就绪的最长时间
	// （--dsh-start-timeout）；0 表示用 config 默认值。
	StartTimeout time.Duration
	TLS          bool
	PublicURL    string
	TrustedHosts []string
	Logger       *slog.Logger
	// Launch 仅测试注入；生产走 portalruntime 的默认实现。
	Launch portalruntime.LaunchFunc
	// Shutdown 是收到本机停机请求时的回调（`dsh-gate stop portal` 走这条路）。
	// 为 nil 时停机端点返回 404——测试里就是这么用的。
	Shutdown func()
}

// buildPortal 组装管理端（不监听端口），便于测试直接驱动。
func buildPortal(opts portalRunOptions) (*portalAssembly, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	store, err := portal.Open(opts.DBPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tr(i18n.KeyCLIErrOpenStore, opts.DBPath), err)
	}

	// 迁移：管理端里一个账号都没有、且存在单实例形态的 users.json 时导入它
	// （第一个用户成为管理员）。这是升级路径上最容易把人挡在门外的一步。
	if count, err := store.CountUsers(); err == nil && count == 0 {
		legacy := filepath.Join(opts.StateDir, "users.json")
		result, importErr := store.ImportLegacyUsers(legacy)
		if importErr != nil {
			store.Close()
			return nil, fmt.Errorf("%s: %w", tr(i18n.KeyAdminErrActionFailed, importErr.Error()), importErr)
		}
		if result.Imported > 0 {
			opts.Logger.Info("已从单实例账号表导入用户",
				"file", legacy, "imported", result.Imported, "admin", result.Admin)
		}
	}

	count, err := store.CountUsers()
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("%s: %w", tr(i18n.KeyAdminErrActionFailed, err.Error()), err)
	}
	if count == 0 {
		store.Close()
		return nil, errors.New(tr(i18n.KeyCLIPortalRunNoAccounts, opts.DBPath))
	}

	gate, err := auth.New(auth.Options{
		Users:        store,
		SessionTTL:   7 * 24 * time.Hour,
		MaxFails:     5,
		LockoutBase:  time.Minute,
		CookieSecure: opts.TLS,
		Title:        "dsh-gate",
		Subtitle:     "管理端",
		Version:      version.Version,
		// 管理端的「就绪」指的是管理端自己在服务，而不是某个 dsh 起来了：实例各有各的
		// 就绪状态（页面上逐实例显示），healthz 只回答「这个进程活着吗」。不设它
		// 的话 healthz 会一直 503，健康检查与启动脚本都会误判。
		Ready: func() bool { return true },
		// Shutdown 让 `dsh-gate stop portal` 走通：停机端点是鉴权网关私有的
		// `/__dsh_gate/shutdown`，管理端复用同一个 Gate，不接上这个回调的话
		// stop 只能发信号（Windows 上做不到），登记里的管理端就成了停不掉的孤儿。
		Shutdown: opts.Shutdown,
		Logger:   opts.Logger,
	})
	if err != nil {
		store.Close()
		return nil, err
	}

	runtime, err := portalruntime.New(portalruntime.Options{
		Store:        store,
		TLS:          opts.TLS,
		PublicURL:    opts.PublicURL,
		TrustedHosts: netutil.TrustedHosts(opts.TrustedHosts, opts.PublicURL),
		StartTimeout: opts.StartTimeout,
		Launch:       opts.Launch,
		Logger:       opts.Logger,
	})
	if err != nil {
		store.Close()
		return nil, err
	}

	// 管理台的「创建实例」需要环境检测与安装能力。适配器把 runtime 接到
	// portal.Installer 接口上——两个包不能互相 import（会成环），接口定义在
	// portal 侧、实现在 portalruntime 侧，装配在这里。
	installer := &portalruntime.InstallerAdapter{
		Runtime:  runtime,
		StateDir: opts.StateDir,
		Registry: opts.NPMRegistry,
		Logger:   opts.Logger,
	}

	localToken, tokenErr := writeLocalToken(opts.StateDir)
	if tokenErr != nil {
		logger := opts.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("写入本机控制令牌失败（dsh 命令将无法控制实例）", "err", tokenErr)
	}

	server, err := portal.NewServer(portal.ServerOptions{
		Store:     store,
		Gate:      gate,
		Upstream:  runtime.Upstream,
		Installer: installer,
		// 启停能力单独注入：管理台的启动 / 停止 / 重启按钮靠它。
		Lifespan:   &portalruntime.LifespanAdapter{Runtime: runtime},
		Version:    version.Version,
		Logger:     opts.Logger,
		LocalToken: localToken,
	})
	if err != nil {
		store.Close()
		return nil, err
	}

	return &portalAssembly{
		Handler: server.Handler(),
		Runtime: runtime,
		Store:   store,
		stop: func(ctx context.Context) error {
			runtimeErr := runtime.Stop(ctx)
			closeErr := store.Close()
			if runtimeErr != nil {
				return runtimeErr
			}
			return closeErr
		},
	}, nil
}

// portalRunCommand 是管理端的实际运行路径（start / start --foreground 调用它）。
func portalRunCommand(args []string) int {
	fs := flag.NewFlagSet("portal run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	host := fs.String("host", "0.0.0.0", "管理端监听地址")
	port := fs.Int("port", 12020, "管理端监听端口")
	tlsCert := fs.String("tls-cert", "", "HTTPS 证书（与 --tls-key 成对）")
	tlsKey := fs.String("tls-key", "", "HTTPS 私钥（与 --tls-cert 成对）")
	publicURL := fs.String("public-url", "", "对外公告地址，如 https://dsh.example.com/")
	dbPath := fs.String("db", "", "管理端数据库路径（默认 <状态目录>/portal.db）")
	fs.String("state-dir", "", "网关状态目录")
	logLevel := fs.String("log-level", "info", "日志级别：debug / info / warn / error")
	logJSON := fs.Bool("log-json", false, "以 JSON 输出日志")
	background := fs.Bool("background", false,
		"放到后台运行：关掉终端也继续，日志写入 <登记目录>/logs/portal.log")
	fs.BoolVar(background, "d", false, "同 --background")
	npmRegistry := fs.String("npm-registry", "",
		"管理台安装 dsh 时用的 npm 源；留空用 npm 默认，连不上时自动改用镜像源重试")
	fs.Duration("dsh-start-timeout", config.DefaultDSHStartTimeout,
		"每个实例拉起 dsh 后等它监听端口的最长时间（如 10m）；首次启动要下载 dsh 与插件，太短会边下边被杀")
	var trusted stringList
	fs.Var(&trusted, "trusted-host", "额外信任的地址（管理端域名），可重复")

	if err := parseFlexible(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() > 0 {
		return usageKey(i18n.KeyCLIUnknownPortalSub, fs.Arg(0))
	}

	cfg := config.Default()
	if err := config.ApplyEnv(&cfg, os.Getenv); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	set := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		// 只收显式给出的：没写就让环境变量与默认值各就其位。
		switch f.Name {
		case "state-dir", "dsh-start-timeout":
			set[f.Name] = f.Value.String()
		}
	})
	if err := config.Apply(&cfg, set); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	if err := cfg.Normalize(); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}

	resolvedDB := strings.TrimSpace(*dbPath)
	if resolvedDB == "" {
		resolvedDB = filepath.Join(cfg.StateDirPath(), "portal.db")
	}

	if err := setupLogger(*logLevel, *logJSON); err != nil {
		return usageKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	logger := slog.Default()

	tlsEnabled := strings.TrimSpace(*tlsCert) != "" || strings.TrimSpace(*tlsKey) != ""
	if (strings.TrimSpace(*tlsCert) == "") != (strings.TrimSpace(*tlsKey) == "") {
		return usageKey(i18n.KeyAdminErrActionFailed, "--tls-cert 与 --tls-key 必须成对提供")
	}

	// 后台化：先自检再拉起子进程。
	//
	// 自检放在这里而不是丢给子进程，是因为后台化的子进程把输出全写进日志文件：
	// 「管理端里一个账号都没有」这种失败在父进程里当场说出来，比让人去翻日志
	// （再等 20 秒端口超时）友好得多。检查本身很便宜——只打开数据库数账号。
	if *background {
		if err := checkPortalAccounts(resolvedDB, cfg.StateDirPath()); err != nil {
			return failKey(i18n.KeyCLIPortalRunFailed, err.Error())
		}
		return portalRunInBackground(*port, resolvedDB, args)
	}

	// 可取消的根上下文：停机端点（`dsh-gate stop portal`）调 Shutdown 回调取消它，
	// 与 Ctrl-C 走同一条收尾路径。必须先建好再组装管理端，否则回调拿不到 cancel。
	baseCtx, stopCtx := context.WithCancel(context.Background())
	defer stopCtx()

	// 登记目录：后台分支用它写条目，前台分支（restart 拉起的那个）也用它自登记。
	runDir := resolveRunDir("")

	assembly, err := buildPortal(portalRunOptions{
		DBPath:       resolvedDB,
		StateDir:     cfg.StateDirPath(),
		NPMRegistry:  strings.TrimSpace(*npmRegistry),
		StartTimeout: cfg.DSHStartTimeout,
		TLS:          tlsEnabled,
		PublicURL:    strings.TrimSpace(*publicURL),
		TrustedHosts: trusted,
		Logger:       logger,
		Shutdown:     stopCtx,
	})
	if err != nil {
		return failKey(i18n.KeyCLIPortalRunFailed, err.Error())
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(*host, fmt.Sprintf("%d", *port)))
	if err != nil {
		_ = assembly.stop(context.Background())
		return failKey(i18n.KeyCLIPortalRunFailed, err.Error())
	}

	// 尽早自登记：Node 运行时下载 / 补库可能几十秒，status、stop、dsh list 不该
	// 在这段时间里看不见管理端。登记只需要监听地址与命令行，与后面的准备无关。
	// restart 按登记里的命令行重新拉起，而那条命令行是 `portal run`（不带
	// --background）——前台跑的管理端如果只靠后台分支写登记，重启出来的这个就
	// 谁也不认得。与单实例形态的 Instance.register 对齐：进程自己写、退出时删。
	registerPortalRecord(runDir, listener.Addr().String(), *port, os.Args, tlsEnabled, strings.TrimSpace(*publicURL))
	defer unregisterPortalRecord(runDir)

	ctx, cancel := signal.NotifyContext(baseCtx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 自备 Node.js 运行时：与单实例形态（run.go 的 prepareDSHForRun）同一件事。
	//
	// 管理端原来漏了这一步，于是「服务器上没装 Node」时实例一个也起不来，页面
	// 永远停在「DSH 正在启动」——而单实例形态明明会自己下载。放在拉起实例之前：
	// 运行时就位后再让 supervisor 去试，第一次重试就能用上。
	if binDir, err := dshproc.EnsureNodeRuntime(ctx, logger, cfg.StateDirPath()); err != nil {
		// 不拦启动：管理端要继续提供管理页面，实例自己按退避重试。
		logger.Warn("自备 Node.js 运行时失败（实例可能起不来，可手工安装 Node.js）", "err", err)
	} else if binDir != "" {
		// 必须前置到 PATH：npx 的 shebang 是 #!/usr/bin/env node，只把 npx 自己
		// 塞进命令行、不把它所在的 bin 目录放进 PATH，子进程照样找不到 node。
		os.Setenv("PATH", dshproc.PrependPATH(binDir, os.Getenv("PATH")))
		logger.Info("已自备 Node.js 运行时", "bin", binDir)
	}

	if err := assembly.Runtime.Start(ctx); err != nil {
		logger.Warn("拉起实例时出错（管理端继续提供管理页面）", "err", err)
	}

	instances, _ := assembly.Store.ListInstances()
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	display := *publicURL
	if display == "" {
		display = fmt.Sprintf("%s://%s/", scheme, listener.Addr().String())
	}
	sayKey(os.Stdout, i18n.KeyCLIPortalRunStarted, display, len(instances))

	server := &http.Server{
		Handler: assembly.Handler,
		// 只设读请求头超时：dsh 的回答包含长连接与流式输出，总超时会把会话拦腰砍断。
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		if tlsEnabled {
			serveErr <- server.ServeTLS(listener, *tlsCert, *tlsKey)
			return
		}
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		_ = assembly.stop(context.Background())
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return failKey(i18n.KeyCLIPortalRunFailed, err.Error())
		}
		return 0
	case <-ctx.Done():
		sayKey(os.Stdout, i18n.KeyCLIPortalRunStopping)
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelShutdown()
		_ = server.Shutdown(shutdownCtx)
		if err := assembly.stop(shutdownCtx); err != nil {
			logger.Warn("收尾时出错", "err", err)
		}
		return 0
	}
}

// portalRecordName 是管理端在运行登记里的名字。
//
// 固定而不可配：管理端是机器级的单例（一个管理端数据库对应一个进程），不像单实例
// 形态的 run 那样可以同时跑多个。写死才能让 status / stop 直接按名字找它。
const portalRecordName = "portal"

// checkPortalAccounts 在后台化之前确认管理端里至少有一个账号。
//
// 管理端是 fail closed 的：没有账号的子进程会立刻退出。后台化的子进程把输出写进
// 日志文件，那种失败从父进程看不出来——端口等 20 秒才超时，用户得到的是一句
// 「端口还没就绪」，真正的原因埋在日志里。所以在拉起之前先数一遍账号。
func checkPortalAccounts(dbPath, stateDir string) error {
	store, err := portal.Open(dbPath)
	if err != nil {
		return fmt.Errorf("%s: %w", tr(i18n.KeyCLIErrOpenStore, dbPath), err)
	}
	defer store.Close()

	count, err := store.CountUsers()
	if err != nil {
		return fmt.Errorf("%s: %w", tr(i18n.KeyAdminErrActionFailed, err.Error()), err)
	}
	if count == 0 {
		// 顺手提一句迁移：老版本单实例账号表是可以被管理端认走的。
		legacy := filepath.Join(stateDir, "users.json")
		return fmt.Errorf("%s（或先迁移旧账号表：把 %s 放到管理端状态目录）",
			tr(i18n.KeyCLIPortalRunNoAccounts, dbPath), legacy)
	}
	return nil
}

// portalRunInBackground 把管理端放到后台运行，然后返回。
//
// 与单实例形态的 runInBackground 用同一套机制（spawnDetached 脱离终端、日志写
// 文件、等端口就绪），差别只在**登记**：管理端是机器级单例，登记名固定为 portal，
// 且 Argv 就是这条命令本身，restart 能原样把它再拉起来。
func portalRunInBackground(port int, dbPath string, argv []string) int {
	dir := resolveRunDir("")
	exe, err := os.Executable()
	if err != nil {
		return fatal("无法确定自身路径: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	// 后台化的子进程显式用 `start --foreground` 重新进入：它不能复用调用方的
	// os.Args——从 restart 或 install 转进来时，os.Args 是那条父命令，子进程照抄
	// 会再执行一遍父命令（restart 会无限递归）。去掉 --background 只保留启动参数。
	child := append([]string{"dsh-gate", "start", "--foreground"}, stripBackgroundFlags(argv)...)
	pid, err := spawnDetached(exe, child, logDirFor(dir), portalRecordName)
	if err != nil {
		return fatal("管理端后台启动失败：%v", err)
	}

	// 登记由子进程自己写（registerPortalRecord）：父进程写的 PID 指向的是
	// 那个子进程，而子进程起来后会用 os.Getpid() 覆盖成真实值。这里只等端口。
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if !waitListening(addr, 20*time.Second) {
		fmt.Fprintf(os.Stderr, "管理端已拉起（进程号 %d），但端口 %d 还没就绪，看 %s\n",
			pid, port, filepath.Join(logDirFor(dir), portalRecordName+".log"))
		return 1
	}

	fmt.Printf("管理端已在后台运行（进程号 %d），访问 http://127.0.0.1:%d/\n", pid, port)
	fmt.Printf("  管理台    http://127.0.0.1:%d/admin\n", port)
	fmt.Printf("  日志      %s\n", filepath.Join(logDirFor(dir), portalRecordName+".log"))
	// 登记名是 portal：stop/status 认的固定是它，与命令名无关。
	fmt.Printf("  停止      dsh-gate stop           状态 dsh-gate status\n")
	fmt.Println()
	fmt.Println("管理端没有拉起任何 dsh。添加实例（默认也是按需启动，管理端重启不会自动拉起）:")
	fmt.Println("  dsh-gate dsh add <名字> --dsh-home <目录> --start-mode ondemand")
	return 0
}

// registerPortalRecord 让管理端把自己写进运行登记。
//
// 前台与后台两条路都靠它：后台分支由父进程先等端口就绪，子进程起来后再用自己
// 的 PID 覆盖一次；restart 按登记命令行拉起的那种没有父进程，全靠这里自登记。
// 写失败不影响管理端本身——只影响 status / stop 能不能看见它。
func registerPortalRecord(dir registry.Dir, listen string, port int, argv []string, tls bool, publicURL string) {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	} else if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	record := registry.Record{
		Name:      portalRecordName,
		PID:       os.Getpid(),
		Exe:       exe,
		Argv:      argv,
		Listen:    listen,
		Port:      port,
		DSHPort:   0, // 管理端不托管任何 dsh：实例各自由 portalruntime 拉起
		PublicURL: publicURL,
		TLS:       tls,
		Version:   version.Version,
		StartedAt: time.Now(),
	}
	if err := dir.Save(record); err != nil {
		slog.Default().Warn("写入运行登记失败（不影响管理端本身，status / stop 将看不到它）", "err", err)
	}
}

// unregisterPortalRecord 在管理端退出时删掉自己的登记条目。
func unregisterPortalRecord(dir registry.Dir) {
	if err := dir.Remove(portalRecordName); err != nil {
		slog.Default().Warn("删除运行登记失败", "err", err)
	}
}

// localTokenName 是本机控制 API 的令牌文件名（放在状态目录里，0600）。
const localTokenName = "portal.token"

// writeLocalToken 生成/刷新本机控制令牌。
//
// 每次启动都换一把新钥匙：旧令牌随上一次进程一起失效，文件被留下也不会变成
// 一个永久有效的后门。CLI 每次调用现读文件，所以换钥匙对用户透明。
func writeLocalToken(stateDir string) (string, error) {
	dir := strings.TrimSpace(stateDir)
	if dir == "" {
		return "", errors.New("状态目录为空，无法生成本机控制令牌")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建状态目录 %s 失败: %w", dir, err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机令牌失败: %w", err)
	}
	token := hex.EncodeToString(buf)
	path := filepath.Join(dir, localTokenName)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return token, nil
}
