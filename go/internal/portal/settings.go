// 门户级全局总开关（docs/portal.md「用户级类别权限」的全局层）。
//
// 与「每用户类别开关」（users.allow_instances / allow_services）是两层：
// 全局开关关掉某类时，**所有人（含管理员）**的该类入口都隐藏；每用户开关只
// 约束普通用户。最终可见性 = 全局开关 AND（管理员 OR 用户开关），判定集中在
// Server.canSeeInstances / canSeeServices，这里只负责存取。
//
// 存在 settings 键值表而非给 users 加列：总开关是门户级事实，与账号无关。
// 键缺省视为「开」（true）：老库没有这两行，升级后行为与升级前逐字一致。
//
// 站点名称（site_title）也放在这张表里：它同样是门户级事实。与布尔开关不同，
// 它是字符串且缺省为空（= 用各页面的默认标题），见 SiteTitle / SetSiteTitle。
package portal

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 全局总开关在 settings 表里的键名。值用 "1"/"0"（与布尔列同一口径）。
const (
	settingInstancesEnabled   = "instances_enabled"
	settingServicesEnabled    = "services_enabled"
	settingExternalAPIEnabled = "external_api_enabled"
	// settingSiteTitle 是自定义站点名称（字符串，空 = 默认标题）。
	settingSiteTitle = "site_title"
)

// siteTitleMaxRunes 限制站点名称长度。按 rune 计数：中文与英文同一口径，
// 64 个字符足够写「家里 NAS」「公司工作站」这类区分性名字，又不至于把浏览器
// 标签页撑爆。
const siteTitleMaxRunes = 64

// SiteTitle 读自定义站点名称；键不存在返回空串（= 各页面用默认标题）。
// 老库没有这一行，升级后标题与升级前逐字一致。
func (s *Store) SiteTitle() (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingSiteTitle).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取站点名称失败: %w", err)
	}
	return value, nil
}

// SetSiteTitle 写站点名称（UPSERT）。先 TrimSpace；拒绝控制字符（换行、
// 制表符之类会破坏页面排版）；上限 64 个字符。空串合法：保存空串即恢复
// 默认标题。
func (s *Store) SetSiteTitle(v string) error {
	v = strings.TrimSpace(v)
	for _, r := range v {
		if unicode.IsControl(r) {
			return errors.New("站点名称不能包含控制字符（换行、制表符等）")
		}
	}
	if utf8.RuneCountInString(v) > siteTitleMaxRunes {
		return fmt.Errorf("站点名称最长 %d 个字符", siteTitleMaxRunes)
	}
	if _, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingSiteTitle, v,
	); err != nil {
		return fmt.Errorf("写入站点名称失败: %w", err)
	}
	return nil
}

// ExternalAPIEnabled 报告对外 HTTP API 是否开启。**缺省关**（false）：网关是内网
// 服务入口，默认不额外开口；管理员在管理台「API」分页显式开启后才生效。
func (s *Store) ExternalAPIEnabled() (bool, error) {
	return s.settingBoolOr(settingExternalAPIEnabled, false)
}

// SetExternalAPIEnabled 写外部 API 开关。
func (s *Store) SetExternalAPIEnabled(v bool) error {
	return s.setSetting(settingExternalAPIEnabled, v)
}

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
	return s.settingBoolOr(key, true)
}

// settingBoolOr 读一个布尔设置；键不存在返回 def。
func (s *Store) settingBoolOr(key string, def bool) (bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil // 缺省：老库 / 空库行为不变
	}
	if err != nil {
		return false, fmt.Errorf("读取设置 %q 失败: %w", key, err)
	}
	return value != "0", nil
}

// setSetting 写一个布尔设置（UPSERT）。
func (s *Store) setSetting(key string, v bool) error {
	if _, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, boolToText(v),
	); err != nil {
		return fmt.Errorf("写入设置 %q 失败: %w", key, err)
	}
	return nil
}

// boolToText 把布尔落成 "1"/"0"（与 settingBool 的读取口径互逆）。
func boolToText(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
