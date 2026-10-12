package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/dshsettings"
	"github.com/wycto/dsh-gate/internal/server"
	"github.com/wycto/dsh-gate/internal/term"
	"github.com/wycto/dsh-gate/internal/users"
)

// setupCommand 实现 `dsh-gate setup`：一次引导式初始化。
//
// 新手第一次用要跨三个命令（user add、run、service install），每一步的选项
// 还得自己记。setup 把它们串成一个向导：问几个带默认值的问题、建好账号，
// 再按需后台启动或装开机自启——后两步调用现有的子命令自己，不新造第二套
// 启动逻辑，设置也因此只有一份真源（run 的登记与 service 的参数）。
//
// 交互式终端里逐项提问（回车接受默认值）；管道 / CI 里一个都不问，缺什么
// 用选项给什么，口令必须走 --random 或 --password-stdin。
func setupCommand(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate setup [选项]

引导式初始设置：创建管理员账号，并可顺带后台启动、安装开机自启。
交互式终端里逐项询问（回车接受括号里的默认值，大写字母即默认值：[y/N] 回车＝否），
答完汇总确认一遍、不满意可重答；脚本里全部用选项给出。

实例选项与 run 的同名选项一致:
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
账号:
  --admin <用户名>    管理员用户名（默认 admin）
  --random            自动生成强口令并打印一次
  --password-stdin    从标准输入读取口令（脚本 / CI）

动作（交互模式会追问；脚本模式只用选项）:
  --start             完成后立即后台启动
  --service           安装开机（登录）自启并立即启动（蕴含 --start）

命令:
  --bin-dir <目录>    把 dsh-gate 拷贝到这里（需在 PATH 里），装完就能直接敲
                      dsh-gate；默认 `+defaultBinDir+`，留空则不拷贝

插件:
  --plugin <包名>     启动后顺带装进 dsh 配置档（可重复），例如 --plugin dsh-dock
                      网关用自备的 pnpm 与镜像源装，不依赖浏览器里的插件页

示例:
  dsh-gate setup
      全程交互

  dsh-gate setup --name company --port 12061 --random
      脚本模式：非默认项用选项给出，口令自动生成

账号表已有账号时不再问口令，也不会改动现有账号。
`)
	}

	var (
		admin             string
		randomMode        bool
		fromStdin         bool
		wantStart         bool
		wantService       bool
		binDir            string
		modelAPIKey       string
		modelKeyFromStdin bool
		modelDefault      string
		modelID           string
		allowRootWrite    bool
	)
	fs.StringVar(&admin, "admin", "admin", "管理员用户名")
	fs.BoolVar(&randomMode, "random", false, "自动生成一个强随机口令并打印一次")
	fs.BoolVar(&fromStdin, "password-stdin", false, "从标准输入读取口令")
	fs.BoolVar(&wantStart, "start", false, "完成后立即后台启动")
	fs.BoolVar(&wantService, "service", false, "安装开机自启并立即启动")
	fs.StringVar(&binDir, "bin-dir", defaultBinDir,
		"把 dsh-gate 拷贝到这个目录（需在 PATH 里）；留空则不拷贝")
	fs.BoolVar(&allowRootWrite, "allow-root-write", false,
		"允许以 root 身份把数据写进别的用户的目录（默认拒绝：会造出 root 属主的配置，那个用户改不动）")
	var plugins stringList
	fs.Var(&plugins, "plugin", "顺带安装的 dsh 插件包名（可重复），例如 --plugin dsh-dock")
	fs.StringVar(&modelAPIKey, "api-key", "", "DeepSeek API Key（不推荐：会留在 shell 历史里，优先用 --api-key-stdin）")
	fs.BoolVar(&modelKeyFromStdin, "api-key-stdin", false, "从标准输入读取 DeepSeek API Key（脚本 / CI 首选）")
	fs.StringVar(&modelDefault, "model-provider", dshsettings.DeepSeekOfficial, "默认模型的服务商（官方 DeepSeek 是 deepseek-official）")
	fs.StringVar(&modelID, "model", defaultDeepSeekModel, "默认模型 id，例如 deepseek-chat")

	registerInstanceFlags(fs)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	// 只放行实例选项：它们既能喂给 config.Apply，也能原样转发给 run 与
	// service install。--config 会与实例选项打架，--background 由 setup 自己
	// 决定，--log-level 之类 service install 不认识，一律拒掉并指路。
	probe := flag.NewFlagSet("probe", flag.ContinueOnError)
	registerInstanceFlags(probe)
	instanceFlags := map[string]bool{}
	probe.VisitAll(func(f *flag.Flag) { instanceFlags[f.Name] = true })
	ownFlags := map[string]bool{"admin": true, "random": true, "password-stdin": true, "start": true, "service": true, "bin-dir": true, "plugin": true, "api-key": true, "api-key-stdin": true, "model": true, "model-provider": true, "allow-root-write": true}
	for name := range visited {
		if !instanceFlags[name] && !ownFlags[name] {
			return usageError("--%s 不属于 setup 的范围；多实例或进阶配置请直接用 run --config 与 service install", name)
		}
	}

	interactive := term.IsTerminal(os.Stdin.Fd())

	// 口令的可用来源在动手前先检查清楚：非交互又没给来源，等问完一圈才
	// 失败是最糟的体验。
	if !interactive && !randomMode && !fromStdin {
		return usageError(`当前不是交互式终端，setup 无法询问口令。
改用以下任一方式:
  echo "$PASSWORD" | dsh-gate setup --password-stdin
  dsh-gate setup --random`)
	}
	if randomMode && fromStdin {
		return usageError("--random 与 --password-stdin 只能用一个")
	}

	overrides, err := collectOverrides(fs, visited, instanceFlags, interactive)
	if err != nil {
		return fatal("%v", err)
	}

	cfg := config.Default()
	if err := config.ApplyEnv(&cfg, os.Getenv); err != nil {
		return usageError("%v", err)
	}
	if err := config.Apply(&cfg, overrides); err != nil {
		return usageError("%v", err)
	}
	// 非交互（脚本 / CI）时向导没有机会当场问端口，这里补一道预检：撞端口要在
	// **动手之前**失败，而不是装完账号与自启、最后一步才报「端口被占用」。
	if !interactive {
		if err := checkPortsForScript(cfg); err != nil {
			return usageError("%v", err)
		}
	}
	if err := cfg.Normalize(); err != nil {
		return usageError("%v", err)
	}

	// 动手之前先拦「以 root 写别人的目录」：那会造出 root 属主的 settings.yaml
	// 与 .credentials.yaml，目标用户自己的 dsh（包括桌面端）从此读不了也改不动。
	if code := guardRootWrites([]config.Instance{cfg}, allowRootWrite); code != 0 {
		return code
	}

	store, err := users.Open(cfg.UsersFile)
	if err != nil {
		return fatal("%v", err)
	}
	if store.Count() > 0 && (randomMode || fromStdin) {
		fmt.Println("账号表已有账号，口令选项（--random / --password-stdin）将被忽略。")
	}

	// 账号：只在账号表为空时创建。已有的表一动不动——setup 是初始化命令，
	// 重复跑不应该有「顺手改了谁口令」的惊喜。
	created := false
	if store.Count() == 0 {
		if interactive && !visited["admin"] {
			answer, err := askLine(fmt.Sprintf("管理员用户名 [%s]: ", admin))
			if err != nil {
				return fatal("%v", err)
			}
			if answer != "" {
				admin = answer
			}
		}
		if err := users.ValidateName(admin); err != nil {
			return usageError("%v", err)
		}
		password, err := acquirePassword(randomMode, fromStdin)
		if err != nil {
			return fatal("%v", err)
		}
		if err := users.ValidatePassword(password); err != nil {
			return usageError("%v", err)
		}
		if err := store.Set(admin, password, users.DefaultIterations); err != nil {
			return fatal("%v", err)
		}
		created = true
	}

	// 动作：交互模式在这里追问；脚本模式只认 --start / --service。
	// service install 自己就会立即启动，选了它就不再问「立即启动」。
	if interactive && !wantService && !wantStart {
		fmt.Println()
		install, err := askYesNo("现在安装开机（登录）自启并立即启动？[y/N] ", false)
		if err != nil {
			return fatal("%v", err)
		}
		if install {
			wantService = true
		} else {
			wantStart, err = askYesNo("现在放到后台启动？[y/N] ", false)
			if err != nil {
				return fatal("%v", err)
			}
		}
	}
	if wantService && wantStart {
		// 两者都要时只装服务：先后台启动再装服务会让后者撞上「实例已在运行」。
		fmt.Println("--service 已蕴含立即启动，忽略 --start。")
		wantStart = false
	}

	// 模型：密钥与默认模型都在这一步定下来。它与 dsh-dock 是互补的两条路，
	// 所以不依赖插件是否装得上。
	if modelKeyFromStdin {
		secret, err := readSecret(true, dshsettings.DeepSeekKeyEnv)
		if err != nil {
			return usageError("%v", err)
		}
		modelAPIKey = secret
	}
	modelRequested := interactive || modelAPIKey != "" || modelKeyFromStdin
	if modelRequested {
		configureModel(cfg, interactive, modelDefault, modelID, modelAPIKey)
	}

	printSummary(cfg, admin, created, store, wantService, wantStart)

	// 把命令放进 PATH。选了自己启就交给 service install 一起做——它会把拷贝后
	// 的路径写进服务单元；否则 setup 自己拷，不然装完还是只能 ./dsh-gate。
	if !wantService {
		printInstallSelfResult(binDir)
	}

	code := 0
	switch {
	case wantService:
		// 注意：单元里的 WorkingDirectory 由 service install 自己从 --workspace
		// 推断（见 serviceOptions.workDirOrDefault），这里不必再转发 --working-dir，
		// 免得同一件事有两个来源。
		args := append([]string{"service", "install", "--bin-dir", binDir}, flagArgs(overrides)...)
		code = runSelf(args)
	case wantStart:
		code = runSelf(append([]string{"run", "--background"}, flagArgs(overrides)...))
	default:
		fmt.Println()
		fmt.Println("接下来:")
		fmt.Println("  dsh-gate run --background    后台启动网关")
		fmt.Println("  dsh-gate service install     设为开机（登录）自启并立即启动")
		fmt.Println("  dsh-gate check               随时体检：端口、dsh、访问地址")
	}

	// 插件放在动作之后：dsh 的配置档（profile）是它第一次启动时创建的，
	// 早于那一步装插件只会得到「找不到配置档」。
	//
	// 没显式给 --plugin 时默认带上 dsh-dock：它是「浏览器里配模型」这条路的
	// 前提，开源用户不会知道它的存在。装不上不是致命错误——下面会给出手动命令，
	// 而模型本身已经能用 `dsh-gate model` 配好。
	wanted := plugins
	bestEffort := false
	if len(wanted) == 0 && modelRequested {
		// 默认替他装 dsh-dock（浏览器里配模型的那条路），但装不上不算失败：
		// 模型已经用 dsh-gate model 配好了，插件只是锦上添花。
		wanted, bestEffort = []string{dockPlugin}, true
	}
	if len(wanted) > 0 && code == 0 {
		if pluginCode := setupInstallPlugins(wanted, cfg); pluginCode != 0 {
			if bestEffort {
				fmt.Printf("\ndsh-dock 没装上。手动安装：\n  dsh-gate plugin add %s\n", dockPlugin)
				fmt.Println("不影响使用：模型已经用 dsh-gate model 配好，浏览器里的设置页用不了而已。")
			} else {
				code = pluginCode
			}
		}
	}
	return code
}

// configureModel 收集并写入模型配置：密钥（官方 DeepSeek）与默认模型。
//
// 这一步与 dsh-dock 是互补的两条路：插件装上了就用它的页面配，装不上就用命令配。
// 所以它必须在 setup 里独立完成，不能「等插件」。
func configureModel(cfg config.Instance, interactive bool, provider, modelID, apiKey string) {
	home := cfg.DSHHome
	if provider == "" {
		provider = dshsettings.DeepSeekOfficial
	}
	if modelID == "" {
		modelID = defaultDeepSeekModel
	}

	if apiKey != "" {
		path, err := dshsettings.SetKey(home, dshsettings.DeepSeekKeyEnv, apiKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "写入密钥失败：%v\n", err)
			return
		}
		fmt.Printf("已写入密钥 %s（文件 %s，密钥本身不回显、不进日志）\n", dshsettings.DeepSeekKeyEnv, path)
	} else if interactive {
		fmt.Printf("\nDeepSeek API Key（输入显示为 *，直接回车跳过）：\n")
		secret, err := readSecretMasked()
		if err != nil {
			fmt.Printf("  读取失败，跳过密钥配置：%v\n", err)
		} else if secret != "" {
			path, writeErr := dshsettings.SetKey(home, dshsettings.DeepSeekKeyEnv, secret)
			if writeErr != nil {
				fmt.Fprintf(os.Stderr, "  写入密钥失败：%v\n", writeErr)
			} else {
				fmt.Printf("已写入密钥 %s（文件 %s，密钥本身不回显）\n", dshsettings.DeepSeekKeyEnv, path)
			}
		}
	}

	path, err := dshsettings.SetDefaultModel(home, provider, modelID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "写入默认模型失败：%v\n", err)
		return
	}
	fmt.Printf("已写入默认模型 %s / %s（%s）\n", provider, modelID, path)
	if !interactive && apiKey == "" {
		fmt.Printf("没配密钥时模型还用不了；稍后执行：\n  printf '%%s' \"$KEY\" | dsh-gate model key set %s --stdin\n",
			dshsettings.DeepSeekKeyEnv)
	}
	fmt.Println("提示：已运行的实例可能要重启才读到新设置（dsh-gate restart <实例名>）。")
}

// setupInstallPlugins 在 setup 收尾时把插件装进 dsh 配置档。
//
// 等一会儿配置档：service install / run --background 是异步的，dsh 起来之后
// 才写 profile。等不到就给出稍后手动执行的命令，而不是让 setup 报一个看不懂的错。
func setupInstallPlugins(plugins []string, cfg config.Instance) int {
	profileDir := filepath.Join(cfg.DSHHome, "profiles", "web")
	packageFile := filepath.Join(profileDir, "package.json")

	fmt.Println()
	if !waitForFile(packageFile, 90*time.Second) {
		fmt.Fprintf(os.Stderr, "提示：dsh 还没创建出配置档（%s），插件这次没装。\n", packageFile)
		fmt.Fprintf(os.Stderr, "dsh 首次启动完成后，执行：dsh-gate plugin add %s\n", strings.Join(plugins, " "))
		return 0
	}

	opts := pluginOptions{
		DSHHome:  cfg.DSHHome,
		Profile:  "web",
		StateDir: cfg.StateDirPath(),
		Timeout:  10 * time.Minute,
	}
	return runPluginAdd(context.Background(), opts, plugins)
}

// waitForFile 轮询等待文件出现。
func waitForFile(path string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		if fileExists(path) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// collectOverrides 汇总实例设置：命令行显式给过的直接保留，其余的在交互终端
// 里逐项问一遍（回车接受默认值），非交互终端里就用默认值。
//
// 返回的 map 只含 instanceFlags 里点名的实例选项，键与 config.Apply 接受的键
// 一致，之后既能喂给 config.Apply，也能原样拼回子命令的选项。账号与动作类
// 选项不属于实例设置，不会出现在里面。
func collectOverrides(fs *flag.FlagSet, visited, instanceFlags map[string]bool, interactive bool) (map[string]string, error) {
	overrides := map[string]string{}
	def := config.Default()
	for name := range visited {
		if !instanceFlags[name] {
			continue
		}
		if f := fs.Lookup(name); f != nil {
			overrides[name] = f.Value.String()
		}
	}
	if !interactive {
		return overrides, nil
	}

	// 只问核心几项，且没被选项显式给过的；进阶项（TLS、公开地址、信任列表…）
	// 引导完直接改 run 的选项或配置文件更合适，塞进向导只会把人淹没。
	//
	// dsh 端口排在对外端口前面：两个端口都是一串数字，问「哪个是对外的」
	// 不如按依赖顺序先问 dsh 端口——后面 check、自启、接管关心的都是它。
	//
	// 工作目录的默认值要写**真实的绝对路径**，不能写「当前目录」：向导后面
	// 会把设置交给 run --background / service install，而后者的默认工作目录
	// 是用户家目录（systemd 单元的 WorkingDirectory 也是它）。写「当前目录」
	// 等于承诺一个落不了地的值——实测用户回车接受后，dsh 的默认 workspace
	// 变成了 /root，DSH 页面上报「无法创建默认工作区」。
	workspace := cwdForSetup()
	ask := []setupQuestion{
		{key: "name", prompt: "实例名", display: def.Name},
		{key: "dsh-port", prompt: "dsh 端口", display: strconv.Itoa(def.DSHPort)},
		{key: "port", prompt: "对外端口", display: strconv.Itoa(def.Port)},
		{key: "dsh-home", prompt: "dsh 实例目录", display: "~/.dsh"},
		{key: "workspace", prompt: "会话默认工作目录", display: workspace, materialize: true},
		{key: "state-dir", prompt: "网关状态目录", display: def.StateDirPath()},
	}

	fmt.Println("回车接受括号里的默认值；括号里大写的那个字母就是默认：[Y/n] 回车＝是，[y/N] 回车＝否。")
	for {
		for _, item := range ask {
			if visited[item.key] {
				continue
			}
			// 重答一轮时括号里显示的是当前答案而不是出厂默认值：回车的
			// 含义始终是「这项不变」。
			current := item.display
			if v, ok := overrides[item.key]; ok {
				current = v
			}
			for {
				answer, err := askLine(fmt.Sprintf("%s [%s]: ", item.prompt, current))
				if err != nil {
					return nil, err
				}
				if answer == "" {
					// 保持当前值；materialize 的题目把当前值也显式写下来。
					//
					// 端口即使是「默认值」也要查一次占用：默认值同样可能被别人占着，
					// 放过去只会把失败推迟到启动那一刻（甚至更晚——装完自启之后）。
					if isPortKey(item.key) {
						if err := checkPortUsable(item.key, current, overrides); err != nil {
							fmt.Printf("  %v\n", err)
							continue
						}
					}
					if item.materialize {
						overrides[item.key] = current
					}
					break
				}
				if isPortKey(item.key) {
					if err := checkPortUsable(item.key, answer, overrides); err != nil {
						fmt.Printf("  %v\n", err)
						continue
					}
					if disposition, err := resolvePortConflict(item.key, answer, interactive); err != nil {
						fmt.Printf("  %v\n", err)
						continue
					} else if disposition == portChange {
						continue
					}
				}
				overrides[item.key] = answer
				break
			}
		}
		ok, err := confirmSettings(ask, overrides)
		if err != nil {
			return nil, err
		}
		if ok {
			break
		}
		fmt.Println("好，重新输入。")
	}
	return overrides, nil
}

// dockPlugin 是「浏览器里配模型」所依赖的插件名。
const dockPlugin = "dsh-dock"

// defaultDeepSeekModel 是官方 DeepSeek 路由下最常用的模型 id。
//
// 取值依据是 DSH 自带的 `dsh-llm-deepseek-api-key`（它负责官方 DeepSeek 的
// 鉴权与模型目录）；这里用最通用的那个，用户之后用 model 命令改即可。
const defaultDeepSeekModel = "deepseek-chat"

// setupQuestion 是交互向导里的一道题：key 与 config.Apply 接受的键一致，
// display 是没有任何输入时的展示值（默认值或提示语）。
type setupQuestion struct {
	key     string
	prompt  string
	display string
	// materialize 表示「就算回车接受默认值，也要把它显式写进设置」。
	//
	// 工作目录需要它：默认值只有在 setup 自己跑的进程里成立，一旦把设置转交
	// 给 `service install`，systemd 单元的 WorkingDirectory 是用户家目录，
	// 空值就会变成家目录。写成绝对路径，向导上的承诺才落得了地。
	materialize bool
}

// cwdForSetup 返回 setup 运行时的工作目录，作为 dsh 会话的默认 workspace。
//
// 取不到就返回空串，退化成「转交时不指定」——比编一个路径强。
func cwdForSetup() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// confirmSettings 打印全部设置的小结并请用户点头：向导连问数题、又没有回头路，
// 答错一项（比如把两个端口填反）此前只能整个 Ctrl-C 重来。回答 n 时返回
// false，调用方带着当前答案重新问一轮。
func confirmSettings(ask []setupQuestion, overrides map[string]string) (bool, error) {
	fmt.Println()
	fmt.Println("确认一下:")
	for _, item := range ask {
		value := item.display
		if v, ok := overrides[item.key]; ok {
			value = v
		}
		fmt.Printf("  %s  %s\n", item.prompt, value)
	}
	return askYesNo("确认以上设置？[Y/n] ", true)
}

// flagArgs 把覆盖项拼回子命令选项，键排序保证同一份设置反复生成完全一致的参数。
func flagArgs(overrides map[string]string) []string {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, 2*len(keys))
	for _, key := range keys {
		out = append(out, "--"+key, overrides[key])
	}
	return out
}

// runSelf 以同一个二进制执行一个子命令，继承标准输入输出，返回其退出码。
//
// 不在进程内直接调 runCommand / serviceCommand：子命令有自己的登记、日志与
// 退出码约定，走一次真实的进程边界，行为才与用户亲手敲完全一致。
func runSelf(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		return fatal("无法确定自身路径: %v", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("\n正在执行: dsh-gate %s\n", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return fatal("执行 dsh-gate %s 失败: %v", strings.Join(args, " "), err)
	}
	return 0
}

// printSummary 打印本次设置的结果与关键路径。
func printSummary(cfg config.Instance, admin string, created bool, store *users.Store, wantService, wantStart bool) {
	fmt.Println()
	fmt.Println("初始设置完成")
	fmt.Printf("  实例        %s\n", cfg.Name)
	fmt.Printf("  对外端口    %d\n", cfg.Port)
	fmt.Printf("  本机访问    %s://%s/\n", cfg.Scheme(), joinHostPort("127.0.0.1", cfg.Port))
	for _, address := range server.DisplayAddresses(3) {
		fmt.Printf("  局域网访问  %s://%s/\n", cfg.Scheme(), joinHostPort(address, cfg.Port))
	}
	fmt.Printf("  dsh         %s（目录 %s）\n", cfg.DSHAddr(), cfg.DSHHome)
	switch {
	case created:
		fmt.Printf("  账号        已创建 %s（%s）\n", admin, cfg.UsersFile)
	default:
		fmt.Printf("  账号        已有 %d 个，未改动（%s）\n", store.Count(), cfg.UsersFile)
	}
	switch {
	case wantService:
		fmt.Println("  自启        正在安装（完成后会立即启动）")
	case wantStart:
		fmt.Println("  启动        正在后台启动")
	}
}

// askLine 打印提示并读一行，去掉首尾空白。
//
// 走 term.ReadLine 的逐字节读取：向导连问数题，中间还夹着不回显的口令输入，
// 任何一环用 bufio 预读都会把粘贴进来的后续行吞在自己缓冲里。
func askLine(prompt string) (string, error) {
	line, err := term.ReadLine(os.Stdin, os.Stdout, prompt)
	if err != nil {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// askYesNo 读一个 y/n 回答；空输入按 def 处理（提示语里的 [y/N] / [Y/n] 与之一致）。
func askYesNo(prompt string, def bool) (bool, error) {
	for {
		answer, err := askLine(prompt)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		case "":
			return def, nil
		}
		fmt.Println("  请回答 y 或 n。")
	}
}

// isPortKey 判断这一题是不是端口。
func isPortKey(key string) bool { return key == "port" || key == "dsh-port" }

// checkPortUsable 校验向导里填的端口值本身（不含占用），不合格返回给人看的错误。
func checkPortUsable(key, value string, chosen map[string]string) error {
	port, err := strconv.Atoi(value)
	if err != nil {
		return errors.New("端口需要是数字，请重试。")
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("端口要在 1-65535 之间，%d 不合法。", port)
	}

	// 两个端口不能撞：撞了以后必然是「网关照常起、dsh 起不来」这种难查的状态。
	otherKey, otherLabel := "dsh-port", "dsh 端口"
	if key == "dsh-port" {
		otherKey, otherLabel = "port", "对外端口"
	}
	if other, ok := chosen[otherKey]; ok && other == value {
		return fmt.Errorf("这两个端口不能相同（%s 已经是 %s）：对外端口与 dsh 端口必须各用一个。", otherLabel, value)
	}
	return nil
}

// portDisposition 表示「这个端口能不能用」这个问题的处理结果。
type portDisposition int

const (
	// portUsable 可以直接用（空闲，或用户选择接管/已经把它腾出来了）。
	portUsable portDisposition = iota
	// portChange 用户要换一个端口，向导应当重新问这一项。
	portChange
)

// resolvePortConflict 检查端口占用，并在被占用时**让用户选择怎么办**。
//
// 以前这里只会说一句「换一个端口」——而很多情况下用户并不想换：占用端口的可能
// 就是他自己刚起的 dsh（换端口反而要重新配一遍），也可能是一个已经不需要的服务。
// 用户反馈的原话是「如果端口被占用，提示是否先停止端口服务，如果是 dsh 实例，
// 提示是否重启先停止实例，让用户选择，也可以选择换端口」。
//
// 三种处置对应三种事实：
//   - 占用者是 dsh：**停掉它让网关自己拉起**是最优解——网关亲自启动才能拿到
//     启动令牌（令牌随机生成、只在 dsh 启动时打印一次、不落盘），否则浏览器会
//     一直看到「要求重新登录」；
//   - 占用者是别的服务：让用户决定是停掉它还是换端口，我们不替用户杀进程；
//   - 非交互（脚本）模式：问不了，保持原样报错。
func resolvePortConflict(key, value string, interactive bool) (portDisposition, error) {
	// 只处理占用；数字与范围由 checkPortUsable 先查过。
	addr := net.JoinHostPort("127.0.0.1", value)
	state := dshproc.Inspect(addr, 700*time.Millisecond)
	if !state.InUse {
		return portUsable, nil
	}

	label := "端口"
	if key == "dsh-port" {
		label = "dsh 端口"
	}
	who := "占用的进程查不出来（只有 Linux 与 macOS 能反查）"
	if state.PID > 0 {
		who = fmt.Sprintf("进程号 %d", state.PID)
	}

	if state.DSH {
		// 接管是允许的（--adopt 的语义），但不该悄悄替用户决定：接管会拿不到
		// 令牌，除非用户还记得 dsh 启动时打印的地址。
		if !interactive {
			fmt.Printf("  注意：这个端口上已经有一个 dsh，网关会直接接管它（不重复启动）。\n")
			return portUsable, nil
		}
		fmt.Printf("  %s %s 上已经有一个 dsh（%s）。怎么办？\n", label, value, who)
		fmt.Println("    [t] 接管它：不重启，最快；但网关拿不到它的令牌，页面可能要求重新登录")
		fmt.Println("    [s] 停掉它，让网关自己拉起 dsh：推荐，网关能拿到令牌，不会有未授权问题")
		fmt.Println("    [c] 换一个端口")
		choice, err := askChoice("  选择 [t/s/c]（默认 s）: ", "s", "t", "s", "c")
		if err != nil {
			return portChange, err
		}
		switch choice {
		case "t":
			fmt.Println("  好，网关会接管这个 dsh。")
			return portUsable, nil
		case "c":
			return portChange, nil
		default:
			if stopOccupant(label, value, state.PID) {
				return portUsable, nil
			}
			return portChange, nil
		}
	}

	// 不是 dsh：不替用户杀进程，问清楚再动手。
	if !interactive {
		if state.PID > 0 {
			return portChange, fmt.Errorf("%s %s 已被占用（进程号 %d）；换一个端口再跑 setup，"+
				"或先停掉占用它的程序", label, value, state.PID)
		}
		return portChange, fmt.Errorf("%s %s 已被占用；换一个端口再跑 setup，或先停掉占用它的程序", label, value)
	}

	fmt.Printf("  %s %s 已被占用（%s）。怎么办？\n", label, value, who)
	if state.PID > 0 {
		fmt.Printf("    [s] 停掉它（给进程 %d 发 SIGTERM，让它正常退出）\n", state.PID)
	} else {
		fmt.Println("    [s] 停掉它（当前平台查不到进程号，只能你自己手动停）")
	}
	fmt.Println("    [c] 换一个端口")
	choice, err := askChoice("  选择 [s/c]（默认 c）: ", "c", "s", "c")
	if err != nil {
		return portChange, err
	}
	if choice == "s" && state.PID > 0 {
		if stopOccupant(label, value, state.PID) {
			return portUsable, nil
		}
	}
	return portChange, nil
}

// stopOccupant 停掉占用端口的进程，并等端口真的释放。
//
// 「发完信号就返回」是不行的：紧接着就启动网关会撞上还没退出的进程，用户看到的
// 又是一次「端口被占用」。所以这里等端口空闲，最多约 5 秒；等不到就说清楚，
// 让用户自己决定下一步。
func stopOccupant(label, value string, pid int) bool {
	if pid <= 0 {
		fmt.Println("  查不到占用它的进程号，请手动停掉之后重试，或换一个端口。")
		return false
	}
	if err := terminateProcess(pid); err != nil {
		fmt.Printf("  停不掉进程 %d：%v。请手动处理，或换一个端口。\n", pid, err)
		return false
	}
	addr := net.JoinHostPort("127.0.0.1", value)
	for i := 0; i < 20; i++ {
		time.Sleep(250 * time.Millisecond)
		if !dshproc.Busy(addr, 200*time.Millisecond) {
			fmt.Printf("  已停掉进程 %d，%s %s 空出来了。\n", pid, label, value)
			return true
		}
	}
	fmt.Printf("  已向进程 %d 发出停止信号，但 %s %s 还没释放（它可能忽略信号或正在收尾）。\n", pid, label, value)
	fmt.Println("  可以稍后重试、手动处理，或换一个端口。")
	return false
}

// askChoice 问一个单字符选项，回车取默认值。
func askChoice(prompt, fallback string, allowed ...string) (string, error) {
	for {
		answer, err := askLine(prompt)
		if err != nil {
			return "", err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "" {
			return fallback, nil
		}
		for _, item := range allowed {
			if answer == item {
				return answer, nil
			}
		}
		fmt.Printf("  请输入 %s 之一。\n", strings.Join(allowed, " / "))
	}
}

// checkPortsForScript 在非交互模式下预检两个端口。
func checkPortsForScript(cfg config.Instance) error {
	if cfg.Port == cfg.DSHPort {
		return fmt.Errorf("对外端口与 dsh 端口都是 %d：必须是两个不同的端口", cfg.Port)
	}
	for _, item := range []struct {
		label string
		port  int
	}{
		{"对外端口", cfg.Port},
		{"dsh 端口", cfg.DSHPort},
	} {
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(item.port))
		state := dshproc.Inspect(addr, 700*time.Millisecond)
		if !state.InUse {
			continue
		}
		// dsh 端口上是个 dsh、且允许接管：正常用法，不算冲突。
		if item.label == "dsh 端口" && state.DSH && cfg.Adopt {
			continue
		}
		detail := state.Detail
		if state.PID > 0 {
			detail = fmt.Sprintf("%s（进程号 %d）", detail, state.PID)
		}
		return fmt.Errorf("%s %d 已被占用：%s；换一个端口再跑 setup（或先停掉占用它的程序）",
			item.label, item.port, detail)
	}
	return nil
}
