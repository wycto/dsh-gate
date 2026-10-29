// `dsh-gate dsh ...`：管理 dsh 实例（与管理台 /admin 的实例页能力对等）。
//
// 独立成一组而不是塞进 manage：manage 管的是「人」（用户、授权），dsh 管的
// 是「实例」。列表、增删改直接读写管理端数据库；启停、重启、连带删目录需要
// 运行时监督者，走管理端的本机控制 API（见 localclient.go）——命令本身不
// 绕过管理端去拉进程，否则会出现「CLI 拉起一个、管理端不认识」的孤儿 dsh。
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/portal"
)

// dshCommand 实现 `dsh-gate dsh`。
func dshCommand(args []string) int {
	if len(args) == 0 {
		printDSHHelp(os.Stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return dshList(args[1:])
	case "add":
		return portalInstanceAdd(args[1:])
	case "install":
		return portalInstanceInstall(args[1:])
	case "rename":
		return portalInstanceRename(args[1:])
	case "edit":
		return dshEdit(args[1:])
	case "rm":
		return dshRemove(args[1:])
	case "start", "stop", "restart":
		return dshLifecycle(args[0], args[1:])
	case "-h", "--help", "help":
		printDSHHelp(os.Stdout)
		return 0
	default:
		return usageError("未知的 dsh 子命令 %q，可用：list / add / edit / rename / start / stop / restart / rm / install", args[0])
	}
}

// printDSHHelp 打印 dsh 子命令帮助。
func printDSHHelp(w *os.File) {
	fmt.Fprint(w, `用法: dsh-gate dsh <子命令> [参数]

管理 dsh 实例。实例定义存在管理端数据库里；启动/停止/重启由管理端监督进程
执行，所以这些动作要求管理端正在运行。

  dsh-gate dsh list                               列出全部实例（含运行状态）
  dsh-gate dsh add <名字> --dsh-home <目录>        新增实例
        [--config-dir <目录>] [--workspace <目录>] [--dsh-port 端口]
        [--listen-host 127.0.0.1] [--start-mode auto|ondemand]
        [--install-mode npx|git|local|pnpm] [--install-dir <目录>] [--dsh-version <版本|标签>]
                                        local = 你自己 clone + pnpm install + pnpm run build
                                        好的源码目录，网关只负责 pnpm dsh web 启动
  dsh-gate dsh edit <名字> [同 add 的字段]         只改显式给出的字段
  dsh-gate dsh rename <旧名> <新名>                改名（id、授权、会话不变）
  dsh-gate dsh start|stop|restart <名字或 id>      启停单个实例
  dsh-gate dsh install <名字或 id> [--install-mode …] [--install-dir …] [--dsh-version …]
                                                   同步安装/重装 dsh
  dsh-gate dsh rm <名字或 id> [--purge] [--purge-install]
                                                   删除实例；--purge 删实例数据目录，
                                                   --purge-install 删安装目录

公共选项: --state-dir <目录>  --db <路径>  --lang zh-CN|en
`)
}

// dshList 列出实例：优先问管理端要运行状态，管理端没在跑时只报定义。
func dshList(args []string) int {
	fs := flag.NewFlagSet("dsh list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() > 0 {
		return usageError("dsh list 不接受位置参数")
	}
	instances, err := listInstancesFromDB(dbPath)
	if err != nil {
		return failKey(i18n.KeyCLIErrOpenStore, err.Error())
	}
	if len(instances) == 0 {
		sayKey(os.Stdout, i18n.KeyCLIInstListEmpty)
		return 0
	}
	states := map[string]localInstanceInfo{}
	portalUp := true
	infos, apiErr := localListInstances(stateDir)
	if apiErr != nil {
		portalUp = false
	}
	for _, info := range infos {
		states[info.ID] = info
	}
	fmt.Printf("共 %d 个实例：\n", len(instances))
	for _, inst := range instances {
		state := "未运行"
		pid := ""
		if info, ok := states[inst.ID]; ok {
			switch {
			case info.Running && info.Ready:
				state = "运行中"
			case info.Running:
				state = "启动中"
			}
			if info.PID > 0 {
				pid = fmt.Sprintf(" pid=%d", info.PID)
			}
		} else if !portalUp {
			state = "未知（管理端未运行）"
		}
		fmt.Printf("  %-20s %-10s 端口 %-6d %-16s%s\n", inst.Name, inst.ID, inst.DshPort, state, pid)
		fmt.Printf("    DSH_HOME %s\n", inst.DshHome)
		if strings.TrimSpace(inst.ConfigDir) != "" {
			fmt.Printf("    配置目录 %s（覆盖 DSH_HOME）\n", inst.ConfigDir)
		}
	}
	if !portalUp {
		fmt.Println("\n管理端没在运行：只能显示实例定义，运行状态未知。启动 dsh-gate start。")
	}
	return 0
}

// dshEdit 修改实例字段：只改命令行显式给出的那些，其余保持原值。
func dshEdit(args []string) int {
	fs := flag.NewFlagSet("dsh edit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dshHome := fs.String("dsh-home", "", "实例目录（DSH_HOME）")
	configDir := fs.String("config-dir", "", "配置目录（留空沿用实例目录；显式给空串表示清空）")
	workspace := fs.String("workspace", "", "工作目录")
	port := fs.Int("dsh-port", 0, "dsh 回环端口")
	listenHost := fs.String("listen-host", "", "dsh 监听地址")
	startMode := fs.String("start-mode", "", "启动方式：auto / ondemand")
	installMode := fs.String("install-mode", "", "安装方式：npx / git / local / pnpm")
	installDir := fs.String("install-dir", "", "安装目录（local 方式填已构建好的源码仓库根目录）")
	dshVersion := fs.String("dsh-version", "", "要装的 dsh 版本或 dist-tag（留空 = 跟随 latest）")
	dbPath, _, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageInstanceName)
	}
	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()
	inst, err := resolveInstance(store, fs.Arg(0))
	if err != nil {
		return failKey(i18n.KeyCLIErrInstanceNotExit, fs.Arg(0))
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["dsh-home"] {
		inst.DshHome = strings.TrimSpace(*dshHome)
	}
	if set["config-dir"] {
		inst.ConfigDir = strings.TrimSpace(*configDir)
	}
	if set["workspace"] {
		inst.Workspace = strings.TrimSpace(*workspace)
	}
	if set["dsh-port"] {
		inst.DshPort = *port
	}
	if set["listen-host"] {
		inst.ListenHost = strings.TrimSpace(*listenHost)
	}
	if set["start-mode"] {
		inst.StartMode = strings.TrimSpace(*startMode)
	}
	if set["install-mode"] {
		mode, err := portal.ParseInstallMode(*installMode)
		if err != nil {
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		inst.InstallMode = string(mode)
	}
	if set["install-dir"] {
		inst.InstallDir = strings.TrimSpace(*installDir)
	}
	if set["dsh-version"] {
		// 显式给空串 = 取消钉住、回到跟随 latest（与 --config-dir 同一个写法）；
		// 校验放在落库前：这一串最后会拼进启动命令，不能让它在参数里做文章。
		version, err := dshproc.ValidateDshVersion(*dshVersion)
		if err != nil {
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		inst.DshVersion = version
	}
	if err := store.UpdateInstance(*inst); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	fmt.Printf("实例 %q 已更新。改动要在实例重启后生效：dsh-gate dsh restart %s\n", inst.Name, inst.Name)
	return 0
}

// dshRemove 删除实例，可按参数连带删除数据目录与安装目录。
//
// 管理端在运行时走本机控制 API（它会先停实例、再删目录、最后删记录）；没在
// 运行时直接改库——此时没有监督者，但可能有残留的 dsh 进程，输出里要提醒。
func dshRemove(args []string) int {
	fs := flag.NewFlagSet("dsh rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	purge := fs.Bool("purge", false, "同时删除实例数据目录（实际生效的 DSH_HOME），不可恢复")
	purgeInstall := fs.Bool("purge-install", false, "同时删除安装目录（dsh 程序本体）")
	dbPath, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageInstanceName)
	}
	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	inst, err := resolveInstance(store, fs.Arg(0))
	if err != nil {
		store.Close()
		return failKey(i18n.KeyCLIErrInstanceNotExit, fs.Arg(0))
	}
	name := inst.Name
	id := inst.ID

	if _, _, apiErr := localEndpoint(stateDir); apiErr == nil {
		store.Close()
		if _, err := localPost(stateDir, localRequest{
			Action: "remove", Instance: id, PurgeHome: *purge, PurgeInstall: *purgeInstall,
		}); err != nil {
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		fmt.Printf("实例 %q 已删除。\n", name)
		return 0
	}
	// 管理端没在运行：停不了进程，只能删记录。数据目录的删除仍按勾选执行。
	if *purge {
		home := strings.TrimSpace(inst.ConfigDir)
		if home == "" {
			home = strings.TrimSpace(inst.DshHome)
		}
		if err := safeRemoveDir(home); err != nil {
			store.Close()
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		fmt.Printf("已删除实例数据目录：%s\n", home)
	}
	if *purgeInstall && strings.TrimSpace(inst.InstallDir) != "" {
		if err := safeRemoveDir(inst.InstallDir); err != nil {
			store.Close()
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		fmt.Printf("已删除安装目录：%s\n", inst.InstallDir)
	}
	if err := store.DeleteInstance(id); err != nil {
		store.Close()
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	store.Close()
	fmt.Printf("实例 %q 已删除（管理端没在运行；如果还有残留 dsh 进程，请自行确认）。\n", name)
	return 0
}

// dshLifecycle 执行 start / stop / restart：交给管理端的监督者。
func dshLifecycle(action string, args []string) int {
	fs := flag.NewFlagSet("dsh "+action, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageInstanceName)
	}
	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	inst, err := resolveInstance(store, fs.Arg(0))
	store.Close()
	if err != nil {
		return failKey(i18n.KeyCLIErrInstanceNotExit, fs.Arg(0))
	}
	if _, err := localPost(stateDir, localRequest{Action: action, Instance: inst.ID}); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	fmt.Printf("实例 %q 已%s。\n", inst.Name, actionLabel(action))
	return 0
}

// actionLabel 把动作翻成中文，用于成功提示。
func actionLabel(action string) string {
	switch action {
	case "start":
		return "启动"
	case "stop":
		return "停止"
	case "restart":
		return "重启"
	}
	return action
}

// safeRemoveDir 安全删除一个目录：拒绝空串、相对路径、根目录与当前用户家目录。
//
// 与管理台页面那份校验保持同一口径：删目录不可逆，宁可多拒一个也不猜。
func safeRemoveDir(path string) error {
	p := strings.TrimSpace(path)
	if p == "" {
		return errors.New("没有配置目录，无法删除")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("路径 %q 不是绝对路径，拒绝删除", p)
	}
	clean := filepath.Clean(p)
	if clean == string(filepath.Separator) {
		return fmt.Errorf("拒绝删除根目录 %q", p)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" &&
		clean == filepath.Clean(strings.TrimSpace(home)) {
		return fmt.Errorf("拒绝删除当前用户家目录 %q", p)
	}
	if _, err := os.Stat(clean); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return os.RemoveAll(clean)
}
