// Package dshproc 管理被网关托管的 dsh 进程：探测端口、按需拉起、解析启动
// 令牌、崩溃后自动重启。
//
// 本文件只负责「看」——端口有没有人占、占着的是不是 dsh。真正的「动」在
// supervisor.go。
package dshproc

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// State 描述某个 dsh 端口的现状。
type State struct {
	// InUse 表示 TCP 连接能被建立，即端口已经被某个进程监听。
	InUse bool
	// DSH 表示高度可能监听的就是一个 dsh 实例。
	//
	// 注意这是**启发式判断**而不是协议握手：dsh 没有提供身份端点，我们只能
	// 通过它公开的行为（首页是 SPA、/api/remote.mux 拒绝普通 GET）来推断。
	// 因此宁可把它当作「大概率是」，也不要在这上面做安全决策——是否允许
	// 接管由配置决定，这里只负责给人一个准确的说法。
	DSH bool
	// PID 是占用端口的进程号，查不到时为 0。
	PID int
	// Detail 是给人看的一句话结论，会原样打进 `dsh-gate check` 的输出。
	Detail string
}

// probeTimeout 是识别 dsh 时的单次 HTTP 超时。给得短，因为这一步通常发生
// 在启动路径上，用户正等着；给得长则一个假死的端口会拖住整个启动。
const probeTimeout = 3 * time.Second

// spaMarker 匹配 dsh 前端首页的典型特征。
//
// dsh 的 Web GUI 是个单页应用，首页是 HTML 外壳，标题与静态资源路径里带
// dsh/deepseek 字样。这里不写死具体字符串（前端随时可能改名），而是匹配
// 一组宽容的特征，命中任意一条就算「像 dsh」。
var spaMarker = regexp.MustCompile(`(?i)(deepseek|<title>[^<]*dsh|/dsh|\bdsh\b)`)

// Inspect 检查 addr 上是否有人监听，以及监听的是不是 dsh。
//
// addr 形如 127.0.0.1:3080。timeout 只作用于后面的 HTTP 识别步骤。
func Inspect(addr string, timeout time.Duration) State {
	if timeout <= 0 {
		timeout = probeTimeout
	}
	state := State{}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		state.Detail = fmt.Sprintf("地址 %q 无法解析: %v", addr, err)
		return state
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		state.Detail = fmt.Sprintf("端口 %q 不是数字: %v", portText, err)
		return state
	}
	if host == "" {
		host = "127.0.0.1"
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, portText), timeout)
	if err != nil {
		state.Detail = "端口空闲（无人监听）"
		return state
	}
	_ = conn.Close()
	state.InUse = true
	state.PID = findPID(port)

	dsh, detail := identifyDSH(net.JoinHostPort(host, portText), state.PID, timeout)
	state.DSH = dsh
	state.Detail = detail
	return state
}

// Busy 报告地址上是否已经有进程在监听。
//
// 与 Inspect 的区别只在于它不做 HTTP 探针：判断「网关端口有没有被占用」
// 只需要一次 TCP 连接，不需要知道对面是什么服务。
func Busy(addr string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = probeTimeout
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// identifyDSH 通过两个 HTTP 探针判断端口后面是不是 dsh，返回判断与一句说明。
func identifyDSH(addr string, pid int, timeout time.Duration) (bool, string) {
	client := &http.Client{
		Timeout: timeout,
		// 不跟随跳转：dsh 在拿到合法令牌后才会 302，我们要看的正是跳转之前
		// 那一步的响应，跟过去只会看到别的东西。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// 探针一：首页。dsh 的首页是公开的 SPA 外壳，所以匿名也能拿到 HTML。
	root := fetch(client, "GET", "http://"+addr+"/")

	// 探针二：dsh 的 API 网关 WebSocket 端点。普通 GET 打过去会被拒
	// （400/426/401 之类），一个无关的 Web 服务器多半是 404。
	gateway := fetch(client, "GET", "http://"+addr+"/api/remote.mux")

	if root.status == 0 && gateway.status == 0 {
		return false, fmt.Sprintf("端口有人监听但 HTTP 无响应（可能是非 HTTP 协议）；占用进程 PID=%s", pidOrUnknown(pid))
	}

	looksLikeSPA := root.status == 200 && spaMarker.MatchString(root.body)
	// 网关端点「拒绝了一个不该有的请求」也算特征：404 说明这个服务根本不是 dsh。
	hasGateway := gateway.status > 0 && gateway.status != http.StatusNotFound

	switch {
	case looksLikeSPA && hasGateway:
		return true, fmt.Sprintf("已识别为 dsh（HTTP %d，SPA + /api 网关），占用进程 PID=%s", root.status, pidOrUnknown(pid))
	case looksLikeSPA:
		return true, fmt.Sprintf("疑似 dsh（HTTP %d 首页像 SPA，但未确认 /api 网关），占用进程 PID=%s", root.status, pidOrUnknown(pid))
	case root.status > 0 && spaMarker.MatchString(root.body):
		return true, fmt.Sprintf("疑似 dsh（HTTP %d，首页含 dsh 特征），占用进程 PID=%s", root.status, pidOrUnknown(pid))
	}
	return false, fmt.Sprintf("端口已被占用，但不是 dsh（首页 HTTP %d、%s），占用进程 PID=%s；可用 lsof/netstat 确认",
		root.status, describeStatus(root), pidOrUnknown(pid))
}

// describeStatus 把首页响应描述成人话，用于「不是 dsh」的提示。
func describeStatus(res probeResult) string {
	switch {
	case res.status == 0:
		return "无响应"
	case res.status >= 200 && res.status < 300:
		return "返回了普通网页"
	case res.status == http.StatusUnauthorized || res.status == http.StatusForbidden:
		return "需要认证"
	default:
		return "返回 " + strconv.Itoa(res.status)
	}
}

// probeResult 是一次探针的结论。
type probeResult struct {
	status int
	body   string
}

// fetch 发一次探针请求。Host 头显式写成目标地址：dsh 的 Host/Origin 栅栏
// 只接受回环或被点名的 authority，这里走回环，天然在允许名单里。
func fetch(client *http.Client, method, rawURL string) probeResult {
	ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return probeResult{}
	}
	req.Header.Set("User-Agent", "dsh-gate-probe")
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return probeResult{}
	}
	defer resp.Body.Close()

	// 只读前 64 KiB：足够判断身份，又不会因为对方的响应体很大而拖住探测。
	// 读取有上限也顺带避免被超大响应撑爆内存。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return probeResult{status: resp.StatusCode, body: string(body)}
}

// pidOrUnknown 把 0 渲染成问号，避免输出里出现「PID=0」这种看着像真的值。
func pidOrUnknown(pid int) string {
	if pid <= 0 {
		return "未知"
	}
	return strconv.Itoa(pid)
}

// WaitPort 等待 addr 上的 TCP 端口可用（有人在监听）。
//
// 用于「刚 fork 出 dsh 子进程」到「可以开始转发」之间的等待。返回 nil 表示
// 端口就绪；返回 ctx 的错误表示超时或被取消。
func WaitPort(ctx context.Context, addr string, poll time.Duration) error {
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var lastErr error
	for {
		dialer := net.Dialer{Timeout: 500 * time.Millisecond}
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("等待 %s 就绪超时: %w（最后一次连接失败: %v）", addr, ctx.Err(), lastErr)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// SplitToken 从 dsh 启动输出的一行里抽出启动令牌。
//
// dsh 打印的是一行形如
//
//	dsh web: http://192.168.1.20:3082/?token=6f1c…
//
// 这里不依赖那行前缀（不同版本措辞可能变），只找 `token=` 后面那一段。
// 返回空串表示这行里没有令牌。
func SplitToken(line string) string {
	idx := strings.Index(line, "token=")
	if idx < 0 {
		return ""
	}
	rest := line[idx+len("token="):]
	end := strings.IndexAny(rest, " \t\r\n\"'&<>")
	if end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}
