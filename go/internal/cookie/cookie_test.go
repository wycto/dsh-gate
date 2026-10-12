package cookie_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/cookie"
)

func TestSetAndGet(t *testing.T) {
	recorder := httptest.NewRecorder()
	cookie.Set(recorder, cookie.Session, "abc123", cookie.Options{MaxAge: time.Hour})

	header := recorder.Header().Get("Set-Cookie")
	if header == "" {
		t.Fatal("应当写出 Set-Cookie")
	}
	// 会话 Cookie 必须对前端脚本不可见：dsh 页面里一旦出现 XSS，攻击者脚本
	// 就偷不走网关的登录态。
	if want := cookie.Session + "=abc123"; header[:len(want)] != want {
		t.Errorf("Set-Cookie = %q", header)
	}
	for _, attribute := range []string{"HttpOnly", "Path=/", "SameSite=Lax", "Max-Age=3600"} {
		if !contains(header, attribute) {
			t.Errorf("Set-Cookie 缺少 %s：%s", attribute, header)
		}
	}
	if contains(header, "Secure") {
		t.Errorf("未启用 HTTPS 时不应带 Secure：%s", header)
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: cookie.Session, Value: "abc123"})
	if got := cookie.Get(request, cookie.Session); got != "abc123" {
		t.Errorf("Get = %q", got)
	}
	if got := cookie.Get(request, "absent"); got != "" {
		t.Errorf("不存在的 Cookie 应返回空串，实际 %q", got)
	}
}

func TestSecureFlag(t *testing.T) {
	recorder := httptest.NewRecorder()
	cookie.Set(recorder, cookie.Session, "x", cookie.Options{Secure: true, MaxAge: time.Hour})
	if !contains(recorder.Header().Get("Set-Cookie"), "Secure") {
		t.Error("HTTPS 下必须带 Secure，否则会话会明文过网")
	}
}

// TestClearDeletesCookie 锁定一个容易踩的细节。
//
// Cookie.MaxAge=0 只表示「会话结束时就过期」，浏览器仍会继续带上它；
// 真正删除必须是负值。中间那层转换由 net/http 完成，这里验证端到端结果。
func TestClearDeletesCookie(t *testing.T) {
	recorder := httptest.NewRecorder()
	cookie.Clear(recorder, cookie.Session, cookie.Options{})

	header := recorder.Header().Get("Set-Cookie")
	if !contains(header, "Max-Age=0") {
		t.Errorf("删除 Cookie 必须写 Max-Age=0：%s", header)
	}
	if !contains(header, "Expires=") {
		t.Errorf("删除 Cookie 必须带过去的过期时间：%s", header)
	}
}

func TestClientIP(t *testing.T) {
	cases := map[string]string{
		"192.168.1.20:54321": "192.168.1.20",
		"[2408:826a::1]:443": "2408:826a::1",
		"127.0.0.1:5000":     "127.0.0.1",
		"unix":               "unix",
	}
	for addr, want := range cases {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = addr
		if got := cookie.ClientIP(request); got != want {
			t.Errorf("ClientIP(%q) = %q，期望 %q", addr, got, want)
		}
	}
}

func TestClientIPIgnoresForwardedHeaders(t *testing.T) {
	// 网关是链路最外层，采信 X-Forwarded-For 等于让任何人伪造来源 IP，
	// 登录限速会立刻失去意义。
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.5:1234"
	request.Header.Set("X-Forwarded-For", "1.2.3.4")
	request.Header.Set("X-Real-IP", "6.7.8.9")
	if got := cookie.ClientIP(request); got != "10.0.0.5" {
		t.Errorf("ClientIP = %q，应当忽略转发头", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
