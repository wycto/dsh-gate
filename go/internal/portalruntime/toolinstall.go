// 运行环境的「补齐」任务：管理台上缺什么工具，点一下就装。
//
// 与 dsh 的安装任务分开：那个跟着**实例**走（每个实例一份），这个跟着**工具**走
// ——pnpm 在一台机器上装一次，所有实例共用。
//
// 任务状态只在内存里，与 dsh 安装任务一致：它是「这次点击的进度」，重启网关后
// 环境检测会重新回答「到底有没有装上」，不需要持久化历史。
package portalruntime

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/portal"
)

// ToolInstallOptions 是补齐一个工具所需的事实，由装配处提供。
type ToolInstallOptions struct {
	// StateDir 是管理端状态目录：自备的 Node 与 pnpm 都放在它下面。
	StateDir string
	// Registry 是拉包用的 npm 源（留空表示 npm 默认）。
	Registry string
	// Logger 收日志；为 nil 时用 Runtime 自己的。
	Logger *slog.Logger
}

// toolJob 是一次工具补齐任务的共享状态。
type toolJob struct {
	mu  sync.Mutex
	job portal.ToolJob
}

func (t *toolJob) snapshot() portal.ToolJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.job
	out.Log = append([]string(nil), t.job.Log...)
	return out
}

func (t *toolJob) update(fn func(*portal.ToolJob)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.job)
}

func (t *toolJob) appendLog(line string) {
	t.update(func(j *portal.ToolJob) {
		j.Log = append(j.Log, line)
		if len(j.Log) > installLogLimit {
			j.Log = j.Log[len(j.Log)-installLogLimit:]
		}
	})
}

// toolJobs 按工具名保存任务，键就是工具名（"pnpm"）。
type toolJobs struct {
	mu   sync.Mutex
	jobs map[string]*toolJob
}

func (m *toolJobs) get(tool string) *toolJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[tool]
}

func (m *toolJobs) put(tool string, task *toolJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs == nil {
		m.jobs = map[string]*toolJob{}
	}
	m.jobs[tool] = task
}

// newToolJobs 造一张空的任务表。
func newToolJobs() *toolJobs { return &toolJobs{jobs: map[string]*toolJob{}} }

// StartToolInstall 起一次后台「补齐工具」任务并立即返回状态。
//
// 同一个工具已经在装时把当前状态给回去，不并排跑第二次：两个 npm install 抢
// 同一个前缀目录，结果只会是互相踩。
func (rt *Runtime) StartToolInstall(tool string, opts ToolInstallOptions) (portal.ToolJob, error) {
	if !portal.ToolInstallable(tool) {
		return portal.ToolJob{}, fmt.Errorf("不支持由管理端安装的工具 %q", tool)
	}
	if existing := rt.toolJobs.get(tool); existing != nil {
		snap := existing.snapshot()
		if snap.Active() {
			return snap, nil
		}
	}

	task := &toolJob{job: portal.ToolJob{
		Tool:      tool,
		Phase:     portal.InstallPhasePending,
		Step:      "已排队，等待开始",
		StartedAt: time.Now(),
	}}
	rt.toolJobs.put(tool, task)

	go rt.runToolInstall(tool, opts, task)
	return task.snapshot(), nil
}

// ToolInstallStatus 查某个工具的补齐任务状态。
func (rt *Runtime) ToolInstallStatus(tool string) (portal.ToolJob, bool) {
	task := rt.toolJobs.get(tool)
	if task == nil {
		return portal.ToolJob{}, false
	}
	return task.snapshot(), true
}

// runToolInstall 真正执行补齐。跑在独立 goroutine 里，状态写进 task。
//
// 用 rt.installCtx() 而不是 HTTP 请求的 context：请求在点击后立刻就返回了，
// 跟着它走的话安装会在第一个 yield 点被取消。这与 dsh 安装走的是同一条路。
func (rt *Runtime) runToolInstall(tool string, opts ToolInstallOptions, task *toolJob) {
	logger := opts.Logger
	if logger == nil {
		logger = rt.log
	}
	ctx := rt.installCtx()

	task.update(func(j *portal.ToolJob) {
		j.Phase = portal.InstallPhaseRunning
		j.Step = "正在检测运行环境"
	})
	env := dshproc.DetectEnv(ctx, opts.StateDir)
	task.appendLog("环境：" + envSummary(env))

	// 先备齐 Node：npm 在自备运行时里时，光看进程 PATH 会得出「找不到 npm」。
	nodeBin := env.SelfNodeBin
	if bin, err := dshproc.EnsureNodeRuntime(ctx, logger, opts.StateDir); err != nil {
		task.appendLog("自备 Node 运行时失败：" + err.Error())
	} else if bin != "" {
		nodeBin = bin
	}

	task.update(func(j *portal.ToolJob) { j.Step = "正在安装 pnpm（装到网关状态目录，不动系统）" })
	bin, err := dshproc.EnsurePnpmWithBin(ctx, logger, opts.StateDir, opts.Registry, nodeBin)
	if err != nil {
		task.appendLog("安装 pnpm 失败：" + err.Error())
		task.update(func(j *portal.ToolJob) {
			j.Phase = portal.InstallPhaseFailed
			j.Error = err.Error()
			j.FinishedAt = time.Now()
		})
		return
	}

	switch {
	case bin != "":
		task.appendLog("pnpm 已装到 " + bin)
	default:
		// EnsurePnpm 在系统里已经有 pnpm 时直接返回空串——那不是失败。
		task.appendLog("系统里已经有 pnpm，无需安装")
	}
	task.update(func(j *portal.ToolJob) {
		j.Phase = portal.InstallPhaseDone
		j.Step = "pnpm 已就绪"
		j.FinishedAt = time.Now()
	})
}
