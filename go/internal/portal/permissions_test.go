package portal

// 用户级「实例 / 服务」类别权限的回归测试（docs/portal.md「用户级类别权限」）。
// 口径：开关只约束普通用户，管理员恒见全部；开关决定「这类入口是否可见」，
// grants 决定「具体能进哪几个」。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// TestMigrationV4ToV5 验证 v4 老库升级后，老用户的两个类别开关默认都开：
// 升级不改变任何既有用户的可见入口。
func TestMigrationV4ToV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("造 v4 库失败: %v", err)
	}
	for _, schema := range []string{schemaV1, schemaV2, schemaV3, schemaV4} {
		if _, err := db.Exec(schema); err != nil {
			t.Fatalf("建老结构失败: %v", err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 4"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	now := nowText()
	if _, err := db.Exec(
		"INSERT INTO users (name, display_name, hash, role, disabled, created_at, updated_at) VALUES ('legacy','legacy','x','user',0,?,?)",
		now, now); err != nil {
		t.Fatalf("插老用户失败: %v", err)
	}
	_ = db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("打开并迁移到 v5 失败: %v", err)
	}
	defer store.Close()
	u, err := store.UserByName("legacy")
	if err != nil {
		t.Fatalf("读老用户失败: %v", err)
	}
	if !u.AllowInstances || !u.AllowServices {
		t.Errorf("老用户升级后两个开关应默认都开: %+v", u)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读结构版本失败: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("结构版本应升到 %d，实际 %d", SchemaVersion, version)
	}
}

// TestSetUserPermsRoundtrip 验证开关写入与读回，且不影响其他字段。
func TestSetUserPermsRoundtrip(t *testing.T) {
	store := newStore(t)
	mustCreateUser(t, store, "root", "root-password-123", RoleAdmin)
	u := mustCreateUser(t, store, "alice", "alice-password-123", RoleUser)
	if !u.AllowInstances || !u.AllowServices {
		t.Fatalf("新建用户两个开关应默认都开: %+v", u)
	}
	if err := store.SetUserPerms("alice", false, true); err != nil {
		t.Fatalf("写开关失败: %v", err)
	}
	got, err := store.UserByName("alice")
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.AllowInstances || !got.AllowServices {
		t.Errorf("开关没落库: %+v", got)
	}
	if got.Role != RoleUser || got.Disabled || got.Hash != u.Hash {
		t.Errorf("写开关不该动其他字段: %+v", got)
	}
	if err := store.SetUserPerms("nobody", true, true); err == nil {
		t.Error("不存在的账号应报错")
	}
}

// permsFixture 在门户 fixture 上再登记一个服务并把实例与服务都授权给 alice。
func permsFixture(t *testing.T) (*portalFixture, *Service) {
	t.Helper()
	f := newPortalFixture(t)
	svc := mustCreateService(t, f.store, "wiki", "http://127.0.0.1:8080")
	if err := f.store.GrantService("alice", svc.ID, "root"); err != nil {
		t.Fatalf("服务授权失败: %v", err)
	}
	return f, svc
}

// TestPermsInstancesOff 实例开关关掉：首页重定向到服务页、服务页没有实例
// 入口、实例挂载即便有 grant 也 404、me API 实例列表为空。
func TestPermsInstancesOff(t *testing.T) {
	f, svc := permsFixture(t)
	if err := f.store.SetUserPerms("alice", false, true); err != nil {
		t.Fatalf("写开关失败: %v", err)
	}
	session := f.login(t, "alice", "alice-password-123")

	rec := f.get(t, "/", session)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != ServicesPagePath {
		t.Fatalf("实例开关关掉后首页应 303 到 %s，实际 %d → %q", ServicesPagePath, rec.Code, rec.Header().Get("Location"))
	}
	rec = f.get(t, ServicesPagePath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("服务页应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "wiki") {
		t.Error("服务页应显示被授权的服务")
	}
	if strings.Contains(body, "href=\"/\"") {
		t.Error("实例开关关掉后服务页不该出现「我的实例」导航链接")
	}
	// 挂载：有 grant 也进不来，与不存在同形 404。
	if rec := f.get(t, MountPrefix+f.aliceIns.ID+"/", session); rec.Code != http.StatusNotFound {
		t.Fatalf("实例开关关掉后挂载应 404，实际 %d", rec.Code)
	}
	// 服务挂载不受影响。
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", session); rec.Code == http.StatusNotFound {
		t.Fatal("服务开关开着，服务挂载不该 404")
	}
	// me API：布尔与空列表同一口径。
	rec = f.get(t, APIMePath, session)
	var payload struct {
		AllowInstances bool              `json:"allowInstances"`
		AllowServices  bool              `json:"allowServices"`
		Instances      []json.RawMessage `json:"instances"`
		Services       []json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 me 响应失败: %v", err)
	}
	if payload.AllowInstances || !payload.AllowServices {
		t.Errorf("me 的开关布尔不对: %+v", payload)
	}
	if len(payload.Instances) != 0 || len(payload.Services) != 1 {
		t.Errorf("实例列表应为空、服务列表应有一条: %+v", payload)
	}
	// 拒绝要写审计。
	entries, _ := f.store.RecentAudit(30)
	found := false
	for _, e := range entries {
		if e.Action == "instance.denied" && e.Actor == "alice" && !e.OK {
			found = true
		}
	}
	if !found {
		t.Error("类别权限拒绝应写 instance.denied 审计")
	}
}

// TestPermsServicesOff 服务开关关掉：服务页重定向回首页、首页没有服务入口、
// 服务挂载即便有 grant 也 404。
func TestPermsServicesOff(t *testing.T) {
	f, svc := permsFixture(t)
	if err := f.store.SetUserPerms("alice", true, false); err != nil {
		t.Fatalf("写开关失败: %v", err)
	}
	session := f.login(t, "alice", "alice-password-123")

	rec := f.get(t, ServicesPagePath, session)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("服务开关关掉后服务页应 303 回首页，实际 %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = f.get(t, "/", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("首页应 200，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "href=\""+ServicesPagePath+"\"") {
		t.Error("服务开关关掉后首页不该出现「我的服务」导航链接")
	}
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", session); rec.Code != http.StatusNotFound {
		t.Fatalf("服务开关关掉后挂载应 404，实际 %d", rec.Code)
	}
	entries, _ := f.store.RecentAudit(30)
	found := false
	for _, e := range entries {
		if e.Action == "service.denied" && e.Actor == "alice" && !e.OK {
			found = true
		}
	}
	if !found {
		t.Error("类别权限拒绝应写 service.denied 审计")
	}
}

// TestPermsBothOff 两个开关都关：首页与服务页都渲染「没有任何访问权限」页，
// 两类导航链接都不出现。
func TestPermsBothOff(t *testing.T) {
	f, _ := permsFixture(t)
	if err := f.store.SetUserPerms("alice", false, false); err != nil {
		t.Fatalf("写开关失败: %v", err)
	}
	session := f.login(t, "alice", "alice-password-123")
	for _, path := range []string{"/", ServicesPagePath} {
		rec := f.get(t, path, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应 200（无权限页），实际 %d", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "没有任何访问权限") {
			t.Errorf("%s 应渲染无权限提示", path)
		}
		if strings.Contains(body, "href=\""+ServicesPagePath+"\"") || strings.Contains(body, "href=\"/\"") {
			t.Errorf("%s 无权限页不该出现两类入口链接", path)
		}
	}
}

// TestPermsAdminUnaffected 管理员不受开关影响：即便库里把 admin 的开关写成
// 全关，入口、挂载与 me API 照旧全量。
func TestPermsAdminUnaffected(t *testing.T) {
	f, svc := permsFixture(t)
	if err := f.store.SetUserPerms("root", false, false); err != nil {
		t.Fatalf("写开关失败: %v", err)
	}
	session := f.login(t, "root", "root-password-123")
	if rec := f.get(t, "/", session); rec.Code != http.StatusOK {
		t.Fatalf("管理员首页应 200，实际 %d", rec.Code)
	}
	if rec := f.get(t, ServicesPagePath, session); rec.Code != http.StatusOK {
		t.Fatalf("管理员服务页应 200，实际 %d", rec.Code)
	}
	if rec := f.get(t, MountPrefix+f.aliceIns.ID+"/", session); rec.Code == http.StatusNotFound {
		t.Fatal("管理员不该被类别开关拦在实例外")
	}
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", session); rec.Code == http.StatusNotFound {
		t.Fatal("管理员不该被类别开关拦在服务外")
	}
	rec := f.get(t, APIMePath, session)
	var payload struct {
		AllowInstances bool `json:"allowInstances"`
		AllowServices  bool `json:"allowServices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 me 响应失败: %v", err)
	}
	if !payload.AllowInstances || !payload.AllowServices {
		t.Errorf("管理员的 me 开关布尔应恒真: %+v", payload)
	}
}

// TestUserPermsActionJSON JSON API 设置开关：落库、state 带字段、审计留痕；
// 非管理员与跨站来源被拒。
func TestUserPermsActionJSON(t *testing.T) {
	f := newPortalFixture(t)
	admin := f.login(t, "root", "root-password-123")
	alice := f.login(t, "alice", "alice-password-123")

	payload := map[string]any{"action": "user.perms", "name": "alice", "allowServices": true}
	if rec := postAdminJSON(t, f, alice, payload, "http://portal.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员改权限应 403，实际 %d", rec.Code)
	}
	if rec := postAdminJSON(t, f, admin, payload, "http://evil.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("跨站来源应 403，实际 %d", rec.Code)
	}
	if rec := postAdminJSON(t, f, admin, payload, "http://portal.test", "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("JSON 改权限应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	u, err := f.store.UserByName("alice")
	if err != nil {
		t.Fatalf("读用户失败: %v", err)
	}
	if u.AllowInstances || !u.AllowServices {
		t.Errorf("开关没按 JSON 落库: %+v", u)
	}

	// state：users 段带开关字段；管理员恒真。
	rec := f.get(t, AdminStatePath, admin)
	var state struct {
		Users []struct {
			Name           string `json:"name"`
			AllowInstances bool   `json:"allowInstances"`
			AllowServices  bool   `json:"allowServices"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析 state 失败: %v", err)
	}
	byName := map[string]int{}
	for i, u := range state.Users {
		byName[u.Name] = i
	}
	if i, ok := byName["alice"]; !ok || state.Users[i].AllowInstances || !state.Users[i].AllowServices {
		t.Errorf("state 里 alice 的开关不对: %+v", state.Users)
	}
	if i, ok := byName["root"]; !ok || !state.Users[i].AllowInstances || !state.Users[i].AllowServices {
		t.Errorf("state 里管理员的开关应恒真: %+v", state.Users)
	}
	entries, _ := f.store.RecentAudit(30)
	found := false
	for _, e := range entries {
		if e.Action == "admin.user.perms" && e.Target == "alice" && e.OK {
			found = true
		}
	}
	if !found {
		t.Error("改权限应写 admin.user.perms 审计")
	}
}

// TestUserPermsActionForm 表单路径：勾选口径（出现且非空）、PRG 回 users 分页、
// 坏 CSRF 拒绝；用户页渲染勾选与 admin 行禁用说明。
func TestUserPermsActionForm(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 只勾服务：实例开关应落成 false。
	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"user.perms"}, "name": {"alice"}, "allow_services": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("表单改权限应 303 PRG，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "tab=users") || !strings.Contains(loc, "ok=user.perms") {
		t.Errorf("PRG 应回 users 分页并带动作码，实际 %q", loc)
	}
	u, _ := f.store.UserByName("alice")
	if u.AllowInstances || !u.AllowServices {
		t.Errorf("表单开关没落库: %+v", u)
	}
	// 回跳提示能翻出来。
	if msg := f.server.flashMessage(i18n.ZH, httptest.NewRequest(http.MethodGet, "/admin?ok=user.perms&arg=alice", nil)); !strings.Contains(msg, "alice") {
		t.Errorf("flash 提示应带上用户名，实际 %q", msg)
	}
	// 坏 CSRF 拒绝且不改库。
	rec = postAdmin(t, f, session, csrfCookie, "not-the-token", url.Values{
		"action": {"user.perms"}, "name": {"alice"}, "allow_instances": {"1"}, "allow_services": {"1"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("坏 CSRF 应 403，实际 %d", rec.Code)
	}
	u, _ = f.store.UserByName("alice")
	if u.AllowInstances {
		t.Error("被拒的请求不该改库")
	}
	// 用户页：普通用户行有勾选表单，admin 行是禁用勾选加说明。
	rec = f.get(t, AdminPath+"?tab=users", session)
	body := rec.Body.String()
	if !strings.Contains(body, "name=\"allow_instances\"") || !strings.Contains(body, "name=\"allow_services\"") {
		t.Error("用户页应有类别权限勾选")
	}
	if !strings.Contains(body, "checked disabled") {
		t.Error("admin 行的勾选应为选中且禁用")
	}
	if !strings.Contains(body, "管理员默认拥有全部权限") {
		t.Error("admin 行应附开关不生效的说明")
	}
}

// TestPermsGrantsTabMatrices 权限控制分页同时有两个矩阵；服务分页不再有授权区块。
func TestPermsGrantsTabMatrices(t *testing.T) {
	f, _ := permsFixture(t)
	session := f.login(t, "root", "root-password-123")

	rec := f.get(t, AdminPath+"?tab=grants", session)
	body := rec.Body.String()
	if !strings.Contains(body, "value=\"grants.save\"") || !strings.Contains(body, "value=\"serviceGrants.save\"") {
		t.Error("权限控制分页应同时有实例与服务两个授权矩阵")
	}
	if !strings.Contains(body, "权限控制") {
		t.Error("分页标题应为「权限控制」")
	}
	// 所有面板都在同一份 DOM 里（CSS 按 tab 显隐），只检查 panel-services 那一段。
	rec = f.get(t, AdminPath+"?tab=services", session)
	page := rec.Body.String()
	start := strings.Index(page, "id=\"panel-services\"")
	end := strings.Index(page, "id=\"panel-audit\"")
	if start < 0 || end < 0 || end < start {
		t.Fatalf("页面缺少服务/审计面板锚点: start=%d end=%d", start, end)
	}
	if strings.Contains(page[start:end], "value=\"serviceGrants.save\"") {
		t.Error("服务分页不该再有服务授权区块")
	}
	// serviceGrants.save 的 PRG 应回权限控制分页。
	if tab := adminTabForAction("serviceGrants.save", false, true, true); tab != "grants" {
		t.Errorf("serviceGrants.save 应回 grants 分页，实际 %q", tab)
	}
	if tab := adminTabForAction("user.perms", false, true, true); tab != "users" {
		t.Errorf("user.perms 应回 users 分页，实际 %q", tab)
	}
}
