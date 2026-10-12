package dshproc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options 是拉起 dsh 所需的全部信息。
type Options struct {
	// Command 是启动命令模板，支持占位符；留空表示自动探测。
	Command string
	// ResolvedCommand 是 EnsureDSH 冒烟验证过的启动命令（自动探测的优先级
	// 高于它被使用前的探测，但低于显式的 Command）。为空表示自动探测。
	ResolvedCommand string
	// NPMRegistry 是用户点名的 npm 源（--npm-registry）；非空时以
	// npm_config_registry 注入 dsh 子进程，dsh 自己拉插件也走同一个源。
	NPMRegistry string
	// Host / Port 是 dsh 的监听地址。
	Host string
	Port int
	// Home 是实例目录，会以 DSH_HOME 的形式注入子进程环境。
	Home string
	// Workspace 是子进程工作目录（dsh 把它当作默认 workspace 根）。
	Workspace string
	// PublicURL 非空时作为 --public-url 传给 dsh。
	PublicURL string
	// TrustedHosts 逐个作为 --trusted-host 传给 dsh。
	TrustedHosts []string
	// Token 是已知的启动令牌；留空则从 dsh 的启动输出里读。
	Token string
	// Adopt 表示端口已被占用时接管那个 dsh，而不是报错。
	Adopt bool
	// AutoStart 表示端口空闲时由本进程把 dsh 拉起来。
	AutoStart bool
	// StartTimeout 是等待 dsh 端口就绪的最长时间。
	StartTimeout time.Duration
	// StopTimeout 是停止 dsh 时等待优雅退出的时间。
	StopTimeout time.Duration
	// Restart 表示 dsh 意外退出后是否自动拉起。
	Restart bool
	// Logger 收日志；为 nil 时用丢弃全部记录的默认 logger。
	Logger *slog.Logger
}

// Supervisor 托管一个 dsh 实例。
//
// 生命周期只有四种状态：没起来（未 Start）、运行中（端口有人听）、崩溃
// 重启中、已停止。所有对外的查询方法都可以在任意时刻安全调用。
type Supervisor struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex
	cmd       *exec.Cmd
	exited    chan struct{} // 当前子进程退出时关闭
	adopted   bool          // 端口上的 dsh 不是我们启动的
	stopped   bool          // 收到过停止指令，不再自动重启
	reaping   bool          // 崩溃重启循环正在运行：此刻只有它有权拉起子进程
	ready     bool
	pid       int
	token     string
	lastErr   string
	supported map[string]bool // dsh 支持的选项，探测一次后缓存
	readyCh   chan struct{}
	recent    []string // dsh 最近若干行输出，失败时原样回放

	// commandMu 保护 opts.ResolvedCommand 与 resolvedRegistry：它们可能在启动
	// 之后才由后台的「准备 dsh」写进来（见 SetResolvedCommand），而每次拉起
	// dsh 都要读它们。
	commandMu        sync.RWMutex
	resolvedRegistry string
}

// New 构造 Supervisor。此时并不启动任何进程。
func New(opts Options) *Supervisor {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 90 * time.Second
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = 10 * time.Second
	}
	return &Supervisor{
		opts:    opts,
		log:     opts.Logger,
		token:   opts.Token,
		readyCh: make(chan struct{}),
	}
}

// Addr 返回 dsh 的监听地址。
func (s *Supervisor) Addr() string {
	return fmt.Sprintf("%s:%d", s.opts.Host, s.opts.Port)
}

// SetResolvedCommand 把冒烟验证过的 dsh 启动方式交给运行中的 supervisor。
//
// 给「首次启动的准备工作放到后台」用：网关先开始服务（登录页立刻能打开），
// Node 与 dsh 在后台备好之后再把命令交进来；每次拉起 dsh 前都会重新解析命令
// （见 buildCommand），所以下一次重试就会用上它，不必重启网关。
//
// 显式 --dsh-cmd 不受影响：那种情况下 buildCommand 走的是用户给的模板。
func (s *Supervisor) SetResolvedCommand(res Resolution) {
	if strings.TrimSpace(res.Command) == "" {
		return
	}
	s.commandMu.Lock()
	s.opts.ResolvedCommand = res.Command
	s.resolvedRegistry = res.Registry
	s.commandMu.Unlock()
}

// resolvedCommand 读取当前生效的已解析命令。
func (s *Supervisor) resolvedCommand() string {
	s.commandMu.RLock()
	defer s.commandMu.RUnlock()
	return s.opts.ResolvedCommand
}

// registry 返回 dsh 子进程该用的 npm 源。
//
// 用户点名的源优先：那是他自己的决定，不能被自动换源盖掉；没点名时才用
// 后台准备阶段实际选中的那个（可能是镜像源）。
func (s *Supervisor) registry() string {
	if s.opts.NPMRegistry != "" {
		return s.opts.NPMRegistry
	}
	s.commandMu.RLock()
	defer s.commandMu.RUnlock()
	return s.resolvedRegistry
}

// childEnv 是 dsh 子进程的环境变量。
func (s *Supervisor) childEnv() []string {
	env := append(os.Environ(), "DSH_HOME="+s.opts.Home)
	return npmEnv(env, s.registry())
}

// NativeURL 返回 dsh 的本机原生访问地址（含启动令牌），令牌未知时返回空串。
//
// 为什么要把它显示出来：出问题时需要能绕过网关直接连 dsh 看一眼——
// 令牌交接、Host 校验、代理转发这三段里哪一段坏了，只有这样才分得清。
//
// 这不构成凭据外泄：dsh 只监听回环，这个地址里的令牌在别的机器上毫无用处，
// 而 dsh 自己启动时也会把同一条 URL 打在终端上。这里只是把它排版得更显眼。
func (s *Supervisor) NativeURL() string {
	token := s.Token()
	if token == "" {
		return ""
	}
	return "http://" + s.Addr() + "/?token=" + token
}

// Token 返回已知的 dsh 启动令牌，可能为空。
//
// 这个令牌是「让浏览器拿到 dsh 会话的唯一钥匙」，因此它的生命周期被严格
// 限制在这个进程的内存里：不落盘、不进结构化日志字段。
func (s *Supervisor) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token
}

// SetToken 记住启动令牌。
//
// 除了从 dsh 启动输出里自动读取，网关还会在浏览器把带 token 的 URL 粘过来
// 时调用它——这是「接管别人启动的 dsh」时唯一可行的令牌来源。
func (s *Supervisor) SetToken(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	changed := s.token != token
	s.token = token
	s.mu.Unlock()
	if changed {
		// 打出完整地址而不是只说「已记录」：网关本身不会用这条地址，
		// 人要用——排查时需要能绕过网关直接连 dsh。
		s.log.Info("已记录 dsh 启动令牌，本机可直接打开（该地址只在回环上有用）",
			"本机地址", "http://"+s.Addr()+"/?token="+token)
	}
}

// PID 返回被托管的 dsh 进程号；接管外部实例时返回 0。
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid
}

// Ready 表示 dsh 端口当前可转发。
func (s *Supervisor) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Running 表示子进程正在运行（接管模式下恒为 false）。
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil && s.cmd.ProcessState == nil
}

// Adopted 表示这个 dsh 是接管来的（不是网关启动的）。
//
// 与 Managed 是互补关系，但在「还没决定」的那一刻两者都是 false——所以要判断
// 「确实接管了一个外部实例」，必须看这个而不是 !Managed。
func (s *Supervisor) Adopted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adopted
}

// Managed 表示这个 dsh 是网关自己拉起的（false 表示接管了外部实例）。
func (s *Supervisor) Managed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd != nil
}

// LastError 返回最近一次启动失败的原因，空串表示没有失败过。
//
// 等待页面会把它显示出来：dsh 起不来时，用户看到的是「因为插件安装失败而
// 启动不了」，而不是一个永远转圈的圈。
func (s *Supervisor) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// setLastError 记录最近一次失败；err 为 nil 表示清空。
func (s *Supervisor) setLastError(err error) {
	s.mu.Lock()
	s.lastErr = errText(err)
	s.mu.Unlock()
}

// Run 持续尝试把 dsh 拉起来，直到成功或上下文结束。
//
// 与一次性的 Start 的区别只在于失败后的重试。之所以要有这个包装：dsh 首次
// 启动要下载插件，网络抽风、npm 源抖动都可能让它失败，而「网关一直在线、
// dsh 慢慢总能起来」正是无界面服务器上应有的行为。成功后本方法立刻返回——
// 此后的崩溃重启由 reap 负责。
func (s *Supervisor) Run(ctx context.Context) {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := s.Start(ctx)
		if err == nil {
			return
		}

		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if stopped || !s.opts.Restart {
			s.log.Error("dsh 未能启动", "addr", s.Addr(), "attempt", attempt, "err", err)
			return
		}

		s.log.Warn("dsh 启动失败，稍后重试",
			"addr", s.Addr(), "attempt", attempt, "after", backoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// WaitReady 阻塞直到 dsh 就绪或上下文结束。
func (s *Supervisor) WaitReady(ctx context.Context) error {
	select {
	case <-s.readyCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start 决定 dsh 该怎么来，然后让它就绪。
//
// 三条路径，正好对应用户的三种现实情况：
//
//  1. 端口空闲且允许自动拉起 → 启动子进程，等它监听成功。
//  2. 端口已被占用且允许接管 → 直接用它，不重复启动（这是最常见的情况：
//     用户已经自己在跑 dsh 了）。
//  3. 端口已被占用但不允许接管，或占用者不是 dsh → 报错退出，而不是把流量
//     默默转发给一个不相干的服务。
func (s *Supervisor) Start(ctx context.Context) (err error) {
	// 无论成功还是失败都记账：等待页面把 LastError 显示给用户，
	// 没有它，一次「打不开」就只剩下一个永远转圈的圈。
	defer func() { s.setLastError(err) }()

	state := Inspect(s.Addr(), probeTimeout)
	switch {
	case state.InUse && state.DSH:
		if !s.opts.Adopt {
			return fmt.Errorf("dsh 端口 %s 已被占用（%s）；加 --adopt=false 可禁止接管，"+
				"或换一个 --dsh-port", s.Addr(), state.Detail)
		}
		s.adopt(state)
		return nil

	case state.InUse:
		if s.opts.Adopt && s.opts.AutoStart {
			// 端口被占但不像 dsh。这里仍然接管，但要喊得足够大声：
			// 宁可用户多看一眼日志，也不要把网关默默挂到一个陌生服务上。
			s.log.Warn("端口已被占用且未识别为 dsh，按接管处理；如果转发结果不对，"+
				"请用 --adopt=false 让网关直接报错，或换端口",
				"addr", s.Addr(), "detail", state.Detail)
			s.adopt(state)
			return nil
		}
		return fmt.Errorf("dsh 端口 %s 已被占用（%s）；换一个 --dsh-port，"+
			"或先停掉占用它的进程", s.Addr(), state.Detail)

	default:
		if !s.opts.AutoStart {
			return fmt.Errorf("dsh 端口 %s 空闲，但当前是「不自动拉起 dsh」模式；"+
				"请自己先启动 dsh，或加 --auto-start", s.Addr())
		}
		return s.launch(ctx)
	}
}

// adopt 接管一个已经在跑的 dsh。
func (s *Supervisor) adopt(state State) {
	s.mu.Lock()
	s.adopted = true
	s.setReadyLocked(true)
	s.mu.Unlock()
	s.log.Info("接管已在运行的 dsh 实例", "addr", s.Addr(), "pid", state.PID, "detail", state.Detail)
	if s.opts.Token == "" {
		// 这条提示与「重新登录」页、启动横幅讲的是同一件事，三处说法要一致。
		// 令牌每个进程随机生成、只在 dsh 启动时打印一次、不落盘，所以别人拉起的
		// dsh 我们无从得知——出路只有「用户还记得那个地址」或「让网关自己管 dsh」。
		s.log.Warn("接管的实例没有已知启动令牌：浏览器能开首页，但对话会报未授权。"+
			"两条出路：① 还留着 dsh 启动时打印的地址，就把它粘到网关地址后面打开一次"+
			"（也可用 --dsh-token 指定）；② 停掉外面那个 dsh 再重启本实例，"+
			"让网关自己拉起 dsh（一定成功）",
			"addr", s.Addr())
	}
}

// launch 启动 dsh 子进程并等待它就绪。
//
// 这是「第一次启动」的路径：spawn 出的孩子交给一个新的 reap goroutine 看守，
// 此后它若秒退，重启由那个 reap 独家负责（退避跨代累计），不会再叠加循环。
func (s *Supervisor) launch(ctx context.Context) error {
	args, err := s.buildCommand(ctx)
	if err != nil {
		return err
	}
	// 超时要留给子进程真正就绪，而不是只够 fork 出来。
	waitCtx, cancel := context.WithTimeout(ctx, s.opts.StartTimeout)
	defer cancel()

	s.mu.Lock()
	reaping := s.reaping
	s.mu.Unlock()
	if reaping {
		// 崩溃重启循环正在拉起 dsh：不要再生一个孩子与它抢端口，等它就绪
		// 即可——dsh 起来时它会把 ready 置位。
		if err := WaitPort(waitCtx, s.Addr(), 200*time.Millisecond); err != nil {
			return fmt.Errorf("dsh 正在由重启流程拉起，但在 %s 内仍未监听 %s：%w",
				s.opts.StartTimeout, s.Addr(), err)
		}
		return nil
	}

	_, waitCh, exited, err := s.spawn(args)
	if err != nil {
		return err
	}
	go s.reap(waitCh, exited, time.Second)

	if err := WaitPort(waitCtx, s.Addr(), 200*time.Millisecond); err != nil {
		s.dumpRecent()
		s.stopChild(s.opts.StopTimeout)
		return fmt.Errorf("dsh 已启动（%s）但在 %s 内没有监听 %s：%w",
			redact(args[0]), s.opts.StartTimeout, s.Addr(), err)
	}
	s.mu.Lock()
	s.setReadyLocked(true)
	s.mu.Unlock()

	s.log.Info("dsh 已就绪", "addr", s.Addr(), "pid", s.PID(), "command", redact(strings.Join(args, " ")))
	return nil
}

// spawn 真正拉起子进程，并接管它的输出与生命周期。
//
// cmd.Wait 由本函数启动的 goroutine 恰好执行一次：它的错误送进 waitCh
// （「孩子退出」的唯一信号源，重启循环与 reap 都从这读），随后才关闭
// exited（「进程确实没了」）。不在 spawn 里启动 reap——重启的所有权归
// reap 自己（见其注释），否则「秒退的子进程」会让新旧两个重启循环并存，
// 崩溃风暴就是这么来的。
func (s *Supervisor) spawn(args []string) (*exec.Cmd, <-chan error, chan struct{}, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = s.opts.Workspace // 空串表示沿用当前进程的工作目录
	cmd.Env = s.childEnv()
	applyProcessAttrs(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("创建 dsh 输出管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("创建 dsh 错误管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, fmt.Errorf("启动 dsh 失败（%s）: %w", args[0], err)
	}

	waitCh := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		waitErr := cmd.Wait()
		s.mu.Lock()
		if s.cmd == cmd {
			s.cmd = nil
		}
		s.mu.Unlock()
		waitCh <- waitErr
		close(exited)
	}()

	s.mu.Lock()
	s.cmd = cmd
	s.pid = cmd.Process.Pid
	s.stopped = false
	s.exited = exited
	s.mu.Unlock()

	go s.consume(stdout)
	go s.consume(stderr)
	return cmd, waitCh, exited, nil
}

// consume 逐行读取 dsh 的输出。
//
// 唯一需要「看懂」的是启动令牌：dsh 把它写在打印出来的 URL 里，不落盘也不
// 有别的接口可问。其余行一律按 debug 级别记录，并且**先脱敏再入库**——
// 启动 URL 里的 token 等同于 dsh 的万能钥匙，绝不能进日志。
func (s *Supervisor) consume(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if token := SplitToken(line); token != "" {
			s.SetToken(token)
		}
		s.remember(RedactToken(line))
		s.log.Debug("dsh", "line", RedactToken(line))
	}
	_ = scanner.Err()
}

// rememberLines 是回放给用户看的行数上限。
//
// dsh 启动失败时会打出几十行插件诊断，全贴出来没人看得下去；只留最近这些，
// 恰好覆盖「最后一个错误」与它前面的因。
const rememberLines = 24

// remember 把一行输出记进环形缓冲。
func (s *Supervisor) remember(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if line == "" && len(s.recent) > 0 {
		return // 不让空行把有用的内容顶掉
	}
	s.recent = append(s.recent, line)
	if len(s.recent) > rememberLines {
		s.recent = s.recent[len(s.recent)-rememberLines:]
	}
}

// dumpRecent 输出 dsh 最近说过的话。
//
// 没有它，一次「dsh 起不来」在默认日志级别下只有一句 exit status 1——
// 用户既不知道是插件装不上、是端口冲突，还是自己机器上的某个 IPv6 地址
// 写错了。这是排查时唯一真正有用的信息。
func (s *Supervisor) dumpRecent() {
	s.mu.Lock()
	lines := append([]string(nil), s.recent...)
	s.recent = nil
	s.mu.Unlock()
	for _, line := range lines {
		s.log.Warn("dsh 输出", "line", line)
	}
}

// reap 等待子进程退出，并按需重启。
//
// 子进程的 Wait 在 spawn 里恰好执行一次，退出错误从 waitCh 读、进程确实
// 没了的信号是 exited 关闭——另开 goroutine 再 Wait 会报错，轮询
// ProcessState 则与它形成数据竞争。
//
// 重启的所有权：整个 Supervisor 同一时刻最多只有一个重启循环（reaping 标记）。
// 若每次 spawn 都另起一个 reap，「秒退」的子进程（Node 跑不动这版 dsh）会让
// 新旧循环并存，各自的退避都从 1 秒重新数起，几秒内就能堆出几十个并发循环
// 把机器拖垮——这个坑在 Debian 容器里真的踩过：四分钟重试了四千多次。因此
// 重启循环自己拉起下一代并等它的三种结局（就绪 / 又秒退 / 超时）；只有孩子
// 真正就绪过，才把看守权连同归零的退避一起交给新的 reap。
func (s *Supervisor) reap(waitCh <-chan error, exited chan struct{}, backoff time.Duration) {
	err := <-waitCh

	s.mu.Lock()
	wasRunning := s.ready
	s.setReadyLocked(false)
	stopped := s.stopped
	pid := s.pid
	s.mu.Unlock()

	if stopped {
		s.log.Info("dsh 已停止", "addr", s.Addr(), "pid", pid)
		return
	}
	if err == nil && !wasRunning {
		// 退出码 0 却从未就绪：dsh「跑了但什么都没做」。最典型的是入口
		// 按 Bun 构建的发行版（0.2.0-rc 系列）在 Node 下静默退出。
		s.log.Error("dsh 启动后立即静默退出（退出码 0、从未监听端口）：这版 dsh 的入口很可能按 Bun 构建，" +
			"当前 Node.js 跑不动它。请安装 Bun（curl -fsSL https://bun.sh/install | bash）后重启网关" +
			"——网关会自动改用 Bun 运行 dsh；或升级 Node.js 到最新版；或用 --dsh-cmd 指定启动命令")
		s.dumpRecent()
	} else {
		s.log.Warn("dsh 意外退出", "addr", s.Addr(), "pid", pid, "err", errText(err), "wasReady", wasRunning)
		s.diagnoseCrash()
		s.dumpRecent()
	}

	if !s.opts.Restart {
		return
	}
	if wasRunning {
		// 稳定运行过一段时间才崩的，多半是偶发（插件抽风、被误杀），
		// 退避从头数；秒退则延续上一代的退避，否则退避形同虚设。
		backoff = time.Second
	}
	s.mu.Lock()
	s.reaping = true
	s.mu.Unlock()

	for attempt := 1; ; attempt++ {
		s.mu.Lock()
		stopped = s.stopped
		s.mu.Unlock()
		if stopped {
			s.mu.Lock()
			s.reaping = false
			s.mu.Unlock()
			return
		}
		s.log.Info("准备重启 dsh", "addr", s.Addr(), "attempt", attempt, "after", backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)

		args, err := s.buildCommand(context.Background())
		if err != nil {
			s.log.Error("重启 dsh 失败，将继续重试", "addr", s.Addr(), "err", err)
			continue
		}
		newCmd, newWaitCh, newExited, err := s.spawn(args)
		if err != nil {
			s.log.Error("重启 dsh 失败，将继续重试", "addr", s.Addr(), "err", err)
			continue
		}

		// 等三种结局之一：就绪（交还看守权）、又秒退（本循环继续退避）、
		// 超时（停掉没监听的孩子，继续退避）。
		waitCtx, cancel := context.WithTimeout(context.Background(), s.opts.StartTimeout)
		portCh := make(chan error, 1)
		go func() { portCh <- WaitPort(waitCtx, s.Addr(), 200*time.Millisecond) }()

		var childErr error
		readyNow := false
		select {
		case portErr := <-portCh:
			if portErr == nil {
				readyNow = true
				break
			}
			s.dumpRecent()
			s.stopChild(s.opts.StopTimeout)
			select {
			case childErr = <-newWaitCh:
			case <-time.After(10 * time.Second):
				// 组 SIGKILL 后 Wait 仍未返回（不可杀的 D 态进程）：
				// 不能让它卡死整个重启循环。
				s.log.Warn("重启的孩子在强制结束后仍未退出，继续退避重试", "pid", newCmd.Process.Pid)
			}
		case childErr = <-newWaitCh:
			cancel()
		}
		cancel()

		if readyNow {
			s.mu.Lock()
			s.setReadyLocked(true)
			s.reaping = false
			s.mu.Unlock()
			s.log.Info("dsh 已就绪", "addr", s.Addr(), "pid", s.PID())
			go s.reap(newWaitCh, newExited, time.Second)
			return
		}
		if childErr != nil {
			s.log.Warn("dsh 意外退出", "addr", s.Addr(), "pid", newCmd.Process.Pid,
				"err", errText(childErr), "wasReady", false)
			s.diagnoseCrash()
			s.dumpRecent()
		}
	}
}

// diagnoseCrash 从最近的输出里识别 Node 运行时崩溃的横幅（Node.js v…），
// 把话说完：否则用户只看到一串「意外退出」按退避重试，不知道该去升级 Node。
func (s *Supervisor) diagnoseCrash() {
	s.mu.Lock()
	lines := append([]string(nil), s.recent...)
	s.mu.Unlock()
	match := nodeBannerPattern.FindStringSubmatch(strings.Join(lines, "\n"))
	if match == nil {
		return
	}
	major, minor := match[1], match[2]
	joined := strings.Join(lines, "\n")
	hint := "崩溃原因请看上面的 dsh 输出"
	if strings.Contains(joined, "SyntaxError") || strings.Contains(joined, "does not provide an export") {
		hint = "输出引用了当前 Node 缺少的新 API——这版 dsh 与当前 Node.js 不匹配，升级 Node.js 后重试即可恢复"
	}
	s.log.Error(fmt.Sprintf("dsh 的崩溃输出带着 Node.js v%s.%s 的运行时横幅：%s；"+
		"也可用 --dsh-cmd 指定其它启动方式", major, minor, hint))
}

// Shutdown 停止被托管的 dsh，并等待它确实退出。
//
// 顺序很重要：先标记 stopped（让 reap 不再重启），再发终止信号。
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	hasChild := s.cmd != nil
	s.setReadyLocked(false)
	s.mu.Unlock()

	if !hasChild {
		return nil
	}

	done := make(chan struct{})
	go func() {
		s.stopChild(s.opts.StopTimeout)
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// setReadyLocked 更新就绪状态并唤醒等待者。调用方必须持有锁。
func (s *Supervisor) setReadyLocked(ready bool) {
	if s.ready == ready {
		return
	}
	s.ready = ready
	if ready {
		close(s.readyCh)
	} else {
		s.readyCh = make(chan struct{})
	}
}

// stopChild 给子进程发终止信号，等宽限时间还不走就强杀。
func (s *Supervisor) stopChild(grace time.Duration) {
	s.mu.Lock()
	cmd, exited := s.cmd, s.exited
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}

	if err := terminate(cmd); err != nil {
		s.log.Debug("向 dsh 发送终止信号失败，直接结束进程", "err", err)
		_ = kill(cmd)
		return
	}

	select {
	case <-exited:
		return
	case <-time.After(grace):
	}

	s.log.Warn("dsh 未在宽限时间内退出，强制结束", "addr", s.Addr(), "pid", cmd.Process.Pid)
	_ = kill(cmd)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		s.log.Warn("强制结束后仍未收到进程退出通知", "addr", s.Addr(), "pid", cmd.Process.Pid)
	}
}

// buildCommand 生成 dsh 的启动命令。
//
// 自动探测出的命令会先问一遍 dsh 自己支持哪些选项（见 supportedFlags），
// 再只保留它认识的那些——不同版本的 dsh 选项差异很大，传一个它不认识的
// 参数会让它直接退出，网关陷入「启动→失败→重启」的死循环。
//
// 用户用 --dsh-cmd 显式给出的命令不做过滤：那是用户自己的决定，出错时
// 由 dsh 自己给出更准确的报错。
func (s *Supervisor) buildCommand(ctx context.Context) ([]string, error) {
	template := strings.TrimSpace(s.opts.Command)
	if template == "" {
		// 冒烟验证过的命令优先于盲探测：EnsureDSH 已经确认它能执行
		// （0.2.0-rc 系列在 Node 下静默退出，盲探测出来的命令是坏的）。
		command := s.resolvedCommand()
		if command == "" {
			var err error
			command, err = detectBinary()
			if err != nil {
				return nil, err
			}
		}
		// 探测结果可能是 "dsh"，也可能是 "npx @deepseek-ai/dsh"——后者必须拆开，
		// 否则 exec 会去找一个名叫 "npx @deepseek-ai/dsh" 的文件。
		prefix, err := SplitCommand(command)
		if err != nil || len(prefix) == 0 {
			prefix = []string{command}
		}
		// 复核可执行文件还在不在：自备的 Node 运行时会被清理掉（冒烟失败、手动
		// 删状态目录、或另一个进程换了目录），而 PATH 里记着的还是旧路径。不查的话
		// 用户看到的是 `fork/exec …/bin/npx: no such file or directory`——一句
		// 只说现象、不说怎么办的话。
		if err := checkLaunchable(prefix[0]); err != nil {
			return nil, err
		}
		args := append([]string{}, prefix...)
		args = append(args, "web", "--host", s.opts.Host, "--port", strconv.Itoa(s.opts.Port), "--no-open")

		optional := []string{}
		if s.opts.PublicURL != "" {
			optional = append(optional, "--public-url", s.opts.PublicURL)
		}
		for _, host := range s.opts.TrustedHosts {
			optional = append(optional, "--trusted-host", host)
		}
		args = append(args, s.filterSupported(ctx, command, optional)...)
		return args, nil
	}

	expanded, err := s.expand(template)
	if err != nil {
		return nil, err
	}
	args, err := SplitCommand(expanded)
	if err != nil {
		return nil, fmt.Errorf("解析 dsh 启动命令 %q 失败: %w", template, err)
	}
	if len(args) == 0 {
		return nil, errors.New("dsh 启动命令为空")
	}
	return args, nil
}

// alwaysSupported 是所有已知 dsh 版本都支持的选项。
//
// 探测失败时按这份保守清单放行：宁可少传一个锦上添花的选项，也不要因为
// 一个新参数让 dsh 起不来。
var alwaysSupported = map[string]bool{
	"--host": true, "--port": true, "--no-open": true, "--trusted-host": true,
}

// filterSupported 从候选参数里剔除当前 dsh 不认识的选项。
func (s *Supervisor) filterSupported(ctx context.Context, binary string, optional []string) []string {
	supported := s.supportedFlags(ctx, binary)
	kept := make([]string, 0, len(optional))
	for i := 0; i+1 < len(optional); i += 2 {
		flag := optional[i]
		if supported[flag] {
			kept = append(kept, flag, optional[i+1])
			continue
		}
		s.log.Info("当前 dsh 不支持该选项，已略过（升级 dsh 后会自动启用）",
			"option", flag, "dsh", s.opts.Home)
	}
	return kept
}

// supportedFlags 通过 `dsh web --help` 学到 dsh 支持哪些长选项，结果只探测一次。
//
// 为什么要问而不是写死：dsh 的命令行选项在版本之间差异很大（0.2.0-rc.2 只有
// --host/--port/--no-open/--trusted-host，更新的版本才有 --public-url 与
// --tls-*）。写死一份清单，要么在新版上少用了能力，要么在旧版上起不来。
func (s *Supervisor) supportedFlags(ctx context.Context, binary string) map[string]bool {
	s.mu.Lock()
	cached := s.supported
	s.mu.Unlock()
	if cached != nil {
		return cached
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args, err := SplitCommand(binary)
	if err != nil {
		args = []string{binary}
	}
	args = append(args, "web", "--help")
	cmd := exec.CommandContext(probeCtx, args[0], args[1:]...)
	cmd.Env = s.childEnv()
	cmd.Dir = s.opts.Workspace

	supported := map[string]bool{}
	if output, err := cmd.CombinedOutput(); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			for _, match := range longFlagPattern.FindAllStringSubmatch(line, -1) {
				supported["--"+match[1]] = true
			}
		}
	}
	if len(supported) == 0 {
		// 探测失败（dsh 还没装好、帮助信息格式变了……）：按保守清单来。
		s.log.Warn("没能读取 dsh 的帮助信息，将只使用通用选项", "command", strings.Join(args, " "))
		for flag := range alwaysSupported {
			supported[flag] = true
		}
	}

	s.mu.Lock()
	s.supported = supported
	s.mu.Unlock()
	return supported
}

// longFlagPattern 从帮助文本里抓出长选项名。
var longFlagPattern = regexp.MustCompile(`--([a-zA-Z][a-zA-Z0-9-]*)`)

// expand 替换命令模板里的占位符。
//
// {trusted} 每次出现替换成下一个可信 host；这样 `--trusted-host {trusted}`
// 写几遍就带几个值，不需要用户自己拼逗号分隔串。
func (s *Supervisor) expand(template string) (string, error) {
	var builder strings.Builder
	trusted := append([]string(nil), s.opts.TrustedHosts...)
	next := func() (string, error) {
		if len(trusted) == 0 {
			return "", fmt.Errorf("命令模板用了 {trusted}，但没有配置任何 --trusted-host")
		}
		value := trusted[0]
		trusted = trusted[1:]
		return value, nil
	}

	for i := 0; i < len(template); {
		open := strings.IndexByte(template[i:], '{')
		if open < 0 {
			builder.WriteString(template[i:])
			break
		}
		open += i
		close := strings.IndexByte(template[open:], '}')
		if close < 0 {
			builder.WriteString(template[i:])
			break
		}
		close += open
		builder.WriteString(template[i:open])
		var (
			value string
			err   error
		)
		switch template[open+1 : close] {
		case "host":
			value = s.opts.Host
		case "port":
			value = fmt.Sprintf("%d", s.opts.Port)
		case "home":
			value = s.opts.Home
		case "workspace":
			value = s.opts.Workspace
		case "url":
			value = s.opts.PublicURL
		case "noopen":
			value = "--no-open"
		case "trusted":
			value, err = next()
		default:
			return "", fmt.Errorf("未知的占位符 {%s}；可用：{host} {port} {home} {workspace} {url} {noopen} {trusted}",
				template[open+1:close])
		}
		if err != nil {
			return "", err
		}
		builder.WriteString(value)
		i = close + 1
	}
	return builder.String(), nil
}

// DetectBinary 探测本机可用的 dsh 启动命令。
//
// 暴露出来是为了让 `dsh-gate check` 能在启动之前就告诉用户「找不到 dsh」，
// 而不是让人先跑起来、在等待页上才发现。
func DetectBinary() (string, error) { return detectBinary() }

// detectBinary 探测官方 dsh 启动命令。
//
// 官方命令就是 npx @deepseek-ai/dsh web：npx 每次启动都会对照 npm 检查版本，
// 包不存在或有新版本时需要交互确认安装/更新——--yes 把这个确认自动化
// （网关拉起的进程没有终端可回答，不加它只会收到 EOF 然后失败）。
//
// 返回值是「可执行文件 + 必要的参数」，因为 npx 这条路必须带上包名。
// 可执行性由 EnsureDSH 的冒烟验证负责，这里只做存在性探测。
func detectBinary() (string, error) {
	// Windows 上 npx 是 npx.cmd；这里连包名一起返回——只返回 "npx" 的话，
	// 拼出来的命令会变成 "npx web"，npx 会去找一个名叫 web 的包然后失败。
	for _, candidate := range []string{"npx", "npx.cmd"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate + " --yes " + dshPackage, nil
		}
	}
	return "", errors.New("找不到 npx：dsh 通过官方命令 npx --yes @deepseek-ai/dsh 启动。" +
		"网关启动时会把官方 Node.js 运行时下载到状态目录；这条错误如果一直重复出现，" +
		"说明那次下载没成功——日志里搜「正在下载」看原因，或手工安装 Node.js" +
		"（https://nodejs.org/），也可以用 --dsh-cmd 指定启动命令，" +
		"例如 \"npx --yes @deepseek-ai/dsh web --port {port}\"")
}

// errText 把错误渲染成适合放进日志的一行文本。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RedactToken 把输出里的启动令牌换成 ***，用于日志与错误信息。
func RedactToken(line string) string {
	if !strings.Contains(line, "token=") {
		return line
	}
	var out strings.Builder
	rest := line
	for {
		idx := strings.Index(rest, "token=")
		if idx < 0 {
			out.WriteString(rest)
			return out.String()
		}
		out.WriteString(rest[:idx])
		out.WriteString("token=***")
		rest = rest[idx+len("token="):]
		if end := strings.IndexAny(rest, " \t\r\n\"'&<>"); end >= 0 {
			out.WriteString(rest[:end])
			rest = rest[end:]
		} else {
			return out.String()
		}
	}
}

// redact 给命令行里的令牌打码。
func redact(command string) string { return RedactToken(command) }

// checkLaunchable 在真正 fork 之前确认可执行文件还在。
//
// 只对绝对路径较真：PATH 里的命令由 exec 自己解析，而自备运行时是写死的绝对
// 路径——它背后的目录可能已经不存在了。
func checkLaunchable(binary string) error {
	if !filepath.IsAbs(binary) {
		return nil
	}
	if _, err := os.Stat(binary); err == nil {
		return nil
	}
	return fmt.Errorf("找不到 %s：它所在的目录可能已被删除或清理。"+
		"如果这是网关自备的 Node.js 运行时，删掉状态目录里的 node/ 让网关重新下载即可"+
		"（或重启网关实例 dsh-gate restart）", binary)
}
