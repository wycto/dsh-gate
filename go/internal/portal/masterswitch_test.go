package portal

// 门户全局总开关（实例 / 服务）的回归测试（docs/portal.md「用户级类别权限」全局层）。
// 口径：最终可见性 = 全局开关 AND（管理员 OR 用户开关）。全局关掉某类时所有人
// （含管理员）的该类入口都隐藏；管理员总能进「权限控制」分页重新打开。

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

// TestMigrationV5ToV6 验证 v5 老库升级后 settings 表建好、两个总开关缺省都开：
// 升级不改变任何既有入口的可见性。
func TestMigrationV5ToV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("造 v5 库失败: %v", err)
	}
	for _, schema := range []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5} {
		if _, err := db.Exec(schema); err != nil {
			t.Fatalf("建老结构失败: %v", err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 5"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	_ = db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("打开并迁移到 v6 失败: %v", err)
	}
	defer store.Close()
	inst, svc, err := store.PortalFeatures()
	if err != nil {
		t.Fatalf("读总开关失败: %v", err)
	}
	if !inst || !svc {
		t.Errorf("老库升级后两个总开关应缺省都开: instances=%v services=%v", inst, svc)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读结构版本失败: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("结构版本应升到 %d，实际 %d", SchemaVersion, version)
	}
}

// TestSetPortalFeaturesRoundtrip 验证总开关写入与读回（UPSERT 覆盖旧值）。
func TestSetPortalFeaturesRoundtrip(t *testing.T) {
	store := newStore(t)
	inst, svc, err := store.PortalFeatures()
	if err != nil || !inst || !svc {
		t.Fatalf("空库总开关应缺省都开: %v %v %v", inst, svc, err)
	}
	if err := store.SetPortalFeatures(false, true); err != nil {
		t.Fatalf("写总开关失败: %v", err)
	}
	inst, svc, err = store.PortalFeatures()
	if err != nil || inst || !svc {
		t.Fatalf("总开关没落库: %v %v %v", inst, svc, err)
	}
	// 再写一次覆盖（UPSERT，不报主键冲突）。
	if err := store.SetPortalFeatures(true, false); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}
	inst, svc, _ = store.PortalFeatures()
	if !inst || svc {
		t.Errorf("覆盖写没生效: instances=%v services=%v", inst, svc)
	}
}

// TestGlobalInstancesOff 全局关实例：管理员首页也重定向到服务页、实例挂载即便
// 管理员有 grant 也 404、管理台导航无实例标签、?tab=instances 回落到权限控制、
// me API 的 allowInstances 对管理员也为 false。
func TestGlobalInstancesOff(t *testing.T) {
	f, _ := permsFixture(t)
	if err := f.store.SetPortalFeatures(false, true); err != nil {
		t.Fatalf("写总开关失败: %v", err)
	}
	session := f.login(t, "root", "root-password-123")

	if rec := f.get(t, "/", session); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != ServicesPagePath {
		t.Fatalf("全局关实例后管理员首页应 303 到 %s，实际 %d → %q", ServicesPagePath, rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.get(t, MountPrefix+f.aliceIns.ID+"/", session); rec.Code != http.StatusNotFound {
		t.Fatalf("全局关实例后管理员挂载也应 404，实际 %d", rec.Code)
	}
	if rec := f.get(t, ServicesPagePath, session); rec.Code != http.StatusOK {
		t.Fatalf("服务仍开着，服务页应 200，实际 %d", rec.Code)
	}
	// 管理台：导航无实例标签；默认分页回落到权限控制（grants 恒可用）。
	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `id="tab-instances"`) {
		t.Error("全局关实例后管理台导航不该有实例标签")
	}
	if !strings.Contains(body, `id="tab-grants" class="tab-input" checked`) {
		t.Error("默认分页应回落到权限控制（grants 选中）")
	}
	// 直接访问被关的 tab 也回落。
	rec = f.get(t, AdminPath+"?tab=instances", session)
	if strings.Contains(rec.Body.String(), `id="tab-instances"`) {
		t.Error("?tab=instances 在全局关闭后不该渲染实例标签")
	}
	// me API：管理员的 allowInstances 也反映全局关闭。
	rec = f.get(t, APIMePath, session)
	var payload struct {
		AllowInstances bool `json:"allowInstances"`
		AllowServices  bool `json:"allowServices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析 me 失败: %v", err)
	}
	if payload.AllowInstances || !payload.AllowServices {
		t.Errorf("管理员 me 开关应反映全局: %+v", payload)
	}
}

// TestGlobalServicesOff 全局关服务：管理员服务页重定向回首页、服务挂载 404、
// 管理台导航无服务标签。
func TestGlobalServicesOff(t *testing.T) {
	f, svc := permsFixture(t)
	if err := f.store.SetPortalFeatures(true, false); err != nil {
		t.Fatalf("写总开关失败: %v", err)
	}
	session := f.login(t, "root", "root-password-123")

	if rec := f.get(t, ServicesPagePath, session); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("全局关服务后服务页应 303 回首页，实际 %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", session); rec.Code != http.StatusNotFound {
		t.Fatalf("全局关服务后管理员挂载也应 404，实际 %d", rec.Code)
	}
	if rec := f.get(t, "/", session); rec.Code != http.StatusOK {
		t.Fatalf("实例仍开着，首页应 200，实际 %d", rec.Code)
	}
	rec := f.get(t, AdminPath, session)
	if strings.Contains(rec.Body.String(), `id="tab-services"`) {
		t.Error("全局关服务后管理台导航不该有服务标签")
	}
	if !strings.Contains(rec.Body.String(), `id="tab-instances"`) {
		t.Error("实例仍开着，导航应保留实例标签")
	}
}

// TestGlobalBothOffAdminCanRecover 两类都关：管理员首页落到无权限页，但管理台
// 「权限控制」分页恒可进入，并能在那里把总开关重新打开。
func TestGlobalBothOffAdminCanRecover(t *testing.T) {
	f, _ := permsFixture(t)
	if err := f.store.SetPortalFeatures(false, false); err != nil {
		t.Fatalf("写总开关失败: %v", err)
	}
	session := f.login(t, "root", "root-password-123")

	if rec := f.get(t, "/", session); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "没有任何访问权限") {
		t.Fatalf("两类都关后管理员首页应渲染无权限页，实际 %d", rec.Code)
	}
	rec := f.get(t, AdminPath+"?tab=grants", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员应总能进权限控制分页，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="features.save"`) {
		t.Fatal("权限控制分页应有总开关表单")
	}
	// 导航既无实例也无服务标签，但权限控制标签在。
	if strings.Contains(body, `id="tab-instances"`) || strings.Contains(body, `id="tab-services"`) {
		t.Error("两类都关后导航不该有实例/服务标签")
	}
	if !strings.Contains(body, `id="tab-grants"`) {
		t.Error("权限控制标签应恒在")
	}
	// 重新打开。
	csrfCookie, csrf := adminCSRF(t, f, session)
	rec = postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"features.save"}, "features_instances": {"1"}, "features_services": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("重新打开总开关应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	inst, svc, _ := f.store.PortalFeatures()
	if !inst || !svc {
		t.Errorf("总开关应被重新打开: instances=%v services=%v", inst, svc)
	}
}

// TestGlobalOverridesUserSwitch 全局开关优先于每用户开关：全局关实例时，即便
// 用户自己的实例开关开着，入口也消失（合成 = 全局 AND 用户）。
func TestGlobalOverridesUserSwitch(t *testing.T) {
	f, _ := permsFixture(t)
	// alice 的每用户开关默认双开；全局关实例。
	if err := f.store.SetPortalFeatures(false, true); err != nil {
		t.Fatalf("写总开关失败: %v", err)
	}
	session := f.login(t, "alice", "alice-password-123")
	if rec := f.get(t, "/", session); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != ServicesPagePath {
		t.Fatalf("全局关实例应压过用户开关，首页 303 到服务页，实际 %d → %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.get(t, MountPrefix+f.aliceIns.ID+"/", session); rec.Code != http.StatusNotFound {
		t.Fatalf("全局关实例后用户挂载应 404，实际 %d", rec.Code)
	}
}

// TestFeaturesSaveActionJSON JSON API 设总开关：落库、state.features 带字段、
// 审计留痕；非管理员与跨站来源被拒。
func TestFeaturesSaveActionJSON(t *testing.T) {
	f := newPortalFixture(t)
	admin := f.login(t, "root", "root-password-123")
	alice := f.login(t, "alice", "alice-password-123")

	payload := map[string]any{"action": "features.save", "featuresInstances": false, "featuresServices": true}
	if rec := postAdminJSON(t, f, alice, payload, "http://portal.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员改总开关应 403，实际 %d", rec.Code)
	}
	if rec := postAdminJSON(t, f, admin, payload, "http://evil.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("跨站来源应 403，实际 %d", rec.Code)
	}
	if rec := postAdminJSON(t, f, admin, payload, "http://portal.test", "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("JSON 改总开关应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	inst, svc, _ := f.store.PortalFeatures()
	if inst || !svc {
		t.Errorf("总开关没按 JSON 落库: instances=%v services=%v", inst, svc)
	}
	// state.features 反映库值。
	rec := f.get(t, AdminStatePath, admin)
	var state struct {
		Features struct {
			Instances bool `json:"instances"`
			Services  bool `json:"services"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析 state 失败: %v", err)
	}
	if state.Features.Instances || !state.Features.Services {
		t.Errorf("state.features 不对: %+v", state.Features)
	}
	entries, _ := f.store.RecentAudit(30)
	found := false
	for _, e := range entries {
		if e.Action == "admin.features.save" && e.OK {
			found = true
		}
	}
	if !found {
		t.Error("改总开关应写 admin.features.save 审计")
	}
}

// TestFeaturesSaveActionForm 表单路径：勾选口径、PRG 回权限控制分页、flash 提示、
// 坏 CSRF 拒绝。
func TestFeaturesSaveActionForm(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 只勾服务：实例总开关应落成 false。
	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"features.save"}, "features_services": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("表单改总开关应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "tab=grants") || !strings.Contains(loc, "ok=features.save") {
		t.Errorf("PRG 应回权限控制分页并带动作码，实际 %q", loc)
	}
	inst, svc, _ := f.store.PortalFeatures()
	if inst || !svc {
		t.Errorf("表单总开关没落库: instances=%v services=%v", inst, svc)
	}
	if msg := f.server.flashMessage(i18n.ZH, httptest.NewRequest(http.MethodGet, "/admin?ok=features.save", nil)); !strings.Contains(msg, "总开关") {
		t.Errorf("flash 提示应提到总开关，实际 %q", msg)
	}
	// 坏 CSRF 拒绝且不改库。
	rec = postAdmin(t, f, session, csrfCookie, "not-the-token", url.Values{
		"action": {"features.save"}, "features_instances": {"1"}, "features_services": {"1"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("坏 CSRF 应 403，实际 %d", rec.Code)
	}
	inst, svc, _ = f.store.PortalFeatures()
	if inst {
		t.Error("被拒的请求不该改库")
	}
}

// TestGrantsSaveWritesPerms 权限控制页矩阵表单（grants.save + perms_present）同时
// 写授权与每用户开关；旧式表单 / JSON 缺省字段不动开关（兼容）；JSON 显式字段才动。
func TestGrantsSaveWritesPerms(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 表单：带 perms_present，只勾服务开关 + 一个实例授权。
	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"grants.save"}, "name": {"alice"}, "perms_present": {"1"},
		"allow_services": {"1"}, "instance": {f.aliceIns.ID},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("矩阵表单保存应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	u, _ := f.store.UserByName("alice")
	if u.AllowInstances || !u.AllowServices {
		t.Errorf("矩阵表单应同时写每用户开关: %+v", u)
	}
	if _, err := f.store.Accessible(u, f.aliceIns.ID); err != nil {
		t.Errorf("矩阵表单应同时写授权: %v", err)
	}

	// JSON 缺省开关字段：不动开关（兼容旧脚本），只写授权。
	if err := f.store.SetUserPerms("alice", true, true); err != nil {
		t.Fatalf("重置开关失败: %v", err)
	}
	if rec := postAdminJSON(t, f, session, map[string]any{
		"action": "grants.save", "name": "alice", "instances": []string{f.shared.ID},
	}, "http://portal.test", "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("JSON 授权应 200，实际 %d", rec.Code)
	}
	u, _ = f.store.UserByName("alice")
	if !u.AllowInstances || !u.AllowServices {
		t.Errorf("JSON 缺省开关字段不该动开关: %+v", u)
	}

	// JSON 显式开关字段：只覆盖提供的哪个，另一个沿用现值。
	if rec := postAdminJSON(t, f, session, map[string]any{
		"action": "grants.save", "name": "alice", "instances": []string{f.shared.ID}, "allowInstances": false,
	}, "http://portal.test", "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("JSON 带开关授权应 200，实际 %d", rec.Code)
	}
	u, _ = f.store.UserByName("alice")
	if u.AllowInstances || !u.AllowServices {
		t.Errorf("JSON 显式 allowInstances=false 应只关实例、服务沿用: %+v", u)
	}

	// 旧式表单（无 perms_present）：不动开关。
	if err := f.store.SetUserPerms("alice", true, true); err != nil {
		t.Fatalf("重置开关失败: %v", err)
	}
	rec = postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"grants.save"}, "name": {"alice"}, "instance": {f.aliceIns.ID},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("旧式矩阵表单应 303，实际 %d", rec.Code)
	}
	u, _ = f.store.UserByName("alice")
	if !u.AllowInstances || !u.AllowServices {
		t.Errorf("无 perms_present 的旧式表单不该动开关: %+v", u)
	}
}

// TestGlobalSwitchDoesNotAffectLocalAPI 全局总开关只约束浏览器入口，不影响本机
// 控制 API（CLI 管理工具）：关掉两类后本机 API 仍能列实例。
func TestGlobalSwitchDoesNotAffectLocalAPI(t *testing.T) {
	f, _ := permsFixture(t)
	if err := f.store.SetPortalFeatures(false, false); err != nil {
		t.Fatalf("写总开关失败: %v", err)
	}
	// 本机 API 走 token + 回环，不过类别开关。这里直接验证 Store 层不受影响：
	// 实例/服务登记仍在库里（总开关只影响 HTTP 可见性，不删数据）。
	instances, err := f.store.ListInstances()
	if err != nil || len(instances) == 0 {
		t.Errorf("总开关不该删实例登记: %v %d", err, len(instances))
	}
	services, err := f.store.ListServices()
	if err != nil || len(services) == 0 {
		t.Errorf("总开关不该删服务登记: %v %d", err, len(services))
	}
}
