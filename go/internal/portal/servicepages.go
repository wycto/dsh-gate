// 用户侧「我的服务」页（内网服务代理，见 docs/service-proxy.md）。
//
// 与首页「我的实例」完全分离（用户 2026-11-05 决议）：独立页面 /services，
// 首页仍是实例列表，两者只以页头导航链接互达，列表绝不混排。做法照
// handleIndex 的完整口径：requireUser、主题/配色/布局三组选项、语言切换、
// 同一套安全头与无脚本 CSP。
package portal

import (
	"html/template"
	"net/http"
	"strings"
	"sync"

	"github.com/wycto/dsh-gate/internal/assets"
	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/i18n"
)

// servicesPageOnce 缓存解析好的「我的服务」模板。
//
// 不放进 Server 结构体（那是 server.go 的地盘）：模板解析只要几十微秒，
// 首次访问解析一次、之后复用，与 NewServer 里「解析失败尽早报错」的差别
// 只是报错时机挪到了第一次访问——模板由 assets 测试锁死，风险可控。
var servicesPageOnce struct {
	sync.Once
	tmpl *template.Template
	err  error
}

func servicesPageTemplate() (*template.Template, error) {
	servicesPageOnce.Do(func() {
		servicesPageOnce.tmpl, servicesPageOnce.err = assets.ServicesTemplate()
	})
	return servicesPageOnce.tmpl, servicesPageOnce.err
}

// handleServicesPage 渲染「我的服务」：管理员见全部服务，普通用户只见被授权的。
// 路由注册在 server.go（ServicesPagePath），这里只实现处理器。
func (s *Server) handleServicesPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != ServicesPagePath {
		http.NotFound(w, r)
		return
	}
	lang := s.lang(w, r)
	theme := s.opts.Gate.Theme(w, r)
	scheme := s.opts.Gate.Scheme(w, r)
	style := s.opts.Gate.Style(w, r)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, i18n.T(lang, i18n.KeyLoginErrMethod), http.StatusMethodNotAllowed)
		return
	}
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// 类别开关：服务入口整体关掉时，还有实例入口就送回首页，两类都没有就
	// 给无权限页（与 handleIndex 同一口径，管理员不受开关约束）。
	if !s.canSeeServices(user) {
		if s.canSeeInstances(user) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.renderNoAccess(w, r, lang, user)
		return
	}

	services, err := s.opts.Store.VisibleServices(user)
	if err != nil {
		s.log.Error("读取可见服务失败", "user", user.Name, "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}

	type card struct {
		ID   string
		Name string
		Href string
		Note string
	}
	cards := make([]card, 0, len(services))
	for _, svc := range services {
		cards = append(cards, card{
			ID: svc.ID, Name: svc.Name,
			Href: ServiceMountPrefix + svc.ID + "/",
			Note: strings.TrimSpace(svc.Note),
		})
	}

	data := struct {
		Lang         string
		Title        string
		SignedInAs   string
		AdminLabel   string
		SignOut      string
		Enter        string
		NoServices   string
		BackLabel    string
		FooterPrefix string
		Version      string
		Host         string
		IsAdmin      bool
		Services     []card
		// ShowInstances / ShowServices：类别开关关掉的那一类导航链接不出现。
		ShowInstances bool
		ShowServices  bool
		OtherLang     string
		OtherLangHref string
		SignOutHref   string
		// 配色与布局（与首页 / 管理台同一套模型）。
		Theme       string
		ThemeLabel  string
		Themes      []auth.ThemeOption
		Scheme      string
		SchemeLabel string
		Schemes     []auth.ThemeOption
		Style       string
		StyleLabel  string
		Styles      []auth.ThemeOption
	}{
		Lang:          string(lang),
		Title:         i18n.T(lang, i18n.KeySvcPageTitle),
		SignedInAs:    i18n.T(lang, i18n.KeyPortalSignedInAs, displayNameOf(user)),
		AdminLabel:    i18n.T(lang, i18n.KeyPortalAdmin),
		SignOut:       i18n.T(lang, i18n.KeyPortalSignOut),
		Enter:         i18n.T(lang, i18n.KeyPortalEnter),
		NoServices:    i18n.T(lang, i18n.KeySvcPageEmpty),
		BackLabel:     i18n.T(lang, i18n.KeySvcBackInstances),
		FooterPrefix:  i18n.T(lang, i18n.KeyLoginFooterPrefix),
		Version:       s.version(),
		Host:          displayHost(r, lang),
		IsAdmin:       user.IsAdmin() && s.AdminEnabled(),
		Services:      cards,
		ShowInstances: s.canSeeInstances(user),
		ShowServices:  s.canSeeServices(user),
		OtherLang:     otherLangLabel(lang),
		OtherLangHref: "?lang=" + string(otherLang(lang)),
		SignOutHref:   auth.PathLogout,
		Theme:         string(theme),
		ThemeLabel:    i18n.T(lang, i18n.KeyThemeLabel),
		Themes:        auth.ThemeOptions(r, theme, lang),
		Scheme:        string(scheme),
		SchemeLabel:   i18n.T(lang, i18n.KeySchemeLabel),
		Schemes:       auth.SchemeOptions(r, scheme, lang),
		Style:         string(style),
		StyleLabel:    i18n.T(lang, i18n.KeyStyleLabel),
		Styles:        auth.StyleOptions(r, style, lang),
	}

	tmpl, err := servicesPageTemplate()
	if err != nil {
		s.log.Error("解析我的服务模板失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	body := &strings.Builder{}
	if err := tmpl.Execute(body, data); err != nil {
		s.log.Error("渲染我的服务页失败", "err", err)
		http.Error(w, i18n.T(lang, i18n.KeyPortalErrInternal), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	// 与首页同一套无脚本 CSP：页面只有内联样式，没有脚本、没有外部资源。
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; img-src data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body.String()))
}
