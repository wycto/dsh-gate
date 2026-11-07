// 管理端的内网服务与授权（见 docs/service-proxy.md）。
//
// 服务与实例的本质区别：实例是「门户拉起的 dsh 进程」，服务是「已经存在、
// 只被登记进来的上游」——门户不拉起、不守护服务的进程，只做代理与探活。
// 授权与实例同一口径：service_grants 多对多表，管理员天然可见所有服务，
// 未命中授权时按「不存在」同形回答（不泄漏存在性）。
package portal

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ServiceIDPattern 是对外路径里服务 id 的白名单（与实例同一套形状）。
// 白名单先于查表执行，路径穿越与奇怪字符在这里就被挡掉。
var ServiceIDPattern = regexp.MustCompile(`^[a-z0-9]{4,64}$`)

// serviceNamePattern 是服务展示名的白名单：与实例名同一口径（允许中文，
// 挡空白/分隔符/控制字符与开头的 - .），理由见 instanceNamePattern 的注释。
var serviceNamePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._-]{0,63}$`)

// envKeyPattern 校验环境变量名：字母或下划线开头，后接字母数字下划线。
var envKeyPattern = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")

// ValidateServiceName 校验服务展示名。
func ValidateServiceName(name string) error {
	if !serviceNamePattern.MatchString(name) {
		return fmt.Errorf("服务名 %q 非法：只允许字母（含中文）、数字与 . _ -，须以字母或数字开头，最长 64 个字符", name)
	}
	return nil
}

// Host 头策略的取值。
const (
	// HostHeaderUpstream 表示转发时 Host 用上游自己的主机名（内网域名服务默认这样，
	// 上游的虚拟主机路由与证书校验都按它来）。
	HostHeaderUpstream = "upstream"
	// HostHeaderGateway 表示 Host 原样保留浏览器看到的门户 authority
	// （少数按请求 Host 生成链接的上游需要）。
	HostHeaderGateway = "gateway"
)

// RouteModePath 是路由模式：P0 只实现 path（子路径挂载）；port / subdomain 预留。
const RouteModePath = "path"

// 服务来源的取值。
const (
	ServiceSourceManual   = "manual"
	ServiceSourceDiscover = "discover"
	ServiceSourceImport   = "import"
)

// Service 是一条被登记的内网服务。
type Service struct {
	// ID 是不透明短 id（对外路径 /s/<id>/ 用），Name 是给人看的名字。
	ID   string
	Name string
	// Upstream 是上游地址（http(s)://host[:port][/base]），落库前经
	// NormalizeUpstream 归一：只允许 http/https、必须有主机名、不带查询与碎片。
	Upstream string
	// HostHeader 是转发时的 Host 策略（upstream / gateway），空视为 upstream。
	HostHeader string
	// InsecureTLS 为真时跳过上游 TLS 证书校验（自签内网服务）；默认校验。
	InsecureTLS bool
	// RouteMode 是路由模式；P0 只支持 path（子路径挂载）。
	RouteMode string
	// ListenPort 是 port 模式的对外端口（P1 预留，P0 恒 0）。
	ListenPort int
	// Note 是备注；TestAccount 是测试账号（P1 上界面，列先预留；明文落 0600 库）。
	Note        string
	TestAccount string
	// HealthIntervalS 是周期健康检查秒数（P1 预留，0 = 只按需探活）。
	HealthIntervalS int
	Enabled         bool
	// Source 是登记来源（manual / discover / import），空视为 manual。
	Source string
	// ---- 受管进程生命周期（文档第 6/7 节；纯代理服务这些字段恒为空）----
	// Managed 为真表示这是「受管进程」服务：由网关按 StartCmd 拉起/守护、可停止与重启；
	// false 为纯代理型（默认），行为与之前完全一致。
	Managed bool
	// StartCmd 是启动命令（经 sh -c / cmd /c 执行）；Managed 为真时必填。
	StartCmd string
	// StopCmd 是可选的自定义停止命令；空则按进程组 SIGTERM→SIGKILL。
	StopCmd string
	// WorkDir 是子进程工作目录（空=网关当前目录）。
	WorkDir string
	// Env 是追加给子进程的环境变量，每行 KEY=VALUE（允许空行）。
	Env string
	// AutoStart 为真表示网关启动时自动拉起该受管服务。
	AutoStart bool
	// HealthPath 是就绪探测路径（GET 该路径 2xx/3xx 算就绪）；空则探测 Upstream 端口。
	HealthPath string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NormalizeUpstream 校验并归一上游地址。
//
// 规则（安全边界，宁严勿松）：
//   - 只允许 http / https；主机名必须非空；
//   - 不允许 userinfo（http://user:pass@host 会把口令带进代理日志与页面）；
//   - 不允许查询串与碎片（它们不是上游地址的一部分）；
//   - 路径部分作为上游 base 保留，去掉尾斜杠（空路径归一为空串）；
//   - 显式端口必须是 1-65535 的整数。
func NormalizeUpstream(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("上游地址不能为空")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", fmt.Errorf("上游地址 %q 含空白字符", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("上游地址 %q 不是合法 URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("上游地址 %q 只支持 http 或 https", raw)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("上游地址 %q 缺少主机名", raw)
	}
	if u.User != nil {
		return "", errors.New("上游地址不允许携带用户名或口令")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", errors.New("上游地址不允许携带查询串")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("上游地址不允许携带碎片（#）")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("上游地址 %q 的端口非法", raw)
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

// applyDefaults 补齐空字段的默认值（与实例同一思路：展示、CLI、代理三条路
// 看到的必须是同一个值，不能「页面显示空、实际用默认值」）。
func (svc *Service) applyDefaults() {
	if strings.TrimSpace(svc.HostHeader) == "" {
		svc.HostHeader = HostHeaderUpstream
	}
	if strings.TrimSpace(svc.RouteMode) == "" {
		svc.RouteMode = RouteModePath
	}
	if strings.TrimSpace(svc.Source) == "" {
		svc.Source = ServiceSourceManual
	}
}

// validate 校验可编辑字段（创建与更新共用）。
func (svc *Service) validate() error {
	if err := ValidateServiceName(svc.Name); err != nil {
		return err
	}
	normalized, err := NormalizeUpstream(svc.Upstream)
	if err != nil {
		return err
	}
	svc.Upstream = normalized
	svc.applyDefaults()
	if svc.HostHeader != HostHeaderUpstream && svc.HostHeader != HostHeaderGateway {
		return fmt.Errorf("Host 头策略 %q 非法：只支持 upstream 或 gateway", svc.HostHeader)
	}
	if svc.RouteMode != RouteModePath {
		return fmt.Errorf("路由模式 %q 暂不支持：当前只支持 path（子路径挂载）", svc.RouteMode)
	}
	if svc.Source != ServiceSourceManual && svc.Source != ServiceSourceDiscover && svc.Source != ServiceSourceImport {
		return fmt.Errorf("服务来源 %q 非法", svc.Source)
	}
	if svc.Managed {
		if strings.TrimSpace(svc.StartCmd) == "" {
			return errors.New("受管服务必须设置启动命令")
		}
		if err := validateServiceEnv(svc.Env); err != nil {
			return err
		}
	} else {
		// 纯代理型：清掉生命周期字段，别把无意义的值带进库与界面。
		svc.StartCmd, svc.StopCmd, svc.WorkDir, svc.Env, svc.HealthPath = "", "", "", "", ""
		svc.AutoStart = false
	}
	if svc.ListenPort != 0 || svc.HealthIntervalS != 0 {
		return errors.New("port 映射与周期健康检查尚未开放（P1），请先用子路径模式")
	}
	return nil
}

// validateServiceEnv 校验 Env（每行 KEY=VALUE，允许空行）。
func validateServiceEnv(env string) error {
	for i, line := range strings.Split(env, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return fmt.Errorf("环境变量第 %d 行格式应为 KEY=VALUE：%q", i+1, line)
		}
		key := strings.TrimSpace(line[:eq])
		if !envKeyPattern.MatchString(key) {
			return fmt.Errorf("环境变量名 %q 非法（只允许字母数字下划线，且不以数字开头）", key)
		}
	}
	return nil
}

// NewServiceID 生成一个不透明服务 id（与实例 id 同一套生成器：40 bit 熵、
// base32 小写，满足路径白名单）。
func NewServiceID() (string, error) { return NewInstanceID() }

const serviceColumns = `id, name, upstream, host_header, insecure_tls, route_mode, listen_port, note, test_account, health_interval_s, enabled, source, created_at, updated_at, managed, start_cmd, stop_cmd, work_dir, env, auto_start, health_path`

// serviceColumnsQualified 是带 s. 前缀的列名：与 service_grants join 时
// 两张表都有 created_at，不加前缀会报 ambiguous column name。
const serviceColumnsQualified = `s.id, s.name, s.upstream, s.host_header, s.insecure_tls, s.route_mode, s.listen_port, s.note, s.test_account, s.health_interval_s, s.enabled, s.source, s.created_at, s.updated_at, s.managed, s.start_cmd, s.stop_cmd, s.work_dir, s.env, s.auto_start, s.health_path`

func scanService(row interface{ Scan(...any) error }) (*Service, error) {
	var (
		svc        Service
		insecure   int64
		enabled    int64
		listenPort int64
		healthSecs int64
		managed    int64
		autoStart  int64
		created    string
		updated    string
	)
	if err := row.Scan(&svc.ID, &svc.Name, &svc.Upstream, &svc.HostHeader, &insecure,
		&svc.RouteMode, &listenPort, &svc.Note, &svc.TestAccount, &healthSecs,
		&enabled, &svc.Source, &created, &updated,
		&managed, &svc.StartCmd, &svc.StopCmd, &svc.WorkDir, &svc.Env, &autoStart, &svc.HealthPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("读取服务失败: %w", err)
	}
	svc.InsecureTLS = intToBool(insecure)
	svc.Enabled = intToBool(enabled)
	svc.ListenPort = int(listenPort)
	svc.HealthIntervalS = int(healthSecs)
	svc.Managed = intToBool(managed)
	svc.AutoStart = intToBool(autoStart)
	svc.CreatedAt = parseTime(created)
	svc.UpdatedAt = parseTime(updated)
	svc.applyDefaults()
	return &svc, nil
}

// CreateService 登记一条服务。ID 为空时自动生成不透明短 id（冲突重试）。
// 新建一律为「启用」；要停用随后 UpdateService（避免零值 bool 默默建成停用态）。
func (s *Store) CreateService(svc *Service) error {
	if svc == nil {
		return errors.New("服务为空")
	}
	if err := svc.validate(); err != nil {
		return err
	}
	if _, err := s.ServiceByName(svc.Name); err == nil {
		return fmt.Errorf("服务名 %q 已存在", svc.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	id := svc.ID
	if id == "" {
		var err error
		for attempt := 0; attempt < 8; attempt++ {
			if id, err = NewServiceID(); err != nil {
				return err
			}
			if _, err := s.ServiceByID(id); errors.Is(err, ErrNotFound) {
				break
			}
			id = ""
		}
		if id == "" {
			return errors.New("生成服务 id 失败：连续冲突，请重试")
		}
	}
	if !ServiceIDPattern.MatchString(id) {
		return fmt.Errorf("服务 id %q 非法：只允许小写字母与数字", id)
	}

	now := nowText()
	if _, err := s.db.Exec(
		`INSERT INTO services (`+serviceColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, svc.Name, svc.Upstream, svc.HostHeader, boolToInt(svc.InsecureTLS),
		svc.RouteMode, svc.ListenPort, strings.TrimSpace(svc.Note), strings.TrimSpace(svc.TestAccount),
		svc.HealthIntervalS, 1, svc.Source, now, now,
		boolToInt(svc.Managed), strings.TrimSpace(svc.StartCmd), strings.TrimSpace(svc.StopCmd),
		strings.TrimSpace(svc.WorkDir), strings.TrimSpace(svc.Env), boolToInt(svc.AutoStart), strings.TrimSpace(svc.HealthPath),
	); err != nil {
		return fmt.Errorf("写入服务失败: %w", err)
	}
	saved, err := s.ServiceByID(id)
	if err != nil {
		return err
	}
	*svc = *saved
	return nil
}

// ListServices 按名字升序列出全部服务。
func (s *Store) ListServices() ([]Service, error) {
	rows, err := s.db.Query(`SELECT ` + serviceColumns + ` FROM services ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("读取服务列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]Service, 0, 8)
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *svc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取服务列表失败: %w", err)
	}
	return out, nil
}

// ServiceByID 按不透明 id 查服务；不存在时返回 ErrNotFound。
func (s *Store) ServiceByID(id string) (*Service, error) {
	if !ServiceIDPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	return scanService(s.db.QueryRow(`SELECT `+serviceColumns+` FROM services WHERE id = ?`, id))
}

// ServiceByName 按展示名查服务；不存在时返回 ErrNotFound。
func (s *Store) ServiceByName(name string) (*Service, error) {
	return scanService(s.db.QueryRow(`SELECT `+serviceColumns+` FROM services WHERE name = ?`, name))
}

// UpdateService 覆盖服务的可编辑字段（id 不可改）。
func (s *Store) UpdateService(svc Service) error {
	if err := svc.validate(); err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE services SET name = ?, upstream = ?, host_header = ?, insecure_tls = ?,
		 route_mode = ?, listen_port = ?, note = ?, test_account = ?, health_interval_s = ?,
		 enabled = ?, source = ?, updated_at = ?,
		 managed = ?, start_cmd = ?, stop_cmd = ?, work_dir = ?, env = ?, auto_start = ?, health_path = ?
		 WHERE id = ?`,
		svc.Name, svc.Upstream, svc.HostHeader, boolToInt(svc.InsecureTLS),
		svc.RouteMode, svc.ListenPort, strings.TrimSpace(svc.Note), strings.TrimSpace(svc.TestAccount),
		svc.HealthIntervalS, boolToInt(svc.Enabled), svc.Source, nowText(),
		boolToInt(svc.Managed), strings.TrimSpace(svc.StartCmd), strings.TrimSpace(svc.StopCmd),
		strings.TrimSpace(svc.WorkDir), strings.TrimSpace(svc.Env), boolToInt(svc.AutoStart), strings.TrimSpace(svc.HealthPath),
		svc.ID,
	)
	if err != nil {
		return fmt.Errorf("更新服务 %q 失败: %w", svc.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("服务 %q: %w", svc.ID, ErrNotFound)
	}
	return nil
}

// DeleteService 删除服务；其授权随之级联删除。服务进程不归门户管，删的只是登记。
func (s *Store) DeleteService(id string) error {
	res, err := s.db.Exec(`DELETE FROM services WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除服务 %q 失败: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("服务 %q: %w", id, ErrNotFound)
	}
	return nil
}

// ---- 服务授权（service_grants，与实例 grants 同一口径）----

// GrantService 给用户授权一个服务。重复授权是幂等的。
func (s *Store) GrantService(userName, serviceID, createdBy string) error {
	u, err := s.UserByName(userName)
	if err != nil {
		return err
	}
	if _, err := s.ServiceByID(serviceID); err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO service_grants (user_id, service_id, created_at, created_by) VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id, service_id) DO NOTHING`,
		u.ID, serviceID, nowText(), createdBy,
	)
	if err != nil {
		return fmt.Errorf("服务授权失败: %w", err)
	}
	return nil
}

// RevokeService 取消服务授权。未授权时也返回成功（幂等）。
func (s *Store) RevokeService(userName, serviceID string) error {
	u, err := s.UserByName(userName)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM service_grants WHERE user_id = ? AND service_id = ?`, u.ID, serviceID); err != nil {
		return fmt.Errorf("取消服务授权失败: %w", err)
	}
	return nil
}

// SetUserServiceGrants 把某个用户的服务授权集合整体设为 serviceIDs（矩阵表单一次提交用）。
func (s *Store) SetUserServiceGrants(userName string, serviceIDs []string, createdBy string) error {
	u, err := s.UserByName(userName)
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(serviceIDs))
	for _, id := range serviceIDs {
		if !ServiceIDPattern.MatchString(id) {
			return fmt.Errorf("服务 id %q 非法", id)
		}
		if _, err := s.ServiceByID(id); err != nil {
			return err
		}
		want[id] = true
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启服务授权事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM service_grants WHERE user_id = ?`, u.ID); err != nil {
		return fmt.Errorf("清空旧服务授权失败: %w", err)
	}
	now := nowText()
	for id := range want {
		if _, err := tx.Exec(
			`INSERT INTO service_grants (user_id, service_id, created_at, created_by) VALUES (?, ?, ?, ?)`,
			u.ID, id, now, createdBy,
		); err != nil {
			return fmt.Errorf("写入服务授权失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交服务授权失败: %w", err)
	}
	return nil
}

// ServiceGrants 列出某个服务被授权给了哪些用户名。
func (s *Store) ServiceGrants(serviceID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT u.name FROM service_grants g JOIN users u ON u.id = g.user_id WHERE g.service_id = ? ORDER BY u.name`,
		serviceID,
	)
	if err != nil {
		return nil, fmt.Errorf("读取服务授权失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取服务授权失败: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ServicesForUser 列出某用户被授权的服务（按名字升序）。
func (s *Store) ServicesForUser(userID int64) ([]Service, error) {
	rows, err := s.db.Query(
		`SELECT `+serviceColumnsQualified+` FROM services s
		 JOIN service_grants g ON g.service_id = s.id
		 WHERE g.user_id = ? ORDER BY s.name`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("读取用户服务失败: %w", err)
	}
	defer rows.Close()
	out := make([]Service, 0, 4)
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *svc)
	}
	return out, rows.Err()
}

// VisibleServices 返回某用户能看到的服务列表：管理员是所有服务，普通用户是被授权的。
func (s *Store) VisibleServices(u *User) ([]Service, error) {
	if u == nil {
		return nil, errors.New("用户为空")
	}
	if u.IsAdmin() {
		return s.ListServices()
	}
	return s.ServicesForUser(u.ID)
}

// ServiceAccessible 判定某用户能否进入某服务，并返回服务本身。
//
// **这是服务唯一的授权入口**（与实例的 Accessible 同一口径）：路径里的 id 只当
// 查询键，是否放行完全由这里的服务端判定决定。未命中授权时返回 ErrNotFound——
// 与「服务不存在」同形，不向调用方泄漏服务是否存在。
func (s *Store) ServiceAccessible(u *User, serviceID string) (*Service, error) {
	if u == nil {
		return nil, errors.New("用户为空")
	}
	svc, err := s.ServiceByID(serviceID)
	if err != nil {
		return nil, err
	}
	if u.IsAdmin() {
		return svc, nil
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM service_grants WHERE user_id = ? AND service_id = ?`, u.ID, serviceID,
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("校验服务授权失败: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	return svc, nil
}
