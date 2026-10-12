// Package users 负责网关账号表：把口令哈希后存到本地 JSON 文件，并提供校验。
//
// 设计取舍：
//
//   - **只存哈希，不存明文**。哈希串自带算法名、迭代次数与盐，可以随时识别
//     老格式并平滑升级，不用迁移整个文件。
//   - **文件权限 0600、目录 0700**。账号表等价于一把万能钥匙，同机的其它
//     用户不该读得到。
//   - **原子写**（临时文件 + rename）。写一半掉电不会留下半截 JSON——那会让
//     整个账号表读不出来，等于把所有人锁在门外。
//   - **用户不存在时也走一遍 KDF**。否则「用户不存在」比「口令错」快几十到
//     几百倍，攻击者可以据此枚举账号。dummyVerify 消掉了这个时间差。
package users

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// DefaultIterations 是 PBKDF2-HMAC-SHA256 的默认迭代次数，遵循 OWASP 的
	// 推荐值。改这个值不会让老账号失效：每条记录都带着自己那份迭代次数。
	DefaultIterations = 600_000

	// minIterations / maxIterations 夹住调用方给的值。太低会让口令哈希形同
	// 虚设，太高则会让小主机（如 NAS、树莓派）上的登录卡到无法接受。
	minIterations = 50_000
	maxIterations = 5_000_000

	saltBytes = 16
	keyBytes  = 32

	// maxUserRunes 是用户名的长度上限。太长的名字没有实际意义，只会制造
	// 日志与文件名上的麻烦。
	maxUserRunes = 64
)

// ErrNoSuchUser 表示账号不存在。
var ErrNoSuchUser = errors.New("账号不存在")

// Record 是一条账号记录。
type Record struct {
	// Hash 是自描述的口令哈希串：pbkdf2-sha256$迭代次数$盐$摘要。
	Hash string `json:"hash"`
	// CreatedAt / UpdatedAt 只用于展示与排查，不参与任何认证判断。
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// fileData 是账号表文件在磁盘上的原样结构。
type fileData struct {
	Version int               `json:"version"`
	Users   map[string]Record `json:"users"`
}

// currentVersion 是当前写出的文件版本号。将来若要换格式（比如加 TOTP 密钥），
// 读旧文件时按版本分支即可，不必猜测。
const currentVersion = 1

// Store 是账号表。并发安全。
type Store struct {
	path string

	mu   sync.RWMutex
	data fileData

	// modTime / size 是上次读文件时的 stat 标记，供 refresh 判断文件是否被
	// 本进程之外的 user add / passwd / rm 改过。零值表示「还没读到过文件」。
	modTime time.Time
	size    int64

	// dummyHash 是一个真实的哈希串，只在「账号不存在」时参与一次校验，
	// 让登录耗时与「账号存在但口令错」保持同一量级。
	dummyHash string
}

// Open 打开（或创建）位于 path 的账号表。
//
// 文件不存在不算错误：返回一个空表，等用户用 `dsh-gate user add` 写入。
// 文件存在但内容损坏则**报错**而不是静默重建——静默重建等于把所有人都
// 锁在门外且毫无线索，远不如启动失败让人去查备份。
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析账号表路径失败 %q: %w", path, err)
	}

	s := &Store{
		path: abs,
		data: fileData{Version: currentVersion, Users: map[string]Record{}},
	}
	// dummyHash 用一个随机口令生成，成本与真实校验相同。
	s.dummyHash, err = HashPassword(randomPassword())
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("读取账号表失败 %q: %w", abs, err)
	}

	var data fileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("账号表 %q 不是合法 JSON（请修复或备份后删除重建）: %w", abs, err)
	}
	if data.Version > currentVersion {
		return nil, fmt.Errorf("账号表 %q 的版本 %d 比当前程序更新的 %d 高，请升级 dsh-gate", abs, data.Version, currentVersion)
	}
	if data.Users == nil {
		data.Users = map[string]Record{}
	}
	data.Version = currentVersion
	s.data = data
	s.markStat()
	return s, nil
}

// markStat 记录当前文件大小与修改时间，作为 refresh 的比对基准。
// 文件不存在时保持零值，第一次有进程写出文件后就会触发一次重读。
func (s *Store) markStat() {
	if info, err := os.Stat(s.path); err == nil {
		s.modTime, s.size = info.ModTime(), info.Size()
	}
}

// refresh 把本进程之外对账号表文件的修改捡进来。
//
// 网关进程只在启动时 Open 一次，而 user add / passwd / rm 是另一个进程直接
// 改文件；不重读的话，改完口令必须重启网关才生效——这正是「明明改了密码
// 还是登不上」的来源。以 stat 标记决定要不要重读：登录本来就低频，每次一次
// os.Stat 的代价可以忽略。
func (s *Store) refresh() {
	info, err := os.Stat(s.path)
	if err != nil {
		return // 文件暂时读不到（被删、被换）：沿用内存里的表，下次再试
	}
	s.mu.RLock()
	same := info.ModTime().Equal(s.modTime) && info.Size() == s.size
	s.mu.RUnlock()
	if same {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// 拿到写锁后复核一次：可能别的 goroutine 已经完成了这次重读。
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var data fileData
	if err := json.Unmarshal(raw, &data); err != nil {
		// 写入方走原子 rename，读到半截只可能是极端竞态：放弃这一次，
		// 保住内存里可用的表，下一次校验会再试。
		return
	}
	if data.Version > currentVersion {
		return // 更高版本的表留给更新的程序
	}
	if data.Users == nil {
		data.Users = map[string]Record{}
	}
	data.Version = currentVersion
	s.data = data
	s.modTime, s.size = info.ModTime(), info.Size()
}

// Path 返回账号表的绝对路径，用于启动横幅与报错。
func (s *Store) Path() string { return s.path }

// Count 返回账号数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Users)
}

// Exists 报告账号是否存在。
func (s *Store) Exists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data.Users[name]
	return ok
}

// Get 返回账号记录；账号不存在时返回零值与 false。
func (s *Store) Get(name string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.data.Users[name]
	return record, ok
}

// Names 按字典序返回全部账号名。
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.data.Users))
	for name := range s.data.Users {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Verify 校验账号口令。无论账号是否存在都返回 false 与否，成本相当，
// 不给枚举账号留下时间侧信道。
func (s *Store) Verify(name, password string) bool {
	s.refresh()
	s.mu.RLock()
	record, ok := s.data.Users[name]
	target := s.dummyHash
	if ok {
		target = record.Hash
	}
	s.mu.RUnlock()

	if ok {
		return VerifyPassword(target, password)
	}
	// 账号不存在：照样做一次完整校验，把耗时拉平。
	_ = VerifyPassword(target, password)
	return false
}

// Set 新增或覆盖一个账号的口令。
//
// 密码为空、或短于 8 个字符时拒绝：网关前面往往直接就是局域网甚至公网，
// 太短的口令等于没有口令。
func (s *Store) Set(name, password string, iterations int) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	iterations, err := clampIterations(iterations)
	if err != nil {
		return err
	}
	hashed, err := HashPasswordWith(password, iterations)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	record := Record{Hash: hashed, CreatedAt: now, UpdatedAt: now}
	if old, ok := s.data.Users[name]; ok {
		record.CreatedAt = old.CreatedAt
	}
	s.data.Users[name] = record
	return s.persistLocked()
}

// Remove 删除账号。拒绝删除最后一个账号——否则就没有人能再登录了。
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Users[name]; !ok {
		return fmt.Errorf("账号 %q 不存在", name)
	}
	if len(s.data.Users) == 1 {
		return errors.New("拒绝删除最后一个账号（会导致无人能登录）")
	}
	delete(s.data.Users, name)
	return s.persistLocked()
}

// persistLocked 原子写出账号表。调用方必须已持有写锁。
func (s *Store) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建账号表目录失败 %q: %w", dir, err)
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化账号表失败: %w", err)
	}
	raw = append(raw, '\n')

	// 临时文件与目标同目录，保证 rename 是同一文件系统内的原子操作。
	tmp, err := os.CreateTemp(dir, ".users-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败 %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 失败路径上不留下垃圾

	// 先写权限再写内容：文件从诞生起就是 0600，不存在「短暂可读」的窗口。
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("写入账号表失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("落盘账号表失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("替换账号表失败 %q: %w", s.path, err)
	}
	s.markStat()
	return nil
}

// HashPassword 用默认迭代次数生成口令哈希串。
func HashPassword(password string) (string, error) {
	return HashPasswordWith(password, DefaultIterations)
}

// HashPasswordWith 按指定迭代次数生成口令哈希串。
//
// 产出形如 pbkdf2-sha256$600000$<盐>$<摘要>，四段都用 base64url 编码，
// 因此不含 `$` 之外的特殊字符，可以安全地塞进任何文本格式。
func HashPasswordWith(password string, iterations int) (string, error) {
	iterations, err := clampIterations(iterations)
	if err != nil {
		return "", err
	}
	salt := make([]byte, saltBytes)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyBytes)
	if err != nil {
		return "", fmt.Errorf("PBKDF2 失败: %w", err)
	}
	enc := base64.RawURLEncoding
	return strings.Join([]string{
		"pbkdf2-sha256",
		strconv.Itoa(iterations),
		enc.EncodeToString(salt),
		enc.EncodeToString(key),
	}, "$"), nil
}

// VerifyPassword 校验口令哈希串。任何格式异常都返回 false：哈希串来自
// 磁盘，出错时宁可拒绝登录，也不要 panic 或放行。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < minIterations || iterations > maxIterations {
		return false
	}
	enc := base64.RawURLEncoding
	salt, err := enc.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := enc.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	// 定长比较，避免逐字节提前返回泄露前缀信息。
	return subtle.ConstantTimeCompare(got, want) == 1
}

// clampIterations 把迭代次数夹到安全区间。传 0 表示用默认值。
func clampIterations(iterations int) (int, error) {
	if iterations == 0 {
		return DefaultIterations, nil
	}
	if iterations < minIterations || iterations > maxIterations {
		return 0, fmt.Errorf("迭代次数 %d 超出允许范围 %d~%d", iterations, minIterations, maxIterations)
	}
	return iterations, nil
}

// ValidateName 检查用户名字符是否合法。
//
// 允许任意非空白字符即可——刻意不限制字母数字，免得给用户添堵；但拒绝
// 空白与控制字符，因为用户名会出现在日志、Cookie 与命令行参数里。
func ValidateName(name string) error {
	if name == "" {
		return errors.New("用户名不能为空")
	}
	if utf8.RuneCountInString(name) > maxUserRunes {
		return fmt.Errorf("用户名过长（最多 %d 个字符）", maxUserRunes)
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("用户名 %q 不能包含空白或控制字符", name)
		}
	}
	return nil
}

// ValidatePassword 检查口令强度。
func ValidatePassword(password string) error {
	// 按字符数而不是字节数：中文口令不该因为 UTF-8 编码被算得更长。
	switch n := utf8.RuneCountInString(password); {
	case n < 8:
		return errors.New("口令至少 8 个字符")
	case n > 256:
		return fmt.Errorf("口令过长（最多 256 个字符，实际 %d 个）", n)
	}
	return nil
}

// randomPassword 生成一次性随机口令，用在 dummy 校验上。
func randomPassword() string {
	buf := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		// crypto/rand 失败是不可恢复的环境问题；这里退化成时间派生值也
		// 不影响安全性目标（它只是用来拉平耗时的）。
		return fmt.Sprintf("dummy-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
