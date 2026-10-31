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
	KeyLoginUsername:            "账号",
	KeyLoginUsernamePlaceholder: "输入账号",
	KeyLoginPassword:            "密码",
	KeyLoginPasswordPlaceholder: "输入密码",
	KeyLoginShowPassword:        "显示密码",
	KeyLoginHidePassword:        "隐藏密码",
	KeyLoginSubmit:              "登录",
	KeyLoginSigningIn:           "正在登录…",
	KeyLoginHint:                "账号由网关管理员分配，如需开通请联系管理员。",
	KeyLoginFeat1:               "一个入口，多台设备：手机、公司电脑、家里的浏览器，登录同一个地址就能用本机的 dsh。",
	KeyLoginFeat2:               "dsh 始终只听 127.0.0.1，对外只有这一道门；账号、限速、锁定，全在网关这一层。",
	KeyLoginFeat3:               "一台机器可以跑多套 dsh：会话、插件、凭据互相隔离，各挂各的端口，互不打扰。",
	KeyLoginPetIdle:             "嗨，登录就交给我看守。",
	KeyLoginPetTyping:           "输入中……我盯着门呢。",
	KeyLoginPetPeek:             "口令要藏好呀，别被人瞄到。",
	KeyLoginPetSad:              "账号或口令不对，再试试？",
	KeyLoginPetSubmit:           "正在核对，马上就好……",
	KeyLoginFooterPrefix:        "由 dsh-gate 保护",

	KeyLoginErrBadRequest:  "请求格式有误",
	KeyLoginErrBadCreds:    "用户名或口令不正确",
	KeyLoginErrTooMany:     "尝试次数过多，请在 %s后再试",
	KeyLoginErrMethod:      "不支持的方法",
	KeyLoginErrNotSignedIn: "尚未登录，请先访问 %s",

	KeyHostLocal: "本机",

	KeyThemeLabel: "配色",
	KeyThemeAuto:  "跟随系统",
	KeyThemeLight: "亮色",
	KeyThemeDark:  "暗色",

	KeySchemeLabel:  "方案",
	KeySchemeSlate:  "石墨蓝",
	KeySchemeOcean:  "海蓝",
	KeySchemeViolet: "绛紫",
	KeySchemeForest: "墨绿",

	KeyStyleLabel:    "布局",
	KeyStyleConsole:  "控制台",
	KeyStyleOverview: "总览",
	KeyStyleSplit:    "分栏",

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
	KeyAdminInstTip:        "在此创建实例并安装 dsh。先建记录（立刻拿到 id 与端口），再按所选方式后台安装；启停与日志也可以走命令行 dsh-gate dsh。",
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
	KeyAdminInstCreate:             "创建实例",
	KeyAdminInstCreateHint:         "先建实例记录（拿到 id 与端口），再按所选方式安装 dsh。安装目录留空时按安装方式自动推导。",
	KeyAdminInstName:               "实例名",
	KeyAdminInstHome:               "实例目录（DSH_HOME）",
	KeyAdminInstHomeHint:           "留空 = %s",
	KeyAdminInstHomeHelp:           "dsh 的数据根：会话、插件、凭据与设置都在这里。一个实例一个目录即完全独立；留空用 ~/.dsh。",
	KeyAdminInstWorkspace:          "工作目录",
	KeyAdminInstWorkspacePh:        "留空 = 实例目录/workspace",
	KeyAdminInstWorkspaceHelp:      "agent 默认读写的工作区；留空用实例目录下的 workspace。",
	KeyAdminInstConfigDir:          "配置目录",
	KeyAdminInstConfigPh:           "留空 = 同实例目录",
	KeyAdminInstConfigHelp:         "一般留空。dsh 没有单独的配置目录，填了会顶替 DSH_HOME。",
	KeyAdminInstInstallDirPh:       "npx 不需要；本地源码填已构建好的仓库根目录",
	KeyAdminInstInstallDirHelp:     "git / pnpm 用来放 dsh 程序本体（npx 走 npm 缓存，不需要）；local 填你自己 clone、并已执行过 pnpm install 与 pnpm run build 的源码目录，网关不 clone、不构建。",
	KeyAdminInstDirsTitle:          "这些目录分别放什么？（一个目录就够）",
	KeyAdminInstDirsTip:            "dsh 只有 DSH_HOME 一个数据根：实例目录决定会话、凭据与设置放在哪，一个实例一个目录就是完全独立。配置目录是 DSH_HOME 的另一种写法；安装目录只有 git / pnpm / local 用（local 指向你手工构建好的源码）——两者都可以留空。",
	KeyAdminInstMetaConfigOverride: "（覆盖 DSH_HOME）",
	KeyAdminInstRemoveHome:         "删数据目录（DSH_HOME）",
	KeyAdminInstRemoveInstall:      "删安装目录",
	KeyAdminInstRemoveHint:         "勾选才会真删，不可恢复。",
	KeyAdminInstCardInstall:        "安装",
	KeyAdminInstCardRemove:         "删除",
	KeyAdminInstPort:               "dsh 端口",
	KeyAdminInstPortAuto:           "0 表示自动分配",
	KeyAdminInstListenHost:         "监听地址",
	KeyAdminInstStartMode:          "启动方式",
	KeyAdminInstStartAuto:          "随管理端启动",
	KeyAdminInstMetaSameHome:       "同 DSH_HOME",
	KeyAdminInstMetaUnset:          "未设置（默认 <实例目录>/workspace）",
	KeyAdminInstMetaCmdAuto:        "自动探测（PATH 里的 dsh，或 npx）",
	KeyAdminRunRunning:             "运行中",
	KeyAdminRunStarting:            "启动中",
	KeyAdminRunStopped:             "已停止",
	KeyAdminRunManaged:             "网关拉起",
	KeyAdminRunAdopted:             "接管的外部实例",
	KeyAdminInstStartDemand:        "按需启动（用户点击时）",
	KeyAdminInstMode:               "安装方式",
	KeyAdminInstModeNpx:            "npx 官方命令（无需预装）",
	KeyAdminInstModeGit:            "从源码构建（git clone + pnpm build）",
	KeyAdminInstModePnpm:           "pnpm 安装",
	KeyAdminInstModeLocal:          "本地源码（我自己 clone 并 build 好，网关只负责 pnpm dsh web 启动）",
	KeyAdminInstDir:                "安装目录",
	KeyAdminInstVersion:            "dsh 版本",
	KeyAdminInstVersionPh:          "留空 = 跟随 latest（可填 0.2.2-alpha.2 / next）",
	KeyAdminInstVersionHelp:        "填 npm 上的版本号或标签（dist-tag），例如 0.2.2-alpha.2、next、alpha，就能装到比 latest 更新的版本。留空跟随 latest，并在它跑不起来时自动退回验证过的旧版本；填了就是钉住这一个版本，装不上会直接报错，不会悄悄换成别的。有哪些可选用 npm view @deepseek-ai/dsh dist-tags 查。源码方式（从源码构建、本地源码）不认这一栏。",
	KeyAdminInstMetaVersionLatest:  "跟随 latest",
	KeyAdminInstMetaVersionBuild:   "%s（源码方式不生效）",
	KeyAdminInstEnvTitle:           "dsh 运行环境检测",
	KeyAdminInstEnvHint:            "检测结果只读，用来判断哪种安装方式现在可用。",
	KeyAdminInstEnvMissing:         "缺失",
	KeyAdminEnvBtnInstall:          "安装",
	KeyAdminEnvBtnRetry:            "重试",
	KeyAdminInstSelfNode:           "管理端自备 Node 运行时",
	KeyAdminInstSelfPnpm:           "管理端自备 pnpm",
	KeyAdminInstExisting:           "本机已有 dsh",
	KeyAdminInstNone:               "无",
	KeyAdminInstPhase:              "安装状态",
	KeyAdminInstStep:               "当前步骤",
	KeyAdminInstLog:                "安装日志",
	KeyAdminInstCmd:                "启动命令",
	KeyAdminInstCmdArgs:            "启动参数",
	KeyAdminInstInstalled:          "已安装",
	KeyAdminInstNotReady:           "未安装",
	KeyAdminInstPhasePend:          "排队中",
	KeyCLIInstInstallStart:         "正在为实例 %s 安装 dsh（方式：%s）…",
	KeyCLIInstInstallDone:          "实例 %s 的 dsh 已装好，启动命令：%s",
	KeyCLIInstInstallFailed:        "实例 %s 安装 dsh 失败：%s",
	KeyAdminInstPhaseRun:           "安装中",
	KeyAdminInstPhaseDone:          "已完成",
	KeyAdminInstPhaseFail:          "失败",
	KeyAdminBtnInstCreate:          "创建实例",
	KeyAdminBtnInstInstall:         "安装 / 重装",
	KeyAdminBtnInstRemove:          "删除实例",
	KeyAdminBtnInstOpen:            "进入",
	KeyAdminBtnInstStart:           "启动",
	KeyAdminBtnInstStop:            "停止",
	KeyAdminBtnInstRestart:         "重启",
	KeyAdminBtnInstSave:            "保存配置",
	KeyAdminColRun:                 "运行状态",
	KeyAdminPIDLabel:               "进程号 ",
	KeyAdminErrLabel:               "启动失败",
	KeyAdminEditTitle:              "编辑配置",
	KeyAdminSaveHint:               "保存后不会自动重启；要让改动生效请再点「重启」。",
	KeyAdminMsgInstCreated:         "实例 %s 已创建。",
	KeyAdminMsgInstRemoved:         "实例 %s 已删除（只解除管理关系，目录保留）。",
	KeyAdminMsgInstStarted:         "实例 %s 的安装已开始，可在下方查看进度。",
	KeyAdminMsgInstRemovedDirs:     "实例 %s 已删除，已删除目录：%s。",
	KeyRoleAdmin:                   "管理员",
	KeyRoleUser:                    "普通用户",

	// 审计页：动作与说明（库里存英文短标识，只有页面渲染时翻译）
	KeyAuditInstanceEnter:    "进入实例",
	KeyAuditInstanceDenied:   "访问被拒",
	KeyAuditUserCreate:       "创建用户",
	KeyAuditUserPasswd:       "修改口令",
	KeyAuditUserRole:         "修改角色",
	KeyAuditUserToggle:       "启用 / 停用用户",
	KeyAuditUserDelete:       "删除用户",
	KeyAuditGrantsSave:       "保存授权",
	KeyAuditInstanceCreate:   "创建实例",
	KeyAuditInstanceInstall:  "安装 dsh",
	KeyAuditInstanceRemove:   "删除实例",
	KeyAuditInstanceStart:    "启动实例",
	KeyAuditInstanceStop:     "停止实例",
	KeyAuditInstanceRestart:  "重启实例",
	KeyAuditInstanceUpdate:   "修改实例配置",
	KeyAuditEnvInstall:       "安装运行环境工具",
	KeyAuditCSRFRejected:     "提交被拒（跨站校验）",
	KeyAuditDetailFailed:     "失败：%s",
	KeyAuditDetailInstanceID: "实例 id：%s",

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
	KeyAdminErrSuperAdmin:   "第一个注册的账号是超级管理员，不能改角色、停用或删除。",
	KeyAdminSuperBadge:      "超级管理员",
	KeyAdminErrActionFailed: "操作失败：%s",
	KeyAdminErrBadRequest:   "请求缺少必要参数。",

	KeyAdminColAccount: "账号",
	KeyAdminColDisplay: "姓名",
	KeyAdminNameHint:   "登录账号：字母或数字开头，可用字母、数字与 . _ -（不支持中文；中文名填「姓名」）",

	KeyCLIPortalHelp: `用法: dsh-gate manage <user|grant> <子命令> [选项]

管理端把账号从「实例级」提升到「管理端一级」，并新增实例与授权。管理页面在浏览器里
（/admin），这里是等价能力的命令行入口。

  dsh-gate manage user add <用户名> [--role admin|user]   新建用户
  dsh-gate manage user passwd <用户名>                    修改口令
  dsh-gate manage user role <用户名> <admin|user>          改角色
  dsh-gate manage user rename <旧名> <新名>                改登录名（超管也可以改）
  dsh-gate manage user disable|enable <用户名>             停用 / 启用
  dsh-gate manage user list                               列出用户
  dsh-gate manage user rm <用户名>                         删除用户
  dsh-gate dsh list                                       实例列表 / 新增 / 编辑 / 改名
  dsh-gate dsh add|edit|rename|rm ...                     实例的增删改（dsh-gate dsh --help）
  dsh-gate dsh start|stop|restart <名字>                   启停单个实例（管理端要在运行）
  dsh-gate manage grant list [<用户名>]                    查看授权
  dsh-gate manage grant add <用户名> <实例名或 id>          授权
  dsh-gate manage grant rm <用户名> <实例名或 id>           取消授权

公共选项:
  --db <路径>         管理端数据库（默认 <状态目录>/portal.db）
  --state-dir <目录>  网关状态目录（默认 ~/.dsh-gate）
  --lang <zh-CN|en>   输出语言（也可用 DSH_GATE_LANG / LC_ALL / LANG）

实例的安装、启动、停止、日志见: dsh-gate dsh --help`,

	KeyCLIPortalUserHelp:    "用法: dsh-gate manage user <add|passwd|role|rename|disable|enable|list|rm> <用户名> [选项]",
	KeyCLIPortalInstHelp:    "用法: dsh-gate dsh <list|add|edit|rename|start|stop|restart|rm|install> [...]（实例命令已移到 dsh 子命令）",
	KeyCLIPortalGrantHelp:   "用法: dsh-gate manage grant <list|add|rm> [<用户名>] [<实例名或 id>]",
	KeyCLIUnknownPortalSub:  "未知的 portal 子命令 %q，可用：user / grant",
	KeyCLIUnknownCommand:    "未知子命令 %q",
	KeyCLIUsageUserName:     "请给出用户名：dsh-gate manage user add <用户名>",
	KeyCLIUsageInstanceName: "请给出实例名或以 id 指定",
	KeyCLIUsageRole:         "角色只能是 admin 或 user",
	KeyCLIUsageUserRename:   "请给出旧名与新名：dsh-gate manage user rename <旧名> <新名>",

	KeyCLIErrOpenStore:       "打开管理端数据库失败：%s",
	KeyCLIErrUserNotExist:    "用户 %q 不存在",
	KeyCLIErrInstanceNotExit: "实例 %q 不存在",
	KeyCLIErrLastAdmin:       "拒绝操作最后一个管理员（会导致无人能管理管理端）",
	KeyCLIErrUserExists:      "用户 %q 已存在",

	KeyCLIUserCreated:      "已创建用户 %q（角色 %s），当前共 %d 个",
	KeyCLIUserPasswd:       "已更新用户 %q 的口令（其全部会话已失效）",
	KeyCLIUserDeleted:      "已删除用户 %q，当前共 %d 个",
	KeyCLIUserRenamed:      "已把用户 %q 改名为 %q（其全部会话已失效，请用新名字重新登录）",
	KeyCLIUserRenamedSuper: "该账号是超级管理员：改名不改变身份，它现在以新名字继续承载超管权限",
	KeyCLIUserListHeader:   "管理端 %s（%d 个用户）:",
	KeyCLIUserListEmpty:    "还没有用户。创建一个：dsh-gate manage user add admin --role admin",
	KeyCLIUserRow:          "  %-16s %-10s %-6s %s",
	KeyCLIUserRoleChanged:  "已把 %q 的角色改为 %s",
	KeyCLIUserEnabled:      "已启用用户 %q",
	KeyCLIUserDisabled:     "已停用用户 %q（其全部会话已失效）",
	KeyCLIRandomNotice:     "初始口令（只显示这一次，请立即保存）：",

	KeyCLIInstCreated:         "已创建实例 %q（id %s，端口 %d，DSH_HOME %s）",
	KeyCLIInstDeleted:         "已删除实例 %q（dsh 数据目录未改动）",
	KeyCLIInstRenamed:         "已把实例 %q 重命名为 %q（实例 id 与授权不变，权限不受影响）",
	KeyCLIUsageInstanceRename: "请给出旧名与新名：dsh-gate dsh rename <旧名或 id> <新名>",
	KeyCLIInstListHeader:      "实例（%d 个）:",
	KeyCLIInstListEmpty:       "还没有实例。创建一个：dsh-gate dsh add alice-main --dsh-home /srv/dsh/alice",
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

	KeyCLITrayUnsupported: "托盘只在 Windows 上提供（本平台请用 dsh-gate start / stop / status）",
	KeyTrayTipRunning:     "dsh-gate 管理端运行中 · %s",
	KeyTrayTipStopped:     "dsh-gate 管理端已停止",
	KeyTrayStateRunning:   "运行中",
	KeyTrayStateStopped:   "已停止",
	KeyTrayMenuOpen:       "打开管理台",
	KeyTrayMenuStart:      "启动管理端",
	KeyTrayMenuStop:       "停止管理端",
	KeyTrayMenuRestart:    "重启管理端",
	KeyTrayMenuQuit:       "退出托盘（管理端继续运行）",
	KeyTrayOpFailed:       "操作失败：%s",

	KeyCLIInstallUsage: `用法: dsh-gate install [选项]

安装管理端：建好管理员账号、把命令装进 PATH，并可顺带启动或设为开机自启。
安装过程不拉起任何 dsh——实例由你在管理端管理台里按需添加。
交互式终端里逐项询问（回车接受括号里的默认值）；脚本里全部用选项给出。

向导顺序: 监听地址 → 端口 → 安装目录 → 配置与数据库目录 → 管理员账号与姓名 → 口令
          → 是否后台启动 → 是否开机自启 → 汇总确认

  --host <地址>       管理端监听地址（默认 0.0.0.0）
  --port <端口>       管理端监听端口（默认 12020）
  --state-dir <目录>  配置与数据库目录（默认 ~/.dsh-gate）
  --admin <账号>      管理员账号（字母数字开头，可含 . _ -；默认 admin，不支持中文）
  --display <姓名>    姓名（给人看，可中文；留空回退账号名）
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
	KeyCLIInstallCreated:     "已创建管理端管理员 %q，当前共 %d 个账号",
	KeyCLIInstallExists:      "管理端已有账号（共 %d 个），未创建新账号",
	KeyCLIInstallHasAccounts: "管理端已有账号（共 %d 个），本次不建号，账号与口令保持原样：",
	KeyCLIInstallExistingRow: "  %-16s %-12s %-12s %s",
	KeyCLIInstallStarted:     "管理端已启动：http://127.0.0.1:%d/（管理台 /admin）",
	KeyCLIInstallNotStarted:  "管理端尚未启动。启动：dsh-gate start --port %d",
	KeyCLIInstallNextSteps: `接下来:
  dsh-gate start / stop / restart   启动、停止、重启整个管理端
  dsh-gate status                   查看管理端与实例状态
  dsh-gate dsh add <名字> --dsh-home <目录> --start-mode ondemand
      添加一个实例（按需启动：管理端重启不会自动拉起它）
  dsh-gate manage grant add %s <实例名>
      把实例授权给某个用户
  dsh-gate dsh list
      查看实例与自动分配的端口
  dsh-gate uninstall
      停止并删除所有实例，最后卸载管理端服务（会先问是否删实例数据）`,
	KeyCLIInstallBadFlag: "--%s 不属于 install 的范围；实例参数请用 dsh-gate dsh add 或到管理台里配置",

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
	KeyLoginSigningIn:           "Signing in…",
	KeyLoginHint:                "Accounts are issued by the gateway administrator; contact them for access.",
	KeyLoginFeat1:               "One door, every device: phone, office PC, home browser — sign in at the same address and use this machine's dsh.",
	KeyLoginFeat2:               "dsh still listens on 127.0.0.1 only; this gateway is the single entrance — accounts, rate limiting and lockouts all live here.",
	KeyLoginFeat3:               "Run several dsh instances on one machine: sessions, plugins and credentials stay isolated, each on its own port.",
	KeyLoginPetIdle:             "Hi there — the door is on me.",
	KeyLoginPetTyping:           "Typing away… I'm watching the gate.",
	KeyLoginPetPeek:             "Keep that password hidden!",
	KeyLoginPetSad:              "Wrong account or password. Try again?",
	KeyLoginPetSubmit:           "Checking credentials… one moment…",
	KeyLoginFooterPrefix:        "Protected by dsh-gate",

	KeyLoginErrBadRequest:  "Malformed request",
	KeyLoginErrBadCreds:    "Incorrect username or password",
	KeyLoginErrTooMany:     "Too many attempts; try again in %s",
	KeyLoginErrMethod:      "Method not allowed",
	KeyLoginErrNotSignedIn: "Not signed in — open %s first",

	KeyHostLocal: "local",

	KeyThemeLabel: "Theme",
	KeyThemeAuto:  "System",
	KeyThemeLight: "Light",
	KeyThemeDark:  "Dark",

	KeySchemeLabel:  "Palette",
	KeySchemeSlate:  "Slate",
	KeySchemeOcean:  "Ocean",
	KeySchemeViolet: "Violet",
	KeySchemeForest: "Forest",

	KeyStyleLabel:    "Layout",
	KeyStyleConsole:  "Console",
	KeyStyleOverview: "Overview",
	KeyStyleSplit:    "Split",

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
	KeyAdminInstTip:        "Create instances and install dsh here. The record is created first (giving you an id and a port immediately), then dsh installs in the background; start/stop and logs are also available via dsh-gate dsh.",
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
	KeyAdminInstCreate:             "Create instance",
	KeyAdminInstCreateHint:         "The instance record is created first (with an id and port), then dsh is installed with the selected method. Leave the install directory empty to derive it from the method.",
	KeyAdminInstName:               "Instance name",
	KeyAdminInstHome:               "Instance directory (DSH_HOME)",
	KeyAdminInstHomeHint:           "blank = %s",
	KeyAdminInstHomeHelp:           "dsh's data root: sessions, plugins, credentials and settings live here. One directory per instance = fully isolated; blank = ~/.dsh.",
	KeyAdminInstWorkspace:          "Workspace",
	KeyAdminInstWorkspacePh:        "blank = instance dir/workspace",
	KeyAdminInstWorkspaceHelp:      "Agent's default workspace; blank = workspace under the instance directory.",
	KeyAdminInstConfigDir:          "Config directory",
	KeyAdminInstConfigPh:           "blank = same as instance directory",
	KeyAdminInstConfigHelp:         "Usually blank. dsh has no separate config directory; if set it replaces DSH_HOME.",
	KeyAdminInstInstallDirPh:       "not needed for npx; for local, the built repo root",
	KeyAdminInstInstallDirHelp:     "git / pnpm keep the dsh program here (npx uses the npm cache instead); local points at the checkout you cloned and ran pnpm install + pnpm run build in — the gateway neither clones nor builds.",
	KeyAdminInstDirsTitle:          "What goes in each directory? (one directory is enough)",
	KeyAdminInstDirsTip:            "dsh has a single data root, DSH_HOME: the instance directory decides where sessions, credentials and settings go, and one directory per instance is full isolation. The config directory is just another spelling of DSH_HOME; the install directory matters only for git / pnpm / local (local points at your own built source tree). Both may be left blank.",
	KeyAdminInstMetaConfigOverride: " (overrides DSH_HOME)",
	KeyAdminInstRemoveHome:         "delete data dir (DSH_HOME)",
	KeyAdminInstRemoveInstall:      "delete install dir",
	KeyAdminInstRemoveHint:         "Checked dirs are really deleted, not recoverable.",
	KeyAdminInstCardInstall:        "Install",
	KeyAdminInstCardRemove:         "Remove",
	KeyAdminInstPort:               "dsh port",
	KeyAdminInstPortAuto:           "0 = allocate automatically",
	KeyAdminInstListenHost:         "Listen address",
	KeyAdminInstStartMode:          "Start mode",
	KeyAdminInstStartAuto:          "Start with the console",
	KeyAdminInstStartDemand:        "Start on demand (when a user opens it)",
	KeyAdminInstMetaSameHome:       "same as DSH_HOME",
	KeyAdminInstMetaUnset:          "unset (defaults to <instance dir>/workspace)",
	KeyAdminInstMetaCmdAuto:        "auto-detected (dsh on PATH, or npx)",
	KeyAdminRunRunning:             "running",
	KeyAdminRunStarting:            "starting",
	KeyAdminRunStopped:             "stopped",
	KeyAdminRunManaged:             "started by the gateway",
	KeyAdminRunAdopted:             "adopted external instance",
	KeyAdminInstMode:               "Install method",
	KeyAdminInstModeNpx:            "npx official command (no prerequisites)",
	KeyAdminInstModeGit:            "Build from source (git clone + pnpm build)",
	KeyAdminInstModePnpm:           "pnpm install",
	KeyAdminInstModeLocal:          "Local source (I clone and build it myself; the gateway only runs pnpm dsh web)",
	KeyAdminInstDir:                "Install directory",
	KeyAdminInstVersion:            "dsh version",
	KeyAdminInstVersionPh:          "blank = follow latest (e.g. 0.2.2-alpha.2 / next)",
	KeyAdminInstVersionHelp:        "Enter an npm version or a dist-tag (0.2.2-alpha.2, next, alpha) to install something newer than latest. Left blank the install follows latest, which also keeps the automatic fallback to a verified older release when latest cannot run. A pinned version is never substituted: if it cannot be installed the job fails loudly instead of quietly picking another one. Run npm view @deepseek-ai/dsh dist-tags to see what is available. Source methods (build from source, local source) ignore this field.",
	KeyAdminInstMetaVersionLatest:  "follows latest",
	KeyAdminInstMetaVersionBuild:   "%s (ignored by source methods)",
	KeyAdminInstEnvTitle:           "dsh runtime environment",
	KeyAdminInstEnvHint:            "Detection is read-only and shows which install methods are usable right now.",
	KeyAdminInstEnvMissing:         "missing",
	KeyAdminEnvBtnInstall:          "install",
	KeyAdminEnvBtnRetry:            "retry",
	KeyAdminInstSelfNode:           "Console-provided Node runtime",
	KeyAdminInstSelfPnpm:           "Console-provided pnpm",
	KeyAdminInstExisting:           "Existing dsh on this host",
	KeyAdminInstNone:               "none",
	KeyAdminInstPhase:              "Install status",
	KeyAdminInstStep:               "Current step",
	KeyAdminInstLog:                "Install log",
	KeyAdminInstCmd:                "Launch command",
	KeyAdminInstCmdArgs:            "Launch flags",
	KeyAdminInstInstalled:          "installed",
	KeyAdminInstNotReady:           "not installed",
	KeyAdminInstPhasePend:          "queued",
	KeyCLIInstInstallStart:         "Installing dsh for instance %s (method: %s)…",
	KeyCLIInstInstallDone:          "dsh for instance %s is ready; launch command: %s",
	KeyCLIInstInstallFailed:        "Failed to install dsh for instance %s: %s",
	KeyAdminInstPhaseRun:           "installing",
	KeyAdminInstPhaseDone:          "done",
	KeyAdminInstPhaseFail:          "failed",
	KeyAdminBtnInstCreate:          "Create instance",
	KeyAdminBtnInstInstall:         "Install / reinstall",
	KeyAdminBtnInstRemove:          "Delete instance",
	KeyAdminBtnInstOpen:            "Open",
	KeyAdminBtnInstStart:           "Start",
	KeyAdminBtnInstStop:            "Stop",
	KeyAdminBtnInstRestart:         "Restart",
	KeyAdminBtnInstSave:            "Save config",
	KeyAdminColRun:                 "Run state",
	KeyAdminPIDLabel:               "pid ",
	KeyAdminErrLabel:               "start failed",
	KeyAdminEditTitle:              "Edit config",
	KeyAdminSaveHint:               "Saving does not restart it; click Restart to apply the change.",
	KeyAdminMsgInstCreated:         "Instance %s created.",
	KeyAdminMsgInstRemoved:         "Instance %s deleted (management entry only; its directories are kept).",
	KeyAdminMsgInstStarted:         "Install for instance %s has started; progress is shown below.",
	KeyAdminMsgInstRemovedDirs:     "Instance %s deleted; removed directories: %s.",
	KeyRoleAdmin:                   "Administrator",
	KeyRoleUser:                    "User",

	// Audit page: actions and details (the store keeps English short ids;
	// only the page rendering translates them).
	KeyAuditInstanceEnter:    "Enter instance",
	KeyAuditInstanceDenied:   "Access denied",
	KeyAuditUserCreate:       "Create user",
	KeyAuditUserPasswd:       "Change password",
	KeyAuditUserRole:         "Change role",
	KeyAuditUserToggle:       "Enable / disable user",
	KeyAuditUserDelete:       "Delete user",
	KeyAuditGrantsSave:       "Save grants",
	KeyAuditInstanceCreate:   "Create instance",
	KeyAuditInstanceInstall:  "Install dsh",
	KeyAuditInstanceRemove:   "Delete instance",
	KeyAuditInstanceStart:    "Start instance",
	KeyAuditInstanceStop:     "Stop instance",
	KeyAuditInstanceRestart:  "Restart instance",
	KeyAuditInstanceUpdate:   "Edit instance config",
	KeyAuditEnvInstall:       "Install env tool",
	KeyAuditCSRFRejected:     "Rejected (CSRF check)",
	KeyAuditDetailFailed:     "Failed: %s",
	KeyAuditDetailInstanceID: "instance id: %s",

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
	KeyAdminErrSuperAdmin:   "The first registered account is the super administrator; its role cannot be changed, and it cannot be disabled or deleted.",
	KeyAdminSuperBadge:      "Super administrator",
	KeyAdminErrActionFailed: "Action failed: %s",
	KeyAdminErrBadRequest:   "The request is missing required parameters.",

	KeyAdminColAccount: "Account",
	KeyAdminColDisplay: "Full name",
	KeyAdminNameHint:   "Login account: start with a letter or digit; letters, digits and . _ - only (no non-ASCII; put the display name in “Full name”)",

	KeyCLIPortalHelp: `Usage: dsh-gate manage <user|grant> <subcommand> [options]

The portal lifts accounts from "per instance" to "portal level" and adds instances
and per-user access. The same capabilities are available in the browser at /admin;
this is the command-line equivalent.

  dsh-gate manage user add <name> [--role admin|user]   create a user
  dsh-gate manage user passwd <name>                    change a password
  dsh-gate manage user role <name> <admin|user>          change a role
  dsh-gate manage user rename <old> <new>                rename an account (works for the super admin too)
  dsh-gate manage user disable|enable <name>             disable / enable
  dsh-gate manage user list                             list users
  dsh-gate manage user rm <name>                        delete a user
  dsh-gate dsh list                                    list / add / edit / rename instances
  dsh-gate dsh add|edit|rename|rm ...                  instance CRUD (dsh-gate dsh --help)
  dsh-gate dsh start|stop|restart <name>               start / stop one instance (console must be running)
  dsh-gate manage grant list [<user>]                  show access
  dsh-gate manage grant add <user> <instance name or id> grant access
  dsh-gate manage grant rm <user> <instance name or id>  revoke access

Common options:
  --db <path>          portal database (default <state dir>/portal.db)
  --state-dir <dir>    gateway state directory (default ~/.dsh-gate)
  --lang <zh-CN|en>    output language (also DSH_GATE_LANG / LC_ALL / LANG)

Instance install / start / stop / logs: see dsh-gate dsh --help`,

	KeyCLIPortalUserHelp:    "Usage: dsh-gate manage user <add|passwd|role|rename|disable|enable|list|rm> <name> [options]",
	KeyCLIPortalInstHelp:    "Usage: dsh-gate dsh <list|add|edit|rename|start|stop|restart|rm|install> [...] (instance commands moved under dsh)",
	KeyCLIPortalGrantHelp:   "Usage: dsh-gate manage grant <list|add|rm> [<user>] [<instance name or id>]",
	KeyCLIUnknownPortalSub:  "unknown portal subcommand %q; expected user / grant",
	KeyCLIUnknownCommand:    "unknown subcommand %q",
	KeyCLIUsageUserName:     "a user name is required: dsh-gate manage user add <name>",
	KeyCLIUsageInstanceName: "an instance name (or id) is required",
	KeyCLIUsageRole:         "the role must be admin or user",
	KeyCLIUsageUserRename:   "two arguments are required: dsh-gate manage user rename <old name> <new name>",

	KeyCLIErrOpenStore:       "cannot open the portal database: %s",
	KeyCLIErrUserNotExist:    "user %q does not exist",
	KeyCLIErrInstanceNotExit: "instance %q does not exist",
	KeyCLIErrLastAdmin:       "the last administrator cannot be changed (nobody would be able to administer the portal)",
	KeyCLIErrUserExists:      "user %q already exists",

	KeyCLIUserCreated:      "created user %q (role %s); %d in total",
	KeyCLIUserPasswd:       "changed the password of %q (all their sessions were revoked)",
	KeyCLIUserDeleted:      "deleted user %q; %d remaining",
	KeyCLIUserRenamed:      "renamed user %q to %q (all their sessions were revoked; sign in with the new name)",
	KeyCLIUserRenamedSuper: "this account is the super administrator: renaming does not change that — it keeps the role under its new name",
	KeyCLIUserListHeader:   "portal %s (%d users):",
	KeyCLIUserListEmpty:    "no users yet. Create one: dsh-gate manage user add admin --role admin",
	KeyCLIUserRow:          "  %-16s %-10s %-6s %s",
	KeyCLIUserRoleChanged:  "changed the role of %q to %s",
	KeyCLIUserEnabled:      "enabled user %q",
	KeyCLIUserDisabled:     "disabled user %q (all their sessions were revoked)",
	KeyCLIRandomNotice:     "initial password (shown once — save it now):",

	KeyCLIInstCreated:         "created instance %q (id %s, port %d, DSH_HOME %s)",
	KeyCLIInstDeleted:         "deleted instance %q (the dsh data directory is untouched)",
	KeyCLIInstRenamed:         "renamed instance %q to %q (the instance id and its grants are unchanged)",
	KeyCLIUsageInstanceRename: "two arguments are required: dsh-gate dsh rename <old name or id> <new name>",
	KeyCLIInstListHeader:      "instances (%d):",
	KeyCLIInstListEmpty:       "no instances yet. Create one: dsh-gate dsh add alice-main --dsh-home /srv/dsh/alice",
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

	KeyCLITrayUnsupported: "the tray exists on Windows only (use dsh-gate start / stop / status here)",
	KeyTrayTipRunning:     "dsh-gate console running · %s",
	KeyTrayTipStopped:     "dsh-gate console stopped",
	KeyTrayStateRunning:   "running",
	KeyTrayStateStopped:   "stopped",
	KeyTrayMenuOpen:       "Open the admin page",
	KeyTrayMenuStart:      "Start the console",
	KeyTrayMenuStop:       "Stop the console",
	KeyTrayMenuRestart:    "Restart the console",
	KeyTrayMenuQuit:       "Quit the tray (console keeps running)",
	KeyTrayOpFailed:       "operation failed: %s",

	KeyCLIInstallUsage: `Usage: dsh-gate install [options]

Installs the console: creates an admin account, puts the command on PATH, and can
run it in the background or install it as a login service. No dsh is started
during installation — you add instances from the console admin page later.
In an interactive terminal it asks step by step; in scripts everything comes
from options.

Wizard order: host -> port -> install dir -> config & database dir -> admin account &
              display name -> password -> run in background -> login service -> confirm

  --host <addr>       console listen address (default 0.0.0.0)
  --port <port>       console listen port (default 12020)
  --state-dir <dir>   config & database dir (default ~/.dsh-gate)
  --admin <account>   admin account (start with letter/digit; . _ - allowed; ASCII only, default admin)
  --display <name>    display name (may be non-ASCII; falls back to the account)
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
	KeyCLIInstallCreated:     "created portal admin %q (%d accounts total)",
	KeyCLIInstallExists:      "portal already has accounts (%d total); no new account created",
	KeyCLIInstallHasAccounts: "portal already has accounts (%d total); nothing is created, names and passwords stay as they are:",
	KeyCLIInstallExistingRow: "  %-16s %-12s %-12s %s",
	KeyCLIInstallStarted:     "portal is up: http://127.0.0.1:%d/ (admin console at /admin)",
	KeyCLIInstallNotStarted:  "portal is not running yet. Start it with: dsh-gate start --port %d",
	KeyCLIInstallNextSteps: `Next steps:
  dsh-gate start / stop / restart   start, stop, restart the whole console
  dsh-gate status                   console and instance status
  dsh-gate dsh add <name> --dsh-home <dir> --start-mode ondemand
      add an instance (on-demand: restarting the portal will not start it)
  dsh-gate manage grant add %s <instance>
      grant an instance to a user
  dsh-gate dsh list
      list instances and their assigned ports
  dsh-gate uninstall
      stop and delete all instances, then remove the console service (asks before deleting data)`,
	KeyCLIInstallBadFlag: "--%s is not part of install; configure instances with dsh-gate dsh add or in the admin console",

	KeyDurationSeconds:        "%ds",
	KeyDurationMinutes:        "%dm",
	KeyDurationMinutesSeconds: "%dm %ds",
	KeyDurationHours:          "%dh",
	KeyDurationHoursMinutes:   "%dh %dm",
}
