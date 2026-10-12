package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// Sessions 是内存中的登录会话表。
//
// 为什么只放内存、不落盘：
//
//   - 会话表里是「谁已登录」的凭据，落盘就多一份要保护的文件；
//   - 网关重启后强制所有人重新登录，对一个家用/办公用的自建网关来说完全
//     可以接受，而换来的是「重启即清空」，不存在过期会话长期滞留的问题。
//
// 内存的代价是多网关实例之间不共享会话——每个实例本来就该有自己的一套账号。
type Sessions struct {
	mu      sync.Mutex
	entries map[string]time.Time
	ttl     time.Duration

	// ops 用来做惰性清理：每写满一批操作就顺手扫一遍过期项，避免定时器
	// goroutine 的生命周期管理，也避免 map 只增不减。
	ops int
}

// NewSessions 创建会话表。
func NewSessions(ttl time.Duration) *Sessions {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	return &Sessions{entries: map[string]time.Time{}, ttl: ttl}
}

// Create 生成一个新的随机会话令牌。
func (s *Sessions) Create() string {
	token := randomToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[token] = time.Now().Add(s.ttl)
	s.maybeSweepLocked()
	return token
}

// Valid 检查令牌是否有效，并在有效时顺延过期时间（滑动过期）。
//
// 滑动过期意味着「一直在用」的会话不会莫名过期，而被偷走的令牌也有最长
// TTL 兜底——只要没人再用它。
func (s *Sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.entries[token]
	if !ok || time.Now().After(expiry) {
		if ok {
			delete(s.entries, token)
		}
		return false
	}
	s.entries[token] = time.Now().Add(s.ttl)
	return true
}

// Destroy 作废一个会话。
func (s *Sessions) Destroy(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, token)
}

// Count 返回当前有效会话数。
func (s *Sessions) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	return len(s.entries)
}

// sweepLocked 清理过期会话。调用方必须持有锁。
func (s *Sessions) sweepLocked() {
	now := time.Now()
	for token, expiry := range s.entries {
		if now.After(expiry) {
			delete(s.entries, token)
		}
	}
}

// maybeSweepLocked 每 64 次操作清理一次。调用方必须持有锁。
func (s *Sessions) maybeSweepLocked() {
	s.ops++
	if s.ops%64 == 0 {
		s.sweepLocked()
	}
}

// randomToken 生成 256 位随机会话令牌。
func randomToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败属于不可恢复的环境问题，此处没有合理的降级路径：
		// 用可预测的令牌发会话，等于把所有会话送人。
		panic("读取系统随机数失败，无法创建会话: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
