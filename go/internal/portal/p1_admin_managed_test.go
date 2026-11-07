package portal

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// fakeSvcLifespan 是测试用的 ServiceLifespan：记录被调用的动作并可注入运行态。
type fakeSvcLifespan struct {
	mu        sync.Mutex
	started   []string
	stopped   []string
	restarted []string
	running   map[string]bool
	state     map[string]string
	pid       map[string]int
	logs      string
}

func newFakeSvcLifespan() *fakeSvcLifespan {
	return &fakeSvcLifespan{running: map[string]bool{}, state: map[string]string{}, pid: map[string]int{}, logs: "line-1 line-2"}
}

func (f *fakeSvcLifespan) StartService(svc Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, svc.ID)
	f.running[svc.ID] = true
	f.state[svc.ID] = "running"
	return nil
}
func (f *fakeSvcLifespan) StopService(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	f.running[id] = false
	f.state[id] = "stopped"
	return nil
}
func (f *fakeSvcLifespan) RestartService(svc Service) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, svc.ID)
	f.running[svc.ID] = true
	f.state[svc.ID] = "running"
	return nil
}
func (f *fakeSvcLifespan) Status(id string) ServiceRunStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ServiceRunStatus{Running: f.running[id], State: f.state[id], PID: f.pid[id]}
}
func (f *fakeSvcLifespan) Logs(id string, tail int) (string, error) { return f.logs, nil }

// attachFakeLifespan 把一个假运行时接到 fixture 的 Server 上并重建 handler。
func attachFakeLifespan(t *testing.T, f *portalFixture) *fakeSvcLifespan {
	t.Helper()
	fake := newFakeSvcLifespan()
	f.server.opts.ServiceLifespan = fake
	f.handler = f.server.Handler()
	return fake
}

func mustCreateServiceDirect(t *testing.T, f *portalFixture, svc *Service) *Service {
	t.Helper()
	if err := f.store.CreateService(svc); err != nil {
		t.Fatalf("直接建服务失败: %v", err)
	}
	return svc
}

// TestAdminServiceCreateManaged 验证受管字段落库、缺启动命令被拒、代理服务不受影响。
func TestAdminServiceCreateManaged(t *testing.T) {
	f := newPortalFixture(t)
	sess := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, sess)

	rec := postAdmin(t, f, sess, csrfCookie, csrf, url.Values{
		"action": {"service.create"}, "name": {"m1"},
		"upstream": {"http://127.0.0.1:9999"}, "managed": {"1"},
		"start_cmd": {"sleep 100"}, "auto_start": {"1"}, "env": {"A=1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("建受管服务应 303，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	svc, err := f.store.ServiceByName("m1")
	if err != nil {
		t.Fatalf("受管服务应已落库: %v", err)
	}
	if !svc.Managed || svc.StartCmd != "sleep 100" || !svc.AutoStart || svc.Env != "A=1" {
		t.Errorf("受管字段落库不对: %+v", svc)
	}

	// 受管但缺启动命令 → 被 services.validate 拒绝。
	rec2 := postAdmin(t, f, sess, csrfCookie, csrf, url.Values{
		"action": {"service.create"}, "name": {"bad"},
		"upstream": {"http://127.0.0.1:9997"}, "managed": {"1"},
	})
	if rec2.Code != http.StatusBadRequest {
		t.Errorf("缺启动命令应 400，实际 %d", rec2.Code)
	}
	if _, err := f.store.ServiceByName("bad"); err == nil {
		t.Error("被拒的服务不应落库")
	}

	// 纯代理服务照旧可建，Managed=false。
	rec3 := postAdmin(t, f, sess, csrfCookie, csrf, url.Values{
		"action": {"service.create"}, "name": {"px"},
		"upstream": {"http://127.0.0.1:9996"},
	})
	if rec3.Code != http.StatusSeeOther {
		t.Fatalf("建代理服务应 303，实际 %d", rec3.Code)
	}
	if px, _ := f.store.ServiceByName("px"); px == nil || px.Managed {
		t.Errorf("代理服务不该是 Managed: %+v", px)
	}
}

// TestAdminServiceLifecycle 验证 start/stop/restart 调运行时；纯代理服务拒绝。
func TestAdminServiceLifecycle(t *testing.T) {
	f := newPortalFixture(t)
	fake := attachFakeLifespan(t, f)
	svc := mustCreateServiceDirect(t, f, &Service{Name: "mg", Upstream: "http://127.0.0.1:9999", Managed: true, StartCmd: "sleep 100"})
	proxy := mustCreateServiceDirect(t, f, &Service{Name: "px", Upstream: "http://127.0.0.1:9998"})

	sess := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, sess)

	for _, act := range []string{"service.start", "service.stop", "service.restart"} {
		rec := postAdmin(t, f, sess, csrfCookie, csrf, url.Values{
			"action": {act}, "service_id": {svc.ID}, "name": {"mg"},
		})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s 应 303，实际 %d（%s）", act, rec.Code, rec.Body.String())
		}
	}
	if len(fake.started) != 1 || fake.started[0] != svc.ID {
		t.Errorf("StartService 应被调用一次: %v", fake.started)
	}
	if len(fake.stopped) != 1 || len(fake.restarted) != 1 {
		t.Errorf("stop/restart 调用次数不对: stop=%v restart=%v", fake.stopped, fake.restarted)
	}

	// 纯代理服务 start → 400，且不触达运行时。
	before := len(fake.started)
	rec := postAdmin(t, f, sess, csrfCookie, csrf, url.Values{
		"action": {"service.start"}, "service_id": {proxy.ID}, "name": {"px"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("纯代理服务 start 应 400，实际 %d", rec.Code)
	}
	if len(fake.started) != before {
		t.Error("纯代理服务不该触达运行时")
	}
}

// TestAdminStateServicesManaged 验证 state/live 暴露受管运行态。
func TestAdminStateServicesManaged(t *testing.T) {
	f := newPortalFixture(t)
	fake := attachFakeLifespan(t, f)
	svc := mustCreateServiceDirect(t, f, &Service{Name: "mg", Upstream: "http://127.0.0.1:9999", Managed: true, StartCmd: "sleep 100"})
	fake.mu.Lock()
	fake.running[svc.ID] = true
	fake.state[svc.ID] = "running"
	fake.pid[svc.ID] = 4242
	fake.mu.Unlock()

	sess := f.login(t, "root", "root-password-123")

	rec := f.get(t, AdminStatePath, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("state 应 200，实际 %d", rec.Code)
	}
	var state struct {
		Services []struct {
			ID      string `json:"id"`
			Managed bool   `json:"managed"`
			Running bool   `json:"running"`
			State   string `json:"state"`
			PID     int    `json:"pid"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析 state 失败: %v", err)
	}
	if len(state.Services) != 1 {
		t.Fatalf("应有一条服务，实际 %d", len(state.Services))
	}
	s := state.Services[0]
	if !s.Managed || !s.Running || s.State != "running" || s.PID != 4242 {
		t.Errorf("state 受管字段不对: %+v", s)
	}

	// live 段也应带受管状态文案。
	recLive := f.get(t, AdminLivePath, sess)
	var live struct {
		Services []struct {
			ID        string `json:"id"`
			Managed   bool   `json:"managed"`
			Running   bool   `json:"running"`
			StateText string `json:"stateText"`
		} `json:"services"`
	}
	if err := json.Unmarshal(recLive.Body.Bytes(), &live); err != nil {
		t.Fatalf("解析 live 失败: %v", err)
	}
	if len(live.Services) != 1 || !live.Services[0].Managed || !live.Services[0].Running || live.Services[0].StateText == "" {
		t.Errorf("live 受管字段不对: %+v", live.Services)
	}
}
