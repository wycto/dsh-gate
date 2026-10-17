// 管理台的 JSON API：给脚本与自动化用，和管理页面走**同一条**动作实现。
//
// 两个端点：
//
//	GET  /_portal/api/admin/state   当前状态（用户/实例/授权矩阵/审计），不含任何哈希
//	POST /_portal/api/admin/action  执行一个管理动作，body 是 adminAction 的 JSON
//
// 安全：两者都要求管理员会话；写操作还要求 Content-Type 是 application/json 且
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

	if err := s.runAdminAction(actor, act, cookie.ClientIP(r)); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, s.adminErrorText(lang, act.Action, act.Name, err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": act.Action, "target": act.Name})
}
