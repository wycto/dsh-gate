package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wycto/dsh-gate/internal/dshtest"
	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/portalruntime"
	"github.com/wycto/dsh-gate/internal/users"
)

// stubSupervisor 是 portal run 测试用的假 dsh 监督者：不拉进程，令牌来自假 dsh。
type stubSupervisor struct {
	fake *dshtest.Server

	mu    sync.Mutex
	token string
}

func (s *stubSupervisor) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

func (s *stubSupervisor) SetToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *stubSupervisor) Ready() bool                    { return true }
func (s *stubSupervisor) LastError() string              { return "" }
func (s *stubSupervisor) Run(ctx context.Context)        { <-ctx.Done() }
func (s *stubSupervisor) Shutdown(context.Context) error { return nil }
func (s *stubSupervisor) PID() int                       { return 1 }
func (s *stubSupervisor) Managed() bool                  { return true }
func (s *stubSupervisor) NativeURL() string              { return "" }

// TestPortalRunEndToEnd 是管理端的「最小可用闭环」验收：登录 → 我的实例 → 点进实例 →
// 令牌交接 → 拿到 dsh 的页面。整条链路只用了两个进程内的假件（假 dsh + 假监督者）。
func TestPortalRunEndToEnd(t *testing.T) {
	fake := dshtest.Start()
	defer fake.Close()

	home := t.TempDir()
	dbPath := filepath.Join(home, "portal.db")

	// 准备数据：一个管理员 + 一个指向假 dsh 的实例。
	store, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	if _, err := store.CreateUser("root", "root-password-123", portal.RoleAdmin); err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	inst := &portal.Instance{Name: "alice-main", DshHome: filepath.Join(home, "dsh-alice"), DshPort: fake.Port()}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	_ = store.Close()

	assembly, err := buildPortal(portalRunOptions{
		DBPath:   dbPath,
		StateDir: home,
		Logger:   slog.New(slog.DiscardHandler),
		Launch: func(i portal.Instance, opts portalruntime.LaunchOptions) (portalruntime.Supervisor, error) {
			return &stubSupervisor{fake: fake, token: fake.Token}, nil
		},
	})
	if err != nil {
		t.Fatalf("组装管理端失败: %v", err)
	}
	defer func() { _ = assembly.stop(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := assembly.Runtime.Start(ctx); err != nil {
		t.Fatalf("拉起实例失败: %v", err)
	}

	server := httptest.NewServer(assembly.Handler)
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("造 cookie jar 失败: %v", err)
	}
	client := &http.Client{Jar: jar}

	// 1) 未登录：被送去登录页。
	noRedirect := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("未登录请求失败: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("未登录访问首页应 303 去登录，实际 %d", resp.StatusCode)
	}

	// 健康检查必须回 200：管理端的「就绪」是「自己在服务」，不是「某个 dsh 起来了」。
	// 不设 Ready 的话这里会 503，健康检查与启动脚本都会误判（真机 e2e 踩过这个坑）。
	resp, err = noRedirect.Get(server.URL + "/__dsh_gate/healthz")
	if err != nil {
		t.Fatalf("健康检查请求失败: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查应 200，实际 %d", resp.StatusCode)
	}

	// 2) 登录。
	resp, err = client.PostForm(server.URL+"/__dsh_gate/login", url.Values{
		"username": {"root"}, "password": {"root-password-123"},
	})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录后应落到首页，实际 %d（body: %s）", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "alice-main") {
		t.Fatalf("首页应显示实例卡片，实际 %q", string(body))
	}

	// 3) 点进实例：管理端做令牌交接，拿到的应该是假 dsh 的页面。
	resp, err = client.Get(server.URL + "/i/" + inst.ID + "/")
	if err != nil {
		t.Fatalf("进入实例失败: %v", err)
	}
	instBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("进入实例应 200，实际 %d（body: %s）", resp.StatusCode, string(instBody))
	}
	if !strings.Contains(string(instBody), "DeepSeek Harness") {
		t.Fatalf("应拿到假 dsh 的页面，实际 %q", string(instBody))
	}
	if fake.Exchanges() == 0 {
		t.Error("进入实例时管理端应代替浏览器完成一次令牌交接")
	}
}

func TestPortalRunRefusesWithoutAccounts(t *testing.T) {
	home := t.TempDir()
	_, err := buildPortal(portalRunOptions{
		DBPath:   filepath.Join(home, "portal.db"),
		StateDir: home,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("没有任何账号时应拒绝启动")
	}
	if !strings.Contains(err.Error(), "manage user add") {
		t.Errorf("拒绝启动时要给出建号命令，实际 %q", err.Error())
	}
}

func TestPortalRunImportsLegacyUsers(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, "users.json")
	legacyStore, err := users.Open(legacy)
	if err != nil {
		t.Fatalf("造旧账号表失败: %v", err)
	}
	if err := legacyStore.Set("legacy-admin", "legacy-password-123", 50_000); err != nil {
		t.Fatalf("写旧账号失败: %v", err)
	}

	dbPath := filepath.Join(home, "portal.db")
	assembly, err := buildPortal(portalRunOptions{
		DBPath:   dbPath,
		StateDir: home,
		Logger:   slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("组装管理端失败: %v", err)
	}
	defer func() { _ = assembly.stop(context.Background()) }()

	user, err := assembly.Store.UserByName("legacy-admin")
	if err != nil {
		t.Fatalf("旧账号应被导入: %v", err)
	}
	if user.Role != portal.RoleAdmin {
		t.Errorf("第一个导入的账号应成为管理员，实际 %q", user.Role)
	}
	if _, err := assembly.Store.Authenticate("legacy-admin", "legacy-password-123"); err != nil {
		t.Errorf("旧口令应继续可用: %v", err)
	}
}

// TestPortalRunPassesTrustedHostsToInstances 验证管理端会把「用户从哪访问」
// 推导成 --trusted-host 交给每个实例的 dsh。
//
// 这是「实例一直连接中、工作区打开失败」的回归：dsh 的 /api 浏览器信任栅栏
// 只接受回环、自身绑定地址与被点名的 authority，而管理端把浏览器的 Host 原样
// 递给 dsh。管理端形态当初没有推导这一步，于是页面能打开（GET / 是令牌交换，
// 不过栅栏），而每一个 XHR 与 WebSocket 都 403——前端只表现成「重新连接中」。
//
// 断言落在 public-url 的主机名上，因为它是「用户实际访问的地址」里唯一
// 不可从本机网卡推导出来的那部分（局域网 IP、主机名已有单实例路径覆盖）。
func TestPortalRunPassesTrustedHostsToInstances(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, "portal.db")
	store, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	if _, err := store.CreateUser("root", "root-password-123", portal.RoleAdmin); err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	inst := &portal.Instance{Name: "alice-main", DshHome: filepath.Join(home, "dsh-alice"), DshPort: 32100}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	_ = store.Close()

	var got []string
	assembly, err := buildPortal(portalRunOptions{
		DBPath:    dbPath,
		StateDir:  home,
		PublicURL: "https://gate.example.com/",
		Logger:    slog.New(slog.DiscardHandler),
		Launch: func(i portal.Instance, opts portalruntime.LaunchOptions) (portalruntime.Supervisor, error) {
			got = append([]string(nil), opts.TrustedHosts...)
			return &stubSupervisor{fake: nil, token: ""}, nil
		},
	})
	if err != nil {
		t.Fatalf("组装管理端失败: %v", err)
	}
	defer func() { _ = assembly.stop(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := assembly.Runtime.Start(ctx); err != nil {
		t.Fatalf("拉起实例失败: %v", err)
	}

	if len(got) == 0 {
		t.Fatal("实例的 dsh 没有拿到任何 --trusted-host，浏览器每个 API 都会 403")
	}
	found := false
	for _, host := range got {
		if host == "gate.example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("public-url 的主机名应当在 trusted-host 里，实际 %v", got)
	}
}
