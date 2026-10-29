// portalruntime 侧对 portal.Installer 接口的实现。
//
// 管理台包不能 import 本包（会成环），所以它在自己那边定义了一个窄接口，由这里
// 实现、在装配处注入。这层适配只做三件事：把检测结果翻译成管理台的类型、把安装
// 请求翻译成 dshproc 的请求、把任务状态翻译回管理台的类型。真正的逻辑都在
// install.go 与 dshproc 里，这里不重复实现。
package portalruntime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/portal"
)

// InstallerAdapter 让 Runtime 满足 portal.Installer。
//
// StateDir 是管理端状态目录（用于发现自备的 Node / pnpm）；Registry 是默认 npm 源。
type InstallerAdapter struct {
	Runtime  *Runtime
	StateDir string
	Registry string
	// Logger 收补齐任务的日志；为 nil 时用 Runtime 自己的。
	Logger *slog.Logger
}

// DetectEnv 检测本机运行环境。
func (a *InstallerAdapter) DetectEnv(ctx context.Context) portal.EnvStatus {
	if a == nil || a.Runtime == nil {
		return portal.EnvStatus{}
	}
	env := dshproc.DetectEnv(ctx, a.StateDir)
	return toEnvStatus(env)
}

// StartInstall 起一次后台安装。
func (a *InstallerAdapter) StartInstall(ctx context.Context, inst portal.Instance, req portal.InstallRequest) (portal.JobStatus, error) {
	if a == nil || a.Runtime == nil {
		return portal.JobStatus{}, portal.ErrInstallerUnavailable
	}
	mode := dshproc.InstallMode(req.Mode)
	if !mode.Valid() {
		return portal.JobStatus{}, fmt.Errorf("不认识的安装方式 %q", req.Mode)
	}
	snapshot, err := a.Runtime.StartInstall(inst, InstallOptions{
		Mode:     mode,
		Dir:      req.Dir,
		Registry: a.Registry,
		GitURL:   req.GitURL,
		GitRef:   req.GitRef,
		// 请求里没带版本就用实例上存的那个：页面上的「安装/重装」按钮不重填版本，
		// 但用户先前钉过的那一个必须照旧生效。
		Version:  installVersion(req.Version, inst.DshVersion),
		StateDir: a.StateDir,
	})
	if err != nil {
		return portal.JobStatus{}, err
	}
	return toJobStatus(snapshot), nil
}

// InstallStatus 查某实例的安装状态。
func (a *InstallerAdapter) InstallStatus(ctx context.Context, inst portal.Instance) (portal.JobStatus, bool) {
	if a == nil || a.Runtime == nil {
		return portal.JobStatus{}, false
	}
	task := a.Runtime.installs.get(inst.ID)
	if task == nil {
		return portal.JobStatus{}, false
	}
	return toJobStatus(task.snapshot()), true
}

// StartToolInstall 起一次后台「补齐工具」任务（目前只有 pnpm）。
func (a *InstallerAdapter) StartToolInstall(ctx context.Context, tool string) (portal.ToolJob, error) {
	if a == nil || a.Runtime == nil {
		return portal.ToolJob{}, portal.ErrInstallerUnavailable
	}
	return a.Runtime.StartToolInstall(tool, ToolInstallOptions{
		StateDir: a.StateDir,
		Registry: a.Registry,
		Logger:   a.Logger,
	})
}

// ToolInstallStatus 查某个工具的补齐任务。
func (a *InstallerAdapter) ToolInstallStatus(ctx context.Context, tool string) (portal.ToolJob, bool) {
	if a == nil || a.Runtime == nil {
		return portal.ToolJob{}, false
	}
	return a.Runtime.ToolInstallStatus(tool)
}

// installVersion 决定这次安装用哪个版本：请求里点名优先，其次实例上存的，都空
// 则跟随 latest。
func installVersion(requested, stored string) string {
	if value := strings.TrimSpace(requested); value != "" {
		return value
	}
	return strings.TrimSpace(stored)
}

// toEnvStatus 把 dshproc 的检测报告翻成管理台类型。
func toEnvStatus(env dshproc.EnvReport) portal.EnvStatus {
	out := portal.EnvStatus{
		NodeBin:     env.SelfNodeBin,
		PnpmBin:     env.SelfPnpmBin,
		ExistingDsh: env.ExistingDsh,
		Platform:    env.Platform,
	}
	for _, t := range env.Tools {
		status := portal.ToolStatus{
			Name:    t.Name,
			Found:   t.Found,
			Path:    t.Path,
			Version: t.Version,
		}
		if !t.Found {
			status.Hint = toolHint(t.Name)
		}
		out.Tools = append(out.Tools, status)
	}
	return out
}

// toolHint 给出某个工具缺失时的补救指引。
func toolHint(name string) string {
	switch name {
	case "node":
		return "装 Node.js（https://nodejs.org/），或在管理端启动时让它自备运行时"
	case "git":
		return "装 git（https://git-scm.com/，或 apt install git / brew install git）"
	case "pnpm":
		return "npm install -g pnpm；npx 方式不需要它"
	case "npm", "npx":
		return "随 Node.js 一起装"
	case "dsh":
		return "还没有装 dsh，可在下面选一种安装方式"
	}
	return ""
}

// toJobStatus 把运行时任务翻成管理台类型。
func toJobStatus(job InstallJob) portal.JobStatus {
	return portal.JobStatus{
		InstanceID: job.InstanceID,
		Mode:       portal.InstallMode(job.Mode),
		Phase:      portal.InstallPhase(job.Phase),
		Step:       job.Step,
		Log:        job.Log,
		Error:      job.Error,
		Command:    job.Command,
		Version:    job.Version,
		StartedAt:  job.StartedAt,
		FinishedAt: job.FinishedAt,
	}
}

// 编译期断言：适配器必须满足管理台接口。
var _ portal.Installer = (*InstallerAdapter)(nil)

// LifespanAdapter 让 Runtime 满足 portal.Lifespan（管理台的启动 / 停止 / 重启）。
//
// 单独一个适配器而不是给 Runtime 加方法：Runtime 的方法收的是 portal.Instance，
// 与 portal.Lifespan 的签名恰好一致，但把它直接当接口用会让「管理台需要什么」
// 与「运行时能做什么」永久绑死。中间隔一层，将来管理台要的启停语义变了（比如
// 加超时参数）不必动运行时。
type LifespanAdapter struct {
	Runtime *Runtime
}

// InstanceStatus 查某实例当前的运行状态。
func (a *LifespanAdapter) InstanceStatus(inst portal.Instance) portal.InstanceStatus {
	st := a.Runtime.Status(inst.ID)
	return portal.InstanceStatus{
		Running: st.Running,
		Ready:   st.Ready,
		PID:     st.PID,
		Managed: st.Managed,
		Error:   st.Error,
	}
}

// StartInstance 启动单个实例。
func (a *LifespanAdapter) StartInstance(ctx context.Context, inst portal.Instance) error {
	return a.Runtime.StartInstance(ctx, inst)
}

// StopInstance 停止单个实例。
func (a *LifespanAdapter) StopInstance(ctx context.Context, inst portal.Instance) error {
	return a.Runtime.StopInstance(ctx, inst.ID)
}

// RestartInstance 用最新配置重启单个实例。
func (a *LifespanAdapter) RestartInstance(ctx context.Context, inst portal.Instance) error {
	return a.Runtime.RestartInstance(ctx, inst)
}

// 编译期断言：适配器必须满足管理台接口。
var _ portal.Lifespan = (*LifespanAdapter)(nil)
