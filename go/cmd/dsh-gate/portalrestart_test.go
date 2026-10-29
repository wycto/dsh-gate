// 「管理台里点重启之后，实例入口立即可用」的端到端回归测试。
//
// 这个文件只放测试代码，不改任何生产代码：它复刻的是一条真实踩过的故障链路——
//
//	用户点进 /i/<id>/  →  管理端把上游处理器连同挂载前缀缓存进 mounts
//	用户在管理台点「重启」  →  运行时停掉旧监督者、丢掉条目、按新配置重建
//	用户再点进 /i/<id>/   →  命中缓存里的旧处理器  →  旧监督者已被 Shutdown
//	                        →  Ready() 恒 false  →  永远停在「DSH 正在启动」
//
// 断言只有两条，都落在用户能看见的东西上：状态码必须是 200（不是 503），
// 页面必须是 dsh 的首页（不是那张等待页）。中间用假 dsh + 假监督者顶替真进程，
// 但「监督者被 Shutdown 之后不再就绪」这条语义是真的——它是整个 bug 的关键。
package main

import (
	"bytes"
	"context"
	"encoding/json"
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
)

// restartSupervisor 是复刻真实语义的假监督者：Shutdown 之后 Ready() 恒 false。
//
// 与 portalrun_test.go 里的 stubSupervisor 的区别只有这一条——那边 Ready 恒真，
// 用它写这条测试会「永远通过」，因为陈旧处理器看起来照样就绪。
type restartSupervisor struct {
	fake *dshtest.Server

	mu      sync.Mutex
	token   string
	stopped int
}

func (s *restartSupervisor) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

func (s *restartSupervisor) SetToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *restartSupervisor) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped == 0
}

func (s *restartSupervisor) LastError() string       { return "" }
func (s *restartSupervisor) Run(ctx context.Context) { <-ctx.Done() }
func (s *restartSupervisor) Shutdown(context.Context) error {
	s.mu.Lock()
	s.stopped++
	s.mu.Unlock()
	return nil
}
func (s *restartSupervisor) PID() int          { return 4242 }
func (s *restartSupervisor) Managed() bool     { return true }
func (s *restartSupervisor) NativeURL() string { return "" }

// StopAdopted 假监督者不接管外部进程，只需要满足接口。
func (s *restartSupervisor) StopAdopted(context.Context) error { return nil }

// TestPortalMountSurvivesInstanceRestart 是「管理台缓存了挂载 → 重启实例 →
// /i/<id>/ 仍能通」的端到端回归测试。
//
// 走完整链路而不是直接调 Runtime：这个 bug 的成因在 portal.Server.mountFor 的
// 缓存与 portalruntime.Runtime.Upstream 的返回值之间的配合上，只在管理台这一层
// 才暴露。所以测试从登录开始，经真实 HTTP 请求驱动。
func TestPortalMountSurvivesInstanceRestart(t *testing.T) {
	fake := dshtest.Start()
	defer fake.Close()

	home := t.TempDir()
	dbPath := filepath.Join(home, "portal.db")

	store, err := portal.Open(dbPath)
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	if _, err := store.CreateUser("root", "root-password-123", portal.RoleAdmin); err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	// 端口显式指向假 dsh：CreateInstance 在端口为 0 时会自己分配，那样代理就打不到假 dsh。
	inst := &portal.Instance{
		Name:    "restart-e2e",
		DshHome: filepath.Join(home, "dsh-restart"),
		DshPort: fake.Port(),
	}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	_ = store.Close()

	// 每次 Launch 都造一个新监督者，并把它们记下来——「重启确实换了监督者」
	// 是这条测试的前提，否则它断言的只是一次没发生过的重启。
	var mu sync.Mutex
	launched := make([]*restartSupervisor, 0, 4)
	assembly, err := buildPortal(portalRunOptions{
		DBPath:   dbPath,
		StateDir: home,
		Logger:   slog.New(slog.DiscardHandler),
		Launch: func(i portal.Instance, opts portalruntime.LaunchOptions) (portalruntime.Supervisor, error) {
			sup := &restartSupervisor{fake: fake, token: fake.Token}
			mu.Lock()
			launched = append(launched, sup)
			mu.Unlock()
			return sup, nil
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

	// 1) 登录（跟随后重定向，落到管理台首页）。
	resp, err := client.PostForm(server.URL+"/__dsh_gate/login", url.Values{
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

	// 2) 第一次点进实例：这次请求会让管理端把上游处理器**缓存**进 mounts。
	//    没有这一步，后面的重启就绕开了缓存，测试也就测不到这个 bug。
	firstBody := mountPage(t, client, server.URL+"/i/"+inst.ID+"/", "重启前")

	mu.Lock()
	before := len(launched)
	oldSup := launched[before-1]
	mu.Unlock()
	if oldSup == nil {
		t.Fatal("重启前应当已经拉起一个监督者")
	}

	// 3) 从管理台点「重启」：走 JSON API（页面按钮与脚本走同一条动作实现），
	//    只带会话 Cookie + 同源 + application/json，不需要 csrf 字段。
	//
	//    键名写 instanceId 而不是 instance_id：这个结构体走的是 encoding/json 的
	//    字段名匹配（大小写不敏感），instance_id 对不上 InstanceID，会被当成未知
	//    字段丢掉，然后以「missing instance_id」400 失败。
	actBody, err := json.Marshal(map[string]string{
		"action":     "instance.restart",
		"instanceId": inst.ID,
	})
	if err != nil {
		t.Fatalf("序列化动作失败: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/_portal/api/admin/action", bytes.NewReader(actBody))
	if err != nil {
		t.Fatalf("造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", server.URL) // 同源校验：Origin 的 host 必须等于请求的 Host。
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("重启请求失败: %v", err)
	}
	actResp, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重启应成功，实际 %d（body: %s）", resp.StatusCode, string(actResp))
	}
	if !strings.Contains(string(actResp), `"ok":true`) {
		t.Fatalf("重启响应应带 ok:true，实际 %s", string(actResp))
	}

	// 前提断言：重启确实重建了监督者（旧的那个已经 Shutdown、不再就绪）。
	mu.Lock()
	after := len(launched)
	newSup := launched[after-1]
	mu.Unlock()
	if after == before {
		t.Fatal("重启应当重建监督者：复用旧监督者等于按旧配置启动，改过的端口与目录不会生效")
	}
	if newSup == oldSup {
		t.Fatal("重启应当换一个新监督者")
	}
	if oldSup.Ready() {
		t.Fatal("旧监督者被 Shutdown 之后不该再就绪——这正是旧处理器会卡在等待页的原因")
	}
	if !newSup.Ready() {
		t.Fatal("新监督者应处于就绪状态")
	}

	// 4) 再点进**同一个**挂载路径：管理端会命中缓存里的旧处理器。
	//    它必须现查当前条目、落到新监督者上，而不是停在「DSH 正在启动」。
	//
	//    这里刻意用同一个 client（同一个 cookie jar）：浏览器刷新页面就是这种状态，
	//    交接标记与 dsh 会话都还在。
	secondBody := mountPage(t, client, server.URL+"/i/"+inst.ID+"/", "重启后")

	// 断言「不是等待页」而不是只断言「含 dsh 字样」：等待页也来自网关自己，
	// 只比字符串很容易漏掉症状。等待页的标题字面就是这四个字。
	for _, page := range []string{firstBody, secondBody} {
		if strings.Contains(page, "DSH 正在启动") {
			t.Fatalf("不该拿到等待页，实际 body: %q", page)
		}
	}
	if !strings.Contains(secondBody, "DeepSeek Harness") {
		t.Fatalf("重启后应拿到假 dsh 的首页，实际 %q", secondBody)
	}
}

// mountPage 请求一次实例挂载路径，断言 200 + dsh 首页内容，返回页面正文。
func mountPage(t *testing.T, client *http.Client, rawURL, when string) string {
	t.Helper()
	resp, err := client.Get(rawURL)
	if err != nil {
		t.Fatalf("%s请求 %s 失败: %v", when, rawURL, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s进入实例应 200，实际 %d（body: %s）", when, resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "DeepSeek Harness") {
		t.Fatalf("%s应拿到假 dsh 的页面，实际 %q", when, string(body))
	}
	return string(body)
}
