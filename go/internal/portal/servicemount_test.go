package portal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// svcTestUpstream 记录假上游收到的请求，作为「门户确实把请求转发到了服务」的证据。
// 与 recordingUpstream 同一思路，另带 Cookie / 重定向两个探针路由。
type svcTestUpstream struct {
	path string
	host string
	raw  string
	hits int
}

func (u *svcTestUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.hits++
	u.path = r.URL.Path
	u.host = r.Host
	u.raw = r.URL.RequestURI()
	switch r.URL.Path {
	case "/cookie":
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc", Path: "/", HttpOnly: true})
	case "/redirect":
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("service-upstream"))
}

// mustCreateService 在 fixture 的库里登记一条服务。
func mustCreateService(t *testing.T, store *Store, name, upstream string) *Service {
	t.Helper()
	svc := &Service{Name: name, Upstream: upstream}
	if err := store.CreateService(svc); err != nil {
		t.Fatalf("建服务失败: %v", err)
	}
	return svc
}

// TestServiceMountStripsPrefixAndRewrites 验证剥前缀、Cookie Path 改写、
// Location 改写与挂载根补斜杠——全部复用 mountproxy 的机械层。
func TestServiceMountStripsPrefixAndRewrites(t *testing.T) {
	f := newPortalFixture(t)
	up := &svcTestUpstream{}
	ts := httptest.NewServer(up)
	defer ts.Close()
	svc := mustCreateService(t, f.store, "wiki", ts.URL)
	if err := f.store.GrantService("alice", svc.ID, "root"); err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	session := f.login(t, "alice", "alice-password-123")

	// 剥前缀：/s/<id>/api/x?y=1 → 上游 /api/x?y=1。
	rec := f.get(t, ServiceMountPrefix+svc.ID+"/api/x?y=1", session)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "service-upstream") {
		t.Fatalf("挂载请求应 200 且来自上游，实际 %d %q", rec.Code, rec.Body.String())
	}
	if up.path != "/api/x" || up.raw != "/api/x?y=1" {
		t.Errorf("上游应看到剥前缀的路径与查询串，实际 %q %q", up.path, up.raw)
	}

	// Cookie Path 挪进挂载前缀。
	rec = f.get(t, ServiceMountPrefix+svc.ID+"/cookie", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie 探针应 200，实际 %d", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Path != ServiceMountPrefix+svc.ID+"/" {
		t.Fatalf("Set-Cookie 的 Path 应改写为挂载前缀，实际 %+v", cookies)
	}

	// 根相对 Location 挪进挂载前缀。
	rec = f.get(t, ServiceMountPrefix+svc.ID+"/redirect", session)
	if rec.Code != http.StatusFound {
		t.Fatalf("重定向探针应 302，实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != ServiceMountPrefix+svc.ID+"/" {
		t.Errorf("Location 应改写进挂载前缀，实际 %q", loc)
	}

	// 挂载根无斜杠：303 补斜杠（保留查询串）。
	rec = f.get(t, ServiceMountPrefix+svc.ID+"?a=b", session)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("挂载根应 303 补斜杠，实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != ServiceMountPrefix+svc.ID+"/?a=b" {
		t.Errorf("补斜杠应保留查询串，实际 %q", loc)
	}
}

// TestServiceMountBasePathAndHostHeader 验证上游 base path 拼接与两种 Host 策略。
func TestServiceMountBasePathAndHostHeader(t *testing.T) {
	f := newPortalFixture(t)
	up := &svcTestUpstream{}
	ts := httptest.NewServer(up)
	defer ts.Close()

	// 上游自带 base path：剥前缀后拼回 base。
	base := mustCreateService(t, f.store, "based", ts.URL+"/base")
	session := f.login(t, "root", "root-password-123")
	if rec := f.get(t, ServiceMountPrefix+base.ID+"/api/x", session); rec.Code != http.StatusOK {
		t.Fatalf("base 服务应 200，实际 %d", rec.Code)
	}
	if up.path != "/base/api/x" {
		t.Errorf("上游应收到 base 拼接后的路径，实际 %q", up.path)
	}
	// 默认 Host 策略 upstream：上游看到自己的主机名，不是门户 authority。
	if up.host != strings.TrimPrefix(ts.URL, "http://") {
		t.Errorf("upstream 策略应转上游主机名，实际 %q", up.host)
	}

	// gateway 策略：Host 原样保留浏览器看到的门户 authority。
	gw := mustCreateService(t, f.store, "gwed", ts.URL)
	gw.HostHeader = HostHeaderGateway
	if err := f.store.UpdateService(*gw); err != nil {
		t.Fatalf("更新 Host 策略失败: %v", err)
	}
	if rec := f.get(t, ServiceMountPrefix+gw.ID+"/", session); rec.Code != http.StatusOK {
		t.Fatalf("gateway 服务应 200，实际 %d", rec.Code)
	}
	if up.host != "portal.test" {
		t.Errorf("gateway 策略应保留门户 authority，实际 %q", up.host)
	}
}

// TestServiceMountAuthorizationAndAudit 验证唯一授权入口、404 同形与审计。
func TestServiceMountAuthorizationAndAudit(t *testing.T) {
	f := newPortalFixture(t)
	up := &svcTestUpstream{}
	ts := httptest.NewServer(up)
	defer ts.Close()
	svc := mustCreateService(t, f.store, "private", ts.URL)

	// 授权入口本身的口径：未命中返回 ErrNotFound（与不存在同形）。
	if _, err := f.store.ServiceAccessible(f.alice, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未授权时 ServiceAccessible 应返回 ErrNotFound，实际 %v", err)
	}
	alice := f.login(t, "alice", "alice-password-123")
	// 未授权：404，且与「不存在」完全同形；上游不该被打到。
	rec := f.get(t, ServiceMountPrefix+svc.ID+"/", alice)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("越权访问应 404，实际 %d", rec.Code)
	}
	missing := f.get(t, ServiceMountPrefix+"zzzzzzzz/", alice)
	if missing.Code != rec.Code || missing.Body.String() != rec.Body.String() {
		t.Errorf("越权与不存在应同形：越权 %d/%q，不存在 %d/%q",
			rec.Code, rec.Body.String(), missing.Code, missing.Body.String())
	}
	if up.hits != 0 {
		t.Error("越权请求不该打到上游（先授权再建上游）")
	}

	// 管理员全通。
	root := f.login(t, "root", "root-password-123")
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", root); rec.Code != http.StatusOK {
		t.Fatalf("管理员应能进入任意服务，实际 %d", rec.Code)
	}
	// grant 后普通用户通。
	if err := f.store.GrantService("alice", svc.ID, "root"); err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", alice); rec.Code != http.StatusOK {
		t.Fatalf("授权后应 200，实际 %d", rec.Code)
	}

	entries, err := f.store.RecentAudit(20)
	if err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	actions := map[string]int{}
	for _, e := range entries {
		actions[e.Action]++
	}
	if actions["service.denied"] == 0 {
		t.Errorf("应有 service.denied 审计，实际 %+v", actions)
	}
	if actions["service.enter"] == 0 {
		t.Errorf("应有 service.enter 审计，实际 %+v", actions)
	}
}

// TestServiceMountDisabled 验证停用的服务对任何人都是 404。
func TestServiceMountDisabled(t *testing.T) {
	f := newPortalFixture(t)
	up := &svcTestUpstream{}
	ts := httptest.NewServer(up)
	defer ts.Close()
	svc := mustCreateService(t, f.store, "off", ts.URL)
	root := f.login(t, "root", "root-password-123")
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", root); rec.Code != http.StatusOK {
		t.Fatalf("停用前应 200，实际 %d", rec.Code)
	}
	svc.Enabled = false
	if err := f.store.UpdateService(*svc); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	f.server.invalidateServiceMount(svc.ID)
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", root); rec.Code != http.StatusNotFound {
		t.Fatalf("停用后应 404，实际 %d", rec.Code)
	}
}

// TestServiceMountInvalidateRebuilds 验证 invalidate 清掉代理与探活缓存，
// 下一次请求按新定义重建。
func TestServiceMountInvalidateRebuilds(t *testing.T) {
	f := newPortalFixture(t)
	up := &svcTestUpstream{}
	ts := httptest.NewServer(up)
	defer ts.Close()
	svc := mustCreateService(t, f.store, "cached", ts.URL)
	root := f.login(t, "root", "root-password-123")
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", root); rec.Code != http.StatusOK {
		t.Fatalf("首次请求应 200，实际 %d", rec.Code)
	}
	f.server.svcMu.Lock()
	first := f.server.svcMounts[svc.ID]
	f.server.svcMu.Unlock()
	if first == nil {
		t.Fatal("首次请求后应有代理缓存")
	}
	f.server.storeServiceProbe(svc.ID, ServiceProbe{Reachable: true, Status: 200})

	f.server.invalidateServiceMount(svc.ID)
	if _, ok := f.server.cachedServiceProbe(svc.ID); ok {
		t.Error("invalidate 应清掉探活缓存")
	}
	f.server.svcMu.Lock()
	_, still := f.server.svcMounts[svc.ID]
	f.server.svcMu.Unlock()
	if still {
		t.Error("invalidate 应清掉代理缓存")
	}
	if rec := f.get(t, ServiceMountPrefix+svc.ID+"/", root); rec.Code != http.StatusOK {
		t.Fatalf("重建后应 200，实际 %d", rec.Code)
	}
	f.server.svcMu.Lock()
	second := f.server.svcMounts[svc.ID]
	f.server.svcMu.Unlock()
	if second == nil || second == first {
		t.Error("invalidate 后应重建一个新的代理实例")
	}
}

// TestServiceMountRequiresLogin 验证未登录进服务挂载会被送去登录页。
func TestServiceMountRequiresLogin(t *testing.T) {
	f := newPortalFixture(t)
	up := &svcTestUpstream{}
	ts := httptest.NewServer(up)
	defer ts.Close()
	svc := mustCreateService(t, f.store, "gated", ts.URL)
	rec := f.get(t, ServiceMountPrefix+svc.ID+"/", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("未登录应 303 去登录页，实际 %d", rec.Code)
	}
	if up.hits != 0 {
		t.Error("未登录请求不该打到上游")
	}
}

// TestServiceProbeAliveAndDead 验证探活口径：<500 算可达，连接错误记 Error。
func TestServiceProbeAliveAndDead(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name      string
		status    int
		reachable bool
	}{
		{"活上游200", http.StatusOK, true},
		{"未授权401也算活", http.StatusUnauthorized, true},
		{"重定向302也算活", http.StatusFound, true},
		{"服务端503不算活", http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer ts.Close()
			svc := Service{Upstream: ts.URL}
			p := s.probeService(context.Background(), svc)
			if p.Reachable != tc.reachable || p.Status != tc.status {
				t.Errorf("探活结果不对: %+v（期望 reachable=%v status=%d）", p, tc.reachable, tc.status)
			}
			if p.Reachable && p.Error != "" {
				t.Errorf("可达时不该有 Error: %+v", p)
			}
		})
	}

	t.Run("死上游记Error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		dead := ts.URL
		ts.Close()
		p := s.probeService(context.Background(), Service{Upstream: dead})
		if p.Reachable || p.Error == "" {
			t.Errorf("死上游应不可达且带 Error: %+v", p)
		}
	})

	t.Run("探活缓存读写", func(t *testing.T) {
		if _, ok := s.cachedServiceProbe("none1234"); ok {
			t.Error("无缓存应返回 false")
		}
		want := ServiceProbe{Reachable: true, Status: 204, LatencyMS: 3}
		s.storeServiceProbe("some1234", want)
		got, ok := s.cachedServiceProbe("some1234")
		if !ok || got != want {
			t.Errorf("缓存读写不一致: got=%+v ok=%v want=%+v", got, ok, want)
		}
		s.invalidateServiceMount("some1234")
		if _, ok := s.cachedServiceProbe("some1234"); ok {
			t.Error("invalidate 后不该再有缓存")
		}
	})
}

// TestServiceIDFromPath 验证路径白名单：非法 id 一律取不出来。
func TestServiceIDFromPath(t *testing.T) {
	for _, path := range []string{"/i/abcd1234/", "/s/", "/s/ABC12345/", "/s/ab/", "/s/abcd../x", "/s/abcd1234"} {
		if id := serviceIDFromPath(path); id != "" && path != "/s/abcd1234" {
			t.Errorf("路径 %q 不该取出 id，实际 %q", path, id)
		}
	}
	if id := serviceIDFromPath("/s/abcd1234/api/x"); id != "abcd1234" {
		t.Errorf("合法路径应取出 id，实际 %q", id)
	}
	if id := serviceIDFromPath("/s/abcd1234"); id != "abcd1234" {
		t.Errorf("挂载根应取出 id，实际 %q", id)
	}
}
