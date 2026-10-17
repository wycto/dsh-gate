// Package portal 是 dsh-gate 管理端的持久层：用户、实例、授权与审计存 SQLite。
//
// 设计取舍：
//
//   - **一个文件就是全部状态**：`<state-dir>/portal.db`（文件 0600、目录 0700）。
//     管理端是单进程服务，这个量级不需要连接池调优；换来的是一份可以直接备份、
//     直接删掉就能回滚的数据库。
//   - **打开时自动迁移**：结构版本存在 `PRAGMA user_version` 里，比程序认识的
//     版本更高就**拒绝启动**（与 internal/users 对 users.json 的处理一致）——
//     宁可起不来让人去升级，也不要用旧代码去写新结构。
//   - **时间统一存 RFC3339Nano 的 UTC 文本**：SQLite 没有原生时间类型，
//     文本比较即时间比较，跨平台稳定，也不受驱动时区处理影响。
//   - **连接数固定为 1**：管理端的每次读写都很短，串行化换来的是「永远不会
//     遇到 database is locked」，也省掉一层重试逻辑。
//   - **口令哈希复用 internal/users**：老账号从 users.json 迁移过来时，
//     哈希串原样可用，不需要任何人改口令。
package portal

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动、无 CGO：六个发行目标仍可交叉编译
)

// SchemaVersion 是当前数据库结构版本，写入 PRAGMA user_version。
//
// v2 给 instances 加了 install_mode / install_dir / config_dir / listen_host /
// start_mode 五列（管理台创建实例时选安装方式与启动方式）。
const SchemaVersion = 2

// ErrNotFound 表示按 id / 名字查不到记录。
var ErrNotFound = errors.New("记录不存在")

// Store 是管理端数据库。并发安全（内部把连接数压到 1，写操作天然串行）。
type Store struct {
	db   *sql.DB
	path string

	// dummyHash 只在「用户不存在」时参与一次校验，让登录耗时与「用户存在但
	// 口令错」保持同一量级，消掉枚举账号的时间侧信道。
	dummyHash string
}

// schemaV1 是结构版本 1 的建表语句。只增不改：后续版本另写迁移。
const schemaV1 = `
CREATE TABLE users (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT    NOT NULL UNIQUE,
  display_name TEXT    NOT NULL DEFAULT '',
  hash         TEXT    NOT NULL,
  role         TEXT    NOT NULL DEFAULT 'user',
  disabled     INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT    NOT NULL,
  updated_at   TEXT    NOT NULL
);

CREATE TABLE instances (
  id             TEXT    PRIMARY KEY,
  name           TEXT    NOT NULL UNIQUE,
  owner_id       INTEGER REFERENCES users(id) ON DELETE SET NULL,
  dsh_home       TEXT    NOT NULL,
  workspace      TEXT    NOT NULL DEFAULT '',
  dsh_port       INTEGER NOT NULL,
  dsh_cmd        TEXT    NOT NULL DEFAULT '',
  on_demand      INTEGER NOT NULL DEFAULT 0,
  idle_timeout_s INTEGER NOT NULL DEFAULT 0,
  enabled        INTEGER NOT NULL DEFAULT 1,
  created_at     TEXT    NOT NULL,
  updated_at     TEXT    NOT NULL
);

CREATE TABLE grants (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  instance_id TEXT    NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  created_at  TEXT    NOT NULL,
  created_by  TEXT    NOT NULL DEFAULT '',
  PRIMARY KEY (user_id, instance_id)
);
CREATE INDEX grants_instance_idx ON grants(instance_id);

CREATE TABLE audit (
  id     INTEGER PRIMARY KEY AUTOINCREMENT,
  at     TEXT    NOT NULL,
  actor  TEXT    NOT NULL DEFAULT '',
  action TEXT    NOT NULL,
  target TEXT    NOT NULL DEFAULT '',
  detail TEXT    NOT NULL DEFAULT '',
  ip     TEXT    NOT NULL DEFAULT '',
  ok     INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX audit_at_idx ON audit(at DESC);
`

// schemaV2 给实例加上「安装方式」相关的列。只增不改：v1 库升级时用 ALTER TABLE
// 逐列补上，已有记录读出来是空串，由 Instance.applyDefaults 归一成 npx / auto /
// 127.0.0.1——老实例的行为与升级前完全一致。
//
// 每列都给 NOT NULL DEFAULT：SQLite 的 ALTER TABLE ADD COLUMN 在有默认值时是
// 常数时间操作，不需要重建表，老库再大也是秒级。
const schemaV2 = `
ALTER TABLE instances ADD COLUMN install_mode TEXT NOT NULL DEFAULT 'npx';
ALTER TABLE instances ADD COLUMN install_dir  TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN config_dir   TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN listen_host  TEXT NOT NULL DEFAULT '127.0.0.1';
ALTER TABLE instances ADD COLUMN start_mode   TEXT NOT NULL DEFAULT 'auto';
`

// Open 打开（必要时创建并迁移）管理端数据库。
//
// 文件不存在不算错误：建一个空库。文件存在但结构版本比程序更新则报错，
// 不静默降级——那样只会把数据写坏。
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析管理端数据库路径失败 %q: %w", path, err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建管理端数据库目录失败 %q: %w", dir, err)
	}
	// 先以 0600 建出文件：数据库从诞生起就只有属主可读（与 users.json 同一约定）。
	if f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		_ = f.Close()
	}

	db, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("打开管理端数据库失败 %q: %w", abs, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db, path: abs}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接管理端数据库失败 %q: %w", abs, err)
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// 未知账号也要付出一次真实 KDF 的代价。
	if s.dummyHash, err = hashPassword(randomDummySecret()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化口令校验桩失败: %w", err)
	}
	return s, nil
}

// dsn 组出 SQLite 的连接串。pragma 写在连接串里而不是开库后 Exec：
// 这样每一条新连接都自带这些设置，不依赖「只有一个连接」这个假设。
func dsn(abs string) string {
	return "file:" + filepath.ToSlash(abs) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

// Close 关闭数据库。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path 返回数据库文件的绝对路径。
func (s *Store) Path() string { return s.path }

// migrate 把结构升到 SchemaVersion。已经是最新则什么也不做。
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读取管理端数据库结构版本失败: %w", err)
	}
	if version > SchemaVersion {
		return fmt.Errorf("管理端数据库 %q 的结构版本 %d 比当前程序支持的 %d 更高，请升级 dsh-gate", s.path, version, SchemaVersion)
	}
	if version == SchemaVersion {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("开启管理端数据库迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if version < 1 {
		if _, err := tx.Exec(schemaV1); err != nil {
			return fmt.Errorf("创建管理端数据库结构失败: %w", err)
		}
	}
	if version < 2 {
		if _, err := tx.Exec(schemaV2); err != nil {
			return fmt.Errorf("升级管理端数据库到结构版本 2 失败: %w", err)
		}
	}
	// PRAGMA 不接受参数占位符；版本号来自本包常量，不涉及外部输入。
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("写入管理端数据库结构版本失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交管理端数据库迁移失败: %w", err)
	}
	return nil
}

// ---- 时间与布尔的小工具（SQLite 没有原生类型） ----

func nowText() string { return formatTime(time.Now()) }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func intToBool(v int64) bool { return v != 0 }

// ---- 实例 id 与端口分配 ----

// instanceIDLen 是对外路径里那段不透明 id 的长度（8 个 base32 小写字符 = 40 bit）。
const instanceIDLen = 8

// idAlphabet 是 RFC 4648 base32 的小写形式：全小写、无连字符，直接可放进 URL 路径。
const idAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// NewInstanceID 生成一个不透明实例 id。
//
// 取每字节的高 5 位映射到 32 个字符：每个字符出现概率相同（无取模偏置），
// 且结果只含小写字母与数字 2-7，满足路径白名单 `^[a-z0-9]{8}$`。
func NewInstanceID() (string, error) {
	buf := make([]byte, instanceIDLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成实例 id 失败: %w", err)
	}
	for i, b := range buf {
		buf[i] = idAlphabet[int(b)>>3]
	}
	return string(buf), nil
}

// DefaultPortBase / DefaultPortSpan 是自动分配 dsh 回环端口时的默认区间。
const (
	DefaultPortBase = 32100
	DefaultPortSpan = 200
)

// AllocatePort 在 [base, base+span) 里挑一个没被实例表占用的端口。
// base/span 非正时用默认值。
func (s *Store) AllocatePort(base, span int) (int, error) {
	if base <= 0 {
		base = DefaultPortBase
	}
	if span <= 0 {
		span = DefaultPortSpan
	}
	rows, err := s.db.Query("SELECT dsh_port FROM instances")
	if err != nil {
		return 0, fmt.Errorf("读取已占用端口失败: %w", err)
	}
	defer rows.Close()
	used := make(map[int]bool)
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			return 0, fmt.Errorf("读取已占用端口失败: %w", err)
		}
		used[port] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读取已占用端口失败: %w", err)
	}
	for p := base; p < base+span; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("端口段 %d-%d 已经用完，请显式指定端口或扩大区间", base, base+span-1)
}
