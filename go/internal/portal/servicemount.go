// 内网服务的子路径挂载与探活（见 docs/service-proxy.md 第 3/5/7/11 节）。
//
// 与实例挂载（server.go 的 handleMount）同一套顺序：**先授权，再建上游**。
// 区别只在两点：
//
//   - 服务是「已经存在、只被登记进来的上游」，门户不拉起进程，上游就是
//     一个指向登记地址的标准库反向代理；
//   - 服务没有令牌交接（那是 dsh 专属），挂载根直接代理。
//
// 授权唯一入口是 Store.ServiceAccessible；未命中与不存在同形 404 并写审计。
// Cookie/Location 改写、WS/SSE 穿透全部复用 mountproxy，本文件不做第二份。
package portal

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/cookie"
	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/mountproxy"
)

// ServiceMountPrefix 是所有服务挂载路径的前缀：/s/<id>/。
const ServiceMountPrefix = "/s/"

// ServicesPagePath 是用户侧「我的服务」独立页面的路径（与首页「我的实例」分开，
// 导航互达；页面实现在 servicepages.go）。
const ServicesPagePath = "/services"

// ServiceProbe 是一次按需探活的结果。健康状态不落库（运行态内存缓存），
// 避免把探活变成写放大。
type ServiceProbe struct {
	Reachable bool
	Status    int
	LatencyMS int64
	Error     string
}

// probeTimeout 是单次探活的总超时：内网服务探活要快进快出，
// 慢上游不能把管理台的状态刷新拖住。
const probeTimeout = 2 * time.Second

// 按 TLS 校验策略分两个共享 Transport（而不是每服务一个）：连接池能复用，
// 豁免校验的影响面也只限于明确勾选了 insecure 的服务。
var (
	serviceTransport         = newServiceTransport(false)
	serviceInsecureTransport = newServiceTransport(true)
)

// newServiceTransport 造服务上游用的 Transport。
//
// 刻意 Proxy=nil：内网上游直连，不走环境里的 HTTP_PROXY——那是给外网请求
// 配的，把内网地址送进代理只会凭空多出故障点。不设响应总超时：WS/SSE 是
// 长连接，总超时会把它们拦腰截断（与实例代理同一约定）。
func newServiceTransport(insecure bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	if insecure {
		// 自签内网服务的显式豁免（登记时勾选，默认校验）；只作用于上游方向。
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- 登记时显式豁免
	}
	return t
}

// handleServiceMount 把一个内网服务挂到 /s/<id>/ 下。
//
// 顺序照 handleMount：requireUser → 路径白名单 → ServiceAccessible（唯一授权
// 入口）→ 未命中写审计 + 404 同形 → Enabled 检查 → 构造/缓存代理 → 进入挂载根
// 时审计 → 转发。未授权的请求连上游代理都不会被构造。
func (s *Server) handleServiceMount(w http.ResponseWriter, r *http.Request) {
	lang := s.lang(w, r)
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}

	id := serviceIDFromPath(r.URL.Path)
	if id == "" {
		s.notFoundPage(w, lang)
		return
	}
	// 类别开关关掉的普通用户：即便手里有 grant 也进不来，与未授权同形 404
	// （管理员不受开关约束，口径与 handleMount 一致）。
	if !s.canSeeServices(user) {
		s.log.Info("拒绝对服务的访问（类别权限）", "user", user.Name, "service", id, "ip", cookie.ClientIP(r))
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: user.Name, Action: "service.denied", Target: id,
			Detail: "未授权或服务不存在", IP: cookie.ClientIP(r), OK: false,
		})
		s.notFoundPage(w, lang)
		return
	}
	svc, err := s.opts.Store.ServiceAccessible(user, id)
	if errors.Is(err, ErrNotFound) {
		// 越权与不存在同形：不让路径变成「服务是否存在」的探测器。
		s.log.Info("拒绝对服务的访问", "user", user.Name, "service", id, "ip", cookie.ClientIP(r))
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: user.Name, Action: "service.denied", Target: id,
			Detail: "未授权或服务不存在", IP: cookie.ClientIP(r), OK: false,
		})
		s.notFoundPage(w, lang)
		return
	}
	if err != nil {
		s.log.Error("查询服务失败", "service", id, "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	if !svc.Enabled {
		s.notFoundPage(w, lang)
		return
	}

	proxy, err := s.serviceMountFor(*svc)
	if err != nil {
		s.log.Error("构造服务挂载失败", "service", id, "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrUnavailable), http.StatusServiceUnavailable)
		return
	}

	// 只在「进入服务」这一跳记审计：挂载下的每个静态资源都记会把审计淹掉。
	if r.URL.Path == ServiceMountPrefix+id || r.URL.Path == ServiceMountPrefix+id+"/" {
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: user.Name, Action: "service.enter", Target: svc.Name,
			Detail: id, IP: cookie.ClientIP(r), OK: true,
		})
	}
	proxy.ServeHTTP(w, r)
}

// serviceMountFor 返回（必要时构造并缓存）某服务的挂载代理。
//
// 与 mountFor 同一思路：缓存「上游代理 + 前缀」这一层，Secure 判定每次请求
// 实时算。服务被 update/remove/toggle 后由管理端调 invalidateServiceMount 失效。
func (s *Server) serviceMountFor(svc Service) (*mountproxy.Proxy, error) {
	s.svcMu.Lock()
	cached := s.svcMounts[svc.ID]
	s.svcMu.Unlock()
	if cached != nil {
		return cached, nil
	}

	target, err := url.Parse(svc.Upstream)
	if err != nil {
		return nil, fmt.Errorf("服务 %s 的上游地址非法: %w", svc.ID, err)
	}
	transport := serviceTransport
	if svc.InsecureTLS {
		transport = serviceInsecureTransport
	}
	reverse := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// mountproxy 已剥掉 /s/<id> 前缀；上游自带 base path 时拼回去。
			req.URL.Path = joinUpstreamPath(target.Path, req.URL.Path)
			req.URL.RawPath = ""
			// Host 策略：upstream 用上游自己的主机名（虚拟主机路由与证书校验
			// 都按它来）；gateway 保留浏览器看到的门户 authority 原样。
			if svc.HostHeader != HostHeaderGateway {
				req.Host = target.Host
			}
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("服务上游请求失败", "service", svc.ID, "err", err)
			http.Error(w, i18n.T(s.lang(w, r), i18n.KeyPortalErrUnavailable), http.StatusBadGateway)
		},
	}
	proxy, err := mountproxy.New(mountproxy.Options{
		Prefix:   ServiceMountPrefix + svc.ID,
		Upstream: reverse,
		Secure:   requestIsHTTPS,
	})
	if err != nil {
		return nil, err
	}
	s.svcMu.Lock()
	if s.svcMounts == nil {
		s.svcMounts = map[string]*mountproxy.Proxy{}
	}
	s.svcMounts[svc.ID] = proxy
	s.svcMu.Unlock()
	return proxy, nil
}

// invalidateServiceMount 删掉某服务的代理缓存与探活缓存。
// 服务 update / remove / toggle 之后由管理端调用，下一次请求按新定义重建。
func (s *Server) invalidateServiceMount(id string) {
	s.svcMu.Lock()
	delete(s.svcMounts, id)
	delete(s.svcProbes, id)
	s.svcMu.Unlock()
}

// probeService 真探活：HTTP GET 上游 base（没配 base 就是 /），2s 超时。
// 任何 <500 的状态码都算 reachable（3xx 重定向与 401/404 都说明上游活着）；
// 连接错误记进 Error。不跟随重定向：探的是登记的那个上游，不是它指去的地方。
func (s *Server) probeService(ctx context.Context, svc Service) ServiceProbe {
	target, err := url.Parse(svc.Upstream)
	if err != nil {
		return ServiceProbe{Error: fmt.Sprintf("上游地址非法: %v", err)}
	}
	if target.Path == "" {
		target.Path = "/"
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return ServiceProbe{Error: err.Error()}
	}
	transport := serviceTransport
	if svc.InsecureTLS {
		transport = serviceInsecureTransport
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return ServiceProbe{LatencyMS: time.Since(start).Milliseconds(), Error: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	probe := ServiceProbe{
		Reachable: resp.StatusCode < 500,
		Status:    resp.StatusCode,
		LatencyMS: time.Since(start).Milliseconds(),
	}
	if !probe.Reachable {
		probe.Error = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
	}
	return probe
}

// cachedServiceProbe 读探活缓存；无缓存返回 false。
func (s *Server) cachedServiceProbe(id string) (ServiceProbe, bool) {
	s.svcMu.Lock()
	defer s.svcMu.Unlock()
	p, ok := s.svcProbes[id]
	return p, ok
}

// storeServiceProbe 写探活缓存（管理台状态徽标与 CLI probe 共用）。
func (s *Server) storeServiceProbe(id string, p ServiceProbe) {
	s.svcMu.Lock()
	if s.svcProbes == nil {
		s.svcProbes = map[string]ServiceProbe{}
	}
	s.svcProbes[id] = p
	s.svcMu.Unlock()
}

// serviceIDFromPath 从 /s/<id>/... 里取出服务 id。取不到返回空串。
// 白名单先于查表执行，路径穿越与奇怪字符在这里就被挡掉。
func serviceIDFromPath(path string) string {
	rest := strings.TrimPrefix(path, ServiceMountPrefix)
	if rest == path {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if !ServiceIDPattern.MatchString(rest) {
		return ""
	}
	return rest
}

// joinUpstreamPath 把上游 base path 拼到剥前缀后的请求路径前。
// base 为空（多数服务）时原样返回；斜杠归一，不会出现双斜杠或缺斜杠。
func joinUpstreamPath(base, path string) string {
	if base == "" || base == "/" {
		return path
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}
