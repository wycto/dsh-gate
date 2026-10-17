// 从旧形态迁移：把网关原来的实例级账号表（users.json）导入管理端用户表。
//
// 为什么需要：升级前每个实例各有一份 users.json，账号是「实例级」的；管理端把
// 账号提升到管理端一级。不迁移的话所有人都会突然登不进来。
//
// 迁移规则（保守）：
//   - 哈希串**原样搬运**——旧账号继续用原口令，不需要谁去改密码；
//   - 已存在的同名账号**跳过**（管理端里人工建过的以管理端为准）；
//   - 如果管理端里还没有管理员，就把**第一个导入的用户**提升为管理员，
//     否则升级完没人能进管理台；
//   - 迁移**不改动** users.json（调用方可以自己备份），可重复执行。
package portal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"
)

// legacyUsersFile 是 internal/users 写出的文件结构（这里只读，不写）。
type legacyUsersFile struct {
	Version int                      `json:"version"`
	Users   map[string]legacyUserRec `json:"users"`
}

type legacyUserRec struct {
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// LegacyImportResult 汇报一次迁移的结果。
type LegacyImportResult struct {
	// Found 是文件里读到的账号数。
	Found int
	// Imported 是真正写入管理端的账号数。
	Imported int
	// Skipped 是因为同名已存在而跳过的数量。
	Skipped int
	// Admin 是被提升为管理员的账号名（没提升则为空）。
	Admin string
	// Missing 表示文件不存在（这不是错误：全新安装就没有旧账号表）。
	Missing bool
}

// ImportLegacyUsers 读取 path 指向的 users.json 并导入管理端用户表。
func (s *Store) ImportLegacyUsers(path string) (LegacyImportResult, error) {
	var result LegacyImportResult

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		result.Missing = true
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("读取旧账号表 %q 失败: %w", path, err)
	}

	var doc legacyUsersFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return result, fmt.Errorf("旧账号表 %q 不是合法 JSON: %w", path, err)
	}
	if len(doc.Users) == 0 {
		return result, nil
	}

	// 按名字排序，保证「第一个导入的用户」可复现（map 遍历顺序随机）。
	names := make([]string, 0, len(doc.Users))
	for name := range doc.Users {
		names = append(names, name)
	}
	sort.Strings(names)
	result.Found = len(names)

	adminCount, err := s.CountAdmins()
	if err != nil {
		return result, err
	}

	for _, name := range names {
		rec := doc.Users[name]
		if rec.Hash == "" {
			result.Skipped++
			continue
		}
		if _, err := s.UserByName(name); err == nil {
			result.Skipped++
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return result, err
		}

		role := RoleUser
		if adminCount == 0 {
			role = RoleAdmin
			result.Admin = name
			adminCount = 1
		}

		created := rec.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		updated := rec.UpdatedAt
		if updated.IsZero() {
			updated = created
		}
		if _, err := s.db.Exec(
			`INSERT INTO users (name, display_name, hash, role, disabled, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 0, ?, ?)`,
			name, name, rec.Hash, string(role), formatTime(created), formatTime(updated),
		); err != nil {
			return result, fmt.Errorf("导入账号 %q 失败: %w", name, err)
		}
		result.Imported++
	}
	return result, nil
}
