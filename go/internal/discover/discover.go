// Package discover 实现 dsh-gate 的「服务发现」：本机监听服务发现 + 局域网网段扫描。
// 设计见 docs/service-api.md 第 15 节。纯探测、无写操作、零第三方依赖。
//
// 两类来源：
//   - local：网关所在机器上正在监听的 TCP 服务（带 pid/命令/工作目录，平台相关）；
//   - lan：管理员给定的私有 CIDR + 端口列表，逐 host:port 做短超时 TCP 连接探测。
package discover

import (
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Candidate 是一个「可登记的服务」候选。
type Candidate struct {
	Source  string // local | lan
	Host    string
	Port    int
	PID     int    // local 才有，0 表示未知
	Command string // local 才有（完整命令行）
	Cwd     string // local 才有（工作目录）
	HTTP    bool   // 是否像 HTTP 服务（探测 GET / 成功）
	Title   string // HTTP 时页面 title（可选）
}

// ErrUnsupported 表示本机服务发现在该平台不可用。
var ErrUnsupported = errors.New("本机服务发现在该平台不可用")

// 默认与上限。
const (
	defaultConcurrency = 64
	defaultTimeout     = 500 * time.Millisecond
	defaultMaxPorts    = 1024
	httpProbeTimeout   = 300 * time.Millisecond
	maxHostsPerScan    = 4096
	maxTitleBytes      = 64 << 10
	maxTitleLen        = 200
)

var titleRe = regexp.MustCompile("(?is)<title[^>]*>(.*?)</title>")

// extractTitle 从 HTML 字节里尽力取 <title>，去空白、截断。
func extractTitle(body []byte) string {
	m := titleRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	t := strings.Join(strings.Fields(string(m[1])), " ")
	if len(t) > maxTitleLen {
		t = t[:maxTitleLen]
	}
	return t
}

// probeHTTP 探测 host:port 是否像 HTTP 服务：GET / 返回 2xx-4xx 视为是，并尽力取标题。
// 不跟随重定向；不走环境代理（内网直连）。
func probeHTTP(host string, port int) (bool, string) {
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
	client := &http.Client{
		Timeout:   httpProbeTimeout,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		return false, ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxTitleBytes))
	return true, extractTitle(body)
}
