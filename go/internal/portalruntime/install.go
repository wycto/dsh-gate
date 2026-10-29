// 实例的 dsh 安装编排。
//
// 管理台上「创建实例」不是一步就完的：选好安装方式之后要 clone/下载/构建，git
// 方式实测十几分钟。所以安装必须**后台跑 + 能看进度**，不能把 HTTP 请求挂在那
// 里等——浏览器早超时了，用户也不知道到底是在下载还是卡死了。
//
// 本文件负责：
//   - 把安装请求排进一个后台任务，返回可查询的 Job；
//   - 记录每个阶段（准备环境 / clone / 装依赖 / 构建 / 验证）的进度与输出尾部；
//   - 装好之后把启动命令写回实例记录（DshCmd），运行时据此拉起。
//
// 任务状态只放在内存里：管理端重启后「正在安装」的任务就没了，用户重进管理台
// 看到的是实例还在但没装好——那时他可以重新点安装。不落盘是有意的：一个跨越
// 重启的「安装中」状态没有任何东西能推进它，留着只会误导。
package portalruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/portal"
)

// InstallPhase 是安装所处的阶段。
type InstallPhase string

const (
	// InstallPending 表示已排队、还没开始。
	InstallPending InstallPhase = "pending"
	// InstallRunning 表示正在装。
	InstallRunning InstallPhase = "running"
	// InstallDone 表示装好了。
	InstallDone InstallPhase = "done"
	// InstallFailed 表示失败（错误在 Job.Error 里）。
	InstallFailed InstallPhase = "failed"
)

// InstallJob 是一次安装任务的可见状态（不可变快照）。
//
// 它**不含锁**：锁在内部的 installTask 里，对外只交出快照。早先的写法把
// sync.Mutex 嵌进这个结构，于是每次取快照都是「按值复制一个含锁的结构」
// （go vet 的 copylocks 会直接报出来）——复制出来的锁与原来的锁无关，看着
// 能用，实际上是个迟早出问题的陷阱。
type InstallJob struct {
	// InstanceID 是目标实例。
	InstanceID string
	// Mode 是安装方式。
	Mode dshproc.InstallMode
	// Phase 是当前阶段。
	Phase InstallPhase
	// Step 是当前步骤的人话描述（"正在 clone 源码" 这类）。
	Step string
	// Log 是各步骤的输出尾部（只留最近若干行，够用户判断卡在哪）。
	Log []string
	// Error 是失败原因（Phase 为 failed 时）。
	Error string
	// Command 是装好之后的启动命令（Phase 为 done 时）。
	Command string
	// Version 是装到的 dsh 版本。
	Version string
	// StartedAt / FinishedAt 是起止时间。
	StartedAt  time.Time
	FinishedAt time.Time
}

// Finished 表示任务已经结束（成功或失败）。
func (j InstallJob) Finished() bool {
	return j.Phase == InstallDone || j.Phase == InstallFailed
}

// Active 表示任务还在跑。
func (j InstallJob) Active() bool {
	return j.Phase == InstallPending || j.Phase == InstallRunning
}

// installTask 是运行中的安装任务：可变状态 + 保护它的锁。
type installTask struct {
	mu  sync.Mutex
	job InstallJob
}

// snapshot 取一份一致的状态快照。
func (t *installTask) snapshot() InstallJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := t.job
	out.Log = append([]string{}, t.job.Log...)
	return out
}

// update 在锁内改状态。
func (t *installTask) update(fn func(*InstallJob)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.job)
}

// appendLog 往日志里追加一行，只保留最近 installLogLimit 行。
func (t *installTask) appendLog(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.job.Log = append(t.job.Log, line)
	if len(t.job.Log) > installLogLimit {
		t.job.Log = t.job.Log[len(t.job.Log)-installLogLimit:]
	}
}

// installLogLimit 是保留的日志行数上限。
const installLogLimit = 60

// installJobs 是运行时的安装任务表。
type installJobs struct {
	mu   sync.Mutex
	jobs map[string]*installTask
}

func newInstallJobs() *installJobs { return &installJobs{jobs: map[string]*installTask{}} }

func (t *installJobs) put(job *installTask) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.jobs[job.job.InstanceID] = job
}

func (t *installJobs) get(instanceID string) *installTask {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.jobs[instanceID]
}

// InstallState 是「某实例的安装状态」的对外表示。
type InstallState struct {
	// Job 是当前或最近一次安装任务（没有则为 nil）。
	Job *InstallJob
	// Env 是本机运行环境检测报告。
	Env dshproc.EnvReport
	// Installed 表示实例已有可用的启动命令。
	Installed bool
	// Command 是实例记录里的启动命令（Installed 为真时有值）。
	Command string
}

// InstallOptions 是一次安装请求的选项。
type InstallOptions struct {
	// Mode 是安装方式。
	Mode dshproc.InstallMode
	// Dir 是安装目录（git 方式 clone 到 <Dir>/deepseek-harness，pnpm 装到 <Dir>/dsh）。
	Dir string
	// Registry 是 npm 源。
	Registry string
	// GitURL / GitRef 用于 git 方式。
	GitURL string
	GitRef string
	// Version 是要装的 dsh 版本或 dist-tag（npx / pnpm 方式）；空表示跟随 latest。
	Version string
	// StateDir 是管理端状态目录（用于找自备的 Node / pnpm）。
	StateDir string
	// Logger 收日志。
	Logger *slog.Logger
}

// StartInstall 为某实例启动一次后台安装，立即返回任务句柄。
//
// 同一个实例同时只允许一个安装任务：重复点击不该并排跑两次 clone 到同一个目录
// （那会互相踩）。已有任务还在跑时直接返回它。
func (rt *Runtime) StartInstall(inst portal.Instance, opts InstallOptions) (InstallJob, error) {
	if strings.TrimSpace(inst.ID) == "" {
		return InstallJob{}, errors.New("安装需要实例 id")
	}
	if !opts.Mode.Valid() {
		return InstallJob{}, fmt.Errorf("不认识的安装方式 %q", opts.Mode)
	}
	// 版本名在这里就验掉：往下走要先下载 Node、再跑几分钟的冒烟，最后才在
	// dshproc 里说「这个字符不允许」，用户白等一轮。
	if _, err := dshproc.ValidateDshVersion(opts.Version); err != nil {
		return InstallJob{}, err
	}

	// 同一个实例已经在装：直接把它当前的状态给回去，不并排跑第二次。
	if existing := rt.installs.get(inst.ID); existing != nil {
		snap := existing.snapshot()
		if snap.Active() {
			return snap, nil
		}
	}

	task := &installTask{job: InstallJob{
		InstanceID: inst.ID,
		Mode:       opts.Mode,
		Phase:      InstallPending,
		Step:       "已排队，等待开始",
		StartedAt:  time.Now(),
	}}
	rt.installs.put(task)

	go rt.runInstall(inst, opts, task)
	return task.snapshot(), nil
}

// runInstall 真正执行安装。跑在独立 goroutine 里，状态写进 job。
func (rt *Runtime) runInstall(inst portal.Instance, opts InstallOptions, job *installTask) {
	logger := opts.Logger
	if logger == nil {
		logger = rt.log
	}

	job.update(func(j *InstallJob) {
		j.Phase = InstallRunning
		j.Step = "正在检测运行环境"
	})

	// 环境检测：把本机有什么、缺什么写进日志，安装失败时用户一眼能看出是
	// 「本机缺 git」还是「网络拉不动」。
	env := dshproc.DetectEnv(rt.installCtx(), opts.StateDir)
	job.appendLog("环境：" + envSummary(env))

	// 先备齐 Node 与 pnpm（git / pnpm 方式都要用）。npx 方式也要 Node。
	nodeBin, pnpmBin := env.SelfNodeBin, env.SelfPnpmBin
	if bin, err := dshproc.EnsureNodeRuntime(rt.installCtx(), logger, opts.StateDir); err != nil {
		job.appendLog("自备 Node 运行时失败：" + err.Error())
	} else if bin != "" {
		nodeBin = bin
	}
	if opts.Mode == dshproc.InstallGit || opts.Mode == dshproc.InstallPnpm || opts.Mode == dshproc.InstallLocal {
		// 把自备 Node 的 bin 目录交下去：没有系统级 npm/pnpm 时，它们就在那里。
		// 本地源码方式同样要 pnpm——启动命令就是 `pnpm dsh web`。
		if bin, err := dshproc.EnsurePnpmWithBin(rt.installCtx(), logger, opts.StateDir, opts.Registry, nodeBin); err != nil {
			job.appendLog("自备 pnpm 失败：" + err.Error())
		} else if bin != "" {
			pnpmBin = bin
		}
	}

	job.update(func(j *InstallJob) { j.Step = "正在安装 dsh（" + opts.Mode.Label() + "）" })

	result, err := dshproc.InstallDSH(rt.installCtx(), logger, dshproc.InstallRequest{
		Mode:     opts.Mode,
		Dir:      opts.Dir,
		DshHome:  inst.DshHome,
		Registry: opts.Registry,
		GitURL:   opts.GitURL,
		GitRef:   opts.GitRef,
		Version:  opts.Version,
		NodeBin:  nodeBin,
		PnpmBin:  pnpmBin,
	})
	if err != nil {
		job.appendLog("安装失败：" + err.Error())
		job.update(func(j *InstallJob) {
			j.Phase = InstallFailed
			j.Step = "安装失败"
			j.Error = err.Error()
			j.FinishedAt = time.Now()
		})
		return
	}

	for _, line := range result.Log {
		job.appendLog(line)
	}

	// 把启动命令写回实例记录：运行时按它拉起。这一步失败要报出来——装好了
	// 但记录没更新，用户点「启动」还是起不来，比直接说失败更让人困惑。
	inst.DshCmd = result.Command
	inst.InstallMode = string(opts.Mode)
	inst.InstallDir = result.Dir
	// 版本回写：安装面板上临时改过版本时，记录要跟实际装的那一份一致——否则下次
	// 重装又把上一次钉住的旧值带回来，用户已经在页面上填过新的了。
	inst.DshVersion = opts.Version
	if err := rt.opts.Store.UpdateInstance(inst); err != nil {
		job.appendLog("写入实例记录失败：" + err.Error())
		job.update(func(j *InstallJob) {
			j.Phase = InstallFailed
			j.Step = "安装完成但写入记录失败"
			j.Error = "安装已完成，但把启动命令写进实例记录失败：" + err.Error()
			j.FinishedAt = time.Now()
		})
		return
	}

	job.appendLog("安装完成，启动命令：" + result.Command)
	job.update(func(j *InstallJob) {
		j.Phase = InstallDone
		j.Step = "安装完成"
		j.Command = result.Command
		j.Version = result.Version
		j.FinishedAt = time.Now()
	})
}

// installCtx 返回安装用的上下文：随运行时一起取消（管理端停掉时安装也停）。
func (rt *Runtime) installCtx() context.Context {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.baseCtx != nil {
		return rt.baseCtx
	}
	return context.Background()
}

// InstallStateFor 返回某实例的安装状态（任务 + 环境 + 是否已装好）。
func (rt *Runtime) InstallStateFor(inst portal.Instance, stateDir string) InstallState {
	state := InstallState{
		Env:       dshproc.DetectEnv(rt.installCtx(), stateDir),
		Installed: strings.TrimSpace(inst.DshCmd) != "",
		Command:   inst.DshCmd,
	}
	if job := rt.installs.get(inst.ID); job != nil {
		snap := job.snapshot()
		state.Job = &snap
	}
	return state
}

// envSummary 把环境检测压成一行，便于日志里一眼看清。
func envSummary(env dshproc.EnvReport) string {
	parts := make([]string, 0, len(env.Tools))
	for _, t := range env.Tools {
		if !t.Found {
			parts = append(parts, t.Name+"=缺失")
			continue
		}
		if t.Version != "" {
			parts = append(parts, t.Name+"="+t.Version)
			continue
		}
		parts = append(parts, t.Name+"=已装")
	}
	line := strings.Join(parts, " ")
	if env.SelfNodeBin != "" {
		line += " 自备Node=" + env.SelfNodeBin
	}
	if env.SelfPnpmBin != "" {
		line += " 自备pnpm=" + env.SelfPnpmBin
	}
	return line
}
