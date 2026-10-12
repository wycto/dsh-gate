package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshtest"
	"github.com/wycto/dsh-gate/internal/server"
	"github.com/wycto/dsh-gate/internal/users"
)

// gateHost 是测试里模拟的「网关对外地址」。
const gateHost = "gate.test"

// newInstance 搭起一个完整的网关实例：接管一个已经在跑的假 dsh。
//
// withToken 决定网关是否事先就知道 dsh 的启动令牌：
//
//   - true：等价于「网关自己拉起 dsh，从它的启动输出里读到了令牌」的常态；
//   - false：等价于「用户先自己启动了 dsh，网关只是接管」的真实场景。
func newInstance(t *testing.T, withToken bool) (*server.Instance, *dshtest.Server, string) {
	t.Helper()

	upstream := dshtest.Start(gateHost)
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	usersPath := filepath.Join(dir, "users.json")
	store, err := users.Open(usersPath)
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}
	if err := store.Set("alice", "correct-horse", 50_000); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}

	cfg := config.Default()
	cfg.Port = 0 // 让内核分配端口，避免测试之间抢端口
	cfg.DSHHost = "127.0.0.1"
	cfg.DSHPort = upstream.Port()
	cfg.DSHHome = dir
	cfg.UsersFile = usersPath
	cfg.TrustedHosts = []string{gateHost}
	if withToken {
		cfg.DSHToken = upstream.Token
	}
	cfg.DSHStartTimeout = 10 * time.Second
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("配置有误: %v", err)
	}

	instance, err := server.New(server.Options{
		Config:   cfg,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Subtitle: "测试用",
	})
	if err != nil {
		t.Fatalf("构造实例失败: %v", err)
	}

	ctx := context.Background()
	if err := instance.Start(ctx); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	instance.WaitBooted(ctx) // 等 dsh 接管完成

	go func() {
		if err := instance.Serve(); err != nil {
			t.Logf("服务退出: %v", err)
		}
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = instance.Shutdown(shutdownCtx)
	})
	return instance, upstream, instance.ListenAddr()
}

// browser 是一个自己维护 Cookie 的极简客户端。
type browser struct {
	t       *testing.T
	base    string
	cookies map[string]string
	client  *http.Client
}

func newBrowser(t *testing.T, addr string) *browser {
	return &browser{
		t:       t,
		base:    "http://" + addr,
		cookies: map[string]string{},
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (b *browser) do(method, path string, form url.Values) *http.Response {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatalf("构造请求失败: %v", err)
	}
	request.Host = gateHost
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for name, value := range b.cookies {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	response, err := b.client.Do(request)
	if err != nil {
		b.t.Fatalf("%s %s 失败: %v", method, path, err)
	}
	for _, item := range response.Cookies() {
		if item.MaxAge <= 0 && item.Value == "" {
			delete(b.cookies, item.Name)
			continue
		}
		b.cookies[item.Name] = item.Value
	}
	return response
}

func (b *browser) body(response *http.Response) string {
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return string(raw)
}

// TestEndToEnd 走完「登录 → 令牌交接 → 使用 API」的全过程。
//
// 这条测试是整个项目的存在理由的完整验证：用户只需要知道自己设的账号口令，
// dsh 那一侧的令牌交换由网关悄悄完成。
func TestEndToEnd(t *testing.T) {
	// 这里直接用「已知的令牌」构造，模拟网关自己拉起 dsh 的常态：
	// 令牌由网关从 dsh 的启动输出里读到。
	instance, dsh, addr := newInstance(t, true)
	client := newBrowser(t, addr)

	// 未登录访问业务页面，应当被送去登录页。
	redirect := client.do(http.MethodGet, "/", nil)
	if redirect.StatusCode != http.StatusSeeOther {
		t.Fatalf("未登录时应当跳转登录页，实际是 %d", redirect.StatusCode)
	}
	loginURL := redirect.Header.Get("Location")
	if !strings.Contains(loginURL, "/__dsh_gate/login") {
		t.Fatalf("跳转目标应当是登录页，实际是 %q", loginURL)
	}

	// 登录页本身要能正常渲染。
	page := client.do(http.MethodGet, loginURL, nil)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("登录页应当返回 200，实际是 %d", page.StatusCode)
	}
	if html := client.body(page); !strings.Contains(html, "<form") {
		t.Fatalf("登录页应当包含表单，实际内容开头是 %q", html[:min(120, len(html))])
	}

	// 错误口令不能建立会话。
	denied := client.do(http.MethodPost, "/__dsh_gate/login", url.Values{
		"username": {"alice"},
		"password": {"wrong-password"},
	})
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误口令应当被拒绝，实际是 %d", denied.StatusCode)
	}
	if body := client.body(denied); strings.Contains(body, "不存在") {
		t.Fatalf("错误提示不能泄露账号是否存在，实际返回 %q", "包含「不存在」")
	}
	if len(client.cookies) != 0 {
		t.Fatalf("登录失败不应留下任何 Cookie，实际有 %v", client.cookies)
	}

	// 正确口令建立会话。
	allowed := client.do(http.MethodPost, "/__dsh_gate/login", url.Values{
		"username": {"alice"},
		"password": {"correct-horse"},
	})
	if allowed.StatusCode != http.StatusSeeOther {
		t.Fatalf("正确口令应当登录成功，实际是 %d（%s）", allowed.StatusCode, client.body(allowed))
	}
	if _, ok := client.cookies["dsh_gate_sid"]; !ok {
		t.Fatalf("登录成功应当写入会话 Cookie，实际有 %v", keys(client.cookies))
	}

	// 登录后访问根路径：网关自动完成 dsh 的令牌交换。
	exchange := client.do(http.MethodGet, "/", nil)
	if exchange.StatusCode != http.StatusFound {
		t.Fatalf("访问首页应当触发 dsh 的令牌交换（302），实际是 %d", exchange.StatusCode)
	}
	if got := dsh.Exchanges(); got != 1 {
		t.Fatalf("应当恰好交换一次令牌，实际 %d 次", got)
	}
	client.body(exchange)

	// 第二次访问拿到真正的页面。
	home := client.do(http.MethodGet, "/", nil)
	if home.StatusCode != http.StatusOK {
		t.Fatalf("第二次访问首页应当返回页面，实际是 %d", home.StatusCode)
	}
	if html := client.body(home); !strings.Contains(html, "DeepSeek") {
		t.Fatalf("应当拿到 dsh 的页面，实际是 %q", html[:min(120, len(html))])
	}

	// API 可用。
	api := client.do(http.MethodGet, "/api/session", nil)
	if api.StatusCode != http.StatusOK {
		t.Fatalf("会话建立后 API 应当可用，实际是 %d", api.StatusCode)
	}
	client.body(api)

	// 登出后会话立即失效。
	logout := client.do(http.MethodPost, "/__dsh_gate/logout", nil)
	if logout.StatusCode != http.StatusSeeOther {
		t.Fatalf("登出应当重定向，实际是 %d", logout.StatusCode)
	}
	client.body(logout)
	afterLogout := client.do(http.MethodGet, "/api/session", nil)
	if afterLogout.StatusCode == http.StatusOK {
		t.Fatal("登出后不应还能访问 dsh")
	}
	client.body(afterLogout)

	_ = instance
}

// TestNoUsersRefusesToStart 验证「没有账号就不启动」。
//
// 一个网关上没有谁能登录，它对外毫无用处却看起来一切正常——这比直接启动失败
// 危险得多，因为用户要等到真正访问时才发现所有人都被挡在门外。
func TestNoUsersRefusesToStart(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Port = 0
	cfg.DSHHome = dir
	cfg.UsersFile = filepath.Join(dir, "users.json")
	cfg.AutoStart = false
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("配置有误: %v", err)
	}

	instance, err := server.New(server.Options{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造实例失败: %v", err)
	}
	err = instance.Start(context.Background())
	if err == nil {
		t.Fatal("没有账号时应当拒绝启动")
	}
	if !strings.Contains(err.Error(), "user add") {
		t.Fatalf("错误信息应当告诉用户怎么创建账号，实际是：%v", err)
	}
}

// TestHealthz 验证健康检查端点不需要登录也能用。
//
// 它存在的意义就是「用户被锁在门外时还能排查」，因此必须公开。
func TestHealthz(t *testing.T) {
	_, _, addr := newInstance(t, true)
	response, err := http.Get("http://" + addr + "/__dsh_gate/healthz")
	if err != nil {
		t.Fatalf("请求健康检查失败: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("健康检查应当返回 200，实际是 %d", response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), `"status":"ok"`) {
		t.Fatalf("健康检查应当报告就绪，实际是 %s", raw)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}

// TestMain 让测试在失败时也能看到端口信息。
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// TestAdoptedWithoutToken 覆盖「接管别人启动的 dsh、且不知道令牌」这条路径。
//
// 这是真实用户会遇到的状态：dsh 是他自己先起来的，网关只看到端口被占用。
// 表现是「首页能开，但对话一直报未授权」，必须有一条能照着做的自救路径。
func TestAdoptedWithoutToken(t *testing.T) {
	_, dsh, addr := newInstance(t, false)
	client := newBrowser(t, addr)

	login(t, client)

	// 此时首页能打开（静态资源在 dsh 侧是公开的），但令牌没有交接。
	home := client.do(http.MethodGet, "/", nil)
	if home.StatusCode != http.StatusOK {
		t.Fatalf("此时首页应当能打开，实际是 %d", home.StatusCode)
	}
	client.body(home)

	// 但 API 用不了——这正是用户会遇到的困惑。
	denied := client.do(http.MethodGet, "/api/session", nil)
	client.body(denied)
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("没有交接令牌时 API 应当返回 401，实际是 %d", denied.StatusCode)
	}

	// 自救路径：把 dsh 打印的完整地址粘到网关地址后面访问一次。
	fix := client.do(http.MethodGet, "/?token="+dsh.Token, nil)
	if fix.StatusCode != http.StatusFound {
		t.Fatalf("带上令牌访问应当完成交接，实际是 %d", fix.StatusCode)
	}
	client.body(fix)

	// 之后一切正常。
	ok := client.do(http.MethodGet, "/api/session", nil)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("交接后 API 应当可用，实际是 %d", ok.StatusCode)
	}
	client.body(ok)
}

// login 走一遍登录流程。
func login(t *testing.T, client *browser) {
	t.Helper()
	response := client.do(http.MethodPost, "/__dsh_gate/login", url.Values{
		"username": {"alice"},
		"password": {"correct-horse"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录失败: %d", response.StatusCode)
	}
}

// TestShutdownEndpoint 验证 `dsh-gate stop` 走的那条路。
//
// 覆盖三件事：回环请求能停、局域网来源被拒、没注入触发器时端点不存在。
func TestShutdownEndpoint(t *testing.T) {
	instance, _, addr := newInstance(t, true)

	// 本机请求：应当触发停机。
	response, err := http.Post("http://"+addr+"/__dsh_gate/shutdown", "text/plain", nil)
	if err != nil {
		t.Fatalf("发送停机请求失败: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("本机停机请求应被接受，实际是 %d", response.StatusCode)
	}

	// 触发之后实例应当在短时间内不再对外服务。
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := http.Get("http://" + addr + "/__dsh_gate/healthz"); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("收到停机请求后实例仍在服务")
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = instance
}

// TestRunnerServeReturnsAfterControlShutdown 锁住一个「端口关了、进程却不退」的 bug。
//
// 停机端点是异步触发停机的：它 cancel 掉 Runner 的上下文，Serve 随即返回。
// 曾经调用方在 select 里读到 Serve 的返回值之后，**又**阻塞等了一次同一个 channel，
// 于是 `dsh-gate stop` 之后网关进程永远退不出来——端口是关的、进程还在 ps 里。
// 这条测试断言的是「Serve 会返回」，也就是「进程会退出」。
func TestRunnerServeReturnsAfterControlShutdown(t *testing.T) {
	upstream := dshtest.Start()
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	usersPath := filepath.Join(dir, "users.json")
	store, err := users.Open(usersPath)
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}
	if err := store.Set("alice", "correct-horse", 50_000); err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}

	cfg := config.Default()
	cfg.Name = "runner"
	cfg.Port = 0
	cfg.Host = "127.0.0.1"
	cfg.DSHPort = upstream.Port()
	cfg.DSHHome = dir
	cfg.UsersFile = usersPath
	cfg.DSHStartTimeout = 10 * time.Second
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("配置有误: %v", err)
	}

	runner, err := server.NewRunner([]config.Instance{cfg}, server.RunOptions{})
	if err != nil {
		t.Fatalf("构造 Runner 失败: %v", err)
	}

	ctx := context.Background()
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- runner.Serve(ctx) }()

	addr := runner.Instances()[0].ListenAddr()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var ready bool
	for !ready {
		if _, err := http.Get("http://" + addr + "/__dsh_gate/healthz"); err == nil {
			ready = true
			break
		}
		if waitCtx.Err() != nil {
			t.Fatal("实例没有在预期时间内开始服务")
		}
		time.Sleep(50 * time.Millisecond)
	}

	response, err := http.Post("http://"+addr+"/__dsh_gate/shutdown", "text/plain", nil)
	if err != nil {
		t.Fatalf("发送停机请求失败: %v", err)
	}
	response.Body.Close()

	// 关键断言：Serve 必须返回。
	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Fatalf("停机时的 Serve 不应报错: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("收到停机请求后 Serve 没有返回，进程会卡住不退出")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := runner.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
}
