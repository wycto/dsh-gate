// 受管服务进程监督器（internal/svclife）到 portal.ServiceLifespan 的适配层。
//
// 与 LifespanAdapter（实例）同一思路：portal 只依赖窄接口，真正的进程监督在
// svclife 包里；这里做 portal.Service <-> svclife.Spec/Status 的翻译。
package portalruntime

import (
	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/svclife"
)

// ServiceLifespanAdapter 让 svclife.Supervisor 满足 portal.ServiceLifespan。
type ServiceLifespanAdapter struct {
	Sup *svclife.Supervisor
}

// ServiceSpec 把门户的服务记录翻译成监督器的启动规格。
func ServiceSpec(svc portal.Service) svclife.Spec {
	return svclife.Spec{
		ID:         svc.ID,
		Name:       svc.Name,
		StartCmd:   svc.StartCmd,
		StopCmd:    svc.StopCmd,
		WorkDir:    svc.WorkDir,
		Env:        svc.Env,
		Upstream:   svc.Upstream,
		HealthPath: svc.HealthPath,
		AutoStart:  svc.AutoStart,
	}
}

// StartService 启动一条受管服务。
func (a *ServiceLifespanAdapter) StartService(svc portal.Service) error {
	return a.Sup.Start(ServiceSpec(svc))
}

// StopService 停止一条受管服务（按 id）。
func (a *ServiceLifespanAdapter) StopService(id string) error {
	return a.Sup.Stop(id)
}

// RestartService 用最新配置重启一条受管服务。
func (a *ServiceLifespanAdapter) RestartService(svc portal.Service) error {
	return a.Sup.Restart(ServiceSpec(svc))
}

// Status 查询一条受管服务的运行态。
func (a *ServiceLifespanAdapter) Status(id string) portal.ServiceRunStatus {
	st := a.Sup.Status(id)
	return portal.ServiceRunStatus{
		Running: st.Running, PID: st.PID, State: st.State,
		StartedAt: st.StartedAt, LastError: st.LastError,
	}
}

// Logs 读取最近 tail 行日志。
func (a *ServiceLifespanAdapter) Logs(id string, tail int) (string, error) {
	return a.Sup.Logs(id, tail)
}

// 编译期断言：适配器必须满足门户的受管服务生命周期接口。
var _ portal.ServiceLifespan = (*ServiceLifespanAdapter)(nil)
