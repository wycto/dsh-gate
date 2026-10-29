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
	"path/filepath"
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
	// StopAdopted 结束端口上那个「不是网关拉起」的 dsh。
	//
	// 只有用户显式停止实例时才调用它：管理端自己退出时不该顺手杀掉别人起的
	// 进程。不是接管来的（网关自己有子进程句柄）时它是空操作。
	StopAdopted(ctx context.Context) error
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

	// stoppedByUser 表示「这次不在跑是用户停的」，不是「还没起来」。
	//
	// 代理路径每来一个请求都会确认实例起没起（按需实例就是靠这个做到点进去才
	// 启动的），而这个确认不能把用户刚停掉的实例又拉起来——那是「点了停止、
	// 过几秒进程号却变了」的直接原因：浏览器里那个标签页每 10 秒刷一次，
	// 每次刷都替用户把 dsh 重新按了一遍启动键。
	stoppedByUser bool
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
		// 没装过（启动命令为空）时，实例上钉的版本也要照进探测出来的那条 npx
		// 命令里——否则填了版本却不点安装，起来的是 latest，与页面上写的相反。
		DshVersion: inst.DshVersion,
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
		TokenFile:    tokenFileOf(inst),
		Logger:       opts.Logger,
	}), nil
}

// tokenFileOf 返回实例的启动令牌文件位置：放在实例自己的 DSH_HOME 里。
//
// 放实例目录而不是网关状态目录：令牌属于那一个 dsh，实例换目录或删掉时它跟着
// 一起走，不会在网关状态目录里堆一堆过期令牌。管理端重启后读回它，接管的 dsh
// 就能把浏览器的会话续上，而不是变成「一对话就要求重新登录」。
func tokenFileOf(inst portal.Instance) string {
	home := strings.TrimSpace(homeOf(inst))
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".dsh-gate-token")
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
//
// 返回的是**间接处理器**，而不是构造时的那个 proxy.Handler 本身。原因：
// portal.Server 会把这里返回的处理器连同挂载前缀一起缓存起来（见
// portal.Server.mountFor），一旦把实体处理器交出去，缓存里那一份就永远绑死在
// 「第一次进入时」的那个监督者上。而重启/停止后再启动会换掉监督者
// （RestartInstance 甚至会丢掉整个条目），缓存里的旧处理器却还指着已经
// Shutdown 的那个——它的 Ready() 恒为 false，于是「进入实例」永远停在
// 「DSH 正在启动」的等待页上。每次请求现查当前条目，就没有这种陈旧引用。
func (rt *Runtime) Upstream(inst portal.Instance) (http.Handler, error) {
	entry, err := rt.ensureInstance(inst)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	rt.startOnRequestLocked(entry)
	rt.mu.Unlock()

	id := inst.ID
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, err := rt.currentHandler(id)
		if err != nil {
			rt.log.Error("实例的上游处理器不可用", "instance", id, "err", err)
			http.Error(w, "实例暂不可用，请回管理台确认它的状态", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}), nil
}

// currentHandler 返回实例**当前**的上游处理器，必要时按库里最新的记录重建条目。
//
// 条目可能因为重启被丢掉（RestartInstance 会 delete），也可能因为改配置被换新。
// 这里每次都按 id 现查：查得到就用它的处理器，查不到就用库里的最新记录重建，
// 保证「重启之后点进入」看到的一定是新监督者。
func (rt *Runtime) currentHandler(instanceID string) (http.Handler, error) {
	rt.mu.Lock()
	entry := rt.byID[instanceID]
	rt.mu.Unlock()

	if entry == nil {
		inst, err := rt.opts.Store.InstanceByID(instanceID)
		if err != nil {
			return nil, fmt.Errorf("实例 %s 已不存在: %w", instanceID, err)
		}
		if entry, err = rt.ensureInstance(*inst); err != nil {
			return nil, err
		}
	}

	rt.mu.Lock()
	started := rt.startOnRequestLocked(entry)
	name := entry.inst.Name
	handler := entry.handler
	rt.mu.Unlock()
	if !started {
		// 用户停过的实例不靠请求复活：给他一张写清楚「已停止、请回管理台点启动」
		// 的页面，而不是偷偷把进程再拉起来一个。
		return proxy.StoppedPage(name), nil
	}
	return handler, nil
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
	// 走到这里就是「有人要它起来」：开机自启、用户点启动、用户点重启，
	// 每一种都不再欠「已停止」这个状态。
	entry.stoppedByUser = false
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

// startOnRequestLocked 处理「浏览器请求驱动的拉起」。调用方必须持有 rt.mu。
//
// 与 startLocked 只差一件事：用户显式停过的实例不在这里复活。按需实例第一次
// 点进去时正是靠这条路径启动的，所以不能整体禁掉，只能针对「刚刚被停过」放行
// 一个否——否则任何一个还开着的标签页都能替用户把实例重新点燃。
//
// 返回 false 表示这次不该拉起，调用方要给出「实例已停止」的页面。
func (rt *Runtime) startOnRequestLocked(entry *instance) bool {
	if entry.stoppedByUser {
		return false
	}
	rt.startLocked(entry)
	return true
}

// StartInstance 启动单个实例（幂等：已在跑就什么都不做）。
//
// 与 Start 的区别：Start 是「管理端启动时把常驻实例都拉起来」，这个是用户
// 在管理台上点「启动」；两者共用同一条 startLocked 路径，不会出现「点启动
// 和开机自启行为不一致」。
//
// inst 必须是**从库里刚读出来**的实例：拉起时用的是这个值里的端口、目录与
// 启动命令，传旧值是「改了配置却按老配置启动」的经典来源。
func (rt *Runtime) StartInstance(ctx context.Context, inst portal.Instance) error {
	entry, err := rt.ensureInstance(inst)
	if err != nil {
		return err
	}
	rt.mu.Lock()
	rt.startLocked(entry)
	rt.mu.Unlock()
	return nil
}

// StopInstance 停止单个实例：先让监督者优雅收尾，再取消它的上下文。
//
// 与 Stop 的区别同上：Stop 收走所有实例（管理端退出），这个只停一个。
//
// 停完把条目标成「已停止」但**不删除**：用户可能马上要点重启，重新构造
// 监督者要重新探测 dsh 命令与端口，没必要。条目里保存的 inst 会在
// RestartInstance 里换成最新值。
func (rt *Runtime) StopInstance(ctx context.Context, instanceID string) error {
	rt.mu.Lock()
	entry := rt.byID[instanceID]
	rt.mu.Unlock()
	if entry == nil {
		return nil
	}
	return rt.stopEntry(ctx, entry)
}

// stopEntry 停掉一个运行时条目。可重复调用（没在跑也要把「用户停的」记下来）。
//
// 两步停止缺一不可：
//   - Shutdown 收走网关自己拉起的那个 dsh；
//   - StopAdopted 收走「端口上那个不是网关启动的」dsh——接管实例没有子进程
//     句柄，只Shutdown 等于谁也没停，管理台却会显示「已停止」。
//
// 先 Shutdown 再 StopAdopted：前者会把监督者标成「已停止」，崩溃重启循环看到
// 这个标记就不再重拉，后一步杀掉进程时才不会被它自己复活。
func (rt *Runtime) stopEntry(ctx context.Context, entry *instance) error {
	rt.mu.Lock()
	cancel := entry.cancel
	entry.cancel = nil
	// 无论此前在不在跑都记下这一刻：条目不删，而代理路径要正是靠这个标记
	// 决定「不再替它复活」。
	entry.stoppedByUser = true
	rt.mu.Unlock()

	if cancel == nil {
		return nil
	}
	err := entry.sup.Shutdown(ctx)
	adoptErr := entry.sup.StopAdopted(ctx)
	cancel()
	if err == nil {
		err = adoptErr
	}
	if err != nil {
		return fmt.Errorf("停止实例 %s 失败: %w", entry.inst.Name, err)
	}
	return nil
}

// RestartInstance 用**最新配置**重启单个实例。
//
// 这是「停止 → 改配置 → 重启」链路的最后一环，所以不能简单地 Stop 再 Start：
// 监督者里写死了拉起时的端口、DSH_HOME、工作目录与启动命令，拿旧监督者再跑
// 一次等于按老配置启动，用户改的东西一个都不会生效。
//
// 因此这里的做法是：停掉旧监督者 → 丢掉运行时条目 → 用传进来的 inst 重新
// 构造监督者与代理 → 启动。代理也要重建，因为上游地址取自实例端口。
//
// 调用方负责传「刚改完并落库」的那份实例。
func (rt *Runtime) RestartInstance(ctx context.Context, inst portal.Instance) error {
	rt.mu.Lock()
	entry := rt.byID[inst.ID]
	rt.mu.Unlock()

	if entry != nil {
		if err := rt.stopEntry(ctx, entry); err != nil {
			return err
		}
		// 丢掉旧条目：代理的上游地址与监督者的配置都是构造时固定的。
		rt.mu.Lock()
		delete(rt.byID, inst.ID)
		rt.mu.Unlock()
	}

	rt.mu.Lock()
	running := rt.started
	rt.mu.Unlock()
	// 管理端还没开始运行时（rt.started 为 false），只确保条目按新配置重建，
	// 不主动拉起——等 Start 时统一按新配置起。这样「管理端启动前的重启」
	// 不会变成「提前跑起来一个实例」。
	if !running {
		_, err := rt.ensureInstance(inst)
		return err
	}
	return rt.StartInstance(ctx, inst)
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
