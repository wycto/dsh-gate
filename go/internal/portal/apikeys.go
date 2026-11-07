// 对外 HTTP API 的鉴权密钥（docs/service-api.md）。本文件只管密钥本身的存取与校验，
// HTTP 路由/限速/审计在 api.go。
//
// 安全口径：
//   - 密钥是高熵随机串，库里只存 sha256 哈希 + 用于查找的前缀，明文只在创建时返回一次；
//   - 校验用常数时间比较（先按前缀取出候选，再比哈希），失败一律 ErrAPIKeyInvalid；
//   - 按 scope 授权（逗号分隔），IP 白名单可选。
package portal

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// 对外 API 的 scope 取值。
const (
	ScopeServicesRead       = "services:read"
	ScopeServicesWrite      = "services:write"
	ScopeServicesLifecycle  = "services:lifecycle"
	ScopeInstancesRead      = "instances:read"
	ScopeInstancesLifecycle = "instances:lifecycle"
)

var validScopes = map[string]bool{
	ScopeServicesRead: true, ScopeServicesWrite: true, ScopeServicesLifecycle: true,
	ScopeInstancesRead: true, ScopeInstancesLifecycle: true,
}

// ErrAPIKeyInvalid 表示密钥无效（不存在 / 已停用 / 不匹配）。对外一律 401。
var ErrAPIKeyInvalid = errors.New("API 密钥无效")

const apiKeyPrefixLen = 12

// APIKey 是一条对外 API 密钥记录（不含明文）。
type APIKey struct {
	ID          string
	Name        string
	Prefix      string
	Hash        string
	Scopes      string
	IPAllowlist string
	Disabled    bool
	CreatedAt   time.Time
	CreatedBy   string
	LastUsedAt  time.Time
}

// ScopeList 返回 scope 列表（去空白）。
func (k *APIKey) ScopeList() []string {
	if k == nil || strings.TrimSpace(k.Scopes) == "" {
		return nil
	}
	parts := strings.Split(k.Scopes, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// HasScope 报告是否含某 scope。
func (k *APIKey) HasScope(scope string) bool {
	for _, s := range k.ScopeList() {
		if s == scope {
			return true
		}
	}
	return false
}

func generateAPIKeySecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 API 密钥失败: %w", err)
	}
	return "dgk_" + hex.EncodeToString(buf), nil
}

func hashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ValidateScopes 校验 scope 列表，返回归一后的逗号串。空列表合法。
func ValidateScopes(scopes []string) (string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !validScopes[s] {
			return "", fmt.Errorf("未知 scope %q；可用：services:read / services:write / services:lifecycle / instances:read / instances:lifecycle", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return strings.Join(out, ","), nil
}

const apiKeyColumns = "id, name, prefix, hash, scopes, ip_allowlist, disabled, created_at, created_by, last_used_at"

func scanAPIKey(row interface{ Scan(...any) error }) (*APIKey, error) {
	var (
		k        APIKey
		disabled int64
		created  string
		lastUsed string
	)
	if err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.Scopes, &k.IPAllowlist,
		&disabled, &created, &k.CreatedBy, &lastUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("读取 API 密钥失败: %w", err)
	}
	k.Disabled = intToBool(disabled)
	k.CreatedAt = parseTime(created)
	k.LastUsedAt = parseTime(lastUsed)
	return &k, nil
}

// CreateAPIKey 创建一条密钥，返回明文（只此一次）与记录。
func (s *Store) CreateAPIKey(name string, scopes []string, ipAllowlist, createdBy string) (secret string, key *APIKey, err error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errors.New("密钥名不能为空")
	}
	scopeStr, err := ValidateScopes(scopes)
	if err != nil {
		return "", nil, err
	}
	if _, err := s.APIKeyByName(name); err == nil {
		return "", nil, fmt.Errorf("密钥名 %q 已存在", name)
	} else if !errors.Is(err, ErrNotFound) {
		return "", nil, err
	}
	secret, err = generateAPIKeySecret()
	if err != nil {
		return "", nil, err
	}
	id, err := NewInstanceID()
	if err != nil {
		return "", nil, err
	}
	prefix := secret[:apiKeyPrefixLen]
	now := nowText()
	if _, err := s.db.Exec(
		"INSERT INTO api_keys ("+apiKeyColumns+") VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, '')",
		id, name, prefix, hashAPIKey(secret), scopeStr, strings.TrimSpace(ipAllowlist), now, strings.TrimSpace(createdBy),
	); err != nil {
		return "", nil, fmt.Errorf("写入 API 密钥失败: %w", err)
	}
	saved, err := s.APIKeyByID(id)
	if err != nil {
		return "", nil, err
	}
	return secret, saved, nil
}

// ListAPIKeys 按创建时间列出全部密钥（不含明文）。
func (s *Store) ListAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query("SELECT " + apiKeyColumns + " FROM api_keys ORDER BY created_at")
	if err != nil {
		return nil, fmt.Errorf("读取 API 密钥列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]APIKey, 0, 4)
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// APIKeyByID 按不透明 id 查密钥。
func (s *Store) APIKeyByID(id string) (*APIKey, error) {
	if !InstanceIDPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	return scanAPIKey(s.db.QueryRow("SELECT "+apiKeyColumns+" FROM api_keys WHERE id = ?", id))
}

// APIKeyByName 按密钥名查。
func (s *Store) APIKeyByName(name string) (*APIKey, error) {
	return scanAPIKey(s.db.QueryRow("SELECT "+apiKeyColumns+" FROM api_keys WHERE name = ?", name))
}

// SetAPIKeyDisabled 启用/停用密钥。
func (s *Store) SetAPIKeyDisabled(id string, disabled bool) error {
	res, err := s.db.Exec("UPDATE api_keys SET disabled = ? WHERE id = ?", boolToInt(disabled), id)
	if err != nil {
		return fmt.Errorf("更新 API 密钥失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("API 密钥 %q: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteAPIKey 删除密钥。
func (s *Store) DeleteAPIKey(id string) error {
	res, err := s.db.Exec("DELETE FROM api_keys WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("删除 API 密钥失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("API 密钥 %q: %w", id, ErrNotFound)
	}
	return nil
}

// AuthenticateAPIKey 校验明文密钥，成功返回记录（并更新 last_used_at）。
func (s *Store) AuthenticateAPIKey(secret string) (*APIKey, error) {
	secret = strings.TrimSpace(secret)
	if len(secret) < apiKeyPrefixLen || !strings.HasPrefix(secret, "dgk_") {
		return nil, ErrAPIKeyInvalid
	}
	prefix := secret[:apiKeyPrefixLen]
	rows, err := s.db.Query("SELECT "+apiKeyColumns+" FROM api_keys WHERE prefix = ?", prefix)
	if err != nil {
		return nil, fmt.Errorf("校验 API 密钥失败: %w", err)
	}
	want := hashAPIKey(secret)
	var matched *APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(want)) == 1 && !k.Disabled {
			matched = k
			break
		}
	}
	err = rows.Err()
	rows.Close() // 必须先关结果集再 Exec：连接池固定 1 条，边迭代边写会自锁
	if err != nil {
		return nil, err
	}
	if matched == nil {
		return nil, ErrAPIKeyInvalid
	}
	_, _ = s.db.Exec("UPDATE api_keys SET last_used_at = ? WHERE id = ?", nowText(), matched.ID)
	return matched, nil
}

// MatchIPAllowlist 报告 ipStr 是否命中 ipAllowlist（逗号分隔 CIDR 或单 IP；空=不限）。
func MatchIPAllowlist(ipAllowlist, ipStr string) bool {
	ipAllowlist = strings.TrimSpace(ipAllowlist)
	if ipAllowlist == "" {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	for _, item := range strings.Split(ipAllowlist, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Contains(item, "/") {
			if _, netw, err := net.ParseCIDR(item); err == nil && netw.Contains(ip) {
				return true
			}
			continue
		}
		if other := net.ParseIP(item); other != nil && other.Equal(ip) {
			return true
		}
	}
	return false
}
