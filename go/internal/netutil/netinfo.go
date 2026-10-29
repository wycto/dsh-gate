// Package netutil 提供本机网络地址推导与 dsh 的 trusted-host 计算。
//
// 从原 internal/server 抽出来：管理端进程也要用它算 --trusted-host，而那个包
// 承载的是已移除的单实例形态。这里只留纯函数，不依赖任何网关状态。
package netutil

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
)

// LocalAddresses 返回本机可用于对外访问的 IP。
//
// 刻意过滤掉回环与链路本地地址（169.254.x.x、fe80::/10）：它们对别的设备
// 没有意义，写进启动横幅只会让人困惑。IPv4 排在前面，因为绝大多数局域网
// 与手机浏览器场景走的还是 IPv4。
func LocalAddresses() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []string
	seen := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			default:
				continue
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
				ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				continue
			}
			text := ip.String()
			if seen[text] {
				continue
			}
			seen[text] = true
			if ip.To4() != nil {
				v4 = append(v4, text)
			} else {
				v6 = append(v6, text)
			}
		}
	}
	sort.Strings(v4)
	sort.Strings(v6)
	return append(v4, v6...)
}

// DisplayAddresses 返回适合打印给人看的地址列表，按可用性排序后截断。
//
// 排序偏好「私网 IPv4 → 公网 IPv4 → IPv6」，因为绝大多数局域网访问走的是
// 私网 IPv4；而一台开了 IPv6 的机器动辄有六七个全球地址，全打印出来只会把
// 启动横幅淹没。limit <= 0 表示不限制。
func DisplayAddresses(limit int) []string {
	var private, public, v6 []string
	for _, address := range LocalAddresses() {
		ip := net.ParseIP(address)
		switch {
		case ip == nil:
			continue
		case ip.To4() != nil:
			if isPrivateIPv4(ip) {
				private = append(private, address)
			} else {
				public = append(public, address)
			}
		default:
			v6 = append(v6, address)
		}
	}
	ordered := append(append(private, public...), v6...)
	if limit > 0 && len(ordered) > limit {
		return ordered[:limit]
	}
	return ordered
}

// isPrivateIPv4 判断是否 RFC1918 私网地址。
func isPrivateIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	switch {
	case v4[0] == 10:
		return true
	case v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31:
		return true
	case v4[0] == 192 && v4[1] == 168:
		return true
	}
	return false
}

// Hostname 返回本机主机名，拿不到时返回空串。
func Hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// PrimaryAddress 返回最可能用于局域网访问的那个地址。
//
// 没有非回环地址时返回空串——无界面服务器、容器网络隔离等场景都可能这样，
// 届时启动横幅会提示用户自行指定 --public-url。
func PrimaryAddress() string {
	addresses := LocalAddresses()
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0]
}

// authority 把地址拼成 URL 里的 authority，IPv6 需要加方括号。
func authority(address string, port int) string {
	if strings.Contains(address, ":") {
		return fmt.Sprintf("[%s]:%d", address, port)
	}
	return fmt.Sprintf("%s:%d", address, port)
}

// TrustedHosts 组装传给 dsh 的 --trusted-host 列表。
//
// dsh 的 `/api` 浏览器信任栅栏只接受三种 authority：回环地址、dsh 自己绑定的
// 地址、以及被显式点名的 trustedHosts。反向代理把浏览器的 Host 原样递过去，
// 所以「用户从哪个地址访问网关」必须在这里点名。漏了会很难查：页面能打开
// （`GET /` 是令牌交换，不过栅栏），而**每一个 XHR 与 WebSocket 都 403**，
// 前端表现成「一直连接中」「工作区打开失败」——报错完全指不到真因。
//
// 三个来源：用户显式点名的、public-url 里的主机名、本机非回环地址与主机名。
// public-url 的主机名刻意**不带端口**：dsh 把不带端口的条目解释成「该主机名
// 的任意端口」，于是宿主端口映射（宿主机 12090 → 容器 12020）也不会漏。
//
// 单实例形态与管理端形态共用这一份——两边各自推导过一次，管理端那次漏了，
// 于是同一套代码在单实例下好好的、在管理端下每个实例都连不上。
func TrustedHosts(explicit []string, publicURL string) []string {
	seen := map[string]bool{}
	var hosts []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		hosts = append(hosts, value)
	}
	for _, host := range explicit {
		add(host)
	}
	if name := hostOfURL(publicURL); name != "" {
		add(authorityHost(name))
	}
	for _, address := range LocalAddresses() {
		add(authorityHost(address))
	}
	if name := Hostname(); name != "" {
		add(name)
	}
	return hosts
}

// hostOfURL 取出 URL 的主机名；空串或解析不出来时返回空串。
func hostOfURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// authorityHost 给 IPv6 地址加方括号：dsh 的 trusted-host 比对按 URL 的
// authority 形式，裸 ::1 与 [::1] 不是同一个字符串。
func authorityHost(address string) string {
	if strings.Contains(address, ":") {
		return "[" + address + "]"
	}
	return address
}
