package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// TestParseStyle 验证布局字面值归一化：别名（classic/dock、dashboard、sidebar/master）
// 一并收下；不认识的值必须被拒绝（ok=false），而不是悄悄当成默认布局接受。
func TestParseStyle(t *testing.T) {
	cases := []struct {
		raw  string
		want Style
		ok   bool
	}{
		{"console", StyleConsole, true},
		{"CONSOLE", StyleConsole, true},
		{" classic ", StyleConsole, true},
		{"dock", StyleConsole, true},
		{"overview", StyleOverview, true},
		{"dashboard", StyleOverview, true},
		{"split", StyleSplit, true},
		{"sidebar", StyleSplit, true},
		{"", DefaultStyle, false},
		{"zen", DefaultStyle, false},
	}
	for _, tc := range cases {
		got, ok := parseStyle(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseStyle(%q) = (%q, %v)，期望 (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// TestLoginPageStyle 验证布局偏好走通：?style= 渲染进 <html data-style> 并记住；
// Cookie 生效；非法值忽略且不留 Cookie；没选过也有缺省。
func TestLoginPageStyle(t *testing.T) {
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

	// 没选过：data-style 写缺省布局（登录页本身不随布局变，但属性总在）。
	body, cookies := render(nil)
	if !strings.Contains(body, `data-style="console"`) {
		t.Error("没选过布局时应渲染缺省布局 console")
	}
	if findCookie(cookies, StyleCookie) != nil {
		t.Error("没选过布局时不该凭空写一条 Cookie")
	}

	// 显式选择：渲染 + 记住。
	body, cookies = render(func(r *http.Request) { r.URL.RawQuery = "style=split" })
	if !strings.Contains(body, `data-style="split"`) {
		t.Error("?style=split 时应渲染分栏布局")
	}
	c := findCookie(cookies, StyleCookie)
	if c == nil || c.Value != string(StyleSplit) {
		t.Errorf("显式选布局应写回 Cookie，实际 %+v", c)
	}
	if c != nil && (c.Path != "/" || c.MaxAge <= 0 || c.HttpOnly) {
		t.Errorf("布局 Cookie 应整站长期有效且不加 HttpOnly，实际 Path=%q MaxAge=%d HttpOnly=%v",
			c.Path, c.MaxAge, c.HttpOnly)
	}

	// Cookie 记住：下次访问不用再带参数。
	body, _ = render(func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: StyleCookie, Value: string(StyleOverview)})
	})
	if !strings.Contains(body, `data-style="overview"`) {
		t.Error("Cookie 里选了总览时应渲染总览布局")
	}

	// ?style= 覆盖 Cookie。
	body, _ = render(func(r *http.Request) {
		r.URL.RawQuery = "style=console"
		r.AddCookie(&http.Cookie{Name: StyleCookie, Value: string(StyleOverview)})
	})
	if !strings.Contains(body, `data-style="console"`) {
		t.Error("链接参数应当覆盖 Cookie")
	}

	// 非法值：忽略、不写 Cookie、沿用 Cookie 里的选择。
	body, cookies = render(func(r *http.Request) {
		r.URL.RawQuery = "style=zen"
		r.AddCookie(&http.Cookie{Name: StyleCookie, Value: string(StyleSplit)})
	})
	if !strings.Contains(body, `data-style="split"`) {
		t.Error("非法布局值应被忽略、沿用 Cookie 里的选择")
	}
	if findCookie(cookies, StyleCookie) != nil {
		t.Error("非法布局值不该被记进 Cookie")
	}
}

// TestStyleOptionsKeepQuery 验证布局切换链接保留其它参数，并且总带着另外两个
// 维度的当前值——换布局不该顺手把明暗或色相重置。
func TestStyleOptionsKeepQuery(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, PathLogin+"?next=%2Fi%2Fabc%2F&lang=en&style=overview&theme=dark&scheme=violet", nil)
	options := StyleOptions(request, StyleOverview, i18n.ZH)

	wantValues := []string{"console", "overview", "split"}
	if len(options) != len(wantValues) {
		t.Fatalf("应当有三个布局选项，实际 %d", len(options))
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
		if got := query.Get("style"); got != wantValues[i] {
			t.Errorf("链接里的 style 应为 %q，实际 %q", wantValues[i], got)
		}
		if got := query.Get("theme"); got != "dark" {
			t.Errorf("链接应带上当前明暗值 dark，实际 %q", got)
		}
		if got := query.Get("scheme"); got != "violet" {
			t.Errorf("链接应带上当前方案值 violet，实际 %q", got)
		}
		if got := query.Get("next"); got != "/i/abc/" {
			t.Errorf("链接应保留 next 参数，实际 %q", got)
		}
		if got := query.Get("lang"); got != "en" {
			t.Errorf("链接应保留 lang 参数，实际 %q", got)
		}
		if option.Current != (option.Value == "overview") {
			t.Errorf("只有「总览」应是当前项，%q 的 Current = %v", option.Value, option.Current)
		}
		if option.Label == "" {
			t.Errorf("选项 %q 缺少显示文案", option.Value)
		}
	}
}

// TestStyleOptionsLocalized 验证布局切换文案跟着页面语言走。
func TestStyleOptionsLocalized(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, PathLogin, nil)
	zh := StyleOptions(request, StyleConsole, i18n.ZH)
	en := StyleOptions(request, StyleConsole, i18n.EN)

	if zh[0].Label != "控制台" || zh[1].Label != "总览" || zh[2].Label != "分栏" {
		t.Errorf("中文布局文案不对: %q / %q / %q", zh[0].Label, zh[1].Label, zh[2].Label)
	}
	if en[0].Label != "Console" || en[1].Label != "Overview" || en[2].Label != "Split" {
		t.Errorf("英文布局文案不对: %q / %q / %q", en[0].Label, en[1].Label, en[2].Label)
	}
	if !zh[0].Current {
		t.Error("缺省布局时第一项应是当前项")
	}
}
