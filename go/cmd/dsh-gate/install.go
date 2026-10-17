// `dsh-gate install`：安装管理端。
//
// 与 setup 的分工：setup 是「单实例形态」的一条龙（建账号 + 后台启动 + 自启），
// 起来的是一个网关带一个 dsh；install 是「管理端形态」的入口，装完之后**一个
// dsh 都不拉起**，实例由用户在 /admin 里按需添加。两者互不覆盖，文档各写各的。
//
// 交互向导的顺序刻意把「环境」放在「人」前面：监听地址、端口、安装目录、
// 配置与数据库目录都是装之前就要定下来的事实，而管理员账号密码是装完之后
// 第一次登录才用到的东西。先问环境，用户对「装的是什么」有概念了，再问账号。
//
// 动作每一步都做成可单独复用的：
//
//  1. 逐项询问（host → 端口 → 安装目录 → 配置与数据库目录 → 账号 → 口令 →
//     后台启动 → 开机自启 → 汇总确认）
//  2. 建管理端管理员（portal.db 里一个账号都没有时；已有账号就原样不动）
//  3. 把命令拷进 PATH（--bin-dir）
//  4. 按选择转交 manage run --background / service install --manage
//
// 第 4 步不自己造启动逻辑，而是走一次真实的进程边界去执行子命令——后台化的
// 登记、日志、停机端点、服务单元都在那边实现一次，install 只是调用方。
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/term"
)

// installCommand 实现 `dsh-gate install`。
func installCommand(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, tr(i18n.KeyCLIInstallUsage))
		fmt.Fprintln(os.Stderr)
	}

	host := fs.String("host", "0.0.0.0", "管理端监听地址")
	port := fs.Int("port", 3082, "管理端监听端口")
	stateDir := fs.String("state-dir", "", "配置与数据库目录（默认 ~/.dsh-gate）")
	admin := fs.String("admin", "admin", "管理员用户名")
	randomMode := fs.Bool("random", false, "自动生成强口令并打印一次")
	fromStdin := fs.Bool("password-stdin", false, "从标准输入读取口令")
	fs.Bool("start", false, "安装完成后立即把管理端放到后台运行")
	fs.Bool("service", false, "安装开机（登录）自启并立即启动（蕴含 --start）")
	binDirFlag := fs.String("bin-dir", defaultBinDir,
		"把 dsh-gate 拷贝到这个目录（需在 PATH 里）；留空则不拷贝")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		return usageKey(i18n.KeyCLIInstallBadFlag, fs.Arg(0))
	}

	interactive := term.IsTerminal(os.Stdin.Fd())

	// 口令的来源在动手之前定清楚：非交互又没给来源，等问完一圈才失败最糟。
	// 只在「需要建账号」时才强制——库里已有账号时不问口令，也就不需要来源。
	needPassword := false
	if db, err := portalDBPath(strings.TrimSpace(*stateDir)); err == nil {
		if store, err := portal.Open(db); err == nil {
			count, _ := store.CountUsers()
			needPassword = count == 0
			_ = store.Close()
		}
	}
	if !interactive && needPassword && !*randomMode && !*fromStdin {
		return usageKey(i18n.KeyCLIInstallNeedPassword)
	}
	if *randomMode && *fromStdin {
		return usageError("--random 与 --password-stdin 只能用一个")
	}
	// 端口范围属于参数合法性，先按用法错误挡掉（退出码 2）；
	// 占用是运行环境的事实，由向导里那套检测负责。
	if *port < 1 || *port > 65535 {
		return usageError("端口 %d 超出范围（1–65535）", *port)
	}

	// 交互式向导：逐项问、端口当场查占用、答完汇总确认。
	// 选项显式给过的项不再问（与 setup 的 collectOverrides 一致）。
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	settings, err := collectInstallSettings(fs, visited, *host, *port, *stateDir, *admin, interactive, needPassword, *randomMode, *fromStdin)
	if err != nil {
		return fatal("%v", err)
	}

	dbPath, err := portalDBPath(settings.stateDir)
	if err != nil {
		return fatal("%v", err)
	}

	store, err := portal.Open(dbPath)
	if err != nil {
		return fatal("%s", tr(i18n.KeyCLIErrOpenStore, dbPath))
	}
	defer store.Close()

	// 账号：只在管理端里一个账号都没有时创建。这是初始化语义——重复执行
	// install 不应该有「顺手改了谁口令」的惊喜，与 setup 对账号表的态度一致。
	created := false
	count, err := store.CountUsers()
	if err != nil {
		return fatal("%s", tr(i18n.KeyAdminErrActionFailed, err.Error()))
	}
	if count == 0 {
		password := settings.password
		if password == "" {
			// 脚本模式（或口令来源是 --random / --password-stdin）时向导没问到，
			// 这里按来源取一次。
			password, err = acquirePassword(*randomMode, *fromStdin)
			if err != nil {
				return fatal("%v", err)
			}
		}
		if _, err := store.CreateUser(settings.admin, password, portal.RoleAdmin); err != nil {
			if errors.Is(err, portal.ErrUserExists) {
				return failKey(i18n.KeyCLIErrUserExists, settings.admin)
			}
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		created = true
		if count2, err := store.CountUsers(); err == nil {
			count = count2
		}
		if *randomMode {
			fmt.Println()
			sayKey(os.Stdout, i18n.KeyCLIRandomNotice)
			fmt.Println("    " + password)
		}
	}
	if created {
		sayKey(os.Stdout, i18n.KeyCLIInstallCreated, settings.admin, count)
	} else {
		sayKey(os.Stdout, i18n.KeyCLIInstallExists, count)
	}

	// 命令进 PATH：装完还得敲 ./dsh-gate 的话，install 只算做了一半。
	// 选了自启就交给 service install 一起做——它会把拷贝后的路径写进单元。
	binDir := settings.binDir
	if binDir == "" {
		binDir = strings.TrimSpace(*binDirFlag)
	}
	if !settings.service {
		printInstallSelfResult(binDir)
	}

	code := 0
	switch {
	case settings.service:
		// service install 自己就会立即启动，选了它就不再问「立即启动」。
		// --manage 告诉它托管管理端形态（manage run）而不是单实例网关。
		args := []string{"service", "install", "--manage", "--bin-dir", binDir}
		if settings.port != 3082 {
			args = append(args, "--port", strconv.Itoa(settings.port))
		}
		if settings.stateDir != "" {
			args = append(args, "--state-dir", settings.stateDir)
		}
		code = runSelf(args)
	case settings.start:
		// 转交 manage run --background：登记、日志、停机端点都在那边。
		// 口令相关的选项一律不带——账号上一步已经建好了。
		args := []string{"manage", "run", "--background",
			"--port", strconv.Itoa(settings.port)}
		if settings.stateDir != "" {
			args = append(args, "--state-dir", settings.stateDir)
		}
		code = runSelf(args)
		if code == 0 {
			sayKey(os.Stdout, i18n.KeyCLIInstallStarted, settings.port)
		}
	default:
		sayKey(os.Stdout, i18n.KeyCLIInstallNotStarted, settings.port)
	}

	fmt.Println()
	sayKey(os.Stdout, i18n.KeyCLIInstallNextSteps, settings.admin)
	return code
}

// installSettings 是向导收集到的安装设置。
type installSettings struct {
	admin    string
	host     string
	port     int
	stateDir string
	binDir   string
	start    bool
	service  bool
	// password 是向导里问到的口令；为空表示用 --random / --password-stdin，
	// 或库里已有账号不需要建号。
	password string
	// needPassword 表示这次要不要建账号（库里一个账号都没有时才建）。
	needPassword bool
}

// collectInstallSettings 逐项收集安装设置。
//
// 顺序是「先环境、后账号」：监听地址、端口、安装目录、配置与数据库目录都是
// 装之前就要定下来的事实；管理员用户名与口令是装完之后第一次登录才用到的，
// 放在环境之后、汇总确认之前——确认时能看到「将要创建哪个账号」。
// 交互式终端里一问一答，端口项带占用检测，被占用时让用户选择换端口 / 停掉
// 占用者；答完汇总确认，不满意可以重答一轮。
// 脚本（管道 / CI）里一个都不问，全部用选项给出的值，并做一次端口预检。
func collectInstallSettings(fs *flag.FlagSet, visited map[string]bool, host string, port int, stateDir, admin string, interactive, needPassword, randomGiven, stdinGiven bool) (installSettings, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	defaultState := filepath.Join(home, ".dsh-gate")

	s := installSettings{
		admin:        strings.TrimSpace(admin),
		host:         strings.TrimSpace(host),
		port:         port,
		stateDir:     strings.TrimSpace(stateDir),
		needPassword: needPassword,
	}
	for _, name := range []string{"start", "service"} {
		if f := fs.Lookup(name); f != nil {
			switch name {
			case "start":
				s.start = f.Value.String() == "true"
			case "service":
				s.service = f.Value.String() == "true"
			}
		}
	}
	if s.service {
		// service install 自己就会立即启动，两者都要时只装服务：先后台启动
		// 再装服务会让后者撞上「实例已在运行」。
		s.start = false
	}

	if !interactive {
		// 脚本模式：端口范围已由调用方校验，这里只查占用——撞端口要在动手之前
		// 失败，而不是装完账号、最后一步才报「端口被占用」。
		if err := checkPortOccupied("管理端端口", s.port, false); err != nil {
			return s, err
		}
		return s, nil
	}

	fmt.Println("回车接受括号里的默认值；[Y/n] 回车＝是，[y/N] 回车＝否。")
	for {
		// 1. 监听地址
		if !visited["host"] {
			answer, err := askLine(fmt.Sprintf("监听地址 [%s]: ", s.host))
			if err != nil {
				return s, err
			}
			if answer != "" {
				s.host = strings.TrimSpace(answer)
			}
		}

		// 2. 端口（带占用检测；被占用且选择换端口时重问这一项）
		if !visited["port"] {
			for {
				answer, err := askLine(fmt.Sprintf("管理端端口 [%d]: ", s.port))
				if err != nil {
					return s, err
				}
				if answer == "" {
					break // 保持默认值；默认值也要查占用（见下）
				}
				parsed, err := strconv.Atoi(strings.TrimSpace(answer))
				if err != nil || parsed < 1 || parsed > 65535 {
					fmt.Println("  端口需要是 1-65535 之间的数字，请重试。")
					continue
				}
				s.port = parsed
				break
			}
			// 默认值同样可能被别人占着，所以无论是否改过都查一次。
			for {
				change, err := askPortIfOccupied("管理端端口", s.port)
				if err != nil {
					return s, err
				}
				if !change {
					break
				}
				answer, err := askLine("换一个管理端端口: ")
				if err != nil {
					return s, err
				}
				parsed, err := strconv.Atoi(strings.TrimSpace(answer))
				if err != nil || parsed < 1 || parsed > 65535 {
					fmt.Println("  端口需要是 1-65535 之间的数字，请重试。")
					continue
				}
				s.port = parsed
			}
		} else if err := checkPortOccupied("管理端端口", s.port, true); err != nil {
			return s, err
		}

		// 3. 安装目录（命令拷去哪儿）
		if !visited["bin-dir"] {
			answer, err := askLine(fmt.Sprintf("安装目录 [%s]: ", defaultBinDir))
			if err != nil {
				return s, err
			}
			if answer != "" {
				s.binDir = strings.TrimSpace(answer)
			} else {
				s.binDir = defaultBinDir
			}
		}

		// 4. 配置与数据库目录（账号库 portal.db、自备 Node 运行时都在里面）
		if !visited["state-dir"] {
			answer, err := askLine(fmt.Sprintf("配置与数据库目录 [%s]: ", defaultState))
			if err != nil {
				return s, err
			}
			if answer != "" {
				s.stateDir = strings.TrimSpace(answer)
			} else {
				s.stateDir = defaultState
			}
		}

		// 5. 管理员用户名与口令
		//
		// 口令放在向导里而不是建号时才问：汇总确认时用户能看到「将要创建哪个
		// 账号」，答错一项重来的成本也只是一轮，不必等建号失败。
		// 库里已有账号时整段跳过——那是重复执行 install，不该顺手改谁的口令。
		if !visited["admin"] {
			answer, err := askLine(fmt.Sprintf("管理员用户名 [%s]: ", s.admin))
			if err != nil {
				return s, err
			}
			if answer != "" {
				s.admin = answer
			}
		}
		if strings.TrimSpace(s.admin) == "" {
			fmt.Println("  管理员用户名不能为空。")
			continue
		}
		if s.needPassword && s.password == "" && !randomGiven && !stdinGiven {
			fmt.Printf("管理员 %s 的口令（输入不回显）: ", s.admin)
			secret, err := readSecretMasked()
			if err != nil {
				return s, fmt.Errorf("读取口令失败: %w", err)
			}
			if strings.TrimSpace(secret) == "" {
				fmt.Println("  口令不能为空。")
				continue
			}
			s.password = secret
		}

		// 6. 是否后台启动 / 开机自启
		if !visited["start"] && !visited["service"] {
			answer, err := askYesNo("现在就启动管理端？[y/N] ", false)
			if err != nil {
				return s, err
			}
			s.start = answer
			if answer {
				auto, err := askYesNo("设为开机（登录）自启？[y/N] ", false)
				if err != nil {
					return s, err
				}
				s.service = auto
			}
		}

		fmt.Println()
		fmt.Println("确认一下:")
		fmt.Printf("  监听地址        %s\n", s.host)
		fmt.Printf("  管理端端口      %d\n", s.port)
		fmt.Printf("  安装目录        %s\n", s.binDir)
		fmt.Printf("  配置与数据库目录 %s\n", s.stateDir)
		fmt.Printf("  管理员          %s\n", s.admin)
		if s.password != "" {
			fmt.Printf("  口令            已设置（%d 位）\n", len(s.password))
		}
		fmt.Printf("  立即启动        %v\n", s.start || s.service)
		fmt.Printf("  开机自启        %v\n", s.service)
		ok, err := askYesNo("确认以上设置？[Y/n] ", true)
		if err != nil {
			return s, err
		}
		if ok {
			return s, nil
		}
		fmt.Println("好，重新输入。")
	}
}

// checkPortOccupied 检查端口是否被占用，返回给人看的错误。
func checkPortOccupied(label string, port int, interactive bool) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	state := dshproc.Inspect(addr, 700*time.Millisecond)
	if !state.InUse {
		return nil
	}
	who := "占用的进程查不出来（只有 Linux 与 macOS 能反查）"
	if state.PID > 0 {
		who = fmt.Sprintf("进程号 %d", state.PID)
	}
	if state.DSH {
		return fmt.Errorf("%s %d 上已经有一个 dsh（%s）：管理端不接管 dsh，请换一个端口", label, port, who)
	}
	return fmt.Errorf("%s %d 已被占用（%s）：换一个端口，或先停掉占用它的程序", label, port, who)
}

// askPortIfOccupied 在端口被占用时询问用户怎么办，返回「要不要换端口」。
func askPortIfOccupied(label string, port int) (bool, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	state := dshproc.Inspect(addr, 700*time.Millisecond)
	if !state.InUse {
		return false, nil
	}
	who := "占用的进程查不出来（只有 Linux 与 macOS 能反查）"
	if state.PID > 0 {
		who = fmt.Sprintf("进程号 %d", state.PID)
	}
	fmt.Printf("  %s %d 已被占用（%s）。怎么办？\n", label, port, who)
	if state.DSH {
		fmt.Println("    [s] 停掉它（管理端要的是空闲端口，dsh 由实例自己管理）")
	}
	if state.PID > 0 {
		fmt.Printf("    [s] 停掉它（给进程 %d 发 SIGTERM，让它正常退出）\n", state.PID)
	} else {
		fmt.Println("    [s] 停掉它（当前平台查不到进程号，只能你自己手动停）")
	}
	fmt.Println("    [c] 换一个端口")
	choice, err := askChoice("  选择 [s/c]（默认 c）: ", "c", "s", "c")
	if err != nil {
		return true, err
	}
	if choice == "s" && state.PID > 0 {
		if stopOccupant(label, strconv.Itoa(port), state.PID) {
			return false, nil
		}
	}
	return true, nil
}

// portalDBPath 算出管理端数据库的位置。
//
// 与 manage run 的推导方式保持一致（<状态目录>/portal.db），否则 install 建的
// 账号和 manage run 读的账号会是两个库——用户登不进去，还查不出原因。
func portalDBPath(stateDir string) (string, error) {
	dir := strings.TrimSpace(stateDir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法确定家目录，请用 --state-dir 指定配置与数据库目录: %w", err)
		}
		dir = filepath.Join(home, ".dsh-gate")
	}
	return filepath.Join(dir, "portal.db"), nil
}
