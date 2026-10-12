// Package auth 实现网关的登录鉴权：会话 Cookie、按 IP 的失败限速，以及
// 登录/登出/健康检查三个自有端点。
//
// 分工边界：auth 只回答「这个人是不是允许访问网关」，至于放行之后请求去
// 哪里，那是 proxy 包的事。这样两层互不依赖，也让「换掉登录页」或「换掉
// 反代实现」都只影响一个包。
package auth

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/assets"
	"github.com/wycto/dsh-gate/internal/cookie"
	"github.com/wycto/dsh-gate/internal/users"
)

// 网关自有端点的路径前缀。
//
// 刻意用一个又长又带下划线的前缀，而不是 /login：dsh 的前端会请求一整套
// /api/... 路由，我们要保证任何可能与 dsh 冲突的短路径都不会被网关吃掉。
const (
	PathPrefix   = "/__dsh_gate"
	PathLogin    = PathPrefix + "/login"
	PathLogout   = PathPrefix + "/logout"
	PathHealth   = PathPrefix + "/healthz"
	PathShutdown = PathPrefix + "/shutdown"
	PathActions  = PathPrefix + "/"
)

// maxFormBytes 限制登录表单的请求体大小，防止有人 POST 一个 1GB 的表单把
// 网关的内存吃光。真实的登录表单只有几十字节。
const maxFormBytes = 8 << 10

// Options 是鉴权网关的配置。
type Options struct {
	// Users 是账号表。
	Users *users.Store
	// SessionTTL 是登录会话有效期。
	SessionTTL time.Duration
	// MaxFails / LockoutBase 是登录失败限速参数。
	MaxFails    int
	LockoutBase time.Duration
	// CookieSecure 表示网关跑在 HTTPS 上，会话 Cookie 需要 Secure 标记。
	CookieSecure bool
	// Title / Subtitle 是登录页上显示的文字。
	Title    string
	Subtitle string
	// Version 是页脚显示的版本号。
	Version string
	// Ready 报告上游 dsh 是否就绪，供健康检查端点使用。
	Ready func() bool
	// Shutdown 收到停机请求时调用它。为 nil 时不提供停机端点。
	Shutdown func()
	// Logger 收日志；为 nil 时丢弃全部记录。
	Logger *slog.Logger
}

// Gate 是鉴权网关。
type Gate struct {
	opts     Options
	sessions *Sessions
	limiter  *Limiter
	tmpl     *template.Template
	log      *slog.Logger
}

// New 构造鉴权网关。
func New(opts Options) (*Gate, error) {
	if opts.Users == nil {
		return nil, errors.New("鉴权网关需要账号表")
	}
	if opts.Title == "" {
		opts.Title = "DSH Gateway"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	tmpl, err := assets.LoginTemplate()
	if err != nil {
		return nil, err
	}
	return &Gate{
		opts:     opts,
		sessions: NewSessions(opts.SessionTTL),
		limiter:  NewLimiter(opts.MaxFails, opts.LockoutBase),
		tmpl:     tmpl,
		log:      opts.Logger,
	}, nil
}

// Sessions 暴露会话表，便于登出时精确作废。
func (g *Gate) Sessions() *Sessions { return g.sessions }

// HasUsers 报告账号表是否非空。
//
// 空账号表意味着「谁都进不来」，启动流程据此 fail closed：宁可拒绝启动并
// 打印创建账号的命令，也不要起一个看起来正常、实际把所有人都挡在门外的进程。
func (g *Gate) HasUsers() bool { return g.opts.Users.Count() > 0 }

// Handler 把鉴权套在下游 handler 外面。
//
// 网关自有端点（登录/登出/健康检查）在这里就地处理，其余请求必须带有效会话
// 才会转交下游。
func (g *Gate) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case PathLogin:
			g.handleLogin(w, r)
			return
		case PathLogout:
			g.handleLogout(w, r)
			return
		case PathHealth:
			g.handleHealth(w, r)
			return
		case PathShutdown:
			g.handleShutdown(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, PathActions) {
			http.NotFound(w, r)
			return
		}

		if g.sessions.Valid(cookie.Get(r, cookie.Session)) {
			next.ServeHTTP(w, r)
			return
		}

		// 未登录：页面导航送去登录页，其它方法直接 401。
		//
		// 不用 401 去打扰浏览器的自动重定向（那是给 XHR 用的），也不对 POST
		// 做 302 —— 302 会把 POST 变成 GET，语义就丢了。
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, g.loginURL(r), http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `FormLogin realm="dsh-gate"`)
		http.Error(w, "尚未登录，请先访问 "+g.loginURL(r), http.StatusUnauthorized)
	})
}

// handleLogin 同时处理登录页的展示与提交。
func (g *Gate) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		g.renderLogin(w, r, http.StatusOK, "")
	case http.MethodPost:
		g.handleLoginPost(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "不支持的方法", http.StatusMethodNotAllowed)
	}
}

// handleLoginPost 校验口令并建立会话。
func (g *Gate) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := cookie.ClientIP(r)
	if wait, ok := g.limiter.Allow(ip); !ok {
		g.renderLogin(w, r, http.StatusTooManyRequests,
			"尝试次数过多，请在 "+humanDuration(wait)+"后再试")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		g.renderLogin(w, r, http.StatusBadRequest, "请求格式有误")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	// 账号不存在与口令错误返回同一句话：把区别讲出来等于提供账号枚举接口。
	if !g.opts.Users.Verify(name, password) {
		wait := g.limiter.Fail(ip)
		g.log.Warn("登录失败", "ip", ip, "user", name, "locked", wait > 0)
		g.renderLogin(w, r, http.StatusUnauthorized, "用户名或口令不正确")
		return
	}

	g.limiter.Reset(ip)
	token := g.sessions.Create()
	cookie.Set(w, cookie.Session, token, cookie.Options{
		Secure: g.opts.CookieSecure,
		MaxAge: g.opts.SessionTTL,
	})
	g.log.Info("登录成功", "ip", ip, "user", name)

	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handleLogout 作废会话并回到登录页。
func (g *Gate) handleLogout(w http.ResponseWriter, r *http.Request) {
	g.sessions.Destroy(cookie.Get(r, cookie.Session))
	cookie.Clear(w, cookie.Session, cookie.Options{Secure: g.opts.CookieSecure})
	// 连同 dsh 的令牌交接标记一起清掉：登出后再进来必须重新走一次交接，
	// 否则会出现「网关认为已交接、dsh 那边却没有会话」的错位状态。
	cookie.Clear(w, cookie.Handoff, cookie.Options{Secure: g.opts.CookieSecure})
	http.Redirect(w, r, PathLogin, http.StatusSeeOther)
}

// handleHealth 输出健康检查结果。
//
// 不需要登录：这个端点只暴露「网关活着吗、dssh 就绪吗」，没有用户、路径、
// 版本之类值得藏起来的信息；反过来，它又必须在用户被锁在门外时还能用来排查。
func (g *Gate) handleHealth(w http.ResponseWriter, r *http.Request) {
	ready := false
	if g.opts.Ready != nil {
		ready = g.opts.Ready()
	}
	status := "ok"
	code := http.StatusOK
	if !ready {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"dsh":    ready,
	})
}

// handleShutdown 处理停机请求，供 `dsh-gate stop` 调用。
//
// **只接受来自回环地址的请求**，且不要求登录：这个端点的意义就是「本机上的人
// 让网关停下来」，而本机的人本来就有的是办法（任务管理器、kill）。
// 局域网里的其它设备即使知道地址也够不着——它们的请求不会来自回环。
//
// 为什么不靠 PID 文件加信号：那套方案要在每个平台各写一份信号代码，而 Windows
// 根本没有 SIGTERM，只能变成「强杀」，dsh 来不及收尾。走控制端点则三个平台的
// 停机方式完全一致，而且网关知道自己被请求停机，可以按和 Ctrl-C 一样的流程收尾。
func (g *Gate) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if g.opts.Shutdown == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "停机端点只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "停机端点只接受本机（回环地址）的请求", http.StatusForbidden)
		return
	}

	g.log.Info("收到本机停机请求")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "正在停止……\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	// 先把响应送出去再触发停机：否则客户端会看到一个连接被直接掐断的
	// 空响应，分不清「命令发成功了」还是「网关崩了」。
	go func() {
		time.Sleep(200 * time.Millisecond)
		g.opts.Shutdown()
	}()
}

// isLoopbackRequest 判断请求是否来自本机。
func isLoopbackRequest(r *http.Request) bool {
	ip := net.ParseIP(cookie.ClientIP(r))
	return ip != nil && ip.IsLoopback()
}

// renderLogin 渲染登录页。
func (g *Gate) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	data := struct {
		Title    string
		Subtitle string
		Error    string
		Username string
		Next     string
		Host     string
		Version  string
		Action   string
	}{
		Title:    g.opts.Title,
		Subtitle: g.opts.Subtitle,
		Error:    message,
		Next:     safeNext(r.URL.Query().Get("next")),
		Host:     displayHost(r),
		Version:  g.version(),
		Action:   PathLogin,
	}
	if r.Method == http.MethodPost {
		data.Username = strings.TrimSpace(r.PostFormValue("username"))
		data.Next = safeNext(r.PostFormValue("next"))
	}

	var body strings.Builder
	if err := g.tmpl.Execute(&body, data); err != nil {
		// 模板渲染失败属于编程错误；此时绝不能把内部错误甩给用户，也不能
		// 渲染出半截页面，直接记日志并回一个简单的 500。
		g.log.Error("渲染登录页失败", "err", err)
		http.Error(w, "登录页渲染失败，请联系管理员查看网关日志", http.StatusInternalServerError)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	// 登录页自带内联样式与脚本，因此 CSP 只能放开 'unsafe-inline'；但可以
	// 彻底禁止外部资源加载，也禁止被任何页面内嵌，防点击劫持。
	header.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"img-src data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body.String())
}

// version 返回页脚显示的版本号。
func (g *Gate) version() string {
	if g.opts.Version == "" {
		return "dev"
	}
	return g.opts.Version
}

// loginURL 构造登录页地址，带上原始路径以便登录后跳回。
func (g *Gate) loginURL(r *http.Request) string {
	next := safeNext(r.URL.RequestURI())
	if next == "/" {
		return PathLogin
	}
	return PathLogin + "?next=" + url.QueryEscape(next)
}

// safeNext 校验登录后的跳转目标。
//
// 只接受以单个斜杠开头的站内路径：`//evil.com` 会被浏览器当成协议相对 URL
// 跳到外站，`http://…` 与 `\n` 则分别可能构成跳转与响应头注入。宁可回首页，
// 也不能把「登录后跳回去」变成一个开放重定向。
func safeNext(raw string) string {
	if raw == "" {
		return "/"
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	// 反斜杠同样危险：部分浏览器会把 "/\evil.test" 当成协议相对 URL。
	if strings.ContainsAny(raw, "\r\n\x00\\") {
		return "/"
	}
	if parsed, err := url.Parse(raw); err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	return raw
}

// displayHost 返回用于页脚展示的主机名。
func displayHost(r *http.Request) string {
	if host := r.Host; host != "" {
		return host
	}
	return "本机"
}

// humanDuration 把时长渲染成「1 分 30 秒」这样的中文，便于阅读错误提示。
func humanDuration(d time.Duration) string {
	seconds := int(d.Round(time.Second) / time.Second)
	switch {
	case seconds <= 1:
		return "1 秒"
	case seconds < 60:
		return strconv.Itoa(seconds) + " 秒"
	case seconds < 3600:
		minutes := seconds / 60
		rest := seconds % 60
		if rest == 0 {
			return strconv.Itoa(minutes) + " 分钟"
		}
		return strconv.Itoa(minutes) + " 分 " + strconv.Itoa(rest) + " 秒"
	default:
		hours := seconds / 3600
		minutes := (seconds % 3600) / 60
		if minutes == 0 {
			return strconv.Itoa(hours) + " 小时"
		}
		return strconv.Itoa(hours) + " 小时 " + strconv.Itoa(minutes) + " 分"
	}
}
