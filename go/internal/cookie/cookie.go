// Package cookie 集中定义 dsh-gate 用到的 Cookie 名与读写方式。
//
// 单独成包只有一个理由：登录会话（auth 包）与 dsh 令牌交接标记（proxy 包）
// 都得用到这两个名字，而它们互不依赖。放在一处定义，可以保证「设置」与
// 「清除」永远用同一套属性——Secure/Path 对不上是最常见的登录状态丢失原因。
package cookie

import (
	"net/http"
	"strings"
	"time"
)

const (
	// Session 是网关自己的登录会话 Cookie。
	//
	// 名字带 dsh_gate_ 前缀，这样即使与 dsh 自己的会话 Cookie 同处一个
	// 浏览器，也不会互相覆盖。
	Session = "dsh_gate_sid"

	// Handoff 标记「这个浏览器已经向 dsh 换过会话」。
	//
	// 它不是凭据，值恒为 1，作用是让网关知道下次要不要再帮浏览器做一次
	// 启动令牌交换。详见 proxy 包的说明。
	Handoff = "dsh_gate_ds"
)

// Options 是写 Cookie 时的公共属性。
type Options struct {
	// Secure 表示只允许通过 HTTPS 发送。网关启用了 TLS 就必须开，
	// 否则会话 Cookie 会以明文过网。
	Secure bool
	// MaxAge 是有效期；<= 0 表示会话 Cookie（浏览器关闭即失效）。
	MaxAge time.Duration
}

// Set 写一个 HttpOnly Cookie。
//
// HttpOnly 让前端脚本读不到它：万一 dsh 的页面里出现 XSS，攻击者脚本也偷不走
// 网关的会话。SameSite=Lax 是必须的——dsh 自己的会话 Cookie 是 Strict，
// 而网关这一跳属于「同站导航」，Lax 既能保持会话，又不会把 Cookie 附在跨站
// 请求上。
func Set(w http.ResponseWriter, name, value string, opts Options) {
	http.SetCookie(w, Value(name, value, opts))
}

// Clear 让浏览器删除一个 Cookie。
//
// MaxAge 必须是负数：写 MaxAge=0 只会把值清空，浏览器仍然会把它带上；
// 真正删除需要 MaxAge<0 加过期时间。
func Clear(w http.ResponseWriter, name string, opts Options) {
	http.SetCookie(w, ClearValue(name, opts))
}

// Value 构造 Cookie 对象。
//
// 单独暴露它，是因为反向代理需要在 ModifyResponse 阶段种 Cookie：那时响应
// 已经交给 httputil.ReverseProxy 拿着，还没有 ResponseWriter 可用，只能往
// 响应头里直接加 Set-Cookie。
func Value(name, value string, opts Options) *http.Cookie {
	return build(name, value, opts)
}

// ClearValue 构造一个「让浏览器删除该 Cookie」的 Cookie 对象。
func ClearValue(name string, opts Options) *http.Cookie {
	return build(name, "", Options{Secure: opts.Secure, MaxAge: -time.Hour})
}

func build(name, value string, opts Options) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   opts.Secure,
		SameSite: http.SameSiteLaxMode,
	}
	switch {
	case opts.MaxAge > 0:
		c.MaxAge = int(opts.MaxAge.Seconds())
		c.Expires = time.Now().Add(opts.MaxAge)
	case opts.MaxAge < 0:
		// 负值表示「删除」。net/http 会把它渲染成 Max-Age=0 加一个过去的
		// Expires——这才是各浏览器都认的删除写法（Max-Age=0 单写是不够的）。
		c.MaxAge = int(opts.MaxAge.Seconds())
		c.Expires = time.Now().Add(opts.MaxAge)
	default:
		// 会话 Cookie：明确给一个过去的过期时间，防止某些浏览器把
		// 无 MaxAge 的 Cookie 永久留在磁盘上。
		c.Expires = time.Unix(0, 0)
	}
	return c
}

// Get 读取指定名称的 Cookie，返回空串表示没有。
func Get(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// ClientIP 返回请求的来源 IP。
//
// 刻意**不**采信 X-Forwarded-For / X-Real-IP：网关就是链路的最外层，除非
// 明确知道前面还挂着自家反向代理，否则这些头任何人都能伪造，拿来限速等于
// 限了个寂寞。
func ClientIP(r *http.Request) string {
	host, _, err := splitHostPort(r.RemoteAddr)
	if err != nil || host == "" {
		return r.RemoteAddr
	}
	return host
}

// splitHostPort 从 RemoteAddr 里取出主机部分。
//
// 逐字节倒着找冒号，因此能处理 IPv6 的 "[2408:826a::1]:443"；方括号是
// RemoteAddr 的写法，去掉后才是可以拿去做限速键的裸地址。
func splitHostPort(addr string) (host, port string, err error) {
	for i := len(addr) - 1; i >= 0; i-- {
		switch addr[i] {
		case ':':
			host = addr[:i]
			if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
				host = host[1 : len(host)-1]
			}
			return host, addr[i+1:], nil
		case ']':
			// IPv6 且没有端口：整个串就是地址。
			if strings.HasPrefix(addr, "[") {
				return addr[1 : len(addr)-1], "", nil
			}
			return addr, "", nil
		}
	}
	return addr, "", nil
}
