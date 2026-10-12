package auth

import (
	"sync"
	"time"
)

// MaxLockout 是单个 IP 被锁定的时长上限。
//
// 15 分钟是个刻意选的折中：足够让自动化爆破在经济上不划算，又不至于让
// 一个手滑输错三次的人第二天早上还进不去。
const MaxLockout = 15 * time.Minute

// Limiter 按来源 IP 做登录失败限速。
//
// 威胁模型很具体：网关的登录页是公网/局域网里唯一需要口令的地方，攻击者
// 会拿它做在线字典爆破。这里按 IP 计数，连续失败达到阈值后锁定该 IP，且
// **每次再失败都把锁定时间翻倍**——指数退避让持续爆破的成本指数上升，
// 而偶尔手滑输错的人顶多等一分钟。
//
// 刻意不做「全局限速」：同一办公室几十人共用一个出口 IP，全局限速会误伤
// 正常用户。IP 级限速 + 口令本身的强度，是这里合适的组合。
type Limiter struct {
	mu       sync.Mutex
	records  map[string]*attempt
	maxFails int
	base     time.Duration
	// gcAfter 决定多久没动静的记录可以被回收，防止 map 被大量一次性 IP 撑大。
	gcAfter time.Duration
}

// attempt 是单个 IP 的失败记录。
type attempt struct {
	fails   int
	until   time.Time // 锁定截止时间
	updated time.Time // 最后一次活动时间
}

// NewLimiter 创建限速器。
func NewLimiter(maxFails int, base time.Duration) *Limiter {
	if maxFails < 1 {
		maxFails = 5
	}
	if base <= 0 {
		base = time.Minute
	}
	return &Limiter{
		records:  map[string]*attempt{},
		maxFails: maxFails,
		base:     base,
		gcAfter:  time.Hour,
	}
}

// Allow 判断该 IP 现在能不能尝试登录。
//
// 第二个返回值是建议的等待时长（未锁定时为 0），用于给用户一句清楚的提示，
// 而不是干巴巴一句「too many requests」。
func (l *Limiter) Allow(ip string) (time.Duration, bool) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)

	record, ok := l.records[ip]
	if !ok {
		return 0, true
	}
	if now.Before(record.until) {
		return record.until.Sub(now), false
	}
	return 0, true
}

// Fail 记录一次失败，并在达到阈值时开始锁定。
func (l *Limiter) Fail(ip string) time.Duration {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	record, ok := l.records[ip]
	if !ok {
		record = &attempt{}
		l.records[ip] = record
	}
	// 锁定已经到期后重新计数，否则一次等待就会把历史失败数带过来。
	if !record.until.IsZero() && now.After(record.until) {
		record.fails = 0
	}
	record.fails++
	record.updated = now

	if record.fails >= l.maxFails {
		// 第 maxFails 次失败锁 base，每多失败一次翻倍，封顶 15 分钟。
		shift := record.fails - l.maxFails
		if shift > 10 {
			shift = 10
		}
		record.until = now.Add(min(l.base<<shift, MaxLockout))
	}
	return time.Until(record.until)
}

// Reset 在登录成功后清空该 IP 的失败记录。
func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.records, ip)
}

// gcLocked 回收长时间没有活动的记录。调用方必须持有锁。
func (l *Limiter) gcLocked(now time.Time) {
	if len(l.records) == 0 {
		return
	}
	for ip, record := range l.records {
		if now.Sub(record.updated) > l.gcAfter {
			delete(l.records, ip)
		}
	}
}
