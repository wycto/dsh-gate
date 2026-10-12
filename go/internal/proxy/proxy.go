// Package proxy 把已登录的请求转发给本机的 dsh，并在需要时替浏览器完成
// dsh 的启动令牌交换。
//
// ## 为什么需要这一层
//
// dsh 自带一套认证：每个进程生成一个随机启动令牌，启动时把它打印在 URL 的
// `?token=` 上；浏览器访问 `GET /?token=...` 才能换到签名会话 Cookie，之后
// 每个 API 调用与 WebSocket 都要带上它。这个设计对「谁在跑这个进程谁看得到
// stdout」的本地场景很合适，但放到「网关后面、从浏览器访问」的拓扑里就有两个
// 麻烦：
//
//  1. 令牌不能直接发给每个访问者——那等于谁拿到 URL 谁能用 agent。
//  2. 浏览器自己拿不到令牌，页面能打开但所有 API 都是 401。
//
// 网关的解法是：**令牌只留在网关进程内**，由网关在转发时替浏览器完成交换。
// 用户看到的只有一个自己设置的账号口令；令牌既不落盘也不进日志。
//
// ## 令牌是怎么交接的
//
//   - 浏览器 GET / 且「已交接」标记与当前令牌对不上 → 网关给上游的 URL 拼 token。
//   - dsh 完成交换，写下签名 Cookie 并 302 回干净的 `./`。
//   - 网关在这次**成功**的响应里种下标记 Cookie，值是该令牌的指纹。
//   - 之后浏览器访问 / 时带着这个指纹，网关不再重复注入（否则会无限 302）。
//
// 标记是必要的，不是优化：dsh 的 302 目标就是 `/` 本身，没有标记就无法区分
// 「第一次进来」和「刚被重定向回来」。
//
// 标记里存**令牌指纹**而不是 `1`，是为了让「dsh 重启过」能被自动识别：dsh 每次
// 启动都换一个令牌，指纹一变，所有浏览器下次打开 `/` 就重新交接一次。用布尔值
// 时踩过这个坑——dsh 重启后浏览器手里的旧标记让网关永远不再补令牌，页面停在
// dsh 那句 `dsh web authentication required` 上，刷新与重新登录都没用。
//
// 若 dsh 因为 cookie 过期、或用户换了 authority（IP ↔ 主机名 ↔ 穿透域名）
// 而返回 401，网关会清掉标记并把浏览器送回 `/` 重新交接一次——整个过程对
// 用户不可见，表现为「页面自己就好了」。
//
// ## Host 头必须原样转发
//
// dsh 会用收到的 `Host` 与被接受的 authority 比对，并要求 `Origin` 指向同一
// authority（DNS rebinding 与跨站防护）。因此这里**不改写** Host，而是给 dsh
// 传 `--trusted-host` 放行对外地址；改写成监听器地址会让 Origin 校验必然失败。
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/wycto/dsh-gate/internal/cookie"
)

// handoffMaxAge 是「已交接」标记的有效期。
//
// 它与 dsh 自己会话 Cookie 的 30 天有效期对齐：标记比会话活得久一点没坏处
// （真过期了会走 401 → 重新交接的自愈路径），但短太多会让用户每次打开页面
// 都莫名其妙多一次跳转。
const handoffMaxAge = 30 * 24 * time.Hour

// TokenSource 是令牌的记忆者，由 dshproc.Supervisor 实现。
type TokenSource interface {
	Token() string
	SetToken(string)
}

// Options 是反向代理的配置。
type Options struct {
	// Upstream 是 dsh 的地址，形如 http://127.0.0.1:3080。
	//
	// 它**只**来自本地配置，绝不接受请求头或查询参数指定——那会让网关变成
	// 一个任意 URL 的跳板，替内网攻击者探测端口。
	Upstream *url.URL
	// Tokens 是启动令牌的读写接口。
	Tokens TokenSource
	// Ready 报告 dsh 是否可以转发。
	Ready func() bool
	// Status 返回 dsh 当前的失败原因，空串表示只是「还在启动」。
	//
	// 等待页面会把它显示出来。dsh 起不来时（插件装不上、端口被别的程序抢了），
	// 用户应该看到原因，而不是一个永远转圈的圈。
	Status func() string
	// CookieSecure 表示网关启用 HTTPS，种 Cookie 时要带 Secure。
	CookieSecure bool
	// Logger 收日志；为 nil 时丢弃全部记录。
	Logger *slog.Logger
}

// Handler 是转发处理器。
type Handler struct {
	opts   Options
	proxy  *httputil.ReverseProxy
	log    *slog.Logger
	tokens TokenSource
}

// exchangeKey 在请求上下文里标记「本次请求让 dsh 做了一次令牌交接」。
type exchangeKey struct{}

// exchangeTokenKey 在请求上下文里带上本次交接使用的令牌，供写标记时算指纹。
type exchangeTokenKey struct{}

// handoffValue 是「已交接」标记的值：这次交接用的是哪个令牌的指纹。
//
// 只取前 12 个十六进制字符：它不是凭据，只是用来判断「dsh 是否换过令牌」，
// 没必要把令牌的任何完整摘要写进浏览器。
func handoffValue(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// New 构造转发处理器。
func New(opts Options) (*Handler, error) {
	if opts.Upstream == nil {
		return nil, fmt.Errorf("反向代理缺少上游地址")
	}
	if opts.Tokens == nil {
		return nil, fmt.Errorf("反向代理缺少令牌来源")
	}
	if opts.Ready == nil {
		opts.Ready = func() bool { return true }
	}
	if opts.Status == nil {
		opts.Status = func() string { return "" }
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	h := &Handler{opts: opts, log: opts.Logger, tokens: opts.Tokens}
	h.proxy = &httputil.ReverseProxy{
		Rewrite:        h.rewrite,
		Transport:      transport(),
		FlushInterval:  -1,
		ModifyResponse: h.modifyResponse,
		ErrorHandler:   h.errorHandler,
	}
	return h, nil
}

// transport 构造转发用的 HTTP 客户端。
func transport() *http.Transport {
	// 克隆默认实现再微调，而不是自建一堆 Transport 字段：默认值已经针对现代
	// 协议调校过，自己重写只会漏掉某条连接复用规则。
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// dsh 的 WebSocket 长连接与聊天流会让连接长期忙碌，多留一些空闲连接可以
	// 避免每开一个浏览器标签就重新握手一次。
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	return transport
}

// rewrite 改写出站请求。
func (h *Handler) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(h.opts.Upstream)

	// SetURL 会把 Out.Host 清空，从而让出站 Host 变成监听器地址；这里必须
	// 改回浏览器看到的 authority，否则 dsh 的 Origin 校验一定失败。
	pr.Out.Host = pr.In.Host

	pr.SetXForwarded()
}

// ServeHTTP 转发一次请求。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 情况一：用户把 dsh 打印的启动地址粘到了网关地址后面
	//（http://192.168.1.20:8080/?token=xxx）。这是接管外部 dsh 时唯一能拿到
	// 令牌的途径，学到之后它就只待在网关内存里。
	if token := r.URL.Query().Get("token"); token != "" {
		h.tokens.SetToken(token)
	}

	if !h.opts.Ready() {
		renderWaiting(w, h.opts.Status())
		return
	}

	// 情况二：直接命中 dsh 前端的直链 /index.html。
	//
	// 它是静态资源，能正常返回页面，但**不会**做令牌交换——结果就是页面能开、
	// 所有 API 401。统一导回 `/`，让浏览器走完整的握手。
	if r.URL.Path == "/index.html" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	// 情况三：正常交接。
	token := h.tokens.Token()
	incoming := r.URL.Query().Get("token")
	exchange := incoming != ""
	if !exchange && needsExchange(r, token) {
		if token == "" {
			// 手上没有令牌：补不了，只能让 dsh 自己说 401，由 401 那条路径
			// 给出可执行的说明。
		} else {
			query := r.URL.Query()
			query.Set("token", token)
			r.URL.RawQuery = query.Encode()
			exchange = true
		}
	}
	// 写标记时要用「这次真正用上的令牌」：用户粘进来的那个优先。
	if incoming != "" {
		token = incoming
	}

	ctx := context.WithValue(r.Context(), exchangeKey{}, exchange)
	ctx = context.WithValue(ctx, exchangeTokenKey{}, token)
	h.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// needsExchange 判断这次请求是否需要网关补上启动令牌。
func needsExchange(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	// 只有 GET / 是 dsh 的令牌交换入口；其它路径要么是静态资源（dsh 侧公开），
	// 要么是 API（本来就带着会话 Cookie）。
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		return false
	}
	// 标记里存的是「上次交接用的令牌指纹」：
	//   - 没有标记 → 这台浏览器还没交接，注入；
	//   - 指纹对不上 → 这台浏览器的会话是在**另一个令牌**下建立的，说明 dsh
	//     重启过（每次启动都换令牌），必须重新交接，否则它会一直 401；
	//   - 指纹一致 → 不重复注入，否则 dsh 的 302 会把浏览器来回弹。
	return cookie.Get(r, cookie.Handoff) != handoffValue(token)
}

// modifyResponse 在响应写给浏览器之前介入，做两件事：
//
//   - 种下「已交接」标记；
//   - 把 dsh 的 401 换成一个自动回到 / 的页面。
//
// 选在这里而不是包一层 ResponseWriter，是因为包装器必须实现 Flusher/Hijacker
// 才不破坏 WebSocket 与流式输出；而 ModifyResponse 天然发生在第一个字节写出
// 之前，改起来没有任何副作用。
func (h *Handler) modifyResponse(res *http.Response) error {
	exchanged, _ := res.Request.Context().Value(exchangeKey{}).(bool)
	// 只有**交接成功**才种标记：失败也种的话，浏览器会被标成「已交接」，
	// 而它其实没拿到会话——下一次访问 / 网关就不补令牌了，于是永远卡在 401。
	if exchanged && res.StatusCode >= 200 && res.StatusCode < 400 {
		token, _ := res.Request.Context().Value(exchangeTokenKey{}).(string)
		res.Header.Add("Set-Cookie", cookie.Value(cookie.Handoff, handoffValue(token), cookie.Options{
			Secure: h.opts.CookieSecure,
			MaxAge: handoffMaxAge,
		}).String())
	}

	// 401 说明浏览器手上的 dsh 会话不可用：过期了、被清掉了，或者用户换了
	// 访问地址（dsh 的会话 Cookie 与 authority 绑定）。清掉交接标记并把
	// 浏览器送回 /，网关会在那里再补一次令牌交接。
	//
	// 只对「页面导航」这么做：XHR 拿到一个 200 的 HTML 页面只会让前端解析
	// JSON 时炸掉，那还不如老老实实把 401 传回去，让调用方自己决定重试。
	if res.StatusCode != http.StatusUnauthorized || !isNavigation(res.Request) {
		return nil
	}

	// 带着交接标记还是 401：标记说明这台浏览器「换过一次会话」，而 dsh 现在
	// 不认它了——dsh 重启、会话过期、换了访问地址都会这样。清掉标记并 303 回
	// /，浏览器下一次请求就没有标记了，网关会补上这次启动的令牌重做交接。
	//
	// **这条对 / 本身同样成立**：以前这里特意放过 `/`（理由是「/ 就是 dsh 的
	// 交换入口」），可那恰恰是最常见的入口——用户在网关登录后被送回 `/`，
	// 看到的是 dsh 那句 `dsh web authentication required; reopen the URL printed
	// by dsh web.`，而标记还在，刷新多少次都一样，重新登录也没用。
	if cookie.Get(res.Request, cookie.Handoff) != "" {
		body := peekBody(&res.Body)
		h.log.Info("dsh 会话失效，清掉交接标记并重新交接",
			"path", res.Request.URL.Path, "body", body)
		_ = res.Body.Close()

		res.Header.Del("Content-Encoding")
		res.Header.Del("Content-Length")
		res.Header.Add("Set-Cookie", cookie.ClearValue(cookie.Handoff, cookie.Options{
			Secure: h.opts.CookieSecure,
		}).String())
		res.Header.Set("Location", "/")
		res.Header.Set("Cache-Control", "no-store")
		res.Body = io.NopCloser(strings.NewReader(""))
		res.ContentLength = 0
		res.StatusCode = http.StatusSeeOther
		return nil
	}

	// 没有标记却还是 401：这台浏览器不曾交接成功，而网关补上令牌之后 dsh 仍然
	// 拒绝——多半是网关手上这次启动的令牌不对（比如接管外部 dsh 又没拿到令牌）。
	// 再跳一次只会来回打转，直接给出可执行的说明。**这一条就是防打转的闸门**：
	// 上面那条 303 清掉了标记，所以下一次 401 必然走到这里。
	body := peekBody(&res.Body)
	h.log.Warn("无法完成 dsh 的令牌交接（未携带交接标记却收到 401）",
		"path", res.Request.URL.Path, "body", body)

	// 页面里要写清 dsh 的端口，用户才知道该把启动地址的端口换成什么。
	dshPort := 0
	if h.opts.Upstream != nil {
		if parsed, err := strconv.Atoi(h.opts.Upstream.Port()); err == nil {
			dshPort = parsed
		}
	}
	page := renderAuthRequired(shortBodyForPage(body), dshPort)
	_ = res.Body.Close()

	res.Header.Del("Content-Encoding")
	res.Header.Del("Content-Length")
	res.Header.Set("Content-Type", "text/html; charset=utf-8")
	res.Header.Set("Cache-Control", "no-store")
	res.Body = io.NopCloser(strings.NewReader(page))
	res.ContentLength = int64(len(page))
	res.StatusCode = http.StatusOK
	return nil
}

// errorHandler 处理连不上上游的情况：dsh 刚崩、被杀、或正在重启。
func (h *Handler) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Warn("转发到 dsh 失败", "upstream", h.opts.Upstream.String(), "err", err)
	renderUpstreamDown(w, err)
}

// isNavigation 判断这次请求是不是「用户在地址栏/链接上打开了一个页面」。
//
// 浏览器对导航请求会带 Sec-Fetch-Mode: navigate，老浏览器则靠 Accept 里的
// text/html 来区分——两者都取不到时按「不是导航」处理，宁可少做一次自愈，
// 也不要把 API 响应换成 HTML。
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// peekBody 读取响应体开头一小段用于日志，并把读走的内容接回去。
//
// 日志里那句 body 很值钱：dsh 返回 401 时不带任何解释，只有响应体能说明
// 是「令牌无效」「cookie 过期」还是「authority 不匹配」。
func peekBody(body *io.ReadCloser) string {
	if *body == nil {
		return ""
	}
	buf := make([]byte, 256)
	n, err := io.ReadFull(*body, buf)
	if n > 0 {
		*body = io.NopCloser(io.MultiReader(bytes.NewReader(buf[:n]), *body))
		return strings.TrimSpace(string(buf[:n]))
	}
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return ""
	}
	return ""
}

// shortBodyForPage 把 dsh 的响应体压成一行、给页面当细节显示。
//
// 两件事必须在这里做掉，都是实测踩出来的：
//
//  1. **响应体可能是压缩过的**。浏览器会发 `Accept-Encoding: gzip`，网关原样透传
//     给 dsh，而 Go 的 Transport 只在「编码头是它自己加的」时才自动解压——于是我们
//     拿到的是原始 gzip 字节。直接塞进页面就是一片乱码（实测页面里出现
//     `? K)?P(OMRH,-`，与响应体字节一一对应），用户完全看不懂。
//  2. **不能按字节截断**。中文一个字三字节，从中间切开会产生无效 UTF-8，页面上
//     又是一个「�」。按 rune 截断。
//
// 解不出可读文本时返回空串：页面上那句说明已经把情况讲清楚了，再贴一段二进制
// 只是噪音。
func shortBodyForPage(body string) string {
	text, ok := readableBody(body)
	if !ok {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	const limit = 160
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// readableBody 尽量把响应体变成可读文本，做不到就返回 ok=false。
func readableBody(body string) (string, bool) {
	if body == "" {
		return "", false
	}
	// gzip 魔数 0x1f 0x8b：尝试解压。解压出一段就够看，不必全解。
	if strings.HasPrefix(body, "\x1f\x8b") {
		reader, err := gzip.NewReader(strings.NewReader(body))
		if err != nil {
			return "", false
		}
		defer reader.Close()
		decoded, readErr := io.ReadAll(io.LimitReader(reader, 4096))
		if readErr != nil && len(decoded) == 0 {
			return "", false
		}
		body = string(decoded)
	}
	if !looksLikeText(body) {
		return "", false
	}
	return body, true
}

// looksLikeText 判断一段字节是否值得当文本显示：可打印字符要占绝大多数。
//
// 阈值取 0.85 而不是 1：真实报错里偶尔夹着控制字符（换行、制表），而那些是
// 该留下的；一片压缩数据则远低于这个比例。
func looksLikeText(body string) bool {
	total, printable := 0, 0
	for _, r := range body {
		total++
		if unicode.IsPrint(r) || r == '\n' || r == '\t' || r == '\r' {
			printable++
		}
	}
	if total == 0 {
		return false
	}
	return float64(printable)/float64(total) >= 0.85
}
