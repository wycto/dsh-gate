package mountproxy

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 编译期断言：包装器必须实现这两个接口，否则 SSE 会被缓冲、WebSocket 升不了级。
var (
	_ http.Flusher  = (*cookieRewriter)(nil)
	_ http.Hijacker = (*cookieRewriter)(nil)
)

// upstreamRecorder 记录上游收到的请求，并按需写入响应。
type upstreamRecorder struct {
	path  string
	host  string
	query string
	raw   string
	head  http.Header
	calls int
	reply func(w http.ResponseWriter)
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.calls++
	u.path = r.URL.Path
	u.host = r.Host
	u.query = r.URL.RawQuery
	u.raw = r.RequestURI
	u.head = r.Header.Clone()
	if u.reply != nil {
		u.reply(w)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func newProxy(t *testing.T, prefix string, upstream http.Handler, secure func(*http.Request) bool) *Proxy {
	t.Helper()
	p, err := New(Options{Prefix: prefix, Upstream: upstream, Secure: secure})
	if err != nil {
		t.Fatalf("构造挂载代理失败: %v", err)
	}
	return p
}

func do(p *Proxy, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	recorder := httptest.NewRecorder()
	p.ServeHTTP(recorder, request)
	return recorder
}

func TestStripsPrefixAndKeepsHost(t *testing.T) {
	upstream := &upstreamRecorder{}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	rec := do(p, http.MethodPost, "http://portal.test/i/abc123/api/remote.mux?x=1&y=2",
		map[string]string{"Origin": "http://portal.test"})

	if rec.Code != http.StatusOK {
		t.Fatalf("应透传上游 200，实际 %d", rec.Code)
	}
	if upstream.path != "/api/remote.mux" {
		t.Errorf("上游应看到剥掉前缀的路径，实际 %q", upstream.path)
	}
	if upstream.query != "x=1&y=2" {
		t.Errorf("查询串应保留，实际 %q", upstream.query)
	}
	if upstream.host != "portal.test" {
		t.Errorf("Host 必须原样转发（dsh 的 Host/Origin 围栏靠它），实际 %q", upstream.host)
	}
	if upstream.raw != "/api/remote.mux?x=1&y=2" {
		t.Errorf("RequestURI 也应同步剥前缀，实际 %q", upstream.raw)
	}
	if got := upstream.head.Get("Origin"); got != "http://portal.test" {
		t.Errorf("其它请求头应透传，Origin 实际 %q", got)
	}
}

func TestMountRootMapsToOriginRoot(t *testing.T) {
	upstream := &upstreamRecorder{}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	if rec := do(p, http.MethodGet, "http://portal.test/i/abc123/", nil); rec.Code != http.StatusOK {
		t.Fatalf("挂载根应透传，实际 %d", rec.Code)
	}
	if upstream.path != "/" {
		t.Errorf("挂载根应映射到上游 /，实际 %q", upstream.path)
	}
}

func TestBareMountRedirectsWithQuery(t *testing.T) {
	upstream := &upstreamRecorder{}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	rec := do(p, http.MethodGet, "http://portal.test/i/abc123?token=secret", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("裸挂载应 303，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/i/abc123/?token=secret" {
		t.Errorf("303 目标应带尾斜杠并保留查询串，实际 %q", got)
	}
	if upstream.calls != 0 {
		t.Error("裸挂载不该打到上游：dsh 只在挂载根完成令牌交换")
	}
}

func TestForeignPathIs404(t *testing.T) {
	upstream := &upstreamRecorder{}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	if rec := do(p, http.MethodGet, "http://portal.test/i/other/api", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("非本前缀应 404，实际 %d", rec.Code)
	}
	if upstream.calls != 0 {
		t.Error("非本前缀的请求绝不能转发到上游（会串到别人的实例）")
	}
}

func TestCookiePathRewrittenToMount(t *testing.T) {
	upstream := &upstreamRecorder{reply: func(w http.ResponseWriter) {
		w.Header().Add("Set-Cookie", "dsh-auth-abc123=token; Path=/; Max-Age=2592000; HttpOnly; SameSite=Strict")
		w.Header().Add("Set-Cookie", "dsh_warden=sess; Path=/; HttpOnly; SameSite=Lax")
		w.WriteHeader(http.StatusOK)
	}}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(true))

	rec := do(p, http.MethodGet, "http://portal.test/i/abc123/", nil)
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("应有 2 个 Cookie，实际 %d", len(cookies))
	}
	for _, c := range cookies {
		if c.Path != "/i/abc123/" {
			t.Errorf("Cookie %s 的 Path 应改写为挂载前缀，实际 %q", c.Name, c.Path)
		}
		if !c.Secure {
			t.Errorf("管理端是 HTTPS 时 Cookie %s 应补 Secure", c.Name)
		}
		if !c.HttpOnly {
			t.Errorf("Cookie %s 的 HttpOnly 不该被丢掉", c.Name)
		}
	}
	if got := cookies[0].SameSite; got != http.SameSiteStrictMode {
		t.Errorf("SameSite 应保留为 Strict，实际 %v", got)
	}
}

func TestCookieSecureOnlyWhenPortalIsHTTPS(t *testing.T) {
	upstream := &upstreamRecorder{reply: func(w http.ResponseWriter) {
		w.Header().Add("Set-Cookie", "dsh-auth-x=1; Path=/; HttpOnly")
	}}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	rec := do(p, http.MethodGet, "http://portal.test/i/abc123/", nil)
	if c := rec.Result().Cookies()[0]; c.Secure {
		t.Error("管理端是明文 HTTP 时不该给 Cookie 补 Secure（浏览器不会回传）")
	}
}

func TestCookiePathRewriteIsIdempotentAndKeepsOtherPaths(t *testing.T) {
	upstream := &upstreamRecorder{reply: func(w http.ResponseWriter) {
		w.Header().Add("Set-Cookie", "a=1; Path=/i/abc123/") // 已经在上游被改过
		w.Header().Add("Set-Cookie", "b=2; Path=/admin")     // 其它绝对路径
		w.Header().Add("Set-Cookie", "c=3")                  // 没有 Path
		w.Header().Add("Set-Cookie", "broken")               // 解析不了
	}}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	rec := do(p, http.MethodGet, "http://portal.test/i/abc123/", nil)
	raw := rec.Header().Values("Set-Cookie")
	joined := strings.Join(raw, "\n")
	for _, want := range []string{
		"Path=/i/abc123/",      // 幂等：不重复前缀
		"Path=/i/abc123/admin", // 其它绝对路径挂到前缀下
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Set-Cookie 缺少 %q，实际:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "/i/abc123/i/abc123") {
		t.Errorf("重复改写导致路径叠加:\n%s", joined)
	}
	if !strings.Contains(joined, "broken") {
		t.Errorf("解析不了的 Set-Cookie 应原样透传，实际:\n%s", joined)
	}
	if strings.Count(joined, "c=3") != 1 {
		t.Errorf("无 Path 的 Cookie 应被补成挂载前缀:\n%s", joined)
	}
}

func TestUnrelatedHeadersAndStatusUntouched(t *testing.T) {
	upstream := &upstreamRecorder{reply: func(w http.ResponseWriter) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusTeapot)
	}}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	rec := do(p, http.MethodGet, "http://portal.test/i/abc123/", nil)
	if rec.Code != http.StatusTeapot {
		t.Errorf("状态码应原样透传，实际 %d", rec.Code)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Error("其它响应头应原样透传")
	}
}

// TestUpstreamRootRelativeRedirectIsMounted 锁住「点进实例闪一下又回管理端首页」那个坑：
// dsh 完成令牌交换后会 302 到 `/`，挂在子路径下时必须改写到挂载根。
func TestUpstreamRootRelativeRedirectIsMounted(t *testing.T) {
	upstream := &upstreamRecorder{reply: func(w http.ResponseWriter) {
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)
	}}
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))

	rec := do(p, http.MethodGet, "http://portal.test/i/abc123/", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("应透传 302，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/i/abc123/" {
		t.Fatalf("根相对重定向应改写到挂载根，实际 %q", got)
	}
}

func TestLocationRewriteOnlyTouchesRootRelative(t *testing.T) {
	cases := map[string]string{
		"/":                     "/i/abc123/",
		"/login?next=%2F":       "/i/abc123/login?next=%2F",
		"/i/abc123/already":     "/i/abc123/already",     // 幂等
		"//evil.test/x":         "//evil.test/x",         // 协议相对：不能改
		"https://dsh.example/x": "https://dsh.example/x", // 绝对地址：上游自己的决定
		"relative/path":         "relative/path",         // 相对路径：交给浏览器按当前目录解析
	}
	for input, want := range cases {
		upstream := &upstreamRecorder{reply: func(w http.ResponseWriter) {
			w.Header().Set("Location", input)
			w.WriteHeader(http.StatusSeeOther)
		}}
		p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))
		rec := do(p, http.MethodGet, "http://portal.test/i/abc123/", nil)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("Location %q 应改写为 %q，实际 %q", input, want, got)
		}
	}
}

// TestStreamingIsNotBuffered 是 SSE 不被缓冲的行为断言：上游写一段就 flush，
// 然后一直阻塞；客户端必须在上游解阻塞**之前**读到那一段。
func TestStreamingIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	})
	p := newProxy(t, "/i/abc123", upstream, SecureAlways(false))
	server := httptest.NewServer(p)
	defer server.Close()
	defer close(release)

	response, err := http.Get(server.URL + "/i/abc123/plugins/events")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer response.Body.Close()

	type result struct {
		line string
		err  error
	}
	lineCh := make(chan result, 1)
	go func() {
		reader := bufio.NewReader(response.Body)
		line, err := reader.ReadString('\n')
		lineCh <- result{line, err}
	}()

	select {
	case got := <-lineCh:
		if got.err != nil {
			t.Fatalf("读第一行失败: %v", got.err)
		}
		if !strings.Contains(got.line, "first") {
			t.Fatalf("应在上游阻塞期间读到第一段，实际 %q", got.line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没读到第一段：响应被缓冲了，SSE 会一直转圈")
	}
}

func TestNewValidatesOptions(t *testing.T) {
	cases := []Options{
		{Prefix: "/i/a", Upstream: nil},
		{Prefix: "", Upstream: http.NotFoundHandler()},
		{Prefix: "i/a", Upstream: http.NotFoundHandler()},
		{Prefix: "/i/a/", Upstream: http.NotFoundHandler()},
		{Prefix: "/i/a b", Upstream: http.NotFoundHandler()},
	}
	for _, opts := range cases {
		if _, err := New(opts); err == nil {
			t.Errorf("非法配置应被拒绝: %+v", opts)
		}
	}
}
