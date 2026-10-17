// 文案目录本体。两个目录必须覆盖 allKeys 里的每一条且都非空（i18n_test.go 锁死）。
//
// 中文是默认语言，因此它同时承担「回退语言」的角色；英文缺一条也会在测试里
// 立刻暴露，不会等到用户切到英文才发现。
package i18n

// catalogs 是语言 → 目录。键来自 allKeys；不要在这里加未登记的键。
var catalogs = map[Lang]map[string]string{
	ZH: zh,
	EN: en,
}

var zh = map[string]string{
	KeyLoginUsername:            "用户名",
	KeyLoginUsernamePlaceholder: "输入用户名",
	KeyLoginPassword:            "密码",
	KeyLoginPasswordPlaceholder: "输入密码",
	KeyLoginShowPassword:        "显示密码",
	KeyLoginHidePassword:        "隐藏密码",
	KeyLoginSubmit:              "登录",
	KeyLoginHint:                "账号由网关管理员分配，如需开通请联系管理员。",
	KeyLoginFooterPrefix:        "由 dsh-gate 保护",

	KeyLoginErrBadRequest:  "请求格式有误",
	KeyLoginErrBadCreds:    "用户名或口令不正确",
	KeyLoginErrTooMany:     "尝试次数过多，请在 %s后再试",
	KeyLoginErrMethod:      "不支持的方法",
	KeyLoginErrNotSignedIn: "尚未登录，请先访问 %s",

	KeyHostLocal: "本机",

	KeyPortalTitle:       "我的实例",
	KeyPortalSignedInAs:  "已登录：%s",
	KeyPortalAdmin:       "管理台",
	KeyPortalSignOut:     "退出登录",
	KeyPortalEnter:       "进入",
	KeyPortalNoInstances: "还没有分配给你的实例，请联系管理员。",

	KeyPortalErrNotFound:    "实例不存在，或你没有访问权限。",
	KeyPortalErrUnavailable: "该实例暂时不可用（尚未接入运行环境）。",
	KeyPortalErrInternal:    "服务内部错误，请稍后重试。",

	KeyAdminTitle:          "管理台",
	KeyAdminBack:           "返回我的实例",
	KeyAdminUsers:          "用户",
	KeyAdminGrants:         "实例授权",
	KeyAdminGrantsTip:      "勾选某个用户可以进入的实例，保存后立即生效（管理员默认可以进入全部实例）。",
	KeyAdminInstances:      "实例",
	KeyAdminInstTip:        "在此创建实例并安装 dsh。先建记录（立刻拿到 id 与端口），再按所选方式后台安装；启停与日志走命令行。",
	KeyAdminAudit:          "审计",
	KeyAdminNavLabel:       "管理台分页",
	KeyAdminNavEnv:         "运行环境",
	KeyAdminColName:        "名字",
	KeyAdminColRole:        "角色",
	KeyAdminColStatus:      "状态",
	KeyAdminColInstances:   "已授权实例",
	KeyAdminColPort:        "端口",
	KeyAdminColTime:        "时间",
	KeyAdminColActor:       "操作者",
	KeyAdminColAction:      "动作",
	KeyAdminColTarget:      "对象",
	KeyAdminColDetail:      "说明",
	KeyAdminBtnApply:       "应用",
	KeyAdminBtnPassword:    "改密",
	KeyAdminBtnEnable:      "启用",
	KeyAdminBtnDisable:     "停用",
	KeyAdminBtnDelete:      "删除",
	KeyAdminBtnCreateUser:  "创建用户",
	KeyAdminBtnSaveGrants:  "保存授权",
	KeyAdminPhPassword:     "口令（至少 8 位）",
	KeyAdminPhNewPassword:  "新口令",
	KeyAdminStatusEnabled:  "启用",
	KeyAdminStatusDisabled: "停用",
	KeyAdminNoUsers:        "还没有用户。",
	KeyAdminNoInstances:    "还没有实例。",
	KeyAdminNoAudit:        "还没有审计记录。",

	// 实例安装面板
	KeyAdminInstCreate:       "创建实例",
	KeyAdminInstCreateHint:   "先建实例记录（拿到 id 与端口），再按所选方式安装 dsh。安装目录留空时按安装方式自动推导。",
	KeyAdminInstName:         "实例名",
	KeyAdminInstHome:         "实例目录（DSH_HOME）",
	KeyAdminInstWorkspace:    "工作目录",
	KeyAdminInstConfigDir:    "配置目录",
	KeyAdminInstPort:         "dsh 端口",
	KeyAdminInstPortAuto:     "0 表示自动分配",
	KeyAdminInstListenHost:   "监听地址",
	KeyAdminInstStartMode:    "启动方式",
	KeyAdminInstStartAuto:    "随管理端启动",
	KeyAdminInstMetaSameHome: "同 DSH_HOME",
	KeyAdminInstMetaUnset:    "未设置（默认 <实例目录>/workspace）",
	KeyAdminInstStartDemand:  "按需启动（用户点击时）",
	KeyAdminInstMode:         "安装方式",
	KeyAdminInstModeNpx:      "npx 官方命令（无需预装）",
	KeyAdminInstModeGit:      "从源码构建（git clone + pnpm build）",
	KeyAdminInstModePnpm:     "pnpm 安装",
	KeyAdminInstDir:          "安装目录",
	KeyAdminInstEnvTitle:     "dsh 运行环境检测",
	KeyAdminInstEnvHint:      "检测结果只读，用来判断哪种安装方式现在可用。",
	KeyAdminInstEnvMissing:   "缺失",
	KeyAdminEnvBtnInstall:    "安装",
	KeyAdminEnvBtnRetry:      "重试",
	KeyAdminInstSelfNode:     "管理端自备 Node 运行时",
	KeyAdminInstSelfPnpm:     "管理端自备 pnpm",
	KeyAdminInstExisting:     "本机已有 dsh",
	KeyAdminInstNone:         "无",
	KeyAdminInstPhase:        "安装状态",
	KeyAdminInstStep:         "当前步骤",
	KeyAdminInstLog:          "安装日志",
	KeyAdminInstCmd:          "启动命令",
	KeyAdminInstInstalled:    "已安装",
	KeyAdminInstNotReady:     "未安装",
	KeyAdminInstPhasePend:    "排队中",
	KeyCLIInstInstallStart:   "正在为实例 %s 安装 dsh（方式：%s）…",
	KeyCLIInstInstallDone:    "实例 %s 的 dsh 已装好，启动命令：%s",
	KeyCLIInstInstallFailed:  "实例 %s 安装 dsh 失败：%s",
	KeyAdminInstPhaseRun:     "安装中",
	KeyAdminInstPhaseDone:    "已完成",
	KeyAdminInstPhaseFail:    "失败",
	KeyAdminBtnInstCreate:    "创建实例",
	KeyAdminBtnInstInstall:   "安装 / 重装",
	KeyAdminBtnInstRemove:    "删除实例",
	KeyAdminMsgInstCreated:   "实例 %s 已创建。",
	KeyAdminMsgInstRemoved:   "实例已删除（数据目录保留）。",
	KeyAdminMsgInstStarted:   "实例 %s 的安装已开始，可在下方查看进度。",
	KeyRoleAdmin:             "管理员",
	KeyRoleUser:              "普通用户",

	KeyAdminMsgUserCreated:     "已创建用户 %s",
	KeyAdminMsgPasswordChanged: "已修改 %s 的口令，其全部会话已失效",
	KeyAdminMsgRoleChanged:     "已把 %s 的角色改为 %s",
	KeyAdminMsgEnabled:         "已启用 %s",
	KeyAdminMsgDisabled:        "已停用 %s，其全部会话已失效",
	KeyAdminMsgUserDeleted:     "已删除 %s",
	KeyAdminMsgGrantsSaved:     "已更新 %s 的实例授权",

	KeyAdminErrForbidden:    "需要管理员权限。",
	KeyAdminErrCSRF:         "表单已过期，请刷新页面后重试。",
	KeyAdminErrUserExists:   "用户 %s 已存在。",
	KeyAdminErrLastAdmin:    "不能停用、降级或删除最后一个管理员（会导致无人能管理管理端）。",
	KeyAdminErrActionFailed: "操作失败：%s",
	KeyAdminErrBadRequest:   "请求缺少必要参数。",

	KeyCLIPortalHelp: `用法: dsh-gate manage <user|instance|grant> <子命令> [选项]

管理端把账号从「实例级」提升到「管理端一级」，并新增实例与授权。管理页面在浏览器里
（/admin），这里是等价能力的命令行入口。

  dsh-gate manage user add <用户名> [--role admin|user]   新建用户
  dsh-gate manage user passwd <用户名>                    修改口令
  dsh-gate manage user role <用户名> <admin|user>          改角色
  dsh-gate manage user disable|enable <用户名>             停用 / 启用
  dsh-gate manage user list                               列出用户
  dsh-gate manage user rm <用户名>                         删除用户
  dsh-gate manage instance list                           列出实例
  dsh-gate manage instance add <名字> --dsh-home <目录>    新建实例（自动分配 id 与端口）
  dsh-gate manage instance install <名字>                  为该实例安装 dsh（--install-mode npx|git|pnpm）
  dsh-gate manage instance rm <名字>                       删除实例（不动 dsh 数据目录）
  dsh-gate manage grant list [<用户名>]                    查看授权
  dsh-gate manage grant add <用户名> <实例名或 id>          授权
  dsh-gate manage grant rm <用户名> <实例名或 id>           取消授权

公共选项:
  --db <路径>         管理端数据库（默认 <状态目录>/portal.db）
  --state-dir <目录>  网关状态目录（默认 ~/.dsh-gate）
  --lang <zh-CN|en>   输出语言（也可用 DSH_GATE_LANG / LC_ALL / LANG）

实例的安装选项（也可在管理台 /admin 页面上操作）:
  --install-mode <npx|git|pnpm>  装 dsh 的方式（默认 npx）
                                 npx  = 用 npx 现拉现跑，最省事
                                 git  = clone 源码后 pnpm install && build，可改代码
                                 pnpm = 装到实例目录下的 node_modules，可离线复用
  --install-dir <目录>           安装目录（npx 方式不需要）
  --config-dir <目录>            配置目录（等价于 dsh 的 DSH_HOME）
  --listen-host <地址>           监听地址（默认 127.0.0.1）
  --on-demand                    按需启动（有访问才拉起 dsh）

实例的启动 / 停止 / 重启 / 日志需要先接入运行环境，属于下一步。`,

	KeyCLIPortalUserHelp:    "用法: dsh-gate manage user <add|passwd|role|disable|enable|list|rm> <用户名> [选项]",
	KeyCLIPortalInstHelp:    "用法: dsh-gate manage instance <list|add|install|rename|rm> [...]\n  add 需要 --dsh-home；install 用 --install-mode 选 npx/git/pnpm\n  rename 用 <旧名或 id> <新名> 改展示名（id 与授权不变，实例名可写中文）",
	KeyCLIPortalGrantHelp:   "用法: dsh-gate manage grant <list|add|rm> [<用户名>] [<实例名或 id>]",
	KeyCLIUnknownPortalSub:  "未知的 portal 子命令 %q，可用：user / instance / grant",
	KeyCLIUnknownCommand:    "未知子命令 %q",
	KeyCLIUsageUserName:     "请给出用户名：dsh-gate manage user add <用户名>",
	KeyCLIUsageInstanceName: "请给出实例名或以 id 指定",
	KeyCLIUsageInstanceHome: "新建实例必须指定 --dsh-home（该实例的 DSH_HOME）",
	KeyCLIUsageRole:         "角色只能是 admin 或 user",

	KeyCLIErrOpenStore:       "打开管理端数据库失败：%s",
	KeyCLIErrUserNotExist:    "用户 %q 不存在",
	KeyCLIErrInstanceNotExit: "实例 %q 不存在",
	KeyCLIErrLastAdmin:       "拒绝操作最后一个管理员（会导致无人能管理管理端）",
	KeyCLIErrUserExists:      "用户 %q 已存在",

	KeyCLIUserCreated:     "已创建用户 %q（角色 %s），当前共 %d 个",
	KeyCLIUserPasswd:      "已更新用户 %q 的口令（其全部会话已失效）",
	KeyCLIUserDeleted:     "已删除用户 %q，当前共 %d 个",
	KeyCLIUserListHeader:  "管理端 %s（%d 个用户）:",
	KeyCLIUserListEmpty:   "还没有用户。创建一个：dsh-gate manage user add admin --role admin",
	KeyCLIUserRow:         "  %-16s %-10s %-6s %s",
	KeyCLIUserRoleChanged: "已把 %q 的角色改为 %s",
	KeyCLIUserEnabled:     "已启用用户 %q",
	KeyCLIUserDisabled:    "已停用用户 %q（其全部会话已失效）",
	KeyCLIRandomNotice:    "初始口令（只显示这一次，请立即保存）：",

	KeyCLIInstCreated:         "已创建实例 %q（id %s，端口 %d，DSH_HOME %s）",
	KeyCLIInstDeleted:         "已删除实例 %q（dsh 数据目录未改动）",
	KeyCLIInstRenamed:         "已把实例 %q 重命名为 %q（实例 id 与授权不变，权限不受影响）",
	KeyCLIUsageInstanceRename: "请给出旧名与新名：dsh-gate manage instance rename <旧名或 id> <新名>",
	KeyCLIInstListHeader:      "实例（%d 个）:",
	KeyCLIInstListEmpty:       "还没有实例。创建一个：dsh-gate manage instance add alice-main --dsh-home /srv/dsh/alice",
	KeyCLIInstRow:             "  %-16s %-10s %-30s %-6s %s",

	KeyCLIGrantAdded:   "已授权：%s → %s",
	KeyCLIGrantRemoved: "已取消授权：%s → %s",
	KeyCLIGrantHeader:  "%s 可访问的实例（%d 个）:",
	KeyCLIGrantRow:     "  %s",
	KeyCLIGrantEmpty:   "%s 还没有被授权任何实例。",

	KeyCLIStatusEnabled:  "启用",
	KeyCLIStatusDisabled: "停用",

	KeyCLIPortalRunStarted: "管理端已启动：%s（实例 %d 个）",
	KeyCLIPortalRunNoAccounts: `管理端里还没有任何账号，先创建一个：

    dsh-gate manage user add admin --role admin --db %s

（管理端拒绝在没有任何账号的情况下启动——否则没人能登录，也没人能建号。）`,
	KeyCLIPortalRunStopping: "收到退出信号，正在收尾…",
	KeyCLIPortalRunFailed:   "启动管理端失败：%s",

	KeyCLIInstallUsage: `用法: dsh-gate install [选项]

安装管理端：建好管理员账号、把命令装进 PATH，并可顺带启动或设为开机自启。
安装过程不拉起任何 dsh——实例由你在管理端管理台里按需添加。
交互式终端里逐项询问（回车接受括号里的默认值）；脚本里全部用选项给出。

向导顺序: 监听地址 → 端口 → 安装目录 → 配置与数据库目录 → 管理员 → 口令
          → 是否后台启动 → 是否开机自启 → 汇总确认

  --host <地址>       管理端监听地址（默认 0.0.0.0）
  --port <端口>       管理端监听端口（默认 3082）
  --state-dir <目录>  配置与数据库目录（默认 ~/.dsh-gate）
  --admin <用户名>    管理员用户名（默认 admin）
  --random            自动生成强口令并打印一次
  --password-stdin    从标准输入读取口令（脚本 / CI）
  --start             安装完成后立即把管理端放到后台运行
  --service           安装开机（登录）自启并立即启动（蕴含 --start）
  --bin-dir <目录>    把 dsh-gate 拷贝到这个目录（需在 PATH 里）；留空则不拷贝

示例:
  dsh-gate install
      全程交互

  dsh-gate install --port 12099 --random --start
      脚本模式：非默认项用选项给出，装好并后台运行，稍后打开
      http://127.0.0.1:12099/admin 配置实例`,
	KeyCLIInstallNeedPassword: `当前不是交互式终端，install 无法询问口令。
改用以下任一方式:
  echo "$PASSWORD" | dsh-gate install --password-stdin
  dsh-gate install --random`,
	KeyCLIInstallCreated:    "已创建管理端管理员 %q，当前共 %d 个账号",
	KeyCLIInstallExists:     "管理端已有账号（共 %d 个），未创建新账号",
	KeyCLIInstallStarted:    "管理端已启动：http://127.0.0.1:%d/（管理台 /admin）",
	KeyCLIInstallNotStarted: "管理端尚未启动。启动：dsh-gate manage run --background --port %d",
	KeyCLIInstallNextSteps: `接下来:
  dsh-gate manage instance add <名字> --dsh-home <目录> --on-demand
      添加一个实例（默认按需启动：管理端重启不会自动拉起它）
  dsh-gate manage grant add %s <实例名>
      把实例授权给某个用户
  dsh-gate manage instance list
      查看实例与自动分配的端口`,
	KeyCLIInstallBadFlag: "--%s 不属于 install 的范围；实例参数请在管理端管理台里配置，或用 dsh-gate manage instance add",

	KeyDurationSeconds:        "%d 秒",
	KeyDurationMinutes:        "%d 分钟",
	KeyDurationMinutesSeconds: "%d 分 %d 秒",
	KeyDurationHours:          "%d 小时",
	KeyDurationHoursMinutes:   "%d 小时 %d 分",
}

var en = map[string]string{
	KeyLoginUsername:            "Username",
	KeyLoginUsernamePlaceholder: "Enter your username",
	KeyLoginPassword:            "Password",
	KeyLoginPasswordPlaceholder: "Enter your password",
	KeyLoginShowPassword:        "Show password",
	KeyLoginHidePassword:        "Hide password",
	KeyLoginSubmit:              "Sign in",
	KeyLoginHint:                "Accounts are issued by the gateway administrator; contact them for access.",
	KeyLoginFooterPrefix:        "Protected by dsh-gate",

	KeyLoginErrBadRequest:  "Malformed request",
	KeyLoginErrBadCreds:    "Incorrect username or password",
	KeyLoginErrTooMany:     "Too many attempts; try again in %s",
	KeyLoginErrMethod:      "Method not allowed",
	KeyLoginErrNotSignedIn: "Not signed in — open %s first",

	KeyHostLocal: "local",

	KeyPortalTitle:       "My instances",
	KeyPortalSignedInAs:  "Signed in as %s",
	KeyPortalAdmin:       "Admin",
	KeyPortalSignOut:     "Sign out",
	KeyPortalEnter:       "Open",
	KeyPortalNoInstances: "No instances have been assigned to you yet — contact the administrator.",

	KeyPortalErrNotFound:    "Instance not found, or you do not have access.",
	KeyPortalErrUnavailable: "This instance is temporarily unavailable.",
	KeyPortalErrInternal:    "Internal server error; please try again later.",

	KeyAdminTitle:          "Admin",
	KeyAdminBack:           "My instances",
	KeyAdminUsers:          "Users",
	KeyAdminGrants:         "Instance access",
	KeyAdminGrantsTip:      "Tick the instances each user may enter; the change applies immediately (administrators can enter every instance).",
	KeyAdminInstances:      "Instances",
	KeyAdminInstTip:        "Create instances and install dsh here. The record is created first (giving you an id and a port immediately), then dsh installs in the background; start/stop and logs stay on the CLI.",
	KeyAdminAudit:          "Audit",
	KeyAdminNavLabel:       "Console sections",
	KeyAdminNavEnv:         "Environment",
	KeyAdminColName:        "Name",
	KeyAdminColRole:        "Role",
	KeyAdminColStatus:      "Status",
	KeyAdminColInstances:   "Granted instances",
	KeyAdminColPort:        "Port",
	KeyAdminColTime:        "Time",
	KeyAdminColActor:       "Actor",
	KeyAdminColAction:      "Action",
	KeyAdminColTarget:      "Target",
	KeyAdminColDetail:      "Detail",
	KeyAdminBtnApply:       "Apply",
	KeyAdminBtnPassword:    "Password",
	KeyAdminBtnEnable:      "Enable",
	KeyAdminBtnDisable:     "Disable",
	KeyAdminBtnDelete:      "Delete",
	KeyAdminBtnCreateUser:  "Create user",
	KeyAdminBtnSaveGrants:  "Save access",
	KeyAdminPhPassword:     "Password (8+ characters)",
	KeyAdminPhNewPassword:  "New password",
	KeyAdminStatusEnabled:  "Enabled",
	KeyAdminStatusDisabled: "Disabled",
	KeyAdminNoUsers:        "No users yet.",
	KeyAdminNoInstances:    "No instances yet.",
	KeyAdminNoAudit:        "No audit entries yet.",

	// Instance install panel
	KeyAdminInstCreate:       "Create instance",
	KeyAdminInstCreateHint:   "The instance record is created first (with an id and port), then dsh is installed with the selected method. Leave the install directory empty to derive it from the method.",
	KeyAdminInstName:         "Instance name",
	KeyAdminInstHome:         "Instance directory (DSH_HOME)",
	KeyAdminInstWorkspace:    "Workspace",
	KeyAdminInstConfigDir:    "Config directory",
	KeyAdminInstPort:         "dsh port",
	KeyAdminInstPortAuto:     "0 = allocate automatically",
	KeyAdminInstListenHost:   "Listen address",
	KeyAdminInstStartMode:    "Start mode",
	KeyAdminInstStartAuto:    "Start with the console",
	KeyAdminInstStartDemand:  "Start on demand (when a user opens it)",
	KeyAdminInstMetaSameHome: "same as DSH_HOME",
	KeyAdminInstMetaUnset:    "unset (defaults to <instance dir>/workspace)",
	KeyAdminInstMode:         "Install method",
	KeyAdminInstModeNpx:      "npx official command (no prerequisites)",
	KeyAdminInstModeGit:      "Build from source (git clone + pnpm build)",
	KeyAdminInstModePnpm:     "pnpm install",
	KeyAdminInstDir:          "Install directory",
	KeyAdminInstEnvTitle:     "dsh runtime environment",
	KeyAdminInstEnvHint:      "Detection is read-only and shows which install methods are usable right now.",
	KeyAdminInstEnvMissing:   "missing",
	KeyAdminEnvBtnInstall:    "install",
	KeyAdminEnvBtnRetry:      "retry",
	KeyAdminInstSelfNode:     "Console-provided Node runtime",
	KeyAdminInstSelfPnpm:     "Console-provided pnpm",
	KeyAdminInstExisting:     "Existing dsh on this host",
	KeyAdminInstNone:         "none",
	KeyAdminInstPhase:        "Install status",
	KeyAdminInstStep:         "Current step",
	KeyAdminInstLog:          "Install log",
	KeyAdminInstCmd:          "Launch command",
	KeyAdminInstInstalled:    "installed",
	KeyAdminInstNotReady:     "not installed",
	KeyAdminInstPhasePend:    "queued",
	KeyCLIInstInstallStart:   "Installing dsh for instance %s (method: %s)…",
	KeyCLIInstInstallDone:    "dsh for instance %s is ready; launch command: %s",
	KeyCLIInstInstallFailed:  "Failed to install dsh for instance %s: %s",
	KeyAdminInstPhaseRun:     "installing",
	KeyAdminInstPhaseDone:    "done",
	KeyAdminInstPhaseFail:    "failed",
	KeyAdminBtnInstCreate:    "Create instance",
	KeyAdminBtnInstInstall:   "Install / reinstall",
	KeyAdminBtnInstRemove:    "Delete instance",
	KeyAdminMsgInstCreated:   "Instance %s created.",
	KeyAdminMsgInstRemoved:   "Instance deleted (its data directory is kept).",
	KeyAdminMsgInstStarted:   "Install for instance %s has started; progress is shown below.",
	KeyRoleAdmin:             "Administrator",
	KeyRoleUser:              "User",

	KeyAdminMsgUserCreated:     "Created user %s",
	KeyAdminMsgPasswordChanged: "Changed %s's password; all their sessions were revoked",
	KeyAdminMsgRoleChanged:     "Changed %s's role to %s",
	KeyAdminMsgEnabled:         "Enabled %s",
	KeyAdminMsgDisabled:        "Disabled %s; all their sessions were revoked",
	KeyAdminMsgUserDeleted:     "Deleted %s",
	KeyAdminMsgGrantsSaved:     "Updated instance access for %s",

	KeyAdminErrForbidden:    "Administrator permission required.",
	KeyAdminErrCSRF:         "The form expired — reload the page and try again.",
	KeyAdminErrUserExists:   "User %s already exists.",
	KeyAdminErrLastAdmin:    "The last administrator cannot be disabled, demoted or deleted (nobody would be able to administer the portal).",
	KeyAdminErrActionFailed: "Action failed: %s",
	KeyAdminErrBadRequest:   "The request is missing required parameters.",

	KeyCLIPortalHelp: `Usage: dsh-gate manage <user|instance|grant> <subcommand> [options]

The portal lifts accounts from "per instance" to "portal level" and adds instances
and per-user access. The same capabilities are available in the browser at /admin;
this is the command-line equivalent.

  dsh-gate manage user add <name> [--role admin|user]   create a user
  dsh-gate manage user passwd <name>                    change a password
  dsh-gate manage user role <name> <admin|user>          change a role
  dsh-gate manage user disable|enable <name>             disable / enable
  dsh-gate manage user list                             list users
  dsh-gate manage user rm <name>                        delete a user
  dsh-gate manage instance list                         list instances
  dsh-gate manage instance add <name> --dsh-home <dir>  create an instance (id and port auto-assigned)
  dsh-gate manage instance install <name>               install dsh for the instance (--install-mode npx|git|pnpm)
  dsh-gate manage instance rm <name>                    delete an instance (the dsh data directory is untouched)
  dsh-gate manage grant list [<user>]                   show access
  dsh-gate manage grant add <user> <instance name or id> grant access
  dsh-gate manage grant rm <user> <instance name or id>  revoke access

Common options:
  --db <path>          portal database (default <state dir>/portal.db)
  --state-dir <dir>    gateway state directory (default ~/.dsh-gate)
  --lang <zh-CN|en>    output language (also DSH_GATE_LANG / LC_ALL / LANG)

Install options for an instance (also available on the /admin page):
  --install-mode <npx|git|pnpm>  how to install dsh (default npx)
                                 npx  = fetch and run on demand, least setup
                                 git  = clone the sources, then pnpm install && build (hackable)
                                 pnpm = install into the instance directory's node_modules (reusable offline)
  --install-dir <dir>            install directory (not needed for npx)
  --config-dir <dir>             config directory (same as dsh's DSH_HOME)
  --listen-host <addr>           listen address (default 127.0.0.1)
  --on-demand                    start on demand (spawn dsh only when accessed)

Starting, stopping, restarting and reading instance logs need the runtime
integration and arrive in the next step.`,

	KeyCLIPortalUserHelp:    "Usage: dsh-gate manage user <add|passwd|role|disable|enable|list|rm> <name> [options]",
	KeyCLIPortalInstHelp:    "Usage: dsh-gate manage instance <list|add|install|rename|rm> [...]\n  add requires --dsh-home; install takes --install-mode npx|git|pnpm\n  rename <old name or id> <new name> changes the display name (id and grants unchanged; Unicode names allowed)",
	KeyCLIPortalGrantHelp:   "Usage: dsh-gate manage grant <list|add|rm> [<user>] [<instance name or id>]",
	KeyCLIUnknownPortalSub:  "unknown portal subcommand %q; expected user / instance / grant",
	KeyCLIUnknownCommand:    "unknown subcommand %q",
	KeyCLIUsageUserName:     "a user name is required: dsh-gate manage user add <name>",
	KeyCLIUsageInstanceName: "an instance name (or id) is required",
	KeyCLIUsageInstanceHome: "creating an instance requires --dsh-home (that instance's DSH_HOME)",
	KeyCLIUsageRole:         "the role must be admin or user",

	KeyCLIErrOpenStore:       "cannot open the portal database: %s",
	KeyCLIErrUserNotExist:    "user %q does not exist",
	KeyCLIErrInstanceNotExit: "instance %q does not exist",
	KeyCLIErrLastAdmin:       "the last administrator cannot be changed (nobody would be able to administer the portal)",
	KeyCLIErrUserExists:      "user %q already exists",

	KeyCLIUserCreated:     "created user %q (role %s); %d in total",
	KeyCLIUserPasswd:      "changed the password of %q (all their sessions were revoked)",
	KeyCLIUserDeleted:     "deleted user %q; %d remaining",
	KeyCLIUserListHeader:  "portal %s (%d users):",
	KeyCLIUserListEmpty:   "no users yet. Create one: dsh-gate manage user add admin --role admin",
	KeyCLIUserRow:         "  %-16s %-10s %-6s %s",
	KeyCLIUserRoleChanged: "changed the role of %q to %s",
	KeyCLIUserEnabled:     "enabled user %q",
	KeyCLIUserDisabled:    "disabled user %q (all their sessions were revoked)",
	KeyCLIRandomNotice:    "initial password (shown once — save it now):",

	KeyCLIInstCreated:         "created instance %q (id %s, port %d, DSH_HOME %s)",
	KeyCLIInstDeleted:         "deleted instance %q (the dsh data directory is untouched)",
	KeyCLIInstRenamed:         "renamed instance %q to %q (the instance id and its grants are unchanged)",
	KeyCLIUsageInstanceRename: "two arguments are required: dsh-gate manage instance rename <old name or id> <new name>",
	KeyCLIInstListHeader:      "instances (%d):",
	KeyCLIInstListEmpty:       "no instances yet. Create one: dsh-gate manage instance add alice-main --dsh-home /srv/dsh/alice",
	KeyCLIInstRow:             "  %-16s %-10s %-30s %-6s %s",

	KeyCLIGrantAdded:   "granted: %s → %s",
	KeyCLIGrantRemoved: "revoked: %s → %s",
	KeyCLIGrantHeader:  "instances %s can enter (%d):",
	KeyCLIGrantRow:     "  %s",
	KeyCLIGrantEmpty:   "%s has not been granted any instance.",

	KeyCLIStatusEnabled:  "enabled",
	KeyCLIStatusDisabled: "disabled",

	KeyCLIPortalRunStarted: "Portal is up at %s (%d instances)",
	KeyCLIPortalRunNoAccounts: `The portal has no accounts yet; create one first:

    dsh-gate manage user add admin --role admin --db %s

(The portal refuses to start without any account — otherwise nobody could log in,
and nobody could create one either.)`,
	KeyCLIPortalRunStopping: "Shutting down…",
	KeyCLIPortalRunFailed:   "cannot start the portal: %s",

	KeyCLIInstallUsage: `Usage: dsh-gate install [options]

Installs the console: creates an admin account, puts the command on PATH, and can
run it in the background or install it as a login service. No dsh is started
during installation — you add instances from the console admin page later.
In an interactive terminal it asks step by step; in scripts everything comes
from options.

Wizard order: host -> port -> install dir -> config & database dir -> admin ->
              password -> run in background -> login service -> confirm

  --host <addr>       console listen address (default 0.0.0.0)
  --port <port>       console listen port (default 3082)
  --state-dir <dir>   config & database dir (default ~/.dsh-gate)
  --admin <name>      admin username (default admin)
  --random            generate a strong password and print it once
  --password-stdin    read the password from stdin (scripts / CI)
  --start             run the console in the background right after installing
  --service           install as a login service and start it now (implies --start)
  --bin-dir <dir>     copy dsh-gate here (must be on PATH); empty = skip

Example:
  dsh-gate install
      fully interactive

  dsh-gate install --port 12099 --random --start
      script mode: installs the console and runs it in the background; then open
      http://127.0.0.1:12099/admin to configure instances`,
	KeyCLIInstallNeedPassword: `Not an interactive terminal, so install cannot ask for a password.
Use one of these instead:
  echo "$PASSWORD" | dsh-gate install --password-stdin
  dsh-gate install --random`,
	KeyCLIInstallCreated:    "created portal admin %q (%d accounts total)",
	KeyCLIInstallExists:     "portal already has accounts (%d total); no new account created",
	KeyCLIInstallStarted:    "portal is up: http://127.0.0.1:%d/ (admin console at /admin)",
	KeyCLIInstallNotStarted: "portal is not running yet. Start it with: dsh-gate manage run --background --port %d",
	KeyCLIInstallNextSteps: `Next steps:
  dsh-gate manage instance add <name> --dsh-home <dir> --on-demand
      add an instance (on-demand by default: restarting the portal will not start it)
  dsh-gate manage grant add %s <instance>
      grant an instance to a user
  dsh-gate manage instance list
      list instances and their auto-assigned ports`,
	KeyCLIInstallBadFlag: "--%s is not part of install; configure instance parameters in the portal admin console, or use dsh-gate manage instance add",

	KeyDurationSeconds:        "%ds",
	KeyDurationMinutes:        "%dm",
	KeyDurationMinutesSeconds: "%dm %ds",
	KeyDurationHours:          "%dh",
	KeyDurationHoursMinutes:   "%dh %dm",
}
