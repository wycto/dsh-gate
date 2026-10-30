package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/i18n"
)

// serviceCommand 实现 `dsh-gate service`：把**管理端**交给系统的服务管理器托管，
// 从而开机（登录）自动启动。
//
// 三个平台各有各的「官方办法」，这里各写一份，不去抽象一个假的中间层：
//
//	Linux    systemd 单元（需要 root）
//	macOS    launchd 的 LaunchAgent（每个用户一个，登录即启动）
//	Windows  计划任务（登录时启动）
//
// 托管的命令固定是 `dsh-gate start --foreground`：服务管理器自己负责重启与日志，
// 管理端不必也不该再后台化一次——两套机制各管一半最容易出「谁在重启谁」。
func serviceCommand(args []string) int {
	if len(args) == 0 {
		printServiceHelp(os.Stderr)
		return 2
	}

	action := args[0]
	switch action {
	case "unit", "install", "uninstall", "status":
	case "run":
		// 服务管理器实际执行的入口（Windows 计划任务用）。用户不必直接敲它，
		// 所以帮助里只留一行说明：与 start --foreground 的区别是把控制台摘掉。
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

	var (
		user     string
		workDir  string
		selfPath string
		binDir   string
		tray     bool
	)
	fs.StringVar(&user, "user", currentUser(), "以哪个用户身份运行（Linux 用）")
	fs.StringVar(&workDir, "working-dir", "", "管理端进程的工作目录（默认用户家目录）")
	fs.StringVar(&selfPath, "binary", "", "dsh-gate 可执行文件的绝对路径（默认用当前运行的真实路径）")
	fs.StringVar(&binDir, "bin-dir", binDirDefault(),
		"把 dsh-gate 拷贝到这个目录（需在 PATH 里），命令就能直接敲；留空则不拷贝")
	fs.BoolVar(&tray, "tray", false,
		"Windows 专用：开机自启改成系统托盘程序，登录时起托盘、由它保证管理端在跑（不再有终端窗口）")
	pf := registerPortalFlags(fs)

	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	if tray && runtime.GOOS != "windows" {
		// 不静默忽略：用户明确要的是「不要那个终端窗口」，在 Linux / macOS 上
		// 根本没有窗口这回事，服务管理器自己就把进程放在没有终端的会话里。
		return usageKey(i18n.KeyCLITrayUnsupported)
	}

	opts := serviceOptions{
		Binary:     selfPath,
		User:       user,
		WorkingDir: workDir,
		Tray:       tray,
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

	stateDir, err := resolveStateDir(pf.stateDir)
	if err != nil {
		return fatal("%v", err)
	}
	overrides := map[string]string{
		"host":      pf.host,
		"port":      fmt.Sprintf("%d", pf.port),
		"state-dir": stateDir,
	}
	if strings.TrimSpace(pf.tlsCert) != "" {
		overrides["tls-cert"] = strings.TrimSpace(pf.tlsCert)
	}
	if strings.TrimSpace(pf.tlsKey) != "" {
		overrides["tls-key"] = strings.TrimSpace(pf.tlsKey)
	}
	if strings.TrimSpace(pf.publicURL) != "" {
		overrides["public-url"] = strings.TrimSpace(pf.publicURL)
	}
	if strings.TrimSpace(pf.dbPath) != "" {
		overrides["db"] = strings.TrimSpace(pf.dbPath)
	}
	if strings.TrimSpace(pf.npmRegistry) != "" {
		overrides["npm-registry"] = strings.TrimSpace(pf.npmRegistry)
	}
	if pf.startTimeout != config.DefaultDSHStartTimeout {
		// 与默认值相同时不写进单元：将来调默认值能惠及已经装好的自启，
		// 而用户点名过的时长必须原样带进 ExecStart。
		overrides["dsh-start-timeout"] = pf.startTimeout.String()
	}
	if len(pf.trusted) > 0 {
		// trusted-host 可重复，用逗号拼成一个值：start 侧会再拆开。
		overrides["trusted-host"] = strings.Join(pf.trusted, ",")
	}
	opts.Overrides = overrides

	switch action {
	case "unit":
		text, err := serviceUnit(opts)
		if err != nil {
			return fatal("%v", err)
		}
		fmt.Print(text)
		return 0
	case "run":
		return serviceRun(pf, stateDir)
	case "install":
		// 把命令拷进 PATH 目录放在写单元之前：单元里的 ExecStart 用拷贝后的
		// 路径，将来原目录被清理也不影响开机自启。拷不过去只提示，不拦安装。
		if copied := printInstallSelfResult(binDir); copied != "" {
			opts.Binary = copied
		}
		if tray {
			// 托盘是另一个二进制（GUI 子系统，见 build.sh），计划任务指的就是它：
			// 没有它就没有托盘形态，这一步失败必须拦住安装——装了个跑不起来的任务
			// 比装不上更糟。
			trayPath, err := installTraySiblingToBinDir(binDir, opts.Binary)
			if err != nil {
				return fatal("%v", err)
			}
			opts.TrayBinary = trayPath
		}
		if err := serviceInstall(opts); err != nil {
			return fatal("%v", err)
		}
		fmt.Print(serviceInstalledHint(opts))
		return 0
	case "uninstall":
		// 先读出来它托管的是什么，再删：用户点的是「取消自启」，但真正想知道的
		// 是「刚刚被取消的是哪个命令」。
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

// serviceRun 是服务管理器实际执行的启动命令：参数与 `start --foreground` 相同。
//
// 为什么不直接让计划任务执行 `start --foreground`：见 taskArgs 的注释。
//
// Windows 上它只当「启动器」：派一个没有控制台的子进程去当管理端，自己立刻退出
// （见 detachServiceConsole）。这样关掉那个终端窗口只会关掉一个已经结束的进程，
// 服务不受影响——而挂在控制台上的进程被关掉窗口时，控制台宿主是一定会把它一起
// 结束的，屏蔽信号也拦不住。
func serviceRun(pf *portalFlags, stateDir string) int {
	hideServiceConsole()
	args := pf.startArgs(stateDir)
	if !detachServiceConsole() {
		return portalRunCommand(args)
	}
	// 已经有一个在跑就安静退出：计划任务的「失败重启」和手工 `schtasks /Run` 都会
	// 二次拉起，派出去的那个只会 bind 失败，留下一条没人看的报错。
	if record, running := portalRecord(resolveRunDir("")); running {
		fmt.Printf("管理端已经在运行（%s，进程号 %d），不再拉起。\n", record.Listen, record.PID)
		return 0
	}
	return portalRunCommand(append(args, "--background"))
}

// serviceOptions 是各平台实现共用的输入。
type serviceOptions struct {
	// Binary 是 dsh-gate 可执行文件的绝对路径。
	Binary string
	// User 是以哪个用户身份运行（Linux systemd 用 User=，其它平台忽略）。
	User string
	// WorkingDir 是管理端进程的工作目录。
	WorkingDir string
	// Overrides 是传给 `start` 的选项，键是选项名（不含前导 --）。
	Overrides map[string]string
	// Tray 表示 Windows 上把计划任务的动作换成托盘程序（GUI 子系统，没有终端窗口）。
	Tray bool
	// TrayBinary 是托盘可执行文件的绝对路径，只有 Tray 为真时有值。
	TrayBinary string
}

// serviceArgs 返回托管时要执行的完整命令行，含程序名本身。
//
// Linux / macOS 的服务管理器自己会处理「不要挂终端」这件事（systemd 没有控制台、
// launchd 的进程也不连终端），所以直接执行 `start --foreground` 即可；Windows 的
// 计划任务会给控制台程序开一个可见窗口，必须换成 `service run`（见 taskArgs）。
func (o serviceOptions) serviceArgs() []string {
	return o.argsWith([]string{o.Binary, "start", "--foreground"})
}

// taskArgs 返回 Windows 计划任务要执行的命令行。
//
// 默认用 `service run` 而不是 `start --foreground`：计划任务以交互身份启动控制台程序时
// 会新开一个终端窗口，用户手一关就是 Ctrl-Close，管理端跟着退出——这正是「启动后
// 又开了一个终端，关掉就访问不到了」的原因。`service run` 会先把控制台隐藏掉，
// 再把管理端派进没有控制台的后代进程。
//
// `--tray` 形态下动作换成托盘程序：它是 GUI 子系统（-H=windowsgui）的那份二进制，
// 系统从一开始就不会给它开控制台，于是连「闪一下」都没有，取而代之的是通知区里的
// 图标。启动参数照旧传过去，由它在需要拉起管理端时原样使用。
func (o serviceOptions) taskArgs() []string {
	if o.Tray && o.TrayBinary != "" {
		return o.argsWith([]string{o.TrayBinary, "tray"})
	}
	return o.argsWith([]string{o.Binary, "service", "run"})
}

// argsWith 在固定前缀之后按稳定顺序拼上要托管的选项。
func (o serviceOptions) argsWith(prefix []string) []string {
	args := append([]string{}, prefix...)
	// 按键排序，保证生成的配置稳定：同一份配置反复生成应当完全一致，否则每次
	// install 都会因为参数顺序不同而看起来「变了」，让人以为没生效。
	keys := make([]string, 0, len(o.Overrides))
	for key := range o.Overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := o.Overrides[key]
		if key == "trusted-host" {
			for _, host := range strings.Split(value, ",") {
				if strings.TrimSpace(host) != "" {
					args = append(args, "--trusted-host", strings.TrimSpace(host))
				}
			}
			continue
		}
		args = append(args, "--"+key, value)
	}
	return args
}

// workDirOrDefault 返回服务的工作目录（写进单元的 WorkingDirectory）。
func (o serviceOptions) workDirOrDefault() string {
	if o.WorkingDir != "" {
		return o.WorkingDir
	}
	return defaultWorkingDir()
}

// serviceName 是服务在各平台统一使用的名字。
const serviceName = "dsh-gate"

// serviceUsageHeader 返回各平台共用的帮助开头。
func serviceUsageHeader() string {
	return fmt.Sprintf(`用法: dsh-gate service <unit|install|uninstall|status> [选项]

把管理端交给系统的服务管理器托管，从而开机（登录）自动启动。
托管的命令是 `+"`dsh-gate start --foreground`"+`（Windows 上是 `+"`dsh-gate service run`"+`，
它先把控制台隐藏掉；加 --tray 则改成托盘程序），参数就是下面这些。

  unit        打印将要安装的配置内容，不做任何改动
  install     安装并启用（立即启动）
  uninstall   取消开机自启（机器级：一个平台只有一个服务单元）
  status      查看当前的托管状态
  run         服务管理器内部使用：启动管理端并隐藏控制台（Windows 计划任务用）

当前平台：%s —— %s

示例:
  sudo dsh-gate service install --host 0.0.0.0 --port 12020
  dsh-gate service install --port 8080 --tls-cert cert.pem --tls-key key.pem
  dsh-gate service install --tray        Windows：登录时起托盘，没有终端窗口
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
