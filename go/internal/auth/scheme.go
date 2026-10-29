// 界面配色**方案**（色相）偏好的解析与切换。
//
// 与 theme.go 的明暗是两个正交维度：
//   - theme（明暗）：跟随系统 / 亮色 / 暗色——决定令牌往哪边取。
//   - scheme（方案）：石墨蓝 / 海蓝 / 绛紫 / 墨绿黄铜——决定这套令牌是什么色相。
//
// 模型与 theme.go 完全一致：服务端在渲染前定稿，写进 <html data-scheme>，页面
// 自身不做判断、不用脚本，CSP 不必为配色放开 script-src。
//
// 方案没有「跟随系统」这一态：系统只告知明暗，不告知色相偏好。因此 Scheme 永远
// 有一个确定的值（缺省 Slate），模板可以无条件写 data-scheme——共享令牌里的
// 选择器也因此总能命中自己那一块。
package auth

import (
	"net/http"
	"strings"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// SchemeCookie 是记住界面配色方案偏好的 Cookie 名。
//
// 与 ThemeCookie 同一个口径：不是凭据，被改掉最多是自己看见另一套色相，
// 所以不加 HttpOnly。
const SchemeCookie = "dsh_gate_scheme"

// Scheme 是配色方案（色相）。
type Scheme string

const (
	// SchemeSlate 石墨蓝：中性灰底 + 标准蓝强调，默认方案。
	SchemeSlate Scheme = "slate"
	// SchemeOcean 海蓝。
	SchemeOcean Scheme = "ocean"
	// SchemeViolet 绛紫。
	SchemeViolet Scheme = "violet"
	// SchemeForest 墨绿黄铜：0.3.x 时代的唯一配色，降级为一个可选方案。
	SchemeForest Scheme = "forest"
)

// DefaultScheme 是没选过时的方案。
const DefaultScheme = SchemeSlate

// schemes 是页脚切换器里的全部方案，顺序即展示顺序。
var schemes = []Scheme{SchemeSlate, SchemeOcean, SchemeViolet, SchemeForest}

// parseScheme 把链接或 Cookie 里的字面值归一化成 Scheme。
func parseScheme(raw string) (Scheme, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(SchemeSlate), "default":
		return SchemeSlate, true
	case string(SchemeOcean):
		return SchemeOcean, true
	case string(SchemeViolet):
		return SchemeViolet, true
	case string(SchemeForest), "brass", "green":
		return SchemeForest, true
	}
	return DefaultScheme, false
}

// schemeLabelKey 返回该方案显示文案的 i18n 键。
func schemeLabelKey(s Scheme) string {
	switch s {
	case SchemeOcean:
		return i18n.KeySchemeOcean
	case SchemeViolet:
		return i18n.KeySchemeViolet
	case SchemeForest:
		return i18n.KeySchemeForest
	default:
		return i18n.KeySchemeSlate
	}
}

// Scheme 解析这次请求该用哪个配色方案，并把选择记进 Cookie。
//
// 优先级与明暗一致：`?scheme=` > Cookie > 默认（石墨蓝）。
func (g *Gate) Scheme(w http.ResponseWriter, r *http.Request) Scheme {
	if explicit := strings.TrimSpace(r.URL.Query().Get("scheme")); explicit != "" {
		if mode, ok := parseScheme(explicit); ok {
			g.storeScheme(w, mode)
			return mode
		}
		// 认不出的值当作没带参数：不写 Cookie，也不覆盖已有偏好。
	}
	return schemeOf(r)
}

// schemeOf 只读地解析当前方案（?scheme= > Cookie > 默认），不写 Cookie。
func schemeOf(r *http.Request) Scheme {
	if explicit := strings.TrimSpace(r.URL.Query().Get("scheme")); explicit != "" {
		if mode, ok := parseScheme(explicit); ok {
			return mode
		}
	}
	if c, err := r.Cookie(SchemeCookie); err == nil {
		if mode, ok := parseScheme(c.Value); ok {
			return mode
		}
	}
	return DefaultScheme
}

// storeScheme 写入配色方案偏好 Cookie。
//
// 与明暗不同：这里没有「清除偏好」这一态（默认值本身就是一个方案），
// 所以永远写值、不删 Cookie。
func (g *Gate) storeScheme(w http.ResponseWriter, mode Scheme) {
	http.SetCookie(w, &http.Cookie{
		Name:     SchemeCookie,
		Value:    string(mode),
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
		Secure:   g.opts.CookieSecure,
	})
}

// SchemeOptions 生成页脚方案切换的四个链接。
//
// 链接都带上当前生效的明暗与布局值：否则从「暗色 + 石墨」点一下「海蓝」，
// 明暗会退回跟随系统——用户只换了色相，不该被顺手改掉其它维度。
func SchemeOptions(r *http.Request, current Scheme, lang i18n.Lang) []ThemeOption {
	options := make([]ThemeOption, 0, len(schemes))
	for _, item := range schemes {
		options = append(options, ThemeOption{
			Value:   string(item),
			Label:   i18n.T(lang, schemeLabelKey(item)),
			Href:    schemeHref(r, item),
			Current: current == item,
		})
	}
	return options
}

// schemeHref 把 `?scheme=<值>` 拼进当前地址，其余参数保留，
// 并补齐明暗与布局的当前值（见 prefHref 的说明）。
func schemeHref(r *http.Request, value Scheme) string {
	return prefHref(r, "scheme", string(value))
}

// themeValueOf 把 Theme 翻成链接里该写的字面值（跟随系统 → auto）。
func themeValueOf(theme Theme) string {
	switch theme {
	case ThemeLight:
		return ThemeValueLight
	case ThemeDark:
		return ThemeValueDark
	default:
		return ThemeValueAuto
	}
}
