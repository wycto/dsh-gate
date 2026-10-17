// Package mountproxy 实现「把上游 dsh 挂到一个子路径下」的那层机械活。
//
// 为什么单独成包：管理端要在一个域名下的 /i/<id>/ 里挂 N 个实例，而 dsh 只认
// origin-root 路由。真正容易出错、也最值得单独测的不是转发本身，而是三件事：
//
//  1. **剥前缀**：`/i/<id>/api/x` → 上游 `/api/x`。dsh 的 index.html 用文档相对
//     路径引用资源（`./assets/…`），因此浏览器在挂载下会自动带上前缀，剥掉即可。
//  2. **改写上游的 `Set-Cookie` 路径**：dsh 的会话 Cookie 名只由 authority 决定
//     （`dsh-auth-<hash(authority)>`），子路径方案下所有实例共用同一个域名 ⇒
//     **Cookie 名完全相同**；上游写的是 `Path=/`，不改写就会互相覆盖、串会话。
//  3. **挂载根重定向**：`/i/<id>` → `/i/<id>/`（dsh 的启动令牌交换只在 `GET /`
//     发生，`/index.html` 直接命中不会交换；所以挂载根必须是带斜杠的那个）。
//
// 这里刻意**不做**令牌交接、不做授权判定：前者是管理端的事（它才知道启动令牌），
// 后者必须先于本包执行（未授权时根本不该走到代理）。本包只保证「前缀与 Cookie
// 的机械正确」，这样它可以脱离管理端单独测试。
package mountproxy

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Options 是挂载代理的配置。
type Options struct {
	// Prefix 是挂载前缀，形如 `/i/abc123`（不带尾斜杠）。
	Prefix string
	// Upstream 是上游处理器（通常是打到该实例 dsh 回环端口的反向代理）。
	Upstream http.Handler
	// Secure 判断「这一次请求在浏览器看来是不是 HTTPS」：是的话，上游签发的
	// Cookie 要补 Secure 标记（上游是明文 HTTP，不会自己加）。留空表示从不补。
	//
	// 用函数而不是布尔值：管理端可能同时被 http 与 https 访问（反代终止 TLS 时
	// 靠 X-Forwarded-Proto 判断），一个进程级常量会给出错误答案。
	Secure func(*http.Request) bool
}

// SecureAlways 返回一个恒定的 Secure 判定，供固定部署与测试使用。
func SecureAlways(secure bool) func(*http.Request) bool {
	return func(*http.Request) bool { return secure }
}

// Proxy 把一个上游处理器挂到 Prefix 下。
type Proxy struct {
	prefix   string
	upstream http.Handler
	secure   func(*http.Request) bool
}

// New 构造挂载代理。
func New(opts Options) (*Proxy, error) {
	if opts.Upstream == nil {
		return nil, errors.New("挂载代理需要上游处理器")
	}
	prefix := strings.TrimSpace(opts.Prefix)
	if prefix == "" || !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("挂载前缀 %q 必须以 / 开头", opts.Prefix)
	}
	if strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("挂载前缀 %q 不该以 / 结尾", opts.Prefix)
	}
	if strings.ContainsAny(prefix, " \t\r\n?#") {
		return nil, fmt.Errorf("挂载前缀 %q 含非法字符", opts.Prefix)
	}
	secure := opts.Secure
	if secure == nil {
		secure = SecureAlways(false)
	}
	return &Proxy{prefix: prefix, upstream: opts.Upstream, secure: secure}, nil
}

// Prefix 返回挂载前缀。
func (p *Proxy) Prefix() string { return p.prefix }

// ServeHTTP 处理挂载前缀下的请求。
//
// 非本前缀的请求一律 404：调用方本不该把别的路径交给它，真发生了说明路由有
// 问题，此时「明确 404」比「转发到别人的实例」安全得多。
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 挂载根本身（没有尾斜杠）：补一个尾斜杠再继续，保留查询串。
	if path == p.prefix {
		target := p.prefix + "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	if !strings.HasPrefix(path, p.prefix+"/") {
		http.NotFound(w, r)
		return
	}

	// 克隆请求：上游拿到的是剥掉前缀的路径，Host 原样保留（dsh 的 Host/Origin
	// 围栏按收到的 Host 比对，改成监听器地址必然 403）。
	upstreamReq := r.Clone(r.Context())
	upstreamReq.URL.Path = strings.TrimPrefix(path, p.prefix)
	upstreamReq.URL.RawPath = ""
	upstreamReq.RequestURI = upstreamReq.URL.RequestURI()
	upstreamReq.Header = r.Header.Clone()

	cw := &cookieRewriter{ResponseWriter: w, prefix: p.prefix, secure: p.secure(r)}
	p.upstream.ServeHTTP(cw, upstreamReq)

	// 上游可能只设了响应头就返回（既不调 WriteHeader 也不写正文）。真实服务器
	// 会在处理器返回时补一个 200 并写出响应头；我们必须在这里做同一件事，
	// 否则那些 Set-Cookie 会绕过改写原样发出去——而「Cookie 没被挪进挂载前缀」
	// 正是本包要消灭的 bug（测试里就抓到过这一条）。
	if !cw.headerSent {
		cw.WriteHeader(http.StatusOK)
	}
}

// cookieRewriter 在响应头写出前改写上游的 Set-Cookie。
//
// 只动 Set-Cookie，其它响应头与状态码原样透传。同时实现 Flusher 与 Hijacker：
// 前者是 SSE（`/plugins/events`）不缓冲的前提，后者是 WebSocket
// （`/api/remote.mux`）升级的前提——少一个就会出现「页面打开但流不动」。
type cookieRewriter struct {
	http.ResponseWriter
	prefix     string
	secure     bool
	headerSent bool
}

func (w *cookieRewriter) WriteHeader(status int) {
	if w.headerSent {
		return
	}
	w.rewriteCookies()
	w.rewriteLocation()
	w.headerSent = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *cookieRewriter) Write(b []byte) (int, error) {
	if !w.headerSent {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush 让流式响应（SSE、长轮询）不被缓冲。
func (w *cookieRewriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 让 WebSocket 升级能穿透这层包装。
func (w *cookieRewriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

// rewriteLocation 把上游的**根相对**重定向改写到挂载前缀下。
//
// 为什么必须有这一步：dsh 完成令牌交换后会 302 到 `/`（这是它的正常行为）。
// 挂在 `/i/<id>/` 下时，浏览器若照着这个 `Location: /` 走，就跳出了实例、回到
// 管理端首页 —— 表现是「点进实例闪一下又回到列表」，而且看起来完全不像代理的错。
//
// 只动根相对路径：
//   - `//evil.test` 是协议相对 URL（可能被解析成外站），原样透传；
//   - `http(s)://…` 是上游自己指定的绝对地址，不该被我们改写；
//   - 已经带前缀的保持幂等。
func (w *cookieRewriter) rewriteLocation() {
	values := w.Header().Values("Location")
	if len(values) == 0 {
		return
	}
	rewritten := make([]string, 0, len(values))
	for _, raw := range values {
		rewritten = append(rewritten, mountLocation(w.prefix, raw))
	}
	w.Header().Del("Location")
	for _, v := range rewritten {
		w.Header().Add("Location", v)
	}
}

// mountLocation 把一处 Location 挪进挂载前缀。
func mountLocation(prefix, location string) string {
	if location == "" || !strings.HasPrefix(location, "/") || strings.HasPrefix(location, "//") {
		return location
	}
	if location == prefix || strings.HasPrefix(location, prefix+"/") {
		return location
	}
	return prefix + location
}

// rewriteCookies 把上游的每个 Set-Cookie 的 Path 挪进挂载前缀，并按需补 Secure。
func (w *cookieRewriter) rewriteCookies() {
	values := w.Header().Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	rewritten := make([]string, 0, len(values))
	for _, raw := range values {
		c, err := http.ParseSetCookie(raw)
		if err != nil {
			// 解析不了就原样透传：宁可让这个 Cookie 行为不完美，也不要把它吞掉
			// （吞掉可能直接让人登不进来）。
			rewritten = append(rewritten, raw)
			continue
		}
		c.Path = mountPath(w.prefix, c.Path)
		if w.secure && !c.Secure {
			c.Secure = true
		}
		rewritten = append(rewritten, c.String())
	}
	w.Header().Del("Set-Cookie")
	for _, v := range rewritten {
		w.Header().Add("Set-Cookie", v)
	}
}

// mountPath 把上游的 Cookie 路径挪到挂载前缀下。
//
//   - 空路径或 `/`（dsh 与多数插件都用这个）→ `<prefix>/`；
//   - 已经是 `<prefix>` 开头的（上游被同一挂载改过一次）→ 原样，保证幂等；
//   - 其它绝对路径 → 前缀拼接，保持相对层级；
//   - 相对路径（不以 / 开头）→ 交给 `<prefix>/`：相对路径在子路径挂载下语义
//     已经变了，落到挂载根比落到域名根更接近上游本意。
func mountPath(prefix, path string) string {
	if path == "" || path == "/" {
		return prefix + "/"
	}
	if !strings.HasPrefix(path, "/") {
		return prefix + "/"
	}
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		return path
	}
	return prefix + path
}
