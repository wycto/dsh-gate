package portal

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/cookie"
)

var csrfFieldPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// adminCSRF 打开管理台，取出 CSRF Cookie 与表单里的令牌（双提交校验需要两者都在）。
func adminCSRF(t *testing.T, f *portalFixture, session *http.Cookie) (*http.Cookie, string) {
	t.Helper()
	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	var csrfCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == CSRFCookie {
			csrfCookie = c
		}
	}
	if csrfCookie == nil {
		t.Fatal("管理台没有下发 CSRF Cookie")
	}
	m := csrfFieldPattern.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatal("管理台表单里没有 csrf 字段")
	}
	return csrfCookie, m[1]
}

// postAdmin 走一次真实的表单提交（带 csrf 字段、CSRF Cookie 与 Origin，模拟浏览器）。
func postAdmin(t *testing.T, f *portalFixture, session *http.Cookie, csrfCookie *http.Cookie, csrf string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := url.Values{}
	for k, v := range form {
		body[k] = v
	}
	if csrf != "" {
		body.Set("csrf", csrf)
	}
	request := httptest.NewRequest(http.MethodPost, AdminPath, strings.NewReader(body.Encode()))
	request.Host = "portal.test"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://portal.test")
	request.AddCookie(session)
	if csrfCookie != nil {
		request.AddCookie(csrfCookie)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestAdminRequiresAdminRole(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "alice", "alice-password-123")

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问管理台应 403，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "管理员") {
		t.Errorf("应提示需要管理员权限，实际 %q", rec.Body.String())
	}
}

func TestAdminPageShowsStateWithoutSecrets(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员访问管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"alice", "bob", "alice-main", "shared-team", "授权"} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少 %q", want)
		}
	}
	// 口令哈希绝不能出现在页面上。
	if strings.Contains(body, "pbkdf2") || strings.Contains(body, "$600000$") {
		t.Error("管理台页面泄漏了口令哈希")
	}
}

// TestAdminPageShowsLaunchArgs 验证卡片把运行时追加的参数也摆出来：启动命令
// 本身只有 dsh 从哪来，真正生效的监听地址与端口在追加参数里。用户问「填的
// 端口用没用上」时，页面上必须有这一行可看。
func TestAdminPageShowsLaunchArgs(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	inst := &Instance{Name: "arg-check", DshHome: t.TempDir(), DshPort: 12080, ListenHost: "0.0.0.0"}
	if err := f.store.CreateInstance(inst); err != nil {
		t.Fatalf("创建实例失败: %v", err)
	}

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"启动参数", "web --host 0.0.0.0 --port 12080 --no-open"} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少 %q（启动命令里看不到实际端口）", want)
		}
	}
}

// TestAdminTabParam 验证分页参数：?tab= 选中对应分页；无效值回落默认；
// 没接运行环境时 env 不可用（否则点进去是一张空面板）。
func TestAdminTabParam(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	checked := func(tab string) string {
		return `id="tab-` + tab + `" class="tab-input" checked`
	}
	// 合法值：对应的单选钮被预选。
	for _, tab := range []string{"users", "grants", "instances", "audit"} {
		body := f.get(t, AdminPath+"?tab="+tab, session).Body.String()
		if !strings.Contains(body, checked(tab)) {
			t.Errorf("?tab=%s 应预选该分页", tab)
		}
	}
	// env 面板只在接了 Installer 时存在；本 fixture 没接，请求它应回落默认。
	body := f.get(t, AdminPath+"?tab=env", session).Body.String()
	if !strings.Contains(body, checked("instances")) {
		t.Error("?tab=env 在无 Installer 时应回落到默认分页（实例）")
	}
	// 乱值同样回落默认。
	body = f.get(t, AdminPath+"?tab=nonsense", session).Body.String()
	if !strings.Contains(body, checked("instances")) {
		t.Error("无效 tab 应回落到默认分页（实例）")
	}
}

// TestAdminPostRedirectKeepsTab 验证 PRG 回跳把用户送回提交前那一页：
// 在「用户」页创建用户后，Location 应带 ?tab=users，操作完不被弹回别处。
func TestAdminPostRedirectKeepsTab(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	cases := []struct{ action, wantTab string }{
		{"user.create", "users"},
		{"grants.save", "grants"},
		{"instance.remove", "instances"},
		{"env.install", "instances"}, // 无 Installer：env 不存在，回落默认
	}
	for _, c := range cases {
		form := url.Values{"action": {c.action}, "name": {"alice"}}
		if c.action == "user.create" {
			form = url.Values{"action": {c.action}, "name": {"newbie"}, "password": {"newbie-password-1"}, "role": {"user"}}
		}
		if c.action == "instance.remove" {
			form = url.Values{"action": {c.action}, "instance_id": {"no-such"}, "name": {"x"}}
		}
		rec := postAdmin(t, f, session, csrfCookie, csrf, form)
		body := rec.Body.String()
		if rec.Code == http.StatusSeeOther {
			// 成功：回跳应带 tab=，回跳后的页面应预选那一页。
			if loc := rec.Header().Get("Location"); !strings.Contains(loc, "tab="+c.wantTab) {
				t.Errorf("%s 的回跳应带 tab=%s，实际 %q", c.action, c.wantTab, loc)
			}
			u, _ := url.Parse(rec.Header().Get("Location"))
			body = f.get(t, AdminPath+"?"+u.RawQuery, session).Body.String()
		} else if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 303 或 400（失败动作重渲染并保留分页），实际 %d", c.action, rec.Code)
		}
		// 无论成功还是失败重渲染，都应停留在提交前那一页。
		if !strings.Contains(body, `id="tab-`+c.wantTab+`" class="tab-input" checked`) {
			t.Errorf("%s 之后应停留在 %s 分页", c.action, c.wantTab)
		}
	}
}

func TestAdminRejectsMissingOrWrongCSRF(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	// 不带令牌（也没带 Cookie）：拒绝，且不建用户。
	rec := postAdmin(t, f, session, nil, "", url.Values{
		"action": {"user.create"}, "name": {"mallory"}, "password": {"mallory-password-1"}, "role": {"user"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 令牌应 403，实际 %d", rec.Code)
	}
	if _, err := f.store.UserByName("mallory"); err == nil {
		t.Fatal("CSRF 校验失败时绝不能建出用户")
	}

	// 带 Cookie 但表单令牌不对：同样拒绝。
	csrfCookie, _ := adminCSRF(t, f, session)
	rec = postAdmin(t, f, session, csrfCookie, "not-the-token", url.Values{
		"action": {"user.create"}, "name": {"mallory"}, "password": {"mallory-password-1"}, "role": {"user"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("令牌不匹配应 403，实际 %d", rec.Code)
	}
	if _, err := f.store.UserByName("mallory"); err == nil {
		t.Fatal("令牌不匹配时绝不能建出用户")
	}
	// 审计里应留下被拒绝的痕迹。
	entries, _ := f.store.RecentAudit(20)
	found := false
	for _, e := range entries {
		if e.Action == "admin.csrf-rejected" {
			found = true
		}
	}
	if !found {
		t.Error("CSRF 被拒绝应记审计")
	}
}

func TestAdminCreatesUserAndGrantsAccess(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 建用户。
	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"user.create"}, "name": {"carol"},
		"password": {"carol-password-123"}, "role": {"user"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("建用户成功应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	carol, err := f.store.UserByName("carol")
	if err != nil {
		t.Fatalf("用户没建出来: %v", err)
	}
	if carol.Role != RoleUser {
		t.Errorf("角色应为 user，实际 %q", carol.Role)
	}

	// 授权：把 shared-team 给 carol。
	rec = postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"grants.save"}, "name": {"carol"}, "instance": {f.shared.ID},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("保存授权应 303，实际 %d", rec.Code)
	}
	if _, err := f.store.Accessible(carol, f.shared.ID); err != nil {
		t.Fatalf("carol 应能访问 shared-team: %v", err)
	}
	if _, err := f.store.Accessible(carol, f.aliceIns.ID); err == nil {
		t.Error("carol 不该能访问别人的实例")
	}

	// 新建的用户能正常登录管理端。
	carolSession := f.login(t, "carol", "carol-password-123")
	page := f.get(t, "/", carolSession)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "shared-team") {
		t.Fatalf("新用户登录后应看到被授权的实例，实际 %d", page.Code)
	}

	// 审计：两个动作各留一条。
	entries, _ := f.store.RecentAudit(20)
	actions := map[string]bool{}
	for _, e := range entries {
		actions[e.Action] = true
	}
	for _, want := range []string{"admin.user.create", "admin.grants.save"} {
		if !actions[want] {
			t.Errorf("审计缺少 %q", want)
		}
	}
}

func TestAdminDisableRevokesPortalSession(t *testing.T) {
	f := newPortalFixture(t)
	bobSession := f.login(t, "bob", "bob-password-123")
	if rec := f.get(t, "/", bobSession); rec.Code != http.StatusOK {
		t.Fatalf("停用前 bob 应能访问，实际 %d", rec.Code)
	}

	rootSession := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, rootSession)
	rec := postAdmin(t, f, rootSession, csrfCookie, csrf, url.Values{
		"action": {"user.toggle"}, "name": {"bob"}, "disabled": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("停用应 303，实际 %d", rec.Code)
	}
	// 停用同时吊销会话：bob 下一次请求就被送去登录页。
	if rec := f.get(t, "/", bobSession); rec.Code != http.StatusSeeOther {
		t.Fatalf("被停用后应立即失效，实际 %d", rec.Code)
	}
}

func TestAdminPasswordChangeRevokesSessions(t *testing.T) {
	f := newPortalFixture(t)
	aliceSession := f.login(t, "alice", "alice-password-123")

	rootSession := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, rootSession)
	rec := postAdmin(t, f, rootSession, csrfCookie, csrf, url.Values{
		"action": {"user.passwd"}, "name": {"alice"}, "password": {"new-alice-password"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("改密应 303，实际 %d", rec.Code)
	}
	if rec := f.get(t, "/", aliceSession); rec.Code != http.StatusSeeOther {
		t.Fatalf("改密后旧会话应立即失效，实际 %d", rec.Code)
	}
	// 新口令可登录。
	f.login(t, "alice", "new-alice-password")
}

func TestAdminSuperAdminProtectionShowsMessage(t *testing.T) {
	// fixture 的 root 是第一个注册的账号：超管保护先于最后管理员保护拦下它。
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"user.toggle"}, "name": {"root"}, "disabled": {"1"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("拒绝操作超级管理员应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "超级管理员") {
		t.Errorf("应给出「超级管理员」的说明，实际 body 里没有")
	}
	root, err := f.store.UserByName("root")
	if err != nil || root.Disabled {
		t.Fatalf("超级管理员不该被停用: %+v %v", root, err)
	}
}

func TestAdminRoleChangeAndDelete(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 把 bob 提升为管理员。
	if rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"user.role"}, "name": {"bob"}, "role": {"admin"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("改角色应 303，实际 %d", rec.Code)
	}
	bob, _ := f.store.UserByName("bob")
	if bob.Role != RoleAdmin {
		t.Fatalf("bob 应已是管理员，实际 %q", bob.Role)
	}
	// 此时可以删除 root 吗？不可以——root 仍是管理员之一，但 bob 也在，故允许。
	if rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"user.delete"}, "name": {"bob"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("有备份管理员时应能删除: %d", rec.Code)
	}
	if _, err := f.store.UserByName("bob"); err == nil {
		t.Error("bob 应已被删除")
	}
}

func TestAdminPageLanguage(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	if body := f.get(t, AdminPath, session).Body.String(); !strings.Contains(body, "管理台") {
		t.Error("默认应渲染中文管理台")
	}
	if body := f.get(t, AdminPath+"?lang=en", session).Body.String(); !strings.Contains(body, "Admin") {
		t.Error("?lang=en 时应渲染英文管理台")
	}
}

// TestAdminPageTheme 验证管理台也跟着配色偏好渲染，并且切换链接不会丢掉 ?tab=。
//
// 管理台刻意不用 JavaScript，配色切换因此是普通链接 + 服务端 Cookie：点一下换一套
// 令牌重新渲染，当前分页必须还在，否则换个配色就被弹回第一页。
func TestAdminPageTheme(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	// 默认：不写 data-theme，页面跟着系统走；页脚「跟随系统」是当前项。
	// data-scheme 总在——色相没有跟随系统这一态，缺省石墨蓝。
	body := f.get(t, AdminPath, session).Body.String()
	if !strings.Contains(body, `<html lang="zh-CN" data-scheme="slate" data-style="console">`) {
		t.Error("没选过配色时管理台不该写 data-theme，但应写缺省方案")
	}
	if !strings.Contains(body, `theme=auto" aria-current="true"`) {
		t.Error("没选过配色时页脚的「跟随系统」应当是当前项")
	}
	// 页脚必须有方案胶囊（四个色相之一被标为当前）。
	if !strings.Contains(body, "scheme=ocean") {
		t.Error("页脚应有方案切换链接")
	}

	// ?theme=dark&tab=users：暗色渲染 + 记住偏好 + 切换链接留住分页。
	recorder := f.get(t, AdminPath+"?theme=dark&tab=users", session)
	body = recorder.Body.String()
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Error("?theme=dark 时管理台应渲染暗色")
	}
	if !strings.Contains(body, `theme=dark" aria-current="true"`) {
		t.Error("?theme=dark 时页脚的「暗色」应当是当前项")
	}
	if !strings.Contains(body, "tab=users") {
		t.Error("配色切换链接应保留当前分页 ?tab=")
	}
	// 方案链接同样要留住 ?tab= 与明暗值：点一下色相不该被弹回第一页。
	// （query.Encode 按字母序 scheme→tab→theme；模板输出时 & 被转义成 &amp;）
	if !strings.Contains(body, "scheme=ocean&amp;style=console&amp;tab=users&amp;theme=dark") {
		t.Error("方案切换链接应带上当前分页与明暗值")
	}
	var themeCookie *http.Cookie
	for _, c := range recorder.Result().Cookies() {
		if c.Name == auth.ThemeCookie {
			themeCookie = c
		}
	}
	if themeCookie == nil || themeCookie.Value != string(auth.ThemeDark) {
		t.Errorf("显式选配色后应写回 %s=dark，实际 %+v", auth.ThemeCookie, themeCookie)
	}

	// Cookie 记住 + 显式亮色覆盖。
	request := httptest.NewRequest(http.MethodGet, AdminPath, nil)
	request.Host = "portal.test"
	request.AddCookie(session)
	request.AddCookie(&http.Cookie{Name: auth.ThemeCookie, Value: string(auth.ThemeLight)})
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, request)
	if body := rec.Body.String(); !strings.Contains(body, `data-theme="light"`) {
		t.Error("Cookie 里选了亮色时管理台应渲染亮色")
	}
}

// 编译期提醒：管理台用的会话 Cookie 名来自 cookie 包，别写错。
var _ = auth.PathLogin
var _ = cookie.Session
