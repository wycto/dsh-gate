// 本机控制 API：给 `dsh-gate dsh ...` 这类本机 CLI 用的一小组管理动作。
//
// 与 /_portal/api/admin/* 的区别（两者刻意分开）：
//
//   - 管理 API 面向浏览器/脚本，靠**管理员登录会话**鉴权，走登录门；
//   - 本机 API 面向本机 CLI，CLI 手里没有管理员口令，改用一个放在状态目录里的
//     随机令牌（portal.token，0600）+ 只接受回环来源。
//
// 它只做 CLI 做不到的事：启停一个由管理端监督者托管的 dsh。列表放在这里是为了
// 让 CLI 一次拿到「库里的定义 + 运行状态」，不用两边拼。
package portal

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/discover"
)

// LocalAPIPath 是本机控制 API 的路径。
const LocalAPIPath = "/_portal/api/local"

// maxLocalBodyBytes 限制本机 API 请求体：真实动作只有几百字节。
const maxLocalBodyBytes = 32 << 10

// localRequest 是本机控制 API 的请求体。
type localRequest struct {
	// Action 是 list / start / stop / restart / remove。
	Action string `json:"action"`
	// Instance 是实例名或不透明 id。
	Instance string `json:"instance"`
	// PurgeHome / PurgeInstall 只在 remove 时有意义：删数据目录 / 安装目录。
	PurgeHome    bool `json:"purgeHome"`
	PurgeInstall bool `json:"purgeInstall"`
	// Service 是服务名或不透明 id（svc.* 动作用；svc.add 时是展示名）。
	Service string `json:"service"`
	// Upstream / HostHeader / Note / InsecureTLS 只在 svc.add 时有意义。
	Upstream    string `json:"upstream"`
	HostHeader  string `json:"hostHeader"`
	Note        string `json:"note"`
	InsecureTLS bool   `json:"insecureTLS"`
	// ---- 受管服务（svc.add 时）----
	Managed    bool   `json:"managed,omitempty"`
	StartCmd   string `json:"startCmd,omitempty"`
	StopCmd    string `json:"stopCmd,omitempty"`
	WorkDir    string `json:"workDir,omitempty"`
	Env        string `json:"env,omitempty"`
	AutoStart  bool   `json:"autoStart,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
	// Tail 只在 svc.logs 时有意义：读取最近多少行（<=0 默认 200）。
	Tail int `json:"tail,omitempty"`
	// ---- 服务发现（svc.discover / svc.discover.add）----
	// Mode 是 local（本机）或 cidr（网段扫描）；CIDR/Ports 见 svc.discover。
	Mode       string           `json:"mode,omitempty"`
	CIDR       string           `json:"cidr,omitempty"`
	Ports      string           `json:"ports,omitempty"`
	Candidates []localCandidate `json:"candidates,omitempty"`
}

// localInstance 是本机 API 返回的一个实例（定义 + 运行状态）。
type localInstance struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	DshHome    string `json:"dshHome"`
	ConfigDir  string `json:"configDir"`
	Workspace  string `json:"workspace"`
	InstallDir string `json:"installDir"`
	Port       int    `json:"port"`
	Enabled    bool   `json:"enabled"`
	Running    bool   `json:"running"`
	Ready      bool   `json:"ready"`
	PID        int    `json:"pid"`
	Error      string `json:"error,omitempty"`
	URL        string `json:"url"`
}

// localService 是本机 API 返回的一个内网服务（定义 + 挂载地址）。
type localService struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	Note     string `json:"note"`
	// Managed 表示受管进程；State 是运行态（受管才有，纯代理为空）。
	Managed bool   `json:"managed"`
	State   string `json:"state,omitempty"`
}

// localCandidate 是服务发现的一个候选（本机 API 用）。
type localCandidate struct {
	Source  string `json:"source,omitempty"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Name    string `json:"name,omitempty"`
	Command string `json:"command,omitempty"`
	Title   string `json:"title,omitempty"`
	Already bool   `json:"alreadyRegistered"`
}

// localResponse 是本机 API 的统一响应体。
type localResponse struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Instances []localInstance `json:"instances,omitempty"`
	Services  []localService  `json:"services,omitempty"`
	// Probe 只在 svc.probe 时返回（按需探活的结果，同时写入探活缓存）。
	Probe *ServiceProbe `json:"probe,omitempty"`
	// Logs 只在 svc.logs 时返回（受管服务的最近日志）。
	Logs string `json:"logs,omitempty"`
	// Candidates 只在 svc.discover 时返回；Created/Skipped 只在 svc.discover.add 时返回。
	Candidates []localCandidate `json:"candidates,omitempty"`
	Created    int              `json:"created,omitempty"`
	Skipped    int              `json:"skipped,omitempty"`
}

// handleLocalAPI 处理 /_portal/api/local。
//
// 安全边界：token 必须是启动时写进状态目录的那一把；请求必须来自回环地址。
// 两者都过了才允许动作，顺序上先判来源再比 token（少给外网一个试错 oracle）。
func (s *Server) handleLocalAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		s.writeLocalError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	if !loopbackRequest(r) {
		s.writeLocalError(w, http.StatusForbidden, "只接受本机（回环）请求")
		return
	}
	if s.opts.LocalToken == "" {
		s.writeLocalError(w, http.StatusServiceUnavailable, "管理端没有开放本机控制入口")
		return
	}
	if !localTokenOK(r, s.opts.LocalToken) {
		s.writeLocalError(w, http.StatusUnauthorized, "令牌不正确")
		return
	}

	var req localRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxLocalBodyBytes))
	if err := decoder.Decode(&req); err != nil {
		s.writeLocalError(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}

	switch req.Action {
	case "list":
		s.localList(w)
	case "start", "stop", "restart":
		s.localLifecycle(w, req)
	case "remove":
		s.localRemove(w, req)
	case "svc.list":
		s.localSvcList(w)
	case "svc.add":
		s.localSvcAdd(w, req)
	case "svc.remove":
		s.localSvcRemove(w, req)
	case "svc.enable", "svc.disable":
		s.localSvcToggle(w, req)
	case "svc.probe":
		s.localSvcProbe(w, r, req)
	case "svc.start", "svc.stop", "svc.restart":
		s.localSvcLifecycle(w, req)
	case "svc.logs":
		s.localSvcLogs(w, req)
	case "svc.discover":
		s.localSvcDiscover(w, req)
	case "svc.discover.add":
		s.localSvcDiscoverAdd(w, req)
	default:
		s.writeLocalError(w, http.StatusBadRequest, fmt.Sprintf("不认识的 action %q", req.Action))
	}
}

// localList 返回全部实例及其运行状态。
func (s *Server) localList(w http.ResponseWriter) {
	instances, err := s.opts.Store.ListInstances()
	if err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]localInstance, 0, len(instances))
	for _, inst := range instances {
		item := localInstance{
			ID: inst.ID, Name: inst.Name,
			DshHome: inst.DshHome, ConfigDir: inst.ConfigDir,
			Workspace: inst.Workspace, InstallDir: inst.InstallDir,
			Port: inst.DshPort, Enabled: inst.Enabled,
			URL: MountPrefix + inst.ID + "/",
		}
		if s.opts.Lifespan != nil {
			st := s.opts.Lifespan.InstanceStatus(inst)
			item.Running = st.Running
			item.Ready = st.Ready
			item.PID = st.PID
			item.Error = st.Error
		}
		out = append(out, item)
	}
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Instances: out})
}

// localLifecycle 执行 start / stop / restart。
func (s *Server) localLifecycle(w http.ResponseWriter, req localRequest) {
	if s.opts.Lifespan == nil {
		s.writeLocalError(w, http.StatusServiceUnavailable, "管理端没有可用的实例启停能力")
		return
	}
	inst, err := s.resolveLocalInstance(req.Instance)
	if err != nil {
		s.writeLocalError(w, http.StatusNotFound, err.Error())
		return
	}
	// 用 Background 而不是请求 ctx：浏览器/终端断开不该把「启动到一半」的实例
	// 腰斩，启停是用户明确下的动作，超时由 Lifespan 自己控制。
	ctx := context.Background()
	switch req.Action {
	case "start":
		err = s.opts.Lifespan.StartInstance(ctx, *inst)
	case "stop":
		err = s.opts.Lifespan.StopInstance(ctx, *inst)
	case "restart":
		err = s.opts.Lifespan.RestartInstance(ctx, *inst)
	}
	if err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true})
}

// localRemove 删除实例，并按参数决定是否连带删除目录。
//
// 直接复用管理台页面的删除实现：两边走同一条「先停 → 再删目录 → 最后删记录」
// 的路径，语义与安全校验不会漂移。
func (s *Server) localRemove(w http.ResponseWriter, req localRequest) {
	inst, err := s.resolveLocalInstance(req.Instance)
	if err != nil {
		s.writeLocalError(w, http.StatusNotFound, err.Error())
		return
	}
	act := &adminAction{InstanceID: inst.ID, DeleteHome: req.PurgeHome, DeleteInstall: req.PurgeInstall}
	if err := s.applyInstanceRemove(act); err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true})
}

// resolveLocalInstance 按 id 或名字取实例。id 优先：它不会与展示名撞车。
func (s *Server) resolveLocalInstance(ref string) (*Instance, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("缺少实例名或 id")
	}
	if InstanceIDPattern.MatchString(ref) {
		if inst, err := s.opts.Store.InstanceByID(ref); err == nil {
			return inst, nil
		}
	}
	inst, err := s.opts.Store.InstanceByName(ref)
	if err != nil {
		return nil, fmt.Errorf("找不到实例 %q", ref)
	}
	return inst, nil
}

// ---- 内网服务（svc.* 动作，见 docs/service-proxy.md 第 5/11 节）----
//
// 与实例动作同一口径：写动作经 Store.Audit 留痕（actor 固定 local-cli——
// 本机 API 只接受回环 + portal.token，操作者就是本机管理员）；
// 改动服务后一律 invalidateServiceMount，运行中的门户下一次请求按新定义重建。

// localSvcList 返回全部登记的服务。
func (s *Server) localSvcList(w http.ResponseWriter) {
	services, err := s.opts.Store.ListServices()
	if err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Services: s.localServicesOf(services)})
}

// localServicesOf 把库里的服务转成本机 API 的返回形状（含受管运行态）。
func (s *Server) localServicesOf(services []Service) []localService {
	out := make([]localService, 0, len(services))
	for _, svc := range services {
		item := localService{
			ID: svc.ID, Name: svc.Name, Upstream: svc.Upstream,
			Enabled: svc.Enabled, URL: ServiceMountPrefix + svc.ID + "/", Note: svc.Note,
			Managed: svc.Managed,
		}
		if svc.Managed && s.opts.ServiceLifespan != nil {
			item.State = s.opts.ServiceLifespan.Status(svc.ID).State
		}
		out = append(out, item)
	}
	return out
}

// localSvcAdd 登记一条服务：Name（Service 字段）与 Upstream 必填。
func (s *Server) localSvcAdd(w http.ResponseWriter, req localRequest) {
	if strings.TrimSpace(req.Service) == "" || strings.TrimSpace(req.Upstream) == "" {
		s.writeLocalError(w, http.StatusBadRequest, "svc.add 需要 service（服务名）与 upstream（上游地址）")
		return
	}
	svc := &Service{
		Name: strings.TrimSpace(req.Service), Upstream: req.Upstream,
		HostHeader: req.HostHeader, Note: req.Note, InsecureTLS: req.InsecureTLS,
		Managed: req.Managed, StartCmd: req.StartCmd, StopCmd: req.StopCmd,
		WorkDir: req.WorkDir, Env: req.Env, AutoStart: req.AutoStart, HealthPath: req.HealthPath,
	}
	// 给了启动命令即视为受管服务，避免「只填了 start-cmd 却忘了 --managed」。
	if strings.TrimSpace(svc.StartCmd) != "" {
		svc.Managed = true
	}
	if err := s.opts.Store.CreateService(svc); err != nil {
		s.writeLocalError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.opts.Store.Audit(AuditEntry{
		Actor: "local-cli", Action: "admin.service.create", Target: svc.Name,
		Detail: svc.Upstream, IP: "127.0.0.1", OK: true,
	})
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Services: s.localServicesOf([]Service{*svc})})
}

// localSvcRemove 删除服务登记（服务进程不归门户管，删的只是登记与授权）。
func (s *Server) localSvcRemove(w http.ResponseWriter, req localRequest) {
	svc, err := s.resolveLocalService(req.Service)
	if err != nil {
		s.writeLocalError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.opts.Store.DeleteService(svc.ID); err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.invalidateServiceMount(svc.ID)
	_ = s.opts.Store.Audit(AuditEntry{
		Actor: "local-cli", Action: "admin.service.delete", Target: svc.Name,
		Detail: svc.ID, IP: "127.0.0.1", OK: true,
	})
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true})
}

// localSvcToggle 执行 svc.enable / svc.disable。
func (s *Server) localSvcToggle(w http.ResponseWriter, req localRequest) {
	svc, err := s.resolveLocalService(req.Service)
	if err != nil {
		s.writeLocalError(w, http.StatusNotFound, err.Error())
		return
	}
	svc.Enabled = req.Action == "svc.enable"
	if err := s.opts.Store.UpdateService(*svc); err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.invalidateServiceMount(svc.ID)
	_ = s.opts.Store.Audit(AuditEntry{
		Actor: "local-cli", Action: "admin.service." + strings.TrimPrefix(req.Action, "svc."),
		Target: svc.Name, Detail: svc.ID, IP: "127.0.0.1", OK: true,
	})
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Services: s.localServicesOf([]Service{*svc})})
}

// localSvcProbe 按需探活：真打一次上游，结果写入探活缓存并随响应返回。
func (s *Server) localSvcProbe(w http.ResponseWriter, r *http.Request, req localRequest) {
	svc, err := s.resolveLocalService(req.Service)
	if err != nil {
		s.writeLocalError(w, http.StatusNotFound, err.Error())
		return
	}
	probe := s.probeService(r.Context(), *svc)
	s.storeServiceProbe(svc.ID, probe)
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Probe: &probe})
}

// localSvcLifecycle 执行 svc.start / svc.stop / svc.restart（仅受管服务）。
func (s *Server) localSvcLifecycle(w http.ResponseWriter, req localRequest) {
	if s.opts.ServiceLifespan == nil {
		s.writeLocalError(w, http.StatusServiceUnavailable, "管理端未接入受管服务运行时")
		return
	}
	svc, err := s.resolveLocalService(req.Service)
	if err != nil {
		s.writeLocalError(w, http.StatusNotFound, err.Error())
		return
	}
	if !svc.Managed {
		s.writeLocalError(w, http.StatusBadRequest, "该服务是纯代理型（无启动命令），不支持启动/停止/重启")
		return
	}
	switch req.Action {
	case "svc.start":
		err = s.opts.ServiceLifespan.StartService(*svc)
	case "svc.stop":
		err = s.opts.ServiceLifespan.StopService(svc.ID)
	case "svc.restart":
		err = s.opts.ServiceLifespan.RestartService(*svc)
	}
	if err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.opts.Store.Audit(AuditEntry{
		Actor: "local-cli", Action: "admin.service." + strings.TrimPrefix(req.Action, "svc."),
		Target: svc.Name, Detail: svc.ID, IP: "127.0.0.1", OK: true,
	})
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Services: s.localServicesOf([]Service{*svc})})
}

// localSvcLogs 读取受管服务最近日志（仅受管服务）。
func (s *Server) localSvcLogs(w http.ResponseWriter, req localRequest) {
	if s.opts.ServiceLifespan == nil {
		s.writeLocalError(w, http.StatusServiceUnavailable, "管理端未接入受管服务运行时")
		return
	}
	svc, err := s.resolveLocalService(req.Service)
	if err != nil {
		s.writeLocalError(w, http.StatusNotFound, err.Error())
		return
	}
	if !svc.Managed {
		s.writeLocalError(w, http.StatusBadRequest, "该服务是纯代理型，没有网关侧的日志")
		return
	}
	logs, err := s.opts.ServiceLifespan.Logs(svc.ID, req.Tail)
	if err != nil {
		s.writeLocalError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Logs: logs})
}

// resolveLocalService 按 id 或名字取服务。id 优先：它不会与展示名撞车
// （与 resolveLocalInstance 同一写法）。
func (s *Server) resolveLocalService(ref string) (*Service, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("缺少服务名或 id")
	}
	if ServiceIDPattern.MatchString(ref) {
		if svc, err := s.opts.Store.ServiceByID(ref); err == nil {
			return svc, nil
		}
	}
	svc, err := s.opts.Store.ServiceByName(ref)
	if err != nil {
		return nil, fmt.Errorf("找不到服务 %q", ref)
	}
	return svc, nil
}

// writeLocalJSON 写一个 JSON 响应。
func (s *Server) writeLocalJSON(w http.ResponseWriter, status int, body localResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeLocalError 写一个失败响应。
func (s *Server) writeLocalError(w http.ResponseWriter, status int, message string) {
	s.writeLocalJSON(w, status, localResponse{OK: false, Error: message})
}

// loopbackRequest 判断请求是否来自回环地址。
//
// 只看 TCP 来源，不信任任何转发头：本机 API 不该经过反向代理，X-Forwarded-For
// 谁都能写。
func loopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

// localTokenOK 校验 Authorization: Bearer <token>。
func localTokenOK(r *http.Request, want string) bool {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	got, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(want)) == 1
}

// localSvcDiscover 执行服务发现（mode=local 本机，mode=cidr 网段扫描）。
func (s *Server) localSvcDiscover(w http.ResponseWriter, req localRequest) {
	mode := strings.TrimSpace(req.Mode)
	if mode == "" || mode == "local" {
		cands, err := discover.Local()
		if errors.Is(err, discover.ErrUnsupported) {
			s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Candidates: []localCandidate{}})
			return
		}
		if err != nil {
			s.writeLocalError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.writeLocalCandidates(w, cands)
		return
	}
	ok, err := discover.IsPrivateCIDR(strings.TrimSpace(req.CIDR))
	if err != nil || !ok {
		s.writeLocalError(w, http.StatusBadRequest, "只允许私有网段（10/8、172.16/12、192.168/16）")
		return
	}
	ports, err := discover.ParsePorts(strings.TrimSpace(req.Ports))
	if err != nil {
		s.writeLocalError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cands, serr := discover.Scan(ctx, strings.TrimSpace(req.CIDR), ports, discover.ScanOptions{})
	if serr != nil && len(cands) == 0 {
		s.writeLocalError(w, http.StatusInternalServerError, serr.Error())
		return
	}
	s.writeLocalCandidates(w, cands)
}

func (s *Server) writeLocalCandidates(w http.ResponseWriter, cands []discover.Candidate) {
	reg := s.registeredUpstreams()
	out := make([]localCandidate, 0, len(cands))
	for _, c := range cands {
		up := fmt.Sprintf("http://%s:%d", c.Host, c.Port)
		out = append(out, localCandidate{
			Source: c.Source, Host: c.Host, Port: c.Port,
			Command: c.Command, Title: c.Title, Already: reg[up],
		})
	}
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Candidates: out})
}

// localSvcDiscoverAdd 批量登记候选（纯代理；已登记跳过）。
func (s *Server) localSvcDiscoverAdd(w http.ResponseWriter, req localRequest) {
	if len(req.Candidates) == 0 {
		s.writeLocalError(w, http.StatusBadRequest, "candidates 为空")
		return
	}
	created, skipped := 0, 0
	for _, c := range req.Candidates {
		host := strings.TrimSpace(c.Host)
		if host == "" || c.Port <= 0 || c.Port > 65535 {
			skipped++
			continue
		}
		up := fmt.Sprintf("http://%s:%d", host, c.Port)
		if _, err := s.findServiceByUpstream(up); err == nil {
			skipped++
			continue
		}
		base := strings.TrimSpace(c.Name)
		if base == "" {
			base = fmt.Sprintf("svc-%s-%d", sanitizeNamePart(host), c.Port)
		}
		name := s.uniqueServiceName(base)
		if err := s.opts.Store.CreateService(&Service{Name: name, Upstream: up}); err != nil {
			skipped++
			continue
		}
		created++
	}
	_ = s.opts.Store.Audit(AuditEntry{
		Actor: "local-cli", Action: "admin.service.discover.add",
		Detail: fmt.Sprintf("新建 %d、跳过 %d", created, skipped), OK: true,
	})
	s.writeLocalJSON(w, http.StatusOK, localResponse{OK: true, Created: created, Skipped: skipped})
}
