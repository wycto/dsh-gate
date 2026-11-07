// 对外 HTTP API（docs/service-api.md 第 4/5/6 节）。
//
// 与现有两套入口的区别：
//   - /_portal/api/admin/*  用管理员登录会话（浏览器/管理台用）；
//   - /_portal/api/local    仅回环 + portal.token（本机 CLI 用）；
//   - /api/v1/*             本文件：外部系统 / AI 用，Authorization: Bearer dgk_… 鉴权。
//
// 三条硬约束：
//  1. **默认关闭**：settings.external_api_enabled 缺省关，关闭时整个前缀 404（不可见）；
//  2. 鉴权顺序：开关 → Bearer 密钥 → IP 白名单 → 每 key 限速 → scope；任一不过即拒；
//  3. 写操作与鉴权失败都写审计（actor = api:<密钥名>，含来源 IP）。
package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wycto/dsh-gate/internal/cookie"
	"github.com/wycto/dsh-gate/internal/discover"
)

// APIPrefix 是外部 API 的路径前缀。
const APIPrefix = "/api/v1"

// defaultAPIRateLimitPerMin 是每个密钥默认的每分钟请求上限（可用 ServerOptions 覆盖）。
const defaultAPIRateLimitPerMin = 60

// maxAPIBodyBytes 限制 JSON 请求体大小。
const maxAPIBodyBytes = 64 << 10

// ---- 限速：固定窗口，按密钥 id 计数 ----

type apiWindow struct {
	start time.Time
	count int
}

// apiRateLimiter 是每个密钥的固定窗口限速器（内存态，进程内）。
type apiRateLimiter struct {
	mu    sync.Mutex
	limit int
	win   map[string]*apiWindow
}

func newAPIRateLimiter(perMin int) *apiRateLimiter {
	if perMin <= 0 {
		perMin = defaultAPIRateLimitPerMin
	}
	return &apiRateLimiter{limit: perMin, win: map[string]*apiWindow{}}
}

// allow 报告该 key 本次请求是否放行；超限返回 false。
func (l *apiRateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w := l.win[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		l.win[key] = &apiWindow{start: now, count: 1}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

// ---- JSON 辅助 ----

func writeAPIJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeAPIError 输出统一错误体 {ok:false,error:{code,message}}。
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeAPIJSON(w, status, map[string]any{
		"ok":    false,
		"error": map[string]any{"code": code, "message": message},
	})
}

// apiServiceJSON 是一个服务的对外 JSON 形态。P1 起带受管字段与运行态。
type apiServiceJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Upstream    string `json:"upstream"`
	HostHeader  string `json:"hostHeader"`
	InsecureTLS bool   `json:"insecureTLS"`
	Note        string `json:"note"`
	Enabled     bool   `json:"enabled"`
	URL         string `json:"url"`
	Managed     bool   `json:"managed"`
	Running     bool   `json:"running"`
	State       string `json:"state"`
}

// toAPIService 组装对外服务 JSON，并从 ServiceLifespan 取运行态（为 nil 时视为未运行）。
func (s *Server) toAPIService(svc *Service) apiServiceJSON {
	out := apiServiceJSON{
		ID: svc.ID, Name: svc.Name, Upstream: svc.Upstream,
		HostHeader: svc.HostHeader, InsecureTLS: svc.InsecureTLS,
		Note: svc.Note, Enabled: svc.Enabled,
		URL: ServiceMountPrefix + svc.ID + "/", Managed: svc.Managed,
	}
	if s.opts.ServiceLifespan != nil {
		st := s.opts.ServiceLifespan.Status(svc.ID)
		out.Running = st.Running
		out.State = st.State
	}
	return out
}

// apiPathMatches 判断请求路径是否落在 /api/v1 下（避免 /api/v10 之类误匹配）。
func apiPathMatches(path string) bool {
	if path == APIPrefix {
		return true
	}
	return strings.HasPrefix(path, APIPrefix+"/")
}

// handleExternalAPI 是 /api/v1/* 的总入口（在登录 Gate 之前分流）。
func (s *Server) handleExternalAPI(w http.ResponseWriter, r *http.Request) {
	// ① 开关：关闭时整个前缀 404（外部 API 默认不可见）。
	enabled, err := s.opts.Store.ExternalAPIEnabled()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", "读取外部 API 开关失败")
		return
	}
	if !enabled {
		http.NotFound(w, r)
		return
	}

	rest := strings.TrimPrefix(r.URL.Path, APIPrefix) // 形如 "" 或 "/health"
	if rest == "" {
		rest = "/"
	}

	// 健康探针免鉴权（但要求已开启）。
	if rest == "/health" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "只支持 GET")
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	// ② Bearer 密钥。
	secret, ok := bearerToken(r)
	if !ok {
		s.auditAPIAuthDenied("", "缺少或非法的 Authorization 头", r)
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "需要 Authorization: Bearer <API 密钥>")
		return
	}
	key, err := s.opts.Store.AuthenticateAPIKey(secret)
	if err != nil {
		s.auditAPIAuthDenied("", "API 密钥无效", r)
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "API 密钥无效")
		return
	}

	// ③ IP 白名单。
	ip := cookie.ClientIP(r)
	if !MatchIPAllowlist(key.IPAllowlist, ip) {
		s.auditAPIAuthDenied(key.Name, "来源 IP 不在白名单: "+ip, r)
		writeAPIError(w, http.StatusForbidden, "forbidden", "来源 IP 不在白名单")
		return
	}

	// ④ 限速。
	if s.apiLimiter != nil && !s.apiLimiter.allow(key.ID) {
		writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁")
		return
	}

	// ⑤ 路由 + scope。
	s.routeExternalAPI(w, r, key, rest, ip)
}

// routeExternalAPI 按路径分发。所有写操作成功/失败都会写审计。
func (s *Server) routeExternalAPI(w http.ResponseWriter, r *http.Request, key *APIKey, rest, ip string) {
	segs := splitAPIPath(rest)
	if len(segs) == 0 {
		writeAPIError(w, http.StatusNotFound, "not_found", "未知路径")
		return
	}
	switch segs[0] {
	case "services":
		s.handleAPIServices(w, r, key, segs, ip)
	case "instances":
		s.handleAPIInstances(w, r, key, segs, ip)
	case "discover":
		s.handleAPIDiscover(w, r, key, segs, ip)
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "未知路径")
	}
}

// handleAPIServices 处理 /services 与 /services/{id}（P0：无生命周期端点）。
func (s *Server) handleAPIServices(w http.ResponseWriter, r *http.Request, key *APIKey, segs []string, ip string) {
	switch len(segs) {
	case 1: // /services
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			if !s.requireAPIScope(w, key, ScopeServicesRead, ip) {
				return
			}
			list, err := s.opts.Store.ListServices()
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			out := make([]apiServiceJSON, 0, len(list))
			for i := range list {
				out = append(out, s.toAPIService(&list[i]))
			}
			writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "services": out})
		case http.MethodPost:
			if !s.requireAPIScope(w, key, ScopeServicesWrite, ip) {
				return
			}
			s.apiCreateService(w, r, key, ip)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "不支持的方法")
		}
	case 2: // /services/{id}
		ref := segs[1]
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			if !s.requireAPIScope(w, key, ScopeServicesRead, ip) {
				return
			}
			svc, err := s.resolveAPIService(ref)
			if err != nil {
				writeAPIError(w, http.StatusNotFound, "not_found", "服务不存在")
				return
			}
			writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "service": s.toAPIService(svc)})
		case http.MethodPatch:
			if !s.requireAPIScope(w, key, ScopeServicesWrite, ip) {
				return
			}
			s.apiUpdateService(w, r, key, ref, ip)
		case http.MethodDelete:
			if !s.requireAPIScope(w, key, ScopeServicesWrite, ip) {
				return
			}
			s.apiDeleteService(w, key, ref, ip)
		default:
			w.Header().Set("Allow", "GET, PATCH, DELETE")
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "不支持的方法")
		}
	case 3: // /services/{id}/{verb}：生命周期与运行态（P1）
		s.handleAPIServiceVerb(w, r, key, segs[1], segs[2], ip)
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "未知路径")
	}
}

// handleAPIServiceVerb 处理 /services/{id}/{start|stop|restart|status|logs}。
//
// 生命周期（start/stop/restart）只对受管服务（managed=1）有效；纯代理服务返回 409
// unsupported。ServiceLifespan 为 nil（未接运行时）时生命周期 503，状态返回零值。
func (s *Server) handleAPIServiceVerb(w http.ResponseWriter, r *http.Request, key *APIKey, ref, verb, ip string) {
	switch verb {
	case "start", "stop", "restart":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "只支持 POST")
			return
		}
		if !s.requireAPIScope(w, key, ScopeServicesLifecycle, ip) {
			return
		}
		svc, err := s.resolveAPIService(ref)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		if s.opts.ServiceLifespan == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "unsupported", "管理端未接入服务运行时，暂不支持启停")
			return
		}
		if !svc.Managed {
			writeAPIError(w, http.StatusConflict, "unsupported", "纯代理服务不支持启停（未设置启动命令）")
			return
		}
		var callErr error
		switch verb {
		case "start":
			callErr = s.opts.ServiceLifespan.StartService(*svc)
		case "stop":
			callErr = s.opts.ServiceLifespan.StopService(svc.ID)
		case "restart":
			callErr = s.opts.ServiceLifespan.RestartService(*svc)
		}
		if callErr != nil {
			s.auditAPI(key, "api.service."+verb, svc.Name, callErr.Error(), ip, false)
			writeAPIError(w, http.StatusInternalServerError, "internal", callErr.Error())
			return
		}
		s.auditAPI(key, "api.service."+verb, svc.Name, "", ip, true)
		writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true})

	case "status":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET")
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "只支持 GET")
			return
		}
		if !s.requireAPIScope(w, key, ScopeServicesRead, ip) {
			return
		}
		svc, err := s.resolveAPIService(ref)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		var st ServiceRunStatus
		if s.opts.ServiceLifespan != nil {
			st = s.opts.ServiceLifespan.Status(svc.ID)
		}
		body := map[string]any{
			"running": st.Running, "pid": st.PID, "state": st.State,
			"lastError": st.LastError,
		}
		if !st.StartedAt.IsZero() {
			body["startedAt"] = st.StartedAt.UTC().Format(time.RFC3339)
		}
		writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "status": body})

	case "logs":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET")
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "只支持 GET")
			return
		}
		if !s.requireAPIScope(w, key, ScopeServicesRead, ip) {
			return
		}
		svc, err := s.resolveAPIService(ref)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, "not_found", "服务不存在")
			return
		}
		if s.opts.ServiceLifespan == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "unsupported", "管理端未接入服务运行时")
			return
		}
		if !svc.Managed {
			writeAPIError(w, http.StatusConflict, "unsupported", "纯代理服务没有日志")
			return
		}
		tail := 200
		if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
			if n, e := strconv.Atoi(raw); e == nil && n > 0 {
				tail = n
			}
		}
		text, err := s.opts.ServiceLifespan.Logs(svc.ID, tail)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "logs": text})

	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "未知的服务子路径")
	}
}

// apiServiceInput 是创建/修改服务的请求体。PATCH 用指针区分「未提供」。
type apiServiceInput struct {
	Name        *string `json:"name"`
	Upstream    *string `json:"upstream"`
	HostHeader  *string `json:"hostHeader"`
	InsecureTLS *bool   `json:"insecureTLS"`
	Note        *string `json:"note"`
	// 受管服务字段（P1）。Managed 为真时需 StartCmd。
	Managed    *bool   `json:"managed"`
	StartCmd   *string `json:"startCmd"`
	StopCmd    *string `json:"stopCmd"`
	WorkDir    *string `json:"workDir"`
	Env        *string `json:"env"`
	AutoStart  *bool   `json:"autoStart"`
	HealthPath *string `json:"healthPath"`
}

func (s *Server) apiCreateService(w http.ResponseWriter, r *http.Request, key *APIKey, ip string) {
	var in apiServiceInput
	if !decodeAPIBody(w, r, &in) {
		return
	}
	name := strOr(in.Name, "")
	upstream := strOr(in.Upstream, "")
	svc := &Service{
		Name:        name,
		Upstream:    upstream,
		HostHeader:  strOr(in.HostHeader, ""),
		InsecureTLS: in.InsecureTLS != nil && *in.InsecureTLS,
		Note:        strOr(in.Note, ""),
		Managed:     in.Managed != nil && *in.Managed,
		StartCmd:    strOr(in.StartCmd, ""),
		StopCmd:     strOr(in.StopCmd, ""),
		WorkDir:     strOr(in.WorkDir, ""),
		Env:         strOr(in.Env, ""),
		AutoStart:   in.AutoStart != nil && *in.AutoStart,
		HealthPath:  strOr(in.HealthPath, ""),
	}
	if err := s.opts.Store.CreateService(svc); err != nil {
		s.auditAPI(key, "api.service.create", name, err.Error(), ip, false)
		status := http.StatusBadRequest
		code := "invalid_argument"
		if strings.Contains(err.Error(), "已存在") {
			status, code = http.StatusConflict, "conflict"
		}
		writeAPIError(w, status, code, err.Error())
		return
	}
	s.auditAPI(key, "api.service.create", svc.Name, "", ip, true)
	writeAPIJSON(w, http.StatusCreated, map[string]any{"ok": true, "service": s.toAPIService(svc)})
}

func (s *Server) apiUpdateService(w http.ResponseWriter, r *http.Request, key *APIKey, ref, ip string) {
	svc, err := s.resolveAPIService(ref)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "服务不存在")
		return
	}
	var in apiServiceInput
	if !decodeAPIBody(w, r, &in) {
		return
	}
	oldManaged := svc.Managed
	if in.Name != nil {
		svc.Name = *in.Name
	}
	if in.Upstream != nil {
		svc.Upstream = *in.Upstream
	}
	if in.HostHeader != nil {
		svc.HostHeader = *in.HostHeader
	}
	if in.InsecureTLS != nil {
		svc.InsecureTLS = *in.InsecureTLS
	}
	if in.Note != nil {
		svc.Note = *in.Note
	}
	if in.Managed != nil {
		svc.Managed = *in.Managed
	}
	if in.StartCmd != nil {
		svc.StartCmd = *in.StartCmd
	}
	if in.StopCmd != nil {
		svc.StopCmd = *in.StopCmd
	}
	if in.WorkDir != nil {
		svc.WorkDir = *in.WorkDir
	}
	if in.Env != nil {
		svc.Env = *in.Env
	}
	if in.AutoStart != nil {
		svc.AutoStart = *in.AutoStart
	}
	if in.HealthPath != nil {
		svc.HealthPath = *in.HealthPath
	}
	if err := s.opts.Store.UpdateService(*svc); err != nil {
		s.auditAPI(key, "api.service.update", svc.Name, err.Error(), ip, false)
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	// 让挂载缓存按新定义重建（与页面更新同一口径）。
	s.invalidateServiceMount(svc.ID)
	// 从受管改为纯代理时，停掉正在跑的进程，避免留下孤儿。
	if oldManaged && !svc.Managed && s.opts.ServiceLifespan != nil {
		_ = s.opts.ServiceLifespan.StopService(svc.ID)
	}
	s.auditAPI(key, "api.service.update", svc.Name, "", ip, true)
	again, err := s.opts.Store.ServiceByID(svc.ID)
	if err != nil {
		writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "service": s.toAPIService(svc)})
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "service": s.toAPIService(again)})
}

func (s *Server) apiDeleteService(w http.ResponseWriter, key *APIKey, ref, ip string) {
	svc, err := s.resolveAPIService(ref)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "服务不存在")
		return
	}
	if err := s.opts.Store.DeleteService(svc.ID); err != nil {
		s.auditAPI(key, "api.service.remove", svc.Name, err.Error(), ip, false)
		writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.invalidateServiceMount(svc.ID)
	s.auditAPI(key, "api.service.remove", svc.Name, "", ip, true)
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAPIInstances 处理 GET /instances（P0 只读）。
func (s *Server) handleAPIInstances(w http.ResponseWriter, r *http.Request, key *APIKey, segs []string, ip string) {
	if len(segs) != 1 {
		writeAPIError(w, http.StatusNotFound, "not_found", "未知路径")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "只支持 GET")
		return
	}
	if !s.requireAPIScope(w, key, ScopeInstancesRead, ip) {
		return
	}
	list, err := s.opts.Store.ListInstances()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, inst := range list {
		item := map[string]any{
			"id":      inst.ID,
			"name":    inst.Name,
			"enabled": inst.Enabled,
			"url":     MountPrefix + inst.ID + "/",
		}
		if s.opts.Lifespan != nil {
			st := s.opts.Lifespan.InstanceStatus(inst)
			item["running"] = st.Running
			item["ready"] = st.Ready
			item["pid"] = st.PID
			if st.Error != "" {
				item["error"] = st.Error
			}
		} else {
			item["running"] = false
			item["ready"] = false
			item["pid"] = 0
		}
		out = append(out, item)
	}
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "instances": out})
}

// ---- 鉴权/scope 辅助 ----

// requireAPIScope 校验 scope；不足时写 403 并返回 false。
func (s *Server) requireAPIScope(w http.ResponseWriter, key *APIKey, scope, ip string) bool {
	if key.HasScope(scope) {
		return true
	}
	s.auditAPI(key, "api.denied", scope, "缺少 scope: "+scope, ip, false)
	writeAPIError(w, http.StatusForbidden, "forbidden", "缺少权限 scope: "+scope)
	return false
}

// bearerToken 从 Authorization 头取出 Bearer 令牌。
func bearerToken(r *http.Request) (string, bool) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// decodeAPIBody 解码 JSON 请求体；失败时写 400 并返回 false。
func decodeAPIBody(w http.ResponseWriter, r *http.Request, out any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBodyBytes))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "读取请求体失败")
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "请求体为空")
		return false
	}
	if err := json.Unmarshal(body, out); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "请求体不是合法 JSON: "+err.Error())
		return false
	}
	return true
}

// resolveAPIService 按 id 或名字取服务（id 优先，镜像本机 CLI 的解析思路）。
func (s *Server) resolveAPIService(ref string) (*Service, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, ErrNotFound
	}
	if ServiceIDPattern.MatchString(ref) {
		if svc, err := s.opts.Store.ServiceByID(ref); err == nil {
			return svc, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return s.opts.Store.ServiceByName(ref)
}

// splitAPIPath 把 "/services/abc/" 折成 ["services","abc"]。
func splitAPIPath(rest string) []string {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return nil
	}
	return strings.Split(rest, "/")
}

func strOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return strings.TrimSpace(*p)
}

// ---- 审计辅助 ----

// auditAPI 写一条 API 动作审计（actor = api:<密钥名>）。
func (s *Server) auditAPI(key *APIKey, action, target, detail, ip string, ok bool) {
	actor := ""
	if key != nil {
		actor = "api:" + key.Name
	}
	_ = s.opts.Store.Audit(AuditEntry{
		Actor: actor, Action: action, Target: target, Detail: detail, IP: ip, OK: ok,
	})
}

// auditAPIAuthDenied 记录一次鉴权失败（没有可用的密钥名时 actor 为空）。
func (s *Server) auditAPIAuthDenied(keyName, detail string, r *http.Request) {
	actor := ""
	if keyName != "" {
		actor = "api:" + keyName
	}
	_ = s.opts.Store.Audit(AuditEntry{
		Actor: actor, Action: "api.auth.denied", Detail: detail, IP: cookie.ClientIP(r), OK: false,
	})
}

// ---- 服务发现（docs/service-api.md 第 15 节）----

// apiDiscoverCandidate 是候选服务的对外 JSON。
type apiDiscoverCandidate struct {
	Source            string `json:"source"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	PID               int    `json:"pid,omitempty"`
	Command           string `json:"command,omitempty"`
	Cwd               string `json:"cwd,omitempty"`
	HTTP              bool   `json:"http"`
	Title             string `json:"title,omitempty"`
	AlreadyRegistered bool   `json:"alreadyRegistered"`
}

// handleAPIDiscover 处理 /discover/{local|scan|register}（均 POST）。
func (s *Server) handleAPIDiscover(w http.ResponseWriter, r *http.Request, key *APIKey, segs []string, ip string) {
	if len(segs) != 2 {
		writeAPIError(w, http.StatusNotFound, "not_found", "未知路径")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_argument", "只支持 POST")
		return
	}
	switch segs[1] {
	case "local":
		if !s.requireAPIScope(w, key, ScopeServicesRead, ip) {
			return
		}
		s.apiDiscoverLocal(w, key, ip)
	case "scan":
		if !s.requireAPIScope(w, key, ScopeServicesRead, ip) {
			return
		}
		s.apiDiscoverScan(w, r, key, ip)
	case "register":
		if !s.requireAPIScope(w, key, ScopeServicesWrite, ip) {
			return
		}
		s.apiDiscoverRegister(w, r, key, ip)
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "未知路径")
	}
}

// registeredUpstreams 返回已登记服务的 upstream 集合（用于标记已登记候选）。
func (s *Server) registeredUpstreams() map[string]bool {
	set := map[string]bool{}
	if list, err := s.opts.Store.ListServices(); err == nil {
		for i := range list {
			set[list[i].Upstream] = true
		}
	}
	return set
}

func (s *Server) toDiscoverCandidates(cands []discover.Candidate) []apiDiscoverCandidate {
	reg := s.registeredUpstreams()
	out := make([]apiDiscoverCandidate, 0, len(cands))
	for _, c := range cands {
		up := fmt.Sprintf("http://%s:%d", c.Host, c.Port)
		out = append(out, apiDiscoverCandidate{
			Source: c.Source, Host: c.Host, Port: c.Port, PID: c.PID, Command: c.Command,
			Cwd: c.Cwd, HTTP: c.HTTP, Title: c.Title, AlreadyRegistered: reg[up],
		})
	}
	return out
}

func (s *Server) apiDiscoverLocal(w http.ResponseWriter, key *APIKey, ip string) {
	cands, err := discover.Local()
	if errors.Is(err, discover.ErrUnsupported) {
		s.auditAPI(key, "api.discover.local", "", err.Error(), ip, true)
		writeAPIJSON(w, http.StatusOK, map[string]any{
			"ok": true, "candidates": []any{}, "unsupported": true, "message": err.Error(),
		})
		return
	}
	if err != nil {
		s.auditAPI(key, "api.discover.local", "", err.Error(), ip, false)
		writeAPIError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := s.toDiscoverCandidates(cands)
	s.auditAPI(key, "api.discover.local", "", fmt.Sprintf("%d 个候选", len(out)), ip, true)
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "candidates": out})
}

// apiScanInput 的 ports 允许字符串（"80,443,3000-3100"）或整数/字符串数组。
type apiScanInput struct {
	CIDR  string          `json:"cidr"`
	Ports json.RawMessage `json:"ports"`
}

func (s *Server) apiDiscoverScan(w http.ResponseWriter, r *http.Request, key *APIKey, ip string) {
	var in apiScanInput
	if !decodeAPIBody(w, r, &in) {
		return
	}
	cidr := strings.TrimSpace(in.CIDR)
	if cidr == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "缺少 cidr")
		return
	}
	ok, err := discover.IsPrivateCIDR(cidr)
	if err != nil || !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "只允许私有网段（10/8、172.16/12、192.168/16）")
		return
	}
	ports, perr := parseAPIPorts(in.Ports)
	if perr != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", perr.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cands, serr := discover.Scan(ctx, cidr, ports, discover.ScanOptions{})
	if serr != nil && len(cands) == 0 {
		s.auditAPI(key, "api.discover.scan", cidr, serr.Error(), ip, false)
		writeAPIError(w, http.StatusInternalServerError, "internal", serr.Error())
		return
	}
	out := s.toDiscoverCandidates(cands)
	s.auditAPI(key, "api.discover.scan", cidr, fmt.Sprintf("%d 个候选", len(out)), ip, true)
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "candidates": out})
}

func parseAPIPorts(raw json.RawMessage) ([]int, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, errors.New("缺少 ports")
	}
	if strings.HasPrefix(trimmed, "\"") {
		var spec string
		if err := json.Unmarshal(raw, &spec); err != nil {
			return nil, err
		}
		return discover.ParsePorts(spec)
	}
	var arr []int
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var sarr []string
	if err := json.Unmarshal(raw, &sarr); err == nil {
		return discover.ParsePorts(strings.Join(sarr, ","))
	}
	return nil, errors.New("ports 需为字符串或整数数组")
}

type apiRegisterInput struct {
	Candidates []apiRegisterCandidate `json:"candidates"`
}

type apiRegisterCandidate struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Note     string `json:"note"`
	Managed  bool   `json:"managed"`
	StartCmd string `json:"startCmd"`
}

func (s *Server) apiDiscoverRegister(w http.ResponseWriter, r *http.Request, key *APIKey, ip string) {
	var in apiRegisterInput
	if !decodeAPIBody(w, r, &in) {
		return
	}
	if len(in.Candidates) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_argument", "candidates 为空")
		return
	}
	created := make([]map[string]any, 0)
	skipped := make([]map[string]any, 0)
	for _, c := range in.Candidates {
		host := strings.TrimSpace(c.Host)
		if host == "" || c.Port <= 0 || c.Port > 65535 {
			skipped = append(skipped, map[string]any{"host": host, "port": c.Port, "reason": "非法 host/port"})
			continue
		}
		up := fmt.Sprintf("http://%s:%d", host, c.Port)
		if _, err := s.findServiceByUpstream(up); err == nil {
			skipped = append(skipped, map[string]any{"host": host, "port": c.Port, "reason": "已登记"})
			continue
		}
		base := strings.TrimSpace(c.Name)
		if base == "" {
			base = "svc-" + sanitizeNamePart(host) + "-" + strconv.Itoa(c.Port)
		}
		name := s.uniqueServiceName(base)
		managed := c.Managed && strings.TrimSpace(c.StartCmd) != ""
		svc := &Service{Name: name, Upstream: up, Note: strings.TrimSpace(c.Note), Managed: managed, StartCmd: strings.TrimSpace(c.StartCmd)}
		if err := s.opts.Store.CreateService(svc); err != nil {
			skipped = append(skipped, map[string]any{"host": host, "port": c.Port, "reason": err.Error()})
			continue
		}
		created = append(created, map[string]any{"id": svc.ID, "name": svc.Name})
	}
	s.auditAPI(key, "api.discover.register", "", fmt.Sprintf("新建 %d、跳过 %d", len(created), len(skipped)), ip, true)
	writeAPIJSON(w, http.StatusOK, map[string]any{"ok": true, "created": created, "skipped": skipped})
}

func (s *Server) findServiceByUpstream(up string) (*Service, error) {
	list, err := s.opts.Store.ListServices()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Upstream == up {
			return &list[i], nil
		}
	}
	return nil, ErrNotFound
}

func (s *Server) uniqueServiceName(base string) string {
	base = strings.TrimSpace(base)
	if base == "" || ValidateServiceName(base) != nil {
		base = "svc"
	}
	if _, err := s.opts.Store.ServiceByName(base); errors.Is(err, ErrNotFound) {
		return base
	}
	for i := 2; i < 100; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if _, err := s.opts.Store.ServiceByName(cand); errors.Is(err, ErrNotFound) {
			return cand
		}
	}
	return base + "-x"
}

// sanitizeNamePart 把 host 变成可进服务名的片段（只留字母数字与 - _）。
func sanitizeNamePart(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '.' || r == ':':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "host"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
