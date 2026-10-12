// Package dshtest 提供一个**忠实模拟 dsh 行为的假服务器**，供代理与集成测试使用。
//
// 为什么值得单独写一个：dsh-gate 最核心、也最容易悄悄坏掉的一环，是「令牌
// 交接」——浏览器第一次访问 `/` 时网关要补上令牌、dsh 换回签名 Cookie、网关
// 种下标记、之后不再重复注入。任何一步错了，表现都是「页面能开但一用就 401」，
// 而且只在真实 dsh 上才看得出来（真实 dsh 要下载安装，不适合放进单元测试）。
//
// 这个假服务器照着 dsh 的**文档化行为**实现：
//
//   - 启动令牌每个进程一个，写在 URL 的 ?token= 上；
//   - 只有 GET / 会用令牌换 Cookie，换完 302 回干净的 ./；
//   - 会话 Cookie 与 authority（hostname:port）绑定，换个地址访问即失效；
//   - Host 必须在允许名单（回环或 --trusted-host 点名的地址）内，否则 403；
//   - Origin 必须与 Host 同源；
//   - 静态资源公开，/api/ 需要会话，否则 401；
//   - /api/remote.mux 接受 WebSocket 升级。
package dshtest

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

// cookieName 是假 dsh 使用的会话 Cookie 名。
const cookieName = "dsh.sid"

// websocketGUID 是 WebSocket 握手要求的固定魔数（RFC 6455）。
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Server 是假 dsh。
type Server struct {
	// Token 是启动令牌。
	Token string
	// Trusted 是被接受的 authority 列表（可以带端口，也可以只有主机名）。
	Trusted []string

	http      *httptest.Server
	secret    []byte
	exchanges atomic.Int64
	apiCalls  atomic.Int64
}

// Start 启动一个假 dsh 并返回它。
func Start(trusted ...string) *Server {
	s := &Server{secret: randomBytes(32)}
	s.Token = "test-token-" + hex.EncodeToString(randomBytes(8))[:12]
	s.Trusted = append([]string{"127.0.0.1", "::1", "localhost"}, trusted...)
	s.http = httptest.NewServer(s)
	return s
}

// URL 返回假 dsh 的地址。
func (s *Server) URL() string { return s.http.URL }

// Addr 返回 host:port。
func (s *Server) Addr() string {
	parsed, _ := url.Parse(s.http.URL)
	return parsed.Host
}

// Port 返回监听端口。
func (s *Server) Port() int {
	_, portText, _ := net.SplitHostPort(s.Addr())
	port, _ := strconv.Atoi(portText)
	return port
}

// SessionCookie 返回一个绑定到指定 authority 的合法会话 Cookie。
//
// 测试里有些场景（例如直接验证 WebSocket 透传）需要绕过令牌交换流程，
// 这时用它伪造一个「已经登录」的浏览器。
func (s *Server) SessionCookie(authority string) *http.Cookie {
	return s.newCookie(authority)
}

// Close 关闭服务器。
func (s *Server) Close() { s.http.Close() }

// Exchanges 返回令牌交换次数：被正确注入令牌的 GET / 请求会推进它。
func (s *Server) Exchanges() int { return int(s.exchanges.Load()) }

// APICalls 返回通过的 API 调用次数。
func (s *Server) APICalls() int { return int(s.apiCalls.Load()) }

// ServeHTTP 实现上面注释里描述的 dsh 行为。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Host 栅栏：只接受回环或被点名�� authority。
	if !s.acceptsHost(r.Host) {
		http.Error(w, "forbidden: unrecognized host "+r.Host, http.StatusForbidden)
		return
	}
	// 2. Origin 必须与 Host 同源。
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(parsed.Host, r.Host) {
			http.Error(w, "forbidden: origin mismatch", http.StatusForbidden)
			return
		}
	}

	// 3. 令牌交换只发生在 GET /。
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		if token := r.URL.Query().Get("token"); token != "" {
			if !hmac.Equal([]byte(token), []byte(s.Token)) {
				http.Error(w, "unauthorized: invalid startup token", http.StatusUnauthorized)
				return
			}
			s.exchanges.Add(1)
			http.SetCookie(w, s.newCookie(r.Host))
			http.Redirect(w, r, "./", http.StatusFound)
			return
		}
	}

	// 4. API 需要会话；静态资源公开。
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !s.validCookie(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if isWebSocket(r) {
			s.serveWebSocket(w, r)
			return
		}
		s.apiCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"path":%q}`, r.URL.Path)
		return
	}

	// 5. 首页：匿名可访问，内容里带 dsh 字样（让网关的身份识别探针能认出它）。
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

// indexHTML 模拟 dsh 的前端外壳。
const indexHTML = `<!DOCTYPE html><html><head><title>DeepSeek Harness</title></head>` +
	`<body><div id="root"></div><script src="/assets/index.js"></script></body></html>`

// acceptsHost 实现 Host/Origin 栅栏。
func (s *Server) acceptsHost(host string) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	// 回环永远放行（dsh 自己绑定的地址）。
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, allowed := range s.Trusted {
		if strings.EqualFold(allowed, host) || strings.EqualFold(allowed, name) {
			return true
		}
	}
	return false
}

// newCookie 造一个绑定到该 authority 的会话 Cookie。
func (s *Server) newCookie(authority string) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    s.sign(authority),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// sign 计算与 authority 绑定的签名。
func (s *Server) sign(authority string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(authority))
	return hex.EncodeToString(mac.Sum(nil))
}

// validCookie 校验 Cookie 是否有效**且**绑定当前 authority。
func (s *Server) validCookie(r *http.Request) bool {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(cookie.Value), []byte(s.sign(r.Host)))
}

// isWebSocket 判断是否是 WebSocket 升级请求。
func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(r.Header.Get("Connection"), "upgrade")
}

// serveWebSocket 完成一次最小可用的 WebSocket 握手，然后原样回显字节。
//
// 这里只实现「握手 + 透传字节」：足够验证网关有没有把 Upgrade 头送上去、
// 有没有把 101 响应原样带回来，而不需要在测试里实现完整的帧协议。
func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "bad websocket handshake", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, buffer, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	accept := computeWebSocketAccept(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := buffer.WriteString(response); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	// 原样回显：客户端发什么，就还什么。
	_, _ = io.Copy(conn, buffer)
}

// computeWebSocketAccept 按 RFC 6455 计算 Sec-WebSocket-Accept。
func computeWebSocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// randomBytes 生成随机字节。
func randomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("读取系统随机数失败: " + err.Error())
	}
	return buf
}
