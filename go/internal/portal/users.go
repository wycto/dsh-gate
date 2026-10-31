// 管理端的用户与身份：账号、口令、角色、停用，以及登录校验。
//
// 口令哈希复用 internal/users（pbkdf2-sha256$迭代次数$盐$摘要），因此
// 从旧 users.json 迁移过来的账号可以直接用原哈希，不需要任何人改口令。
package portal

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/users"
)

// Role 是管理端角色。本期只有两种：管理员能管用户/实例/授权，普通用户只能进
// 自己被授权的实例。更细的角色（viewer 等）留到后续版本。
type Role string

const (
	// RoleAdmin 可以管理用户、实例与授权，并能进入所有实例。
	RoleAdmin Role = "admin"
	// RoleUser 只能进入被授权的实例。
	RoleUser Role = "user"
)

// ValidRole 判断角色取值是否合法。
func ValidRole(r Role) bool { return r == RoleAdmin || r == RoleUser }

// 认证失败的原因。调用方据此决定提示语；注意「账号不存在」与「口令错」
// 对外必须给同一句话，否则等于送人一个账号枚举接口。
var (
	// ErrBadCredentials 用户名或口令不正确（含账号不存在）。
	ErrBadCredentials = errors.New("用户名或口令不正确")
	// ErrDisabled 账号已被停用。只有口令校验通过后才可能返回它。
	ErrDisabled = errors.New("账号已停用")
	// ErrUserExists 同名账号已存在。
	ErrUserExists = errors.New("账号已存在")
	// ErrLastAdmin 该操作会让管理端失去最后一个管理员，因此被拒绝。
	// 用哨兵错误而不是裸字符串：管理台要据此给出准确文案。
	ErrLastAdmin = errors.New("拒绝操作最后一个管理员")
	// ErrSuperAdmin 该操作指向超级管理员（第一个注册的账号）。它承载管理端
	// 的初始身份：不能改角色、不能停用、不能删除。改口令、展示名与登录名
	// 不在禁止之列——忘了口令就把整个管理端锁死；默认账号名（admin）不合
	// 人心意，而超管身份由 id 决定，改名不改变谁是超管。
	ErrSuperAdmin = errors.New("拒绝操作超级管理员（第一个注册的账号）")
)

// User 是一条管理端账号记录。
type User struct {
	ID          int64
	Name        string
	DisplayName string
	// Hash 是自描述的口令哈希串，永不外传（页面与 API 都不包含它）。
	Hash      string
	Role      Role
	Disabled  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsAdmin 是否是管理员。
func (u *User) IsAdmin() bool { return u != nil && u.Role == RoleAdmin }

// SuperAdminID 返回超级管理员的账号 id：用户表里 id 最小的那条，也就是
// 第一个注册的账号（含从旧 users.json 迁移过来的首位）。它不存字段、
// 不建配置——身份由「最早注册」这一事实决定，永不因后续操作而改变。
// 表为空时返回 ErrNotFound。
func (s *Store) SuperAdminID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM users ORDER BY id ASC LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("查询超级管理员失败: %w", err)
	}
	return id, nil
}

// IsSuperAdmin 判断某账号是否超级管理员。
func (s *Store) IsSuperAdmin(name string) bool {
	u, err := s.UserByName(name)
	if err != nil {
		return false
	}
	id, err := s.SuperAdminID()
	return err == nil && id == u.ID
}

// randomDummySecret 给「账号不存在」分支造一个随机口令，用于计算校验桩哈希。
func randomDummySecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// 极不可能发生；真发生了就用固定串，只是失去一点时间侧信道防护。
		return "dsh-gate-dummy-secret"
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// hashPassword 是对 users.HashPassword 的薄封装，让本包其余代码只依赖一处。
func hashPassword(password string) (string, error) { return users.HashPassword(password) }

// Verify / Count 让管理端账号表满足 auth.Verifier：这样管理端可以直接复用
// internal/auth 的登录页、限速与会话机制，而不必再写一套登录流程。
//
// Verify 只回答「口令对不对」——**停用视为不通过**。停用的提示留给 Authenticate
// 的调用方（管理端页面能给出更准确的原因），而鉴权层要的是一个简单的布尔。
func (s *Store) Verify(name, password string) bool {
	_, err := s.Authenticate(name, password)
	return err == nil
}

// Count 返回账号总数（auth 用它做「没有账号就拒绝启动」的 fail-closed 判断）。
func (s *Store) Count() int {
	n, err := s.CountUsers()
	if err != nil {
		return 0
	}
	return n
}

// CreateUser 新建账号（不带姓名，姓名回退登录名）。name/password 走
// internal/users 的同一套校验规则（账号 ASCII、口令至少 8 位），role 为空
// 时按普通用户处理。需要给人看的中文名请用 CreateUserWithDisplay。
func (s *Store) CreateUser(name, password string, role Role) (*User, error) {
	return s.CreateUserWithDisplay(name, password, role, "")
}

// CreateUserWithDisplay 新建账号并带上「姓名」（给人看的中文名）。
// display 为空时回退登录名；非空时按 ValidateDisplayName 校验。
func (s *Store) CreateUserWithDisplay(name, password string, role Role, display string) (*User, error) {
	if err := users.ValidateName(name); err != nil {
		return nil, err
	}
	if err := users.ValidatePassword(password); err != nil {
		return nil, err
	}
	if err := users.ValidateDisplayName(display); err != nil {
		return nil, err
	}
	display = strings.TrimSpace(display)
	if role == "" {
		role = RoleUser
	}
	if !ValidRole(role) {
		return nil, fmt.Errorf("角色 %q 非法（只能是 admin 或 user）", role)
	}
	// 第一个注册的账号就是超级管理员（身份判定见 SuperAdminID）：即便调用方
	// 要的是普通用户，也按管理员建——超管不是「先建人再提权」的第二步，
	// 而是这张表出生时就带着的身份。
	if n, err := s.CountUsers(); err != nil {
		return nil, err
	} else if n == 0 {
		role = RoleAdmin
	}
	if _, err := s.UserByName(name); err == nil {
		return nil, fmt.Errorf("账号 %q: %w", name, ErrUserExists)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	// 姓名留空回退登录名：display_name 参与「已登录：X」这类展示，空串会显示成空白。
	if display == "" {
		display = name
	}
	now := nowText()
	res, err := s.db.Exec(
		`INSERT INTO users (name, display_name, hash, role, disabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 0, ?, ?)`,
		name, display, hash, string(role), now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("写入账号失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("读取新账号 id 失败: %w", err)
	}
	return s.UserByID(id)
}

const userColumns = `id, name, display_name, hash, role, disabled, created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var (
		u        User
		role     string
		disabled int64
		created  string
		updated  string
	)
	if err := row.Scan(&u.ID, &u.Name, &u.DisplayName, &u.Hash, &role, &disabled, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("读取账号失败: %w", err)
	}
	u.Role = Role(role)
	u.Disabled = intToBool(disabled)
	u.CreatedAt = parseTime(created)
	u.UpdatedAt = parseTime(updated)
	return &u, nil
}

// UserByName 按登录名查账号；不存在时返回 ErrNotFound。
func (s *Store) UserByName(name string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE name = ?`, name))
}

// UserByID 按主键查账号；不存在时返回 ErrNotFound。
func (s *Store) UserByID(id int64) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// ListUsers 按名字升序列出全部账号。
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("读取账号列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]User, 0, 8)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取账号列表失败: %w", err)
	}
	return out, nil
}

// CountUsers 返回账号总数。
func (s *Store) CountUsers() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计账号数量失败: %w", err)
	}
	return n, nil
}

// CountAdmins 返回管理员数量（含被停用的，用于「最后一个管理员」保护）。
func (s *Store) CountAdmins() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, string(RoleAdmin)).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计管理员数量失败: %w", err)
	}
	return n, nil
}

// SetPassword 覆盖账号口令。
func (s *Store) SetPassword(name, password string) error {
	if err := users.ValidatePassword(password); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return s.updateUser(name, `UPDATE users SET hash = ?, updated_at = ? WHERE name = ?`, hash, nowText(), name)
}

// SetRole 改角色。超级管理员（第一个注册的账号）的角色不可变；
// 其余拒绝把最后一个管理员降级——否则没人能再管理管理端。
func (s *Store) SetRole(name string, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("角色 %q 非法（只能是 admin 或 user）", role)
	}
	u, err := s.UserByName(name)
	if err != nil {
		return err
	}
	if err := s.guardSuperAdmin(u, "改角色"); err != nil {
		return err
	}
	if u.Role == RoleAdmin && role != RoleAdmin {
		n, err := s.CountAdmins()
		if err != nil {
			return err
		}
		if n <= 1 {
			return fmt.Errorf("%w（会导致无人能管理管理端）", ErrLastAdmin)
		}
	}
	return s.updateUser(name, `UPDATE users SET role = ?, updated_at = ? WHERE name = ?`, string(role), nowText(), name)
}

// SetDisabled 启用 / 停用账号。超级管理员不可停用；其余同样保护最后一个管理员。
func (s *Store) SetDisabled(name string, disabled bool) error {
	u, err := s.UserByName(name)
	if err != nil {
		return err
	}
	if disabled {
		if err := s.guardSuperAdmin(u, "停用"); err != nil {
			return err
		}
		if u.Role == RoleAdmin && !u.Disabled {
			n, err := s.CountAdmins()
			if err != nil {
				return err
			}
			if n <= 1 {
				return fmt.Errorf("%w（会导致无人能管理管理端）", ErrLastAdmin)
			}
		}
	}
	return s.updateUser(name, `UPDATE users SET disabled = ?, updated_at = ? WHERE name = ?`, boolToInt(disabled), nowText(), name)
}

// RenameUser 改登录名（审计里的人名会跟着变，历史记录不做回填）。
// 超级管理员**可以**改名：它的身份依据是「id 最小 / 第一个注册」，改名不动 id，
// 改完之后它照样是超管。默认账号 admin 不合心意时，这是把它换掉的唯一正路。
//
// 展示名如果还和旧登录名相同（创建时回退出来的），一并跟着改，免得页面上
// 显示的还是旧名字。已登录会话的主体是旧登录名，改名后自然失效、需要重新登录。
func (s *Store) RenameUser(oldName, newName string) error {
	if err := users.ValidateName(newName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	if _, err := s.UserByName(newName); err == nil {
		return fmt.Errorf("账号 %q 已存在", newName)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	// 先改展示名再改登录名：两条语句都按旧名定位。updateUser 把「没改到行」
	// 报成 ErrNotFound，而展示名回退时可能为空串，所以这里只在相等时才动它。
	u, err := s.UserByName(oldName)
	if err != nil {
		return err
	}
	if u.DisplayName == oldName {
		if err := s.SetDisplayName(oldName, newName); err != nil {
			return err
		}
	}
	return s.updateUser(oldName, `UPDATE users SET name = ?, updated_at = ? WHERE name = ?`, newName, nowText(), oldName)
}

// SetDisplayName 设置展示名（可为空，空则回退登录名）。
func (s *Store) SetDisplayName(name, display string) error {
	return s.updateUser(name, `UPDATE users SET display_name = ?, updated_at = ? WHERE name = ?`,
		strings.TrimSpace(display), nowText(), name)
}

// DeleteUser 删除账号。超级管理员不可删除；其余拒绝删掉最后一个管理员。
// 该用户的授权随之级联删除。
func (s *Store) DeleteUser(name string) error {
	u, err := s.UserByName(name)
	if err != nil {
		return err
	}
	if err := s.guardSuperAdmin(u, "删除"); err != nil {
		return err
	}
	if u.Role == RoleAdmin {
		n, err := s.CountAdmins()
		if err != nil {
			return err
		}
		if n <= 1 {
			return fmt.Errorf("%w（会导致无人能管理管理端）", ErrLastAdmin)
		}
	}
	return s.updateUser(name, `DELETE FROM users WHERE name = ?`, name)
}

// guardSuperAdmin 拦住对超级管理员（第一个注册的账号）的受保护操作。
// what 是中文动作名，出现在错误里供日志与页面使用。
//
// 刻意不保护改口令与展示名：忘了口令就把整个管理端锁死，代价远大于收益。
func (s *Store) guardSuperAdmin(u *User, what string) error {
	id, err := s.SuperAdminID()
	if err != nil {
		// 查不出超级管理员（表为空的竞态）不拦：后面的语句自会处理不存在。
		return nil
	}
	if u.ID == id {
		return fmt.Errorf("%w（%s %s）", ErrSuperAdmin, what, u.Name)
	}
	return nil
}

// updateUser 跑一条「按名字改一行」的语句，并统一把「没改到任何行」报成 ErrNotFound。
func (s *Store) updateUser(name, query string, args ...any) error {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("更新账号 %q 失败: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("更新账号 %q 失败: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("账号 %q: %w", name, ErrNotFound)
	}
	return nil
}

// Authenticate 校验登录。
//
// 顺序刻意如此：**先校验口令，再判断是否停用** —— 这样「账号已停用」只会在
// 知道正确口令的前提下被告知，不会变成枚举账号的旁路。账号不存在时也走一次
// 真实的 KDF，让耗时与「存在但口令错」同量级。
func (s *Store) Authenticate(name, password string) (*User, error) {
	u, err := s.UserByName(name)
	if errors.Is(err, ErrNotFound) {
		_ = users.VerifyPassword(s.dummyHash, password)
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if !users.VerifyPassword(u.Hash, password) {
		return nil, ErrBadCredentials
	}
	if u.Disabled {
		return nil, ErrDisabled
	}
	return u, nil
}
