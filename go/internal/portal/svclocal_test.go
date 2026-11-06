package portal

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSvcLocalRequiresLoopbackAndToken 验证 svc.* 动作同样过本机 API 的两道门。
func TestSvcLocalRequiresLoopbackAndToken(t *testing.T) {
	s, _, _ := newLocalTestServer(t)

	// 非回环来源：令牌对也不行。
	req := httptest.NewRequest(http.MethodPost, LocalAPIPath, bytes.NewReader([]byte(`{"action":"svc.list"}`)))
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.handleLocalAPI(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("非回环请求应 403，实际 %d", rec.Code)
	}

	// 缺令牌 / 错令牌：401。
	for _, token := range []string{"", "wrong-token"} {
		rec, _ := callLocal(t, s, token, localRequest{Action: "svc.list"})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("令牌 %q 应 401，实际 %d", token, rec.Code)
		}
	}
}

// TestSvcLocalAddListToggleRemove 走一遍登记册全链路：
// add → list → disable/enable（名与 id 两种解析）→ remove，并核对审计。
func TestSvcLocalAddListToggleRemove(t *testing.T) {
	s, store, _ := newLocalTestServer(t)

	// 缺必填字段：400。
	rec, resp := callLocal(t, s, "test-token", localRequest{Action: "svc.add", Service: "wiki"})
	if rec.Code != http.StatusBadRequest || resp.OK {
		t.Fatalf("缺 upstream 应 400，实际 %d %+v", rec.Code, resp)
	}
	// 非法上游：400。
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.add", Service: "wiki", Upstream: "ftp://x.test"})
	if rec.Code != http.StatusBadRequest || resp.OK {
		t.Fatalf("非法上游应 400，实际 %d %+v", rec.Code, resp)
	}

	// 正常登记。
	rec, resp = callLocal(t, s, "test-token", localRequest{
		Action: "svc.add", Service: "wiki", Upstream: "http://127.0.0.1:8080/base/", Note: "内网百科",
	})
	if rec.Code != http.StatusOK || !resp.OK || len(resp.Services) != 1 {
		t.Fatalf("svc.add 应成功并回服务，实际 %d %+v", rec.Code, resp)
	}
	created := resp.Services[0]
	if created.Name != "wiki" || !created.Enabled || created.URL != ServiceMountPrefix+created.ID+"/" {
		t.Fatalf("返回的服务形状不对: %+v", created)
	}
	if created.Upstream != "http://127.0.0.1:8080/base" {
		t.Errorf("上游应经 NormalizeUpstream 归一（去尾斜杠），实际 %q", created.Upstream)
	}
	if created.Note != "内网百科" {
		t.Errorf("备注应落库，实际 %q", created.Note)
	}

	// 重名：拒绝。
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.add", Service: "wiki", Upstream: "http://127.0.0.1:9"})
	if rec.Code == http.StatusOK || resp.OK {
		t.Fatalf("重名应拒绝，实际 %d %+v", rec.Code, resp)
	}

	// list：一条。
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.list"})
	if rec.Code != http.StatusOK || !resp.OK || len(resp.Services) != 1 {
		t.Fatalf("svc.list 应返回一条，实际 %d %+v", rec.Code, resp)
	}

	// 探活缓存先放一条，验证 toggle 会连带失效。
	s.storeServiceProbe(created.ID, ServiceProbe{Reachable: true, Status: 200})

	// disable 按名字解析。
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.disable", Service: "wiki"})
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("svc.disable 应成功，实际 %d %+v", rec.Code, resp)
	}
	svc, err := store.ServiceByID(created.ID)
	if err != nil || svc.Enabled {
		t.Fatalf("disable 后库里应是停用态: svc=%+v err=%v", svc, err)
	}
	if _, ok := s.cachedServiceProbe(created.ID); ok {
		t.Error("toggle 后探活缓存应失效")
	}

	// enable 按 id 解析。
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.enable", Service: created.ID})
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("svc.enable 应成功，实际 %d %+v", rec.Code, resp)
	}
	svc, err = store.ServiceByID(created.ID)
	if err != nil || !svc.Enabled {
		t.Fatalf("enable 后库里应是启用态: svc=%+v err=%v", svc, err)
	}

	// remove 按名字解析。
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.remove", Service: "wiki"})
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("svc.remove 应成功，实际 %d %+v", rec.Code, resp)
	}
	if _, err := store.ServiceByID(created.ID); err == nil {
		t.Error("服务记录应已删除")
	}
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.list"})
	if rec.Code != http.StatusOK || len(resp.Services) != 0 {
		t.Fatalf("删除后 list 应为空，实际 %d %+v", rec.Code, resp)
	}

	// 审计：写动作都留痕，actor 是 local-cli。
	entries, err := store.RecentAudit(20)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	want := map[string]bool{
		"admin.service.create": false, "admin.service.delete": false,
		"admin.service.enable": false, "admin.service.disable": false,
	}
	for _, e := range entries {
		if _, ok := want[e.Action]; ok {
			if e.Actor != "local-cli" {
				t.Errorf("审计 %s 的 actor 应为 local-cli，实际 %q", e.Action, e.Actor)
			}
			want[e.Action] = true
		}
	}
	for action, seen := range want {
		if !seen {
			t.Errorf("缺少审计 %s", action)
		}
	}
}

// TestSvcLocalProbe 验证探活动作：真打上游、结果随响应返回并写入缓存。
func TestSvcLocalProbe(t *testing.T) {
	s, _, _ := newLocalTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("探活应打上游根路径，实际 %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	rec, resp := callLocal(t, s, "test-token", localRequest{Action: "svc.add", Service: "probed", Upstream: ts.URL})
	if rec.Code != http.StatusOK || !resp.OK || len(resp.Services) != 1 {
		t.Fatalf("登记失败: %d %+v", rec.Code, resp)
	}
	id := resp.Services[0].ID

	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.probe", Service: "probed"})
	if rec.Code != http.StatusOK || !resp.OK || resp.Probe == nil {
		t.Fatalf("svc.probe 应带探活结果，实际 %d %+v", rec.Code, resp)
	}
	if !resp.Probe.Reachable || resp.Probe.Status != http.StatusNoContent {
		t.Errorf("活上游探活结果不对: %+v", resp.Probe)
	}
	cached, ok := s.cachedServiceProbe(id)
	if !ok || !cached.Reachable {
		t.Errorf("探活结果应写入缓存: cached=%+v ok=%v", cached, ok)
	}

	// 上游关掉再探：不可达且带 Error，缓存跟着更新。
	ts.Close()
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.probe", Service: id})
	if rec.Code != http.StatusOK || !resp.OK || resp.Probe == nil {
		t.Fatalf("死上游探活也应正常应答，实际 %d %+v", rec.Code, resp)
	}
	if resp.Probe.Reachable || resp.Probe.Error == "" {
		t.Errorf("死上游应不可达且带 Error: %+v", resp.Probe)
	}
	cached, ok = s.cachedServiceProbe(id)
	if !ok || cached.Reachable {
		t.Errorf("缓存应更新为不可达: cached=%+v ok=%v", cached, ok)
	}
}

// TestSvcLocalUnknownService 验证找不到服务时是 404，错误文案可读。
func TestSvcLocalUnknownService(t *testing.T) {
	s, _, _ := newLocalTestServer(t)
	for _, action := range []string{"svc.remove", "svc.enable", "svc.disable", "svc.probe"} {
		rec, resp := callLocal(t, s, "test-token", localRequest{Action: action, Service: "ghost"})
		if rec.Code != http.StatusNotFound || resp.OK {
			t.Fatalf("%s 找不到服务应 404，实际 %d %+v", action, rec.Code, resp)
		}
		if !strings.Contains(resp.Error, "ghost") {
			t.Errorf("错误里应带上找不到的名字，实际 %q", resp.Error)
		}
	}
	// 缺引用字段。
	rec, resp := callLocal(t, s, "test-token", localRequest{Action: "svc.remove"})
	if rec.Code != http.StatusNotFound || resp.OK {
		t.Fatalf("缺服务引用应 404，实际 %d %+v", rec.Code, resp)
	}
}
