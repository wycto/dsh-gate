// 管理端的实例与授权：一个实例 = 一个 dsh（独立 DSH_HOME）+ 一个只绑回环的端口；
// 授权是「用户 ↔ 实例」的多对多表（grants），管理员天然可以进所有实例。
package portal

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// InstanceIDPattern 是对外路径里实例 id 的白名单：只允许小写字母与数字。
// 白名单先于查表执行，路径穿越与奇怪字符在这里就被挡掉。
var InstanceIDPattern = regexp.MustCompile(`^[a-z0-9]{4,64}$`)

// instanceNamePattern 是实例展示名的白名单（会出现在页面与 CLI 输出里）。
//
// 允许中文等 Unicode 字母。这是**展示名**：对外路径用的是不透明 id（`/i/<id>/`），
// 它既不进文件路径、也不进命令行参数，所以没有任何理由把它限死成 ASCII。最初
// 只放行 ASCII 的后果是：用户想给实例取个中文名字都做不到，管理台的授权矩阵里
// 只能出现 alice-main、bob-main 这类英文名——中文界面上留着两串英文标识。
//
// 仍然挡掉空白字符、路径分隔符、控制字符，以及开头的 `-` 与 `.`：开头的 `-` 会被
// CLI 当成选项，空白与分隔符在复制粘贴进命令行时容易出事。
var instanceNamePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._-]{0,63}$`)

// ValidateInstanceName 校验实例展示名。
func ValidateInstanceName(name string) error {
	if !instanceNamePattern.MatchString(name) {
		return fmt.Errorf("实例名 %q 非法：只允许字母（含中文）、数字与 . _ -，须以字母或数字开头，最长 64 个字符", name)
	}
	return nil
}

// Instance 是一个 dsh 实例的定义。
type Instance struct {
	// ID 是不透明短 id（对外路径用），Name 是给人看的名字。
	ID   string
	Name string
	// OwnerID 是默认拥有者（可空）：仅用于展示与「新实例默认授权给谁」，
	// 真正的访问判定只看 grants。
	OwnerID *int64
	// DshHome 是该实例的 DSH_HOME（会话、插件、配置都在里面）。
	DshHome string
	// Workspace 是 dsh 的工作目录（决定会话默认能访问哪些文件）。
	Workspace string
	// DshPort 是 dsh 只绑回环的监听端口。
	DshPort int
	// DshCmd 留空表示自动探测启动命令。
	DshCmd string
	// InstallMode 是 dsh 的安装方式（npx / git / local / pnpm），空视为 npx。
	// 它只影响「怎么把 dsh 准备好」，不影响运行时——几种方式最终都归结为
	// DshCmd 这一条启动命令。
	InstallMode string
	// InstallDir 是该实例的 dsh 安装目录（git 放源码，pnpm 放包，local 指向
	// 用户已构建好的源码仓库）；npx 方式不用它。
	InstallDir string
	// DshVersion 是要装的 dsh 版本或 dist-tag（`0.2.2-alpha.2`、`next`、`alpha`）。
	// 空表示跟随 latest，并保留「latest 在这台机器上跑不起来就退回实测可用版本」
	// 的自动回退；非空表示**钉住**：不再回退，装不上就直接说装不上（用户点名要
	// 的版本被悄悄换成别的版本，比装不上更难查）。只对走 npm 包的两种方式有效
	// （npx / pnpm）；从源码构建用 git 分支或标签，见 --git-ref。
	DshVersion string
	// ConfigDir 是该实例的配置目录。留空表示用 DshHome——dsh 自己会把配置放在
	// DSH_HOME 里；单独给一个目录是为了「配置与数据分开备份」这种诉求。
	ConfigDir string
	// ListenHost 是 dsh 的监听地址（默认回环 127.0.0.1）。只有管理端需要把
	// 实例直接暴露出去时才改它——默认回环意味着对外只有管理端一个口。
	ListenHost string
	// StartMode 是启动方式：auto（管理端启动时拉起）/ ondemand（用户点击才拉）。
	// 它是 OnDemand 的字符串化表达，管理台上更好呈现；两者保持一致。
	StartMode string
	// OnDemand 为真时不在启动管理端时拉起，等用户点击进入再拉。
	OnDemand bool
	// IdleTimeoutS 是空闲回收秒数，0 表示不回收。
	IdleTimeoutS int
	Enabled      bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Grant 是一条「用户被授权访问某实例」的记录。
type Grant struct {
	UserID     int64
	UserName   string
	InstanceID string
	CreatedAt  time.Time
	CreatedBy  string
}

const instanceColumns = `id, name, owner_id, dsh_home, workspace, dsh_port, dsh_cmd, install_mode, install_dir, dsh_version, config_dir, listen_host, start_mode, on_demand, idle_timeout_s, enabled, created_at, updated_at`

// instanceColumnsQualified 是同一份列名、带 `i.` 前缀的版本：与 grants 表 join 时
// 两张表都有 created_at，不加前缀会报 ambiguous column name。
const instanceColumnsQualified = `i.id, i.name, i.owner_id, i.dsh_home, i.workspace, i.dsh_port, i.dsh_cmd, i.install_mode, i.install_dir, i.dsh_version, i.config_dir, i.listen_host, i.start_mode, i.on_demand, i.idle_timeout_s, i.enabled, i.created_at, i.updated_at`

func scanInstance(row interface{ Scan(...any) error }) (*Instance, error) {
	var (
		inst     Instance
		owner    sql.NullInt64
		onDemand int64
		enabled  int64
		created  string
		updated  string
		idleSecs int64
	)
	if err := row.Scan(&inst.ID, &inst.Name, &owner, &inst.DshHome, &inst.Workspace,
		&inst.DshPort, &inst.DshCmd, &inst.InstallMode, &inst.InstallDir, &inst.DshVersion,
		&inst.ConfigDir,
		&inst.ListenHost, &inst.StartMode, &onDemand, &idleSecs, &enabled, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("读取实例失败: %w", err)
	}
	if owner.Valid {
		id := owner.Int64
		inst.OwnerID = &id
	}
	inst.OnDemand = intToBool(onDemand)
	inst.IdleTimeoutS = int(idleSecs)
	inst.Enabled = intToBool(enabled)
	inst.CreatedAt = parseTime(created)
	inst.UpdatedAt = parseTime(updated)
	inst.applyDefaults()
	return &inst, nil
}

// DefaultDshHomeName 是实例目录留空时用的默认值（与 dsh 自己的默认值一致）。
//
// 写成 `~` 开头的形式是因为它要展示给人看（帮助文本、页面占位符）；真正落库
// 前必须经 DefaultDshHome 展开成绝对路径。
const DefaultDshHomeName = "~/.dsh"

// DefaultDshHome 返回默认实例目录的**绝对路径**（当前用户主目录下的 .dsh）。
//
// 为什么必须展开而不是把 `~/.dsh` 直接存下来：这个值会被原样塞进子进程的环境
// 变量（DSH_HOME=...），环境变量不经过 shell，`~` 不会被展开。存字面量 `~/.dsh`
// 的后果是 dsh 在**当前工作目录**下建出一个名字就叫 `~` 的目录——数据写错地方，
// 而且目录里多出一个 `~`（这个坑在网关的单实例形态上真踩过，见 ownerguard 的注释）。
//
// 展开不了（拿不到家目录）时退回空串，由调用方决定怎么报错：宁可不建实例，
// 也不要写一个含义不明的路径进去。
func DefaultDshHome() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".dsh")
}

// applyDefaults 补齐空字段的默认值，并让 StartMode 与 OnDemand 保持一致。
//
// 默认值放在这里而不是「读的时候就地判断」，是因为这些字段要经过管理台展示、
// CLI 输出、运行时拉起三条路：分散判断迟早会出现「页面显示 auto、实际按需」
// 这种自相矛盾。老记录（schema v1 写的）读出来这些列是空串，也在这里归一。
func (inst *Instance) applyDefaults() {
	if strings.TrimSpace(inst.InstallMode) == "" {
		inst.InstallMode = "npx"
	}
	if strings.TrimSpace(inst.ListenHost) == "" {
		inst.ListenHost = "127.0.0.1"
	}
	if strings.TrimSpace(inst.StartMode) == "" {
		inst.StartMode = StartModeAuto
		if inst.OnDemand {
			inst.StartMode = StartModeOnDemand
		}
	}
	// 两个字段互为镜像，以 StartMode 为准（它是更新的一方）。
	inst.OnDemand = inst.StartMode == StartModeOnDemand
	// 工作目录留空时要给一个可用的默认值，不能留给 dsh 自己去猜：
	// dsh 的「默认工作区」是系统的「文档」目录，无头 Linux 上靠 xdg-user-dir
	// 解析——容器与服务器通常没有这个命令（即使有，禁用时它会返回家目录，
	// dsh 明确拒绝），前端只会弹一句「无法创建默认工作区」。网关的预置能力
	// （SeedDefaultWorkspace）也要求工作目录非空才动手，所以默认值必须在这里补。
	// 实例目录留空时用 dsh 自己的默认目录（~/.dsh）。放在这里与工作目录同理：
	// 展示、CLI 输出、运行时拉起三条路看到的必须是同一个值，不能「页面显示空、
	// 实际用默认值」。注意这里写成绝对路径，理由见 DefaultDshHome。
	if strings.TrimSpace(inst.DshHome) == "" {
		inst.DshHome = DefaultDshHome()
	}
	if strings.TrimSpace(inst.Workspace) == "" {
		inst.Workspace = inst.defaultWorkspaceDir()
	}
}

// defaultWorkspaceDir 返回实例未显式配置工作目录时的默认目录：<实例目录>/workspace。
//
// 放在实例自己的目录下而不是宿主的家目录：每个实例天然隔离、不依赖宿主的
// 「文档」目录长什么样，卸载实例时也随 DSH_HOME 一起清掉。配置目录显式指定时
// 工作区跟着它走——与运行时 homeOf 的取值保持一致，否则 dsh 的 DSH_HOME 与
// 工作区会落在两个不相干的根下。
func (inst *Instance) defaultWorkspaceDir() string {
	home := strings.TrimSpace(inst.ConfigDir)
	if home == "" {
		home = strings.TrimSpace(inst.DshHome)
	}
	if home == "" {
		// 只有「留空且取不到家目录」才可能走到这里；保持空串，不编路径。
		// 建实例时那种情况已经被 CreateInstance 拒掉了。
		return ""
	}
	return filepath.Join(home, "workspace")
}

// 启动方式的取值。
const (
	// StartModeAuto 表示管理端启动时一并拉起。
	StartModeAuto = "auto"
	// StartModeOnDemand 表示等用户点击进入再拉。
	StartModeOnDemand = "ondemand"
)

// CreateInstance 新建实例。
//
// ID 为空时自动生成不透明短 id（做一次存在性检查，冲突则重试）；DshPort 为 0
// 时在 [portBase, portBase+span) 里自动分配。DshHome 留空时用 dsh 自己的默认目录
// （~/.dsh 展开后的绝对路径），见 DefaultDshHome。
// 新建的实例一律为「启用」状态；要停用请随后 UpdateInstance（避免零值 bool 把
// 新实例默默建成停用态）。
func (s *Store) CreateInstance(inst *Instance) error {
	if inst == nil {
		return errors.New("实例为空")
	}
	if err := ValidateInstanceName(inst.Name); err != nil {
		return err
	}
	// 实例目录留空是合法输入：用 dsh 自己的默认目录（applyDefaults 补上，
	// 写库前会展开成绝对路径）。只有连默认值都取不到（拿不到家目录）才失败。
	if strings.TrimSpace(inst.DshHome) == "" && DefaultDshHome() == "" {
		return errors.New("实例目录留空且取不到当前用户主目录，无法推导默认的 DSH_HOME；请显式指定 dsh-home")
	}
	if _, err := s.InstanceByName(inst.Name); err == nil {
		return fmt.Errorf("实例名 %q 已存在", inst.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	id := inst.ID
	if id == "" {
		var err error
		for attempt := 0; attempt < 8; attempt++ {
			if id, err = NewInstanceID(); err != nil {
				return err
			}
			if _, err := s.InstanceByID(id); errors.Is(err, ErrNotFound) {
				break
			}
			id = ""
		}
		if id == "" {
			return errors.New("生成实例 id 失败：连续冲突，请重试")
		}
	}
	if !InstanceIDPattern.MatchString(id) {
		return fmt.Errorf("实例 id %q 非法：只允许小写字母与数字", id)
	}

	if inst.DshPort == 0 {
		port, err := s.AllocatePort(DefaultPortBase, DefaultPortSpan)
		if err != nil {
			return err
		}
		inst.DshPort = port
	}

	inst.applyDefaults()
	now := nowText()
	if _, err := s.db.Exec(
		`INSERT INTO instances (`+instanceColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inst.Name, nullableID(inst.OwnerID), strings.TrimSpace(inst.DshHome),
		strings.TrimSpace(inst.Workspace), inst.DshPort, strings.TrimSpace(inst.DshCmd),
		strings.TrimSpace(inst.InstallMode), strings.TrimSpace(inst.InstallDir),
		strings.TrimSpace(inst.DshVersion),
		strings.TrimSpace(inst.ConfigDir), strings.TrimSpace(inst.ListenHost),
		strings.TrimSpace(inst.StartMode),
		boolToInt(inst.OnDemand), inst.IdleTimeoutS, 1, now, now,
	); err != nil {
		return fmt.Errorf("写入实例失败: %w", err)
	}
	saved, err := s.InstanceByID(id)
	if err != nil {
		return err
	}
	*inst = *saved
	return nil
}

func nullableID(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

// ListInstances 按名字升序列出全部实例。
func (s *Store) ListInstances() ([]Instance, error) {
	rows, err := s.db.Query(`SELECT ` + instanceColumns + ` FROM instances ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("读取实例列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]Instance, 0, 8)
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inst)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取实例列表失败: %w", err)
	}
	return out, nil
}

// InstanceByID 按不透明 id 查实例；不存在时返回 ErrNotFound。
func (s *Store) InstanceByID(id string) (*Instance, error) {
	if !InstanceIDPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	return scanInstance(s.db.QueryRow(`SELECT `+instanceColumns+` FROM instances WHERE id = ?`, id))
}

// InstanceByName 按展示名查实例；不存在时返回 ErrNotFound。
func (s *Store) InstanceByName(name string) (*Instance, error) {
	return scanInstance(s.db.QueryRow(`SELECT `+instanceColumns+` FROM instances WHERE name = ?`, name))
}

// UpdateInstance 覆盖实例的可编辑字段（id 不可改）。
func (s *Store) UpdateInstance(inst Instance) error {
	if err := ValidateInstanceName(inst.Name); err != nil {
		return err
	}
	if strings.TrimSpace(inst.DshHome) == "" && DefaultDshHome() == "" {
		return errors.New("实例目录留空且取不到当前用户主目录，无法推导默认的 DSH_HOME；请显式指定 dsh-home")
	}
	if inst.DshPort <= 0 {
		return errors.New("实例端口必须为正整数")
	}
	inst.applyDefaults()
	res, err := s.db.Exec(
		`UPDATE instances SET name = ?, owner_id = ?, dsh_home = ?, workspace = ?, dsh_port = ?,
		 dsh_cmd = ?, install_mode = ?, install_dir = ?, dsh_version = ?, config_dir = ?, listen_host = ?,
		 start_mode = ?, on_demand = ?, idle_timeout_s = ?, enabled = ?, updated_at = ?
		 WHERE id = ?`,
		inst.Name, nullableID(inst.OwnerID), strings.TrimSpace(inst.DshHome), strings.TrimSpace(inst.Workspace),
		inst.DshPort, strings.TrimSpace(inst.DshCmd),
		strings.TrimSpace(inst.InstallMode), strings.TrimSpace(inst.InstallDir),
		strings.TrimSpace(inst.DshVersion),
		strings.TrimSpace(inst.ConfigDir), strings.TrimSpace(inst.ListenHost),
		strings.TrimSpace(inst.StartMode),
		boolToInt(inst.OnDemand), inst.IdleTimeoutS,
		boolToInt(inst.Enabled), nowText(), inst.ID,
	)
	if err != nil {
		return fmt.Errorf("更新实例 %q 失败: %w", inst.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("实例 %q: %w", inst.ID, ErrNotFound)
	}
	return nil
}

// DeleteInstance 删除实例；其授权随之级联删除。**不删除 dsh 的数据目录**：
// 会话与工作区是用户的数据，管理端只解除管理关系。
func (s *Store) DeleteInstance(id string) error {
	res, err := s.db.Exec(`DELETE FROM instances WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除实例 %q 失败: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("实例 %q: %w", id, ErrNotFound)
	}
	return nil
}

// ---- 授权（grants） ----

// Grant 给用户授权一个实例。重复授权是幂等的。
func (s *Store) Grant(userName, instanceID, createdBy string) error {
	u, err := s.UserByName(userName)
	if err != nil {
		return err
	}
	if _, err := s.InstanceByID(instanceID); err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO grants (user_id, instance_id, created_at, created_by) VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id, instance_id) DO NOTHING`,
		u.ID, instanceID, nowText(), createdBy,
	)
	if err != nil {
		return fmt.Errorf("授权失败: %w", err)
	}
	return nil
}

// Revoke 取消授权。未授权时也返回成功（幂等）。
func (s *Store) Revoke(userName, instanceID string) error {
	u, err := s.UserByName(userName)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM grants WHERE user_id = ? AND instance_id = ?`, u.ID, instanceID); err != nil {
		return fmt.Errorf("取消授权失败: %w", err)
	}
	return nil
}

// SetUserGrants 把某个用户的授权集合整体设为 instanceIDs（矩阵表单一次提交用）。
func (s *Store) SetUserGrants(userName string, instanceIDs []string, createdBy string) error {
	u, err := s.UserByName(userName)
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		if !InstanceIDPattern.MatchString(id) {
			return fmt.Errorf("实例 id %q 非法", id)
		}
		if _, err := s.InstanceByID(id); err != nil {
			return err
		}
		want[id] = true
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启授权事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM grants WHERE user_id = ?`, u.ID); err != nil {
		return fmt.Errorf("清空旧授权失败: %w", err)
	}
	now := nowText()
	for id := range want {
		if _, err := tx.Exec(
			`INSERT INTO grants (user_id, instance_id, created_at, created_by) VALUES (?, ?, ?, ?)`,
			u.ID, id, now, createdBy,
		); err != nil {
			return fmt.Errorf("写入授权失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交授权失败: %w", err)
	}
	return nil
}

// InstanceGrants 列出某个实例被授权给了哪些用户名。
func (s *Store) InstanceGrants(instanceID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT u.name FROM grants g JOIN users u ON u.id = g.user_id WHERE g.instance_id = ? ORDER BY u.name`,
		instanceID,
	)
	if err != nil {
		return nil, fmt.Errorf("读取实例授权失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取实例授权失败: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// InstancesForUser 列出某用户被授权的实例（按名字升序）。
func (s *Store) InstancesForUser(userID int64) ([]Instance, error) {
	rows, err := s.db.Query(
		`SELECT `+instanceColumnsQualified+` FROM instances i
		 JOIN grants g ON g.instance_id = i.id
		 WHERE g.user_id = ? ORDER BY i.name`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("读取用户实例失败: %w", err)
	}
	defer rows.Close()
	out := make([]Instance, 0, 4)
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inst)
	}
	return out, rows.Err()
}

// VisibleInstances 返回某用户能看到的实例列表：管理员是所有实例，普通用户是被授权的。
func (s *Store) VisibleInstances(u *User) ([]Instance, error) {
	if u == nil {
		return nil, errors.New("用户为空")
	}
	if u.IsAdmin() {
		return s.ListInstances()
	}
	return s.InstancesForUser(u.ID)
}

// Accessible 判定某用户能否进入某实例，并返回实例本身。
//
// **这是唯一的授权入口**：路径/表单里的实例 id 只当查询键，是否放行完全由
// 这里的服务端判定决定。未命中授权时返回 ErrNotFound —— 与「实例不存在」
// 同形，不向调用方泄漏实例是否存在（文档第 6 节）。
func (s *Store) Accessible(u *User, instanceID string) (*Instance, error) {
	if u == nil {
		return nil, errors.New("用户为空")
	}
	inst, err := s.InstanceByID(instanceID)
	if err != nil {
		return nil, err
	}
	if u.IsAdmin() {
		return inst, nil
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM grants WHERE user_id = ? AND instance_id = ?`, u.ID, instanceID,
	).Scan(&n); err != nil {
		return nil, fmt.Errorf("校验授权失败: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	return inst, nil
}
