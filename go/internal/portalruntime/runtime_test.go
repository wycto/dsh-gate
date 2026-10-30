package portalruntime

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshtest"
	"github.com/wycto/dsh-gate/internal/portal"
)

// fakeSupervisor 是一个不拉进程的假 dsh 监督者：令牌来自假 dsh，Ready 恒真。
type fakeSupervisor struct {
	fake *dshtest.Server

	mu      sync.Mutex
	token   string
	ran     int
	stopped bool // 就绪状态：Run 起来为 false，Shutdown 之后为 true
	// stopCalls 数 Shutdown 被调了几次；与 stopped 分开，因为重新 Run 会清掉
	// stopped（真监督者就是这样：重新起来了就重新就绪），而记账不能跟着清零。
	stopCalls int
	// adoptedStops 数的是「停掉接管实例」这一动作被调了几次。
	adoptedStops int
}

func newFakeSupervisor(fake *dshtest.Server) *fakeSupervisor {
	return &fakeSupervisor{fake: fake, token: fake.Token}
}

func (f *fakeSupervisor) Token() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

func (f *fakeSupervisor) SetToken(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = token
}

// Ready 与真监督者保持一致：Shutdown 之后就不再就绪，重新 Run 起来之后又就绪。
//
// 这条语义是「重启后卡在等待页」那个 bug 的关键：旧监督者被 Shutdown 后
// Ready() 恒 false，谁还握着它，谁的请求就永远落在「DSH 正在启动」上。
func (f *fakeSupervisor) Ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.stopped
}

func (f *fakeSupervisor) LastError() string { return "" }
func (f *fakeSupervisor) PID() int          { return 4242 }
func (f *fakeSupervisor) Managed() bool     { return true }
func (f *fakeSupervisor) NativeURL() string { return "" }

func (f *fakeSupervisor) Run(ctx context.Context) {
	f.mu.Lock()
	f.ran++
	f.stopped = false
	f.mu.Unlock()
	<-ctx.Done()
}

func (f *fakeSupervisor) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	f.stopCalls++
	f.stopped = true
	f.mu.Unlock()
	return nil
}

// StopAdopted 记下这一次「停掉接管实例」的调用。
//
// 假监督者不拉真进程，但它必须被调到：接管来的实例只靠 Shutdown 是停不掉的
// （没有子进程句柄），漏掉这一步就是管理台上「点了停止、端口还被人占着」。
func (f *fakeSupervisor) StopAdopted(ctx context.Context) error {
	f.mu.Lock()
	f.adoptedStops++
	f.mu.Unlock()
	return nil
}

func (f *fakeSupervisor) runCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.ran }
func (f *fakeSupervisor) stopCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.stopCalls }

func (f *fakeSupervisor) adoptedStopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.adoptedStops
}

func newStore(t *testing.T) *portal.Store {
	t.Helper()
	store, err := portal.Open(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatalf("打开管理端库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newRuntime 造一个运行时：假 dsh 监听在它自己的端口上，实例登记指过去。
func newRuntime(t *testing.T, trustedHost string) (*Runtime, *portal.Store, *dshtest.Server, map[string]*fakeSupervisor) {
	t.Helper()
	store := newStore(t)
	fake := dshtest.Start(trustedHost)
	t.Cleanup(fake.Close)

	supers := map[string]*fakeSupervisor{}
	rt, err := New(Options{
		Store: store,
		Launch: func(inst portal.Instance, opts LaunchOptions) (Supervisor, error) {
			sup := newFakeSupervisor(fake)
			supers[inst.ID] = sup
			return sup, nil
		},
	})
	if err != nil {
		t.Fatalf("构造运行时失败: %v", err)
	}
	return rt, store, fake, supers
}

// waitFor 等一个条件成立：拉起监督者发生在 goroutine 里，断言不能抢跑。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时", what)
}

// TestUpstreamPerformsTokenHandoff 是这一层的核心断言：走管理端的代理能真的从假 dsh
// 换到会话（也就是「用户点进实例就已经是登录态」这件事在运行时长这样）。
//
// 用真 http.Client + CookieJar 发请求：交接过程包含 dsh 侧的一次 302，浏览器会跟着走，
// 测试也必须跟着走，否则断言的是一段没人会看到的中间状态。
func TestUpstreamPerformsTokenHandoff(t *testing.T) {
	rt, _, fake, _ := newRuntime(t, "portal.test")

	inst := portal.Instance{ID: "aaaaaaaa", Name: "alice-main", DshHome: "/srv/dsh/alice", DshPort: fake.Port(), Enabled: true}
	handler, err := rt.Upstream(inst)
	if err != nil {
		t.Fatalf("构造上游失败: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("造 cookie jar 失败: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}

	resp, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("第一次访问应完成交接并成功，实际 %d（body: %s）", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "DeepSeek Harness") {
		t.Errorf("应拿到假 dsh 的首页，实际 %q", string(body))
	}
	if fake.Exchanges() != 1 {
		t.Fatalf("假 dsh 应完成 1 次令牌交换，实际 %d", fake.Exchanges())
	}

	// 交接标记必须被种下，否则每次刷新都会再交接一次。
	parsed, _ := url.Parse(server.URL)
	found := false
	for _, c := range jar.Cookies(parsed) {
		if c.Name == "dsh_gate_ds" {
			found = true
		}
	}
	if !found {
		t.Error("交接后应种下 dsh_gate_ds 标记")
	}

	// 第二次访问：已交接，不该再换一次令牌。
	resp2, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("第二次请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp2.Body)
	_ = resp2.Body.Close()
	if fake.Exchanges() != 1 {
		t.Errorf("已交接的浏览器不该重复交换令牌，实际 %d 次", fake.Exchanges())
	}
}

func TestStartLaunchesResidentInstancesOnly(t *testing.T) {
	rt, store, fake, supers := newRuntime(t, "portal.test")

	resident := &portal.Instance{Name: "resident", DshHome: "/srv/a", DshPort: fake.Port()}
	onDemand := &portal.Instance{Name: "on-demand", DshHome: "/srv/b", DshPort: fake.Port(), OnDemand: true}
	disabled := &portal.Instance{Name: "disabled", DshHome: "/srv/c", DshPort: fake.Port()}
	for _, inst := range []*portal.Instance{resident, onDemand, disabled} {
		if err := store.CreateInstance(inst); err != nil {
			t.Fatalf("建实例失败: %v", err)
		}
	}
	disabled.Enabled = false
	if err := store.UpdateInstance(*disabled); err != nil {
		t.Fatalf("停用实例失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	waitFor(t, "常驻实例被拉起", func() bool {
		return supers[resident.ID] != nil && supers[resident.ID].runCount() > 0
	})
	if supers[onDemand.ID] != nil {
		t.Error("按需实例不该在管理端启动时被拉起")
	}
	if supers[disabled.ID] != nil {
		t.Error("已停用的实例不该被拉起")
	}
}

func TestOnDemandInstanceStartsLazily(t *testing.T) {
	rt, _, fake, supers := newRuntime(t, "portal.test")

	inst := portal.Instance{ID: "bbbbbbbb", Name: "lazy", DshHome: "/srv/lazy", DshPort: fake.Port(), OnDemand: true, Enabled: true}
	if _, err := rt.Upstream(inst); err != nil {
		t.Fatalf("构造上游失败: %v", err)
	}
	waitFor(t, "按需实例在首次访问时被拉起", func() bool {
		return supers[inst.ID] != nil && supers[inst.ID].runCount() > 0
	})
}

func TestStopShutsDownInstances(t *testing.T) {
	rt, store, fake, supers := newRuntime(t, "portal.test")
	inst := &portal.Instance{Name: "solo", DshHome: "/srv/solo", DshPort: fake.Port()}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	waitFor(t, "实例被拉起", func() bool {
		return supers[inst.ID] != nil && supers[inst.ID].runCount() > 0
	})

	if err := rt.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if supers[inst.ID].stopCount() == 0 {
		t.Error("Stop 应调用监督者的 Shutdown")
	}
}

func TestStatusReportsInstanceState(t *testing.T) {
	rt, _, fake, _ := newRuntime(t, "portal.test")
	inst := portal.Instance{ID: "cccccccc", Name: "stat", DshHome: "/srv/stat", DshPort: fake.Port(), Enabled: true}

	if got := rt.Status(inst.ID); got.Running {
		t.Error("还没接管过的实例不应报告为运行中")
	}
	if _, err := rt.Upstream(inst); err != nil {
		t.Fatalf("构造上游失败: %v", err)
	}
	got := rt.Status(inst.ID)
	if !got.Ready || got.PID != 4242 || !got.Managed || !got.Running {
		t.Errorf("状态不对: %+v", got)
	}
}

// 编译期提醒：假监督者必须满足接口。
var _ Supervisor = (*fakeSupervisor)(nil)

// TestStartStopRestartSingleInstance 验证管理台用的「单实例启停」三件套。
//
// 这是「停止 → 改配置 → 重启」链路的运行时底座。重启必须**丢掉旧监督者**按
// 新实例记录重建：监督者的端口、DSH_HOME、启动命令都是构造时固定的，拿旧的
// 再跑一次等于按老配置启动，用户改的东西一个都不会生效。
func TestStartStopRestartSingleInstance(t *testing.T) {
	rt, store, fake, supers := newRuntime(t, "portal.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("启动运行时失败: %v", err)
	}

	inst := &portal.Instance{Name: "lifecycle", DshHome: "/srv/dsh/life", DshPort: fake.Port()}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}

	// 1) 启动：状态变运行中，监督者被跑起来。
	if err := rt.StartInstance(ctx, *inst); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	st := rt.Status(inst.ID)
	if !st.Running || !st.Ready {
		t.Fatalf("启动后应为运行中且就绪，实际 %+v", st)
	}
	oldSup := supers[inst.ID]
	if oldSup == nil {
		t.Fatal("启动后应当有监督者")
	}
	waitFor(t, "监督者跑起来", func() bool { return oldSup.runCount() > 0 })

	// 2) 停止：状态回到未运行，监督者收到 Shutdown。
	if err := rt.StopInstance(ctx, inst.ID); err != nil {
		t.Fatalf("停止实例失败: %v", err)
	}
	if got := rt.Status(inst.ID); got.Running {
		t.Fatalf("停止后不该是运行中，实际 %+v", got)
	}
	if oldSup.stopCount() == 0 {
		t.Error("停止时应当调用监督者的 Shutdown")
	}

	// 3) 重启：必须用**新**监督者（旧条目被丢弃、代理按新端口重建）。
	inst.DshPort = fake.Port()
	if err := rt.RestartInstance(ctx, *inst); err != nil {
		t.Fatalf("重启实例失败: %v", err)
	}
	newSup := supers[inst.ID]
	if newSup == nil {
		t.Fatal("重启后应当有监督者")
	}
	if newSup == oldSup {
		t.Fatal("重启应当重建监督者：复用旧监督者等于按旧配置启动，改过的端口/目录不会生效")
	}
	if got := rt.Status(inst.ID); !got.Running {
		t.Fatalf("重启后应为运行中，实际 %+v", got)
	}

	// 4) 幂等：重复启动不重复构造。
	if err := rt.StartInstance(ctx, *inst); err != nil {
		t.Fatalf("重复启动应幂等，实际报错 %v", err)
	}
	if again := supers[inst.ID]; again != newSup {
		t.Error("重复启动不该重建监督者")
	}
}

// TestUpstreamHandlerSurvivesRestart 是「重启之后点进入一直卡在 DSH 正在启动」
// 这个 bug 的回归测试。
//
// 管理端会把 Upstream 返回的处理器按实例缓存起来（portal.Server.mountFor），
// 于是 Upstream 交出去的必须是一个**间接**处理器：它每次请求都现查当前条目，
// 而不是把自己绑死在第一次进入时的那个监督者上。否则重启换了监督者、缓存里的
// 旧处理器还指着已经 Shutdown 的那个，Ready() 恒 false → 永远停在等待页。
func TestUpstreamHandlerSurvivesRestart(t *testing.T) {
	rt, store, fake, supers := newRuntime(t, "portal.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("启动运行时失败: %v", err)
	}

	inst := &portal.Instance{Name: "restarty", DshHome: "/srv/dsh/resty", DshPort: fake.Port()}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	if err := rt.StartInstance(ctx, *inst); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}

	// 关键：只取一次处理器（模拟管理端把它缓存进 mounts）。
	handler, err := rt.Upstream(*inst)
	if err != nil {
		t.Fatalf("构造上游失败: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("造 cookie jar 失败: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	if code := getStatus(t, client, server.URL+"/"); code != http.StatusOK {
		t.Fatalf("重启前应能进入实例，实际 %d", code)
	}

	oldSup := supers[inst.ID]
	if err := rt.RestartInstance(ctx, *inst); err != nil {
		t.Fatalf("重启实例失败: %v", err)
	}
	waitFor(t, "新监督者跑起来", func() bool {
		return supers[inst.ID] != nil && supers[inst.ID] != oldSup && supers[inst.ID].runCount() > 0
	})

	// 用**同一个**（已被缓存的）处理器再请求：必须走新监督者，不能停在等待页。
	if code := getStatus(t, client, server.URL+"/"); code != http.StatusOK {
		t.Fatalf("重启后同一个上游处理器应能进入实例，实际 %d（多半是缓存里绑死了旧监督者）", code)
	}
}

// getStatus 走完整链路（含令牌交接的 302）发一次 GET，返回最终状态码。
func getStatus(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestStopInstanceUnknownIsNoop 停止一个从没启动过的实例不该报错：
// 管理台上「停止」按钮可能点在按需实例上，那个实例本来就没在跑。
func TestStopInstanceUnknownIsNoop(t *testing.T) {
	rt, _, _, _ := newRuntime(t, "portal.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.StopInstance(ctx, "neverstarted"); err != nil {
		t.Fatalf("停未运行的实例应当无害，实际报错 %v", err)
	}
}

// TestStoppedInstanceIsNotResurrectedByRequests 是「点了停止却停不下来」的回归测试。
//
// 上游处理器每来一个请求都要确认实例起没起（按需实例正是靠这个做到「点进去才
// 启动」）。这个确认不能把用户刚停掉的实例又拉起来：浏览器里那个标签页每隔几秒
// 就刷一次，早先每刷一次就等于替用户按了一遍启动——进程号一直在换、状态一直停在
// 「启动中」，用户看到的就是「根本停不掉」。
func TestStoppedInstanceIsNotResurrectedByRequests(t *testing.T) {
	rt, store, fake, supers := newRuntime(t, "portal.test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("启动运行时失败: %v", err)
	}

	inst := &portal.Instance{Name: "stubborn", DshHome: "/srv/dsh/stubborn", DshPort: fake.Port()}
	if err := store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	if err := rt.StartInstance(ctx, *inst); err != nil {
		t.Fatalf("启动实例失败: %v", err)
	}
	sup := supers[inst.ID]
	if sup == nil {
		t.Fatal("启动后应当有监督者")
	}
	waitFor(t, "监督者跑起来", func() bool { return sup.runCount() > 0 })

	// 只取一次上游处理器：管理端把它缓存进 mounts，之后的请求全复用这一份。
	handler, err := rt.Upstream(*inst)
	if err != nil {
		t.Fatalf("构造上游失败: %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("造 cookie jar 失败: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	if code := getStatus(t, client, server.URL+"/"); code != http.StatusOK {
		t.Fatalf("停止前应能进入实例，实际 %d", code)
	}

	if err := rt.StopInstance(ctx, inst.ID); err != nil {
		t.Fatalf("停止实例失败: %v", err)
	}
	if sup.stopCount() == 0 {
		t.Error("停止应当调用监督者的 Shutdown")
	}
	if sup.adoptedStopCount() == 0 {
		t.Error("停止还应当调用 StopAdopted：接管来的实例没有子进程句柄，只靠 Shutdown 谁也没停")
	}

	// 那个还开着的标签页又来请求了。
	runs := sup.runCount()
	body := getPage(t, client, server.URL+"/")
	if !strings.Contains(body, "实例已停止") {
		t.Errorf("停止之后再来请求应当看到「实例已停止」，实际页面：%s", firstLines(body))
	}
	if sup.runCount() != runs {
		t.Errorf("请求路径把用户刚停的实例又拉起来了（Run 从 %d 次变成 %d 次）", runs, sup.runCount())
	}
	if got := rt.Status(inst.ID); got.Running {
		t.Errorf("停止后不该是运行中，实际 %+v", got)
	}

	// 用户点「启动」还得能起来：这个标记只对「请求驱动的自动拉起」说不。
	if err := rt.StartInstance(ctx, *inst); err != nil {
		t.Fatalf("重新启动实例失败: %v", err)
	}
	waitFor(t, "点启动之后重新运行", func() bool { return rt.Status(inst.ID).Running })
	if code := getStatus(t, client, server.URL+"/"); code != http.StatusOK {
		t.Errorf("重新启动后应能再次进入实例，实际 %d", code)
	}
}

// getPage 走完整链路发一次 GET，返回最终响应正文。
//
// 状态码不够用：等待页、连不上页与「已停止」页都是 503，只有正文能分清是哪一张。
func getPage(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		t.Fatalf("读取 %s 的响应失败: %v", url, err)
	}
	return string(body)
}

// firstLines 取页面正文的前几行，用于失败时打印出人能看懂的线索。
func firstLines(body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) > 6 {
		lines = lines[:6]
	}
	return strings.Join(lines, " / ")
}

// TestStartTimeoutReachesLaunch 钉住「--dsh-start-timeout 一路传到监督者」这条链。
//
// 它断过：超时曾经在 defaultLaunch 里写死 90 秒，配置怎么填都不生效；而 90 秒
// 又短过「首次启动下载 dsh 与插件」，于是超时 → 杀掉正在下载的子进程 → 重试，
// 实例永远停在「DSH 正在启动」。
func TestStartTimeoutReachesLaunch(t *testing.T) {
	store := newStore(t)
	fake := dshtest.Start("127.0.0.1")
	t.Cleanup(fake.Close)

	var got time.Duration
	launched := false
	rt, err := New(Options{
		Store:        store,
		StartTimeout: 10 * time.Minute,
		Launch: func(inst portal.Instance, opts LaunchOptions) (Supervisor, error) {
			got = opts.StartTimeout
			launched = true
			return newFakeSupervisor(fake), nil
		},
	})
	if err != nil {
		t.Fatalf("构造运行时失败: %v", err)
	}
	if _, err := rt.ensureInstance(portal.Instance{ID: "i-timeout", DshPort: 3080}); err != nil {
		t.Fatalf("构造实例条目失败: %v", err)
	}
	if !launched {
		t.Fatal("Launch 没被调用")
	}
	if got != 10*time.Minute {
		t.Errorf("监督者拿到的启动超时 = %s，期望 10m", got)
	}
}

// TestStartTimeoutDefaultsToConfig 钉住「没配就用 config 的默认值」。
func TestStartTimeoutDefaultsToConfig(t *testing.T) {
	rt, err := New(Options{Store: newStore(t)})
	if err != nil {
		t.Fatalf("构造运行时失败: %v", err)
	}
	if rt.opts.StartTimeout != config.DefaultDSHStartTimeout {
		t.Errorf("启动超时默认 = %s，期望 config.DefaultDSHStartTimeout（%s）",
			rt.opts.StartTimeout, config.DefaultDSHStartTimeout)
	}
}
