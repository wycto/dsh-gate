// Package svclife 是「受管服务」的通用进程监督器（docs/service-api.md 第 7 节）。
//
// 与 dshproc（监督 dsh 实例）不同：这里管的是**任意**受管服务的启动命令，与 dsh 无关。
// 取舍与 dshproc 一致：
//   - 进程组独立（unix Setpgid），停止时杀整组，不留孤儿；
//   - stdout/stderr 落盘（<StateDir>/logs/<id>.log，0600、超 8MB 轮转）；
//   - 运行态只在内存里（pid/state/last_error），重启网关即清零——文档已声明运行态不落库；
//   - 意外退出按退避自动重启（1s 起、上限 30s），直到被显式 Stop。
package svclife

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Options 是监督器配置。
type Options struct {
	// StateDir 是状态目录：日志写到 <StateDir>/logs/<id>.log。
	StateDir string
	// Logger 收日志；为 nil 时丢弃。
	Logger *slog.Logger
	// StartTimeout 是等待服务就绪的上限；<=0 用 30s。
	StartTimeout time.Duration
}

// Spec 是一条受管服务的启动规格。
type Spec struct {
	ID         string
	Name       string
	StartCmd   string
	StopCmd    string
	WorkDir    string
	Env        string // 每行 KEY=VALUE
	Upstream   string // http(s)://host:port[/base]，用于就绪探测与代理
	HealthPath string // 就绪探测路径；空则探测 Upstream 端口
	AutoStart  bool
}

// Status 是一条受管服务的运行态。
type Status struct {
	Running   bool
	PID       int
	State     string // stopped / starting / running / exited / crashed
	StartedAt time.Time
	LastError string
}

// proc 是单个受管服务的内部运行态。
type proc struct {
	spec      Spec
	cmd       *exec.Cmd
	logFile   *os.File
	running   bool
	stopping  bool
	state     string
	pid       int
	startedAt time.Time
	lastErr   string
	done      chan struct{} // 当前进程退出时关闭
}

// Supervisor 是一组受管服务的监督器（并发安全）。
type Supervisor struct {
	opts  Options
	log   *slog.Logger
	mu    sync.Mutex
	procs map[string]*proc
}

// New 构造监督器。
func New(opts Options) *Supervisor {
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	_ = os.MkdirAll(filepath.Join(opts.StateDir, "logs"), 0o700)
	return &Supervisor{opts: opts, log: opts.Logger, procs: map[string]*proc{}}
}

func (s *Supervisor) logPath(id string) string {
	return filepath.Join(s.opts.StateDir, "logs", id+".log")
}

// Start 启动一条受管服务（幂等：已在运行返回 nil）。就绪等待不阻塞超过 StartTimeout，
// 超时只记 LastError，进程照旧存活（由 waitLoop 继续守护）。
func (s *Supervisor) Start(spec Spec) error {
	s.mu.Lock()
	p := s.procs[spec.ID]
	if p != nil && p.running {
		s.mu.Unlock()
		return nil
	}
	if p == nil {
		p = &proc{}
		s.procs[spec.ID] = p
	}
	p.spec = spec
	p.stopping = false
	p.state = "starting"
	p.lastErr = ""
	if err := s.startProcess(p); err != nil {
		p.state = "stopped"
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()

	go s.waitLoop(p)
	s.waitReady(p)
	return nil
}

// startProcess 真正拉起子进程。调用方必须持有 s.mu。
func (s *Supervisor) startProcess(p *proc) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", p.spec.StartCmd)
	} else {
		cmd = exec.Command("sh", "-c", p.spec.StartCmd)
	}
	if wd := strings.TrimSpace(p.spec.WorkDir); wd != "" {
		cmd.Dir = wd
	}
	cmd.Env = append(os.Environ(), parseEnv(p.spec.Env)...)
	configureProc(cmd)

	lf, err := openLogFile(s.logPath(p.spec.ID))
	if err != nil {
		return fmt.Errorf("打开服务日志失败: %w", err)
	}
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		return fmt.Errorf("启动受管服务失败: %w", err)
	}
	p.cmd = cmd
	p.logFile = lf
	p.running = true
	p.pid = cmd.Process.Pid
	p.startedAt = time.Now()
	p.state = "starting"
	p.done = make(chan struct{})
	s.log.Info("受管服务已启动", "id", p.spec.ID, "name", p.spec.Name, "pid", p.pid)
	return nil
}

// waitLoop 等待当前进程退出；未被 Stop 时按退避自动重启。
func (s *Supervisor) waitLoop(p *proc) {
	backoff := time.Second
	for {
		cmd := p.cmd
		err := cmd.Wait()

		s.mu.Lock()
		if p.logFile != nil {
			_ = p.logFile.Close()
			p.logFile = nil
		}
		if p.done != nil {
			close(p.done)
			p.done = nil
		}
		p.running = false
		p.pid = 0
		stopping := p.stopping
		if stopping {
			p.state = "stopped"
		} else {
			p.state = "crashed"
			if err != nil {
				p.lastErr = err.Error()
			}
			s.log.Warn("受管服务意外退出，将自动重启", "id", p.spec.ID, "err", err, "backoff", backoff)
		}
		s.mu.Unlock()
		if stopping {
			return
		}

		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}

		s.mu.Lock()
		if p.stopping {
			s.mu.Unlock()
			return
		}
		if err := s.startProcess(p); err != nil {
			p.lastErr = err.Error()
			p.state = "crashed"
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		go s.waitReady(p)
	}
}

// Stop 停止一条受管服务：先 SIGTERM 进程组，10s 未退再 SIGKILL。幂等。
func (s *Supervisor) Stop(id string) error {
	s.mu.Lock()
	p := s.procs[id]
	if p == nil {
		s.mu.Unlock()
		return nil
	}
	p.stopping = true
	cmd := p.cmd
	done := p.done
	stopCmd := p.spec.StopCmd
	s.mu.Unlock()

	if cmd == nil || cmd.Process == nil || done == nil {
		s.mu.Lock()
		p.running = false
		p.pid = 0
		p.state = "stopped"
		s.mu.Unlock()
		return nil
	}
	if strings.TrimSpace(stopCmd) != "" {
		runShell(stopCmd)
	}
	_ = terminateProc(cmd, false)
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
	}
	s.log.Warn("受管服务未在宽限内退出，强制结束", "id", id)
	_ = terminateProc(cmd, true)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

// Restart 停止后重新启动。
func (s *Supervisor) Restart(spec Spec) error {
	if err := s.Stop(spec.ID); err != nil {
		return err
	}
	return s.Start(spec)
}

// Status 返回受管服务的运行态（未知服务返回 stopped）。
func (s *Supervisor) Status(id string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.procs[id]
	if p == nil {
		return Status{State: "stopped"}
	}
	return Status{Running: p.running, PID: p.pid, State: p.state, StartedAt: p.startedAt, LastError: p.lastErr}
}

// Logs 返回最近 tail 行日志（tail<=0 默认 200）。文件不存在返回空串。
func (s *Supervisor) Logs(id string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	data, err := os.ReadFile(s.logPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n"), nil
}

// StopAll 停掉全部受管进程（网关关闭时调用）。
func (s *Supervisor) StopAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.procs))
	for id := range s.procs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_ = s.Stop(id)
	}
}

// waitReady 在 StartTimeout 内等待服务就绪（端口监听或健康路径 2xx/3xx）。
func (s *Supervisor) waitReady(p *proc) {
	up := strings.TrimSpace(p.spec.Upstream)
	hp := strings.TrimSpace(p.spec.HealthPath)
	if up == "" && hp == "" {
		s.mu.Lock()
		if p.running {
			p.state = "running"
		}
		s.mu.Unlock()
		return
	}
	deadline := time.Now().Add(s.opts.StartTimeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running := p.running
		s.mu.Unlock()
		if !running {
			return
		}
		if probeReady(up, hp) {
			s.mu.Lock()
			if p.running {
				p.state = "running"
				p.lastErr = ""
			}
			s.mu.Unlock()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.mu.Lock()
	if p.running && p.state == "starting" {
		p.lastErr = "启动后在超时内未就绪（进程仍在运行）"
	}
	s.mu.Unlock()
}

// probeReady 探测服务是否就绪：HealthPath 非空则 GET 它，否则探测 Upstream 端口。
func probeReady(upstream, healthPath string) bool {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	if healthPath != "" {
		scheme := u.Scheme
		if scheme != "http" && scheme != "https" {
			scheme = "http"
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(scheme + "://" + host + "/" + strings.TrimPrefix(healthPath, "/"))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400
	}
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// parseEnv 把每行 KEY=VALUE 解析成环境变量切片（跳过空行）。
func parseEnv(env string) []string {
	var out []string
	for _, line := range strings.Split(env, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "=") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// runShell 执行自定义停止命令（尽力而为）。
func runShell(cmdline string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", cmdline)
	} else {
		cmd = exec.Command("sh", "-c", cmdline)
	}
	_ = cmd.Run()
}

// openLogFile 打开（必要时轮转）服务日志文件，0600 追加。
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 8<<20 {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
