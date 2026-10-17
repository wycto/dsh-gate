package portal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/cookie"
)

// recordingUpstream 记录上游收到的请求，作为「管理端确实把请求转发到了实例」的证据。
type recordingUpstream struct {
	name string
	path string
	host string
	raw  string
	hits int
}

func (u *recordingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.hits++
	u.path = r.URL.Path
	u.host = r.Host
	u.raw = r.RequestURI
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "upstream:"+u.name)
}

type portalFixture struct {
	store    *Store
	gate     *auth.Gate
	server   *Server
	handler  http.Handler
	alice    *User
	bob      *User
	root     *User
	aliceIns *Instance
	bobIns   *Instance
	shared   *Instance
	ups      map[string]*recordingUpstream
}

// newPortalFixture 造一个「1 个管理员 + 2 个用户 + 3 个实例（含一个共享）」的管理端。
func newPortalFixture(t *testing.T) *portalFixture {
	t.Helper()
	store := newStore(t)

	f := &portalFixture{store: store, ups: map[string]*recordingUpstream{}}
	f.root = mustCreateUser(t, store, "root", "root-password-123", RoleAdmin)
	f.alice = mustCreateUser(t, store, "alice", "alice-password-123", RoleUser)
	f.bob = mustCreateUser(t, store, "bob", "bob-password-123", RoleUser)

	f.aliceIns = mustCreateInstance(t, store, "alice-main", "/srv/dsh/alice")
	f.bobIns = mustCreateInstance(t, store, "bob-main", "/srv/dsh/bob")
	f.shared = mustCreateInstance(t, store, "shared-team", "/srv/dsh/shared")

	for _, g := range []struct{ user, instance string }{
		{"alice", f.aliceIns.ID},
		{"alice", f.shared.ID},
		{"bob", f.bobIns.ID},
		{"bob", f.shared.ID},
	} {
		if err := store.Grant(g.user, g.instance, "root"); err != nil {
			t.Fatalf("授权 %s→%s 失败: %v", g.user, g.instance, err)
		}
	}

	gate, err := auth.New(auth.Options{
		Users:       store,
		SessionTTL:  time.Hour,
		MaxFails:    50,
		LockoutBase: time.Minute,
	})
	if err != nil {
		t.Fatalf("构造鉴权网关失败: %v", err)
	}
	f.gate = gate

	var err2 error
	f.server, err2 = NewServer(ServerOptions{
		Store: store,
		Gate:  gate,
		Upstream: func(inst Instance) (http.Handler, error) {
			up := &recordingUpstream{name: inst.Name}
			f.ups[inst.ID] = up
			return up, nil
		},
		Version: "test",
	})
	if err2 != nil {
		t.Fatalf("构造管理端失败: %v", err2)
	}
	f.handler = f.server.Handler()
	return f
}

// login 走真实的登录表单，拿到会话 Cookie。
func (f *portalFixture) login(t *testing.T, username, password string) *http.Cookie {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	request := httptest.NewRequest(http.MethodPost, auth.PathLogin, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("登录应 303，实际 %d（body: %s）", recorder.Code, recorder.Body.String())
	}
	for _, c := range recorder.Result().Cookies() {
		if c.Name == cookie.Session && c.Value != "" {
			return c
		}
	}
	t.Fatal("登录响应没有会话 Cookie")
	return nil
}

func (f *portalFixture) get(t *testing.T, path string, session *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "portal.test"
	if session != nil {
		request.AddCookie(session)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestPortalRequiresLogin(t *testing.T) {
	f := newPortalFixture(t)

	rec := f.get(t, "/", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("未登录访问首页应 303 去登录页，实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, auth.PathLogin) {
		t.Errorf("应跳登录页，实际 %q", loc)
	}
}

func TestPortalListsOnlyGrantedInstances(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "alice", "alice-password-123")

	rec := f.get(t, "/", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("首页应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"alice-main", "shared-team", f.aliceIns.ID} {
		if !strings.Contains(body, want) {
			t.Errorf("首页缺少 %q（被授权的实例应显示）", want)
		}
	}
	if strings.Contains(body, "bob-main") {
		t.Error("首页不该出现别人实例的名字")
	}

	// JSON 端点：同一份可见性口径。
	rec = f.get(t, APIMePath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s 应 200，实际 %d", APIMePath, rec.Code)
	}
	var payload struct {
		OK   bool `json:"ok"`
		User struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"user"`
		Instances []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 JSON 失败: %v", err)
	}
	if !payload.OK || payload.User.Name != "alice" || payload.User.Role != string(RoleUser) {
		t.Fatalf("用户信息不对: %+v", payload)
	}
	if len(payload.Instances) != 2 {
		t.Fatalf("alice 应看到 2 个实例，实际 %d", len(payload.Instances))
	}
	for _, inst := range payload.Instances {
		if inst.URL != MountPrefix+inst.ID+"/" {
			t.Errorf("实例 URL 应为挂载路径，实际 %q", inst.URL)
		}
	}
}

func TestMountAuthorizesByGrant(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "alice", "alice-password-123")

	// 被授权的实例：进入后上游看到的是剥掉前缀的路径。
	rec := f.get(t, MountPrefix+f.aliceIns.ID+"/", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("进入自己的实例应 200，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "upstream:alice-main") {
		t.Errorf("应答应来自该实例的上游，实际 %q", body)
	}
	up := f.ups[f.aliceIns.ID]
	if up == nil || up.path != "/" {
		t.Fatalf("上游应收到挂载根映射后的 /，实际 %+v", up)
	}
	if up.host != "portal.test" {
		t.Errorf("Host 必须原样转发，实际 %q", up.host)
	}

	// 未授权的实例：404，且**不该**构造上游。
	rec = f.get(t, MountPrefix+f.bobIns.ID+"/", session)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("越权访问应 404，实际 %d", rec.Code)
	}
	if f.ups[f.bobIns.ID] != nil {
		t.Error("越权的请求不该构造上游（先授权再建上游）")
	}
	// 与「不存在的实例」完全同形。
	missing := f.get(t, MountPrefix+"zzzzzzzz/", session)
	if missing.Code != rec.Code || missing.Body.String() != rec.Body.String() {
		t.Errorf("越权与不存在的应答应当同形：越权 %d/%q，不存在 %d/%q",
			rec.Code, rec.Body.String(), missing.Code, missing.Body.String())
	}
}

func TestAdminSeesEveryInstance(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	rec := f.get(t, "/", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员首页应 200，实际 %d", rec.Code)
	}
	for _, want := range []string{"alice-main", "bob-main", "shared-team"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("管理员应看到全部实例，缺少 %q", want)
		}
	}
	if rec := f.get(t, MountPrefix+f.bobIns.ID+"/", session); rec.Code != http.StatusOK {
		t.Fatalf("管理员应能进入任意实例，实际 %d", rec.Code)
	}
}

func TestMountPathSubtreeReachesUpstream(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "alice", "alice-password-123")

	rec := f.get(t, MountPrefix+f.aliceIns.ID+"/api/remote.mux?x=1", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("子树请求应 200，实际 %d", rec.Code)
	}
	up := f.ups[f.aliceIns.ID]
	if up.path != "/api/remote.mux" || up.raw != "/api/remote.mux?x=1" {
		t.Errorf("上游应看到剥掉前缀的路径与查询串，实际 %q %q", up.path, up.raw)
	}
}

func TestDisabledUserLosesPortalSession(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "bob", "bob-password-123")

	if rec := f.get(t, "/", session); rec.Code != http.StatusOK {
		t.Fatalf("停用前应能访问，实际 %d", rec.Code)
	}
	// 管理员在别处停用了 bob。
	if err := f.store.SetDisabled("bob", true); err != nil {
		t.Fatalf("停用失败: %v", err)
	}

	rec := f.get(t, "/", session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("被停用后应跳登录页，实际 %d", rec.Code)
	}
	// 会话已被作废：同一个 Cookie 再用仍应跳登录页。
	if rec := f.get(t, "/", session); rec.Code != http.StatusSeeOther {
		t.Fatalf("会话应已被作废，实际 %d", rec.Code)
	}
}

func TestMountUnavailableWithoutUpstream(t *testing.T) {
	store := newStore(t)
	mustCreateUser(t, store, "root", "root-password-123", RoleAdmin)
	inst := mustCreateInstance(t, store, "solo", "/srv/dsh/solo")
	gate, err := auth.New(auth.Options{Users: store, SessionTTL: time.Hour, MaxFails: 50, LockoutBase: time.Minute})
	if err != nil {
		t.Fatalf("构造鉴权网关失败: %v", err)
	}
	server, err := NewServer(ServerOptions{Store: store, Gate: gate})
	if err != nil {
		t.Fatalf("构造管理端失败: %v", err)
	}
	handler := server.Handler()

	form := url.Values{"username": {"root"}, "password": {"root-password-123"}}
	request := httptest.NewRequest(http.MethodPost, auth.PathLogin, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var session *http.Cookie
	for _, c := range recorder.Result().Cookies() {
		if c.Name == cookie.Session {
			session = c
		}
	}
	if session == nil {
		t.Fatal("登录没拿到会话 Cookie")
	}

	request = httptest.NewRequest(http.MethodGet, MountPrefix+inst.ID+"/", nil)
	request.AddCookie(session)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("没有上游时应 503，实际 %d", recorder.Code)
	}
}

func TestPortalAuditsEntryAndDenial(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "alice", "alice-password-123")

	// 进入自己的实例 → instance.enter
	f.get(t, MountPrefix+f.aliceIns.ID+"/", session)
	// 越权 → instance.denied
	f.get(t, MountPrefix+f.bobIns.ID+"/", session)

	entries, err := f.store.RecentAudit(10)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	actions := map[string]int{}
	for _, e := range entries {
		actions[e.Action]++
	}
	if actions["instance.enter"] == 0 {
		t.Errorf("应有 instance.enter 审计，实际 %+v", actions)
	}
	if actions["instance.denied"] == 0 {
		t.Errorf("应有 instance.denied 审计，实际 %+v", actions)
	}
}

func TestPortalPageLanguage(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "alice", "alice-password-123")

	if body := f.get(t, "/", session).Body.String(); !strings.Contains(body, "我的实例") {
		t.Error("默认应渲染中文首页")
	}
	if body := f.get(t, "/?lang=en", session).Body.String(); !strings.Contains(body, "My instances") {
		t.Error("?lang=en 时应渲染英文首页")
	}
}

// TestInstanceLinksOpenInNewWindow 验证两处实例入口都用新窗口打开。
//
// 原先点实例是原地跳转：在管理台点进去，管理台这一页就没了；想同时开着
// 两个实例对照也不可能。两处都补上 target="_blank"。
func TestInstanceLinksOpenInNewWindow(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "alice", "alice-password-123")

	// 入口页：进入按钮。只拼路径，引号在断言里加，免得两处各加一次。
	body := f.get(t, "/", session).Body.String()
	path := "/i/" + f.aliceIns.ID + "/"
	if !strings.Contains(body, `href="`+path+`"`) {
		t.Fatalf("入口页缺少实例链接 %s", path)
	}
	if !strings.Contains(body, `href="`+path+`" target="_blank"`) {
		t.Error("入口页的实例链接应当用新窗口打开")
	}
	// target="_blank" 不带 rel 会让新页面拿到 window.opener，必须一起给。
	if !strings.Contains(body, `target="_blank" rel="noopener"`) {
		t.Error("新窗口链接必须带 rel=\"noopener\"")
	}
}

// TestAdminInstanceNameLinksToInstance 验证管理台的实例名本身是入口。
//
// 管理台原先根本没有进入实例的入口——只能绕去「我的实例」。对管理员来说
// 装完就该能直接点进去看。
func TestAdminInstanceNameLinksToInstance(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	body := f.get(t, AdminPath, session).Body.String()
	path := "/i/" + f.aliceIns.ID + "/"
	if !strings.Contains(body, `href="`+path+`"`) {
		t.Fatalf("管理台应当有进入实例的链接 %s", path)
	}
	if !strings.Contains(body, `href="`+path+`" target="_blank" rel="noopener"`) {
		t.Error("管理台的实例链接应当新窗口打开并带 noopener")
	}
}
