package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// apiFakeLifespan 记录本机 API 对监督者的调用，不真的拉进程。
type apiFakeLifespan struct {
	running     map[string]bool
	starts      []string
	stops       []string
	restarts    []string
	stopErr     error
	startCalled bool
}

func newAPIFakeLifespan() *apiFakeLifespan {
	return &apiFakeLifespan{running: map[string]bool{}}
}

func (f *apiFakeLifespan) InstanceStatus(inst Instance) InstanceStatus {
	return InstanceStatus{Running: f.running[inst.ID]}
}

func (f *apiFakeLifespan) StartInstance(ctx context.Context, inst Instance) error {
	f.starts = append(f.starts, inst.ID)
	f.startCalled = true
	f.running[inst.ID] = true
	return nil
}

func (f *apiFakeLifespan) StopInstance(ctx context.Context, inst Instance) error {
	f.stops = append(f.stops, inst.ID)
	if f.stopErr != nil {
		return f.stopErr
	}
	delete(f.running, inst.ID)
	return nil
}

func (f *apiFakeLifespan) RestartInstance(ctx context.Context, inst Instance) error {
	f.restarts = append(f.restarts, inst.ID)
	f.running[inst.ID] = true
	return nil
}

// newLocalTestServer 造一个只带本机 API 需要的字段的 Server。
func newLocalTestServer(t *testing.T) (*Server, *Store, *apiFakeLifespan) {
	t.Helper()
	store := newStore(t)
	life := newAPIFakeLifespan()
	s := &Server{opts: ServerOptions{Store: store, Lifespan: life, LocalToken: "test-token"}}
	return s, store, life
}

func callLocal(t *testing.T, s *Server, token string, payload localRequest) (*httptest.ResponseRecorder, localResponse) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("编码请求失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, LocalAPIPath, bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:54321"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.handleLocalAPI(rec, req)
	var resp localResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

// TestLocalAPIRequiresLoopbackAndToken 验证本机 API 的两道门：来源与令牌。
func TestLocalAPIRequiresLoopbackAndToken(t *testing.T) {
	s, _, _ := newLocalTestServer(t)

	// 非回环来源：即使令牌对也不行（令牌只防本机其它用户，来源才是边界）。
	raw := []byte(`{"action":"list"}`)
	req := httptest.NewRequest(http.MethodPost, LocalAPIPath, bytes.NewReader(raw))
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.handleLocalAPI(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("非回环请求应 403，实际 %d", rec.Code)
	}

	// 缺令牌 / 错令牌：401。
	for _, token := range []string{"", "wrong-token"} {
		rec, _ := callLocal(t, s, token, localRequest{Action: "list"})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("令牌 %q 应 401，实际 %d", token, rec.Code)
		}
	}
}

// TestLocalAPIListAndLifecycle 验证 list 返回实例与状态，start/stop/restart 落到 Lifespan。
func TestLocalAPIListAndLifecycle(t *testing.T) {
	s, store, life := newLocalTestServer(t)
	inst := &Instance{Name: "demo", DshHome: "/srv/demo", DshPort: 32123}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}

	rec, resp := callLocal(t, s, "test-token", localRequest{Action: "list"})
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("list 应成功，实际 %d %+v", rec.Code, resp)
	}
	if len(resp.Instances) != 1 || resp.Instances[0].Name != "demo" || resp.Instances[0].Port != 32123 {
		t.Fatalf("list 返回不对: %+v", resp.Instances)
	}

	for _, action := range []string{"start", "stop", "restart"} {
		rec, resp = callLocal(t, s, "test-token", localRequest{Action: action, Instance: "demo"})
		if rec.Code != http.StatusOK || !resp.OK {
			t.Fatalf("%s 应成功，实际 %d %+v", action, rec.Code, resp)
		}
	}
	if len(life.starts) != 1 || len(life.stops) != 1 || len(life.restarts) != 1 {
		t.Errorf("监督者调用次数不对: starts=%v stops=%v restarts=%v", life.starts, life.stops, life.restarts)
	}
}

// svcFakeLifespan 记录受管服务生命周期调用，不真拉进程。
type svcFakeLifespan struct {
	started  []string
	stopped  []string
	restart  []string
	state    map[string]string
	logsText string
}

func newSvcFake() *svcFakeLifespan { return &svcFakeLifespan{state: map[string]string{}} }

func (f *svcFakeLifespan) StartService(svc Service) error {
	f.started = append(f.started, svc.ID)
	f.state[svc.ID] = "running"
	return nil
}
func (f *svcFakeLifespan) StopService(id string) error {
	f.stopped = append(f.stopped, id)
	f.state[id] = "stopped"
	return nil
}
func (f *svcFakeLifespan) RestartService(svc Service) error {
	f.restart = append(f.restart, svc.ID)
	f.state[svc.ID] = "running"
	return nil
}
func (f *svcFakeLifespan) Status(id string) ServiceRunStatus {
	return ServiceRunStatus{State: f.state[id], Running: f.state[id] == "running"}
}
func (f *svcFakeLifespan) Logs(id string, tail int) (string, error) { return f.logsText, nil }

func newSvcLocalServer(t *testing.T, withRuntime bool) (*Server, *Store, *svcFakeLifespan) {
	t.Helper()
	store := newStore(t)
	fake := newSvcFake()
	opts := ServerOptions{Store: store, LocalToken: "test-token"}
	if withRuntime {
		opts.ServiceLifespan = fake
	}
	return &Server{opts: opts}, store, fake
}

func mustCreateSvc(t *testing.T, store *Store, name string, managed bool) *Service {
	t.Helper()
	svc := &Service{Name: name, Upstream: "http://127.0.0.1:18080"}
	if managed {
		svc.Managed = true
		svc.StartCmd = "python3 -m http.server 18080"
	}
	if err := store.CreateService(svc); err != nil {
		t.Fatalf("建服务失败: %v", err)
	}
	return svc
}

func TestLocalSvcManagedLifecycleAndLogs(t *testing.T) {
	s, store, fake := newSvcLocalServer(t, true)
	fake.logsText = "line1\nline2"
	svc := mustCreateSvc(t, store, "m1", true)

	for _, action := range []string{"svc.start", "svc.stop", "svc.restart"} {
		rec, resp := callLocal(t, s, "test-token", localRequest{Action: action, Service: "m1"})
		if rec.Code != http.StatusOK || !resp.OK {
			t.Fatalf("%s 应成功，实际 %d %+v", action, rec.Code, resp)
		}
	}
	if len(fake.started) != 1 || len(fake.stopped) != 1 || len(fake.restart) != 1 {
		t.Errorf("生命周期调用次数不对: %+v", fake)
	}
	rec, resp := callLocal(t, s, "test-token", localRequest{Action: "svc.logs", Service: svc.ID, Tail: 10})
	if rec.Code != http.StatusOK || resp.Logs != "line1\nline2" {
		t.Fatalf("logs 应返回内容，实际 %d %q", rec.Code, resp.Logs)
	}
	// list 显示 managed 与运行态
	rec, resp = callLocal(t, s, "test-token", localRequest{Action: "svc.list"})
	if rec.Code != http.StatusOK || len(resp.Services) != 1 || !resp.Services[0].Managed {
		t.Fatalf("list 应显示受管，实际 %+v", resp.Services)
	}
}

func TestLocalSvcProxyRejectsLifecycle(t *testing.T) {
	s, store, _ := newSvcLocalServer(t, true)
	mustCreateSvc(t, store, "px", false)
	rec, resp := callLocal(t, s, "test-token", localRequest{Action: "svc.start", Service: "px"})
	if rec.Code != http.StatusBadRequest || resp.OK {
		t.Fatalf("纯代理服务 start 应 400，实际 %d %+v", rec.Code, resp)
	}
}

func TestLocalSvcLifecycleRequiresRuntime(t *testing.T) {
	s, store, _ := newSvcLocalServer(t, false)
	mustCreateSvc(t, store, "m2", true)
	rec, resp := callLocal(t, s, "test-token", localRequest{Action: "svc.start", Service: "m2"})
	if rec.Code != http.StatusServiceUnavailable || resp.OK {
		t.Fatalf("无运行时 start 应 503，实际 %d %+v", rec.Code, resp)
	}
}

func TestLocalSvcAddManaged(t *testing.T) {
	s, store, _ := newSvcLocalServer(t, true)
	rec, resp := callLocal(t, s, "test-token", localRequest{
		Action: "svc.add", Service: "m3", Upstream: "http://127.0.0.1:18081",
		StartCmd: "sleep 1", AutoStart: true,
	})
	if rec.Code != http.StatusOK || !resp.OK || len(resp.Services) != 1 || !resp.Services[0].Managed {
		t.Fatalf("svc.add 受管应成功，实际 %d %+v", rec.Code, resp)
	}
	got, err := store.ServiceByName("m3")
	if err != nil || !got.Managed || got.StartCmd != "sleep 1" || !got.AutoStart {
		t.Fatalf("受管字段未落库: %+v / %v", got, err)
	}
}

// TestLocalAPIRemoveStopsAndPurges 验证删除：先停、再删目录、最后删记录。
func TestLocalAPIRemoveStopsAndPurges(t *testing.T) {
	s, store, life := newLocalTestServer(t)
	home := filepath.Join(t.TempDir(), "home")
	installDir := filepath.Join(t.TempDir(), "install")
	for _, dir := range []string{home, installDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("准备目录失败: %v", err)
		}
	}
	inst := &Instance{Name: "gone", DshHome: home, InstallDir: installDir, DshPort: 32124}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	life.running[inst.ID] = true

	rec, resp := callLocal(t, s, "test-token", localRequest{
		Action: "remove", Instance: "gone", PurgeHome: true, PurgeInstall: true,
	})
	if rec.Code != http.StatusOK || !resp.OK {
		t.Fatalf("remove 应成功，实际 %d %+v", rec.Code, resp)
	}
	if len(life.stops) != 1 {
		t.Errorf("删除运行中的实例前应先停止，实际 stops=%v", life.stops)
	}
	for _, dir := range []string{home, installDir} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("目录 %s 应已删除，stat err=%v", dir, err)
		}
	}
	if _, err := store.InstanceByID(inst.ID); err == nil {
		t.Error("实例记录应已删除")
	}
}

// TestLocalAPIRemoveRejectsDangerousPath 验证危险路径被拒且实例记录还在。
func TestLocalAPIRemoveRejectsDangerousPath(t *testing.T) {
	s, store, _ := newLocalTestServer(t)
	inst := &Instance{Name: "danger", DshHome: "/", DshPort: 32125}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	rec, resp := callLocal(t, s, "test-token", localRequest{
		Action: "remove", Instance: "danger", PurgeHome: true,
	})
	if rec.Code == http.StatusOK || resp.OK {
		t.Fatalf("拒绝删除根目录，实际 %d %+v", rec.Code, resp)
	}
	if !strings.Contains(resp.Error, "根目录") {
		t.Errorf("错误里应说明拒绝原因，实际 %q", resp.Error)
	}
	if _, err := store.InstanceByID(inst.ID); err != nil {
		t.Error("删除失败时实例记录应保留")
	}
}

func TestLocalSvcDiscoverAdd(t *testing.T) {
	s, store, _ := newLocalTestServer(t)
	_, resp := callLocal(t, s, "test-token", localRequest{
		Action: "svc.discover.add",
		Candidates: []localCandidate{
			{Host: "10.1.1.1", Port: 8080},
			{Host: "10.1.1.1", Port: 8080},
		},
	})
	if resp.Created != 1 || resp.Skipped != 1 {
		t.Fatalf("应新建 1 跳过 1，实际 created=%d skipped=%d", resp.Created, resp.Skipped)
	}
	if _, err := store.ServiceByName("svc-10-1-1-1-8080"); err != nil {
		t.Errorf("应登记服务 svc-10-1-1-1-8080: %v", err)
	}
}
