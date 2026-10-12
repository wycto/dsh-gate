package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/users"
)

// newTestUsers 建一个带一个账号的账号表。
func newTestUsers(t *testing.T) *users.Store {
	t.Helper()
	store, err := users.Open(filepath.Join(t.TempDir(), "users.json"))
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}
	if err := store.Set("alice", "password-1234", 50_000); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	return store
}

// newGateForTest 造一个带停机触发器的网关，返回它与一个「是否被触发过」的标记。
func newGateForTest(t *testing.T) (*Gate, *bool) {
	t.Helper()
	triggered := false
	gate, err := New(Options{
		Users:    newTestUsers(t),
		Shutdown: func() { triggered = true },
	})
	if err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}
	return gate, &triggered
}

// TestShutdownEndpointOnlyFromLoopback 验证停机端点只认回环来源。
//
// 否则局域网里的任何设备只要知道端口，就能把别人的网关关掉——而这台机器上的
// 用户本来就有别的办法停它，不需要走网络。
func TestShutdownEndpointOnlyFromLoopback(t *testing.T) {
	gate, stop := newGateForTest(t)

	cases := []struct {
		remote string
		want   int
		desc   string
	}{
		{"127.0.0.1:5000", http.StatusOK, "IPv4 回环"},
		{"[::1]:5000", http.StatusOK, "IPv6 回环"},
		{"192.168.1.20:5000", http.StatusForbidden, "局域网来源"},
		{"203.0.113.9:5000", http.StatusForbidden, "公网来源"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodPost, PathShutdown, nil)
		request.RemoteAddr = tc.remote
		recorder := httptest.NewRecorder()
		gate.Handler(http.NotFoundHandler()).ServeHTTP(recorder, request)
		if recorder.Code != tc.want {
			t.Errorf("%s（%s）应返回 %d，实际 %d", tc.desc, tc.remote, tc.want, recorder.Code)
		}
	}
	_ = stop
}

// TestShutdownRequiresPost 验证停机端点只接受 POST。
func TestShutdownRequiresPost(t *testing.T) {
	gate, _ := newGateForTest(t)

	request := httptest.NewRequest(http.MethodGet, PathShutdown, nil)
	request.RemoteAddr = "127.0.0.1:5000"
	recorder := httptest.NewRecorder()
	gate.Handler(http.NotFoundHandler()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Allow"); !strings.Contains(got, "POST") {
		t.Errorf("405 应带上 Allow: POST，实际 %q", got)
	}
}

// TestShutdownAbsentWithoutHook 验证没注入触发器时端点不存在（404）。
func TestShutdownAbsentWithoutHook(t *testing.T) {
	store := newTestUsers(t)
	gate, err := New(Options{Users: store})
	if err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, PathShutdown, nil)
	request.RemoteAddr = "127.0.0.1:5000"
	recorder := httptest.NewRecorder()
	gate.Handler(http.NotFoundHandler()).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未注入触发器时应当 404，实际 %d", recorder.Code)
	}
}
