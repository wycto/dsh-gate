package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/users"
)

// newTestUsers 建一个带一个账号的账号表。
func newTestUsers(t *testing.T) *users.Store {
	t.Helper()
	store, err := users.Open(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}
	if err := store.Set("alice", "password-1234", 50_000); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	return store
}

// newGateForTest 造一个带停机触发器的网关，返回它与一个「是否被触发过」的标记。
func newGateForTest(t *testing.T) (*Gate, *bool) {
	t.Helper()
	triggered := false
	gate, err := New(Options{
		Users:    newTestUsers(t),
		Shutdown: func() { triggered = true },
	})
	if err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}
	return gate, &triggered
}

// TestShutdownEndpointOnlyFromLoopback 验证停机端点只认回环来源。
//
// 否则局域网里的任何设备只要知道端口，就能把别人的网关关掉——而这台机器上的
// 用户本来就有别的办法停它，不需要走网络。
func TestShutdownEndpointOnlyFromLoopback(t *testing.T) {
	gate, stop := newGateForTest(t)

	cases := []struct {
		remote string
		want   int
		desc   string
	}{
		{"127.0.0.1:5000", http.StatusOK, "IPv4 回环"},
		{"[::1]:5000", http.StatusOK, "IPv6 回环"},
		{"192.168.1.20:5000", http.StatusForbidden, "局域网来源"},
		{"203.0.113.9:5000", http.StatusForbidden, "公网来源"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodPost, PathShutdown, nil)
		request.RemoteAddr = tc.remote
		recorder := httptest.NewRecorder()
		gate.Handler(http.NotFoundHandler()).ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Errorf("%s（%s）应返回 %d，实际 %d", tc.desc, tc.remote, tc.want, recorder.Code)
		}
	}
	_ = stop
}

// TestShutdownRequiresPost 验证停机端点只接受 POST。
func TestShutdownRequiresPost(t *testing.T) {
	gate, _ := newGateForTest(t)

	request := httptest.NewRequest(http.MethodGet, PathShutdown, nil)
	request.RemoteAddr = "127.0.0.1:5000"
	recorder := httptest.NewRecorder()
	gate.Handler(http.NotFoundHandler()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Allow"); !strings.Contains(got, "POST") {
		t.Errorf("405 应带上 Allow: POST，实际 %q", got)
	}
}

// TestShutdownAbsentWithoutHook 验证没注入触发器时端点不存在（404）。
func TestShutdownAbsentWithoutHook(t *testing.T) {
	store := newTestUsers(t)
	gate, err := New(Options{Users: store})
	if err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, PathShutdown, nil)
	request.RemoteAddr = "127.0.0.1:5000"
	recorder := httptest.NewRecorder()
	gate.Handler(http.NotFoundHandler()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未注入触发器时应当 404，实际 %d", recorder.Code)
	}
}

// isolateLangEnv 把语言相关的环境变量清空，避免开发机的 LANG/LC_ALL 影响断言。
func isolateLangEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"DSH_GATE_LANG", "LC_ALL", "LANG"} {
		t.Setenv(name, "")
	}
}

// TestLoginPageLanguage 验证登录页真的会跟着语言变，而不是只有目录里躺着一份英文。
func TestLoginPageLanguage(t *testing.T) {
	isolateLangEnv(t)
	gate, _ := newGateForTest(t)
	handler := gate.Handler(http.NotFoundHandler())

	renderLogin := func(mutate func(*http.Request)) (string, []*http.Cookie) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, PathLogin, nil)
		if mutate != nil {
			mutate(request)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("登录页应回 200，实际 %d", recorder.Code)
		}
		return recorder.Body.String(), recorder.Result().Cookies()
	}

	// 默认（中文）。「账号」是登录名的叫法（与「姓名」区分），文案改了这里跟着改。
	body, _ := renderLogin(nil)
	if !strings.Contains(body, "登录") || !strings.Contains(body, "账号") {
		t.Errorf("默认应为中文登录页，实际缺少中文文案")
	}

	// 显式 ?lang=en：英文文案 + 记住偏好
	body, cookies := renderLogin(func(r *http.Request) {
		r.URL.RawQuery = "lang=en"
	})
	for _, want := range []string{"Sign in", "Username", "Password"} {
		if !strings.Contains(body, want) {
			t.Errorf("?lang=en 的登录页缺少英文文案 %q", want)
		}
	}
	if strings.Contains(body, "账号") || strings.Contains(body, "登录") {
		t.Error("?lang=en 时不该再出现中文文案")
	}
	var langCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == LangCookie {
			langCookie = c
		}
	}
	if langCookie == nil {
		t.Fatal("显式指定语言时应写回偏好 Cookie")
	}
	if langCookie.Value != string(i18n.EN) {
		t.Errorf("语言 Cookie 值应为 %q，实际 %q", i18n.EN, langCookie.Value)
	}

	// Accept-Language 兜底
	body, _ = renderLogin(func(r *http.Request) {
		r.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	})
	if !strings.Contains(body, "Sign in") {
		t.Error("Accept-Language: en 时应渲染英文登录页")
	}

	// 不支持的语言回默认语言（而不是回退成 key）
	body, _ = renderLogin(func(r *http.Request) {
		r.Header.Set("Accept-Language", "fr-FR,de;q=0.9")
	})
	if !strings.Contains(body, "登录") {
		t.Error("不支持的语言应回落到默认语言")
	}

	// 环境变量指定服务器默认语言
	t.Setenv("DSH_GATE_LANG", "en")
	body, _ = renderLogin(nil)
	if !strings.Contains(body, "Sign in") {
		t.Error("DSH_GATE_LANG=en 时应渲染英文登录页")
	}
}

// TestLoginFailureMessageLanguage 验证失败提示也跟着语言走（含限速文案的时长格式）。
func TestLoginFailureMessageLanguage(t *testing.T) {
	isolateLangEnv(t)
	gate, _ := newGateForTest(t)
	handler := gate.Handler(http.NotFoundHandler())

	request := httptest.NewRequest(http.MethodPost, PathLogin+"?lang=en",
		strings.NewReader("username=alice&password=wrong-password"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("口令错误应回 401，实际 %d", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "Incorrect username or password") {
		t.Errorf("英文失败提示不对: %s", body)
	}
}
