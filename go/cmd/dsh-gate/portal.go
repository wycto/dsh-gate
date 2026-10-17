// `dsh-gate manage`：管理端的命令行入口。
//
// 与浏览器管理台（/admin）能力对等的部分：用户增删改查、实例增删查、授权增删查。
// 刻意不改动已有的 `dsh-gate user`（它管的是单实例形态的 users.json）：两套账号
// 表语义不同，让同一条命令按「有没有 portal.db」猜模式只会让人踩坑。
//
// 命令行管不到的地方（写在这里免得用户以为漏了）：管理端的登录会话在**进程内存**里，
// 所以命令行改口令/停用用户之后，运行中的管理端不会立刻把那个人踢下线——重启管理端
// 或让管理员在页面上操作才会。页面上做同样的动作会立即吊销会话。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/portal"
)

// manageCommand 实现 `dsh-gate manage`：管理端的命令行入口。
//
// 与浏览器管理台（/admin）能力对等：用户增删改查、实例增删查、授权增删查。
// 刻意不改动已有的 `dsh-gate user`（它管的是单实例形态的 users.json）：两套账号
// 表语义不同，让同一条命令按「有没有 portal.db」猜模式只会让人踩坑。
//
// 命令行管不到的地方（写在这里免得用户以为漏了）：管理端的登录会话在**进程
// 内存**里，所以命令行改口令/停用用户之后，运行中的管理端不会立刻把那个人踢
// 下线——重启管理端或让管理员在页面上操作才会。页面上做同样的动作会立即吊销
// 会话。
func manageCommand(args []string) int {
	if len(args) == 0 {
		sayKey(os.Stderr, i18n.KeyCLIPortalHelp)
		return 2
	}
	switch args[0] {
	case "run":
		return portalRunCommand(args[1:])
	case "user":
		return portalUser(args[1:])
	case "instance":
		return portalInstance(args[1:])
	case "grant":
		return portalGrant(args[1:])
	case "-h", "--help", "help":
		sayKey(os.Stdout, i18n.KeyCLIPortalHelp)
		return 0
	default:
		return usageKey(i18n.KeyCLIUnknownPortalSub, args[0])
	}
}

// parsePortalFlags 注册管理端命令的公共选项并解析，返回管理端数据库路径。
//
// 刻意**不**注册 --dsh-home：实例子命令自己会注册它（`instance add` 需要它），
// 同一个 FlagSet 上重复注册同名选项会让 flag 包直接 panic。
func parsePortalFlags(fs *flag.FlagSet, args []string) (string, error) {
	db, _, err := parsePortalFlagsFull(fs, args)
	return db, err
}

// parsePortalFlagsFull 与 parsePortalFlags 相同，但额外返回状态目录。
//
// `instance install` 需要状态目录：自备的 Node / pnpm 运行时放在它下面。多返回
// 一个值是必要的——只从 db 路径反推会在用户显式指定 --db 时推错。
func parsePortalFlagsFull(fs *flag.FlagSet, args []string) (string, string, error) {
	db := fs.String("db", "", "管理端数据库路径（默认 <状态目录>/portal.db）")
	fs.String("state-dir", "", "网关状态目录，用于推导管理端数据库路径")

	if err := parseFlexible(fs, args); err != nil {
		return "", "", err
	}

	cfg := config.Default()
	if err := config.ApplyEnv(&cfg, os.Getenv); err != nil {
		return "", "", err
	}
	set := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "state-dir" {
			set[f.Name] = f.Value.String()
		}
	})
	if err := config.Apply(&cfg, set); err != nil {
		return "", "", err
	}
	if err := cfg.Normalize(); err != nil {
		return "", "", err
	}
	stateDir := cfg.StateDirPath()
	if strings.TrimSpace(*db) != "" {
		return *db, stateDir, nil
	}
	return filepath.Join(stateDir, "portal.db"), stateDir, nil
}

// openPortal 打开管理端数据库；失败时直接给出退出码。
func openPortal(path string) (*portal.Store, int) {
	store, err := portal.Open(path)
	if err != nil {
		return nil, failKey(i18n.KeyCLIErrOpenStore, err.Error())
	}
	return store, 0
}

// roleLabel 把角色翻成当前语言。
func roleLabel(role portal.Role) string {
	if role == portal.RoleAdmin {
		return tr(i18n.KeyRoleAdmin)
	}
	return tr(i18n.KeyRoleUser)
}

// statusLabel 把启用状态翻成当前语言。
func statusLabel(enabled bool) string {
	if enabled {
		return tr(i18n.KeyCLIStatusEnabled)
	}
	return tr(i18n.KeyCLIStatusDisabled)
}

// portalUser 实现 `dsh-gate manage user`。
func portalUser(args []string) int {
	if len(args) == 0 {
		sayKey(os.Stderr, i18n.KeyCLIPortalUserHelp)
		return 2
	}
	switch args[0] {
	case "add":
		return portalUserAdd(args[1:])
	case "passwd":
		return portalUserPasswd(args[1:])
	case "role":
		return portalUserRole(args[1:])
	case "disable":
		return portalUserToggle(args[1:], true)
	case "enable":
		return portalUserToggle(args[1:], false)
	case "list":
		return portalUserList(args[1:])
	case "rm", "remove":
		return portalUserRemove(args[1:])
	case "-h", "--help", "help":
		sayKey(os.Stdout, i18n.KeyCLIPortalUserHelp)
		return 0
	default:
		return usageKey(i18n.KeyCLIUnknownPortalSub, args[0])
	}
}

func portalUserAdd(args []string) int {
	fs := flag.NewFlagSet("portal user add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	role := fs.String("role", string(portal.RoleUser), "角色：admin 或 user")
	randomMode := fs.Bool("random", false, "生成随机强口令并打印一次")
	fromStdin := fs.Bool("password-stdin", false, "从标准输入读取口令")

	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageUserName)
	}
	name := fs.Arg(0)
	if !portal.ValidRole(portal.Role(*role)) {
		return usageKey(i18n.KeyCLIUsageRole)
	}

	password, err := acquirePassword(*randomMode, *fromStdin)
	if err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	if _, err := store.CreateUser(name, password, portal.Role(*role)); err != nil {
		if errors.Is(err, portal.ErrUserExists) {
			return failKey(i18n.KeyCLIErrUserExists, name)
		}
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	count, _ := store.CountUsers()
	sayKey(os.Stdout, i18n.KeyCLIUserCreated, name, roleLabel(portal.Role(*role)), count)
	if *randomMode {
		fmt.Println()
		sayKey(os.Stdout, i18n.KeyCLIRandomNotice)
		fmt.Println("    " + password)
	}
	return 0
}

func portalUserPasswd(args []string) int {
	fs := flag.NewFlagSet("portal user passwd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	randomMode := fs.Bool("random", false, "生成随机强口令并打印一次")
	fromStdin := fs.Bool("password-stdin", false, "从标准输入读取口令")

	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageUserName)
	}
	name := fs.Arg(0)

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	// 先确认账号存在再问口令：别让人输完两遍才被告知名字打错了。
	if _, err := store.UserByName(name); err != nil {
		return failKey(i18n.KeyCLIErrUserNotExist, name)
	}
	password, err := acquirePassword(*randomMode, *fromStdin)
	if err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	if err := store.SetPassword(name, password); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	sayKey(os.Stdout, i18n.KeyCLIUserPasswd, name)
	if *randomMode {
		fmt.Println()
		sayKey(os.Stdout, i18n.KeyCLIRandomNotice)
		fmt.Println("    " + password)
	}
	return 0
}

func portalUserRole(args []string) int {
	fs := flag.NewFlagSet("portal user role", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 2 {
		return usageKey(i18n.KeyCLIUsageRole)
	}
	name, role := fs.Arg(0), portal.Role(fs.Arg(1))
	if !portal.ValidRole(role) {
		return usageKey(i18n.KeyCLIUsageRole)
	}

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	if err := store.SetRole(name, role); err != nil {
		return portalStoreError(err)
	}
	sayKey(os.Stdout, i18n.KeyCLIUserRoleChanged, name, roleLabel(role))
	return 0
}

func portalUserToggle(args []string, disable bool) int {
	fs := flag.NewFlagSet("portal user toggle", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageUserName)
	}
	name := fs.Arg(0)

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	if err := store.SetDisabled(name, disable); err != nil {
		return portalStoreError(err)
	}
	if disable {
		sayKey(os.Stdout, i18n.KeyCLIUserDisabled, name)
	} else {
		sayKey(os.Stdout, i18n.KeyCLIUserEnabled, name)
	}
	return 0
}

func portalUserList(args []string) int {
	fs := flag.NewFlagSet("portal user list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	users, err := store.ListUsers()
	if err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	if len(users) == 0 {
		sayKey(os.Stdout, i18n.KeyCLIUserListEmpty)
		return 0
	}
	sayKey(os.Stdout, i18n.KeyCLIUserListHeader, dbPath, len(users))
	for _, u := range users {
		status := tr(i18n.KeyCLIStatusEnabled)
		if u.Disabled {
			status = tr(i18n.KeyCLIStatusDisabled)
		}
		fmt.Printf(tr(i18n.KeyCLIUserRow)+"\n", u.Name, roleLabel(u.Role), status,
			u.CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	return 0
}

func portalUserRemove(args []string) int {
	fs := flag.NewFlagSet("portal user rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageUserName)
	}
	name := fs.Arg(0)

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	if err := store.DeleteUser(name); err != nil {
		return portalStoreError(err)
	}
	count, _ := store.CountUsers()
	sayKey(os.Stdout, i18n.KeyCLIUserDeleted, name, count)
	return 0
}

// portalStoreError 把存储层的哨兵错误翻成当前语言的提示。
func portalStoreError(err error) int {
	switch {
	case errors.Is(err, portal.ErrLastAdmin):
		return failKey(i18n.KeyCLIErrLastAdmin)
	case errors.Is(err, portal.ErrUserExists):
		return failKey(i18n.KeyCLIErrUserExists, "")
	case errors.Is(err, portal.ErrNotFound):
		return failKey(i18n.KeyCLIErrUserNotExist, "")
	}
	return failKey(i18n.KeyAdminErrActionFailed, err.Error())
}

// portalInstance 实现 `dsh-gate manage instance`。
func portalInstance(args []string) int {
	if len(args) == 0 {
		sayKey(os.Stderr, i18n.KeyCLIPortalInstHelp)
		return 2
	}
	switch args[0] {
	case "list":
		return portalInstanceList(args[1:])
	case "add":
		return portalInstanceAdd(args[1:])
	case "install":
		return portalInstanceInstall(args[1:])
	case "rename", "mv":
		return portalInstanceRename(args[1:])
	case "rm", "remove":
		return portalInstanceRemove(args[1:])
	case "-h", "--help", "help":
		sayKey(os.Stdout, i18n.KeyCLIPortalInstHelp)
		return 0
	default:
		return usageKey(i18n.KeyCLIUnknownPortalSub, args[0])
	}
}

func portalInstanceList(args []string) int {
	fs := flag.NewFlagSet("portal instance list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	instances, err := store.ListInstances()
	if err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	if len(instances) == 0 {
		sayKey(os.Stdout, i18n.KeyCLIInstListEmpty)
		return 0
	}
	sayKey(os.Stdout, i18n.KeyCLIInstListHeader, len(instances))
	for _, inst := range instances {
		fmt.Printf(tr(i18n.KeyCLIInstRow)+"\n", inst.Name, inst.ID, inst.DshHome,
			fmt.Sprintf("%d", inst.DshPort), statusLabel(inst.Enabled))
	}
	return 0
}

func portalInstanceAdd(args []string) int {
	fs := flag.NewFlagSet("portal instance add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dshHome := fs.String("dsh-home", "", "该实例的 DSH_HOME（必填）")
	workspace := fs.String("workspace", "", "该实例的工作目录")
	owner := fs.String("owner", "", "默认拥有者（登录名，可空）")
	port := fs.Int("port", 0, "dsh 回环端口（0 = 自动分配）")
	onDemand := fs.Bool("on-demand", false, "不在管理端启动时拉起（用户点击进入时才拉）")
	idleTimeout := fs.Int("idle-timeout", 0, "空闲回收秒数（0 = 不回收）")
	installMode := fs.String("install-mode", "", "dsh 安装方式：npx / git / pnpm（留空为 npx）")
	installDir := fs.String("install-dir", "", "dsh 安装目录（git 方式放源码，pnpm 方式放包）")
	configDir := fs.String("config-dir", "", "配置目录（留空沿用 --dsh-home）")
	listenHost := fs.String("listen-host", "", "dsh 监听地址（留空为 127.0.0.1）")

	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageInstanceName)
	}
	if strings.TrimSpace(*dshHome) == "" {
		return usageKey(i18n.KeyCLIUsageInstanceHome)
	}
	name := fs.Arg(0)

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	mode, err := portal.ParseInstallMode(*installMode)
	if err != nil {
		return usageKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	startMode := portal.StartModeAuto
	if *onDemand {
		startMode = portal.StartModeOnDemand
	}
	inst := portal.Instance{
		Name: name, DshHome: *dshHome, Workspace: *workspace,
		DshPort: *port, OnDemand: *onDemand, IdleTimeoutS: *idleTimeout,
		InstallMode: string(mode), InstallDir: strings.TrimSpace(*installDir),
		ConfigDir:  strings.TrimSpace(*configDir),
		ListenHost: strings.TrimSpace(*listenHost),
		StartMode:  startMode,
	}
	if strings.TrimSpace(*owner) != "" {
		u, err := store.UserByName(strings.TrimSpace(*owner))
		if err != nil {
			return failKey(i18n.KeyCLIErrUserNotExist, *owner)
		}
		inst.OwnerID = &u.ID
	}
	if err := store.CreateInstance(&inst); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	sayKey(os.Stdout, i18n.KeyCLIInstCreated, inst.Name, inst.ID, inst.DshPort, inst.DshHome)
	return 0
}

// portalInstanceInstall 实现 `dsh-gate manage instance install <名字>`。
//
// 与页面不同，CLI 是同步跑的：命令行的用户本来就在等，进度直接打在终端上更直观。
// 装好后把启动命令写回实例记录（页面那条路走的是同一个 dshproc.InstallDSH）。
func portalInstanceInstall(args []string) int {
	fs := flag.NewFlagSet("portal instance install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	installMode := fs.String("install-mode", "", "安装方式：npx / git / pnpm（留空沿用实例记录的设置）")
	installDir := fs.String("install-dir", "", "安装目录（留空沿用实例记录的设置）")
	registry := fs.String("npm-registry", "", "拉包用的 npm 源；留空自动")
	gitURL := fs.String("git-url", "", "git 方式的源码仓库（留空用官方仓库）")
	gitRef := fs.String("git-ref", "", "git 方式要检出的分支或标签")

	dbPath, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageInstanceName)
	}
	ref := fs.Arg(0)

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	inst, err := resolveInstance(store, ref)
	if err != nil {
		return failKey(i18n.KeyCLIErrInstanceNotExit, ref)
	}
	mode, err := portal.ParseInstallMode(firstNonEmpty(*installMode, inst.InstallMode))
	if err != nil {
		return usageKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	dir := firstNonEmpty(*installDir, inst.InstallDir)
	if dir == "" && mode != portal.InstallModeNpx {
		dir = filepath.Join(inst.DshHome, "install")
	}

	// 与页面同一条路径：先备齐 Node / pnpm，再按方式装。这里同步等结果，
	// 所以进度直接打在终端上。
	ctx := context.Background()
	logger := slog.Default()
	resolvedState := stateDir

	sayKey(os.Stdout, i18n.KeyCLIInstInstallStart, inst.Name, string(mode))
	nodeBin, pnpmBin := "", ""
	if bin, err := dshproc.EnsureNodeRuntime(ctx, logger, resolvedState); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
	} else {
		nodeBin = bin
	}
	if mode != portal.InstallModeNpx {
		// 把自备 Node 的 bin 目录交下去：没有系统级 npm 时，npm 就在那里。
		if bin, err := dshproc.EnsurePnpmWithBin(ctx, logger, resolvedState, *registry, nodeBin); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", err)
		} else {
			pnpmBin = bin
		}
	}

	result, err := dshproc.InstallDSH(ctx, logger, dshproc.InstallRequest{
		Mode:     dshproc.InstallMode(mode),
		Dir:      dir,
		DshHome:  inst.DshHome,
		Registry: *registry,
		GitURL:   *gitURL,
		GitRef:   *gitRef,
		NodeBin:  nodeBin,
		PnpmBin:  pnpmBin,
	})
	if err != nil {
		return failKey(i18n.KeyCLIInstInstallFailed, inst.Name, err.Error())
	}

	inst.DshCmd = result.Command
	inst.InstallMode = string(mode)
	inst.InstallDir = result.Dir
	if err := store.UpdateInstance(*inst); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	sayKey(os.Stdout, i18n.KeyCLIInstInstallDone, inst.Name, result.Command)
	return 0
}

// firstNonEmpty 返回第一个非空（去掉空白后）的字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func portalInstanceRemove(args []string) int {
	fs := flag.NewFlagSet("portal instance rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 1 {
		return usageKey(i18n.KeyCLIUsageInstanceName)
	}
	ref := fs.Arg(0)

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	inst, err := resolveInstance(store, ref)
	if err != nil {
		return failKey(i18n.KeyCLIErrInstanceNotExit, ref)
	}
	if err := store.DeleteInstance(inst.ID); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	sayKey(os.Stdout, i18n.KeyCLIInstDeleted, inst.Name)
	return 0
}

// portalInstanceRename 实现 `dsh-gate manage instance rename <旧名或 id> <新名>`。
//
// 为什么值得一条命令：实例名是**展示名**——它出现在管理台的授权矩阵、实例列表
// 与用户的「我的实例」里，而对外路径用的是不透明 id。没有改名这条路时，名字一旦
// 起错（比如起了一串自己都认不出的英文），唯一的补救是删掉重建，而删除会连带丢掉
// 授权与那台 dsh 的登记关系。改名只动展示名：id 不变，所以授权与已有会话都不受影响。
func portalInstanceRename(args []string) int {
	fs := flag.NewFlagSet("portal instance rename", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 2 {
		return usageKey(i18n.KeyCLIUsageInstanceRename)
	}
	ref := strings.TrimSpace(fs.Arg(0))
	newName := strings.TrimSpace(fs.Arg(1))

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	inst, err := resolveInstance(store, ref)
	if err != nil {
		return failKey(i18n.KeyCLIErrInstanceNotExit, ref)
	}
	oldName := inst.Name
	// 改成同一个名字不算错，也没必要写库。
	if oldName == newName {
		sayKey(os.Stdout, i18n.KeyCLIInstRenamed, oldName, newName)
		return 0
	}
	if err := portal.ValidateInstanceName(newName); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	// 重名要挡在这里：数据库有 UNIQUE 约束，放过去只会得到一句 SQL 错误，
	// 而用户想看的是「这个名字已经有人用了」。
	if other, err := store.InstanceByName(newName); err == nil && other.ID != inst.ID {
		return failKey(i18n.KeyAdminErrActionFailed,
			fmt.Sprintf("实例名 %q 已存在（id %s）", newName, other.ID))
	}
	inst.Name = newName
	if err := store.UpdateInstance(*inst); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	sayKey(os.Stdout, i18n.KeyCLIInstRenamed, oldName, newName)
	return 0
}

// portalGrant 实现 `dsh-gate manage grant`。
func portalGrant(args []string) int {
	if len(args) == 0 {
		sayKey(os.Stderr, i18n.KeyCLIPortalGrantHelp)
		return 2
	}
	switch args[0] {
	case "list":
		return portalGrantList(args[1:])
	case "add":
		return portalGrantChange(args[1:], true)
	case "rm", "remove":
		return portalGrantChange(args[1:], false)
	case "-h", "--help", "help":
		sayKey(os.Stdout, i18n.KeyCLIPortalGrantHelp)
		return 0
	default:
		return usageKey(i18n.KeyCLIUnknownPortalSub, args[0])
	}
}

func portalGrantList(args []string) int {
	fs := flag.NewFlagSet("portal grant list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	targets, err := store.ListUsers()
	if err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	if fs.NArg() == 1 {
		u, err := store.UserByName(fs.Arg(0))
		if err != nil {
			return failKey(i18n.KeyCLIErrUserNotExist, fs.Arg(0))
		}
		targets = []portal.User{*u}
	}
	for _, u := range targets {
		instances, err := store.InstancesForUser(u.ID)
		if err != nil {
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		if len(instances) == 0 {
			sayKey(os.Stdout, i18n.KeyCLIGrantEmpty, u.Name)
			continue
		}
		sayKey(os.Stdout, i18n.KeyCLIGrantHeader, u.Name, len(instances))
		for _, inst := range instances {
			fmt.Printf(tr(i18n.KeyCLIGrantRow)+"\n", inst.Name)
		}
	}
	return 0
}

func portalGrantChange(args []string, add bool) int {
	fs := flag.NewFlagSet("portal grant", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath, err := parsePortalFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageKey(i18n.KeyCLIUnknownPortalSub, err.Error())
	}
	if fs.NArg() != 2 {
		sayKey(os.Stderr, i18n.KeyCLIPortalGrantHelp)
		return 2
	}
	userName, instanceRef := fs.Arg(0), fs.Arg(1)

	store, code := openPortal(dbPath)
	if code != 0 {
		return code
	}
	defer store.Close()

	if _, err := store.UserByName(userName); err != nil {
		return failKey(i18n.KeyCLIErrUserNotExist, userName)
	}
	inst, err := resolveInstance(store, instanceRef)
	if err != nil {
		return failKey(i18n.KeyCLIErrInstanceNotExit, instanceRef)
	}

	if add {
		if err := store.Grant(userName, inst.ID, "cli"); err != nil {
			return failKey(i18n.KeyAdminErrActionFailed, err.Error())
		}
		sayKey(os.Stdout, i18n.KeyCLIGrantAdded, userName, inst.Name)
		return 0
	}
	if err := store.Revoke(userName, inst.ID); err != nil {
		return failKey(i18n.KeyAdminErrActionFailed, err.Error())
	}
	sayKey(os.Stdout, i18n.KeyCLIGrantRemoved, userName, inst.Name)
	return 0
}

// resolveInstance 支持「实例名」或「不透明 id」两种写法。
func resolveInstance(store *portal.Store, ref string) (*portal.Instance, error) {
	if inst, err := store.InstanceByName(ref); err == nil {
		return inst, nil
	}
	if inst, err := store.InstanceByID(ref); err == nil {
		return inst, nil
	}
	return nil, fmt.Errorf("%w: %s", portal.ErrNotFound, ref)
}

// 编译期提醒：这些类型/常量来自管理端包，改名时这里会先报错。
var _ = time.Time{}
