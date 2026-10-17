// 管理台的实例安装面板：环境检测、三种安装方式、进度查看。
//
// 与实例登记（instances.go）分开是因为它们解决的是不同问题：instances.go 管
// 「有哪些实例、谁有权访问」，这里管「这个实例的 dsh 怎么装起来、装到哪一步」。
//
// 管理台包不 import portalruntime（那会成环：运行时要用实例记录）。所以这里定义
// 一个窄接口 Installer，由 portalruntime 实现，管理端在装配处注入。接口只暴露
// 页面真正需要的三件事：检测环境、起一次安装、查进度——运行时的其他能力不进这里。
package portal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// InstallMode 是 dsh 的安装方式（管理台选项）。
type InstallMode string

const (
	// InstallModeNpx 是官方 npx 命令方式。
	InstallModeNpx InstallMode = "npx"
	// InstallModeGit 是从源码 clone 构建方式。
	InstallModeGit InstallMode = "git"
	// InstallModePnpm 是 pnpm 安装方式。
	InstallModePnpm InstallMode = "pnpm"
)

// ParseInstallMode 解析安装方式文本；空串按默认（npx）。
func ParseInstallMode(text string) (InstallMode, error) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "", string(InstallModeNpx):
		return InstallModeNpx, nil
	case string(InstallModeGit):
		return InstallModeGit, nil
	case string(InstallModePnpm):
		return InstallModePnpm, nil
	default:
		return "", fmt.Errorf("不认识的安装方式 %q（可选：npx、git、pnpm）", text)
	}
}

// Valid 表示这是不是一个已支持的安装方式。
func (m InstallMode) Valid() bool {
	switch m {
	case InstallModeNpx, InstallModeGit, InstallModePnpm:
		return true
	}
	return false
}

// Label 返回给人看的名字。
func (m InstallMode) Label() string {
	switch m {
	case InstallModeGit:
		return "从源码构建"
	case InstallModePnpm:
		return "pnpm 安装"
	default:
		return "npx 官方命令"
	}
}

// ToolStatus 是一个工具的检测结果（管理台展示用）。
type ToolStatus struct {
	Name    string
	Found   bool
	Path    string
	Version string
	// Hint 是这个工具缺失时该怎么办（缺了才有值）。
	Hint string
}

// EnvStatus 是本机 dsh 运行环境的检测结果。
type EnvStatus struct {
	Tools []ToolStatus
	// NodeBin / PnpmBin 是管理端自备的运行时目录（空表示用系统的）。
	NodeBin string
	PnpmBin string
	// ExistingDsh 是本机已有的 dsh（空表示没有）。
	ExistingDsh string
	// Platform 是 GOOS/GOARCH。
	Platform string
}

// Tool 按名字取工具状态。
func (e EnvStatus) Tool(name string) ToolStatus {
	for _, t := range e.Tools {
		if t.Name == name {
			return t
		}
	}
	return ToolStatus{Name: name}
}

// CanNpx / CanGit / CanPnpm 表示对应安装方式当前能不能用。
func (e EnvStatus) CanNpx() bool  { return e.Tool("node").Found }
func (e EnvStatus) CanGit() bool  { return e.Tool("node").Found && e.Tool("git").Found }
func (e EnvStatus) CanPnpm() bool { return e.Tool("node").Found || e.NodeBin != "" }

// InstallPhase 是安装所处的阶段。
type InstallPhase string

const (
	InstallPhasePending InstallPhase = "pending"
	InstallPhaseRunning InstallPhase = "running"
	InstallPhaseDone    InstallPhase = "done"
	InstallPhaseFailed  InstallPhase = "failed"
)

// JobStatus 是一次安装任务的可见状态。
type JobStatus struct {
	InstanceID string
	Mode       InstallMode
	Phase      InstallPhase
	Step       string
	Log        []string
	Error      string
	Command    string
	Version    string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Finished 表示任务已经结束（成功或失败）。
func (j JobStatus) Finished() bool {
	return j.Phase == InstallPhaseDone || j.Phase == InstallPhaseFailed
}

// Active 表示任务还在跑。
func (j JobStatus) Active() bool {
	return j.Phase == InstallPhasePending || j.Phase == InstallPhaseRunning
}

// InstallRequest 是一次安装请求（管理台 → 运行时）。
type InstallRequest struct {
	// InstanceID 是目标实例。
	InstanceID string
	// Mode 是安装方式。
	Mode InstallMode
	// Dir 是安装目录。
	Dir string
	// Registry 是 npm 源。
	Registry string
	// GitURL / GitRef 用于 git 方式。
	GitURL string
	GitRef string
}

// ToolJob 是一次「补齐运行环境」后台任务的状态。
//
// 与 dsh 的 JobStatus 分开：那个跟着**实例**走（每个实例一份），这个跟着**工具**走
// ——pnpm 在一台机器上装一次，所有实例共用，所以它不该挂在某个实例下面。
type ToolJob struct {
	Tool       string
	Phase      InstallPhase
	Step       string
	Log        []string
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Active 表示任务还没结束。
func (j ToolJob) Active() bool {
	return j.Phase == InstallPhasePending || j.Phase == InstallPhaseRunning
}

// ToolInstallable 报告某个工具能不能由管理端直接补齐。
//
// 只放 pnpm：dsh 的插件安装器走的是它，而 Node 只自带 npm/npx——干净的服务器上
// 点「添加插件」会报 `spawn pnpm ENOENT`。其余几个刻意不给按钮：
//   - node / npm / npx：网关在准备 dsh 时会自己下一份到状态目录，不需要用户点；
//   - git：得动系统包管理器，发行版差异太大（apt/dnf/apk/yum），给命令让用户自己执行更稳妥；
//   - dsh 本身：走下面按实例的三种安装方式，那是有进度可看的长任务。
func ToolInstallable(name string) bool { return name == "pnpm" }

// Installer 是管理台需要的安装能力。由 portalruntime 实现并在装配处注入。
//
// 这些方法都只读或启动后台任务，没有一个会阻塞到安装结束——git 方式要构建十几
// 分钟，HTTP 请求等不起。
type Installer interface {
	// DetectEnv 检测本机运行环境。
	DetectEnv(ctx context.Context) EnvStatus
	// StartInstall 起一次后台安装并立即返回任务状态。
	StartInstall(ctx context.Context, inst Instance, req InstallRequest) (JobStatus, error)
	// InstallStatus 查某实例的安装状态（没有任务时 Job 为零值）。
	InstallStatus(ctx context.Context, inst Instance) (JobStatus, bool)
	// StartToolInstall 起一次后台「补齐工具」任务（目前只有 pnpm）。
	StartToolInstall(ctx context.Context, tool string) (ToolJob, error)
	// ToolInstallStatus 查某个工具的补齐任务（没有任务时 ok 为 false）。
	ToolInstallStatus(ctx context.Context, tool string) (ToolJob, bool)
}

// ErrInstallerUnavailable 表示管理端没接安装能力（例如单实例模式）。
var ErrInstallerUnavailable = errors.New("当前没有可用的 dsh 安装能力")
