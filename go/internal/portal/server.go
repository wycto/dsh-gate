// 管理端的 HTTP 层：登录（复用 internal/auth）、「我的实例」、实例挂载与管理 API。
//
// 分层：
//
//	auth.Gate      回答「这个人登录了没有」——登录页、限速、会话 Cookie 全部复用，
//	               管理端只是换了一份账号表（SQLite）。
//	portal.Server  回答「这个人能看/能进哪些实例」——按会话里的登录名查 grants，
//	               未命中一律 404（与「实例不存在」同形，不泄漏存在性）。
//	mountproxy     回答「怎么把上游 dsh 挂到 /i/<id>/ 下」——剥前缀、改写 Cookie 路径。
//
// 三者互不越界：Server 不解析 Cookie 之外的请求语义，mountproxy 不做授权判定。
package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wycto/dsh-gate/internal/assets"
	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/cookie"
	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/mountproxy"
)

// MountPrefix 是所有实例挂载路径的前缀：/i/<id>/。
const MountPrefix = "/i/"

// APIMePath 是「当前用户与可见实例」的 JSON 端点。
//
// 刻意**不**放在 `/__dsh_gate/` 下：那个前缀是鉴权网关的私有区，它对自己不认识的
// `/__dsh_gate/*` 一律回 404（避免未知路径被转发到 dsh）。管理端的 API 与实例挂载
// 同属管理端自己的路由空间，用一个独立前缀更清楚，也不会与 dsh 的既有路由冲突。
const APIMePath = "/_portal/api/me"

// ServerOptions 是管理端 HTTP 层的配置。
type ServerOptions struct {
	// Store 是管理端持久层（账号、实例、授权、审计）。
	Store *Store
	// Gate 是复用的鉴权网关；管理端直接把它套在自己的处理器外面。
	Gate *auth.Gate
	// Upstream 为某个实例返回上游处理器（通常是指向该实例 dsh 回环端口的反向代理）。
	// 返回 nil 表示该实例暂时接不上（页面给出「暂不可用」）。字段本身为 nil 时
	// 所有实例挂载都返回「暂不可用」——管理端还没接运行环境时就是这样。
	Upstream func(inst Instance) (http.Handler, error)
	// Version 显示在页面页脚。
	Version string
	// Installer 提供 dsh 环境检测与安装能力（管理台的「创建实例」用它）。
	// 为 nil 时管理台不显示安装面板——单实例模式、或没接运行环境时就是这样。
	Installer Installer
	// Lifespan 提供单个实例的启停（管理台的启动 / 停止 / 重启按钮用它）。
	// 为 nil 时不显示这些按钮：没有运行时句柄就没法真的去拉进程，
	// 摆一个按了没反应的按钮比不摆更糟。
	Lifespan Lifespan
	// Logger 收日志；为 nil 时丢弃。
	Logger *slog.Logger
	// LocalToken 是本机控制 API 的共享密钥（CLI 从 <状态目录>/portal.token 读）。
	// 为空表示不开放本机控制入口——单测与嵌入式使用时不需要它。
	LocalToken string
	// CookiePort 是管理端监听端口：CSRF Cookie 名带 _p<端口> 后缀，与 Gate 的
	// 会话 Cookie 同一口径（同机多网关隔离，见 cookie.ScopeName）。0 不加后缀。
	CookiePort int
	// APIRateLimitPerMin 是外部 API 每个密钥的每分钟请求上限；0 用默认 60。
	APIRateLimitPerMin int
	// ServiceLifespan 提供「受管服务」的启停/状态/日志（P1）。为 nil 时不支持受管
	// 服务的生命周期操作（纯代理服务不受影响）。
	ServiceLifespan ServiceLifespan
}

// ServiceRunStatus 是受管服务的一次运行状态快照（运行态不落库，内存态）。
type ServiceRunStatus struct {
	Running   bool
	PID       int
	State     string
	StartedAt time.Time
	LastError string
}

// ServiceLifespan 抽象「受管服务」的进程生命周期，由 portalruntime 用 internal/svclife
// 实现、在装配处注入。管理端与对外 API 只依赖这个窄接口，不直接碰 svclife。
type ServiceLifespan interface {
	StartService(svc Service) error
	StopService(id string) error
	RestartService(svc Service) error
	Status(id string) ServiceRunStatus
	Logs(id string, tail int) (string, error)
}

// Server 是管理端的 HTTP 处理器。
type Server struct {
	opts      ServerOptions
	log       *slog.Logger
	page      *template.Template
	adminPage *template.Template
	// liveScript 是管理台页面内联的自动刷新脚本，liveCSP 是放行它的完整
	// Content-Security-Policy（script-src 里带着脚本内容的 sha256）。
	liveScript template.JS
	liveCSP    string

	mu     sync.Mutex
	mounts map[string]*mountproxy.Proxy

	// 服务挂载与探活缓存（servicemount.go）：与实例的 mounts 分开，
	// 惰性初始化，零值 Server（单测直接构造）也能用。
	svcMu     sync.Mutex
	svcMounts map[string]*mountproxy.Proxy
	svcProbes map[string]ServiceProbe
	// csrfCookie 是带端口后缀的 CSRF Cookie 名（同机多网关隔离）。
	csrfCookie string
	// apiLimiter 是外部 API 的每密钥限速器（见 api.go）。
	apiLimiter *apiRateLimiter
}

// NewServer 构造管理端 HTTP 层。模板解析失败会在这里就报错（而不是第一次访问才炸）。
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("管理端需要持久层")
	}
	if opts.Gate == nil {
		return nil, errors.New("管理端需要鉴权网关")
	}
	tmpl, err := assets.PortalTemplate()
	if err != nil {
		return nil, err
	}
	adminTmpl, err := assets.AdminTemplate()
	if err != nil {
		return nil, err
	}
	liveScript, liveHash, err := assets.AdminLiveScript()
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		opts: opts, log: logger,
		page: tmpl, adminPage: adminTmpl,
		liveScript: template.JS(liveScript),
		// 管理台与登录页的差别只有这三条：放行这一个脚本的哈希、让它访问
		// 同源的实时状态接口，以及允许导航图标用的 data: 遮罩图（与登录页、
		// 管理端首页同一份规则）。别的来源、别的脚本仍然是 default-src 'none'。
		liveCSP: "default-src 'none'; style-src 'unsafe-inline'; img-src data:; script-src '" + liveHash +
			"'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
		mounts:     map[string]*mountproxy.Proxy{},
		csrfCookie: cookie.ScopeName(CSRFCookie, opts.CookiePort),
		apiLimiter: newAPIRateLimiter(opts.APIRateLimitPerMin),
	}, nil
}

// Handler 返回管理端的完整处理器：鉴权在最外层，未登录的请求根本进不到路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(LocalAPIPath, s.handleLocalAPI)
	mux.HandleFunc(APIMePath, s.handleAPIMe)
	mux.HandleFunc(AdminStatePath, s.handleAdminJSON)
	mux.HandleFunc(AdminActionPath, s.handleAdminJSON)
	mux.HandleFunc(AdminLivePath, s.handleAdminJSON)
	mux.HandleFunc(MountPrefix, s.handleMount)
	mux.HandleFunc(ServiceMountPrefix, s.handleServiceMount)
	mux.HandleFunc(ServicesPagePath, s.handleServicesPage)
	mux.HandleFunc(AdminPath, s.handleAdmin)
	mux.HandleFunc(AdminPath+"/", s.handleAdmin)
	mux.HandleFunc("/", s.handleIndex)
	// 本机控制 API 走 token 鉴权、不过登录门：它是给本机 CLI 用的，CLI 没有
	// 管理员口令也会调它；拒绝非回环来源与错 token 都在处理函数里做。
	guarded := s.opts.Gate.Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 外部 API 走 API Key 鉴权、不过登录门，且必须在 Gate 之前分流
		// （否则未登录的 API 调用会被重定向去登录页）。
		if apiPathMatches(r.URL.Path) {
			s.handleExternalAPI(w, r)
			return
		}
		if r.URL.Path == LocalAPIPath {
			s.handleLocalAPI(w, r)
			return
		}
		guarded.ServeHTTP(w, r)
	})
}

// AdminEnabled 报告管理台是否可用。管理端自带管理台，因此恒为 true——
// 保留这个方法是为了让首页模板的 `{{if .IsAdmin}}` 有一个明确的判断来源。
func (s *Server) AdminEnabled() bool { return s.adminPage != nil }

// requireUser 取出当前登录用户。
//
// 会话有效但账号已被删除 / 停用时（管理员在别处操作的结果），就地作废会话并
// 送去登录页：否则这个会话会一直「有效」却什么都做不了。
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (*User, bool) {
	token := cookie.Get(r, s.opts.Gate.SessionCookieName())
	name, ok := s.opts.Gate.Sessions().Subject(token)
	if !ok {
		s.sendToLogin(w, r)
		return nil, false
	}
	user, err := s.opts.Store.UserByName(name)
	if err != nil || user.Disabled {
		s.opts.Gate.Sessions().Destroy(token)
		s.sendToLogin(w, r)
		return nil, false
	}
	return user, true
}

// sendToLogin 送去登录页，并清掉已经无效的会话 Cookie（避免来回跳）。
func (s *Server) sendToLogin(w http.ResponseWriter, r *http.Request) {
	cookie.Clear(w, s.opts.Gate.SessionCookieName(), cookie.Options{Secure: requestIsHTTPS(r)})
	target := auth.PathLogin
	if next := r.URL.RequestURI(); next != "" && next != "/" {
		target += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// portalInstanceCard 是首页「我的实例」里的一张卡片。
type portalInstanceCard struct {
	ID   string
	Name string
	Href string
}

// portalPageData 是门户页骨架（portal.html）的渲染数据：「我的实例」与
// 「没有任何访问权限」页共用同一份模板，后者只是 NoAccess 非空、卡片为空。
type portalPageData struct {
	Lang          string
	Title         string
	SignedInAs    string
	AdminLabel    string
	SignOut       string
	Enter         string
	NoInstances   string
	ServicesLabel string
	FooterPrefix  string
	Version       string
	Host          string
	IsAdmin       bool
	Instances     []portalInstanceCard
	OtherLang     string
	OtherLangHref string
	SignOutHref   string
	// NoAccess 非空时渲染「没有任何访问权限」提示而不是实例卡片：
	// 两类开关都关掉的普通用户落到这里（见 handleIndex / handleServicesPage）。
	NoAccess string
	// ShowInstances / ShowServices 决定页头导航链接是否出现：类别开关关掉的
	// 那一类入口整体消失（管理员恒 true）。模板据此条件渲染互达链接。
	ShowInstances bool
	ShowServices  bool
	// 配色与布局（与登录页 / 管理台同一套模型：明暗三态 + 方案四选一 + 布局三选一）。
	Theme       string
	ThemeLabel  string
	Themes      []auth.ThemeOption
	Scheme      string
	SchemeLabel string
	Schemes     []auth.ThemeOption
	Style       string
	StyleLabel  string
	Styles      []auth.ThemeOption
}

// globalFeatures 读门户两个全局总开关；读失败按开启处理（退化为仅每用户开关，
// 不因一次瞬时 DB 错误把所有人锁死）。settings 表迁移后恒存在，错误属异常。
func (s *Server) globalFeatures() (instances, services bool) {
	inst, svc, err := s.opts.Store.PortalFeatures()
	if err != nil {
		s.log.Error("读取门户总开关失败，按开启处理", "err", err)
		return true, true
	}
	return inst, svc
}

// canSeeInstances / canSeeServices 是可见性的唯一定义点：
// 最终可见性 = 全局总开关 AND（管理员 OR 用户类别开关）。全局关掉某类时
// 所有人（含管理员）都看不到该类；管理员不受每用户开关约束，但受全局约束。
// 导航、页面重定向、挂载拦截、me API 与管理台分页都走这里，口径不会漂移。
func (s *Server) canSeeInstances(u *User) bool {
	inst, _ := s.globalFeatures()
	return inst && (u.IsAdmin() || u.AllowInstances)
}
func (s *Server) canSeeServices(u *User) bool {
	_, svc := s.globalFeatures()
	return svc && (u.IsAdmin() || u.AllowServices)
}

// pageTitle 给页面标题加上自定义站点名称前缀：管理多台设备上的网关时，
// 浏览器标签页一眼能分出「这是哪台」（如「家里 NAS · 管理控制台」）。
// 未设置或读失败都返回原文案：一次瞬时 DB 错误不该改变页面标题。
// 登录页不走这里——它走 auth 的 TitleFunc（登录页大标题也要跟着变）。
func (s *Server) pageTitle(lang i18n.Lang, key string) string {
	title := i18n.T(lang, key)
	site, err := s.opts.Store.SiteTitle()
	if err != nil {
		s.log.Error("读取站点名称失败，按默认标题处理", "err", err)
		return title
	}
	if site == "" {
		return title
	}
	return site + " · " + title
}

// handleIndex 渲染「我的实例」。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	lang := s.lang(w, r)
	theme := s.opts.Gate.Theme(w, r)
	scheme := s.opts.Gate.Scheme(w, r)
	style := s.opts.Gate.Style(w, r)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, i18n.T(lang, i18n.KeyLoginErrMethod), http.StatusMethodNotAllowed)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// 类别开关：实例入口整体关掉时，首页不再是他的落地页——还有服务入口就
	// 送过去，两类都没有就给无权限页（管理员不受开关约束，canSee* 恒真）。
	if !s.canSeeInstances(user) {
		if s.canSeeServices(user) {
			http.Redirect(w, r, ServicesPagePath, http.StatusSeeOther)
			return
		}
		s.renderNoAccess(w, r, lang, user)
		return
	}

	instances, err := s.opts.Store.VisibleInstances(user)
	if err != nil {
		s.log.Error("读取可见实例失败", "user", user.Name, "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}

	cards := make([]portalInstanceCard, 0, len(instances))
	for _, inst := range instances {
		cards = append(cards, portalInstanceCard{ID: inst.ID, Name: inst.Name, Href: MountPrefix + inst.ID + "/"})
	}

	data := portalPageData{
		Lang:          string(lang),
		Title:         s.pageTitle(lang, i18n.KeyPortalTitle),
		SignedInAs:    i18n.T(lang, i18n.KeyPortalSignedInAs, displayNameOf(user)),
		AdminLabel:    i18n.T(lang, i18n.KeyPortalAdmin),
		SignOut:       i18n.T(lang, i18n.KeyPortalSignOut),
		Enter:         i18n.T(lang, i18n.KeyPortalEnter),
		NoInstances:   i18n.T(lang, i18n.KeyPortalNoInstances),
		ServicesLabel: i18n.T(lang, i18n.KeyPortalServices),
		FooterPrefix:  i18n.T(lang, i18n.KeyLoginFooterPrefix),
		Version:       s.version(),
		Host:          displayHost(r, lang),
		IsAdmin:       user.IsAdmin() && s.AdminEnabled(),
		Instances:     cards,
		ShowInstances: s.canSeeInstances(user),
		ShowServices:  s.canSeeServices(user),
		OtherLang:     otherLangLabel(lang),
		OtherLangHref: "?lang=" + string(otherLang(lang)),
		SignOutHref:   auth.PathLogout,
		Theme:         string(theme),
		ThemeLabel:    i18n.T(lang, i18n.KeyThemeLabel),
		Themes:        auth.ThemeOptions(r, theme, lang),
		Scheme:        string(scheme),
		SchemeLabel:   i18n.T(lang, i18n.KeySchemeLabel),
		Schemes:       auth.SchemeOptions(r, scheme, lang),
		Style:         string(style),
		StyleLabel:    i18n.T(lang, i18n.KeyStyleLabel),
		Styles:        auth.StyleOptions(r, style, lang),
	}

	body := &strings.Builder{}
	if err := s.page.Execute(body, data); err != nil {
		s.log.Error("渲染管理端首页失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	// 页面只有内联样式，没有脚本、没有外部资源（与登录页同一套 CSP 口径）。
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body.String()))
}

// renderNoAccess 渲染「没有任何访问权限」页：两类开关都关掉的普通用户
// 访问首页或服务页时落到这里。复用门户页骨架（同一套配色 / 语言 / 安全头），
// 只是卡片为空、NoAccess 提示非空、两类导航链接都不出现。管理员恒见全部，
// 不会走到这里。
func (s *Server) renderNoAccess(w http.ResponseWriter, r *http.Request, lang i18n.Lang, user *User) {
	theme := s.opts.Gate.Theme(w, r)
	scheme := s.opts.Gate.Scheme(w, r)
	style := s.opts.Gate.Style(w, r)
	data := portalPageData{
		Lang:          string(lang),
		Title:         s.pageTitle(lang, i18n.KeyPortalNoAccessTitle),
		SignedInAs:    i18n.T(lang, i18n.KeyPortalSignedInAs, displayNameOf(user)),
		AdminLabel:    i18n.T(lang, i18n.KeyPortalAdmin),
		SignOut:       i18n.T(lang, i18n.KeyPortalSignOut),
		Enter:         i18n.T(lang, i18n.KeyPortalEnter),
		NoInstances:   i18n.T(lang, i18n.KeyPortalNoInstances),
		ServicesLabel: i18n.T(lang, i18n.KeyPortalServices),
		FooterPrefix:  i18n.T(lang, i18n.KeyLoginFooterPrefix),
		Version:       s.version(),
		Host:          displayHost(r, lang),
		NoAccess:      i18n.T(lang, i18n.KeyPortalNoAccess),
		OtherLang:     otherLangLabel(lang),
		OtherLangHref: "?lang=" + string(otherLang(lang)),
		SignOutHref:   auth.PathLogout,
		Theme:         string(theme),
		ThemeLabel:    i18n.T(lang, i18n.KeyThemeLabel),
		Themes:        auth.ThemeOptions(r, theme, lang),
		Scheme:        string(scheme),
		SchemeLabel:   i18n.T(lang, i18n.KeySchemeLabel),
		Schemes:       auth.SchemeOptions(r, scheme, lang),
		Style:         string(style),
		StyleLabel:    i18n.T(lang, i18n.KeyStyleLabel),
		Styles:        auth.StyleOptions(r, style, lang),
	}
	body := &strings.Builder{}
	if err := s.page.Execute(body, data); err != nil {
		s.log.Error("渲染无权限页失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body.String()))
}

// handleAPIMe 返回当前用户与可见实例（页面脚本与外部工具都用它）。
func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) {
	lang := s.lang(w, r)
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, i18n.T(lang, i18n.KeyLoginErrMethod))
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// 类别开关关掉的那一类：列表直接给空（入口整体消失的口径与页面一致），
	// 管理员恒见全部。布尔字段让外部工具不用猜空列表是「没授权」还是「没开关」。
	allowInstances := s.canSeeInstances(user)
	allowServices := s.canSeeServices(user)
	var instances []Instance
	if allowInstances {
		var err error
		instances, err = s.opts.Store.VisibleInstances(user)
		if err != nil {
			s.log.Error("读取可见实例失败", "user", user.Name, "err", err)
			writeJSONError(w, http.StatusInternalServerError, i18n.T(lang, i18n.KeyPortalErrInternal))
			return
		}
	}
	type instanceJSON struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	out := make([]instanceJSON, 0, len(instances))
	for _, inst := range instances {
		out = append(out, instanceJSON{ID: inst.ID, Name: inst.Name, URL: MountPrefix + inst.ID + "/"})
	}
	var services []Service
	if allowServices {
		var err error
		services, err = s.opts.Store.VisibleServices(user)
		if err != nil {
			s.log.Error("读取可见服务失败", "user", user.Name, "err", err)
			writeJSONError(w, http.StatusInternalServerError, i18n.T(lang, i18n.KeyPortalErrInternal))
			return
		}
	}
	type serviceJSON struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	svcOut := make([]serviceJSON, 0, len(services))
	for _, svc := range services {
		svcOut = append(svcOut, serviceJSON{ID: svc.ID, Name: svc.Name, URL: ServiceMountPrefix + svc.ID + "/"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"user": map[string]any{
			"name":        user.Name,
			"displayName": displayNameOf(user),
			"role":        string(user.Role),
		},
		"instances":      out,
		"services":       svcOut,
		"allowInstances": allowInstances,
		"allowServices":  allowServices,
	})
}

// handleMount 把一个实例挂到 /i/<id>/ 下。
//
// 顺序是刻意的：**先授权，再建上游**。未授权的请求连上游处理器都不会被构造，
// 也就不存在「构造过程中做了不该做的事」这种可能。
func (s *Server) handleMount(w http.ResponseWriter, r *http.Request) {
	lang := s.lang(w, r)
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}

	id := instanceIDFromPath(r.URL.Path)
	if id == "" {
		s.notFoundPage(w, lang)
		return
	}
	// 类别开关关掉的普通用户：即便手里有 grant 也进不来，与未授权同形 404，
	// 不让挂载路径变成「实例是否存在」的探测器（管理员不受开关约束）。
	if !s.canSeeInstances(user) {
		s.log.Info("拒绝对实例的访问（类别权限）", "user", user.Name, "instance", id, "ip", cookie.ClientIP(r))
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: user.Name, Action: "instance.denied", Target: id,
			Detail: "未授权或实例不存在", IP: cookie.ClientIP(r), OK: false,
		})
		s.notFoundPage(w, lang)
		return
	}
	inst, err := s.opts.Store.Accessible(user, id)
	if errors.Is(err, ErrNotFound) {
		// 越权与不存在同形：不让路径变成「实例是否存在」的探测器。
		s.log.Info("拒绝对实例的访问", "user", user.Name, "instance", id, "ip", cookie.ClientIP(r))
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: user.Name, Action: "instance.denied", Target: id,
			Detail: "未授权或实例不存在", IP: cookie.ClientIP(r), OK: false,
		})
		s.notFoundPage(w, lang)
		return
	}
	if err != nil {
		s.log.Error("查询实例失败", "instance", id, "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	if !inst.Enabled {
		s.notFoundPage(w, lang)
		return
	}

	proxy, err := s.mountFor(*inst)
	if err != nil {
		s.log.Error("构造实例挂载失败", "instance", id, "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrUnavailable), http.StatusServiceUnavailable)
		return
	}
	if proxy == nil {
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrUnavailable), http.StatusServiceUnavailable)
		return
	}

	// 只在「进入实例」这一跳记审计：挂载下的每个静态资源都记会把审计淹掉。
	if r.URL.Path == MountPrefix+id || r.URL.Path == MountPrefix+id+"/" {
		_ = s.opts.Store.Audit(AuditEntry{
			Actor: user.Name, Action: "instance.enter", Target: inst.Name,
			Detail: id, IP: cookie.ClientIP(r), OK: true,
		})
	}
	proxy.ServeHTTP(w, r)
}

// mountFor 返回（必要时构造并缓存）某实例的挂载代理。
//
// 缓存的是「上游处理器 + 前缀」这一层；Secure 判定是每次请求实时算的，所以
// 同一个实例被 http 与 https 同时访问也不会给出错误答案。
func (s *Server) mountFor(inst Instance) (*mountproxy.Proxy, error) {
	if s.opts.Upstream == nil {
		return nil, nil
	}
	s.mu.Lock()
	cached := s.mounts[inst.ID]
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	upstream, err := s.opts.Upstream(inst)
	if err != nil {
		return nil, fmt.Errorf("实例 %s 的上游不可用: %w", inst.ID, err)
	}
	if upstream == nil {
		return nil, nil
	}
	proxy, err := mountproxy.New(mountproxy.Options{
		Prefix:   MountPrefix + inst.ID,
		Upstream: upstream,
		Secure:   requestIsHTTPS,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.mounts[inst.ID] = proxy
	s.mu.Unlock()
	return proxy, nil
}

// notFoundPage 用统一文案回答「实例不存在 / 无权限」。
//
// 刻意不用 dsh 的 404 页面：那需要构造上游，而这一步我们连上游都不该碰。
func (s *Server) notFoundPage(w http.ResponseWriter, lang i18n.Lang) {
	http.Error(w, i18n.T(lang, i18n.KeyPortalErrNotFound), http.StatusNotFound)
}

// lang 复用鉴权网关的语言判定（同一套优先级与同一个偏好 Cookie）。
func (s *Server) lang(w http.ResponseWriter, r *http.Request) i18n.Lang {
	return s.opts.Gate.Lang(w, r)
}

func (s *Server) version() string {
	if strings.TrimSpace(s.opts.Version) == "" {
		return "dev"
	}
	return s.opts.Version
}

// ---- 小工具 ----

// instanceIDFromPath 从 `/i/<id>/...` 里取出实例 id。取不到返回空串。
func instanceIDFromPath(path string) string {
	rest := strings.TrimPrefix(path, MountPrefix)
	if rest == path {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if !InstanceIDPattern.MatchString(rest) {
		return ""
	}
	return rest
}

// requestIsHTTPS 判断浏览器这一侧是不是 HTTPS：直接 TLS，或者上游代理明确
// 声明了 X-Forwarded-Proto: https（此时管理端自己通常是明文监听）。
func requestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

func displayNameOf(u *User) string {
	if u == nil {
		return ""
	}
	if strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return u.Name
}

func displayHost(r *http.Request, lang i18n.Lang) string {
	if host := r.Host; host != "" {
		return host
	}
	return i18n.T(lang, i18n.KeyHostLocal)
}

// otherLang 返回另一种语言（用于语言切换链接）。
func otherLang(lang i18n.Lang) i18n.Lang {
	if lang == i18n.EN {
		return i18n.ZH
	}
	return i18n.EN
}

// otherLangLabel 是切换链接上的文字：显示**另一种语言自己的名字**，
// 因此它不翻译（中文用户看到 English，英文用户看到中文）。
func otherLangLabel(lang i18n.Lang) string {
	if lang == i18n.EN {
		return "中文"
	}
	return "English"
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}
