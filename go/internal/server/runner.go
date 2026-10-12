package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/registry"
	"github.com/wycto/dsh-gate/internal/version"
)

// Runner 把多个网关实例跑在一个进程里。
//
// 多实例存在的理由很具体：家里同时开「公司那台 dsh」和「我自己这台 dsh」时，
// 两套实例要共享同一个二进制、同一份账号管理习惯，但端口、实例目录、会话
// 历史互不相干。放进一个进程，用户只需要 `dsh-gate run --config dsh-gate.json`
// 一条命令，Ctrl-C 一起停——比维护两份 systemd 单元简单得多。
type Runner struct {
	instances []*Instance
	logger    *slog.Logger
	// ctx / cancel 是 Start 内部派生出来的运行期上下文。
	//
	// Ctrl-C 与 `dsh-gate stop` 取消的是**同一个** ctx：只有这样，两种停机
	// 方式才会走完全一样的收尾流程；Serve 也必须盯这个 ctx，否则停机端点
	// 取消了也没人看见（这正是第一版 restart 卡住的原因）。
	ctx    context.Context
	cancel context.CancelFunc
}

// RunOptions 是构造 Runner 时的运行期参数。
type RunOptions struct {
	// Subtitle 是登录页副标题。
	Subtitle string
	// Registry 是运行登记目录；为空则不登记。
	Registry registry.Dir
	// Argv 是启动网关的完整命令行，restart 靠它把实例原样拉起来。
	Argv []string
}

// NewRunner 按配置构造全部实例。
func NewRunner(configs []config.Instance, opts RunOptions) (*Runner, error) {
	if len(configs) == 0 {
		return nil, errors.New("没有可运行的实例")
	}
	logger := slog.Default()
	r := &Runner{logger: logger}
	for _, cfg := range configs {
		instance, err := New(Options{
			Config:   cfg,
			Logger:   prefixLogger(logger, cfg.Name, len(configs) > 1),
			Subtitle: opts.Subtitle,
			Registry: opts.Registry,
			Argv:     opts.Argv,
		})
		if err != nil {
			return nil, fmt.Errorf("实例 %s: %w", cfg.Name, err)
		}
		r.instances = append(r.instances, instance)
	}
	return r, nil
}

// Instances 返回全部实例。
func (r *Runner) Instances() []*Instance { return r.instances }

// SetDSHCommand 把冒烟验证过的 dsh 启动方式转交给所有按自动探测运行的实例。
//
// 用于「首次启动的准备工作放在后台」：网关已经在服务了，命令备好之后从外部
// 送进来，各实例的下一次 dsh 拉起就会用上它。已经显式指定 --dsh-cmd 的实例
// 由 supervisor 自己忽略这个值。
func (r *Runner) SetDSHCommand(res dshproc.Resolution) {
	for _, instance := range r.instances {
		instance.SetDSHCommand(res)
	}
}

// Start 打开全部监听端口。
//
// 任一实例起不来就把已经起来的全部停掉：与其让用户面对「网关起了一半」的
// 局面，不如干净地失败并报错——反正退出码非零，systemd 也不会把它当成功。
func (r *Runner) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	r.ctx, r.cancel = ctx, cancel

	started := make([]*Instance, 0, len(r.instances))
	for _, instance := range r.instances {
		if err := instance.Start(ctx); err != nil {
			rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 15*time.Second)
			for _, done := range started {
				_ = done.Shutdown(rollbackCtx)
			}
			cancelRollback()
			cancel()
			r.ctx, r.cancel = nil, nil
			return err
		}
		started = append(started, instance)
	}

	// 停机端点接到请求时取消同一个上下文——停机流程只有一条，
	// 不存在「Ctrl-C 停得干净、stop 停得潦草」这种事。
	for _, instance := range r.instances {
		instance.SetShutdown(cancel)
	}
	return nil
}

// Serve 阻塞提供全部实例的服务，直到 ctx 结束或某个实例出错。
func (r *Runner) Serve(ctx context.Context) error {
	// 用 Start 派生出来的那个 ctx，而不是调用方传进来的：停机端点取消的是它。
	if r.ctx != nil {
		ctx = r.ctx
	}

	// 缓冲着发送：Serve 一返回就没人再读 errCh 了，容量必须够，否则这些
	// goroutine 会永远卡在发送上。
	errCh := make(chan error, len(r.instances))
	for _, instance := range r.instances {
		go func(inst *Instance) {
			if err := inst.Serve(); err != nil {
				errCh <- fmt.Errorf("实例 %s 停止服务: %w", inst.Name(), err)
			}
		}(instance)
	}

	// 立刻返回，**不**等这些 goroutine 结束。
	//
	// 它们要等 HTTP 服务器被关掉才会退出，而关服务器是调用方在 Serve 返回
	// **之后**才做的事（Shutdown）。在这里等，等于双方互等，停机端点就会被
	// 卡在「已收到请求但端口一直没关」——第一版 restart 就死在这儿。
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// Shutdown 停止全部实例。
func (r *Runner) Shutdown(ctx context.Context) error {
	var problems []string
	for _, instance := range r.instances {
		if err := instance.Shutdown(ctx); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", instance.Name(), err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("停止时出现问题: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Banner 生成全部实例的启动横幅。
func (r *Runner) Banner() string {
	multi := len(r.instances) > 1
	var b strings.Builder
	fmt.Fprintf(&b, "dsh-gate %s · 共 %d 个实例\n", version.Version, len(r.instances))
	for index, instance := range r.instances {
		if index > 0 {
			b.WriteString("\n")
		}
		text := instance.Banner()
		if multi {
			text = strings.Replace(text, "实例 ", "实例 ", 1)
		}
		b.WriteString(text)
	}
	return b.String()
}

// Status 返回全部实例的状态。
func (r *Runner) Status() []Status {
	statuses := make([]Status, 0, len(r.instances))
	for _, instance := range r.instances {
		statuses = append(statuses, instance.Status())
	}
	return statuses
}

// prefixLogger 给每个实例的日志加上名字前缀。
//
// 只有多实例运行时才加：单实例时每行日志前面挂个 "default" 纯属噪音。
func prefixLogger(base *slog.Logger, name string, multi bool) *slog.Logger {
	if !multi {
		return base
	}
	return base.With("instance", name)
}
