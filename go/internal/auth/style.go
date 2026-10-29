// 界面**布局风格**（console / overview / split）偏好的解析与切换。
//
// 与明暗（theme.go）、色相（scheme.go）是第三个正交维度：
//   - console 控制台（默认）：底部 Dock 分页 + 单列高密度面板，现在的样子。
//   - overview 总览：顶部横向页签 + 统计卡，先给「现在什么状态」的一眼结论。
//   - split 分栏：左侧竖排导航 + 右侧详情区（Linear / GitHub 式后台骨架）。
//
// 三者共用同一份 DOM：导航选中本来就靠 body:has(#tab-x:checked) 驱动，
// 一组成员相同的 <label for> 分别放进 Dock / 顶栏 / 侧栏，CSS 按 data-style
// 决定哪套可见、面板怎么排——依旧零脚本。
//
// 模型与 scheme.go 一致：服务端渲染前定稿，写进 <html data-style>，永远有值
// （布局没有「跟随系统」这一态）。
package auth

import (
	"net/http"
	"strings"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// StyleCookie 是记住界面布局风格偏好的 Cookie 名。
const StyleCookie = "dsh_gate_style"

// Style 是布局风格。
type Style string

const (
	// StyleConsole 控制台：底部 Dock + 单列面板（默认）。
	StyleConsole Style = "console"
	// StyleOverview 总览：顶部页签 + 统计卡。
	StyleOverview Style = "overview"
	// StyleSplit 分栏：左侧栏 + 右侧详情。
	StyleSplit Style = "split"
)

// DefaultStyle 是没选过时的布局。
const DefaultStyle = StyleConsole

// styles 是页脚布局切换器里的全部选项，顺序即展示顺序。
var styles = []Style{StyleConsole, StyleOverview, StyleSplit}

// parseStyle 把链接或 Cookie 里的字面值归一化成 Style。
//
// classic / dock 是 console 的别名：老书签与手写链接不该因为换个词被忽略。
func parseStyle(raw string) (Style, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(StyleConsole), "classic", "dock":
		return StyleConsole, true
	case string(StyleOverview), "dashboard":
		return StyleOverview, true
	case string(StyleSplit), "sidebar", "master":
		return StyleSplit, true
	}
	return DefaultStyle, false
}

// styleLabelKey 返回该布局显示文案的 i18n 键。
func styleLabelKey(s Style) string {
	switch s {
	case StyleOverview:
		return i18n.KeyStyleOverview
	case StyleSplit:
		return i18n.KeyStyleSplit
	default:
		return i18n.KeyStyleConsole
	}
}

// Style 解析这次请求该用哪种布局，并把选择记进 Cookie。
func (g *Gate) Style(w http.ResponseWriter, r *http.Request) Style {
	if explicit := strings.TrimSpace(r.URL.Query().Get("style")); explicit != "" {
		if mode, ok := parseStyle(explicit); ok {
			g.storeStyle(w, mode)
			return mode
		}
		// 认不出的值当作没带参数：不写 Cookie，也不覆盖已有偏好。
	}
	return styleOf(r)
}

// styleOf 只读地解析当前布局（?style= > Cookie > 默认），不写 Cookie。
//
// 拼切换链接时用它：布局链接要带上「其它两个维度当前的值」，
// 但读值不该产生写副作用。
func styleOf(r *http.Request) Style {
	if explicit := strings.TrimSpace(r.URL.Query().Get("style")); explicit != "" {
		if mode, ok := parseStyle(explicit); ok {
			return mode
		}
	}
	if c, err := r.Cookie(StyleCookie); err == nil {
		if mode, ok := parseStyle(c.Value); ok {
			return mode
		}
	}
	return DefaultStyle
}

// storeStyle 写入布局偏好 Cookie（永远有值，没有清除态）。
func (g *Gate) storeStyle(w http.ResponseWriter, mode Style) {
	http.SetCookie(w, &http.Cookie{
		Name:     StyleCookie,
		Value:    string(mode),
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   g.opts.CookieSecure,
	})
}

// StyleOptions 生成页脚布局切换的三个链接。
func StyleOptions(r *http.Request, current Style, lang i18n.Lang) []ThemeOption {
	options := make([]ThemeOption, 0, len(styles))
	for _, item := range styles {
		options = append(options, ThemeOption{
			Value:   string(item),
			Label:   i18n.T(lang, styleLabelKey(item)),
			Href:    styleHref(r, item),
			Current: current == item,
		})
	}
	return options
}

// styleHref 把 `?style=<值>` 拼进当前地址，并带上另外两个维度的当前值。
func styleHref(r *http.Request, value Style) string {
	return prefHref(r, "style", string(value))
}

// prefHref 在当前地址上改一个偏好参数，并补齐另外两个偏好参数的当前值。
//
// 补齐不是多余的：三个维度各自有 Cookie，链接里只带自己那一个的话，
// 点「布局」会把「明暗」弹回跟随系统——用户只动了一个维度。
// 其余查询参数（?next=、?tab=、?lang=）原样保留。
func prefHref(r *http.Request, key, value string) string {
	query := r.URL.Query()
	query.Set(key, value)
	if key != "theme" {
		query.Set("theme", themeValueOf(themeOf(r)))
	}
	if key != "scheme" {
		query.Set("scheme", string(schemeOf(r)))
	}
	if key != "style" {
		query.Set("style", string(styleOf(r)))
	}
	target := r.URL.Path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}
