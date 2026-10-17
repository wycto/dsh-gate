package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// serviceCommand 实现 `dsh-gate service`：把网关交给系统的服务管理器托管，
// 从而开机（登录）自动启动。
//
// 三个平台各有各的「官方办法」，这里各写一份，不去抽象一个假的中间层：
//
//	Linux    systemd 单元（需要 root）
//	macOS    launchd 的 LaunchAgent（每个用户一个，登录即启动）
//	Windows  计划任务（登录时启动）
//
// 共同点是：托管的都是 `dsh-gate run`（前台）。服务管理器自己负责重启与日志，
// 网关不必也不该再后台化一次——两套机制各管一半最容易出「谁在重启谁」的烂摊子。
func serviceCommand(args []string) int {
	if len(args) == 0 {
		printServiceHelp(os.Stderr)
		return 2
	}

	action := args[0]
	switch action {
	case "unit", "install", "uninstall", "status":
	case "-h", "--help", "help":
		printServiceHelp(os.Stdout)
		return 0
	default:
		return usageError("未知的 service 子命令 %q，可用：unit / install / uninstall / status", action)
	}

	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, serviceUsageHeader())
		fs.PrintDefaults()
	}

	hold := registerInstanceFlags(fs)
	var (
		configPath string
		user       string
		workDir    string
		selfPath   string
		binDir     string
	)
	fs.StringVar(&configPath, "config", "", "多实例配置文件路径，会写进启动命令")
	fs.StringVar(&user, "user", currentUser(), "以哪个用户身份运行（Linux 用）")
	fs.StringVar(&workDir, "working-dir", defaultWorkingDir(),
		"工作目录。dsh 把它当作会话的默认 workspace")
	fs.StringVar(&selfPath, "binary", "", "dsh-gate 可执行文件的绝对路径（默认用当前运行的真实路径）")
	fs.StringVar(&binDir, "bin-dir", defaultBinDir,
		"把 dsh-gate 拷贝到这个目录（需在 PATH 里），命令就能直接敲；留空则不拷贝")
	manage := fs.Bool("manage", false,
		"托管管理端（manage run）而不是单实例网关（run）")

	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	// 区分「显式给了 --working-dir」与「用默认值」。
	//
	// 这一条踩过坑：--working-dir 的默认值是家目录，于是 workDir 永远非空，
	// workDirOrDefault 里「显式 > workspace > 家目录」的第二档永远轮不到——
	// 实测 `service install --workspace /srv` 写出的单元里 WorkingDirectory 仍是
	// /root，网关进程与 dsh 的工作目录分家。没显式给就清空，交由下面按优先级决定。
	workDirSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "working-dir" {
			workDirSet = true
		}
	})
	if !workDirSet {
		workDir = ""
	}

	opts := serviceOptions{
		Binary:     selfPath,
		User:       user,
		WorkingDir: workDir,
		ConfigPath: configPath,
	}
	if *manage {
		opts.Command = "manage run"
	}
	if opts.Binary == "" {
		// 用自己的真实路径而不是 PATH 查找：服务管理器的环境很干净，写死绝对
		// 路径才不会在某天突然「找不到命令」。
		executable, err := os.Executable()
		if err != nil {
			return fatal("无法确定自身路径，请用 --binary 指定: %v", err)
		}
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		opts.Binary = executable
	}
	if configPath == "" {
		values, err := overrides(fs, hold.trustedHosts)
		if err != nil {
			return usageError("%v", err)
		}
		// 这几个不是实例配置：它们决定「怎么装」而不是「装成什么样」。
		// --manage 也在这里剔除——它已经体现在子命令里，再传一次就是重复。
		for _, key := range []string{"user", "working-dir", "binary", "bin-dir", "manage"} {
			delete(values, key)
		}
		opts.Overrides = values
	}

	switch action {
	case "unit":
		text, err := serviceUnit(opts)
		if err != nil {
			return fatal("%v", err)
		}
		fmt.Print(text)
		return 0
	case "install":
		// 把命令拷进 PATH 目录放在写单元之前：单元里的 ExecStart 用拷贝后的
		// 路径，将来原目录被清理也不影响开机自启。拷不过去只提示，不拦安装。
		if copied := printInstallSelfResult(binDir); copied != "" {
			opts.Binary = copied
		}
		if err := serviceInstall(opts); err != nil {
			return fatal("%v", err)
		}
		// 把「装完 dsh 会在哪工作」直接说出来：服务托管时网关的工作目录是安装时
		// 的默认值（用户家目录），不提示的话，用户会以为会话默认工作目录还是
		// setup 里填的那个——实测正是这样踩的。
		if workspace := opts.Overrides["workspace"]; workspace != "" {
			fmt.Printf("会话默认工作目录  %s\n", workspace)
		} else {
			fmt.Printf("会话默认工作目录  未指定：跟随网关进程的工作目录 %s\n", opts.workDirOrDefault())
			fmt.Println("  想让 agent 默认读写某个目录，加 --workspace <目录> 重装一次服务。")
		}
		fmt.Print(serviceInstalledHint())
		return 0
	case "uninstall":
		// 先读出来它托管的是什么，再删：用户点的是「取消自启」，但真正想知道的
		// 是「刚刚被取消的是哪个实例」——服务单元是机器级的，一个平台只有一个。
		managed := serviceManagedCommand()
		if err := serviceUninstall(opts); err != nil {
			return fatal("%v", err)
		}
		if managed != "" {
			fmt.Printf("已取消开机自启（它托管的命令：%s）\n", managed)
		} else {
			fmt.Println("已取消开机自启。")
		}
		return 0
	case "status":
		return serviceStatus()
	}
	return 2
}

// serviceOptions 是各平台实现共用的输入。
type serviceOptions struct {
	// Binary 是 dsh-gate 可执行文件的绝对路径。
	Binary string
	// User 是以哪个用户身份运行（Linux systemd 用 User=，其它平台忽略）。
	User string
	// WorkingDir 是工作目录，dsh 把它当作会话的默认 workspace。
	WorkingDir string
	// ConfigPath 非空时走多实例配置文件。
	ConfigPath string
	// Overrides 是命令行显式给出的实例选项，键是选项名（不含前导 --）。
	Overrides map[string]string
	// Command 是托管时要跑的子命令，默认 "run"（单实例形态）。
	//
	// 管理端形态要托管的是 "manage run"：它有自己的端口、账号库与实例表，
	// 与单实例形态互不兼容。为空时按 "run" 处理，保持老调用方不变。
	Command string
}

// serviceArgs 返回托管时要执行的完整命令行，含程序名本身。
func (o serviceOptions) serviceArgs() []string {
	command := o.Command
	if command == "" {
		command = "run"
	}
	args := []string{o.Binary, command, "--log-json"}
	if o.ConfigPath != "" {
		return append(args, "--config", o.ConfigPath)
	}
	// 按键排序，保证生成的配置稳定：同一份配置反复生成应当完全一致，否则每次
	// install 都会因为参数顺序不同而看起来「变了」，让人以为没生效。
	keys := make([]string, 0, len(o.Overrides))
	for key := range o.Overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--"+key, o.Overrides[key])
	}
	return args
}

// workDirOrDefault 返回服务的工作目录（写进单元的 WorkingDirectory）。
//
// 优先级：显式的 --working-dir > 实例的 --workspace > 用户家目录。
//
// 中间那一条是实测补上的：用户手敲
// `service install --workspace /srv` 时，只有 ExecStart 里带了 --workspace，
// 而 WorkingDirectory 仍停在安装时的默认（家目录）。结果是网关进程自己的 cwd
// 与 dsh 的会话工作目录分家——凡是「留空 --workspace 时跟随网关进程」的语义，
// 在服务托管下就都指向了 /root 那种地方。setup 已经会转发 --working-dir，
// 但手敲命令这条路径不该依赖它。
//
// 最后才用家目录：它几乎总是「想看到的东西都在附近」，而 "/" 既没意义也不安全。
func (o serviceOptions) workDirOrDefault() string {
	if o.WorkingDir != "" {
		return o.WorkingDir
	}
	if workspace := o.Overrides["workspace"]; workspace != "" {
		return workspace
	}
	return defaultWorkingDir()
}

// serviceName 是服务在各平台统一使用的名字。
const serviceName = "dsh-gate"

// serviceUsageHeader 返回各平台共用的帮助开头。
func serviceUsageHeader() string {
	return fmt.Sprintf(`用法: dsh-gate service <unit|install|uninstall|status> [选项]

把网关交给系统的服务管理器托管，从而开机（登录）自动启动。

  unit        打印将要安装的配置内容，不做任何改动
  install     安装并启用
  uninstall   取消开机自启（机器级：一个平台只有一个服务单元）
  status      查看当前的托管状态

当前平台：%s —— %s

示例:
  sudo dsh-gate service install --dsh-home /var/lib/dsh --port 3082
  dsh-gate service install --config ~/dsh-gate.json
  dsh-gate service status

选项:
`, runtime.GOOS, serviceFlavor())
}

// printServiceHelp 打印完整帮助（含当前平台的特有说明）。
func printServiceHelp(w *os.File) {
	fmt.Fprint(w, serviceUsageHeader())
	fmt.Fprint(w, platformServiceHelp())
}

// currentUser 返回当前用户名。
func currentUser() string {
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	if user := os.Getenv("USERNAME"); user != "" {
		return user
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Base(home)
	}
	return ""
}

// defaultWorkingDir 返回默认工作目录：当前用户的家目录。
func defaultWorkingDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return "/"
}

// quoteForTaskScheduler 把命令行转成 schtasks /TR 需要的写法。
//
// 单独抽出来是因为它是 Windows 那条路径上最容易错、又最难实机验证的一段：
// 本机不是 Windows 也能把它测到。
//
// 规则有两层：
//
//   - 外层那一对引号是 /TR 的分隔符，会被 schtasks 自己吃掉；
//   - 里面**需要保留的**引号必须写成 \"，否则会连同外层一起被剥掉。
//
// 程序路径一定要引起来：dsh-gate 装在 C:\Users\... 或 Program Files 下面时路径
// 几乎必然含空格，不引起来 CreateProcess 会把它当成两个 token，任务启动即失败。
func quoteForTaskScheduler(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		if i == 0 || strings.ContainsAny(arg, " \t") {
			out[i] = `\"` + arg + `\"`
			continue
		}
		out[i] = arg
	}
	return out
}
