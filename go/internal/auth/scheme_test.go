package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// TestParseScheme 验证方案字面值归一化：不认识的值必须被拒绝（ok=false），
// 而不是悄悄当成某个方案接受；别名（default/brass/green）一并收下。
func TestParseScheme(t *testing.T) {
	cases := []struct {
		raw  string
		want Scheme
		ok   bool
	}{
		{"slate", SchemeSlate, true},
		{"SLATE", SchemeSlate, true},
		{" default ", SchemeSlate, true},
		{"ocean", SchemeOcean, true},
		{"violet", SchemeViolet, true},
		{"forest", SchemeForest, true},
		{"brass", SchemeForest, true},
		{"green", SchemeForest, true},
		{"", DefaultScheme, false},
		{"neon", DefaultScheme, false},
	}
	for _, tc := range cases {
		got, ok := parseScheme(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseScheme(%q) = (%q, %v)，期望 (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLoginPageScheme 验证方案偏好真的走通：?scheme= 写 Cookie 并渲染进
// <html data-scheme>；Cookie 记住；非法值忽略且不留 Cookie；没选过也有缺省。
func TestLoginPageScheme(t *testing.T) {
	isolateLangEnv(t)
	gate, _ := newGateForTest(t)
	handler := gate.Handler(http.NotFoundHandler())

	render := func(mutate func(*http.Request)) (string, []*http.Cookie) {
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
	findCookie := func(cookies []*http.Cookie, name string) *http.Cookie {
		for _, c := range cookies {
			if c.Name == name {
				return c
			}
		}
		return nil
	}

	// 没选过：data-scheme 写缺省方案（色相没有「跟随系统」这一态）。
	body, cookies := render(nil)
	if !strings.Contains(body, `data-scheme="slate"`) {
		t.Error("没选过方案时应渲染缺省方案 slate")
	}
	if findCookie(cookies, SchemeCookie) != nil {
		t.Error("没选过方案时不该凭空写一条 Cookie")
	}

	// 显式选择：渲染 + 记住。
	body, cookies = render(func(r *http.Request) { r.URL.RawQuery = "scheme=forest" })
	if !strings.Contains(body, `data-scheme="forest"`) {
		t.Error("?scheme=forest 时应渲染墨绿方案")
	}
	c := findCookie(cookies, SchemeCookie)
	if c == nil || c.Value != string(SchemeForest) {
		t.Errorf("显式选方案应写回 Cookie，实际 %+v", c)
	}
	if c != nil && (c.Path != "/" || c.MaxAge <= 0 || c.HttpOnly) {
		t.Errorf("方案 Cookie 应整站长期有效且不加 HttpOnly，实际 Path=%q MaxAge=%d HttpOnly=%v",
			c.Path, c.MaxAge, c.HttpOnly)
	}

	// Cookie 记住：下次访问不用再带参数。
	body, _ = render(func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: SchemeCookie, Value: string(SchemeViolet)})
	})
	if !strings.Contains(body, `data-scheme="violet"`) {
		t.Error("Cookie 里选了绛紫时应渲染绛紫页面")
	}

	// ?scheme= 覆盖 Cookie。
	body, _ = render(func(r *http.Request) {
		r.URL.RawQuery = "scheme=ocean"
		r.AddCookie(&http.Cookie{Name: SchemeCookie, Value: string(SchemeViolet)})
	})
	if !strings.Contains(body, `data-scheme="ocean"`) {
		t.Error("链接参数应当覆盖 Cookie")
	}

	// 非法值：忽略、不写 Cookie、回落到 Cookie / 缺省。
	body, cookies = render(func(r *http.Request) {
		r.URL.RawQuery = "scheme=neon"
		r.AddCookie(&http.Cookie{Name: SchemeCookie, Value: string(SchemeForest)})
	})
	if !strings.Contains(body, `data-scheme="forest"`) {
		t.Error("非法方案值应被忽略、沿用 Cookie 里的选择")
	}
	if findCookie(cookies, SchemeCookie) != nil {
		t.Error("非法方案值不该被记进 Cookie")
	}
}

// TestSchemeOptionsKeepQuery 验证方案切换链接保留其它参数，并且总带着当前的
// 明暗与布局值——换色相不该顺手把另外两个维度重置成默认。
func TestSchemeOptionsKeepQuery(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, PathLogin+"?next=%2Fi%2Fabc%2F&lang=en&scheme=ocean&theme=dark&style=split", nil)
	options := SchemeOptions(request, SchemeOcean, i18n.ZH)

	wantValues := []string{"slate", "ocean", "violet", "forest"}
	if len(options) != len(wantValues) {
		t.Fatalf("应当有四个方案选项，实际 %d", len(options))
	}
	for i, option := range options {
		if option.Value != wantValues[i] {
			t.Errorf("第 %d 个选项应为 %q，实际 %q", i+1, wantValues[i], option.Value)
		}
		parsed, err := url.Parse(option.Href)
		if err != nil {
			t.Fatalf("链接 %q 解析失败: %v", option.Href, err)
		}
		if parsed.Path != PathLogin {
			t.Errorf("链接应回到原页面 %s，实际 %q", PathLogin, parsed.Path)
		}
		query := parsed.Query()
		if got := query.Get("scheme"); got != wantValues[i] {
			t.Errorf("链接里的 scheme 应为 %q，实际 %q", wantValues[i], got)
		}
		if got := query.Get("theme"); got != "dark" {
			t.Errorf("链接应带上当前明暗值 dark，实际 %q", got)
		}
		if got := query.Get("style"); got != "split" {
			t.Errorf("链接应带上当前布局值 split，实际 %q", got)
		}
		if got := query.Get("next"); got != "/i/abc/" {
			t.Errorf("链接应保留 next 参数，实际 %q", got)
		}
		if got := query.Get("lang"); got != "en" {
			t.Errorf("链接应保留 lang 参数，实际 %q", got)
		}
		if option.Current != (option.Value == "ocean") {
			t.Errorf("只有「海蓝」应是当前项，%q 的 Current = %v", option.Value, option.Current)
		}
		if option.Label == "" {
			t.Errorf("选项 %q 缺少显示文案", option.Value)
		}
	}
}

// TestSchemeOptionsLocalized 验证方案切换文案跟着页面语言走。
func TestSchemeOptionsLocalized(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, PathLogin, nil)
	zh := SchemeOptions(request, SchemeSlate, i18n.ZH)
	en := SchemeOptions(request, SchemeSlate, i18n.EN)

	if zh[0].Label != "石墨蓝" || zh[3].Label != "墨绿" {
		t.Errorf("中文方案文案不对: %q / %q", zh[0].Label, zh[3].Label)
	}
	if en[0].Label != "Slate" || en[3].Label != "Forest" {
		t.Errorf("英文方案文案不对: %q / %q", en[0].Label, en[3].Label)
	}
	if !zh[0].Current {
		t.Error("缺省方案时第一项应是当前项")
	}
}
