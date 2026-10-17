package auth

import (
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/i18n"
)

func TestSafeNext(t *testing.T) {
	// 只接受站内绝对路径。任何可能把用户「登录后跳到别处」的形式都必须回落到首页，
	// 因为登录后的跳转是开放重定向最经典的落点。
	cases := map[string]string{
		"":                     "/",
		"/":                    "/",
		"/chat":                "/chat",
		"/chat?x=1&y=2":        "/chat?x=1&y=2",
		"//evil.test":          "/", // 协议相对 URL：会被浏览器当外站
		"https://evil.test":    "/",
		"/\\evil.test":         "/",
		"http://evil.test/x":   "/",
		"/path\nSet-Cookie: x": "/",
		"relative/path":        "/",
		"javascript:alert(1)":  "/",
	}
	for input, want := range cases {
		if got := safeNext(input); got != want {
			t.Errorf("safeNext(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Millisecond:       "1 秒",
		30 * time.Second:             "30 秒",
		90 * time.Second:             "1 分 30 秒",
		2 * time.Minute:              "2 分钟",
		2*time.Hour + 15*time.Minute: "2 小时 15 分",
		3 * time.Hour:                "3 小时",
		90 * 24 * time.Hour:          "2160 小时",
	}
	for input, want := range cases {
		if got := humanDuration(input, i18n.ZH); got != want {
			t.Errorf("humanDuration(%s, zh) = %q，期望 %q", input, got, want)
		}
	}

	// 英文走同一套分支，但用的是英文目录。
	english := map[time.Duration]string{
		500 * time.Millisecond:       "1s",
		30 * time.Second:             "30s",
		90 * time.Second:             "1m 30s",
		2 * time.Minute:              "2m",
		2*time.Hour + 15*time.Minute: "2h 15m",
		3 * time.Hour:                "3h",
	}
	for input, want := range english {
		if got := humanDuration(input, i18n.EN); got != want {
			t.Errorf("humanDuration(%s, en) = %q，期望 %q", input, got, want)
		}
	}
}

func TestSessions(t *testing.T) {
	sessions := NewSessions(time.Hour)
	token := sessions.Create("alice")
	if token == "" {
		t.Fatal("令牌不应为空")
	}
	if !sessions.Valid(token) {
		t.Error("新建的会话应当有效")
	}
	// 会话必须记得「是谁」：管理端按登录名查实例授权。
	if subject, ok := sessions.Subject(token); !ok || subject != "alice" {
		t.Errorf("会话主体应为 alice，实际 %q ok=%v", subject, ok)
	}
	if _, ok := sessions.Subject("never-issued"); ok {
		t.Error("伪造令牌不该有主体")
	}
	if sessions.Valid("") {
		t.Error("空令牌不应有效")
	}
	if sessions.Valid("never-issued") {
		t.Error("伪造的令牌不应有效")
	}
	if sessions.Count() != 1 {
		t.Errorf("会话数 = %d", sessions.Count())
	}
	sessions.Destroy(token)
	if sessions.Valid(token) {
		t.Error("销毁后的令牌不应有效")
	}
}

// TestDestroySessionsOfSubject 覆盖管理端的「改密 / 停用后把人踢下线」。
func TestDestroySessionsOfSubject(t *testing.T) {
	sessions := NewSessions(time.Hour)
	aliceA := sessions.Create("alice")
	aliceB := sessions.Create("alice")
	bob := sessions.Create("bob")

	if n := sessions.DestroySubject("alice"); n != 2 {
		t.Errorf("应作废 alice 的 2 个会话，实际 %d", n)
	}
	if sessions.Valid(aliceA) || sessions.Valid(aliceB) {
		t.Error("alice 的会话应全部失效")
	}
	if !sessions.Valid(bob) {
		t.Error("不该影响别人的会话")
	}
	if n := sessions.DestroySubject(""); n != 0 {
		t.Errorf("空主体不该作废任何会话，实际 %d", n)
	}
}

func TestSessionsExpire(t *testing.T) {
	sessions := NewSessions(time.Millisecond)
	token := sessions.Create("alice")
	time.Sleep(5 * time.Millisecond)
	if sessions.Valid(token) {
		t.Error("过期的会话不应有效")
	}
	if sessions.Count() != 0 {
		t.Errorf("过期会话应当被清掉，实际剩 %d", sessions.Count())
	}
}

func TestLimiterBackoff(t *testing.T) {
	limiter := NewLimiter(3, time.Minute)

	// 失败次数未达阈值（3 次）前不锁定。
	for i := 0; i < 2; i++ {
		if wait, ok := limiter.Allow("1.2.3.4"); !ok {
			t.Fatalf("第 %d 次失败后不应锁定，却提示等待 %s", i+1, wait)
		}
		limiter.Fail("1.2.3.4")
	}
	limiter.Fail("1.2.3.4") // 第 3 次：达到阈值
	if _, ok := limiter.Allow("1.2.3.4"); ok {
		t.Fatal("达到阈值后应当锁定")
	}

	// 其它 IP 不受影响：办公室里几十人共用一个出口 IP，全局限速会误伤。
	if _, ok := limiter.Allow("5.6.7.8"); !ok {
		t.Error("锁定不应影响其它 IP")
	}

	// 锁定会随失败次数翻倍。
	limiter.Reset("1.2.3.4")
	first := waitFrom(limiter, "1.2.3.4", 3)
	second := waitFrom(limiter, "1.2.3.4", 4)
	if second <= first {
		t.Errorf("连续失败应指数退避：3 次 %s，4 次 %s", first, second)
	}
	if second > MaxLockout {
		t.Errorf("锁定时长 %s 超过上限 %s", second, MaxLockout)
	}

	// 登录成功会清零。
	limiter.Reset("1.2.3.4")
	if _, ok := limiter.Allow("1.2.3.4"); !ok {
		t.Error("重置后不应锁定")
	}
}

// waitFrom 让某个 IP 连续失败 failures 次，返回本次触发的锁定时长。
func waitFrom(limiter *Limiter, ip string, failures int) time.Duration {
	var wait time.Duration
	for i := 0; i < failures; i++ {
		wait = limiter.Fail(ip)
	}
	return wait
}
