package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// TestParseTheme 验证字面值归一化。空串与 auto/system 都表示「跟随系统」，
// 其余不认识的值必须被拒绝（而不是当成跟随系统悄悄接受）。
func TestParseTheme(t *testing.T) {
	cases := []struct {
		raw  string
		want Theme
		ok   bool
	}{
		{"light", ThemeLight, true},
		{"Light", ThemeLight, true},
		{"  LIGHT  ", ThemeLight, true},
		{"dark", ThemeDark, true},
		{"DARK", ThemeDark, true},
		{"auto", ThemeAuto, true},
		{"SYSTEM", ThemeAuto, true},
		{"", ThemeAuto, false},
		{"neon", ThemeAuto, false},
	}
	for _, tc := range cases {
		got, ok := parseTheme(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseTheme(%q) = (%q, %v)，期望 (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLoginPageTheme 验证登录页真的跟着配色偏好渲染，而不只是目录里存着两套令牌。
func TestLoginPageTheme(t *testing.T) {
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

	findCookie := func(cookies []*http.Cookie) *http.Cookie {
		for _, c := range cookies {
			if c.Name == ThemeCookie {
				return c
			}
		}
		return nil
	}

	// 没选过：不写 data-theme，页面跟着系统走；页脚「跟随系统」是当前项。
	// data-scheme 则总在——色相没有「跟随系统」这一态，缺省就是石墨蓝。
	body, cookies := render(nil)
	if !strings.Contains(body, `<html lang="zh-CN" data-scheme="slate" data-style="console">`) {
		t.Error("没选过配色时不该写 data-theme（否则跟不上系统），但应写缺省方案 data-scheme")
	}
	if !strings.Contains(body, `theme=auto" aria-current="true"`) {
		t.Error("没选过配色时页脚的「跟随系统」应当是当前项")
	}
	if findCookie(cookies) != nil {
		t.Error("没选过配色时不该凭空写一条 Cookie")
	}

	// 显式暗色：写进 <html data-theme>，同时记住偏好。
	body, cookies = render(func(r *http.Request) { r.URL.RawQuery = "theme=dark" })
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Error("?theme=dark 时应渲染暗色页面")
	}
	if !strings.Contains(body, `theme=dark" aria-current="true"`) {
		t.Error("?theme=dark 时页脚的「暗色」应当是当前项")
	}
	darkCookie := findCookie(cookies)
	if darkCookie == nil {
		t.Fatal("显式选配色时应写回偏好 Cookie")
	}
	if darkCookie.Value != string(ThemeDark) {
		t.Errorf("配色 Cookie 值应为 %q，实际 %q", ThemeDark, darkCookie.Value)
	}
	if darkCookie.Path != "/" || darkCookie.MaxAge <= 0 {
		t.Errorf("配色 Cookie 应整站有效且长期保存，实际 Path=%q MaxAge=%d", darkCookie.Path, darkCookie.MaxAge)
	}
	if darkCookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("配色 Cookie 应为 SameSite=Lax，实际 %v", darkCookie.SameSite)
	}
	if darkCookie.HttpOnly {
		t.Error("配色 Cookie 不是凭据，不该加 HttpOnly")
	}

	// Cookie 记住：下次访问不用再带参数。
	body, _ = render(func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: ThemeCookie, Value: string(ThemeLight)})
	})
	if !strings.Contains(body, `data-theme="light"`) {
		t.Error("Cookie 里选了亮色时应渲染亮色页面")
	}
	if !strings.Contains(body, `theme=light" aria-current="true"`) {
		t.Error("Cookie 里选了亮色时页脚的「亮色」应当是当前项")
	}

	// ?theme= 覆盖 Cookie，而不是被 Cookie 压住。
	body, _ = render(func(r *http.Request) {
		r.URL.RawQuery = "theme=light"
		r.AddCookie(&http.Cookie{Name: ThemeCookie, Value: string(ThemeDark)})
	})
	if !strings.Contains(body, `data-theme="light"`) {
		t.Error("链接参数应当覆盖 Cookie")
	}

	// 选回「跟随系统」：删 Cookie，页面回到不写 data-theme。
	body, cookies = render(func(r *http.Request) {
		r.URL.RawQuery = "theme=auto"
		r.AddCookie(&http.Cookie{Name: ThemeCookie, Value: string(ThemeDark)})
	})
	if !strings.Contains(body, `<html lang="zh-CN" data-scheme="slate" data-style="console">`) {
		t.Error("选回跟随系统时不该再写 data-theme")
	}
	if autoCookie := findCookie(cookies); autoCookie == nil {
		t.Error("选回跟随系统时应清除偏好 Cookie")
	} else if autoCookie.MaxAge >= 0 {
		t.Errorf("清除用的 Cookie 应带负数 MaxAge，实际 %d", autoCookie.MaxAge)
	}

	// 认不出来的值当作没带参数：不写 Cookie，也不报错。
	body, cookies = render(func(r *http.Request) { r.URL.RawQuery = "theme=neon" })
	if !strings.Contains(body, `<html lang="zh-CN" data-scheme="slate" data-style="console">`) {
		t.Error("非法配色值应被忽略")
	}
	if findCookie(cookies) != nil {
		t.Error("非法配色值不该被记进 Cookie")
	}
}

// TestThemeOptionsKeepQuery 验证切换链接不会丢掉当前页面的其它参数。
//
// 登录页带 ?next=（否则登录后回不到原页面），管理台带 ?tab=（否则点一下配色
// 就跳回第一页），两者都必须原样留在链接里。
func TestThemeOptionsKeepQuery(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, PathLogin+"?next=%2Fi%2Fabc%2F&lang=en&theme=light", nil)
	options := ThemeOptions(request, ThemeLight, i18n.ZH)

	wantValues := []string{ThemeValueAuto, ThemeValueLight, ThemeValueDark}
	if len(options) != len(wantValues) {
		t.Fatalf("应当有三个配色选项，实际 %d", len(options))
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
		if got := parsed.Query().Get("theme"); got != wantValues[i] {
			t.Errorf("链接里的 theme 应为 %q，实际 %q", wantValues[i], got)
		}
		if got := parsed.Query().Get("next"); got != "/i/abc/" {
			t.Errorf("链接应保留 next 参数，实际 %q", got)
		}
		if got := parsed.Query().Get("lang"); got != "en" {
			t.Errorf("链接应保留 lang 参数，实际 %q", got)
		}
		if option.Current != (option.Value == ThemeValueLight) {
			t.Errorf("只有「亮色」应是当前项，%q 的 Current = %v", option.Value, option.Current)
		}
		if option.Label == "" {
			t.Errorf("选项 %q 缺少显示文案", option.Value)
		}
	}
}

// TestThemeOptionsLocalized 验证页脚的切换文案跟着页面语言走。
func TestThemeOptionsLocalized(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, PathLogin, nil)
	zh := ThemeOptions(request, ThemeAuto, i18n.ZH)
	en := ThemeOptions(request, ThemeAuto, i18n.EN)

	if zh[0].Label != "跟随系统" {
		t.Errorf("中文的「跟随系统」文案不对: %q", zh[0].Label)
	}
	if en[0].Label != "System" {
		t.Errorf("英文的「跟随系统」文案不对: %q", en[0].Label)
	}
	if !zh[0].Current || en[0].Current != true {
		t.Error("跟随系统时第一项应是当前项")
	}
}
