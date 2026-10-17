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

	"github.com/wycto/dsh-gate/internal/dshtest"
	"github.com/wycto/dsh-gate/internal/portal"
)

// fakeSupervisor 是一个不拉进程的假 dsh 监督者：令牌来自假 dsh，Ready 恒真。
type fakeSupervisor struct {
	fake *dshtest.Server

	mu      sync.Mutex
	token   string
	ran     int
	stopped int
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

func (f *fakeSupervisor) Ready() bool       { return true }
func (f *fakeSupervisor) LastError() string { return "" }
func (f *fakeSupervisor) PID() int          { return 4242 }
func (f *fakeSupervisor) Managed() bool     { return true }
func (f *fakeSupervisor) NativeURL() string { return "" }

func (f *fakeSupervisor) Run(ctx context.Context) {
	f.mu.Lock()
	f.ran++
	f.mu.Unlock()
	<-ctx.Done()
}

func (f *fakeSupervisor) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	f.stopped++
	f.mu.Unlock()
	return nil
}

func (f *fakeSupervisor) runCount() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.ran }
func (f *fakeSupervisor) stopCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.stopped }

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
