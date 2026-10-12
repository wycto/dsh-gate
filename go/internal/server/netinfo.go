// Package server 把配置、账号、dsh 进程、鉴权与反向代理装配成一个可运行的
// 网关实例，并负责多实例的编排、启动横幅与优雅停机。
//
// 装配顺序是有讲究的：**先把监听端口打开，再去拉起 dsh**。反过来的话，
// Linux 服务器首次启动要下载插件的一两分钟里，浏览器只会看到「连接被拒绝」，
// 而现在用户至少能看到一个写着「DSH 正在启动」的页面。
package server

import (
	"fmt"
	"net"
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
