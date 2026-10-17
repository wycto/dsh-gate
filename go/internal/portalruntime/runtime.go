// Package portalruntime 把管理端里的「实例登记」接到真实的 dsh 进程上。
//
// 它只做三件事，其余全部复用既有实现：
//
//  1. 为某个实例构造 dsh 进程监督者（`dshproc.Supervisor`）——拉起、接管、崩溃重启；
//  2. 为某个实例构造反向代理（`proxy.Handler`）——**令牌交接**、401 自愈、等待页
//     全是它现成的能力，本包不再重写一遍；
//  3. 管住这些东西的生命周期：管理端启动时拉起该拉起的实例，退出时收走。
//
// 实现刻意用接口（Supervisor + LaunchFunc）隔开：单元测试可以注入一个假 dsh
// （`internal/dshtest`）走完整链路——登录 → 进实例 → 令牌交接 → 代理 —— 而不必
// 真的在本机拉起一个 dsh 进程。
package portalruntime

import (
	"context"
	"errors"
	"fmt"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/proxy"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Supervisor 是网关对一个 dsh 进程的全部需求。`dshproc.Supervisor` 实现它；
// 测试用假实现替换。
type Supervisor interface {
	proxy.TokenSource // Token() / SetToken()
	Ready() bool
	LastError() string
	Run(ctx context.Context)
	Shutdown(ctx context.Context) error
	PID() int
	Managed() bool
	NativeURL() string
}

// LaunchOptions 是拉起一个实例时用到的部署事实（实例自己的字段在 portal.Instance 里）。
type LaunchOptions struct {
	// PublicURL 是管理端对外公告的地址；dsh 用它生成启动 URL 与 agent 看到的地址。
	PublicURL string
	// TrustedHosts 是浏览器可见的 authority，必须包含管理端域名，否则每个 API 403。
	TrustedHosts []string
	// Logger 收日志；为 nil 时丢弃。
	Logger *slog.Logger
}

// LaunchFunc 拉起一个实例的 dsh 监督者。
type LaunchFunc func(inst portal.Instance, opts LaunchOptions) (Supervisor, error)

// Options 是运行时的配置。
type Options struct {
	// Store 是实例登记来源。
	Store *portal.Store
	// TLS 表示管理端这一侧是 HTTPS（决定上游 Cookie 是否补 Secure）。
	TLS bool
	// PublicURL / TrustedHosts 原样传给每个实例的 dsh。
	PublicURL    string
	TrustedHosts []string
	// Launch 为 nil 时使用 dshproc 的默认实现。
	Launch LaunchFunc
	// Logger 收日志；为 nil 时丢弃。
	Logger *slog.Logger
}

// Status 是一个实例的运行状态（页面与 CLI 展示用）。
type Status struct {
	Running bool
	Ready   bool
	PID     int
	Managed bool
	Error   string
}

// Runtime 管理管理端下所有实例的运行状态。
type Runtime struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	byID     map[string]*instance
	baseCtx  context.Context
	baseStop context.CancelFunc
	started  bool

	// installs 是安装任务表（见 install.go）。
	installs *installJobs
	// toolJobs 是运行环境补齐任务表（见 toolinstall.go）。
	toolJobs *toolJobs
}

type instance struct {
	inst    portal.Instance
	sup     Supervisor
	handler *proxy.Handler
	cancel  context.CancelFunc
}

// New 构造运行时。
func New(opts Options) (*Runtime, error) {
	if opts.Store == nil {
		return nil, errors.New("管理端运行时需要持久层")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.Launch == nil {
		opts.Launch = defaultLaunch
	}
	return &Runtime{
		opts:     opts,
		log:      logger,
		byID:     map[string]*instance{},
		installs: newInstallJobs(),
		toolJobs: newToolJobs(),
	}, nil
}

// defaultLaunch 用 dshproc 的默认实现拉起一个实例。
func defaultLaunch(inst portal.Instance, opts LaunchOptions) (Supervisor, error) {
	return dshproc.New(dshproc.Options{
		// 留空 = 让 supervisor 自动探测启动命令（PATH 里的 dsh，或 npx）。
		ResolvedCommand: inst.DshCmd,
		// 监听地址默认回环（对外只有管理端一个口）；实例记录里显式指定时用它。
		// 把实例直接暴露到局域网是管理端用户自己的选择，不该被这里的默认值否掉。
		Host:         listenHostOf(inst),
		Port:         inst.DshPort,
		Home:         homeOf(inst),
		Workspace:    inst.Workspace,
		PublicURL:    opts.PublicURL,
		TrustedHosts: opts.TrustedHosts,
		AutoStart:    true,
		Adopt:        true,
		Restart:      true,
		StartTimeout: 90 * time.Second,
		StopTimeout:  10 * time.Second,
		Logger:       opts.Logger,
	}), nil
}

// homeOf 返回实例该用的 DSH_HOME。
//
// dsh 把会话、插件、profile（$DSH_HOME/profiles）都放在 DSH_HOME 下，没有独立的
// 「配置目录」环境变量——所以实例记录里的 ConfigDir 就是 DSH_HOME 的显式写法，
// 而不是另造一个 dsh 不认的变量。ConfigDir 留空时用 DshHome（老记录的行为）。
func homeOf(inst portal.Instance) string {
	if dir := strings.TrimSpace(inst.ConfigDir); dir != "" {
		return dir
	}
	return inst.DshHome
}

// listenHostOf 返回实例该用的监听地址。
//
// 空值（老记录）按回环处理——这是升级前的行为，必须保持一致。
func listenHostOf(inst portal.Instance) string {
	if host := strings.TrimSpace(inst.ListenHost); host != "" {
		return host
	}
	return "127.0.0.1"
}

// Start 拉起所有「常驻」实例（enabled 且非 OnDemand）。
//
// 单个实例起不来不会让整个管理端起不来：日志里留下原因，用户点进去时看到的是
// 「实例暂不可用」而不是管理端打不开。
func (rt *Runtime) Start(ctx context.Context) error {
	rt.mu.Lock()
	if rt.started {
		rt.mu.Unlock()
		return nil
	}
	rt.started = true
	rt.baseCtx, rt.baseStop = context.WithCancel(ctx)
	rt.mu.Unlock()

	instances, err := rt.opts.Store.ListInstances()
	if err != nil {
		return fmt.Errorf("读取实例列表失败: %w", err)
	}
	for _, inst := range instances {
		if !inst.Enabled || inst.OnDemand {
			continue
		}
		entry, err := rt.ensureInstance(inst)
		if err != nil {
			rt.log.Error("拉起实例失败", "instance", inst.Name, "err", err)
			continue
		}
		rt.mu.Lock()
		rt.startLocked(entry)
		rt.mu.Unlock()
	}
	return nil
}

// Stop 收走所有实例：先停监督者，再取消它们的上下文。
func (rt *Runtime) Stop(ctx context.Context) error {
	rt.mu.Lock()
	entries := make([]*instance, 0, len(rt.byID))
	for _, entry := range rt.byID {
		entries = append(entries, entry)
	}
	baseStop := rt.baseStop
	rt.mu.Unlock()

	var firstErr error
	for _, entry := range entries {
		if entry.cancel == nil {
			continue
		}
		if err := entry.sup.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("停止实例 %s 失败: %w", entry.inst.Name, err)
		}
	}
	if baseStop != nil {
		baseStop()
	}
	return firstErr
}

// Upstream 返回某实例的反向代理处理器，供 portal.Server 当作上游使用。
//
// 第一次调用会把这个实例的监督者与代理建好，并**幂等地**把监督者跑起来：
// 常驻实例通常在管理端启动时已经被 Start 拉起（这里什么也不做），而按需实例
// 正是在「用户点进来」的这一刻才开始启动。
func (rt *Runtime) Upstream(inst portal.Instance) (http.Handler, error) {
	entry, err := rt.ensureInstance(inst)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	rt.startLocked(entry)
	rt.mu.Unlock()
	return entry.handler, nil
}

// ensureInstance 返回实例对应的运行时条目，必要时构造它。
func (rt *Runtime) ensureInstance(inst portal.Instance) (*instance, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if existing := rt.byID[inst.ID]; existing != nil {
		return existing, nil
	}

	sup, err := rt.opts.Launch(inst, LaunchOptions{
		PublicURL:    rt.opts.PublicURL,
		TrustedHosts: rt.opts.TrustedHosts,
		Logger:       rt.log,
	})
	if err != nil {
		return nil, fmt.Errorf("实例 %s 的 dsh 监督者创建失败: %w", inst.ID, err)
	}
	handler, err := proxy.New(proxy.Options{
		// 上游地址只来自实例登记（本地数据），不接受请求里的任何输入。
		Upstream: &url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(inst.DshPort)),
		},
		Tokens:       sup,
		Ready:        sup.Ready,
		Status:       sup.LastError,
		CookieSecure: rt.opts.TLS,
		Logger:       rt.log,
	})
	if err != nil {
		return nil, err
	}

	entry := &instance{inst: inst, sup: sup, handler: handler}
	rt.byID[inst.ID] = entry
	return entry, nil
}

// startLocked 起监督者（幂等）。调用方必须持有 rt.mu。
func (rt *Runtime) startLocked(entry *instance) {
	if entry.cancel != nil {
		return
	}
	base := rt.baseCtx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	entry.cancel = cancel
	go entry.sup.Run(ctx)
}

// Status 返回某实例的运行状态。没接管过的实例返回零值（未运行）。
func (rt *Runtime) Status(instanceID string) Status {
	rt.mu.Lock()
	entry := rt.byID[instanceID]
	rt.mu.Unlock()
	if entry == nil {
		return Status{}
	}
	return Status{
		Running: entry.cancel != nil,
		Ready:   entry.sup.Ready(),
		PID:     entry.sup.PID(),
		Managed: entry.sup.Managed(),
		Error:   entry.sup.LastError(),
	}
}
