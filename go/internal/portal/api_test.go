package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// apiDo 发一次外部 API 请求。token 为空表示不带 Authorization。
var apiSecretPattern = regexp.MustCompile(`dgk_[0-9a-f]{48}`)

func apiDo(t *testing.T, f *portalFixture, method, path, body, token, remote string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestExternalAPIDisabledIsInvisible(t *testing.T) {
	f := newPortalFixture(t)
	// 默认关闭：整个前缀 404，health 也 404。
	for _, p := range []string{"/api/v1/health", "/api/v1/services", "/api/v1/instances"} {
		if rec := apiDo(t, f, http.MethodGet, p, "", "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("外部 API 关闭时 %s 应 404，实际 %d", p, rec.Code)
		}
	}
}

func TestExternalAPIAuthScopesAndCRUD(t *testing.T) {
	f := newPortalFixture(t)
	if err := f.store.SetExternalAPIEnabled(true); err != nil {
		t.Fatalf("开启外部 API 失败: %v", err)
	}

	// health 免鉴权。
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/health", "", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("health 应 200，实际 %d", rec.Code)
	}

	secret, key, err := f.store.CreateAPIKey("ci", []string{ScopeServicesRead, ScopeServicesWrite, ScopeInstancesRead}, "", "root")
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	_ = key

	// 错误密钥 401。
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services", "", "dgk_wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("错误密钥应 401，实际 %d", rec.Code)
	}

	// 列服务。
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services", "", secret, ""); rec.Code != http.StatusOK {
		t.Fatalf("列服务应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}

	// 创建服务。
	rec := apiDo(t, f, http.MethodPost, "/api/v1/services", `{"name":"api-up","upstream":"http://127.0.0.1:18080","note":"x"}`, secret, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	var created struct {
		Service struct {
			ID string `json:"id"`
		} `json:"service"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Service.ID == "" {
		t.Fatalf("创建响应解析失败: %v / %s", err, rec.Body.String())
	}
	id := created.Service.ID

	// 查单个。
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services/"+id, "", secret, ""); rec.Code != http.StatusOK {
		t.Errorf("查单个应 200，实际 %d", rec.Code)
	}
	// 修改。
	if rec := apiDo(t, f, http.MethodPatch, "/api/v1/services/"+id, `{"note":"changed"}`, secret, ""); rec.Code != http.StatusOK {
		t.Errorf("修改应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	svc, err := f.store.ServiceByID(id)
	if err != nil || svc.Note != "changed" {
		t.Errorf("修改未生效: %v / %+v", err, svc)
	}
	// 列实例。
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/instances", "", secret, ""); rec.Code != http.StatusOK {
		t.Errorf("列实例应 200，实际 %d", rec.Code)
	}
	// 删除。
	if rec := apiDo(t, f, http.MethodDelete, "/api/v1/services/"+id, "", secret, ""); rec.Code != http.StatusOK {
		t.Errorf("删除应 200，实际 %d", rec.Code)
	}
	if _, err := f.store.ServiceByID(id); err == nil {
		t.Error("删除后应查不到")
	}

	// 审计里应有 api.service.create。
	audit, err := f.store.RecentAudit(50)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	found := false
	for _, e := range audit {
		if e.Action == "api.service.create" && e.Actor == "api:ci" && e.OK {
			found = true
		}
	}
	if !found {
		t.Error("写操作应产生 api.service.create 审计")
	}
}

func TestExternalAPIScopeEnforcement(t *testing.T) {
	f := newPortalFixture(t)
	if err := f.store.SetExternalAPIEnabled(true); err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	// 只有 services:read。
	secret, _, err := f.store.CreateAPIKey("ro", []string{ScopeServicesRead}, "", "root")
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services", "", secret, ""); rec.Code != http.StatusOK {
		t.Errorf("读应放行，实际 %d", rec.Code)
	}
	if rec := apiDo(t, f, http.MethodPost, "/api/v1/services", `{"name":"x","upstream":"http://127.0.0.1:9"}`, secret, ""); rec.Code != http.StatusForbidden {
		t.Errorf("无 services:write 应 403，实际 %d", rec.Code)
	}
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/instances", "", secret, ""); rec.Code != http.StatusForbidden {
		t.Errorf("无 instances:read 应 403，实际 %d", rec.Code)
	}
}

func TestExternalAPIKeyDisabledAndIPAllowlist(t *testing.T) {
	f := newPortalFixture(t)
	if err := f.store.SetExternalAPIEnabled(true); err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	// 停用的 key → 401。
	secret, key, err := f.store.CreateAPIKey("disabled", []string{ScopeServicesRead}, "", "root")
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	if err := f.store.SetAPIKeyDisabled(key.ID, true); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services", "", secret, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("停用 key 应 401，实际 %d", rec.Code)
	}

	// IP 白名单：只允许 10.0.0.5。
	secret2, _, err := f.store.CreateAPIKey("iplist", []string{ScopeServicesRead}, "10.0.0.5", "root")
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services", "", secret2, "192.0.2.1:1234"); rec.Code != http.StatusForbidden {
		t.Errorf("来源不在白名单应 403，实际 %d", rec.Code)
	}
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services", "", secret2, "10.0.0.5:1234"); rec.Code != http.StatusOK {
		t.Errorf("来源在白名单应 200，实际 %d", rec.Code)
	}
}

func TestAdminAPIKeyCreateShowsSecretOnce(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// API 分页应能打开。
	if rec := f.get(t, AdminPath+"?tab=api", session); rec.Code != http.StatusOK {
		t.Fatalf("API 分页应 200，实际 %d", rec.Code)
	}

	// 创建密钥：直接渲染（200，非 303 PRG），页面含一次性明文。
	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action":    {"apikey.create"},
		"key_name":  {"bootstrap"},
		"key_scope": {ScopeServicesRead, ScopeInstancesRead},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("apikey.create 应直接 200 渲染，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	m := apiSecretPattern.FindString(rec.Body.String())
	if m == "" {
		t.Fatal("创建响应里应含一次性明文密钥（dgk_<48 hex>）")
	}
	// 库里应有该密钥，且列表页再次渲染时**不再**含明文。
	keys, err := f.store.ListAPIKeys()
	if err != nil || len(keys) != 1 || keys[0].Name != "bootstrap" {
		t.Fatalf("密钥未落库: %v / %+v", err, keys)
	}
	again := f.get(t, AdminPath+"?tab=api", session)
	if strings.Contains(again.Body.String(), m) {
		t.Error("再次打开页面不应出现明文密钥")
	}

	// 总开关：POST api.toggle 打开。
	rec = postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action":               {"api.toggle"},
		"external_api_enabled": {"1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("api.toggle 应 303，实际 %d", rec.Code)
	}
	if on, _ := f.store.ExternalAPIEnabled(); !on {
		t.Error("api.toggle 后外部 API 应为开启")
	}

	// 停用 / 启用 / 删除走 PRG。
	rec = postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"apikey.disable"}, "key_id": {keys[0].ID}, "name": {"bootstrap"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("apikey.disable 应 303，实际 %d", rec.Code)
	}
	if k, _ := f.store.APIKeyByID(keys[0].ID); k != nil && !k.Disabled {
		t.Error("停用后应 Disabled")
	}
}

func TestExternalAPIStateAndAdminJSON(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	_, _, err := f.store.CreateAPIKey("visible", []string{ScopeServicesRead}, "", "root")
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	rec := f.get(t, AdminStatePath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("state 应 200，实际 %d", rec.Code)
	}
	var st struct {
		ExternalAPIEnabled bool `json:"externalAPIEnabled"`
		APIKeys            []struct {
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"apiKeys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("state 解析失败: %v", err)
	}
	if len(st.APIKeys) != 1 || st.APIKeys[0].Name != "visible" || st.APIKeys[0].Prefix == "" {
		t.Errorf("state 应含密钥且只有前缀: %+v", st.APIKeys)
	}
}

// ---- P1：受管服务生命周期端点测试 ----

type fakeServiceLifespan struct {
	started   []string
	stopped   []string
	restarted []string
	status    ServiceRunStatus
	logs      string
	logsErr   error
}

func (f *fakeServiceLifespan) StartService(svc Service) error {
	f.started = append(f.started, svc.ID)
	return nil
}
func (f *fakeServiceLifespan) StopService(id string) error {
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeServiceLifespan) RestartService(svc Service) error {
	f.restarted = append(f.restarted, svc.ID)
	return nil
}
func (f *fakeServiceLifespan) Status(string) ServiceRunStatus   { return f.status }
func (f *fakeServiceLifespan) Logs(string, int) (string, error) { return f.logs, f.logsErr }

// attachServiceLifespan 用给定 ServiceLifespan 重建 fixture 的 handler。
func attachServiceLifespan(t *testing.T, f *portalFixture, ls ServiceLifespan) {
	t.Helper()
	srv, err := NewServer(ServerOptions{Store: f.store, Gate: f.gate, ServiceLifespan: ls})
	if err != nil {
		t.Fatalf("重建 Server 失败: %v", err)
	}
	f.handler = srv.Handler()
}

func mustManagedService(t *testing.T, store *Store, name string) *Service {
	t.Helper()
	svc := &Service{Name: name, Upstream: "http://127.0.0.1:18080", Managed: true, StartCmd: "true"}
	if err := store.CreateService(svc); err != nil {
		t.Fatalf("建受管服务失败: %v", err)
	}
	return svc
}

func mustProxyService(t *testing.T, store *Store, name string) *Service {
	t.Helper()
	svc := &Service{Name: name, Upstream: "http://127.0.0.1:18081"}
	if err := store.CreateService(svc); err != nil {
		t.Fatalf("建代理服务失败: %v", err)
	}
	return svc
}

func TestExternalAPIServiceLifecycle(t *testing.T) {
	f := newPortalFixture(t)
	if err := f.store.SetExternalAPIEnabled(true); err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	ls := &fakeServiceLifespan{
		status: ServiceRunStatus{Running: true, PID: 4242, State: "running"},
		logs:   "hello log",
	}
	attachServiceLifespan(t, f, ls)

	secret, _, err := f.store.CreateAPIKey("life", []string{ScopeServicesRead, ScopeServicesLifecycle}, "", "root")
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	m := mustManagedService(t, f.store, "managed-one")
	p := mustProxyService(t, f.store, "proxy-one")

	// start / stop / restart 都应调到接口。
	for _, verb := range []string{"start", "stop", "restart"} {
		rec := apiDo(t, f, http.MethodPost, "/api/v1/services/"+m.ID+"/"+verb, "", secret, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应 200，实际 %d（%s）", verb, rec.Code, rec.Body.String())
		}
	}
	if len(ls.started) != 1 || len(ls.stopped) != 1 || len(ls.restarted) != 1 {
		t.Errorf("生命周期调用未全部命中: started=%v stopped=%v restarted=%v", ls.started, ls.stopped, ls.restarted)
	}

	// 纯代理服务 start → 409。
	if rec := apiDo(t, f, http.MethodPost, "/api/v1/services/"+p.ID+"/start", "", secret, ""); rec.Code != http.StatusConflict {
		t.Errorf("纯代理服务 start 应 409，实际 %d", rec.Code)
	}

	// status。
	rec := apiDo(t, f, http.MethodGet, "/api/v1/services/"+m.ID+"/status", "", secret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status 应 200，实际 %d", rec.Code)
	}
	var st struct {
		Status struct {
			Running bool   `json:"running"`
			PID     int    `json:"pid"`
			State   string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || !st.Status.Running || st.Status.PID != 4242 {
		t.Errorf("status 内容不对: %v / %s", err, rec.Body.String())
	}

	// logs（正常 + 非法 tail 回退）。
	rec = apiDo(t, f, http.MethodGet, "/api/v1/services/"+m.ID+"/logs?tail=10", "", secret, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello log") {
		t.Errorf("logs 应 200 且含内容，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services/"+m.ID+"/logs?tail=abc", "", secret, ""); rec.Code != http.StatusOK {
		t.Errorf("非法 tail 应回退 200，实际 %d", rec.Code)
	}

	// 列表/详情带 managed 与运行态。
	rec = apiDo(t, f, http.MethodGet, "/api/v1/services/"+m.ID, "", secret, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"managed":true`) || !strings.Contains(rec.Body.String(), `"running":true`) {
		t.Errorf("详情应含 managed/running：%s", rec.Body.String())
	}

	// scope 不足：只有 read 的 key 做 start → 403。
	ro, _, _ := f.store.CreateAPIKey("ro-life", []string{ScopeServicesRead}, "", "root")
	if rec := apiDo(t, f, http.MethodPost, "/api/v1/services/"+m.ID+"/start", "", ro, ""); rec.Code != http.StatusForbidden {
		t.Errorf("缺 lifecycle scope 应 403，实际 %d", rec.Code)
	}

	// 方法不对：GET /start → 405；未知 verb → 404。
	if rec := apiDo(t, f, http.MethodGet, "/api/v1/services/"+m.ID+"/start", "", secret, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET start 应 405，实际 %d", rec.Code)
	}
	if rec := apiDo(t, f, http.MethodPost, "/api/v1/services/"+m.ID+"/bogus", "", secret, ""); rec.Code != http.StatusNotFound {
		t.Errorf("未知 verb 应 404，实际 %d", rec.Code)
	}

	// 审计里应有 api.service.start / stop / restart。
	audit, _ := f.store.RecentAudit(100)
	seen := map[string]bool{}
	for _, e := range audit {
		if e.Actor == "api:life" && e.OK {
			seen[e.Action] = true
		}
	}
	for _, want := range []string{"api.service.start", "api.service.stop", "api.service.restart"} {
		if !seen[want] {
			t.Errorf("审计缺少 %s", want)
		}
	}
}

func TestExternalAPILifecycleWithoutRuntime(t *testing.T) {
	f := newPortalFixture(t) // 默认 fixture 未注入 ServiceLifespan
	if err := f.store.SetExternalAPIEnabled(true); err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	secret, _, _ := f.store.CreateAPIKey("life2", []string{ScopeServicesLifecycle, ScopeServicesRead}, "", "root")
	m := mustManagedService(t, f.store, "managed-two")
	if rec := apiDo(t, f, http.MethodPost, "/api/v1/services/"+m.ID+"/start", "", secret, ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未接运行时 start 应 503，实际 %d", rec.Code)
	}
}

func TestExternalAPIDiscoverRegister(t *testing.T) {
	f := newPortalFixture(t)
	if err := f.store.SetExternalAPIEnabled(true); err != nil {
		t.Fatalf("开启外部 API 失败: %v", err)
	}
	secret, _, err := f.store.CreateAPIKey("disc", []string{ScopeServicesRead, ScopeServicesWrite}, "", "root")
	if err != nil {
		t.Fatalf("建 key 失败: %v", err)
	}
	body := "{\"candidates\":[{\"host\":\"10.0.0.5\",\"port\":8080,\"note\":\"x\"},{\"host\":\"10.0.0.5\",\"port\":8080}]}"
	rec := apiDo(t, f, http.MethodPost, "/api/v1/discover/register", body, secret, "127.0.0.1:1234")
	if rec.Code != http.StatusOK {
		t.Fatalf("register 应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Created []map[string]any `json:"created"`
		Skipped []map[string]any `json:"skipped"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Created) != 1 || len(out.Skipped) != 1 {
		t.Fatalf("应新建 1 跳过 1，实际 %+v", out)
	}
	if _, err := f.store.ServiceByName("svc-10-0-0-5-8080"); err != nil {
		t.Errorf("应生成并登记服务名 svc-10-0-0-5-8080: %v", err)
	}
	// 公网网段应被拒
	if rec := apiDo(t, f, http.MethodPost, "/api/v1/discover/scan", "{\"cidr\":\"8.8.8.0/24\",\"ports\":\"80\"}", secret, "127.0.0.1:1"); rec.Code != http.StatusBadRequest {
		t.Errorf("公网网段应 400，实际 %d", rec.Code)
	}
	// 只读 key 不能 register
	ro, _, _ := f.store.CreateAPIKey("ro", []string{ScopeServicesRead}, "", "root")
	if rec := apiDo(t, f, http.MethodPost, "/api/v1/discover/register", "{\"candidates\":[{\"host\":\"10.0.0.9\",\"port\":1}]}", ro, "127.0.0.1:1"); rec.Code != http.StatusForbidden {
		t.Errorf("缺 services:write 应 403，实际 %d", rec.Code)
	}
}
