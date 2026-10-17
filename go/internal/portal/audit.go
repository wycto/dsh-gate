// 管理端审计：谁在什么时候做了什么。只追加、不修改（要清理就整表按时间删）。
package portal

import (
	"fmt"
	"time"
)

// AuditEntry 是一条审计记录。
type AuditEntry struct {
	ID     int64
	At     time.Time
	Actor  string // 操作者登录名；登录失败等场景可以为空
	Action string // 稳定短标识，如 login.ok / login.fail / instance.enter / user.add
	Target string // 受影响对象（用户名、实例名或实例 id）
	Detail string // 补充说明（错误原因、参数摘要）；不得写入口令等秘密
	IP     string
	OK     bool
}

// Audit 追加一条审计。失败只返回错误，绝不 panic——审计写不进去不应该
// 把正常业务操作打断（调用方自行决定是否记日志）。
func (s *Store) Audit(e AuditEntry) error {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.db.Exec(
		`INSERT INTO audit (at, actor, action, target, detail, ip, ok) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		formatTime(at), e.Actor, e.Action, e.Target, e.Detail, e.IP, boolToInt(e.OK),
	)
	if err != nil {
		return fmt.Errorf("写入审计失败: %w", err)
	}
	return nil
}

// RecentAudit 返回最近 limit 条审计（新的在前）。limit<=0 时按 100 处理。
func (s *Store) RecentAudit(limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, at, actor, action, target, detail, ip, ok FROM audit ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("读取审计失败: %w", err)
	}
	defer rows.Close()
	out := make([]AuditEntry, 0, limit)
	for rows.Next() {
		var (
			e  AuditEntry
			at string
			ok int64
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.IP, &ok); err != nil {
			return nil, fmt.Errorf("读取审计失败: %w", err)
		}
		e.At = parseTime(at)
		e.OK = intToBool(ok)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditCount 返回审计总条数（页面显示「共 N 条」用）。
func (s *Store) AuditCount() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计审计条数失败: %w", err)
	}
	return n, nil
}
