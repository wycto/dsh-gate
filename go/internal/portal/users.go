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

// CreateUser 新建账号。name/password 走 internal/users 的同一套校验规则
// （用户名 1-64 字符、口令至少 8 位），role 为空时按普通用户处理。
func (s *Store) CreateUser(name, password string, role Role) (*User, error) {
	if err := users.ValidateName(name); err != nil {
		return nil, err
	}
	if err := users.ValidatePassword(password); err != nil {
		return nil, err
	}
	if role == "" {
		role = RoleUser
	}
	if !ValidRole(role) {
		return nil, fmt.Errorf("角色 %q 非法（只能是 admin 或 user）", role)
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
	now := nowText()
	res, err := s.db.Exec(
		`INSERT INTO users (name, display_name, hash, role, disabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 0, ?, ?)`,
		name, name, hash, string(role), now, now,
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

// SetRole 改角色。拒绝把最后一个管理员降级——否则没人能再管理管理端。
func (s *Store) SetRole(name string, role Role) error {
	if !ValidRole(role) {
		return fmt.Errorf("角色 %q 非法（只能是 admin 或 user）", role)
	}
	u, err := s.UserByName(name)
	if err != nil {
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

// SetDisabled 启用 / 停用账号。同样保护最后一个管理员。
func (s *Store) SetDisabled(name string, disabled bool) error {
	u, err := s.UserByName(name)
	if err != nil {
		return err
	}
	if disabled && u.Role == RoleAdmin && !u.Disabled {
		n, err := s.CountAdmins()
		if err != nil {
			return err
		}
		if n <= 1 {
			return fmt.Errorf("%w（会导致无人能管理管理端）", ErrLastAdmin)
		}
	}
	return s.updateUser(name, `UPDATE users SET disabled = ?, updated_at = ? WHERE name = ?`, boolToInt(disabled), nowText(), name)
}

// RenameUser 改登录名（审计里的人名会跟着变，历史记录不做回填）。
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
	return s.updateUser(oldName, `UPDATE users SET name = ?, updated_at = ? WHERE name = ?`, newName, nowText(), oldName)
}

// SetDisplayName 设置展示名（可为空，空则回退登录名）。
func (s *Store) SetDisplayName(name, display string) error {
	return s.updateUser(name, `UPDATE users SET display_name = ?, updated_at = ? WHERE name = ?`,
		strings.TrimSpace(display), nowText(), name)
}

// DeleteUser 删除账号。拒绝删掉最后一个管理员；该用户的授权随之级联删除。
func (s *Store) DeleteUser(name string) error {
	u, err := s.UserByName(name)
	if err != nil {
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
