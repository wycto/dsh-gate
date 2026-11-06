// 门户级全局总开关（docs/portal.md「用户级类别权限」的全局层）。
//
// 与「每用户类别开关」（users.allow_instances / allow_services）是两层：
// 全局开关关掉某类时，**所有人（含管理员）**的该类入口都隐藏；每用户开关只
// 约束普通用户。最终可见性 = 全局开关 AND（管理员 OR 用户开关），判定集中在
// Server.canSeeInstances / canSeeServices，这里只负责存取。
//
// 存在 settings 键值表而非给 users 加列：总开关是门户级事实，与账号无关。
// 键缺省视为「开」（true）：老库没有这两行，升级后行为与升级前逐字一致。
package portal

import (
	"database/sql"
	"errors"
	"fmt"
)

// 全局总开关在 settings 表里的键名。值用 "1"/"0"（与布尔列同一口径）。
const (
	settingInstancesEnabled = "instances_enabled"
	settingServicesEnabled  = "services_enabled"
)

// PortalFeatures 读门户「实例 / 服务」两个全局总开关。键不存在视为开（true），
// 因此空库 / 老库升级后默认两类都可用。
func (s *Store) PortalFeatures() (instances bool, services bool, err error) {
	instances, err = s.settingBool(settingInstancesEnabled)
	if err != nil {
		return false, false, err
	}
	services, err = s.settingBool(settingServicesEnabled)
	if err != nil {
		return false, false, err
	}
	return instances, services, nil
}

// SetPortalFeatures 写两个全局总开关（UPSERT）。一次事务写两行，避免读到
// 「实例关了、服务还没关」的中间态。
func (s *Store) SetPortalFeatures(instances, services bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启总开关写入事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range []struct {
		key string
		val bool
	}{
		{settingInstancesEnabled, instances},
		{settingServicesEnabled, services},
	} {
		if _, err := tx.Exec(
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			kv.key, boolToText(kv.val),
		); err != nil {
			return fmt.Errorf("写入总开关 %q 失败: %w", kv.key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交总开关写入失败: %w", err)
	}
	return nil
}

// settingBool 读一个布尔设置；键不存在视为 true（开）。
func (s *Store) settingBool(key string) (bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // 缺省开：老库 / 空库行为不变
	}
	if err != nil {
		return false, fmt.Errorf("读取设置 %q 失败: %w", key, err)
	}
	return value != "0", nil
}

// boolToText 把布尔落成 "1"/"0"（与 settingBool 的读取口径互逆）。
func boolToText(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
