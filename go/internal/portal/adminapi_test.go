package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postAdminJSON 发一个管理 API 的 JSON 动作。
func postAdminJSON(t *testing.T, f *portalFixture, session *http.Cookie, payload any, origin, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, AdminActionPath, strings.NewReader(string(raw)))
	request.Host = "portal.test"
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if session != nil {
		request.AddCookie(session)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestAdminJSONStateRequiresAdmin(t *testing.T) {
	f := newPortalFixture(t)
	aliceSession := f.login(t, "alice", "alice-password-123")

	rec := f.get(t, AdminStatePath, aliceSession)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户读管理状态应 403，实际 %d", rec.Code)
	}

	rootSession := f.login(t, "root", "root-password-123")
	rec = f.get(t, AdminStatePath, rootSession)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员读管理状态应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "pbkdf2") {
		t.Fatal("管理状态里泄漏了口令哈希")
	}

	var state adminStateJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析状态 JSON 失败: %v", err)
	}
	if !state.OK || state.Counts["users"] != 3 || state.Counts["instances"] != 3 {
		t.Fatalf("状态计数不对: %+v", state.Counts)
	}
	if len(state.Users) != 3 || len(state.Instances) != 3 {
		t.Fatalf("用户/实例数量不对: %d/%d", len(state.Users), len(state.Instances))
	}
	// 授权矩阵随用户一起给出；管理员看得到全部实例，普通用户只看被授权的。
	if got := len(state.Grants["alice"]); got != 2 {
		t.Errorf("alice 应有 2 条授权，实际 %d（%v）", got, state.Grants["alice"])
	}
	if got := len(state.Grants["root"]); got != 0 {
		t.Errorf("管理员不该有额外授权行（它天然全通），实际 %v", state.Grants["root"])
	}
	for _, inst := range state.Instances {
		if inst.URL != MountPrefix+inst.ID+"/" {
			t.Errorf("实例 URL 应是挂载路径，实际 %q", inst.URL)
		}
	}
	if state.User["name"] != "root" || state.User["role"] != string(RoleAdmin) {
		t.Errorf("调用者信息不对: %v", state.User)
	}
}

func TestAdminJSONActionLifecycle(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	// 建用户
	rec := postAdminJSON(t, f, session, map[string]any{
		"action": "user.create", "name": "carol", "password": "carol-password-123", "role": "user",
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 建用户应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	carol, err := f.store.UserByName("carol")
	if err != nil {
		t.Fatalf("用户没建出来: %v", err)
	}

	// 授权
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "grants.save", "name": "carol", "instances": []string{f.shared.ID},
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 授权应 200，实际 %d", rec.Code)
	}
	if _, err := f.store.Accessible(carol, f.shared.ID); err != nil {
		t.Fatalf("授权没生效: %v", err)
	}
	if _, err := f.store.Accessible(carol, f.aliceIns.ID); err == nil {
		t.Error("没授权过的实例不该可访问")
	}

	// 停用（顺带验证会话吊销：carol 先登录一次）
	carolSession := f.login(t, "carol", "carol-password-123")
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "user.toggle", "name": "carol", "disabled": true,
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON 停用应 200，实际 %d", rec.Code)
	}
	if got := f.get(t, "/", carolSession); got.Code != http.StatusSeeOther {
		t.Fatalf("被停用后应立即失效，实际 %d", got.Code)
	}

	// 超级管理员（fixture 里第一个注册的 root）：拒绝并给出说明
	rec = postAdminJSON(t, f, session, map[string]any{
		"action": "user.toggle", "name": "root", "disabled": true,
	}, "http://portal.test", "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("拒绝操作超级管理员应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "超级管理员") {
		t.Errorf("应给出「超级管理员」的说明，实际 %q", rec.Body.String())
	}
}

func TestAdminJSONActionGuards(t *testing.T) {
	f := newPortalFixture(t)
	admin := f.login(t, "root", "root-password-123")
	alice := f.login(t, "alice", "alice-password-123")

	// 非管理员
	if rec := postAdminJSON(t, f, alice, map[string]any{"action": "user.create", "name": "x", "password": "x-password-123"},
		"http://portal.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("非管理员调用管理 API 应 403，实际 %d", rec.Code)
	}
	// 跨站来源
	if rec := postAdminJSON(t, f, admin, map[string]any{"action": "user.create", "name": "mallory", "password": "mallory-password-1"},
		"http://evil.test", "application/json"); rec.Code != http.StatusForbidden {
		t.Fatalf("跨站来源应 403，实际 %d", rec.Code)
	}
	if _, err := f.store.UserByName("mallory"); err == nil {
		t.Fatal("跨站请求绝不能建出用户")
	}
	// 表单 content-type（跨站表单能发出来的那种）
	if rec := postAdminJSON(t, f, admin, map[string]any{"action": "user.create", "name": "formy", "password": "formy-password-1"},
		"", "application/x-www-form-urlencoded"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON content-type 应 415，实际 %d", rec.Code)
	}
	if _, err := f.store.UserByName("formy"); err == nil {
		t.Fatal("表单形态的请求绝不能建出用户")
	}
	// 坏 JSON
	request := httptest.NewRequest(http.MethodPost, AdminActionPath, strings.NewReader("{not json"))
	request.Host = "portal.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://portal.test")
	request.AddCookie(admin)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，实际 %d", recorder.Code)
	}

	// 被拒绝的调用都应留下审计（服务端能区分原因，响应里不泄漏）。
	entries, _ := f.store.RecentAudit(30)
	rejected := 0
	for _, e := range entries {
		if e.Action == "admin.csrf-rejected" {
			rejected++
		}
	}
	if rejected == 0 {
		t.Error("被拒绝的管理 API 调用应写审计")
	}
}

// TestAdminJSONStateExposesInstallFields 验证管理 API 把实例的安装相关字段
// 一并给出：脚本化运维要靠它判断某台实例装没装、用什么方式装的。
func TestAdminJSONStateExposesInstallFields(t *testing.T) {
	f := newPortalFixture(t)
	rootSession := f.login(t, "root", "root-password-123")

	// 给一台实例写上安装相关字段（页面与 CLI 走的是同一批列）。
	inst, err := f.store.InstanceByName("shared-team")
	if err != nil {
		t.Fatalf("取实例失败: %v", err)
	}
	inst.InstallMode = string(InstallModeGit)
	inst.InstallDir = "/srv/shared/src"
	inst.ConfigDir = "/srv/shared/config"
	inst.ListenHost = "0.0.0.0"
	inst.StartMode = StartModeOnDemand
	inst.DshCmd = "node /srv/shared/src/deepseek-harness/apps/cli/lib/bin.js"
	if err := f.store.UpdateInstance(*inst); err != nil {
		t.Fatalf("更新实例失败: %v", err)
	}

	rec := f.get(t, AdminStatePath, rootSession)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员读管理状态应 200，实际 %d", rec.Code)
	}
	var state adminStateJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析状态 JSON 失败: %v", err)
	}

	var found *adminInstanceJSON
	for i := range state.Instances {
		if state.Instances[i].Name == "shared-team" {
			found = &state.Instances[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("状态里应有 shared-team 实例，实际 %+v", state.Instances)
	}
	if found.InstallMode != string(InstallModeGit) {
		t.Errorf("installMode 应为 git，实际 %q", found.InstallMode)
	}
	if found.InstallDir != "/srv/shared/src" {
		t.Errorf("installDir 不对: %q", found.InstallDir)
	}
	if found.ConfigDir != "/srv/shared/config" {
		t.Errorf("configDir 不对: %q", found.ConfigDir)
	}
	if found.ListenHost != "0.0.0.0" {
		t.Errorf("listenHost 不对: %q", found.ListenHost)
	}
	if found.StartMode != StartModeOnDemand {
		t.Errorf("startMode 不对: %q", found.StartMode)
	}
	if !found.Installed {
		t.Error("有启动命令时 installed 应为 true")
	}
	if found.DshCmd == "" {
		t.Error("dshCmd 不该为空")
	}
	// 没装过的实例要如实报 false，不能一律 true。
	for _, inst := range state.Instances {
		if inst.Name == "shared-team" {
			continue
		}
		if inst.Installed {
			t.Errorf("%s 没设启动命令，installed 应为 false", inst.Name)
		}
	}
}
