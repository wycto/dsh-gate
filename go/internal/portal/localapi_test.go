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
