package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/auth"
	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/fmtx"
	"github.com/wycto/dsh-gate/internal/proxy"
	"github.com/wycto/dsh-gate/internal/registry"
	"github.com/wycto/dsh-gate/internal/users"
	"github.com/wycto/dsh-gate/internal/version"
)

// Options 是构造网关实例所需的输入。
type Options struct {
	// Config 是该实例的配置。
	Config config.Instance
	// Logger 是带实例名前缀的 logger。
	Logger *slog.Logger
	// Subtitle 是登录页的副标题。
	Subtitle string
	// Registry 是运行登记目录：实例启动后在这里登记自己，停机时注销。
	// 为空则不登记（这样 status / stop 看不到它，但网关本身照常工作）。
	Registry registry.Dir
	// Argv 是启动网关的完整命令行，restart 靠它把实例原样拉起来。
	Argv []string
}

// Instance 是一个可运行的网关：监听端口、托管一个 dsh、做鉴权与转发。
type Instance struct {
	cfg        config.Instance
	log        *slog.Logger
	users      *users.Store
	supervisor *dshproc.Supervisor
	auth       *auth.Gate
	proxy      *proxy.Handler
	server     *http.Server
	listener   net.Listener

	// trustedHosts 是实际传给 dsh 的 --trusted-host 列表。
	trustedHosts []string
	// publicURL 是实际传给 dsh 的 --public-url。
	publicURL string

	cancel context.CancelFunc
	booted chan struct{}

	// shutdown 被 SetShutdown 注入，供 `dsh-gate stop` 通过控制端点触发。
	shutdown func()

	registry   registry.Dir
	argv       []string
	registered bool
}

// New 构造实例，但不监听任何端口。
func New(opts Options) (*Instance, error) {
	cfg := opts.Config
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	store, err := users.Open(cfg.UsersFile)
	if err != nil {
		return nil, err
	}

	i := &Instance{
		cfg:      cfg,
		log:      log,
		users:    store,
		booted:   make(chan struct{}),
		registry: opts.Registry,
		argv:     opts.Argv,
	}
	i.trustedHosts = buildTrustedHosts(cfg)
	i.publicURL = resolvePublicURL(cfg)

	upstream, err := url.Parse(cfg.UpstreamURL())
	if err != nil {
		return nil, fmt.Errorf("上游地址 %q 无法解析: %w", cfg.UpstreamURL(), err)
	}

	i.supervisor = dshproc.New(dshproc.Options{
		Command:         cfg.DSHCommand,
		ResolvedCommand: cfg.ResolvedDSHCommand,
		NPMRegistry:     cfg.NPMRegistry,
		Host:            cfg.DSHHost,
		Port:            cfg.DSHPort,
		Home:            cfg.DSHHome,
		Workspace:       cfg.Workspace,
		PublicURL:       i.publicURL,
		TrustedHosts:    i.trustedHosts,
		Token:           cfg.DSHToken,
		AutoStart:       cfg.AutoStart,
		Adopt:           cfg.Adopt,
		Restart:         cfg.Restart,
		StartTimeout:    cfg.DSHStartTimeout,
		StopTimeout:     cfg.DSHStopTimeout,
		Logger:          log,
	})

	i.proxy, err = proxy.New(proxy.Options{
		Upstream:     upstream,
		Tokens:       i.supervisor,
		Ready:        i.supervisor.Ready,
		Status:       i.supervisor.LastError,
		CookieSecure: cfg.TLSEnabled(),
		Logger:       log,
	})
	if err != nil {
		return nil, err
	}

	i.auth, err = auth.New(auth.Options{
		Shutdown:     func() { i.RequestShutdown() },
		Users:        store,
		SessionTTL:   cfg.SessionTTL,
		MaxFails:     cfg.MaxLoginFails,
		LockoutBase:  cfg.LockoutBase,
		CookieSecure: cfg.TLSEnabled(),
		Title:        "DSH Gateway",
		Subtitle:     opts.Subtitle,
		Version:      version.Version,
		Ready:        i.supervisor.Ready,
		Logger:       log,
	})
	if err != nil {
		return nil, err
	}

	i.server = &http.Server{
		Handler: i.auth.Handler(i.proxy),
		// 只给「读请求头」设超时，不给整体响应设超时：dsh 的回答可能包含
		// 长连接与长时间的流式输出，任何一处的总超时都会把正常会话拦腰砍断。
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return i, nil
}

// ListenAddr 返回实际监听地址。
//
// 与配置里的 ListenAddr 的区别是：端口写成 0 时（系统分配），这里返回的是内核
// 真正给的那个端口。启动横幅与测试都用它。
func (i *Instance) ListenAddr() string {
	if i.listener != nil {
		return i.listener.Addr().String()
	}
	return i.cfg.ListenAddr()
}

// SetShutdown 注入停机触发器（由 Runner 调用）。
//
// 用注入而不是直接持有 Runner，是为了让 Instance 不必知道外面还有谁在编排它。
func (i *Instance) SetShutdown(fn func()) { i.shutdown = fn }

// RequestShutdown 请求本实例停下，等价于在它自己的终端里按 Ctrl-C。
//
// 由停机端点调用；没注入触发器时是个空操作，单元测试与嵌入使用都安全。
func (i *Instance) RequestShutdown() {
	if i.shutdown != nil {
		i.shutdown()
		return
	}
	// 没有注入触发器（实例被单独使用，没有 Runner 在外面编排）时自己收尾，
	// 否则停机端点会变成一个只会说「好的」却不干活的空承诺。
	ctx, cancel := context.WithTimeout(context.Background(), i.cfg.DSHStopTimeout)
	defer cancel()
	if err := i.Shutdown(ctx); err != nil {
		i.log.Error("停机失败", "name", i.cfg.Name, "err", err)
	}
}

// Name 返回实例名。
func (i *Instance) Name() string { return i.cfg.Name }

// NativeURL 返回 dsh 的本机原生地址（含启动令牌），令牌未知时为空串。
//
// 令牌要等 dsh 把启动输出打出来才学得到，因此横幅打印的那一刻它多半还是
// 空的——前台运行时要的就是「就绪后补打一次」，见 run 命令的 awaitNativeURLs。
func (i *Instance) NativeURL() string { return i.supervisor.NativeURL() }

// Config 返回实例配置（只读用途）。
func (i *Instance) Config() config.Instance { return i.cfg }

// SetDSHCommand 把冒烟验证过的 dsh 启动方式交给这个实例的后台 supervisor。
//
// 首次启动的准备工作（下载 Node.js、安装 dsh）放在后台跑，备好之后从这里交
// 进来：supervisor 每次拉起 dsh 前都会重新解析命令，下一次重试就会用上。
// 显式 --dsh-cmd 不受影响——那是用户自己的决定。
//
// 只改 supervisor 里的那份：cfg 在构造之后被别的 goroutine 只读地取用，
// 这里再动它会引入数据竞争，而除了 supervisor 也没有别人需要它。
func (i *Instance) SetDSHCommand(res dshproc.Resolution) {
	i.supervisor.SetResolvedCommand(res)
}

// Start 打开监听端口，并异步开始拉起 dsh。
//
// dsh 不在 Start 里同步等待：Linux 服务器首次启动要下载插件，此时用户已经
// 能打开网关、看到「DSH 正在启动」的页面，而不是对着「连接被拒绝」发呆。
func (i *Instance) Start(ctx context.Context) error {
	if !i.auth.HasUsers() {
		return fmt.Errorf(`实例 %q 的账号表 %s 里还没有任何账号，网关拒绝启动。
请先创建账号：

    dsh-gate user add --users %s <用户名>

（创建完成后重新运行本命令）`,
			i.cfg.Name, i.cfg.UsersFile, i.cfg.UsersFile)
	}

	listener, err := net.Listen("tcp", i.cfg.ListenAddr())
	if err != nil {
		return listenError(err, i.cfg)
	}
	i.listener = listener

	i.register()

	runCtx, cancel := context.WithCancel(ctx)
	i.cancel = cancel
	go func() {
		defer close(i.booted)
		i.supervisor.Run(runCtx)
	}()
	return nil
}

// register 把自己登记到运行清单里。
//
// 登记在监听成功之后立刻写：清单的作用就是回答「现在谁在跑」，端口没起来就
// 登记，等于告诉别人一个还不存在的地址。
func (i *Instance) register() {
	if i.registry == "" {
		return
	}
	_, portText, err := net.SplitHostPort(i.ListenAddr())
	if err != nil {
		return
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return
	}

	executable, err := os.Executable()
	if err != nil {
		executable = ""
	} else if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}

	record := registry.Record{
		Name:      i.cfg.Name,
		PID:       os.Getpid(),
		Exe:       executable,
		Argv:      i.argv,
		Listen:    i.ListenAddr(),
		Port:      port,
		DSHPort:   i.cfg.DSHPort,
		DSHHome:   i.cfg.DSHHome,
		UsersFile: i.cfg.UsersFile,
		StateDir:  i.cfg.StateDir,
		PublicURL: i.publicURL,
		TLS:       i.cfg.TLSEnabled(),
		Managed:   i.supervisor.Managed(),
		Version:   version.Version,
		StartedAt: time.Now(),
	}
	if err := i.registry.Save(record); err != nil {
		i.log.Warn("写入运行登记失败（不影响网关本身，status / stop 将看不到它）", "err", err)
	}
	i.registered = true
}

// unregister 从运行清单里注销自己。
func (i *Instance) unregister() {
	if i.registry == "" || !i.registered {
		return
	}
	if err := i.registry.Remove(i.cfg.Name); err != nil {
		i.log.Warn("删除运行登记失败", "err", err)
	}
	i.registered = false
}

// Serve 阻塞提供 HTTP 服务，直到 Shutdown 被调用。
func (i *Instance) Serve() error {
	if i.listener == nil {
		return errors.New("实例尚未 Start")
	}
	var err error
	if i.cfg.TLSEnabled() {
		err = i.server.ServeTLS(i.listener, i.cfg.TLSCert, i.cfg.TLSKey)
	} else {
		err = i.server.Serve(i.listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 停止网关与被托管的 dsh。
//
// 顺序是「先断新请求，再收尾 dsh」：先 Shutdown HTTP 服务（它会等已有请求
// 结束），确保没有人正在通过网关访问 dsh 时就把 dsh 收走。
func (i *Instance) Shutdown(ctx context.Context) error {
	var err error
	i.unregister()
	if i.server != nil {
		err = i.server.Shutdown(ctx)
	}
	if i.cancel != nil {
		i.cancel()
	}
	if dshErr := i.supervisor.Shutdown(ctx); err == nil {
		err = dshErr
	}
	return err
}

// WaitBooted 等待 dsh 的首次启动流程结束（成功或放弃）。
func (i *Instance) WaitBooted(ctx context.Context) {
	select {
	case <-i.booted:
	case <-ctx.Done():
	}
}

// Addrs 返回可访问地址列表。
//
// 顺序即推荐程度：本机回环在前（自己用最稳），局域网 IP 在后，最后才是
// 主机名——mDNS 在某些网络里根本解析不了。
func (i *Instance) Addrs() []string {
	scheme := i.cfg.Scheme()
	// 以实际监听的端口为准：配置里写 0 时，用户看到的是内核分配的端口。
	_, portText, err := net.SplitHostPort(i.ListenAddr())
	port, convErr := strconv.Atoi(portText)
	if err != nil || convErr != nil {
		return []string{scheme + "://" + i.ListenAddr() + "/"}
	}

	addrs := []string{scheme + "://127.0.0.1:" + strconv.Itoa(port) + "/"}
	// 只展示 3 个局域网地址：多网卡主机可能有十来个，全列出来没人看得下去，
	// 而真要用的话 check 里都有完整的列表。
	for _, address := range DisplayAddresses(3) {
		addrs = append(addrs, scheme+"://"+authority(address, port)+"/")
	}
	if name := Hostname(); name != "" && !strings.Contains(name, ".") {
		addrs = append(addrs, scheme+"://"+authority(name, port)+"/")
	}
	return addrs
}

// Status 汇总实例状态，供 `dsh-gate status` 展示。
type Status struct {
	Name      string
	Listen    string
	DSHAddr   string
	PublicURL string
	Users     int
	Ready     bool
	Managed   bool
	PID       int
	LastError string
	// NeedsToken 表示这是个被接管的 dsh，而网关还不知道它的启动令牌。
	//
	// 此时浏览器能打开首页，但每个 API 调用都会 401；唯一的解法是让浏览器把
	// 带令牌的地址访问一次。横幅里必须把这件事说清楚，否则用户只会看到
	// 「页面能开但用不了」，完全无从下手。
	NeedsToken bool
}

// Status 返回当前状态快照。
func (i *Instance) Status() Status {
	return Status{
		Name:      i.cfg.Name,
		Listen:    i.cfg.ListenAddr(),
		DSHAddr:   i.cfg.DSHAddr(),
		PublicURL: i.publicURL,
		Users:     i.users.Count(),
		Ready:     i.supervisor.Ready(),
		Managed:   i.supervisor.Managed(),
		PID:       i.supervisor.PID(),
		LastError: i.supervisor.LastError(),

		NeedsToken: i.supervisor.Adopted() && i.supervisor.Token() == "",
	}
}

// Banner 生成启动横幅。
//
// 它的存在本身就是一个功能需求：「我在任何一台电脑上该打开哪个地址？」
// 答案不该让人去查 ifconfig 或问自己刚才输的是什么端口。
func (i *Instance) Banner() string {
	status := i.Status()
	var b strings.Builder

	label := func(text string) string { return "  " + fmtx.Pad(text, 10) }

	b.WriteString(fmt.Sprintf("实例 %s 已启动\n", status.Name))
	b.WriteString(fmt.Sprintf("  访问地址（网关端口 %s）\n", i.ListenAddr()))
	for index, addr := range i.Addrs() {
		name := "局域网"
		if index == 0 {
			name = "本机"
		} else if index == len(i.Addrs())-1 {
			name = "主机名"
		}
		b.WriteString("  " + fmtx.Pad(name, 10) + addr + "\n")
	}
	b.WriteString(label("dsh") + status.DSHAddr)
	switch {
	case status.Ready && status.Managed:
		b.WriteString(fmt.Sprintf("（已由网关拉起，PID %d）\n", status.PID))
	case status.Ready:
		b.WriteString("（已接管正在运行的实例）\n")
	default:
		b.WriteString("（启动中，稍后会自动就绪并打印本机直连地址）\n")
	}
	// dsh 的本机原生地址（含启动令牌）。只在网关自己启动 dsh、因而读得到
	// 启动输出时才有；接管外部实例且令牌未知时不显示，免得给出半截地址。
	if native := i.supervisor.NativeURL(); native != "" {
		b.WriteString(label("本机直连") + native + "（绕过网关，仅本机可用）\n")
	}
	b.WriteString(label("dsh 目录") + i.cfg.DSHHome + "\n")
	// 工作目录是「agent 默认能读写哪里的文件」，配错了要等到用的时候才发现，
	// 所以每次启动都把它打在横幅上——systemd 场景下这是唯一能一眼看出
	// 「单元里没带 --workspace」的地方。
	workspace, explicit := effectiveWorkspace(i.cfg)
	b.WriteString(label("工作目录") + workspace + "\n")
	if !explicit {
		b.WriteString(fmt.Sprintf("%s%s\n", fmtx.Pad("", 10),
			"未显式指定：跟随网关进程的工作目录（由服务托管时是单元的 WorkingDirectory，通常是用户家目录）"))
	}
	b.WriteString(label("网关目录") + i.cfg.StateDir + "\n")
	b.WriteString(label("账号") + fmt.Sprintf("%d 个（%s）\n", status.Users, i.cfg.UsersFile))

	if status.NeedsToken {
		pad := fmtx.Pad("", 10)
		b.WriteString(label("注意") + "这是接管的 dsh（不是网关拉起的），网关拿不到它的启动令牌。\n")
		b.WriteString(fmt.Sprintf("%s令牌每个进程随机生成、只在 dsh 启动时打印一次、不落盘，\n", pad))
		b.WriteString(fmt.Sprintf("%s所以别人拉起的 dsh，网关无从得知。现在首页能开，但对话会报未授权。\n", pad))
		b.WriteString(fmt.Sprintf("%s两条出路（② 一定成功）：\n", pad))
		b.WriteString(fmt.Sprintf("%s① 还留着 dsh 启动时打印的地址：把 http://127.0.0.1:%d/?token=…\n",
			pad, i.cfg.DSHPort))
		b.WriteString(fmt.Sprintf("%s   粘到上面任一访问地址后面打开一次（也可用 --dsh-token 指定）。\n", pad))
		b.WriteString(fmt.Sprintf("%s② 让网关自己管 dsh：先停掉外面那个，再重启本实例——\n", pad))
		b.WriteString(fmt.Sprintf("%s   pkill -f \"dsh web\"   # 或 ps 找到 PID 后 kill\n", pad))
		b.WriteString(fmt.Sprintf("%s   dsh-gate restart %s\n", pad, i.cfg.Name))
	}
	if !i.cfg.TLSEnabled() {
		b.WriteString(label("提示") + "当前是明文 HTTP。跨公网访问请配 --tls-cert/--tls-key，\n")
		b.WriteString(fmt.Sprintf("%10s或在内网穿透工具后面使用（流量由隧道加密）。\n", ""))
	}
	if i.publicURL == "" {
		b.WriteString(label("提示") + "本机没有非回环地址，dsh 的公告地址留空；\n")
		b.WriteString(fmt.Sprintf("%10s如需从其它设备访问，请显式指定 --public-url。\n", ""))
	}
	return b.String()
}

// buildTrustedHosts 组装传给 dsh 的 --trusted-host 列表。
//
// dsh 只接受回环地址、自身绑定地址，以及被显式点名的 authority。反向代理
// 的对外地址（192.168.x.x:8080、内网穿透域名、mDNS 主机名）都必须在这里
// 点名，否则浏览器每个 API 调用都会收到 403。
//
// 实现搬到 TrustedHosts 里，因为管理端形态也要用同一份推导：那份代码当初
// 漏了这一步，于是单实例下一切正常、管理端下每个实例都「一直连接中」。
func buildTrustedHosts(cfg config.Instance) []string {
	return TrustedHosts(cfg.TrustedHosts, cfg.PublicURL)
}

// authorityHost 把地址写成 dsh 认得的 authority 形式。
//
// IPv6 必须带方括号：dsh 的 trustedHosts 解析器会把裸的 "2408:...::1" 判成
// 非法条目并**让整个 dsh 起不来**（required plugin connection 激活失败）。
// 这个坑很隐蔽——错误信息里只有一行「not a bare host[:port] authority」，
// 而用户很可能压根没想到是自己机器上的某个 IPv6 地址惹的祸。
func authorityHost(address string) string {
	if strings.Contains(address, ":") {
		return "[" + address + "]"
	}
	return address
}

// resolvePublicURL 计算传给 dsh 的 --public-url。
//
// 它决定 dsh 打印什么地址、agent 通过 DSH_WEB_URL 看到什么地址。用户没显式
// 指定时，用「本机第一个局域网 IP + 网关端口」推导：这是多数人真正会分发给
// 同事、贴进群里的那个地址。推导不出来就留空——dsh 会退回打印绑定地址，
// 而不是编一个连不上的地址出来。
func resolvePublicURL(cfg config.Instance) string {
	if cfg.PublicURL != "" {
		return cfg.PublicURL
	}
	address := PrimaryAddress()
	if address == "" {
		return ""
	}
	return cfg.Scheme() + "://" + authority(address, cfg.Port) + "/"
}

// listenError 把 net.Listen 的错误翻译成能照着改的提示。
func listenError(err error, cfg config.Instance) error {
	var addrErr *net.AddrError
	if errors.As(err, &addrErr) {
		switch addrErr.Err {
		case "address already in use":
			return fmt.Errorf(`网关端口 %d 已被占用。
换一个端口：加 --port <其它端口>
或者先停掉占用它的程序。`, cfg.Port)
		case "permission denied":
			return fmt.Errorf(`没有权限监听端口 %d。
1024 以下的端口需要管理员权限，请换一个 1024 以上的端口。`, cfg.Port)
		case "cannot assign requested address":
			return fmt.Errorf(`无法监听 %s：这个地址不在本机上。
用 --host 0.0.0.0 监听所有网卡，或指定一个本机真实存在的地址。`, cfg.ListenAddr())
		}
	}
	return fmt.Errorf("监听 %s 失败: %w", cfg.ListenAddr(), err)
}

// effectiveWorkspace 返回 dsh 子进程实际会用的工作目录，以及它是否被显式指定。
//
// 「留空 = 继承网关进程的工作目录」这条语义在 systemd 下很容易被忽略：网关由
// 服务管理器托管时，它的 WorkingDirectory 是安装时的默认（用户家目录），于是
// dsh 的工作目录就变成了 /root 之类的地方，而用户以为是自己 setup 时填的那个。
func effectiveWorkspace(cfg config.Instance) (string, bool) {
	if dir := strings.TrimSpace(cfg.Workspace); dir != "" {
		return dir, true
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		return cwd + "（跟随网关进程）", false
	}
	return "跟随网关进程的工作目录", false
}
