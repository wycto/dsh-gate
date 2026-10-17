// 管理台：用户 CRUD、实例授权矩阵、审计。
//
// 为什么是「无 JS 的普通表单 + PRG」：页面可以沿用与登录页同一套严格 CSP
// （`default-src 'none'`，连内联脚本都不需要），也不存在脚本报错导致按钮点了
// 没反应的情况。代价是每次操作刷新一次页面，对管理操作完全可以接受。
//
// CSRF 防护是两层：
//   - 会话 Cookie 是 SameSite=Lax，跨站表单不会带上它；
//   - 写操作还要求表单里的 `csrf` 字段与 `dsh_gate_csrf` Cookie 相等（双提交），
//     并在请求带 Origin/Referer 时校验同源。
//
// 这里既管用户与授权，也管实例：创建实例记录、选安装方式、看安装进度。
// 安装本身由 portal.Installer（由 portalruntime 实现）在后台跑，页面只负责
// 提交参数与轮询状态——git 方式的构建要十几分钟，HTTP 请求等不起。
package portal

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/cookie"
	"github.com/wycto/dsh-gate/internal/i18n"
)

// CSRFCookie 是双提交校验用的 Cookie 名。它不是凭据，只是「这个表单来自本站页面」
// 的证明，因此不加 HttpOnly。
const CSRFCookie = "dsh_gate_csrf"

// AdminPath 是管理台页面的路径。
const AdminPath = "/admin"

// auditLimit 是管理台展示的审计条数。
const auditLimit = 50

// adminTabs 是管理台的分页（悬浮导航里的每一项）。顺序即导航顺序；env 一栏
// 只在接了 Installer 时出现（没接运行环境就没有那张检测表，点进去只会是空白）。
//
// ID 同时是 URL 里的 ?tab= 值与模板里面板 id 的一段，两处必须一致：
// panel-<ID>（模板）与 adminTabIDs（这里）由测试锁死。
var adminTabs = []string{"users", "grants", "instances", "env", "audit"}

// adminTabDefault 是默认分页：管理台最常用的操作是管用户。
const adminTabDefault = "users"

// validAdminTab 判断 tab 值是否可用。haveInstaller 决定 env 一栏是否存在；
// 请求了一个不存在的分页时回落默认，而不是渲染一张空面板。
func validAdminTab(tab string, hasInstaller bool) bool {
	for _, id := range adminTabs {
		if id == tab {
			if id == "env" && !hasInstaller {
				return false
			}
			return true
		}
	}
	return false
}

// adminTabFromRequest 取当前应显示的分页：优先 URL 的 ?tab=，无效值回落默认。
func adminTabFromRequest(r *http.Request, hasInstaller bool) string {
	tab := strings.TrimSpace(r.URL.Query().Get("tab"))
	if validAdminTab(tab, hasInstaller) {
		return tab
	}
	return adminTabDefault
}

// adminTabForAction 把一个管理动作映射回它所在的分页。PRG 回跳带上它，操作完
// 仍停在原来的分页里——否则提交一次表单就被弹回第一页，进度和结果都看不到。
//
// 动作与分页是一一对应的（表单渲染在哪个面板里，动作就属于哪一页），
// 所以从动作推导比让每个表单捎一个隐藏字段更可靠：模板少改十处，也不会有
// 漏填的字段。
func adminTabForAction(action string, hasInstaller bool) string {
	tab := adminTabDefault
	switch {
	case strings.HasPrefix(action, "user."):
		tab = "users"
	case action == "grants.save":
		tab = "grants"
	case strings.HasPrefix(action, "instance."):
		tab = "instances"
	case action == "env.install":
		tab = "env"
	}
	if validAdminTab(tab, hasInstaller) {
		return tab
	}
	return adminTabDefault
}

type adminRoleOption struct {
	Value    string
	Label    string
	Selected bool
}

// installModeOptions 生成安装方式下拉项，并把 current 那一项标成选中。
//
// 与 roleOptions 同理：必须每次调用生成新切片，否则模板按行渲染时选中态会互相覆盖。
func installModeOptions(lang i18n.Lang, current InstallMode) []adminModeOption {
	opts := make([]adminModeOption, 0, 3)
	for _, m := range []InstallMode{InstallModeNpx, InstallModeGit, InstallModePnpm} {
		opts = append(opts, adminModeOption{
			Value:    string(m),
			Label:    installModeLabel(lang, m),
			Selected: m == current,
		})
	}
	return opts
}

// currentInstallMode 取实例当前的安装方式，用于把下拉预选到它。
// 记录里存的是空或脏值时回落 npx（与实例默认值一致）。
func currentInstallMode(inst Instance) InstallMode {
	mode, err := ParseInstallMode(strings.TrimSpace(inst.InstallMode))
	if err != nil {
		return InstallModeNpx
	}
	return mode
}

// roleOptions 生成角色下拉项，并把 current 那一项标成选中。
//
// 必须是「每次调用生成一份新切片」：模板里按行渲染，共用一份切片时要么每行
// 都选中同一个角色（选中态被最后一次调用覆盖），要么谁都不选中。
func (s *Server) roleOptions(lang i18n.Lang, current Role) []adminRoleOption {
	opts := []adminRoleOption{
		{Value: string(RoleAdmin), Label: s.roleLabel(lang, RoleAdmin)},
		{Value: string(RoleUser), Label: s.roleLabel(lang, RoleUser)},
	}
	for i := range opts {
		opts[i].Selected = opts[i].Value == string(current)
	}
	return opts
}

type adminInstanceOption struct {
	ID      string
	Name    string
	Granted bool
}

type adminUserRow struct {
	Name            string
	DisplayName     string
	Role            Role
	Disabled        bool
	StatusLabel     string
	GrantCount      int
	InstanceOptions []adminInstanceOption

	// RoleOptions 是该用户自己的角色下拉项，选中态按他的实际角色定。
	// 不能所有行共用一份：共用时没有任何一项带 Selected，浏览器会显示第一项，
	// 于是每个用户看起来都是管理员——与数据库里的角色不符。
	RoleOptions []adminRoleOption
}

type adminInstanceRow struct {
	ID          string
	Name        string
	Home        string
	Port        int
	Enabled     bool
	StatusLabel string

	// ---- 安装相关（管理端接了 Installer 时才填）----
	// InstallDir 是当前安装目录（重装时预填）。
	InstallDir string
	// Installed 表示已有启动命令。
	Installed bool
	// InstalledLabel / NotReadyLabel 是上面两个状态的文案。
	InstalledLabel string
	NotReadyLabel  string

	// ModeOptions 是该实例自己的安装方式下拉项，选中态按它的 install_mode 定。
	// 与用户角色下拉同一个坑：所有行共用一份、又没人设 Selected 时，浏览器显示
	// 第一项，于是 npx / git / pnpm 三种实例在页面上看起来全是 npx。
	ModeOptions []adminModeOption
	// Href 是进入该实例的地址。管理台原先没有进入入口——只能去「我的实例」
	// 点一下，管理台这一页就没了。现在实例名本身就是入口，新窗口打开。
	Href string
	// ---- 实例设置（创建时能填的都要在列表里看得到）----
	// ConfigDir / Workspace 留空的语义由这里翻成一句人话，模板不判断空值。
	ConfigDir  string
	Workspace  string
	ListenHost string
	// StartLabel 是启动方式的文案（随管理端启动 / 点击时启动）。
	StartLabel string
	// JobPhase 是安装任务的阶段文案（没有任务时为空）。
	JobPhase string
	// JobStep 是当前步骤描述（没有任务时为空）。
	JobStep string
	// JobError 是失败原因。
	JobError string
	// JobCommand 是装好后的启动命令。
	JobCommand string
}

// adminEnvRow 是环境检测表里的一行。
type adminEnvRow struct {
	Name    string
	Found   bool
	Version string
	Path    string
	Hint    string
	// Installable 表示这一行缺失时，管理台能不能直接点一下装好（目前只有 pnpm）。
	Installable bool
	// Job 是这一行的补齐任务（没有任务时为零值），有它才有进度与结果。
	Job ToolJob
	// JobActive / JobDone / JobFailed 是模板里好用的三个判断。
	JobActive bool
	JobDone   bool
	JobFailed bool
	// JobStep 是任务当前在做什么（只在有任务时非空）。
	JobStep string
	// JobError 是任务失败原因（只在失败时非空）。
	JobError string
}

// adminModeOption 是安装方式下拉的一项。
type adminModeOption struct {
	Value    string
	Label    string
	Selected bool
}

type adminAuditRow struct {
	At     string
	Actor  string
	Action string
	Target string
	Detail string
}

// handleAdmin 处理 /admin 与 /admin/：GET 渲染，POST 执行一个动作。
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != AdminPath && r.URL.Path != AdminPath+"/" {
		http.NotFound(w, r)
		return
	}
	lang := s.lang(w, r)
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if !user.IsAdmin() {
		s.log.Warn("非管理员访问管理台", "user", user.Name, "ip", cookie.ClientIP(r))
		s.renderAdmin(w, r, lang, user, http.StatusForbidden, i18n.T(lang, i18n.KeyAdminErrForbidden), "", adminTabFromRequest(r, s.opts.Installer != nil))
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderAdmin(w, r, lang, user, http.StatusOK, "", s.flashMessage(lang, r), adminTabFromRequest(r, s.opts.Installer != nil))
	case http.MethodPost:
		s.handleAdminPost(w, r, lang, user)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, i18n.T(lang, i18n.KeyLoginErrMethod), http.StatusMethodNotAllowed)
	}
}

// adminAction 是一次管理动作的参数。表单与 JSON API 两个入口共用同一份，
// 于是「页面能做的事」与「脚本能做的事」在实现上就是同一条路径，不会各自漂移。
type adminAction struct {
	Action    string
	Name      string
	Password  string
	Role      string
	Disabled  *bool
	Instances []string

	// ---- 实例相关（instance.create / instance.install / instance.remove）----
	// InstanceID 是目标实例 id（已有实例的操作用它）。
	InstanceID string
	// DshHome / Workspace / ConfigDir 是实例的目录设置。
	DshHome   string
	Workspace string
	ConfigDir string
	// DshPort 是 dsh 监听端口，0 表示自动分配。
	DshPort int
	// ListenHost 是 dsh 监听地址（默认回环）。
	ListenHost string
	// StartMode 是启动方式（auto / ondemand）。
	StartMode string
	// InstallMode 是安装方式（npx / git / pnpm）。
	InstallMode string
	// InstallDir 是安装目录。
	InstallDir string
	// ---- 运行环境补齐（env.install）----
	// Tool 是目标工具名（目前只有 pnpm）。
	Tool string
}

// adminActionFromForm 从表单里取出动作参数。
func adminActionFromForm(r *http.Request) adminAction {
	act := adminAction{
		Action:    strings.TrimSpace(r.FormValue("action")),
		Name:      strings.TrimSpace(r.FormValue("name")),
		Password:  r.FormValue("password"),
		Role:      strings.TrimSpace(r.FormValue("role")),
		Instances: r.Form["instance"],
	}
	switch r.FormValue("disabled") {
	case "1":
		v := true
		act.Disabled = &v
	case "0":
		v := false
		act.Disabled = &v
	}
	act.InstanceID = strings.TrimSpace(r.FormValue("instance_id"))
	act.DshHome = strings.TrimSpace(r.FormValue("dsh_home"))
	act.Workspace = strings.TrimSpace(r.FormValue("workspace"))
	act.ConfigDir = strings.TrimSpace(r.FormValue("config_dir"))
	act.ListenHost = strings.TrimSpace(r.FormValue("listen_host"))
	act.StartMode = strings.TrimSpace(r.FormValue("start_mode"))
	act.InstallMode = strings.TrimSpace(r.FormValue("install_mode"))
	act.InstallDir = strings.TrimSpace(r.FormValue("install_dir"))
	act.Tool = strings.TrimSpace(r.FormValue("tool"))
	if port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("dsh_port"))); err == nil {
		act.DshPort = port
	}
	return act
}

// handleAdminPost 校验 CSRF 后执行一个管理动作；成功走 303 回跳（PRG）。
func (s *Server) handleAdminPost(w http.ResponseWriter, r *http.Request, lang i18n.Lang, actor *User) {
	if err := r.ParseForm(); err != nil {
		s.renderAdmin(w, r, lang, actor, http.StatusBadRequest, i18n.T(lang, i18n.KeyAdminErrBadRequest), "", adminTabDefault)
		return
	}
	if !s.csrfOK(r) {
		s.log.Warn("管理台写操作 CSRF 校验失败", "user", actor.Name, "ip", cookie.ClientIP(r))
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: actor.Name, Action: "admin.csrf-rejected", Target: r.FormValue("action"),
			IP: cookie.ClientIP(r), OK: false,
		})
		s.renderAdmin(w, r, lang, actor, http.StatusForbidden, i18n.T(lang, i18n.KeyAdminErrCSRF), "", adminTabDefault)
		return
	}

	act := adminActionFromForm(r)
	if err := s.runAdminAction(actor, act, cookie.ClientIP(r)); err != nil {
		s.renderAdmin(w, r, lang, actor, http.StatusBadRequest,
			s.adminErrorText(lang, act.Action, act.Name, err), "", adminTabForAction(act.Action, s.opts.Installer != nil))
		return
	}

	// PRG：回跳时带上动作码与所在分页，由 GET 侧翻成人类可读的一句话
	// （避免刷新重复提交），并把用户送回提交前那一页。
	tab := adminTabForAction(act.Action, s.opts.Installer != nil)
	target := AdminPath + "?tab=" + url.QueryEscape(tab) +
		"&ok=" + url.QueryEscape(act.Action) + "&arg=" + url.QueryEscape(act.Name)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// runAdminAction 执行动作并写审计（成功与失败都写）。表单与 JSON API 共用。
func (s *Server) runAdminAction(actor *User, act adminAction, ip string) error {
	err := s.applyAdminAction(actor, act)
	entry := AuditEntry{
		Actor:  actor.Name,
		Action: "admin." + act.Action,
		Target: act.Name,
		IP:     ip,
		OK:     err == nil,
	}
	if err != nil {
		entry.Detail = err.Error()
	}
	_ = s.opts.Store.Audit(entry)
	return err
}

// applyAdminAction 执行一个管理动作。参数与错误都来自同一份 adminAction，
// 因此页面与 JSON API 的行为必然一致。
func (s *Server) applyAdminAction(actor *User, act adminAction) error {
	name := act.Name
	switch act.Action {
	case "user.create":
		if name == "" || act.Password == "" {
			return errors.New("missing name or password")
		}
		_, err := s.opts.Store.CreateUser(name, act.Password, Role(act.Role))
		return err

	case "user.passwd":
		if name == "" || act.Password == "" {
			return errors.New("missing name or password")
		}
		if err := s.opts.Store.SetPassword(name, act.Password); err != nil {
			return err
		}
		// 改密后把该用户已有的会话全部作废：否则旧会话还能继续用。
		s.opts.Gate.Sessions().DestroySubject(name)
		return nil

	case "user.role":
		if name == "" {
			return errors.New("missing name")
		}
		return s.opts.Store.SetRole(name, Role(act.Role))

	case "user.toggle":
		if name == "" {
			return errors.New("missing name")
		}
		disabled := act.Disabled != nil && *act.Disabled
		if err := s.opts.Store.SetDisabled(name, disabled); err != nil {
			return err
		}
		if disabled {
			s.opts.Gate.Sessions().DestroySubject(name)
		}
		return nil

	case "user.delete":
		if name == "" {
			return errors.New("missing name")
		}
		if err := s.opts.Store.DeleteUser(name); err != nil {
			return err
		}
		s.opts.Gate.Sessions().DestroySubject(name)
		return nil

	case "grants.save":
		if name == "" {
			return errors.New("missing name")
		}
		return s.opts.Store.SetUserGrants(name, act.Instances, actor.Name)

	case "instance.create":
		return s.applyInstanceCreate(actor, act)

	case "instance.install":
		return s.applyInstanceInstall(actor, act)

	case "env.install":
		return s.applyToolInstall(actor, act)

	case "instance.remove":
		if act.InstanceID == "" {
			return errors.New("missing instance_id")
		}
		// 删实例只解除管理关系，不动它的数据目录：会话与工作区是用户的数据。
		return s.opts.Store.DeleteInstance(act.InstanceID)

	default:
		return fmt.Errorf("未知动作 %q", act.Action)
	}
}

// applyInstanceCreate 新建一条实例记录。
//
// 顺序按用户的要求：先落记录（拿到 id 与端口），再按需触发安装。分开是必要的——
// 安装要十几分钟，记录必须立刻可见，否则用户刷新页面就找不到自己刚建的实例。
func (s *Server) applyInstanceCreate(actor *User, act adminAction) error {
	name := strings.TrimSpace(act.Name)
	if name == "" {
		return errors.New("missing name")
	}
	if act.DshHome == "" {
		return errors.New("missing dsh_home")
	}
	mode, err := ParseInstallMode(act.InstallMode)
	if err != nil {
		return err
	}
	startMode := strings.TrimSpace(act.StartMode)
	if startMode == "" {
		startMode = StartModeAuto
	}
	if startMode != StartModeAuto && startMode != StartModeOnDemand {
		return fmt.Errorf("不认识的启动方式 %q（可选：auto、ondemand）", startMode)
	}
	host := strings.TrimSpace(act.ListenHost)
	if host == "" {
		host = "127.0.0.1"
	}

	inst := &Instance{
		Name:        name,
		DshHome:     act.DshHome,
		Workspace:   act.Workspace,
		ConfigDir:   act.ConfigDir,
		DshPort:     act.DshPort,
		ListenHost:  host,
		StartMode:   startMode,
		InstallMode: string(mode),
		InstallDir:  act.InstallDir,
	}
	if owner, err := s.opts.Store.UserByName(actor.Name); err == nil {
		id := owner.ID
		inst.OwnerID = &id
	}
	if err := s.opts.Store.CreateInstance(inst); err != nil {
		return err
	}
	// 建记录时把创建者也授权给它：管理员建实例多半是要自己用的，还要再手工
	// 勾一次授权反而奇怪。失败不回滚——实例已经建好了，授权可以在下面补。
	if err := s.opts.Store.Grant(actor.Name, inst.ID, actor.Name); err != nil {
		s.log.Warn("创建实例后自动授权失败", "instance", inst.ID, "err", err)
	}

	// 安装目录留空时按安装方式推导一个默认值，省得用户每次都要想。
	if strings.TrimSpace(act.InstallDir) == "" && mode != InstallModeNpx {
		dir := filepath.Join(inst.DshHome, "install")
		inst.InstallDir = dir
		if err := s.opts.Store.UpdateInstance(*inst); err != nil {
			s.log.Warn("回写实例安装目录失败", "instance", inst.ID, "err", err)
		}
	}

	// 有安装能力就直接把安装排上（npx 方式也走一遍：它要冒烟验证）。
	// 没接安装能力（单实例模式）时跳过，实例记录照样建好。
	if s.opts.Installer != nil {
		if _, err := s.opts.Installer.StartInstall(context.Background(), *inst, InstallRequest{
			InstanceID: inst.ID,
			Mode:       mode,
			Dir:        inst.InstallDir,
		}); err != nil {
			// 安装没排上不算创建失败：记录已经建好，用户可以在页面上重试安装。
			s.log.Warn("实例已创建但安装没能启动", "instance", inst.ID, "err", err)
		}
	}
	return nil
}

// applyInstanceInstall 对已有实例触发一次安装（页面上「安装/重装」按钮）。
func (s *Server) applyInstanceInstall(actor *User, act adminAction) error {
	if act.InstanceID == "" {
		return errors.New("missing instance_id")
	}
	inst, err := s.opts.Store.InstanceByID(act.InstanceID)
	if err != nil {
		return err
	}
	mode, err := ParseInstallMode(act.InstallMode)
	if err != nil {
		return err
	}
	if s.opts.Installer == nil {
		return ErrInstallerUnavailable
	}
	dir := strings.TrimSpace(act.InstallDir)
	if dir == "" {
		dir = inst.InstallDir
	}
	_, err = s.opts.Installer.StartInstall(context.Background(), *inst, InstallRequest{
		InstanceID: inst.ID,
		Mode:       mode,
		Dir:        dir,
	})
	return err
}

// applyToolInstall 补齐一个缺失的运行环境工具（目前只有 pnpm）。
//
// 起点是「环境检测说 pnpm 缺失」：dsh 的插件安装器走 pnpm，而 Node 只自带 npm/npx，
// 干净的服务器上点「添加插件」会报 `spawn pnpm ENOENT`。以前这里只给一句提示，
// 用户得自己登机器敲命令；现在直接在页面点一下。
//
// 装到网关状态目录（`<状态目录>/pnpm`），不碰系统、不需要 root，卸载时随
// `uninstall --purge` 一起消失。与 dsh 安装一样是**后台任务**：页面刷新看进度。
func (s *Server) applyToolInstall(actor *User, act adminAction) error {
	if act.Tool == "" {
		return errors.New("missing tool")
	}
	if s.opts.Installer == nil {
		return ErrInstallerUnavailable
	}
	if !ToolInstallable(act.Tool) {
		return fmt.Errorf("不支持由管理端安装的工具 %q", act.Tool)
	}
	// 用 Background 而不是请求的 context：安装跑在后台 goroutine 里，请求一返回
	// 就被取消的话，安装会在第一个 yield 点停掉。
	_, err := s.opts.Installer.StartToolInstall(context.Background(), act.Tool)
	return err
}

// adminErrorText 把存储层的错误翻成用户看得懂的一句话。
//
// 特意对「最后一个管理员」「用户已存在」这类高频错误给出准确文案；其余原样带出，
// 因为那通常是校验细节（例如口令太短），原文比笼统的「操作失败」有用。
func (s *Server) adminErrorText(lang i18n.Lang, action, name string, err error) string {
	switch {
	case errors.Is(err, ErrLastAdmin):
		return i18n.T(lang, i18n.KeyAdminErrLastAdmin)
	case errors.Is(err, ErrUserExists):
		return i18n.T(lang, i18n.KeyAdminErrUserExists, name)
	}
	return i18n.T(lang, i18n.KeyAdminErrActionFailed, err.Error())
}

// flashMessage 把 PRG 回跳带的动作码翻成一句话。
func (s *Server) flashMessage(lang i18n.Lang, r *http.Request) string {
	code := r.URL.Query().Get("ok")
	if code == "" {
		return ""
	}
	arg := r.URL.Query().Get("arg")
	switch code {
	case "user.create":
		return i18n.T(lang, i18n.KeyAdminMsgUserCreated, arg)
	case "user.passwd":
		return i18n.T(lang, i18n.KeyAdminMsgPasswordChanged, arg)
	case "user.role":
		// 角色名单独翻，避免把 admin/user 直接甩给用户。
		current, err := s.opts.Store.UserByName(arg)
		if err != nil {
			return ""
		}
		return i18n.T(lang, i18n.KeyAdminMsgRoleChanged, arg, s.roleLabel(lang, current.Role))
	case "user.toggle":
		u, err := s.opts.Store.UserByName(arg)
		if err != nil {
			return ""
		}
		if u.Disabled {
			return i18n.T(lang, i18n.KeyAdminMsgDisabled, arg)
		}
		return i18n.T(lang, i18n.KeyAdminMsgEnabled, arg)
	case "user.delete":
		return i18n.T(lang, i18n.KeyAdminMsgUserDeleted, arg)
	case "grants.save":
		return i18n.T(lang, i18n.KeyAdminMsgGrantsSaved, arg)
	}
	return ""
}

// renderAdmin 渲染管理台。tab 是当前显示的分页（悬浮导航高亮项）。
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, lang i18n.Lang, actor *User, status int, problem, message, tab string) {
	users, err := s.opts.Store.ListUsers()
	if err != nil {
		s.log.Error("管理台读取用户失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	instances, err := s.opts.Store.ListInstances()
	if err != nil {
		s.log.Error("管理台读取实例失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	audit, err := s.opts.Store.RecentAudit(auditLimit)
	if err != nil {
		s.log.Error("管理台读取审计失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}

	instanceOptions := make([]adminInstanceOption, 0, len(instances))
	for _, inst := range instances {
		instanceOptions = append(instanceOptions, adminInstanceOption{ID: inst.ID, Name: inst.Name})
	}

	userRows := make([]adminUserRow, 0, len(users))
	for _, u := range users {
		granted := map[string]bool{}
		visible, err := s.opts.Store.InstancesForUser(u.ID)
		if err != nil {
			s.log.Error("管理台读取授权失败", "user", u.Name, "err", err)
			http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
			return
		}
		for _, inst := range visible {
			granted[inst.ID] = true
		}
		options := make([]adminInstanceOption, 0, len(instanceOptions))
		for _, opt := range instanceOptions {
			options = append(options, adminInstanceOption{ID: opt.ID, Name: opt.Name, Granted: granted[opt.ID]})
		}
		statusLabel := i18n.T(lang, i18n.KeyAdminStatusEnabled)
		if u.Disabled {
			statusLabel = i18n.T(lang, i18n.KeyAdminStatusDisabled)
		}
		userRows = append(userRows, adminUserRow{
			Name:            u.Name,
			DisplayName:     u.DisplayName,
			Role:            u.Role,
			Disabled:        u.Disabled,
			StatusLabel:     statusLabel,
			GrantCount:      len(visible),
			InstanceOptions: options,
			RoleOptions:     s.roleOptions(lang, u.Role),
		})
	}

	// 环境检测与安装状态：只有接了 Installer（管理端形态）才有。
	hasInstaller := s.opts.Installer != nil
	envRows := []adminEnvRow{}
	var env EnvStatus
	if hasInstaller {
		env = s.opts.Installer.DetectEnv(r.Context())
		for _, t := range env.Tools {
			row := adminEnvRow{
				Name: t.Name, Found: t.Found, Version: t.Version,
				Path: t.Path, Hint: t.Hint,
				Installable: ToolInstallable(t.Name),
			}
			// 补齐任务：装在跑就显示进度，装完就显示结果。缺了才有意义——
			// 已经找到工具时按钮本来就不显示。
			if job, ok := s.opts.Installer.ToolInstallStatus(r.Context(), t.Name); ok {
				row.Job = job
				row.JobActive = job.Active()
				row.JobDone = job.Phase == InstallPhaseDone
				row.JobFailed = job.Phase == InstallPhaseFailed
				row.JobStep = job.Step
				row.JobError = job.Error
			}
			envRows = append(envRows, row)
		}
	}

	// 新建实例表单的安装方式下拉：默认 npx（最省事）。
	modeOptions := installModeOptions(lang, InstallModeNpx)

	instanceRows := make([]adminInstanceRow, 0, len(instances))
	for _, inst := range instances {
		label := i18n.T(lang, i18n.KeyAdminStatusEnabled)
		if !inst.Enabled {
			label = i18n.T(lang, i18n.KeyAdminStatusDisabled)
		}
		row := adminInstanceRow{
			ID: inst.ID, Name: inst.Name, Home: inst.DshHome, Port: inst.DshPort,
			Enabled: inst.Enabled, StatusLabel: label,
			InstallDir:     inst.InstallDir,
			Installed:      strings.TrimSpace(inst.DshCmd) != "",
			InstalledLabel: i18n.T(lang, i18n.KeyAdminInstInstalled),
			NotReadyLabel:  i18n.T(lang, i18n.KeyAdminInstNotReady),
			ModeOptions:    installModeOptions(lang, currentInstallMode(inst)),
			Href:           MountPrefix + inst.ID + "/",
			// 创建时能填的字段，建完都得能在列表里看到——否则用户改完配置就再也
			// 确认不了自己填的是哪几个目录。留空的两个给一句「等同于什么」，
			// 而不是留一片空白让人猜。
			ConfigDir:  orDefault(strings.TrimSpace(inst.ConfigDir), i18n.T(lang, i18n.KeyAdminInstMetaSameHome)),
			Workspace:  orDefault(strings.TrimSpace(inst.Workspace), i18n.T(lang, i18n.KeyAdminInstMetaUnset)),
			ListenHost: inst.ListenHost,
			StartLabel: i18n.T(lang, i18n.KeyAdminInstStartAuto),
		}
		if strings.TrimSpace(inst.StartMode) == StartModeOnDemand {
			row.StartLabel = i18n.T(lang, i18n.KeyAdminInstStartDemand)
		}
		if hasInstaller {
			if job, ok := s.opts.Installer.InstallStatus(r.Context(), inst); ok {
				row.JobPhase = installPhaseLabel(lang, job.Phase)
				row.JobStep = job.Step
				row.JobError = job.Error
				row.JobCommand = job.Command
			}
		}
		instanceRows = append(instanceRows, row)
	}

	auditRows := make([]adminAuditRow, 0, len(audit))
	for _, e := range audit {
		auditRows = append(auditRows, adminAuditRow{
			At: e.At.Local().Format("2006-01-02 15:04:05"), Actor: e.Actor,
			Action: e.Action, Target: e.Target, Detail: e.Detail,
		})
	}

	// 新建用户的角色下拉：默认选中「普通用户」。新建账号默认给管理员是过权，
	// 要提权得再点一次「应用」，多一步但方向是对的。
	roles := s.roleOptions(lang, RoleUser)

	data := struct {
		Lang           string
		Title          string
		SignedInAs     string
		BackToPortal   string
		OtherLang      string
		OtherLangHref  string
		SignOut        string
		SignOutHref    string
		Error          string
		Message        string
		CSRF           string
		UsersTitle     string
		ColName        string
		ColRole        string
		ColStatus      string
		ColInstances   string
		ColPort        string
		ColTime        string
		ColActor       string
		ColAction      string
		ColTarget      string
		ColDetail      string
		BtnApply       string
		BtnPassword    string
		BtnEnable      string
		BtnDisable     string
		BtnDelete      string
		BtnCreateUser  string
		BtnSaveGrants  string
		PhPassword     string
		PhNewPassword  string
		NoUsers        string
		NoInstances    string
		NoAudit        string
		GrantsTitle    string
		GrantsHint     string
		InstancesTitle string
		InstancesHint  string
		AuditTitle     string

		// ---- 分页导航 ----
		ActiveTab string
		NavLabel  string
		NavEnv    string

		// ---- 实例安装面板 ----
		HasInstaller     bool
		AdminPath        string
		EnvTitle         string
		EnvHint          string
		EnvMissing       string
		EnvInstallFailed string
		EnvBtnInstall    string
		EnvBtnRetry      string
		InstallingLabel  string
		EnvTools         []adminEnvRow
		EnvPlatform      string
		PlatformLabel    string
		SelfNodeLabel    string
		SelfNode         string
		SelfPnpmLabel    string
		SelfPnpm         string
		ExistingLabel    string
		ExistingDsh      string
		NoneLabel        string
		CreateTitle      string
		CreateHint       string
		NameLabel        string
		HomeLabel        string
		ConfigLabel      string
		WorkspaceLabel   string
		PortLabel        string
		HostLabel        string
		StartLabel       string
		StartAuto        string
		StartDemand      string
		ModeLabel        string
		InstDirLabel     string
		InstDir          string
		Modes            []adminModeOption
		BtnInstCreate    string
		BtnInstInstall   string
		BtnInstRemove    string
		ColPhase         string
		StepLabel        string
		CmdLabel         string
		FooterPrefix     string
		Version          string
		Host             string
		Roles            []adminRoleOption
		Users            []adminUserRow
		Instances        []adminInstanceRow
		Audit            []adminAuditRow
	}{
		Lang:           string(lang),
		Title:          i18n.T(lang, i18n.KeyAdminTitle),
		SignedInAs:     i18n.T(lang, i18n.KeyPortalSignedInAs, displayNameOf(actor)),
		BackToPortal:   i18n.T(lang, i18n.KeyAdminBack),
		OtherLang:      otherLangLabel(lang),
		// 切语言是一次整页跳转，带上当前分页，免得切完语言被弹回第一页。
		OtherLangHref:  AdminPath + "?lang=" + string(otherLang(lang)) + "&tab=" + url.QueryEscape(tab),
		SignOut:        i18n.T(lang, i18n.KeyPortalSignOut),
		SignOutHref:    auth.PathLogout,
		Error:          problem,
		Message:        message,
		CSRF:           s.csrfToken(w, r),
		UsersTitle:     i18n.T(lang, i18n.KeyAdminUsers),
		ColName:        i18n.T(lang, i18n.KeyAdminColName),
		ColRole:        i18n.T(lang, i18n.KeyAdminColRole),
		ColStatus:      i18n.T(lang, i18n.KeyAdminColStatus),
		ColInstances:   i18n.T(lang, i18n.KeyAdminColInstances),
		ColPort:        i18n.T(lang, i18n.KeyAdminColPort),
		ColTime:        i18n.T(lang, i18n.KeyAdminColTime),
		ColActor:       i18n.T(lang, i18n.KeyAdminColActor),
		ColAction:      i18n.T(lang, i18n.KeyAdminColAction),
		ColTarget:      i18n.T(lang, i18n.KeyAdminColTarget),
		ColDetail:      i18n.T(lang, i18n.KeyAdminColDetail),
		BtnApply:       i18n.T(lang, i18n.KeyAdminBtnApply),
		BtnPassword:    i18n.T(lang, i18n.KeyAdminBtnPassword),
		BtnEnable:      i18n.T(lang, i18n.KeyAdminBtnEnable),
		BtnDisable:     i18n.T(lang, i18n.KeyAdminBtnDisable),
		BtnDelete:      i18n.T(lang, i18n.KeyAdminBtnDelete),
		BtnCreateUser:  i18n.T(lang, i18n.KeyAdminBtnCreateUser),
		BtnSaveGrants:  i18n.T(lang, i18n.KeyAdminBtnSaveGrants),
		PhPassword:     i18n.T(lang, i18n.KeyAdminPhPassword),
		PhNewPassword:  i18n.T(lang, i18n.KeyAdminPhNewPassword),
		NoUsers:        i18n.T(lang, i18n.KeyAdminNoUsers),
		NoInstances:    i18n.T(lang, i18n.KeyAdminNoInstances),
		NoAudit:        i18n.T(lang, i18n.KeyAdminNoAudit),
		GrantsTitle:    i18n.T(lang, i18n.KeyAdminGrants),
		GrantsHint:     i18n.T(lang, i18n.KeyAdminGrantsTip),
		InstancesTitle: i18n.T(lang, i18n.KeyAdminInstances),
		InstancesHint:  i18n.T(lang, i18n.KeyAdminInstTip),
		AuditTitle:     i18n.T(lang, i18n.KeyAdminAudit),

		// ---- 分页导航 ----
		ActiveTab:  tab,
		NavLabel:   i18n.T(lang, i18n.KeyAdminNavLabel),
		NavEnv:     i18n.T(lang, i18n.KeyAdminNavEnv),

		HasInstaller:     hasInstaller,
		AdminPath:        AdminPath,
		EnvTitle:         i18n.T(lang, i18n.KeyAdminInstEnvTitle),
		EnvHint:          i18n.T(lang, i18n.KeyAdminInstEnvHint),
		EnvMissing:       i18n.T(lang, i18n.KeyAdminInstEnvMissing),
		EnvInstallFailed: i18n.T(lang, i18n.KeyAdminInstPhaseFail),
		EnvBtnInstall:    i18n.T(lang, i18n.KeyAdminEnvBtnInstall),
		EnvBtnRetry:      i18n.T(lang, i18n.KeyAdminEnvBtnRetry),
		InstallingLabel:  i18n.T(lang, i18n.KeyAdminInstPhaseRun),
		EnvTools:         envRows,
		EnvPlatform:      env.Platform,
		PlatformLabel:    i18n.T(lang, i18n.KeyAdminColTarget) + ": ",
		SelfNodeLabel:    i18n.T(lang, i18n.KeyAdminInstSelfNode) + ": ",
		SelfNode:         env.NodeBin,
		SelfPnpmLabel:    i18n.T(lang, i18n.KeyAdminInstSelfPnpm) + ": ",
		SelfPnpm:         env.PnpmBin,
		ExistingLabel:    i18n.T(lang, i18n.KeyAdminInstExisting) + ": ",
		ExistingDsh:      env.ExistingDsh,
		NoneLabel:        i18n.T(lang, i18n.KeyAdminInstNone),
		CreateTitle:      i18n.T(lang, i18n.KeyAdminInstCreate),
		CreateHint:       i18n.T(lang, i18n.KeyAdminInstCreateHint),
		NameLabel:        i18n.T(lang, i18n.KeyAdminInstName),
		HomeLabel:        i18n.T(lang, i18n.KeyAdminInstHome),
		ConfigLabel:      i18n.T(lang, i18n.KeyAdminInstConfigDir),
		WorkspaceLabel:   i18n.T(lang, i18n.KeyAdminInstWorkspace),
		PortLabel:        i18n.T(lang, i18n.KeyAdminInstPort),
		HostLabel:        i18n.T(lang, i18n.KeyAdminInstListenHost),
		StartLabel:       i18n.T(lang, i18n.KeyAdminInstStartMode),
		StartAuto:        i18n.T(lang, i18n.KeyAdminInstStartAuto),
		StartDemand:      i18n.T(lang, i18n.KeyAdminInstStartDemand),
		ModeLabel:        i18n.T(lang, i18n.KeyAdminInstMode),
		InstDirLabel:     i18n.T(lang, i18n.KeyAdminInstDir),
		InstDir:          i18n.T(lang, i18n.KeyAdminInstDir),
		Modes:            modeOptions,
		BtnInstCreate:    i18n.T(lang, i18n.KeyAdminBtnInstCreate),
		BtnInstInstall:   i18n.T(lang, i18n.KeyAdminBtnInstInstall),
		BtnInstRemove:    i18n.T(lang, i18n.KeyAdminBtnInstRemove),
		ColPhase:         i18n.T(lang, i18n.KeyAdminInstPhase),
		StepLabel:        i18n.T(lang, i18n.KeyAdminInstStep) + ": ",
		CmdLabel:         i18n.T(lang, i18n.KeyAdminInstCmd) + ": ",
		FooterPrefix:     i18n.T(lang, i18n.KeyLoginFooterPrefix),
		Version:          s.version(),
		Host:             displayHost(r, lang),
		Roles:            roles,
		Users:            userRows,
		Instances:        instanceRows,
		Audit:            auditRows,
	}

	body := &strings.Builder{}
	if err := s.adminPage.Execute(body, data); err != nil {
		s.log.Error("渲染管理台失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body.String()))
}

// installModeLabel 是安装方式下拉里的文案。
func installModeLabel(lang i18n.Lang, mode InstallMode) string {
	switch mode {
	case InstallModeGit:
		return i18n.T(lang, i18n.KeyAdminInstModeGit)
	case InstallModePnpm:
		return i18n.T(lang, i18n.KeyAdminInstModePnpm)
	default:
		return i18n.T(lang, i18n.KeyAdminInstModeNpx)
	}
}

// installPhaseLabel 是安装阶段的状态文案。
func installPhaseLabel(lang i18n.Lang, phase InstallPhase) string {
	switch phase {
	case InstallPhasePending:
		return i18n.T(lang, i18n.KeyAdminInstPhasePend)
	case InstallPhaseRunning:
		return i18n.T(lang, i18n.KeyAdminInstPhaseRun)
	case InstallPhaseDone:
		return i18n.T(lang, i18n.KeyAdminInstPhaseDone)
	case InstallPhaseFailed:
		return i18n.T(lang, i18n.KeyAdminInstPhaseFail)
	}
	return ""
}

// orDefault 在值为空时给一个说明性的替代文案。
//
// 用在「留空有含义」的字段上：实例的配置目录与工作目录留空都不是错误，而是
// 「用默认」。列表里直接留白没人看得懂，所以翻成一句话再说清楚。
func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (s *Server) roleLabel(lang i18n.Lang, role Role) string {
	if role == RoleAdmin {
		return i18n.T(lang, i18n.KeyRoleAdmin)
	}
	return i18n.T(lang, i18n.KeyRoleUser)
}

// csrfToken 返回本次表单该带的 CSRF 令牌：优先读 Cookie，没有就新发一个。
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(CSRFCookie); err == nil && len(c.Value) >= 16 {
		return c.Value
	}
	token := newCSRFToken()
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookie,
		Value:    token,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
	return token
}

// csrfOK 校验双提交令牌 + 同源。
func (s *Server) csrfOK(r *http.Request) bool {
	c, err := r.Cookie(CSRFCookie)
	if err != nil || c.Value == "" {
		return false
	}
	form := r.FormValue("csrf")
	if form == "" || len(form) != len(c.Value) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(form), []byte(c.Value)) != 1 {
		return false
	}
	return sameOrigin(r)
}

// sameOrigin 在请求带 Origin/Referer 时要求它与 Host 同源。
// 两者都没有时不拒绝：令牌校验已经构成 CSRF 防护，这里只是纵深。
func sameOrigin(r *http.Request) bool {
	host := r.Host
	check := func(raw string) (bool, bool) {
		if raw == "" {
			return true, false
		}
		u, err := url.Parse(raw)
		if err != nil {
			return false, true
		}
		return strings.EqualFold(u.Host, host), true
	}
	if ok, present := check(r.Header.Get("Origin")); present && !ok {
		return false
	}
	if ok, present := check(r.Header.Get("Referer")); present && !ok {
		return false
	}
	return true
}

func newCSRFToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// 拿不到随机数就退回一个固定串：CSRF 防护会弱化，但不能因此拒绝服务。
		return "dsh-gate-csrf-unavailable"
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// compile-time 断言：模板与日志字段用到的东西都真实存在。
var _ = template.HTMLEscapeString
var _ = slog.LevelInfo
