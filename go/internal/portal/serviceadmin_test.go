package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// mustCreateService 复用 servicemount_test.go 里的同名基建（签名一致）。

// TestServiceAdminJSONLifecycle 走 JSON API 的登记 → 更新 → 启停 → 探活 → 删除。
func TestServiceAdminJSONLifecycle(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	// 登记
	rec := postAdminJSON(t, f, session, map[string]any{
		"action": "service.create", "name": "wiki", "upstream": "http://127.0.0.1:8080",
		"hostHeader": "upstream", "note": "内网知识库",
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 登记服务应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	svc, err := f.store.ServiceByName("wiki")
	if err != nil {
		t.Fatalf("服务没登记出来: %v", err)
	}
	if svc.Upstream != "http://127.0.0.1:8080" || svc.Note != "内网知识库" || !svc.Enabled {
		t.Fatalf("登记字段不对: %+v", svc)
	}

	// 缺必填字段要拒
	if rec := postAdminJSON(t, f, session, map[string]any{
		"action": "service.create", "name": "no-upstream",
	}, "http://portal.test", "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺上游地址应 400，实际 %d", rec.Code)
	}

	// 更新（改名 + 换上游 + 勾自签豁免）
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "service.update", "serviceID": svc.ID, "name": "wiki2",
		"upstream": "https://wiki.corp.example", "hostHeader": "gateway",
		"insecureTLS": true, "note": "换域名了",
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 更新服务应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	svc, err = f.store.ServiceByID(svc.ID)
	if err != nil {
		t.Fatalf("更新后取服务失败: %v", err)
	}
	if svc.Name != "wiki2" || svc.Upstream != "https://wiki.corp.example" ||
		svc.HostHeader != HostHeaderGateway || !svc.InsecureTLS || svc.Note != "换域名了" {
		t.Fatalf("更新字段没落库: %+v", svc)
	}

	// 启停翻转
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "service.toggle", "serviceID": svc.ID,
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 启停应 200，实际 %d", rec.Code)
	}
	svc, _ = f.store.ServiceByID(svc.ID)
	if svc.Enabled {
		t.Fatal("toggle 后应为停用")
	}

	// 探活：起一个真上游，结果应可达并进缓存与审计。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "service.update", "serviceID": svc.ID, "name": "wiki2", "upstream": upstream.URL,
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("改上游应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "service.probe", "serviceID": svc.ID,
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 探活应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	probe, ok := f.server.cachedServiceProbe(svc.ID)
	if !ok || !probe.Reachable {
		t.Fatalf("探活结果应进缓存且可达: ok=%v probe=%+v", ok, probe)
	}
	entries, _ := f.store.RecentAudit(30)
	foundProbe := false
	for _, e := range entries {
		if e.Action == "admin.service.probe" && e.OK && strings.Contains(e.Detail, "reachable=true") {
			foundProbe = true
		}
	}
	if !foundProbe {
		t.Error("探活结果应写进审计 detail")
	}

	// 删除
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "service.remove", "serviceID": svc.ID,
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 删除应 200，实际 %d", rec.Code)
	}
	if _, err := f.store.ServiceByID(svc.ID); err == nil {
		t.Fatal("删除后服务不该还在")
	}
	// 删完再操作应 404（ErrNotFound 同形）。
	if rec := postAdminJSON(t, f, session, map[string]any{
		"action": "service.toggle", "serviceID": svc.ID,
	}, "http://portal.test", "application/json"); rec.Code != http.StatusNotFound {
		t.Fatalf("操作已删服务应 404，实际 %d", rec.Code)
	}
}

// TestServiceAdminJSONGuards 验证服务动作与实例动作同一套闸：
// 非管理员 403、跨站来源 403、表单 content-type 415。
func TestServiceAdminJSONGuards(t *testing.T) {
	f := newPortalFixture(t)
	admin := f.login(t, "root", "root-password-123")
	alice := f.login(t, "alice", "alice-password-123")

	payload := map[string]any{"action": "service.create", "name": "evil", "upstream": "http://127.0.0.1:1"}
	if rec := postAdminJSON(t, f, alice, payload, "http://portal.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员登记服务应 403，实际 %d", rec.Code)
	}
	if rec := postAdminJSON(t, f, admin, payload, "http://evil.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("跨站来源应 403，实际 %d", rec.Code)
	}
	if rec := postAdminJSON(t, f, admin, payload, "", "application/x-www-form-urlencoded"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON content-type 应 415，实际 %d", rec.Code)
	}
	if _, err := f.store.ServiceByName("evil"); err == nil {
		t.Fatal("被拒的请求绝不能登记出服务")
	}
}

// TestServiceGrantsAndState 验证服务授权保存与 state 里的 services 段。
func TestServiceGrantsAndState(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	wiki := mustCreateService(t, f.store, "wiki", "http://127.0.0.1:8080")
	mustCreateService(t, f.store, "nas", "http://127.0.0.1:5000")

	rec := postAdminJSON(t, f, session, map[string]any{
		"action": "serviceGrants.save", "name": "alice", "services": []string{wiki.ID},
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("保存服务授权应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	if _, err := f.store.ServiceAccessible(f.alice, wiki.ID); err != nil {
		t.Fatalf("授权没生效: %v", err)
	}
	if _, err := f.store.ServiceAccessible(f.bob, wiki.ID); err == nil {
		t.Error("没授权的用户不该可访问")
	}

	rec = f.get(t, AdminStatePath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("读状态应 200，实际 %d", rec.Code)
	}
	var state adminStateJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析状态失败: %v", err)
	}
	if state.Counts["services"] != 2 || len(state.Services) != 2 {
		t.Fatalf("services 段数量不对: counts=%v len=%d", state.Counts, len(state.Services))
	}
	for _, svc := range state.Services {
		if svc.URL != ServiceMountPrefix+svc.ID+"/" {
			t.Errorf("服务 URL 应是挂载路径，实际 %q", svc.URL)
		}
	}
	if got := state.ServiceGrants["alice"]; len(got) != 1 || got[0] != wiki.ID {
		t.Errorf("alice 的服务授权不对: %v", got)
	}
	if got := state.ServiceGrants["bob"]; len(got) != 0 {
		t.Errorf("bob 不该有服务授权: %v", got)
	}

	// live 段：未探活时 probed=false；探活后翻成就绪文案。
	rec = f.get(t, AdminLivePath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("读 live 应 200，实际 %d", rec.Code)
	}
	var live adminLiveJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("解析 live 失败: %v", err)
	}
	if len(live.Services) != 2 {
		t.Fatalf("live services 段数量不对: %d", len(live.Services))
	}
	for _, svc := range live.Services {
		if svc.Probed || svc.StatusText == "" {
			t.Errorf("未探活的服务应 probed=false 且有文案: %+v", svc)
		}
	}
	f.server.storeServiceProbe(wiki.ID, ServiceProbe{Reachable: true, Status: 200})
	rec = f.get(t, AdminLivePath, session)
	_ = json.Unmarshal(rec.Body.Bytes(), &live)
	for _, svc := range live.Services {
		if svc.ID == wiki.ID && (!svc.Probed || !svc.Reachable || svc.StatusClass != "on") {
			t.Errorf("探活缓存应反映到 live: %+v", svc)
		}
	}
}

// TestServiceAdminFormPRG 走真实表单提交：成功后 303 回服务分页；坏 CSRF 拒绝。
func TestServiceAdminFormPRG(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"service.create"}, "name": {"wiki"}, "upstream": {"http://127.0.0.1:8080"},
		"host_header": {"upstream"}, "note": {"表单登记"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("表单登记应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "tab=services") || !strings.Contains(loc, "ok=service.create") {
		t.Fatalf("PRG 应回服务分页并带动作码，实际 %q", loc)
	}
	svc, err := f.store.ServiceByName("wiki")
	if err != nil {
		t.Fatalf("表单没登记出服务: %v", err)
	}

	// 管理台服务分页能渲染出这张卡片与授权矩阵。
	rec = f.get(t, AdminPath+"?tab=services", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("服务分页应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"id=\"panel-services\"", svc.Name, svc.Upstream, "serviceGrants.save", "name=\"service\""} {
		if !strings.Contains(body, want) {
			t.Errorf("服务分页缺少 %q", want)
		}
	}

	// 坏 CSRF 令牌必须拒绝。
	rec = postAdmin(t, f, session, csrfCookie, "not-the-token", url.Values{
		"action": {"service.remove"}, "service_id": {svc.ID},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("坏 CSRF 应 403，实际 %d", rec.Code)
	}
	if _, err := f.store.ServiceByID(svc.ID); err != nil {
		t.Fatal("坏 CSRF 的请求绝不能删掉服务")
	}
}

// TestServicesPageVisibility 验证用户侧 /services 页：管理员见全部、
// 普通用户只见被授权的、未登录跳登录页。
func TestServicesPageVisibility(t *testing.T) {
	f := newPortalFixture(t)
	wiki := mustCreateService(t, f.store, "wiki", "http://127.0.0.1:8080")
	nas := mustCreateService(t, f.store, "nas", "http://127.0.0.1:5000")
	if err := f.store.GrantService("alice", wiki.ID, "root"); err != nil {
		t.Fatalf("授权失败: %v", err)
	}

	// 未登录：303 去登录页。
	if rec := f.get(t, ServicesPagePath, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("未登录访问 /services 应 303，实际 %d", rec.Code)
	}

	// 管理员见全部。
	root := f.login(t, "root", "root-password-123")
	rec := f.get(t, ServicesPagePath, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员访问 /services 应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, wiki.Name) || !strings.Contains(body, nas.Name) {
		t.Error("管理员应看到全部服务")
	}
	if !strings.Contains(body, ServiceMountPrefix+wiki.ID+"/") {
		t.Error("卡片应带挂载入口链接")
	}
	if !strings.Contains(body, "href=\"/\"") {
		t.Error("页头应有回「我的实例」的导航")
	}
	if strings.Contains(body, "<script") {
		t.Error("我的服务页不该带脚本（与首页同一套无脚本 CSP）")
	}

	// 普通用户只见被授权的。
	alice := f.login(t, "alice", "alice-password-123")
	rec = f.get(t, ServicesPagePath, alice)
	if rec.Code != http.StatusOK {
		t.Fatalf("普通用户访问 /services 应 200，实际 %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, wiki.Name) {
		t.Error("alice 应看到被授权的 wiki")
	}
	if strings.Contains(body, nas.Name) {
		t.Error("alice 不该看到未授权的 nas")
	}
	if strings.Contains(body, "href=\"/admin\"") {
		t.Error("普通用户不该看到管理台入口")
	}

	// 没有任何授权的用户看到空状态。
	bob := f.login(t, "bob", "bob-password-123")
	rec = f.get(t, ServicesPagePath, bob)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "还没有分配给你的服务") {
		t.Errorf("bob 应看到空状态文案，实际 %d", rec.Code)
	}
}
