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

// localResponse 是本机 API 的统一响应体。
type localResponse struct {
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Instances []localInstance `json:"instances,omitempty"`
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
