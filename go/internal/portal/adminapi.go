// 管理台的 JSON API：给脚本与自动化用，和管理页面走**同一条**动作实现。
//
// 三个端点：
//
//	GET  /_portal/api/admin/state   当前状态（用户/实例/授权矩阵/审计），不含任何哈希
//	GET  /_portal/api/admin/live    页面自动刷新用的实时状态（只含会变的格子）
//	POST /_portal/api/admin/action  执行一个管理动作，body 是 adminAction 的 JSON
//
// 安全：三者都要求管理员会话；写操作还要求 Content-Type 是 application/json 且
// 同源（Origin/Referer 与 Host 一致）。JSON 的 content-type 本身就挡住了跨站表单
// CSRF（表单只能发 urlencoded/multipart/plain），同源校验是纵深。
package portal

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/wycto/dsh-gate/internal/cookie"
	"github.com/wycto/dsh-gate/internal/i18n"
)

// AdminStatePath 是管理状态 JSON 端点。
const AdminStatePath = "/_portal/api/admin/state"

// AdminActionPath 是管理动作 JSON 端点。
const AdminActionPath = "/_portal/api/admin/action"

// AdminLivePath 是管理台自动刷新用的实时状态端点。
//
// 与 state 分开是因为两者的消费者不同：state 是给脚本与自动化的全量快照
// （含授权矩阵），live 是页面每几秒拉一次的小对象，只含「会随时间变」的格子，
// 每个字段都已经翻成当前语言、拼好给人看的文案——页面脚本因此不需要知道
// 「启动中」怎么翻、标签该用 on 还是 warn。
const AdminLivePath = "/_portal/api/admin/live"

// maxAdminBodyBytes 限制 JSON 动作体大小：真实动作只有几百字节。
const maxAdminBodyBytes = 64 << 10

// adminStateJSON 是状态端点的响应体。**不含口令哈希**。
type adminStateJSON struct {
	OK        bool                `json:"ok"`
	Users     []adminUserJSON     `json:"users"`
	Instances []adminInstanceJSON `json:"instances"`
	Audit     []adminAuditJSON    `json:"audit"`
	Grants    map[string][]string `json:"grants"`
	Counts    map[string]int      `json:"counts"`
	User      map[string]string   `json:"user"`
}

type adminUserJSON struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Role        string   `json:"role"`
	Disabled    bool     `json:"disabled"`
	Instances   []string `json:"instances"`
}

type adminInstanceJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DshHome     string `json:"dshHome"`
	ConfigDir   string `json:"configDir"`
	InstallMode string `json:"installMode"`
	InstallDir  string `json:"installDir"`
	DshVersion  string `json:"dshVersion"`
	ListenHost  string `json:"listenHost"`
	StartMode   string `json:"startMode"`
	DshCmd      string `json:"dshCmd"`
	Installed   bool   `json:"installed"`
	Port        int    `json:"port"`
	Enabled     bool   `json:"enabled"`
	URL         string `json:"url"`
}

type adminAuditJSON struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
	OK     bool   `json:"ok"`
}

// adminLiveJSON 是自动刷新端点的响应体。字段都是**显示就绪**的文案：
// 页面脚本只负责把值贴回 DOM，不做翻译与拼接。
type adminLiveJSON struct {
	OK        bool                    `json:"ok"`
	Stats     adminLiveStatsJSON      `json:"stats"`
	Instances []adminLiveInstanceJSON `json:"instances"`
	// Env 只在请求带 ?env=1 时给出：检测环境要真去 exec 一遍，不能每 3 秒跑。
	Env   *adminLiveEnvJSON  `json:"env,omitempty"`
	Audit adminLiveAuditJSON `json:"audit"`
}

// adminLiveStatsJSON 是统计卡上的数字与两行小字。
type adminLiveStatsJSON struct {
	Instances  int `json:"instances"`
	Running    int `json:"running"`
	Enabled    int `json:"enabled"`
	Installing int `json:"installing"`
	Users      int `json:"users"`
	Audit      int `json:"audit"`
	// InstancesNote / RunningNote 是第一张与第三张卡下面那行说明。
	InstancesNote string `json:"instancesNote"`
	RunningNote   string `json:"runningNote"`
}

// adminLiveInstanceJSON 是一张实例卡片上会变的那几格。
type adminLiveInstanceJSON struct {
	ID        string `json:"id"`
	Running   bool   `json:"running"`
	Ready     bool   `json:"ready"`
	Installed bool   `json:"installed"`
	PID       int    `json:"pid"`

	RunLabel       string `json:"runLabel"`
	RunSource      string `json:"runSource"`
	InstalledLabel string `json:"installedLabel"`
	NotReadyLabel  string `json:"notReadyLabel"`
	// JobPhase 是安装阶段徽标（空表示没有任务）；JobNote 是那一整行进度说明。
	JobPhase string `json:"jobPhase"`
	JobNote  string `json:"jobNote"`
	// RunError 是最近一次启动失败的原因（空表示没有）。
	RunError string `json:"runError"`
	// Command 是当前记录的启动命令（安装完成后会变），展示用的兜底文案已拼好。
	Command string `json:"command"`
}

// adminLiveEnvJSON 是运行环境页的实时状态（只在该页可见时请求）。
type adminLiveEnvJSON struct {
	Tools []adminLiveToolJSON `json:"tools"`
}

type adminLiveToolJSON struct {
	Name string `json:"name"`
	// StatusText / StatusClass 是状态徽标的文案与颜色（on / warn / off / 空）。
	StatusText  string `json:"statusText"`
	StatusClass string `json:"statusClass"`
	// PathText 是 PATH 那一格：找到时是路径，失败时是错误，缺失时是补装提示。
	PathText string `json:"pathText"`
	// ShowInstall 表示这一行此刻该显示「安装 / 重试」按钮；ShowDuring 表示
	// 该显示「安装中」。两者互斥，且都要求这个工具能由管理端补齐。
	ShowInstall bool   `json:"showInstall"`
	ShowDuring  bool   `json:"showDuring"`
	ButtonLabel string `json:"buttonLabel"`
}

// adminLiveAuditJSON 是审计页的增量快照。rev 是当前最大的审计 id：
// 页面记住它，只有变了才重建表格——不然每 3 秒重排一次会打断正在选中的文字。
type adminLiveAuditJSON struct {
	Rev  int64                `json:"rev"`
	Rows []adminAuditLiveJSON `json:"rows"`
}

type adminAuditLiveJSON struct {
	At          string `json:"at"`
	Actor       string `json:"actor"`
	Action      string `json:"action"`
	ActionLabel string `json:"actionLabel"`
	Target      string `json:"target"`
	Detail      string `json:"detail"`
	DetailLabel string `json:"detailLabel"`
}

// handleAdminJSON 是 /_portal/api/admin/* 的入口。
func (s *Server) handleAdminJSON(w http.ResponseWriter, r *http.Request) {
	lang := s.lang(w, r)
	w.Header().Set("Cache-Control", "no-store")

	actor, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if !actor.IsAdmin() {
		s.log.Warn("非管理员调用管理 API", "user", actor.Name, "ip", cookie.ClientIP(r))
		writeJSONError(w, http.StatusForbidden, i18n.T(lang, i18n.KeyAdminErrForbidden))
		return
	}

	switch r.URL.Path {
	case AdminStatePath:
		s.handleAdminState(w, r, actor)
	case AdminLivePath:
		s.handleAdminLive(w, r, lang)
	case AdminActionPath:
		s.handleAdminAction(w, r, lang, actor)
	default:
		http.NotFound(w, r)
	}
}

// handleAdminState 返回当前状态。
func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request, actor *User) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	users, err := s.opts.Store.ListUsers()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	instances, err := s.opts.Store.ListInstances()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit, err := s.opts.Store.RecentAudit(auditLimit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := adminStateJSON{
		OK:        true,
		Grants:    map[string][]string{},
		Counts:    map[string]int{"users": len(users), "instances": len(instances)},
		User:      map[string]string{"name": actor.Name, "role": string(actor.Role)},
		Users:     make([]adminUserJSON, 0, len(users)),
		Instances: make([]adminInstanceJSON, 0, len(instances)),
		Audit:     make([]adminAuditJSON, 0, len(audit)),
	}
	for _, u := range users {
		visible, err := s.opts.Store.InstancesForUser(u.ID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		ids := make([]string, 0, len(visible))
		for _, inst := range visible {
			ids = append(ids, inst.ID)
		}
		out.Grants[u.Name] = ids
		out.Users = append(out.Users, adminUserJSON{
			Name: u.Name, DisplayName: u.DisplayName, Role: string(u.Role),
			Disabled: u.Disabled, Instances: ids,
		})
	}
	for _, inst := range instances {
		out.Instances = append(out.Instances, adminInstanceJSON{
			ID: inst.ID, Name: inst.Name, DshHome: inst.DshHome, Port: inst.DshPort,
			ConfigDir: inst.ConfigDir, InstallMode: inst.InstallMode, InstallDir: inst.InstallDir,
			DshVersion: inst.DshVersion,
			ListenHost: inst.ListenHost, StartMode: inst.StartMode, DshCmd: inst.DshCmd,
			// installed 是「记录里已经有启动命令」的意思。它不代表进程此刻活着，
			// 只代表这台实例被装过、有命令可用。
			Installed: strings.TrimSpace(inst.DshCmd) != "",
			Enabled:   inst.Enabled, URL: MountPrefix + inst.ID + "/",
		})
	}
	for _, e := range audit {
		out.Audit = append(out.Audit, adminAuditJSON{
			At: e.At.UTC().Format("2006-01-02T15:04:05Z"), Actor: e.Actor,
			Action: e.Action, Target: e.Target, Detail: e.Detail, OK: e.OK,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminLive 返回管理台页面上会随时间变的那部分状态。
//
// 与 page 的渲染共用同一批计算（instanceLiveState / liveEnv），所以「页面刚
// 打开看到的」与「三秒后自动刷到的」不会各说一套。只读、无副作用；环境探测
// 只在 ?env=1 时做——那是唯一会真去 exec 的部分。
func (s *Server) handleAdminLive(w http.ResponseWriter, r *http.Request, lang i18n.Lang) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, i18n.T(lang, i18n.KeyLoginErrMethod))
		return
	}
	instances, err := s.opts.Store.ListInstances()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	users, err := s.opts.Store.ListUsers()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit, err := s.opts.Store.RecentAudit(auditLimit)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var running, enabled, installing int
	out := adminLiveJSON{
		OK:        true,
		Instances: make([]adminLiveInstanceJSON, 0, len(instances)),
	}
	for _, inst := range instances {
		live := s.instanceLiveState(r.Context(), lang, inst)
		if live.Running {
			running++
		}
		if inst.Enabled {
			enabled++
		}
		if live.JobPhase != "" {
			installing++
		}
		out.Instances = append(out.Instances, adminLiveInstanceJSON{
			ID: inst.ID, Running: live.Running, Ready: live.Ready,
			Installed: live.Installed, PID: live.PID,
			RunLabel: live.RunLabel, RunSource: live.RunSource,
			InstalledLabel: live.InstalledLabel, NotReadyLabel: live.NotReadyLabel,
			JobPhase: live.JobPhase, JobNote: live.JobNote,
			RunError: live.RunError, Command: live.Command,
		})
	}
	out.Stats = adminLiveStatsJSON{
		Instances: len(instances), Running: running, Enabled: enabled,
		Installing: installing, Users: len(users), Audit: len(audit),
		InstancesNote: instancesStatNote(lang, running, enabled, installing, s.opts.Installer != nil),
		RunningNote:   runningStatNote(lang, len(instances)),
	}

	out.Audit.Rows = make([]adminAuditLiveJSON, 0, len(audit))
	for _, e := range audit {
		if e.ID > out.Audit.Rev {
			out.Audit.Rev = e.ID
		}
		out.Audit.Rows = append(out.Audit.Rows, adminAuditLiveJSON{
			At: e.At.Local().Format("2006-01-02 15:04:05"), Actor: e.Actor,
			Action: e.Action, ActionLabel: auditActionLabel(lang, e.Action),
			Target: e.Target, Detail: e.Detail,
			DetailLabel: auditDetailLabel(lang, e.Action, e.Detail, e.OK),
		})
	}

	if r.URL.Query().Get("env") == "1" {
		out.Env = s.liveEnv(r.Context(), lang)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminAction 执行一个管理动作。
func (s *Server) handleAdminAction(w http.ResponseWriter, r *http.Request, lang i18n.Lang, actor *User) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, i18n.T(lang, i18n.KeyLoginErrMethod))
		return
	}
	// 只接受 JSON：跨站表单发不出这个 content-type，等于免费挡掉一类 CSRF。
	essence := r.Header.Get("Content-Type")
	if i := strings.IndexByte(essence, ';'); i >= 0 {
		essence = essence[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(essence), "application/json") {
		writeJSONError(w, http.StatusUnsupportedMediaType, "content-type must be application/json")
		return
	}
	// 纵深：带了 Origin/Referer 就必须与 Host 同源。
	if !sameOrigin(r) {
		s.log.Warn("管理 API 同源校验失败", "user", actor.Name, "origin", r.Header.Get("Origin"))
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: actor.Name, Action: "admin.csrf-rejected", Target: "json-api",
			IP: cookie.ClientIP(r), OK: false,
		})
		writeJSONError(w, http.StatusForbidden, i18n.T(lang, i18n.KeyAdminErrCSRF))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxAdminBodyBytes))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, i18n.T(lang, i18n.KeyAdminErrBadRequest))
		return
	}
	var act adminAction
	if err := json.Unmarshal(body, &act); err != nil {
		writeJSONError(w, http.StatusBadRequest, i18n.T(lang, i18n.KeyAdminErrBadRequest))
		return
	}
	act.Action = strings.TrimSpace(act.Action)
	act.Name = strings.TrimSpace(act.Name)

	if err := s.runAdminAction(actor, &act, cookie.ClientIP(r)); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, s.adminErrorText(lang, act.Action, act.Name, err))
		return
	}
	resp := map[string]any{"ok": true, "action": act.Action, "target": act.Name}
	// instance.remove 勾选了删目录时，把实际删掉的路径也告诉调用方：
	// 脚本化运维要能核对「到底删了哪几个目录」，而不是只看一个 ok。
	if len(act.RemovedDirs) > 0 {
		resp["removedDirs"] = act.RemovedDirs
	}
	writeJSON(w, http.StatusOK, resp)
}
