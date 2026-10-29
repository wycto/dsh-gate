// 管理台：用户 CRUD、实例授权矩阵、审计。
//
// 写操作是「无 JS 的普通表单 + PRG」：页面沿用与登录页同一套严格 CSP，不存在
// 脚本报错导致按钮点了没反应的情况。**读**这一侧只有一件事交给脚本——页面上的
// 状态自动刷新（安装进度、运行状态、环境检测、审计）：安装 git 方式要十几分钟，
// 逼用户每隔一会儿手动刷新才能看到进度是不合理的。那个脚本是唯一被 CSP 按
// sha256 放行的内联脚本，只轮询同源的 /_portal/api/admin/live 并改文本、
// class、hidden，绝不整页重载（否则正在填的表单与展开的面板会被刷没）。
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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/cookie"
	// 只取版本名的校验：钉住哪个 dsh 版本是安装引擎（dshproc）的语义，管理台
	// 不该自己抄一份正则，否则两边的接受字符集会各自漂移——而这份校验挡的是
	// 「版本名里塞参数」这种注入。
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/i18n"
)

// CSRFCookie 是双提交校验用的 Cookie 名。它不是凭据，只是「这个表单来自本站页面」
// 的证明，因此不加 HttpOnly。
const CSRFCookie = "dsh_gate_csrf"

// AdminPath 是管理台页面的路径。
const AdminPath = "/admin"

// auditLimit 是管理台展示的审计条数。
const auditLimit = 50

// adminTabs 是管理台的分页（悬浮导航里的每一项）。顺序即导航顺序：实例排第一，
// 因为管理台最常做的事是看/管实例；默认分页也跟着是它（见 adminTabDefault）。
// env 一栏只在接了 Installer 时出现（没接运行环境就没有那张检测表，点进去只会是空白）。
//
// ID 同时是 URL 里的 ?tab= 值与模板里面板 id 的一段，两处必须一致：
// panel-<ID>（模板）与 adminTabs（这里）由测试锁死。
var adminTabs = []string{"instances", "users", "grants", "env", "audit"}

// adminTabDefault 是默认分页：管理台最常用的操作是管实例，打开就该停在实例列表。
const adminTabDefault = "instances"

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
	opts := make([]adminModeOption, 0, 4)
	for _, m := range []InstallMode{InstallModeNpx, InstallModeGit, InstallModeLocal, InstallModePnpm} {
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
	// SuperAdmin 标记第一个注册的账号（超级管理员）：模板据此收掉改角色、
	// 停用、删除三颗按钮，只留改口令。判定见 Store.SuperAdminID。
	SuperAdmin bool
	// RoleLabel 是实际角色的中文名（超管行不给下拉，用纯文本显示角色）。
	RoleLabel string
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
	// 第一项，于是 npx / git / local / pnpm 几种实例在页面上看起来全是 npx。
	ModeOptions []adminModeOption
	// InstallModeLabel 是安装方式的纯文本，列表常驻显示用（下拉在折叠的编辑表单里）。
	InstallModeLabel string
	// DshVersionLabel 是「装哪个版本」的纯文本：钉住时显示那个版本，留空时说明
	// 跟随 latest。跟着安装方式走是因为源码方式不认这个字段（git 认分支/标签，
	// local 认那个目录），在钉住 + 源码的组合上要写清楚，否则用户以为自己装的
	// 就是钉住的那个版本。
	DshVersionLabel string
	// Command 是拿去拉起 dsh 的启动命令；留空表示运行时自动探测，翻成人话而不是留白。
	Command string
	// CmdArgs 是运行时在 Command 后面自动追加的参数（实际监听地址与端口在这里）。
	CmdArgs string
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
	// JobPhase 是安装任务的阶段徽标（没有任务时为空）。
	JobPhase string
	// JobNote 是那一整行进度说明（当前步骤 + 失败原因 + 启动命令），
	// 已在 Go 侧拼好：自动刷新端点返回的是同一份文案，两边不会各说一套。
	JobNote string

	// ---- 运行时（管理端接了 Lifespan 时才填）----
	// Running / Ready 决定三颗按钮的可用性：没在跑就只给「启动」，
	// 在跑就只给「停止」与「重启」——把做不到的动作藏起来，比点了报错好。
	Running bool
	Ready   bool
	// RunLabel 是运行时状态文案（运行中 / 已停止 / 启动中）。
	RunLabel string
	// RunSource 是正在跑的这个 dsh 的来源（网关拉起 / 接管的外部实例）；没在跑时为空。
	// 接管来的进程用的不是本实例装好的命令，这一格把「装的一套」和「跑的一套」对齐。
	RunSource string
	// RunPID 是 dsh 进程号（0 表示未知，不显示）。
	RunPID int
	// RunError 是最近一次启动失败的原因。
	RunError string
	// Raw 是表单回填用的原始值。ConfigDir / Workspace 在展示时被翻成了
	// 「同 DSH_HOME」这类人话，直接塞回编辑表单会把那句话当地的路径存下去，
	// 所以编辑要用未翻译的原值。
	RawConfigDir   string
	RawWorkspace   string
	RawInstallDir  string
	RawInstallMode string
	RawDshVersion  string
	RawStartMode   string
}

// adminInstanceLive 是一张实例卡片上会随时间变的那几格。
//
// 单独抽出来是为了让「页面刚渲染的」与「自动刷新拿到的」走同一个函数：
// 两边各写一份判断，迟早会出现页面说运行中、三秒后刷新成已停止这种自相矛盾。
type adminInstanceLive struct {
	Running   bool
	Ready     bool
	Installed bool
	PID       int

	RunLabel       string
	RunSource      string
	InstalledLabel string
	NotReadyLabel  string
	JobPhase       string
	JobNote        string
	RunError       string
	Command        string
}

// instanceLiveState 计算一个实例此刻的运行 / 安装状态。
//
// 两个可选能力各查各的：没接 Lifespan 就没有运行状态，没接 Installer 就没有
// 安装任务；两者都为 nil 时只剩「装了没有」这一格（看记录里有没有启动命令）。
func (s *Server) instanceLiveState(ctx context.Context, lang i18n.Lang, inst Instance) adminInstanceLive {
	out := adminInstanceLive{
		Installed:      strings.TrimSpace(inst.DshCmd) != "",
		InstalledLabel: i18n.T(lang, i18n.KeyAdminInstInstalled),
		NotReadyLabel:  i18n.T(lang, i18n.KeyAdminInstNotReady),
		// 启动命令留空不是漏填：它表示运行时自动探测。列表里直接留白没人看得懂。
		Command: orDefault(strings.TrimSpace(inst.DshCmd), i18n.T(lang, i18n.KeyAdminInstMetaCmdAuto)),
	}
	if s.opts.Installer != nil {
		if job, ok := s.opts.Installer.InstallStatus(ctx, inst); ok {
			out.JobPhase = installPhaseLabel(lang, job.Phase)
			out.JobNote = jobNote(lang, job)
		}
	}
	if s.opts.Lifespan != nil {
		st := s.opts.Lifespan.InstanceStatus(inst)
		out.Running = st.Running
		out.Ready = st.Ready
		out.RunError = st.Error
		out.RunLabel = runStateLabel(lang, st)
		// 监督者会保留上一次的 PID（崩溃后重启、手动停止后都还在），但卡片上
		// 那一栏问的是「现在跑着的进程是谁」。没在跑就留空，否则停掉之后还挂着
		// 一个死进程号，看起来像是没停下来。
		if st.Running || st.Ready {
			out.PID = st.PID
		}
		// 只在确实有 dsh 在 serving 时标来源。判据不能写成 !Managed：还没决定
		// 拉起还是接管时两者都是 false，刚点下「启动」的会被标成接管来的。
		switch {
		case st.Managed:
			out.RunSource = i18n.T(lang, i18n.KeyAdminRunManaged)
		case st.Ready:
			out.RunSource = i18n.T(lang, i18n.KeyAdminRunAdopted)
		}
	}
	return out
}

// commandArgs 是运行时在启动命令后面固定追加的参数。
//
// 卡片上那条「启动命令」只说明 dsh 从哪来（安装写进去的）；真正执行时
// supervisor 还会补上 web --host … --port … --no-open——「填的端口到底用没
// 用上」只能看这一行。参数取自 dshproc.WebArgs，与运行时共用一份，避免显示
// 与实际执行各写一份后悄悄漂移。
func commandArgs(inst Instance) string {
	host := strings.TrimSpace(inst.ListenHost)
	if host == "" {
		// 老记录的监听地址为空时按回环处理，与运行时 listenHostOf 一致。
		host = "127.0.0.1"
	}
	return strings.Join(dshproc.WebArgs(host, inst.DshPort), " ")
}

// jobNote 把一次安装任务翻成卡片上的一行说明。
func jobNote(lang i18n.Lang, job JobStatus) string {
	if strings.TrimSpace(job.Step) == "" && job.Error == "" && job.Command == "" {
		return ""
	}
	note := i18n.T(lang, i18n.KeyAdminInstStep) + ": " + job.Step
	if job.Error != "" {
		note += " · " + job.Error
	}
	if job.Command != "" {
		note += " · " + i18n.T(lang, i18n.KeyAdminInstCmd) + ": " + job.Command
	}
	return note
}

// instancesStatNote 是第一张统计卡下面那行小字（运行 / 启用 / 安装中）。
func instancesStatNote(lang i18n.Lang, running, enabled, installing int, hasInstaller bool) string {
	note := i18n.T(lang, i18n.KeyAdminColRun) + " " + strconv.Itoa(running) +
		" · " + i18n.T(lang, i18n.KeyAdminColStatus) + " " + strconv.Itoa(enabled)
	if hasInstaller {
		note += " · " + i18n.T(lang, i18n.KeyAdminInstPhase) + " " + strconv.Itoa(installing)
	}
	return note
}

// runningStatNote 是「运行中」那张卡下面那行小字（实例 N）。
func runningStatNote(lang i18n.Lang, total int) string {
	return i18n.T(lang, i18n.KeyAdminInstances) + " " + strconv.Itoa(total)
}

// buildEnvRows 检测环境并翻成页面行；渲染与自动刷新共用这一份口径。
func (s *Server) buildEnvRows(ctx context.Context, lang i18n.Lang) ([]adminEnvRow, EnvStatus) {
	env := s.opts.Installer.DetectEnv(ctx)
	rows := make([]adminEnvRow, 0, len(env.Tools))
	for _, t := range env.Tools {
		row := adminEnvRow{
			Name: t.Name, Found: t.Found, Version: t.Version,
			Path: t.Path, Hint: t.Hint,
			Installable: ToolInstallable(t.Name),
		}
		// 补齐任务：装在跑就显示进度，装完就显示结果。缺了才有意义——
		// 已经找到工具时按钮本来就不显示。
		if job, ok := s.opts.Installer.ToolInstallStatus(ctx, t.Name); ok {
			row.Job = job
			row.JobActive = job.Active()
			row.JobDone = job.Phase == InstallPhaseDone
			row.JobFailed = job.Phase == InstallPhaseFailed
			row.JobStep = job.Step
			row.JobError = job.Error
		}
		rows = append(rows, row)
	}
	return rows, env
}

// liveEnv 把环境行翻成自动刷新端点的 JSON（文案与徽标颜色都在这里定）。
func (s *Server) liveEnv(ctx context.Context, lang i18n.Lang) *adminLiveEnvJSON {
	if s.opts.Installer == nil {
		return nil
	}
	rows, _ := s.buildEnvRows(ctx, lang)
	out := &adminLiveEnvJSON{Tools: make([]adminLiveToolJSON, 0, len(rows))}
	for _, row := range rows {
		tool := adminLiveToolJSON{
			Name:        row.Name,
			ButtonLabel: i18n.T(lang, i18n.KeyAdminEnvBtnInstall),
		}
		switch {
		case row.Found:
			tool.StatusText, tool.StatusClass = row.Version, "on"
			tool.PathText = row.Path
		case row.JobActive:
			tool.StatusText = row.JobStep
			tool.PathText = row.Hint
		case row.JobFailed:
			tool.StatusText, tool.StatusClass = i18n.T(lang, i18n.KeyAdminInstPhaseFail), "off"
			tool.PathText = row.JobError
		default:
			tool.StatusText, tool.StatusClass = i18n.T(lang, i18n.KeyAdminInstEnvMissing), "off"
			tool.PathText = row.Hint
		}
		if row.JobFailed {
			tool.ButtonLabel = i18n.T(lang, i18n.KeyAdminEnvBtnRetry)
		}
		tool.ShowInstall = row.Installable && !row.Found && !row.JobActive
		tool.ShowDuring = row.Installable && !row.Found && row.JobActive
		out.Tools = append(out.Tools, tool)
	}
	return out
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
	// ID 是审计行号，页面只用它当「有没有新记录」的版本号（自动刷新对账用）。
	ID     int64
	At     string
	Actor  string
	Action string
	Target string
	Detail string
	// ActionLabel / DetailLabel 是当前语言下的显示文案（Action / Detail 仍是
	// 库里的原始标识，供 title 提示与排查）；翻译规则见 auditlabel.go。
	ActionLabel string
	DetailLabel string
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
	// Display 是「姓名」（给人看的中文名），仅 user.create 用；留空回退登录名。
	Display string

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
	// InstallMode 是安装方式（npx / git / local / pnpm）。
	InstallMode string
	// InstallDir 是安装目录。
	InstallDir string
	// DshVersion 是要装的 dsh 版本或 dist-tag（0.2.2-alpha.2 / next / alpha）。
	// 留空 = 跟随 latest。它同时是 instance.create / instance.update 的字段与
	// instance.install 的参数：三个入口共用同一份表单语义。
	DshVersion string
	// DeleteHome / DeleteInstall 是 instance.remove 的两个勾选框：勾了就顺带
	// 删掉对应的目录。默认都不勾——删实例的默认语义仍是「只解除管理关系」，
	// 数据是用户的，不能因为点了一下删除就没了。
	DeleteHome    bool `json:"deleteHome,omitempty"`
	DeleteInstall bool `json:"deleteInstall,omitempty"`
	// RemovedDirs 记录 instance.remove 实际删掉的目录，供 PRG 回跳提示与审计用。
	// 它是**输出**字段：不接受请求方伪造，否则审计里会出现没删过的路径。
	RemovedDirs []string `json:"-"`
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
		Display:   strings.TrimSpace(r.FormValue("display")),
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
	act.DshVersion = strings.TrimSpace(r.FormValue("dsh_version"))
	// 复选框：浏览器只在勾选时提交值，因此「出现且非空」就是勾上了。
	act.DeleteHome = strings.TrimSpace(r.FormValue("delete_home")) != ""
	act.DeleteInstall = strings.TrimSpace(r.FormValue("delete_install")) != ""
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
	if err := s.runAdminAction(actor, &act, cookie.ClientIP(r)); err != nil {
		s.renderAdmin(w, r, lang, actor, http.StatusBadRequest,
			s.adminErrorText(lang, act.Action, act.Name, err), "", adminTabForAction(act.Action, s.opts.Installer != nil))
		return
	}

	// PRG：回跳时带上动作码与所在分页，由 GET 侧翻成人类可读的一句话
	// （避免刷新重复提交），并把用户送回提交前那一页。
	tab := adminTabForAction(act.Action, s.opts.Installer != nil)
	target := AdminPath + "?tab=" + url.QueryEscape(tab) +
		"&ok=" + url.QueryEscape(act.Action) + "&arg=" + url.QueryEscape(act.Name)
	// 删了哪些目录是这次操作最要紧的信息（不可逆），带回去让提示语说清楚；
	// 没删目录时不多带参数，提示语自动退回「目录保留」那一句。
	if len(act.RemovedDirs) > 0 {
		target += "&dirs=" + url.QueryEscape(strings.Join(act.RemovedDirs, "、"))
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// runAdminAction 执行动作并写审计（成功与失败都写）。表单与 JSON API 共用。
func (s *Server) runAdminAction(actor *User, act *adminAction, ip string) error {
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
	} else if len(act.RemovedDirs) > 0 {
		// 删目录不可逆：以后回查「这台实例的数据去哪了」只能靠审计里这一行，
		// 所以成功时也写 detail，把实际删掉的路径逐条列出来。
		entry.Detail = "已删除目录：" + strings.Join(act.RemovedDirs, "；")
	}
	_ = s.opts.Store.Audit(entry)
	return err
}

// applyAdminAction 执行一个管理动作。参数与错误都来自同一份 adminAction，
// 因此页面与 JSON API 的行为必然一致。
func (s *Server) applyAdminAction(actor *User, act *adminAction) error {
	name := act.Name
	switch act.Action {
	case "user.create":
		if name == "" || act.Password == "" {
			return errors.New("missing name or password")
		}
		_, err := s.opts.Store.CreateUserWithDisplay(name, act.Password, Role(act.Role), act.Display)
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
		return s.applyInstanceRemove(act)

	case "instance.start":
		return s.applyInstanceStart(act)

	case "instance.stop":
		return s.applyInstanceStop(act)

	case "instance.restart":
		return s.applyInstanceRestart(act)

	case "instance.update":
		return s.applyInstanceUpdate(actor, act)

	default:
		return fmt.Errorf("未知动作 %q", act.Action)
	}
}

// applyInstanceCreate 新建一条实例记录。
//
// 顺序按用户的要求：先落记录（拿到 id 与端口），再按需触发安装。分开是必要的——
// 安装要十几分钟，记录必须立刻可见，否则用户刷新页面就找不到自己刚建的实例。
func (s *Server) applyInstanceCreate(actor *User, act *adminAction) error {
	name := strings.TrimSpace(act.Name)
	if name == "" {
		return errors.New("missing name")
	}
	// 实例目录留空是合法输入：用 dsh 自己的默认目录，由 applyDefaults 补上。
	// 表单上那一栏因此去掉了 required（见 admin.html），并给出默认值提示。
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
	version, err := dshproc.ValidateDshVersion(act.DshVersion)
	if err != nil {
		return err
	}
	// 钉住版本 + 源码是对做不到的组合（源码认的是分支/标签或本地那份目录，不是
	// npm 包版本）。这里在**建记录之前**就拦下：安装引擎也会拒，但那样用户看到的
	// 是一条已经失败的后台任务，而不是一句能照着改的表单报错。
	if isSourceInstallMode(mode) && version != "" {
		how := "它就是你在本地构建好的那份源码，版本由那个目录决定"
		if mode == InstallModeGit {
			how = "它装的是仓库分支/标签"
		}
		return fmt.Errorf("%s不认 dsh 版本 %q：%s。"+
			"要把版本钉到 npm 包上，请把安装方式选成 npx 或 pnpm；"+
			"这一栏留空即可，改分支请用命令行 --git-ref", mode.Label(), version, how)
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
		DshVersion:  version,
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
	// 推导的根必须是**实际生效的 DSH_HOME**：配置目录填了会顶替实例目录
	// （见 portalruntime.homeOf），默认安装目录跟着实例目录走就会落到另一个根下。
	// 本地源码方式不推导：它指向的是用户已经构建好的那份目录，猜一个路径只会在
	// 安装时报「那里没有 package.json」，不如让他自己填。
	if strings.TrimSpace(act.InstallDir) == "" && mode != InstallModeNpx && mode != InstallModeLocal {
		dir := filepath.Join(instanceEffectiveHome(*inst), "install")
		inst.InstallDir = dir
		if err := s.opts.Store.UpdateInstance(*inst); err != nil {
			s.log.Warn("回写实例安装目录失败", "instance", inst.ID, "err", err)
		}
	}

	// 有安装能力就直接把安装排上（npx 方式也走一遍：它要冒烟验证）。
	// 没接安装能力（单实例模式）时跳过，实例记录照样建好。
	// 本地源码方式且没填目录时也跳过：此时安装必然失败（没有可指向的源码），
	// 排一份注定失败的活只会让人以为哪儿坏了；用户填好目录后再点安装。
	if s.opts.Installer != nil && !(mode == InstallModeLocal && strings.TrimSpace(inst.InstallDir) == "") {
		if _, err := s.opts.Installer.StartInstall(context.Background(), *inst, InstallRequest{
			InstanceID: inst.ID,
			Mode:       mode,
			Dir:        inst.InstallDir,
			Version:    version,
		}); err != nil {
			// 安装没排上不算创建失败：记录已经建好，用户可以在页面上重试安装。
			s.log.Warn("实例已创建但安装没能启动", "instance", inst.ID, "err", err)
		}
	}
	return nil
}

// requireInstance 按 id 取实例，并给出管理台需要的统一错误。
func (s *Server) requireInstance(act *adminAction) (*Instance, error) {
	if strings.TrimSpace(act.InstanceID) == "" {
		return nil, errors.New("missing instance_id")
	}
	inst, err := s.opts.Store.InstanceByID(act.InstanceID)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

// applyInstanceRemove 删除一条实例记录，并按勾选连带删除它的目录。
//
// 顺序是刻意的：先停进程 → 再删目录 → 最后删记录。
//   - 先停：运行中的 dsh 还在往数据目录里写，先删目录会被它接着写回来，进程
//     也会在「自己的配置突然消失」里报一堆错。
//   - 目录成功删完才删记录：目录删失败（权限、被占用）时记录还在，用户能
//     看到这台实例并重试；反过来先删记录，失败的目录就成了没人认领的残留，
//     谁也不知道那是什么、还能不能删。
//
// 两个勾选框都不勾时仍是「只解除管理关系」：会话与工作区是用户的数据，
// 不该因为点了一下「删除实例」就消失。
func (s *Server) applyInstanceRemove(act *adminAction) error {
	inst, err := s.requireInstance(act)
	if err != nil {
		return err
	}
	// 审计与回跳提示都用实例名：表单里带了，但 JSON 调用方可能只给 id。
	if strings.TrimSpace(act.Name) == "" {
		act.Name = inst.Name
	}

	targets := make([]removalDir, 0, 2)
	if act.DeleteHome {
		// 数据目录取**实际生效的 DSH_HOME**：配置目录填了会顶替实例目录
		// （与运行时 homeOf 一致）。按 DshHome 删会删掉一个 dsh 根本没用过的
		// 空目录，真正装着会话与凭据的那份反而留着。
		targets = append(targets, removalDir{"实例数据目录", instanceEffectiveHome(*inst)})
	}
	if act.DeleteInstall && strings.TrimSpace(inst.InstallDir) != "" {
		// 没装过 dsh（安装目录为空）时「删安装目录」没有可删的东西，不算错：
		// 用户勾的是意图，不是要求这个目录必须存在。
		targets = append(targets, removalDir{"安装目录", strings.TrimSpace(inst.InstallDir)})
	}

	// 先整体校验再动手：两个目录里有一个是危险路径时，不能让另一个已经被删掉。
	if err := validateRemovalTargets(targets); err != nil {
		return err
	}

	// 删除实例就等于不要这个实例了：只要它在跑就先停掉，否则记录没了、dsh
	// 进程还留在后台，谁也管不到它。没接 Lifespan 时停不了，按现有能力继续——
	// 目录删除在文件系统层面仍然可行，只是要由用户自己保证没有进程在写。
	if s.opts.Lifespan != nil {
		if st := s.opts.Lifespan.InstanceStatus(*inst); st.Running {
			if err := s.opts.Lifespan.StopInstance(context.Background(), *inst); err != nil {
				return fmt.Errorf("实例正在运行，停止失败，未删除任何内容：%w", err)
			}
		}
	}

	removed := make([]string, 0, len(targets))
	for _, t := range targets {
		if err := os.RemoveAll(t.path); err != nil {
			return fmt.Errorf("删除%s %s 失败（实例记录保留，可重试）：%w", t.label, t.path, err)
		}
		removed = append(removed, t.path)
	}
	if err := s.opts.Store.DeleteInstance(inst.ID); err != nil {
		return err
	}
	act.RemovedDirs = removed
	return nil
}

// removalDir 是一个「允许删除的目录」：label 用于报错时点名是哪一个，
// path 是待校验/删除的路径。
type removalDir struct {
	label string
	path  string
}

// instanceEffectiveHome 返回实例实际生效的 DSH_HOME：配置目录非空就以它为准
// （与运行时 portalruntime.homeOf 的口径一致），否则用实例目录。
func instanceEffectiveHome(inst Instance) string {
	if cfg := strings.TrimSpace(inst.ConfigDir); cfg != "" {
		return cfg
	}
	return strings.TrimSpace(inst.DshHome)
}

// validateRemovalTargets 在真正删任何东西之前，把要删的路径全部校验一遍。
//
// 一次校验全部而不是边删边校验：两个目录里有一个危险时，另一个不该先被删掉。
func validateRemovalTargets(targets []removalDir) error {
	for _, t := range targets {
		if err := validateRemovalPath(t.path); err != nil {
			return fmt.Errorf("%s：%w（未删除任何内容）", t.label, err)
		}
	}
	return nil
}

// validateRemovalPath 检查一个目录是否可以安全删除。
//
// 删目录不可逆，宁可多拒一个也不猜：空串（没配置）、相对路径（相对进程的
// 工作目录，不同启动方式下指向不同地方）、根目录、当前用户家目录一律拒绝。
// 家目录本身要拒，但家目录下的路径（例如默认的 ~/.dsh）是正常的实例目录，放行。
func validateRemovalPath(path string) error {
	p := strings.TrimSpace(path)
	if p == "" {
		return errors.New("没有配置目录，无法删除")
	}
	if !filepath.IsAbs(p) {
		return fmt.Errorf("路径 %q 不是绝对路径", p)
	}
	clean := filepath.Clean(p)
	// 根目录：Unix 的 "/"，以及 Windows 的卷根（"C:\"）。
	volume := filepath.VolumeName(clean)
	if clean == string(filepath.Separator) || clean == volume+string(filepath.Separator) {
		return fmt.Errorf("拒绝删除根目录 %q", p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		home = strings.TrimSpace(home)
		if home != "" && clean == filepath.Clean(home) {
			return fmt.Errorf("拒绝删除当前用户家目录 %q", p)
		}
	}
	return nil
}

// applyInstanceStart 启动单个实例（管理台上的「启动」）。
//
// 用 Background 而不是请求 ctx：这里的 ctx 只用于「停/起」这一步的等待，
// 但若浏览器提前断开，请求 ctx 会被取消，正在进行的启动就会被腰斩。启停是
// 用户明确点下的动作，应当做完——超时由 Lifespan 实现自己控制。
func (s *Server) applyInstanceStart(act *adminAction) error {
	inst, err := s.requireInstance(act)
	if err != nil {
		return err
	}
	if s.opts.Lifespan == nil {
		return ErrLifespanUnavailable
	}
	return s.opts.Lifespan.StartInstance(context.Background(), *inst)
}

// applyInstanceStop 停止单个实例。
//
// 刻意不把实例标成 disabled：停只是一个临时状态，重启机器、管理端重启之后
// 它仍应按自己的 start_mode 回来。要真的不再拉起，用「停用」。
func (s *Server) applyInstanceStop(act *adminAction) error {
	inst, err := s.requireInstance(act)
	if err != nil {
		return err
	}
	if s.opts.Lifespan == nil {
		return ErrLifespanUnavailable
	}
	return s.opts.Lifespan.StopInstance(context.Background(), *inst)
}

// applyInstanceRestart 用当前库里的配置重启单个实例。
//
// 从库里重新读一遍而不是用表单回传的字段：重启的语义是「按现在生效的配置
// 重来一次」，配置改没改、改在哪一份上，都以库为准。
func (s *Server) applyInstanceRestart(act *adminAction) error {
	inst, err := s.requireInstance(act)
	if err != nil {
		return err
	}
	if s.opts.Lifespan == nil {
		return ErrLifespanUnavailable
	}
	return s.opts.Lifespan.RestartInstance(context.Background(), *inst)
}

// applyInstanceUpdate 修改已有实例的配置。
//
// 与 create 的区别只有一个：不碰 id、不碰授权、不碰安装任务。改完**不自动重启**
// ——用户要的是「先停、改配置、再重启」这条可控链路，自动重启会把「我想改完
// 一起重启」和「我只是改一下先不动」两种意图混成一种。页面上因此把「保存配置」
// 与「重启」摆成两个按钮。
//
// 端口改动要落库并让运行中的实例生效，必须走重启（监督者里的端口是构造时定的），
// 所以改端口时给一句提示由页面显示。
func (s *Server) applyInstanceUpdate(actor *User, act *adminAction) error {
	inst, err := s.requireInstance(act)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(act.Name)
	if name == "" {
		return errors.New("missing name")
	}
	if err := ValidateInstanceName(name); err != nil {
		return err
	}
	// 改名要查重：name 上有 UNIQUE 约束，撞了会返回一句莫名其妙的 SQL 错误。
	if other, err := s.opts.Store.InstanceByName(name); err == nil && other.ID != inst.ID {
		return fmt.Errorf("实例名 %q 已被占用", name)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	inst.Name = name
	inst.DshHome = strings.TrimSpace(act.DshHome)
	inst.Workspace = strings.TrimSpace(act.Workspace)
	inst.ConfigDir = strings.TrimSpace(act.ConfigDir)
	if host := strings.TrimSpace(act.ListenHost); host != "" {
		inst.ListenHost = host
	}
	if mode, err := ParseInstallMode(act.InstallMode); err == nil {
		inst.InstallMode = string(mode)
	}
	inst.InstallDir = strings.TrimSpace(act.InstallDir)
	// 「装哪个版本」与安装方式是两个独立字段：把方式改成源码构建时，钉住的版本
	// 照样留在记录里（它只是暂时用不上，改回 npx / pnpm 会重新生效）。保存配置
	// 不触发安装，所以这里不像创建那样成对校验，只在真正安装时报错。
	version, err := dshproc.ValidateDshVersion(act.DshVersion)
	if err != nil {
		return err
	}
	inst.DshVersion = version
	if mode := strings.TrimSpace(act.StartMode); mode == StartModeAuto || mode == StartModeOnDemand {
		inst.StartMode = mode
	}
	if act.DshPort > 0 {
		inst.DshPort = act.DshPort
	}
	// DshHome 留空时 UpdateInstance 的默认值逻辑会补成 dsh 默认目录。
	return s.opts.Store.UpdateInstance(*inst)
}

// applyInstanceInstall 对已有实例触发一次安装（页面上「安装/重装」按钮）。
func (s *Server) applyInstanceInstall(actor *User, act *adminAction) error {
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
	// 本地源码方式没有可推导的默认目录：它必须指向用户已经 clone + 构建好的那份
	// 源码。放在这里拦，用户得到的是表单报错；交给后台任务只会得到一条「安装失败」。
	if mode == InstallModeLocal && strings.TrimSpace(dir) == "" {
		return errors.New("本地源码方式要在「安装目录」里填 clone 下来的仓库根目录" +
			"（已执行过 pnpm install 与 pnpm run build 的那个），不能留空")
	}
	// 版本：表单填了用表单的，没填用实例记录里钉住的那个（留空 = 跟随 latest）。
	// 与安装目录同一个取法，用户在安装面板里不改这一栏不会把钉住的版本弄丢。
	version := strings.TrimSpace(act.DshVersion)
	if version == "" {
		version = inst.DshVersion
	}
	if _, err := dshproc.ValidateDshVersion(version); err != nil {
		return err
	}
	_, err = s.opts.Installer.StartInstall(context.Background(), *inst, InstallRequest{
		InstanceID: inst.ID,
		Mode:       mode,
		Dir:        dir,
		Version:    version,
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
func (s *Server) applyToolInstall(actor *User, act *adminAction) error {
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
	case errors.Is(err, ErrSuperAdmin):
		return i18n.T(lang, i18n.KeyAdminErrSuperAdmin)
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
	case "instance.remove":
		// 勾了删目录时把路径说出来：这是不可逆操作，用户得能确认删的是哪几个目录。
		if dirs := strings.TrimSpace(r.URL.Query().Get("dirs")); dirs != "" {
			return i18n.T(lang, i18n.KeyAdminMsgInstRemovedDirs, arg, dirs)
		}
		return i18n.T(lang, i18n.KeyAdminMsgInstRemoved, arg)
	}
	return ""
}

// renderAdmin 渲染管理台。tab 是当前显示的分页（悬浮导航高亮项）。
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, lang i18n.Lang, actor *User, status int, problem, message, tab string) {
	// 配色和语言一样由服务端定：?theme= / ?scheme= 会在这里写 Cookie，响应头都还没发，
	// 所以 Set-Cookie 能跟这次的页面一起到达。
	theme := s.opts.Gate.Theme(w, r)
	scheme := s.opts.Gate.Scheme(w, r)
	style := s.opts.Gate.Style(w, r)
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

	// 超级管理员的 id 在循环外查一次：它是「第一个注册的账号」这个事实，
	// 对整张表是同一个值，没有按行重查的必要。
	superID, superErr := s.opts.Store.SuperAdminID()

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
		// 库里 display_name 为空时回退登录名（见 CreateUserWithDisplay），
		// 到这里再折回来：姓名与登录名相同就不单独占一行，否则 admin 会显示成
		// 上下两行一样的字，看起来像渲染坏了。
		display := u.DisplayName
		if display == u.Name {
			display = ""
		}
		userRows = append(userRows, adminUserRow{
			Name:            u.Name,
			DisplayName:     display,
			Role:            u.Role,
			Disabled:        u.Disabled,
			StatusLabel:     statusLabel,
			GrantCount:      len(visible),
			InstanceOptions: options,
			RoleOptions:     s.roleOptions(lang, u.Role),
			SuperAdmin:      superErr == nil && u.ID == superID,
			RoleLabel:       s.roleLabel(lang, u.Role),
		})
	}

	// 环境检测与安装状态：只有接了 Installer（管理端形态）才有。
	hasInstaller := s.opts.Installer != nil
	// 启停能力单独判断：没接 Inspector 的单实例形态也可能有运行时。
	hasLifespan := s.opts.Lifespan != nil
	envRows := []adminEnvRow{}
	var env EnvStatus
	if hasInstaller {
		envRows, env = s.buildEnvRows(r.Context(), lang)
	}

	// 新建实例表单的安装方式下拉：默认 npx（最省事）。
	modeOptions := installModeOptions(lang, InstallModeNpx)

	instanceRows := make([]adminInstanceRow, 0, len(instances))
	for _, inst := range instances {
		label := i18n.T(lang, i18n.KeyAdminStatusEnabled)
		if !inst.Enabled {
			label = i18n.T(lang, i18n.KeyAdminStatusDisabled)
		}
		// 配置目录的展示要区分两种含义：留空 = 用 DSH_HOME；填了 = 顶替 DSH_HOME
		// （dsh 没有独立的配置目录，运行时 homeOf 以它为准）。只显示一串路径会让
		// 人误以为「配置放这里、数据放实例目录」，实际不是。
		configDir := strings.TrimSpace(inst.ConfigDir)
		if configDir == "" {
			configDir = i18n.T(lang, i18n.KeyAdminInstMetaSameHome)
		} else {
			configDir += i18n.T(lang, i18n.KeyAdminInstMetaConfigOverride)
		}
		live := s.instanceLiveState(r.Context(), lang, inst)
		row := adminInstanceRow{
			ID: inst.ID, Name: inst.Name, Home: inst.DshHome, Port: inst.DshPort,
			Enabled: inst.Enabled, StatusLabel: label,
			InstallDir:     inst.InstallDir,
			Installed:      live.Installed,
			InstalledLabel: live.InstalledLabel,
			NotReadyLabel:  live.NotReadyLabel,
			ModeOptions:    installModeOptions(lang, currentInstallMode(inst)),
			// 安装方式与启动命令是「这个实例的 dsh 从哪来」的两半：装到一半失败时
			// 命令还停在上一次那条，只标方式会误判，所以两条都得摆出来。
			InstallModeLabel: installModeLabel(lang, currentInstallMode(inst)),
			DshVersionLabel:  dshVersionLabel(lang, currentInstallMode(inst), inst.DshVersion),
			Command:          live.Command,
			CmdArgs:          commandArgs(inst),
			Href:             MountPrefix + inst.ID + "/",
			// 创建时能填的字段，建完都得能在列表里看到——否则用户改完配置就再也
			// 确认不了自己填的是哪几个目录。留空的两个给一句「等同于什么」，
			// 而不是留一片空白让人猜。
			ConfigDir:  configDir,
			Workspace:  orDefault(strings.TrimSpace(inst.Workspace), i18n.T(lang, i18n.KeyAdminInstMetaUnset)),
			ListenHost: inst.ListenHost,
			StartLabel: i18n.T(lang, i18n.KeyAdminInstStartAuto),
			// 编辑表单回填用原值：上面几个字段展示时可能被替换成人话。
			RawConfigDir:   strings.TrimSpace(inst.ConfigDir),
			RawWorkspace:   strings.TrimSpace(inst.Workspace),
			RawInstallDir:  strings.TrimSpace(inst.InstallDir),
			RawInstallMode: string(currentInstallMode(inst)),
			RawDshVersion:  strings.TrimSpace(inst.DshVersion),
			RawStartMode:   strings.TrimSpace(inst.StartMode),
		}
		if strings.TrimSpace(inst.StartMode) == StartModeOnDemand {
			row.StartLabel = i18n.T(lang, i18n.KeyAdminInstStartDemand)
		}
		row.JobPhase = live.JobPhase
		row.JobNote = live.JobNote
		row.Running = live.Running
		row.Ready = live.Ready
		row.RunPID = live.PID
		row.RunError = live.RunError
		row.RunLabel = live.RunLabel
		row.RunSource = live.RunSource
		instanceRows = append(instanceRows, row)
	}

	auditRows := make([]adminAuditRow, 0, len(audit))
	var auditRev int64
	for _, e := range audit {
		if e.ID > auditRev {
			auditRev = e.ID
		}
		auditRows = append(auditRows, adminAuditRow{
			ID: e.ID,
			At: e.At.Local().Format("2006-01-02 15:04:05"), Actor: e.Actor,
			Action: e.Action, Target: e.Target, Detail: e.Detail,
			ActionLabel: auditActionLabel(lang, e.Action),
			DetailLabel: auditDetailLabel(lang, e.Action, e.Detail, e.OK),
		})
	}

	// 新建用户的角色下拉：默认选中「普通用户」。新建账号默认给管理员是过权，
	// 要提权得再点一次「应用」，多一步但方向是对的。
	roles := s.roleOptions(lang, RoleUser)

	// 总览统计卡：数字全部从已经取到的行里数出来，不再多查一次库。
	// 「安装中」只有接了安装能力才有意义，没接就不摆一张恒为 0 的卡。
	var running, installing, enabled int
	for _, row := range instanceRows {
		if row.Running {
			running++
		}
		if row.Enabled {
			enabled++
		}
		if row.JobPhase != "" {
			installing++
		}
	}

	data := struct {
		Lang          string
		Title         string
		SignedInAs    string
		BackToPortal  string
		OtherLang     string
		OtherLangHref string
		SignOut       string
		SignOutHref   string
		Error         string
		Message       string
		CSRF          string
		UsersTitle    string
		ColName       string
		ColAccount    string
		ColDisplay    string
		NameHint      string
		ColRole       string
		ColStatus     string
		ColInstances  string
		ColPort       string
		ColRun        string
		ColTime       string
		ColActor      string
		ColAction     string
		ColTarget     string
		ColDetail     string
		BtnApply      string
		BtnPassword   string
		BtnEnable     string
		BtnDisable    string
		BtnDelete     string
		BtnCreateUser string
		BtnSaveGrants string
		PhPassword    string
		PhNewPassword string
		NoUsers       string
		NoInstances   string
		NoAudit       string
		// SuperBadge 是超管行名字下面的徽标文案（页面级，供模板 $. 取用）。
		SuperBadge     string
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
		HasInstaller      bool
		HasLifespan       bool
		PIDLabel          string
		ErrLabel          string
		EditTitle         string
		SaveHint          string
		AdminPath         string
		EnvTitle          string
		EnvHint           string
		EnvMissing        string
		EnvInstallFailed  string
		EnvBtnInstall     string
		EnvBtnRetry       string
		InstallingLabel   string
		EnvTools          []adminEnvRow
		EnvPlatform       string
		PlatformLabel     string
		SelfNodeLabel     string
		SelfNode          string
		SelfPnpmLabel     string
		SelfPnpm          string
		ExistingLabel     string
		ExistingDsh       string
		NoneLabel         string
		CreateTitle       string
		CreateHint        string
		NameLabel         string
		HomeLabel         string
		HomePlaceholder   string
		ConfigLabel       string
		WorkspaceLabel    string
		WorkspacePh       string
		WorkspaceHelp     string
		HomeHelp          string
		ConfigPh          string
		ConfigHelp        string
		InstallDirPh      string
		InstallDirHelp    string
		VersionLabel      string
		VersionPh         string
		VersionHelp       string
		DirsTitle         string
		DirsTip           string
		PortLabel         string
		HostLabel         string
		StartLabel        string
		StartAuto         string
		StartDemand       string
		ModeLabel         string
		InstDirLabel      string
		InstDir           string
		Modes             []adminModeOption
		BtnInstCreate     string
		BtnInstInstall    string
		BtnInstRemove     string
		BtnInstOpen       string
		InstRemoveHome    string
		InstRemoveInstall string
		InstRemoveHint    string
		InstCardInstall   string
		InstCardRemove    string
		BtnInstStart      string
		BtnInstStop       string
		BtnInstRestart    string
		BtnInstSave       string
		ColPhase          string
		StepLabel         string
		CmdLabel          string
		CmdArgsLabel      string
		FooterPrefix      string
		Version           string
		Host              string
		// 配色（与登录页 / 我的实例同一套模型：明暗三态 + 方案四选一）。
		Theme       string
		ThemeLabel  string
		Themes      []auth.ThemeOption
		Scheme      string
		SchemeLabel string
		Schemes     []auth.ThemeOption
		Style       string
		StyleLabel  string
		Styles      []auth.ThemeOption
		NavItems    []adminNavItem
		// 总览布局的统计卡：数字从已取到的行里数出来（服务端算，自动刷新脚本
		// 只负责把新数字换进同一处文字）。
		CountInstances  int
		CountRunning    int
		CountEnabled    int
		CountInstalling int
		CountUsers      int
		// 页面自动刷新：内联脚本（CSP 按它的内容放行）、轮询端点、审计版本号，
		// 以及统计卡上两行会随时间变的说明。
		LiveScript         template.JS
		LivePath           string
		AuditRev           int64
		CountInstancesNote string
		CountRunningNote   string
		Roles              []adminRoleOption
		Users              []adminUserRow
		Instances          []adminInstanceRow
		Audit              []adminAuditRow
	}{
		Lang:         string(lang),
		Title:        i18n.T(lang, i18n.KeyAdminTitle),
		SignedInAs:   i18n.T(lang, i18n.KeyPortalSignedInAs, displayNameOf(actor)),
		BackToPortal: i18n.T(lang, i18n.KeyAdminBack),
		OtherLang:    otherLangLabel(lang),
		// 切语言是一次整页跳转，带上当前分页，免得切完语言被弹回第一页。
		OtherLangHref:  AdminPath + "?lang=" + string(otherLang(lang)) + "&tab=" + url.QueryEscape(tab),
		SignOut:        i18n.T(lang, i18n.KeyPortalSignOut),
		SignOutHref:    auth.PathLogout,
		Error:          problem,
		Message:        message,
		CSRF:           s.csrfToken(w, r),
		UsersTitle:     i18n.T(lang, i18n.KeyAdminUsers),
		ColName:        i18n.T(lang, i18n.KeyAdminColName),
		ColAccount:     i18n.T(lang, i18n.KeyAdminColAccount),
		ColDisplay:     i18n.T(lang, i18n.KeyAdminColDisplay),
		NameHint:       i18n.T(lang, i18n.KeyAdminNameHint),
		ColRole:        i18n.T(lang, i18n.KeyAdminColRole),
		ColStatus:      i18n.T(lang, i18n.KeyAdminColStatus),
		ColInstances:   i18n.T(lang, i18n.KeyAdminColInstances),
		ColPort:        i18n.T(lang, i18n.KeyAdminColPort),
		ColRun:         i18n.T(lang, i18n.KeyAdminColRun),
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
		SuperBadge:     i18n.T(lang, i18n.KeyAdminSuperBadge),
		GrantsTitle:    i18n.T(lang, i18n.KeyAdminGrants),
		GrantsHint:     i18n.T(lang, i18n.KeyAdminGrantsTip),
		InstancesTitle: i18n.T(lang, i18n.KeyAdminInstances),
		InstancesHint:  i18n.T(lang, i18n.KeyAdminInstTip),
		AuditTitle:     i18n.T(lang, i18n.KeyAdminAudit),

		// ---- 分页导航 ----
		ActiveTab: tab,
		NavLabel:  i18n.T(lang, i18n.KeyAdminNavLabel),
		NavEnv:    i18n.T(lang, i18n.KeyAdminNavEnv),

		HasInstaller:       hasInstaller,
		HasLifespan:        hasLifespan,
		PIDLabel:           i18n.T(lang, i18n.KeyAdminPIDLabel),
		ErrLabel:           i18n.T(lang, i18n.KeyAdminErrLabel),
		EditTitle:          i18n.T(lang, i18n.KeyAdminEditTitle),
		SaveHint:           i18n.T(lang, i18n.KeyAdminSaveHint),
		AdminPath:          AdminPath,
		EnvTitle:           i18n.T(lang, i18n.KeyAdminInstEnvTitle),
		EnvHint:            i18n.T(lang, i18n.KeyAdminInstEnvHint),
		EnvMissing:         i18n.T(lang, i18n.KeyAdminInstEnvMissing),
		EnvInstallFailed:   i18n.T(lang, i18n.KeyAdminInstPhaseFail),
		EnvBtnInstall:      i18n.T(lang, i18n.KeyAdminEnvBtnInstall),
		EnvBtnRetry:        i18n.T(lang, i18n.KeyAdminEnvBtnRetry),
		InstallingLabel:    i18n.T(lang, i18n.KeyAdminInstPhaseRun),
		EnvTools:           envRows,
		EnvPlatform:        env.Platform,
		PlatformLabel:      i18n.T(lang, i18n.KeyAdminColTarget) + ": ",
		SelfNodeLabel:      i18n.T(lang, i18n.KeyAdminInstSelfNode) + ": ",
		SelfNode:           env.NodeBin,
		SelfPnpmLabel:      i18n.T(lang, i18n.KeyAdminInstSelfPnpm) + ": ",
		SelfPnpm:           env.PnpmBin,
		ExistingLabel:      i18n.T(lang, i18n.KeyAdminInstExisting) + ": ",
		ExistingDsh:        env.ExistingDsh,
		NoneLabel:          i18n.T(lang, i18n.KeyAdminInstNone),
		CreateTitle:        i18n.T(lang, i18n.KeyAdminInstCreate),
		CreateHint:         i18n.T(lang, i18n.KeyAdminInstCreateHint),
		NameLabel:          i18n.T(lang, i18n.KeyAdminInstName),
		HomeLabel:          i18n.T(lang, i18n.KeyAdminInstHome),
		HomePlaceholder:    i18n.T(lang, i18n.KeyAdminInstHomeHint, DefaultDshHomeName),
		HomeHelp:           i18n.T(lang, i18n.KeyAdminInstHomeHelp),
		ConfigLabel:        i18n.T(lang, i18n.KeyAdminInstConfigDir),
		ConfigPh:           i18n.T(lang, i18n.KeyAdminInstConfigPh),
		ConfigHelp:         i18n.T(lang, i18n.KeyAdminInstConfigHelp),
		WorkspaceLabel:     i18n.T(lang, i18n.KeyAdminInstWorkspace),
		WorkspacePh:        i18n.T(lang, i18n.KeyAdminInstWorkspacePh),
		WorkspaceHelp:      i18n.T(lang, i18n.KeyAdminInstWorkspaceHelp),
		InstallDirPh:       i18n.T(lang, i18n.KeyAdminInstInstallDirPh),
		InstallDirHelp:     i18n.T(lang, i18n.KeyAdminInstInstallDirHelp),
		VersionLabel:       i18n.T(lang, i18n.KeyAdminInstVersion),
		VersionPh:          i18n.T(lang, i18n.KeyAdminInstVersionPh),
		VersionHelp:        i18n.T(lang, i18n.KeyAdminInstVersionHelp),
		DirsTitle:          i18n.T(lang, i18n.KeyAdminInstDirsTitle),
		DirsTip:            i18n.T(lang, i18n.KeyAdminInstDirsTip),
		PortLabel:          i18n.T(lang, i18n.KeyAdminInstPort),
		HostLabel:          i18n.T(lang, i18n.KeyAdminInstListenHost),
		StartLabel:         i18n.T(lang, i18n.KeyAdminInstStartMode),
		StartAuto:          i18n.T(lang, i18n.KeyAdminInstStartAuto),
		StartDemand:        i18n.T(lang, i18n.KeyAdminInstStartDemand),
		ModeLabel:          i18n.T(lang, i18n.KeyAdminInstMode),
		InstDirLabel:       i18n.T(lang, i18n.KeyAdminInstDir),
		InstDir:            i18n.T(lang, i18n.KeyAdminInstDir),
		Modes:              modeOptions,
		BtnInstCreate:      i18n.T(lang, i18n.KeyAdminBtnInstCreate),
		BtnInstInstall:     i18n.T(lang, i18n.KeyAdminBtnInstInstall),
		BtnInstRemove:      i18n.T(lang, i18n.KeyAdminBtnInstRemove),
		BtnInstOpen:        i18n.T(lang, i18n.KeyAdminBtnInstOpen),
		InstRemoveHome:     i18n.T(lang, i18n.KeyAdminInstRemoveHome),
		InstRemoveInstall:  i18n.T(lang, i18n.KeyAdminInstRemoveInstall),
		InstRemoveHint:     i18n.T(lang, i18n.KeyAdminInstRemoveHint),
		InstCardInstall:    i18n.T(lang, i18n.KeyAdminInstCardInstall),
		InstCardRemove:     i18n.T(lang, i18n.KeyAdminInstCardRemove),
		BtnInstStart:       i18n.T(lang, i18n.KeyAdminBtnInstStart),
		BtnInstStop:        i18n.T(lang, i18n.KeyAdminBtnInstStop),
		BtnInstRestart:     i18n.T(lang, i18n.KeyAdminBtnInstRestart),
		BtnInstSave:        i18n.T(lang, i18n.KeyAdminBtnInstSave),
		ColPhase:           i18n.T(lang, i18n.KeyAdminInstPhase),
		StepLabel:          i18n.T(lang, i18n.KeyAdminInstStep) + ": ",
		CmdLabel:           i18n.T(lang, i18n.KeyAdminInstCmd),
		CmdArgsLabel:       i18n.T(lang, i18n.KeyAdminInstCmdArgs),
		FooterPrefix:       i18n.T(lang, i18n.KeyLoginFooterPrefix),
		Version:            s.version(),
		Host:               displayHost(r, lang),
		Theme:              string(theme),
		ThemeLabel:         i18n.T(lang, i18n.KeyThemeLabel),
		Themes:             auth.ThemeOptions(r, theme, lang),
		Scheme:             string(scheme),
		SchemeLabel:        i18n.T(lang, i18n.KeySchemeLabel),
		Schemes:            auth.SchemeOptions(r, scheme, lang),
		Style:              string(style),
		StyleLabel:         i18n.T(lang, i18n.KeyStyleLabel),
		Styles:             auth.StyleOptions(r, style, lang),
		NavItems:           navItems(lang, tab, hasInstaller),
		CountInstances:     len(instanceRows),
		CountRunning:       running,
		CountEnabled:       enabled,
		CountInstalling:    installing,
		CountUsers:         len(userRows),
		LiveScript:         s.liveScript,
		LivePath:           AdminLivePath,
		AuditRev:           auditRev,
		CountInstancesNote: instancesStatNote(lang, running, enabled, installing, hasInstaller),
		CountRunningNote:   runningStatNote(lang, len(instanceRows)),
		Roles:              roles,
		Users:              userRows,
		Instances:          instanceRows,
		Audit:              auditRows,
	}

	body := &strings.Builder{}
	if err := s.adminPage.Execute(body, data); err != nil {
		s.log.Error("渲染管理台失败", "err", err)
		http.Error(w, "TPLDEBUG: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	// 管理台比登录页多两条：按内容哈希放行那段自动刷新脚本，并允许它访问同源的
	// 实时状态接口。哈希在 NewServer 里按脚本字节算好，脚本一改就不可能漏放行。
	w.Header().Set("Content-Security-Policy", s.liveCSP)
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
	case InstallModeLocal:
		return i18n.T(lang, i18n.KeyAdminInstModeLocal)
	default:
		return i18n.T(lang, i18n.KeyAdminInstModeNpx)
	}
}

// isSourceInstallMode 表示这种安装方式跑的是源码（分支或本地目录），而不是 npm 包。
func isSourceInstallMode(mode InstallMode) bool {
	return mode == InstallModeGit || mode == InstallModeLocal
}

// dshVersionLabel 是「装哪个版本的 dsh」这一格的文案。
//
// 钉住的版本原样显示（它本来就是 npm 上的名字），留空要写成「跟随 latest」而不是
// 留白——空白看起来像漏填。源码方式那一格再补一句「不生效」：版本名是 npm 包的说法，
// 按分支或本地目录跑时它只是留在记录里等安装方式改回去，不写清楚会被当成已经装上了那个版本。
func dshVersionLabel(lang i18n.Lang, mode InstallMode, pinned string) string {
	pinned = strings.TrimSpace(pinned)
	if pinned == "" {
		return i18n.T(lang, i18n.KeyAdminInstMetaVersionLatest)
	}
	if isSourceInstallMode(mode) {
		return i18n.T(lang, i18n.KeyAdminInstMetaVersionBuild, pinned)
	}
	return pinned
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

// adminNavItem 是管理台导航的一项。
//
// 三种布局（底部 Dock / 顶栏页签 / 侧栏列表）渲染的是同一组项，所以收敛成
// 数据在 Go 侧算好；图标不再各抄一份内联 SVG，而是走 CSS 的 mask
// （--icon-<id>），模板里只有 class 与文案。
type adminNavItem struct {
	ID     string // users / grants / instances / env / audit
	Title  string
	Active bool
}

// navItems 按展示顺序给出当前能力下可见的导航项。
//
// 运行环境页只在接了安装能力时出现——没有安装就没有环境可看，摆个空页等于骗人。
func navItems(lang i18n.Lang, tab string, hasInstaller bool) []adminNavItem {
	items := []adminNavItem{
		{"instances", i18n.T(lang, i18n.KeyAdminInstances), tab == "instances"},
		{"users", i18n.T(lang, i18n.KeyAdminUsers), tab == "users"},
		{"grants", i18n.T(lang, i18n.KeyAdminGrants), tab == "grants"},
	}
	if hasInstaller {
		items = append(items, adminNavItem{"env", i18n.T(lang, i18n.KeyAdminNavEnv), tab == "env"})
	}
	items = append(items, adminNavItem{"audit", i18n.T(lang, i18n.KeyAdminAudit), tab == "audit"})
	return items
}

// runStateLabel 把运行状态翻成一句人话。
//
// 三态而不是两态：「已停止」与「启动中」必须分开，否则刚点完启动、dsh 还在
// 准备环境的那几秒里页面显示「已停止」，用户会以为点了没反应。
func runStateLabel(lang i18n.Lang, st InstanceStatus) string {
	switch {
	case st.Ready:
		return i18n.T(lang, i18n.KeyAdminRunRunning)
	case st.Running:
		return i18n.T(lang, i18n.KeyAdminRunStarting)
	default:
		return i18n.T(lang, i18n.KeyAdminRunStopped)
	}
}
