// Package i18n 是 dsh-gate 自带界面的文案目录（CLI 与页面共用）。
//
// 为什么自己写：本项目的依赖策略只允许一处第三方依赖（SQLite，见 go.mod），
// 而需求很简单——两种语言、按 key 查、缺 key 回退默认语言。
//
// 约定：
//
//   - **key 用常量**（本文件下方的 Key* ），不在别处写裸字符串：这样「代码用了
//     一个目录里不存在的 key」在编译期与测试里都能被发现（见 i18n_test.go）。
//   - **两种语言的键必须完全一致**、且都不为空——由测试锁死。新增文案时
//     两个目录一起加，否则测试失败。
//   - T() 找不到 key 时回退默认语言；再找不到就原样返回 key。页面上出现一个
//     key 很难看，但总好过空白——而测试保证这种情况不会进发布版本。
//   - 服务的是「网关自己的界面」（登录页、管理端页、CLI 输出）。被代理的 dsh
//     页面不归这里管：那是 DSH 自己的 i18n。
package i18n

import (
	"fmt"
	"sort"
	"strings"
)

// Lang 是受支持的语言标签。
type Lang string

const (
	// ZH 是默认语言（也是回退语言）。
	ZH Lang = "zh-CN"
	// EN 是英文。
	EN Lang = "en"
	// Default 是没有更多线索时使用的语言。
	Default = ZH
)

// Languages 按稳定顺序返回受支持的语言。
func Languages() []Lang { return []Lang{ZH, EN} }

// Supported 判断语言标签是否在受支持集合里（精确匹配，不做前缀收敛）。
func Supported(l Lang) bool { _, ok := catalogs[l]; return ok }

// Normalize 把任意语言标签收敛到受支持的语言：`zh`、`zh-Hans`、`zh_CN` → ZH；
// `en-US`、`en_GB` → EN；其它一律回默认。
func Normalize(raw string) Lang {
	tag := strings.ToLower(strings.TrimSpace(raw))
	if tag == "" {
		return Default
	}
	// 容忍 "zh-CN,zh;q=0.9" 这种整段传进来的情况：只看第一段。
	if i := strings.IndexAny(tag, ",;"); i >= 0 {
		tag = strings.TrimSpace(tag[:i])
	}
	switch {
	case strings.HasPrefix(tag, "zh"):
		return ZH
	case strings.HasPrefix(tag, "en"):
		return EN
	}
	return Default
}

// Detect 按「显式指定 > DSH_GATE_LANG > LC_ALL > LANG > 默认」推导语言。
// getenv 允许传 nil（等同全部为空）。
func Detect(explicit string, getenv func(string) string) Lang {
	get := func(name string) string {
		if getenv == nil {
			return ""
		}
		return getenv(name)
	}
	for _, raw := range []string{explicit, get("DSH_GATE_LANG"), get("LC_ALL"), get("LANG")} {
		if strings.TrimSpace(raw) != "" {
			return Normalize(raw)
		}
	}
	return Default
}

// FromAcceptLanguage 取 Accept-Language 里第一个能识别的语言。
// 第二个返回值为 false 表示整段都没有受支持的语言（此时用默认语言）。
func FromAcceptLanguage(header string) (Lang, bool) {
	for _, part := range strings.Split(header, ",") {
		tag := part
		if i := strings.Index(tag, ";"); i >= 0 {
			tag = tag[:i]
		}
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}
		switch {
		case strings.HasPrefix(tag, "zh"):
			return ZH, true
		case strings.HasPrefix(tag, "en"):
			return EN, true
		}
	}
	return Default, false
}

// T 查文案并做格式化（args 为空时原样返回）。
func T(lang Lang, key string, args ...any) string {
	text, ok := lookup(lang, key)
	if !ok {
		text, ok = lookup(Default, key)
	}
	if !ok {
		return key
	}
	if len(args) == 0 {
		return text
	}
	return fmt.Sprintf(text, args...)
}

// Has 判断某语言下是否有这条文案（回退不算）。
func Has(lang Lang, key string) bool {
	_, ok := lookup(lang, key)
	return ok
}

func lookup(lang Lang, key string) (string, bool) {
	catalog, ok := catalogs[lang]
	if !ok {
		return "", false
	}
	text, ok := catalog[key]
	return text, ok
}

// Keys 返回全部文案键（按字典序）。测试用它与目录逐一核对。
func Keys() []string {
	out := make([]string, 0, len(allKeys))
	out = append(out, allKeys...)
	sort.Strings(out)
	return out
}

// Catalog 返回某语言的目录副本（测试与排查用）。
func Catalog(lang Lang) map[string]string {
	src, ok := catalogs[lang]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// ---- 文案键（唯一真源：代码里只准用这些常量） ----

const (
	// 登录页
	KeyLoginUsername            = "login.username"
	KeyLoginUsernamePlaceholder = "login.usernamePlaceholder"
	KeyLoginPassword            = "login.password"
	KeyLoginPasswordPlaceholder = "login.passwordPlaceholder"
	KeyLoginShowPassword        = "login.showPassword"
	KeyLoginHidePassword        = "login.hidePassword"
	KeyLoginSubmit              = "login.submit"
	KeyLoginSigningIn           = "login.signingIn"
	KeyLoginHint                = "login.hint"
	KeyLoginFooterPrefix        = "login.footerPrefix"

	// 登录页左栏介绍与宠物（小守卫）的状态文案
	KeyLoginFeat1     = "login.feat1"
	KeyLoginFeat2     = "login.feat2"
	KeyLoginFeat3     = "login.feat3"
	KeyLoginPetIdle   = "login.pet.idle"
	KeyLoginPetTyping = "login.pet.typing"
	KeyLoginPetPeek   = "login.pet.peek"
	KeyLoginPetSad    = "login.pet.sad"
	KeyLoginPetSubmit = "login.pet.submit"

	// 登录失败提示
	KeyLoginErrBadRequest  = "login.err.badRequest"
	KeyLoginErrBadCreds    = "login.err.badCredentials"
	KeyLoginErrTooMany     = "login.err.tooMany"
	KeyLoginErrMethod      = "login.err.methodNotAllowed"
	KeyLoginErrNotSignedIn = "login.err.notSignedIn"

	// 其它
	KeyHostLocal = "host.local"

	// 配色切换（三个页面页脚共用）
	KeyThemeLabel = "theme.label"
	KeyThemeAuto  = "theme.auto"
	KeyThemeLight = "theme.light"
	KeyThemeDark  = "theme.dark"

	// 配色方案切换（与明暗正交：色相；三个页面页脚共用）
	KeySchemeLabel  = "scheme.label"
	KeySchemeSlate  = "scheme.slate"
	KeySchemeOcean  = "scheme.ocean"
	KeySchemeViolet = "scheme.violet"
	KeySchemeForest = "scheme.forest"

	// 布局风格切换（第三维度：信息架构；管理台页脚用）
	KeyStyleLabel    = "style.label"
	KeyStyleConsole  = "style.console"
	KeyStyleOverview = "style.overview"
	KeyStyleSplit    = "style.split"

	// 管理端首页（「我的实例」）
	KeyPortalTitle       = "portal.title"
	KeyPortalSignedInAs  = "portal.signedInAs"
	KeyPortalAdmin       = "portal.admin"
	KeyPortalSignOut     = "portal.signOut"
	KeyPortalEnter       = "portal.enter"
	KeyPortalNoInstances = "portal.noInstances"

	// 管理端错误提示
	KeyPortalErrNotFound    = "portal.err.notFound"
	KeyPortalErrUnavailable = "portal.err.unavailable"
	KeyPortalErrInternal    = "portal.err.internal"

	// 管理台：框架
	KeyAdminTitle     = "admin.title"
	KeyAdminBack      = "admin.back"
	KeyAdminUsers     = "admin.users"
	KeyAdminGrants    = "admin.grants"
	KeyAdminGrantsTip = "admin.grants.hint"
	KeyAdminInstances = "admin.instances"
	KeyAdminInstTip   = "admin.instances.hint"
	KeyAdminAudit     = "admin.audit"

	// 管理台：分页导航（悬浮菜单）。内容分五页，导航常驻侧边 / 底部。
	KeyAdminNavLabel = "admin.nav.label"
	KeyAdminNavEnv   = "admin.nav.env"

	// 管理台：表头与字段
	KeyAdminColName      = "admin.col.name"
	KeyAdminColRole      = "admin.col.role"
	KeyAdminColStatus    = "admin.col.status"
	KeyAdminColInstances = "admin.col.instances"
	KeyAdminColPort      = "admin.col.port"
	KeyAdminColTime      = "admin.col.time"
	KeyAdminColActor     = "admin.col.actor"
	KeyAdminColAction    = "admin.col.action"
	KeyAdminColTarget    = "admin.col.target"
	KeyAdminColDetail    = "admin.col.detail"

	// 管理台：按钮与占位符
	KeyAdminBtnApply       = "admin.btn.apply"
	KeyAdminBtnPassword    = "admin.btn.password"
	KeyAdminBtnEnable      = "admin.btn.enable"
	KeyAdminBtnDisable     = "admin.btn.disable"
	KeyAdminBtnDelete      = "admin.btn.delete"
	KeyAdminBtnCreateUser  = "admin.btn.createUser"
	KeyAdminBtnSaveGrants  = "admin.btn.saveGrants"
	KeyAdminPhPassword     = "admin.ph.password"
	KeyAdminPhNewPassword  = "admin.ph.newPassword"
	KeyAdminStatusEnabled  = "admin.status.enabled"
	KeyAdminStatusDisabled = "admin.status.disabled"
	KeyAdminNoUsers        = "admin.noUsers"
	KeyAdminNoInstances    = "admin.noInstances"
	KeyAdminNoAudit        = "admin.noAudit"

	// 管理台：实例安装（环境检测 + 四种安装方式 + 进度）
	KeyAdminInstCreate             = "admin.inst.create"
	KeyAdminInstCreateHint         = "admin.inst.createHint"
	KeyAdminInstName               = "admin.inst.name"
	KeyAdminInstHome               = "admin.inst.home"
	KeyAdminInstHomeHint           = "admin.inst.homePlaceholder"
	KeyAdminInstHomeHelp           = "admin.inst.homeHelp"
	KeyAdminInstWorkspace          = "admin.inst.workspace"
	KeyAdminInstWorkspacePh        = "admin.inst.workspacePlaceholder"
	KeyAdminInstWorkspaceHelp      = "admin.inst.workspaceHelp"
	KeyAdminInstConfigDir          = "admin.inst.configDir"
	KeyAdminInstConfigPh           = "admin.inst.configPlaceholder"
	KeyAdminInstConfigHelp         = "admin.inst.configHelp"
	KeyAdminInstInstallDirPh       = "admin.inst.installDirPlaceholder"
	KeyAdminInstInstallDirHelp     = "admin.inst.installDirHelp"
	KeyAdminInstDirsTitle          = "admin.inst.dirsTitle"
	KeyAdminInstDirsTip            = "admin.inst.dirsTip"
	KeyAdminInstMetaConfigOverride = "admin.inst.metaConfigOverride"
	// 删除实例时可选连带删除的目录（两个勾选框 + 一句不可恢复的提醒）
	KeyAdminInstRemoveHome    = "admin.inst.removeHome"
	KeyAdminInstRemoveInstall = "admin.inst.removeInstall"
	KeyAdminInstRemoveHint    = "admin.inst.removeHint"
	// 操作列两张卡的小标题：勾选框和按钮的归属要在视觉上说清楚
	KeyAdminInstCardInstall  = "admin.inst.cardInstall"
	KeyAdminInstCardRemove   = "admin.inst.cardRemove"
	KeyAdminInstPort         = "admin.inst.port"
	KeyAdminInstPortAuto     = "admin.inst.portAuto"
	KeyAdminInstListenHost   = "admin.inst.listenHost"
	KeyAdminInstStartMode    = "admin.inst.startMode"
	KeyAdminInstMetaSameHome = "admin.inst.metaSameHome"
	KeyAdminInstMetaUnset    = "admin.inst.metaUnset"
	KeyAdminInstMetaCmdAuto  = "admin.inst.metaCmdAuto"
	KeyAdminRunRunning       = "admin.run.running"
	KeyAdminRunStarting      = "admin.run.starting"
	KeyAdminRunStopped       = "admin.run.stopped"
	KeyAdminRunManaged       = "admin.run.managed"
	KeyAdminRunAdopted       = "admin.run.adopted"
	KeyAdminInstStartAuto    = "admin.inst.startAuto"
	KeyAdminInstStartDemand  = "admin.inst.startDemand"
	KeyAdminInstMode         = "admin.inst.mode"
	KeyAdminInstModeNpx      = "admin.inst.modeNpx"
	KeyAdminInstModeGit      = "admin.inst.modeGit"
	KeyAdminInstModePnpm     = "admin.inst.modePnpm"
	KeyAdminInstModeLocal    = "admin.inst.modeLocal"
	KeyAdminInstDir          = "admin.inst.dir"

	// 「装哪个版本的 dsh」：npx / pnpm 方式可以把版本或 dist-tag 钉住。
	KeyAdminInstVersion           = "admin.inst.version"
	KeyAdminInstVersionPh         = "admin.inst.versionPlaceholder"
	KeyAdminInstVersionHelp       = "admin.inst.versionHelp"
	KeyAdminInstMetaVersionLatest = "admin.inst.metaVersionLatest"
	KeyAdminInstMetaVersionBuild  = "admin.inst.metaVersionBuild"

	KeyAdminInstEnvTitle   = "admin.inst.envTitle"
	KeyAdminInstEnvHint    = "admin.inst.envHint"
	KeyAdminInstEnvMissing = "admin.inst.envMissing"
	KeyAdminEnvBtnInstall  = "admin.env.btnInstall"
	KeyAdminEnvBtnRetry    = "admin.env.btnRetry"
	KeyAdminInstSelfNode   = "admin.inst.selfNode"
	KeyAdminInstSelfPnpm   = "admin.inst.selfPnpm"
	KeyAdminInstExisting   = "admin.inst.existing"
	KeyAdminInstNone       = "admin.inst.none"
	KeyAdminInstPhase      = "admin.inst.phase"
	KeyAdminInstStep       = "admin.inst.step"
	KeyAdminInstLog        = "admin.inst.log"
	KeyAdminInstCmd        = "admin.inst.cmd"
	KeyAdminInstCmdArgs    = "admin.inst.cmdArgs"
	KeyAdminInstInstalled  = "admin.inst.installed"
	KeyAdminInstNotReady   = "admin.inst.notReady"
	KeyAdminInstPhasePend  = "admin.inst.phasePending"
	// CLI：实例安装
	KeyCLIInstInstallStart  = "cli.inst.installStart"
	KeyCLIInstInstallDone   = "cli.inst.installDone"
	KeyCLIInstInstallFailed = "cli.inst.installFailed"
	KeyAdminInstPhaseRun    = "admin.inst.phaseRunning"
	KeyAdminInstPhaseDone   = "admin.inst.phaseDone"
	KeyAdminInstPhaseFail   = "admin.inst.phaseFailed"
	KeyAdminBtnInstCreate   = "admin.btn.instCreate"
	KeyAdminBtnInstInstall  = "admin.btn.instInstall"
	KeyAdminBtnInstRemove   = "admin.btn.instRemove"
	KeyAdminBtnInstOpen     = "admin.btn.instOpen"
	KeyAdminBtnInstStart    = "admin.btn.instStart"
	KeyAdminBtnInstStop     = "admin.btn.instStop"
	KeyAdminBtnInstRestart  = "admin.btn.instRestart"
	KeyAdminBtnInstSave     = "admin.btn.instSave"
	KeyAdminColRun          = "admin.col.run"
	KeyAdminPIDLabel        = "admin.label.pid"
	KeyAdminErrLabel        = "admin.label.err"
	KeyAdminEditTitle       = "admin.inst.editTitle"
	KeyAdminSaveHint        = "admin.inst.saveHint"
	KeyAdminMsgInstCreated  = "admin.msg.instCreated"
	KeyAdminMsgInstRemoved  = "admin.msg.instRemoved"
	KeyAdminMsgInstStarted  = "admin.msg.instStarted"
	// 勾选了删目录时的成功提示：要把删掉的路径说出来，删目录不可逆。
	KeyAdminMsgInstRemovedDirs = "admin.msg.instRemovedDirs"

	// 管理台：角色名
	KeyRoleAdmin = "role.admin"
	KeyRoleUser  = "role.user"

	// 管理台：审计页的动作与说明（库里存稳定英文标识，只有页面渲染时翻译）
	KeyAuditInstanceEnter    = "audit.instance.enter"
	KeyAuditInstanceDenied   = "audit.instance.denied"
	KeyAuditUserCreate       = "audit.user.create"
	KeyAuditUserPasswd       = "audit.user.passwd"
	KeyAuditUserRole         = "audit.user.role"
	KeyAuditUserToggle       = "audit.user.toggle"
	KeyAuditUserDelete       = "audit.user.delete"
	KeyAuditGrantsSave       = "audit.grants.save"
	KeyAuditInstanceCreate   = "audit.instance.create"
	KeyAuditInstanceInstall  = "audit.instance.install"
	KeyAuditInstanceRemove   = "audit.instance.remove"
	KeyAuditInstanceStart    = "audit.instance.start"
	KeyAuditInstanceStop     = "audit.instance.stop"
	KeyAuditInstanceRestart  = "audit.instance.restart"
	KeyAuditInstanceUpdate   = "audit.instance.update"
	KeyAuditEnvInstall       = "audit.env.install"
	KeyAuditCSRFRejected     = "audit.csrfRejected"
	KeyAuditDetailFailed     = "audit.detail.failed"
	KeyAuditDetailInstanceID = "audit.detail.instanceID"

	// 管理台：操作结果
	KeyAdminMsgUserCreated     = "admin.msg.userCreated"
	KeyAdminMsgPasswordChanged = "admin.msg.passwordChanged"
	KeyAdminMsgRoleChanged     = "admin.msg.roleChanged"
	KeyAdminMsgEnabled         = "admin.msg.enabled"
	KeyAdminMsgDisabled        = "admin.msg.disabled"
	KeyAdminMsgUserDeleted     = "admin.msg.userDeleted"
	KeyAdminMsgGrantsSaved     = "admin.msg.grantsSaved"

	// 管理台：错误
	KeyAdminErrForbidden    = "admin.err.forbidden"
	KeyAdminErrCSRF         = "admin.err.csrf"
	KeyAdminErrUserExists   = "admin.err.userExists"
	KeyAdminErrLastAdmin    = "admin.err.lastAdmin"
	KeyAdminErrSuperAdmin   = "admin.err.superAdmin"
	KeyAdminSuperBadge      = "admin.superBadge"
	KeyAdminErrActionFailed = "admin.err.actionFailed"
	KeyAdminErrBadRequest   = "admin.err.badRequest"

	// 管理台：用户页把「账号」（登录名，ASCII）与「姓名」（给人看）分开
	KeyAdminColAccount = "admin.col.account"
	KeyAdminColDisplay = "admin.col.display"
	KeyAdminNameHint   = "admin.name.hint"

	// CLI：管理端命令（帮助、用法、错误、结果）
	KeyCLIPortalHelp       = "cli.portal.help"
	KeyCLIPortalUserHelp   = "cli.portal.user.help"
	KeyCLIPortalInstHelp   = "cli.portal.inst.help"
	KeyCLIPortalGrantHelp  = "cli.portal.grant.help"
	KeyCLIUnknownPortalSub = "cli.err.unknownPortalSub"
	KeyCLIUnknownCommand   = "cli.err.unknownCommand"

	KeyCLIUsageUserName     = "cli.usage.userName"
	KeyCLIUsageInstanceName = "cli.usage.instanceName"
	KeyCLIUsageRole         = "cli.usage.role"
	KeyCLIUsageUserRename   = "cli.usage.userRename"

	KeyCLIErrOpenStore       = "cli.err.openStore"
	KeyCLIErrUserNotExist    = "cli.err.userNotExist"
	KeyCLIErrInstanceNotExit = "cli.err.instanceNotExist"
	KeyCLIErrLastAdmin       = "cli.err.lastAdmin"
	KeyCLIErrUserExists      = "cli.err.userExists"

	KeyCLIUserCreated      = "cli.user.created"
	KeyCLIUserPasswd       = "cli.user.passwd"
	KeyCLIUserDeleted      = "cli.user.deleted"
	KeyCLIUserRenamed      = "cli.user.renamed"
	KeyCLIUserRenamedSuper = "cli.user.renamedSuper"
	KeyCLIUserListHeader   = "cli.user.listHeader"
	KeyCLIUserListEmpty    = "cli.user.listEmpty"
	KeyCLIUserRow          = "cli.user.row"
	KeyCLIUserRoleChanged  = "cli.user.roleChanged"
	KeyCLIUserEnabled      = "cli.user.enabled"
	KeyCLIUserDisabled     = "cli.user.disabled"
	KeyCLIRandomNotice     = "cli.random.notice"

	KeyCLIInstCreated         = "cli.instance.created"
	KeyCLIInstDeleted         = "cli.instance.deleted"
	KeyCLIInstRenamed         = "cli.instance.renamed"
	KeyCLIUsageInstanceRename = "cli.instance.renameUsage"
	KeyCLIInstListHeader      = "cli.instance.listHeader"
	KeyCLIInstListEmpty       = "cli.instance.listEmpty"
	KeyCLIInstRow             = "cli.instance.row"

	KeyCLIGrantAdded   = "cli.grant.added"
	KeyCLIGrantRemoved = "cli.grant.removed"
	KeyCLIGrantHeader  = "cli.grant.header"
	KeyCLIGrantRow     = "cli.grant.row"
	KeyCLIGrantEmpty   = "cli.grant.empty"

	KeyCLIStatusEnabled  = "cli.status.enabled"
	KeyCLIStatusDisabled = "cli.status.disabled"

	// CLI：管理端进程
	KeyCLIPortalRunStarted    = "cli.portal.run.started"
	KeyCLIPortalRunNoAccounts = "cli.portal.run.noAccounts"
	KeyCLIPortalRunStopping   = "cli.portal.run.stopping"
	KeyCLIPortalRunFailed     = "cli.portal.run.failed"

	// 托盘（Windows）：通知区图标上的状态、菜单项与失败提示。
	KeyCLITrayUnsupported = "cli.tray.unsupported"
	KeyTrayTipRunning     = "tray.tip.running"
	KeyTrayTipStopped     = "tray.tip.stopped"
	KeyTrayStateRunning   = "tray.state.running"
	KeyTrayStateStopped   = "tray.state.stopped"
	KeyTrayMenuOpen       = "tray.menu.open"
	KeyTrayMenuStart      = "tray.menu.start"
	KeyTrayMenuStop       = "tray.menu.stop"
	KeyTrayMenuRestart    = "tray.menu.restart"
	KeyTrayMenuQuit       = "tray.menu.quit"
	KeyTrayOpFailed       = "tray.op.failed"

	// CLI：install（管理端安装）
	KeyCLIInstallUsage        = "cli.install.usage"
	KeyCLIInstallNeedPassword = "cli.install.needPassword"
	KeyCLIInstallCreated      = "cli.install.created"
	KeyCLIInstallExists       = "cli.install.exists"
	// 已有账号时把账号连同类型（谁是超级管理员）一并列出来，省得人以为
	// 向导里那个「管理员账号」会生效。
	KeyCLIInstallHasAccounts = "cli.install.hasAccounts"
	KeyCLIInstallExistingRow = "cli.install.existingRow"
	KeyCLIInstallStarted     = "cli.install.started"
	KeyCLIInstallNotStarted  = "cli.install.notStarted"
	KeyCLIInstallNextSteps   = "cli.install.nextSteps"
	KeyCLIInstallBadFlag     = "cli.install.badFlag"

	// 时长（错误提示里的「还有多久能再试」）
	KeyDurationSeconds        = "duration.seconds"
	KeyDurationMinutes        = "duration.minutes"
	KeyDurationMinutesSeconds = "duration.minutesSeconds"
	KeyDurationHours          = "duration.hours"
	KeyDurationHoursMinutes   = "duration.hoursMinutes"
)

// allKeys 列出全部键。新增文案时**两处**都要加：常量 + 这里。
var allKeys = []string{
	KeyLoginUsername,
	KeyLoginUsernamePlaceholder,
	KeyLoginPassword,
	KeyLoginPasswordPlaceholder,
	KeyLoginShowPassword,
	KeyLoginHidePassword,
	KeyLoginSubmit,
	KeyLoginSigningIn,
	KeyLoginHint,
	KeyLoginFooterPrefix,
	KeyLoginFeat1,
	KeyLoginFeat2,
	KeyLoginFeat3,
	KeyLoginPetIdle,
	KeyLoginPetTyping,
	KeyLoginPetPeek,
	KeyLoginPetSad,
	KeyLoginPetSubmit,
	KeyLoginErrBadRequest,
	KeyLoginErrBadCreds,
	KeyLoginErrTooMany,
	KeyLoginErrMethod,
	KeyLoginErrNotSignedIn,
	KeyHostLocal,
	KeyThemeLabel,
	KeyThemeAuto,
	KeyThemeLight,
	KeyThemeDark,
	KeySchemeLabel,
	KeySchemeSlate,
	KeySchemeOcean,
	KeySchemeViolet,
	KeySchemeForest,
	KeyStyleLabel,
	KeyStyleConsole,
	KeyStyleOverview,
	KeyStyleSplit,
	KeyPortalTitle,
	KeyPortalSignedInAs,
	KeyPortalAdmin,
	KeyPortalSignOut,
	KeyPortalEnter,
	KeyPortalNoInstances,
	KeyPortalErrNotFound,
	KeyPortalErrUnavailable,
	KeyPortalErrInternal,
	KeyAdminTitle,
	KeyAdminBack,
	KeyAdminUsers,
	KeyAdminGrants,
	KeyAdminGrantsTip,
	KeyAdminInstances,
	KeyAdminInstTip,
	KeyAdminAudit,
	KeyAdminNavLabel,
	KeyAdminNavEnv,
	KeyAdminColName,
	KeyAdminColRole,
	KeyAdminColStatus,
	KeyAdminColInstances,
	KeyAdminColPort,
	KeyAdminColTime,
	KeyAdminColActor,
	KeyAdminColAction,
	KeyAdminColTarget,
	KeyAdminColDetail,
	KeyAdminBtnApply,
	KeyAdminBtnPassword,
	KeyAdminBtnEnable,
	KeyAdminBtnDisable,
	KeyAdminBtnDelete,
	KeyAdminBtnCreateUser,
	KeyAdminBtnSaveGrants,
	KeyAdminPhPassword,
	KeyAdminPhNewPassword,
	KeyAdminStatusEnabled,
	KeyAdminStatusDisabled,
	KeyAdminNoUsers,
	KeyAdminNoInstances,
	KeyAdminNoAudit,
	KeyAdminInstCreate,
	KeyAdminInstCreateHint,
	KeyAdminInstName,
	KeyAdminInstHome,
	KeyAdminInstHomeHint,
	KeyAdminInstHomeHelp,
	KeyAdminInstWorkspace,
	KeyAdminInstWorkspacePh,
	KeyAdminInstWorkspaceHelp,
	KeyAdminInstConfigDir,
	KeyAdminInstConfigPh,
	KeyAdminInstConfigHelp,
	KeyAdminInstInstallDirPh,
	KeyAdminInstInstallDirHelp,
	KeyAdminInstDirsTitle,
	KeyAdminInstDirsTip,
	KeyAdminInstMetaConfigOverride,
	KeyAdminInstRemoveHome,
	KeyAdminInstRemoveInstall,
	KeyAdminInstRemoveHint,
	KeyAdminInstCardInstall,
	KeyAdminInstCardRemove,
	KeyAdminInstPort,
	KeyAdminInstPortAuto,
	KeyAdminInstListenHost,
	KeyAdminInstStartMode,
	KeyAdminInstStartAuto,
	KeyAdminInstMetaSameHome,
	KeyAdminInstMetaUnset,
	KeyAdminInstMetaCmdAuto,
	KeyAdminRunRunning,
	KeyAdminRunStarting,
	KeyAdminRunStopped,
	KeyAdminRunManaged,
	KeyAdminRunAdopted,
	KeyAdminInstStartDemand,
	KeyAdminInstMode,
	KeyAdminInstModeNpx,
	KeyAdminInstModeGit,
	KeyAdminInstModePnpm,
	KeyAdminInstModeLocal,
	KeyAdminInstDir,
	KeyAdminInstVersion,
	KeyAdminInstVersionPh,
	KeyAdminInstVersionHelp,
	KeyAdminInstMetaVersionLatest,
	KeyAdminInstMetaVersionBuild,
	KeyAdminInstEnvTitle,
	KeyAdminInstEnvHint,
	KeyAdminInstEnvMissing,
	KeyAdminEnvBtnInstall,
	KeyAdminEnvBtnRetry,
	KeyAdminInstSelfNode,
	KeyAdminInstSelfPnpm,
	KeyAdminInstExisting,
	KeyAdminInstNone,
	KeyAdminInstPhase,
	KeyAdminInstStep,
	KeyAdminInstLog,
	KeyAdminInstCmd,
	KeyAdminInstCmdArgs,
	KeyAdminInstInstalled,
	KeyAdminInstNotReady,
	KeyAdminInstPhasePend,
	KeyCLIInstInstallStart,
	KeyCLIInstInstallDone,
	KeyCLIInstInstallFailed,
	KeyAdminInstPhaseRun,
	KeyAdminInstPhaseDone,
	KeyAdminInstPhaseFail,
	KeyAdminBtnInstCreate,
	KeyAdminBtnInstInstall,
	KeyAdminBtnInstRemove,
	KeyAdminBtnInstOpen,
	KeyAdminBtnInstStart,
	KeyAdminBtnInstStop,
	KeyAdminBtnInstRestart,
	KeyAdminBtnInstSave,
	KeyAdminColRun,
	KeyAdminPIDLabel,
	KeyAdminErrLabel,
	KeyAdminEditTitle,
	KeyAdminSaveHint,
	KeyAdminMsgInstCreated,
	KeyAdminMsgInstRemoved,
	KeyAdminMsgInstRemovedDirs,
	KeyAdminMsgInstStarted,
	KeyRoleAdmin,
	KeyRoleUser,
	KeyAuditInstanceEnter,
	KeyAuditInstanceDenied,
	KeyAuditUserCreate,
	KeyAuditUserPasswd,
	KeyAuditUserRole,
	KeyAuditUserToggle,
	KeyAuditUserDelete,
	KeyAuditGrantsSave,
	KeyAuditInstanceCreate,
	KeyAuditInstanceInstall,
	KeyAuditInstanceRemove,
	KeyAuditInstanceStart,
	KeyAuditInstanceStop,
	KeyAuditInstanceRestart,
	KeyAuditInstanceUpdate,
	KeyAuditEnvInstall,
	KeyAuditCSRFRejected,
	KeyAuditDetailFailed,
	KeyAuditDetailInstanceID,
	KeyAdminMsgUserCreated,
	KeyAdminMsgPasswordChanged,
	KeyAdminMsgRoleChanged,
	KeyAdminMsgEnabled,
	KeyAdminMsgDisabled,
	KeyAdminMsgUserDeleted,
	KeyAdminMsgGrantsSaved,
	KeyAdminErrForbidden,
	KeyAdminErrCSRF,
	KeyAdminErrUserExists,
	KeyAdminErrLastAdmin,
	KeyAdminErrSuperAdmin,
	KeyAdminSuperBadge,
	KeyAdminErrActionFailed,
	KeyAdminErrBadRequest,
	KeyAdminColAccount,
	KeyAdminColDisplay,
	KeyAdminNameHint,
	KeyCLIPortalHelp,
	KeyCLIPortalUserHelp,
	KeyCLIPortalInstHelp,
	KeyCLIPortalGrantHelp,
	KeyCLIUnknownPortalSub,
	KeyCLIUnknownCommand,
	KeyCLIUsageUserName,
	KeyCLIUsageInstanceName,
	KeyCLIUsageRole,
	KeyCLIUsageUserRename,
	KeyCLIErrOpenStore,
	KeyCLIErrUserNotExist,
	KeyCLIErrInstanceNotExit,
	KeyCLIErrLastAdmin,
	KeyCLIErrUserExists,
	KeyCLIUserCreated,
	KeyCLIUserPasswd,
	KeyCLIUserDeleted,
	KeyCLIUserRenamed,
	KeyCLIUserRenamedSuper,
	KeyCLIUserListHeader,
	KeyCLIUserListEmpty,
	KeyCLIUserRow,
	KeyCLIUserRoleChanged,
	KeyCLIUserEnabled,
	KeyCLIUserDisabled,
	KeyCLIRandomNotice,
	KeyCLIInstCreated,
	KeyCLIInstDeleted,
	KeyCLIInstListHeader,
	KeyCLIInstListEmpty,
	KeyCLIInstRow,
	KeyCLIInstRenamed,
	KeyCLIUsageInstanceRename,
	KeyCLIGrantAdded,
	KeyCLIGrantRemoved,
	KeyCLIGrantHeader,
	KeyCLIGrantRow,
	KeyCLIGrantEmpty,
	KeyCLIStatusEnabled,
	KeyCLIStatusDisabled,
	KeyCLIPortalRunStarted,
	KeyCLIPortalRunNoAccounts,
	KeyCLIPortalRunStopping,
	KeyCLIPortalRunFailed,
	KeyCLITrayUnsupported,
	KeyTrayTipRunning,
	KeyTrayTipStopped,
	KeyTrayStateRunning,
	KeyTrayStateStopped,
	KeyTrayMenuOpen,
	KeyTrayMenuStart,
	KeyTrayMenuStop,
	KeyTrayMenuRestart,
	KeyTrayMenuQuit,
	KeyTrayOpFailed,
	KeyCLIInstallUsage,
	KeyCLIInstallNeedPassword,
	KeyCLIInstallCreated,
	KeyCLIInstallExists,
	KeyCLIInstallHasAccounts,
	KeyCLIInstallExistingRow,
	KeyCLIInstallStarted,
	KeyCLIInstallNotStarted,
	KeyCLIInstallNextSteps,
	KeyCLIInstallBadFlag,
	KeyDurationSeconds,
	KeyDurationMinutes,
	KeyDurationMinutesSeconds,
	KeyDurationHours,
	KeyDurationHoursMinutes,
}

// LoginStrings 是登录页模板需要的结构（模板里用 {{.L.X}} 取值）。
type LoginStrings struct {
	Username            string
	UsernamePlaceholder string
	Password            string
	PasswordPlaceholder string
	ShowPassword        string
	HidePassword        string
	Submit              string
	SigningIn           string
	Hint                string
	FooterPrefix        string
	// 左栏介绍的三条要点与小守卫的状态语（脚本按状态换气泡）。
	Feat1     string
	Feat2     string
	Feat3     string
	PetIdle   string
	PetTyping string
	PetPeek   string
	PetSad    string
	PetSubmit string
}

// Login 返回某语言的登录页文案。
func Login(lang Lang) LoginStrings {
	return LoginStrings{
		Username:            T(lang, KeyLoginUsername),
		UsernamePlaceholder: T(lang, KeyLoginUsernamePlaceholder),
		Password:            T(lang, KeyLoginPassword),
		PasswordPlaceholder: T(lang, KeyLoginPasswordPlaceholder),
		ShowPassword:        T(lang, KeyLoginShowPassword),
		HidePassword:        T(lang, KeyLoginHidePassword),
		Submit:              T(lang, KeyLoginSubmit),
		SigningIn:           T(lang, KeyLoginSigningIn),
		Hint:                T(lang, KeyLoginHint),
		FooterPrefix:        T(lang, KeyLoginFooterPrefix),
		Feat1:               T(lang, KeyLoginFeat1),
		Feat2:               T(lang, KeyLoginFeat2),
		Feat3:               T(lang, KeyLoginFeat3),
		PetIdle:             T(lang, KeyLoginPetIdle),
		PetTyping:           T(lang, KeyLoginPetTyping),
		PetPeek:             T(lang, KeyLoginPetPeek),
		PetSad:              T(lang, KeyLoginPetSad),
		PetSubmit:           T(lang, KeyLoginPetSubmit),
	}
}

// PortalStrings 是管理端首页模板需要的文案。
type PortalStrings struct {
	Title        string
	SignedInAs   string
	Admin        string
	SignOut      string
	Enter        string
	NoInstances  string
	FooterPrefix string
}

// Portal 返回某语言的管理端首页文案。
func Portal(lang Lang) PortalStrings {
	return PortalStrings{
		Title:        T(lang, KeyPortalTitle),
		SignedInAs:   T(lang, KeyPortalSignedInAs),
		Admin:        T(lang, KeyPortalAdmin),
		SignOut:      T(lang, KeyPortalSignOut),
		Enter:        T(lang, KeyPortalEnter),
		NoInstances:  T(lang, KeyPortalNoInstances),
		FooterPrefix: T(lang, KeyLoginFooterPrefix),
	}
}
