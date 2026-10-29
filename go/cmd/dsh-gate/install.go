// `dsh-gate install`：安装管理端。
//
// install 是管理端的安装入口：建管理员账号、把命令装进 PATH，可选后台启动或
// 开机自启。装完之后**一个 dsh 都不拉起**，实例由用户在 /admin 或 `dsh-gate dsh`
// 里按需添加。
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
//  4. 按选择转交 start（后台）/ service install（开机自启 + 立即启动）
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
	"unicode/utf8"

	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/term"
	"github.com/wycto/dsh-gate/internal/users"
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
	port := fs.Int("port", 12020, "管理端监听端口")
	stateDir := fs.String("state-dir", "", "配置与数据库目录（默认 ~/.dsh-gate）")
	admin := fs.String("admin", "admin", "管理员账号（字母数字开头，可含 . _ -）")
	display := fs.String("display", "", "姓名（给人看，可中文；留空回退账号名）")
	randomMode := fs.Bool("random", false, "自动生成强口令并打印一次")
	fromStdin := fs.Bool("password-stdin", false, "从标准输入读取口令")
	fs.Bool("start", false, "安装完成后立即把管理端放到后台运行")
	fs.Bool("service", false, "安装开机（登录）自启并立即启动（蕴含 --start）")
	binDirFlag := fs.String("bin-dir", binDirDefault(),
		"把 dsh-gate 拷贝到这个目录（需在 PATH 里）；留空则不拷贝（Windows 忽略此项）")

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
	var existing []portal.User
	var superID int64
	if db, err := portalDBPath(strings.TrimSpace(*stateDir)); err == nil {
		if store, err := portal.Open(db); err == nil {
			count, _ := store.CountUsers()
			needPassword = count == 0
			if !needPassword {
				// 已有账号时向导不再问账号，改为把这些账号连同类型列出来：
				// 用户看到的应当是「这里有谁、谁是超级管理员」，而不是一句
				// 「未创建新账号」就让人以为刚才填的账号生效了。
				if users, err := store.ListUsers(); err == nil {
					existing = users
				}
				superID, _ = store.SuperAdminID()
			}
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
	// 选项显式给过的项不再问。
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	settings, err := collectInstallSettings(fs, visited, *host, *port, *stateDir, *admin, *display, interactive, needPassword, existing, superID, *randomMode, *fromStdin)
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
	// install 不应该有「顺手改了谁口令」的惊喜。
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
		if _, err := store.CreateUserWithDisplay(settings.admin, password, portal.RoleAdmin, settings.display); err != nil {
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
		// 服务单元托管的是管理端（dsh-gate start --foreground）。
		args := []string{"service", "install", "--bin-dir", binDir,
			"--host", settings.host, "--port", strconv.Itoa(settings.port),
			"--state-dir", settings.stateDir}
		code = runSelf(args)
	case settings.start:
		// 转交 start：登记、日志、停机端点都在那边。
		// 口令相关的选项一律不带——账号上一步已经建好了。
		args := []string{"start",
			"--host", settings.host, "--port", strconv.Itoa(settings.port),
			"--state-dir", settings.stateDir}
		code = runSelf(args)
		if code == 0 {
			sayKey(os.Stdout, i18n.KeyCLIInstallStarted, settings.port)
		}
	default:
		sayKey(os.Stdout, i18n.KeyCLIInstallNotStarted, settings.port)
	}

	fmt.Println()
	// 「接下来」里的示例账号：这次建了号就用刚建的，否则用已有的超级管理员。
	// 重装时填的账号并没有生效，拿它当示例会让人以为那个账号真的存在。
	hint := settings.admin
	if !settings.needPassword {
		if name := superAdminName(settings.existing, settings.superID); name != "" {
			hint = name
		}
	}
	sayKey(os.Stdout, i18n.KeyCLIInstallNextSteps, hint)
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
	// display 是「姓名」（给人看，可中文）；为空回退账号名（CreateUser 内部处理）。
	display string
	// needPassword 表示这次要不要建账号（库里一个账号都没有时才建）。
	needPassword bool
	// existing 是库里已有的账号（needPassword 为假时才有内容），用于在确认页
	// 列出「这里有谁、谁是超级管理员」；superID 是超级管理员的账号 id。
	existing []portal.User
	superID  int64
}

// collectInstallSettings 逐项收集安装设置。
//
// 顺序是「先环境、后账号」：监听地址、端口、安装目录、配置与数据库目录都是
// 装之前就要定下来的事实；管理员用户名与口令是装完之后第一次登录才用到的，
// 放在环境之后、汇总确认之前——确认时能看到「将要创建哪个账号」。
// 交互式终端里一问一答，端口项带占用检测，被占用时让用户选择换端口 / 停掉
// 占用者；答完汇总确认，不满意可以重答一轮。
// 脚本（管道 / CI）里一个都不问，全部用选项给出的值，并做一次端口预检。
func collectInstallSettings(fs *flag.FlagSet, visited map[string]bool, host string, port int, stateDir, admin, display string, interactive, needPassword bool, existing []portal.User, superID int64, randomGiven, stdinGiven bool) (installSettings, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	defaultState := filepath.Join(home, ".dsh-gate")

	s := installSettings{
		admin:        strings.TrimSpace(admin),
		display:      strings.TrimSpace(display),
		host:         strings.TrimSpace(host),
		port:         port,
		stateDir:     strings.TrimSpace(stateDir),
		needPassword: needPassword,
		existing:     existing,
		superID:      superID,
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
		//
		// 默认值各平台不同：Linux / macOS 是 /usr/local/bin，Windows 是
		// %LOCALAPPDATA%\dsh-gate\bin（见 binDirDefault 的注释）。命令拷到那里
		// 之后，服务单元 / 计划任务都指向拷贝件，原目录被删也不影响自启。
		if def := binDirDefault(); def == "" {
			s.binDir = ""
		} else if !visited["bin-dir"] {
			answer, err := askLine(fmt.Sprintf("安装目录 [%s]: ", def))
			if err != nil {
				return s, err
			}
			if answer != "" {
				s.binDir = strings.TrimSpace(answer)
			} else {
				s.binDir = def
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

		// 5. 管理员账号、姓名与口令
		//
		// 口令放在向导里而不是建号时才问：汇总确认时用户能看到「将要创建哪个
		// 账号」，答错一项重来的成本也只是一轮，不必等建号失败。
		// 库里已有账号时整段跳过——那是重复执行 install，不该顺手改谁的口令；
		// 账号与姓名同样不问：问了也不会建号，只会在确认页上留下让人误以为
		// 生效的一行。已有的账号改在确认页里列出来（见下）。
		if s.needPassword {
			if !visited["admin"] {
				answer, err := askLine(fmt.Sprintf("管理员账号 [%s]: ", s.admin))
				if err != nil {
					return s, err
				}
				if answer != "" {
					s.admin = answer
				}
			}
			if strings.TrimSpace(s.admin) == "" {
				fmt.Println("  管理员账号不能为空。")
				continue
			}
			// 当场校验账号字符：向导里敲中文姓名当账号，到建号那步才报错，
			// 用户得把整轮答案重看一遍才知道错哪了。
			if err := users.ValidateName(strings.TrimSpace(s.admin)); err != nil {
				fmt.Printf("  %v\n", err)
				continue
			}
			// 姓名只在向导里问（脚本模式走 --display 或回退账号名）；这里已在
			// interactive 分支内。回车留空 = 与账号同名，与建号回退口径一致。
			if !visited["display"] {
				answer, err := askLine(fmt.Sprintf("姓名（给人看，可中文）[%s]: ", s.admin))
				if err != nil {
					return s, err
				}
				s.display = strings.TrimSpace(answer)
			}
			if s.password == "" && !randomGiven && !stdinGiven {
				// 问两遍并要求一致：口令看不见，敲错一个字符当场没人能发现，
				// 要等下次登录才暴露，那时管理端已经锁在一个进不去的状态里。
				// 输入按星号回显，否则终端停在提示语后一动不动，用户会以为没在等输入。
				secret, err := readNewPassword(
					fmt.Sprintf("管理员 %s 的口令（输入显示为 *）: ", s.admin),
					"再输入一次确认: ",
				)
				if err != nil {
					return s, fmt.Errorf("读取口令失败: %w", err)
				}
				if strings.TrimSpace(secret) == "" {
					fmt.Println("  口令不能为空。")
					continue
				}
				s.password = secret
			}
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
		if s.binDir == "" {
			fmt.Printf("  安装目录        不拷贝（直接使用当前可执行文件）\n")
		} else {
			fmt.Printf("  安装目录        %s\n", s.binDir)
		}
		fmt.Printf("  配置与数据库目录 %s\n", s.stateDir)
		if s.needPassword {
			fmt.Printf("  管理员账号      %s\n", s.admin)
			name := s.display
			if name == "" {
				name = s.admin
			}
			fmt.Printf("  姓名            %s\n", name)
			if s.password != "" {
				// 按字符数报位数，与 users.ValidatePassword 的口径一致；用 len() 数的是
				// 字节，中文口令会报成两三倍（「至少 8 个字符」的提示会自相矛盾）。
				fmt.Printf("  口令            已设置（%d 位）\n", utf8.RuneCountInString(s.password))
			}
		} else {
			printExistingAccounts(s.existing, s.superID)
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

// superAdminName 返回超级管理员的账号名；查不到时退回列表第一个（ListUsers
// 按名字排序），保证示例命令里出现的总是一个真实存在的账号。
func superAdminName(users []portal.User, superID int64) string {
	for _, u := range users {
		if u.ID == superID {
			return u.Name
		}
	}
	if len(users) > 0 {
		return users[0].Name
	}
	return ""
}

// printExistingAccounts 把库里已有的账号连同类型列出来。
//
// 重装时最容易误会的是「向导里填的账号会变成新的管理员」：库里已有账号时
// 一个都不建，所以这里明说「本次不建号」，并标出谁是超级管理员（表里 id
// 最小的那个），让人知道账号与口令都保持原样。
func printExistingAccounts(users []portal.User, superID int64) {
	if len(users) == 0 {
		return
	}
	sayKey(os.Stdout, i18n.KeyCLIInstallHasAccounts, len(users))
	fmt.Printf(tr(i18n.KeyCLIInstallExistingRow)+"\n",
		tr(i18n.KeyAdminColAccount), tr(i18n.KeyAdminColDisplay),
		tr(i18n.KeyAdminColRole), tr(i18n.KeyAdminColStatus))
	for _, u := range users {
		status := tr(i18n.KeyCLIStatusEnabled)
		if u.Disabled {
			status = tr(i18n.KeyCLIStatusDisabled)
		}
		display := u.DisplayName
		if display == "" {
			display = u.Name
		}
		fmt.Printf(tr(i18n.KeyCLIInstallExistingRow)+"\n",
			u.Name, display, accountTypeLabel(u, superID), status)
	}
}

// checkPortOccupied 检查端口是否被占用，返回给人看的错误。
func checkPortOccupied(label string, port int, interactive bool) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	state := dshproc.Inspect(addr, 700*time.Millisecond)
	if !state.InUse {
		return nil
	}
	// 重装时端口当然被上一个管理端占着：那不是「别的程序抢了端口」。install 会
	// 让它停下再启动新版，所以不算冲突（Windows 上反查不到 PID，只能靠登记
	// 目录里的记录认人）。
	if portOwnedByPortal(port) {
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

// portOwnedByPortal 报告 port 上占着的进程是不是我们自己正在跑的管理端。
func portOwnedByPortal(port int) bool {
	record, ok, err := resolveRunDir("").Load(portalRecordName)
	if err != nil || !ok || !record.Alive() {
		return false
	}
	return record.Port == port
}

// askPortIfOccupied 在端口被占用时询问用户怎么办，返回「要不要换端口」。
func askPortIfOccupied(label string, port int) (bool, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	state := dshproc.Inspect(addr, 700*time.Millisecond)
	if !state.InUse {
		return false, nil
	}
	// 占用者是自己的管理端（重装场景）：install 会替换它，不必问。
	if portOwnedByPortal(port) {
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
// 与 start / portal 运行时的推导方式保持一致（<状态目录>/portal.db），否则
// install 建的账号和管理端读的账号会是两个库——用户登不进去，还查不出原因。
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
