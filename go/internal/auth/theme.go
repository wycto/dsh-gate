// 界面配色（明暗主题）偏好的解析与切换。
//
// 三个页面共用一套模型：**服务端在渲染前就决定用哪套配色**，写进 <html data-theme>，
// 页面自身不做任何判断。做法与语言偏好一致（`?lang=` 写 Cookie），好处是配色本身
// 不依赖脚本，CSP 也不必为了配色放开 script-src；代价是切配色要刷新一次页面。
//
// 三态而不是两态：「跟随系统 / 亮色 / 暗色」。两态会让「跟随系统」这个默认行为
// 无法回去——用户手动选过一次亮色之后，系统换主题页面就再也不跟了。
package auth

import (
	"net/http"
	"strings"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// ThemeCookie 是记住界面配色偏好的 Cookie 名。
//
// 它不是凭据（被改掉最多是自己看见另一套配色），因此不加 HttpOnly，与 LangCookie
// 同一个口径。名字带 dsh_gate_ 前缀，与 dsh 自己的 Cookie 同处一个浏览器也不打架。
const ThemeCookie = "dsh_gate_theme"

// Theme 是页面配色。空串表示「跟随系统」。
type Theme string

const (
	// ThemeAuto 跟随系统的 prefers-color-scheme（默认）。
	ThemeAuto Theme = ""
	// ThemeLight 亮色。
	ThemeLight Theme = "light"
	// ThemeDark 暗色。
	ThemeDark Theme = "dark"
)

// 链接与 Cookie 里出现的字面值。
//
// ThemeAuto 的空串不适合直接出现在 `?theme=` 里（`?theme=` 与「没带参数」分不开），
// 因此「跟随系统」显式用一个 auto。
const (
	ThemeValueAuto  = "auto"
	ThemeValueLight = "light"
	ThemeValueDark  = "dark"
)

// parseTheme 把链接或 Cookie 里的字面值归一化成 Theme。
//
// 多余的别名（system）一并收下：手写链接的人不该因为换个词就被忽略。
func parseTheme(raw string) (Theme, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ThemeValueLight:
		return ThemeLight, true
	case ThemeValueDark:
		return ThemeDark, true
	case ThemeValueAuto, "system":
		return ThemeAuto, true
	}
	return ThemeAuto, false
}

// themeOf 只读地解析当前明暗（?theme= > Cookie > 跟随系统），不写 Cookie。
//
// 拼其它维度的切换链接时用它：布局/色相的链接要带上当前明暗值，
// 但读值不该产生写副作用。
func themeOf(r *http.Request) Theme {
	if explicit := strings.TrimSpace(r.URL.Query().Get("theme")); explicit != "" {
		if mode, ok := parseTheme(explicit); ok {
			return mode
		}
	}
	if c, err := r.Cookie(ThemeCookie); err == nil {
		if mode, ok := parseTheme(c.Value); ok {
			return mode
		}
	}
	return ThemeAuto
}

// Theme 解析这次请求该用哪套配色，并把显式选择记进 Cookie。
//
// 优先级：`?theme=`（一次性写入 / 清除 Cookie）> Cookie > 跟随系统。
// 显式选「跟随系统」时要把 Cookie 删掉：留着一条 auto 虽然也能用，但删掉之后
// 浏览器回到「从没选过」的状态，语义更干净。
func (g *Gate) Theme(w http.ResponseWriter, r *http.Request) Theme {
	if explicit := strings.TrimSpace(r.URL.Query().Get("theme")); explicit != "" {
		if mode, ok := parseTheme(explicit); ok {
			g.storeTheme(w, mode)
			return mode
		}
	}
	if c, err := r.Cookie(ThemeCookie); err == nil {
		if mode, ok := parseTheme(c.Value); ok {
			return mode
		}
	}
	return ThemeAuto
}

// storeTheme 写入 / 清除配色偏好 Cookie。
func (g *Gate) storeTheme(w http.ResponseWriter, mode Theme) {
	c := &http.Cookie{
		Name:     ThemeCookie,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   g.opts.CookieSecure,
	}
	if mode == ThemeAuto {
		c.Value = ""
		c.MaxAge = -1
	} else {
		c.Value = string(mode)
	}
	http.SetCookie(w, c)
}

// ThemeOption 是页脚配色切换里的一个选项。
type ThemeOption struct {
	// Value 是链接携带的字面值（auto / light / dark）。
	Value string
	// Label 是显示文案，已按语言翻译。
	Label string
	// Href 保留了当前请求的其它查询参数（?next=、?tab=、?lang=）。
	Href string
	// Current 表示这一项正是当前生效的配色。
	Current bool
}

// ThemeOptions 生成页脚配色切换的三个链接。
//
// 链接都是普通 GET：点一下带着 `?theme=` 回到同一页，服务端据此写 Cookie 并按新
// 配色渲染。没有脚本，也就不存在「点了没反应」。
func ThemeOptions(r *http.Request, current Theme, lang i18n.Lang) []ThemeOption {
	items := []struct {
		value string
		mode  Theme
		label string
	}{
		{ThemeValueAuto, ThemeAuto, i18n.T(lang, i18n.KeyThemeAuto)},
		{ThemeValueLight, ThemeLight, i18n.T(lang, i18n.KeyThemeLight)},
		{ThemeValueDark, ThemeDark, i18n.T(lang, i18n.KeyThemeDark)},
	}
	options := make([]ThemeOption, 0, len(items))
	for _, item := range items {
		options = append(options, ThemeOption{
			Value:   item.value,
			Label:   item.label,
			Href:    themeHref(r, item.value),
			Current: current == item.mode,
		})
	}
	return options
}

// themeHref 把 `?theme=<value>` 拼进当前地址，其它查询参数原样保留，
// 并补齐色相与布局的当前值（见 prefHref 的说明）。
func themeHref(r *http.Request, value string) string {
	return prefHref(r, "theme", value)
}
