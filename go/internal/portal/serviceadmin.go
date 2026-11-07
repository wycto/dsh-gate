// 管理台的「服务」动作与渲染数据（内网服务代理，见 docs/service-proxy.md）。
//
// 服务与实例的页面完全分离（用户 2026-11-05 决议）：这里只供管理台独立的
// 「服务」分页使用，不与实例列表混排。动作走 admin.go 的同一套表单 / JSON API
// 通道（applyAdminAction 分派、runAdminAction 统一写审计），页面与脚本因此
// 必然一致。
//
// 与实例动作的一个本质区别：服务是「已经存在、只被登记进来的上游」，所以
// update / remove / toggle 成功后必须调 invalidateServiceMount 让挂载代理
// 重建——否则旧的上游地址 / Host 策略 / 证书豁免还会继续生效。
package portal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// applyServiceCreate 登记一条服务。名称与上游地址必填，其余字段交给
// Store.CreateService 校验与归一（NormalizeUpstream 的安全边界在那里）。
func (s *Server) applyServiceCreate(actor *User, act *adminAction) error {
	name := strings.TrimSpace(act.Name)
	upstream := strings.TrimSpace(act.Upstream)
	if name == "" || upstream == "" {
		return errors.New("missing name or upstream")
	}
	svc := &Service{
		Name:        name,
		Upstream:    upstream,
		HostHeader:  strings.TrimSpace(act.HostHeader),
		InsecureTLS: act.InsecureTLS,
		Note:        act.Note,
		Managed:     act.Managed,
		StartCmd:    act.StartCmd,
		StopCmd:     act.StopCmd,
		WorkDir:     act.WorkDir,
		Env:         act.Env,
		AutoStart:   act.AutoStart,
		HealthPath:  act.HealthPath,
	}
	if err := s.opts.Store.CreateService(svc); err != nil {
		return err
	}
	act.Name = svc.Name
	return nil
}

// requireService 按 id 取服务，并给出管理台需要的统一错误。
func (s *Server) requireService(act *adminAction) (*Service, error) {
	if strings.TrimSpace(act.ServiceID) == "" {
		return nil, errors.New("missing service_id")
	}
	return s.opts.Store.ServiceByID(act.ServiceID)
}

// applyServiceUpdate 覆盖服务的可编辑字段。
//
// 启用状态不在这里改（那是 service.toggle 的语义）；RouteMode / ListenPort 等
// P1 预留字段原样保留。改完成功必须让挂载缓存失效，否则代理还按旧配置转发。
func (s *Server) applyServiceUpdate(actor *User, act *adminAction) error {
	svc, err := s.requireService(act)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(act.Name)
	if name == "" {
		return errors.New("missing name")
	}
	// 改了受管服务的启动命令/目录/环境时，先停掉在跑的旧进程，避免它按旧配置
	// 继续跑成孤儿（新配置只在下次启动生效）。
	if svc.Managed && s.serviceRunStatus(svc.ID).Running {
		if err := s.stopServiceProc(svc); err != nil {
			return err
		}
	}
	svc.Name = name
	svc.Upstream = strings.TrimSpace(act.Upstream)
	svc.HostHeader = strings.TrimSpace(act.HostHeader)
	svc.InsecureTLS = act.InsecureTLS
	svc.Note = act.Note
	svc.Managed = act.Managed
	svc.StartCmd = act.StartCmd
	svc.StopCmd = act.StopCmd
	svc.WorkDir = act.WorkDir
	svc.Env = act.Env
	svc.AutoStart = act.AutoStart
	svc.HealthPath = act.HealthPath
	if err := s.opts.Store.UpdateService(*svc); err != nil {
		return err
	}
	s.invalidateServiceMount(svc.ID)
	act.Name = svc.Name
	return nil
}

// applyServiceRemove 删除服务登记（授权随之级联删除）。服务进程不归门户管，
// 删的只是登记——这一点在页面的删除面板里写明。
func (s *Server) applyServiceRemove(act *adminAction) error {
	svc, err := s.requireService(act)
	if err != nil {
		return err
	}
	// 删登记前先停掉受管进程，避免留下孤儿进程占端口。
	if svc.Managed && s.serviceRunStatus(svc.ID).Running {
		if err := s.stopServiceProc(svc); err != nil {
			return err
		}
	}
	if err := s.opts.Store.DeleteService(svc.ID); err != nil {
		return err
	}
	s.invalidateServiceMount(svc.ID)
	// 审计与回跳提示都用服务名：表单里带了，但 JSON 调用方可能只给 id。
	if strings.TrimSpace(act.Name) == "" {
		act.Name = svc.Name
	}
	return nil
}

// applyServiceToggle 翻转服务的启用状态。停用只影响代理入口（挂载侧按
// Enabled 判定），不动服务本身。
func (s *Server) applyServiceToggle(act *adminAction) error {
	svc, err := s.requireService(act)
	if err != nil {
		return err
	}
	// 由启用切到停用时，先停掉受管进程（停用语义是「不要它对外可用」，进程也该停）。
	if svc.Enabled && svc.Managed && s.serviceRunStatus(svc.ID).Running {
		if err := s.stopServiceProc(svc); err != nil {
			return err
		}
	}
	svc.Enabled = !svc.Enabled
	if err := s.opts.Store.UpdateService(*svc); err != nil {
		return err
	}
	s.invalidateServiceMount(svc.ID)
	if strings.TrimSpace(act.Name) == "" {
		act.Name = svc.Name
	}
	return nil
}

// applyServiceProbe 按需探活一次并把结果缓存起来。
//
// 探活失败（上游不可达）不算动作失败：动作的语义是「探一次并报告」，结果写进
// 审计 detail 与缓存徽标。用 Background 而不是请求 ctx：浏览器提前断开不该
// 腰斩一次用户明确点下的探测；超时由 probeService 自己控制。
func (s *Server) applyServiceProbe(act *adminAction) error {
	svc, err := s.requireService(act)
	if err != nil {
		return err
	}
	probe := s.probeService(context.Background(), *svc)
	s.storeServiceProbe(svc.ID, probe)
	detail := fmt.Sprintf("reachable=%t", probe.Reachable)
	if probe.Status > 0 {
		detail += fmt.Sprintf(" status=%d", probe.Status)
	}
	detail += fmt.Sprintf(" latency=%dms", probe.LatencyMS)
	if probe.Error != "" {
		detail += " error=" + probe.Error
	}
	// 输出字段：runAdminAction 统一写审计时带上它（请求方伪造不了，json:"-"）。
	act.AuditDetail = detail
	if strings.TrimSpace(act.Name) == "" {
		act.Name = svc.Name
	}
	return nil
}

// applyServiceGrantsSave 把某个用户的服务授权集合整体设为表单勾选的那些。
func (s *Server) applyServiceGrantsSave(actor *User, act *adminAction) error {
	if strings.TrimSpace(act.Name) == "" {
		return errors.New("missing name")
	}
	return s.opts.Store.SetUserServiceGrants(act.Name, act.Services, actor.Name)
}

// serviceRunStatus 读受管服务的运行状态；未接运行时（nil）时返回零值。
func (s *Server) serviceRunStatus(id string) ServiceRunStatus {
	if s.opts.ServiceLifespan == nil {
		return ServiceRunStatus{}
	}
	return s.opts.ServiceLifespan.Status(id)
}

// stopServiceProc 停掉受管服务的进程（未接运行时直接报错，避免静默「以为停了」）。
func (s *Server) stopServiceProc(svc *Service) error {
	if s.opts.ServiceLifespan == nil {
		return errors.New("未接入服务运行时，无法停止受管进程")
	}
	if err := s.opts.ServiceLifespan.StopService(svc.ID); err != nil {
		return fmt.Errorf("停止服务 %q 失败: %w", svc.Name, err)
	}
	return nil
}

// requireManagedService 取服务并确认它是受管进程、且运行时已接入。
func (s *Server) requireManagedService(act *adminAction) (*Service, error) {
	svc, err := s.requireService(act)
	if err != nil {
		return nil, err
	}
	if !svc.Managed {
		return nil, errors.New("纯代理服务不支持启停（只有受管服务才由网关拉起进程）")
	}
	if s.opts.ServiceLifespan == nil {
		return nil, errors.New("未接入服务运行时")
	}
	return svc, nil
}

// applyServiceStart 启动受管服务。
func (s *Server) applyServiceStart(act *adminAction) error {
	svc, err := s.requireManagedService(act)
	if err != nil {
		return err
	}
	if err := s.opts.ServiceLifespan.StartService(*svc); err != nil {
		return err
	}
	s.invalidateServiceMount(svc.ID)
	if strings.TrimSpace(act.Name) == "" {
		act.Name = svc.Name
	}
	return nil
}

// applyServiceStop 停止受管服务。
func (s *Server) applyServiceStop(act *adminAction) error {
	svc, err := s.requireManagedService(act)
	if err != nil {
		return err
	}
	if err := s.opts.ServiceLifespan.StopService(svc.ID); err != nil {
		return err
	}
	s.invalidateServiceMount(svc.ID)
	if strings.TrimSpace(act.Name) == "" {
		act.Name = svc.Name
	}
	return nil
}

// applyServiceRestart 重启受管服务。
func (s *Server) applyServiceRestart(act *adminAction) error {
	svc, err := s.requireManagedService(act)
	if err != nil {
		return err
	}
	if err := s.opts.ServiceLifespan.RestartService(*svc); err != nil {
		return err
	}
	s.invalidateServiceMount(svc.ID)
	if strings.TrimSpace(act.Name) == "" {
		act.Name = svc.Name
	}
	return nil
}

// ---- 渲染数据（管理台「服务」分页）----

// adminServiceRow 是一张服务卡片。探活徽标只读缓存（cachedServiceProbe），
// 渲染时绝不真探活——否则打开一次管理台就要等所有上游超时。
type adminServiceRow struct {
	ID              string
	Name            string
	Upstream        string
	HostHeaderLabel string
	// HostOptions 是该服务自己的 Host 策略下拉项。与实例安装方式下拉同一个坑：
	// 所有行共用一份切片时选中态会互相覆盖，必须每行生成新的。
	HostOptions []adminModeOption
	InsecureTLS bool
	Note        string
	Enabled     bool
	StatusLabel string
	Href        string
	ProbeLabel  string
	ProbeClass  string
	// ---- 受管服务（P1）----
	Managed    bool
	Running    bool
	RunLabel   string
	RunClass   string
	StartCmd   string
	StopCmd    string
	WorkDir    string
	Env        string
	AutoStart  bool
	HealthPath string
	// Logs 是最近日志的纯文本（受管服务才填，用于卡片里的 details 展开）。
	Logs string
}

// adminServiceGrantRow 是服务授权矩阵里的一行（一个用户 × 全部服务）。
type adminServiceGrantRow struct {
	Name    string
	Options []adminInstanceOption
}

// serviceHostOptions 生成 Host 策略下拉项，并把 current 那一项标成选中。
func serviceHostOptions(lang i18n.Lang, current string) []adminModeOption {
	opts := []adminModeOption{
		{Value: HostHeaderUpstream, Label: i18n.T(lang, i18n.KeyAdminSvcHostUpstream)},
		{Value: HostHeaderGateway, Label: i18n.T(lang, i18n.KeyAdminSvcHostGateway)},
	}
	if current != HostHeaderGateway {
		current = HostHeaderUpstream
	}
	for i := range opts {
		opts[i].Selected = opts[i].Value == current
	}
	return opts
}

// serviceHostLabel 是卡片上 Host 策略的纯文本。
func serviceHostLabel(lang i18n.Lang, current string) string {
	if current == HostHeaderGateway {
		return i18n.T(lang, i18n.KeyAdminSvcHostGateway)
	}
	return i18n.T(lang, i18n.KeyAdminSvcHostUpstream)
}

// serviceProbeLabel 把探活缓存翻成徽标文案与颜色（on / off / 空）。
// 页面渲染与自动刷新端点共用这一份口径，两边不会各说一套。
func serviceProbeLabel(lang i18n.Lang, probe ServiceProbe, ok bool) (string, string) {
	if !ok {
		return i18n.T(lang, i18n.KeyAdminSvcProbeNone), ""
	}
	if probe.Reachable {
		return i18n.T(lang, i18n.KeyAdminSvcProbeOK), "on"
	}
	return i18n.T(lang, i18n.KeyAdminSvcProbeFail), "off"
}

// buildServiceRows 把服务列表翻成卡片行。
func (s *Server) buildServiceRows(lang i18n.Lang, services []Service) []adminServiceRow {
	rows := make([]adminServiceRow, 0, len(services))
	for _, svc := range services {
		label := i18n.T(lang, i18n.KeyAdminStatusEnabled)
		if !svc.Enabled {
			label = i18n.T(lang, i18n.KeyAdminStatusDisabled)
		}
		probe, ok := s.cachedServiceProbe(svc.ID)
		probeLabel, probeClass := serviceProbeLabel(lang, probe, ok)
		runLabel, runClass, runLogs := "", "", ""
		if svc.Managed {
			st := s.serviceRunStatus(svc.ID)
			runLabel, runClass = i18n.T(lang, i18n.KeyAdminSvcStopped), "off"
			if st.Running {
				runLabel, runClass = i18n.T(lang, i18n.KeyAdminSvcRunning), "on"
			}
			if s.opts.ServiceLifespan != nil {
				if l, err := s.opts.ServiceLifespan.Logs(svc.ID, 100); err == nil {
					runLogs = l
				}
			}
		}
		rows = append(rows, adminServiceRow{
			ID:              svc.ID,
			Name:            svc.Name,
			Upstream:        svc.Upstream,
			HostHeaderLabel: serviceHostLabel(lang, svc.HostHeader),
			HostOptions:     serviceHostOptions(lang, svc.HostHeader),
			InsecureTLS:     svc.InsecureTLS,
			Note:            strings.TrimSpace(svc.Note),
			Enabled:         svc.Enabled,
			StatusLabel:     label,
			Href:            ServiceMountPrefix + svc.ID + "/",
			ProbeLabel:      probeLabel,
			ProbeClass:      probeClass,
			Managed:         svc.Managed,
			Running:         svc.Managed && s.serviceRunStatus(svc.ID).Running,
			RunLabel:        runLabel,
			RunClass:        runClass,
			StartCmd:        svc.StartCmd,
			StopCmd:         svc.StopCmd,
			WorkDir:         svc.WorkDir,
			Env:             svc.Env,
			AutoStart:       svc.AutoStart,
			HealthPath:      svc.HealthPath,
			Logs:            runLogs,
		})
	}
	return rows
}

// buildServiceGrantRows 生成服务授权矩阵：每个用户一行，勾选项来自
// ServicesForUser（与实例授权矩阵同一做法）。
func (s *Server) buildServiceGrantRows(users []User, services []Service) ([]adminServiceGrantRow, error) {
	options := make([]adminInstanceOption, 0, len(services))
	for _, svc := range services {
		options = append(options, adminInstanceOption{ID: svc.ID, Name: svc.Name})
	}
	rows := make([]adminServiceGrantRow, 0, len(users))
	for _, u := range users {
		granted := map[string]bool{}
		visible, err := s.opts.Store.ServicesForUser(u.ID)
		if err != nil {
			return nil, err
		}
		for _, svc := range visible {
			granted[svc.ID] = true
		}
		opts := make([]adminInstanceOption, 0, len(options))
		for _, opt := range options {
			opts = append(opts, adminInstanceOption{ID: opt.ID, Name: opt.Name, Granted: granted[opt.ID]})
		}
		rows = append(rows, adminServiceGrantRow{Name: u.Name, Options: opts})
	}
	return rows, nil
}
