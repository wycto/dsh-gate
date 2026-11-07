package discover

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ScanOptions 是网段扫描参数；零值走默认（并发 64、单连接超时 500ms、端口上限 1024）。
type ScanOptions struct {
	Concurrency int
	Timeout     time.Duration
	MaxPorts    int
}

func (o ScanOptions) withDefaults() ScanOptions {
	if o.Concurrency <= 0 {
		o.Concurrency = defaultConcurrency
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.MaxPorts <= 0 {
		o.MaxPorts = defaultMaxPorts
	}
	return o
}

// privateBaseNets 是允许扫描的网段（私有段 + 本机回环）。
var privateBaseNets = mustCIDRs(
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
)

func mustCIDRs(specs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(specs))
	for _, s := range specs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("discover: 内置网段非法: " + s)
		}
		out = append(out, n)
	}
	return out
}

// IsPrivateCIDR 报告 cidr 是否为可扫描网段（10/8、172.16/12、192.168/16 及其子网，
// 以及本机回环 127/8）。公网返回 false；格式错误返回 error。仅支持 IPv4。
func IsPrivateCIDR(cidr string) (bool, error) {
	cidr = strings.TrimSpace(cidr)
	if cidr == "" {
		return false, errors.New("网段不能为空")
	}
	ip, netw, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, fmt.Errorf("网段 %q 非法: %w", cidr, err)
	}
	if ip.To4() == nil {
		return false, errors.New("只支持 IPv4 网段")
	}
	ones, bits := netw.Mask.Size()
	if bits != 32 {
		return false, errors.New("只支持 IPv4 网段")
	}
	for _, base := range privateBaseNets {
		bOnes, _ := base.Mask.Size()
		if ones >= bOnes && base.Contains(netw.IP) {
			return true, nil
		}
	}
	return false, nil
}

// ParsePorts 解析端口列表，支持 "80,443,3000-3100"；去重排序。非法或超过上限报错。
func ParsePorts(spec string) ([]int, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("端口列表不能为空")
	}
	set := map[int]bool{}
	parts := strings.Split(spec, ",")
	for _, raw := range parts {
		tok := strings.TrimSpace(raw)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "-") {
			segs := strings.SplitN(tok, "-", 2)
			lo, err1 := strconv.Atoi(strings.TrimSpace(segs[0]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(segs[1]))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("端口区间 %q 非法", tok)
			}
			if lo < 1 || hi > 65535 || lo > hi {
				return nil, fmt.Errorf("端口区间 %q 超出范围（1-65535）或起始大于结束", tok)
			}
			if hi-lo+1 > defaultMaxPorts {
				return nil, fmt.Errorf("端口区间过大（%d），上限 %d", hi-lo+1, defaultMaxPorts)
			}
			for p := lo; p <= hi; p++ {
				set[p] = true
			}
			continue
		}
		p, err := strconv.Atoi(tok)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("端口 %q 非法（1-65535）", tok)
		}
		set[p] = true
	}
	if len(set) == 0 {
		return nil, errors.New("端口列表为空")
	}
	if len(set) > defaultMaxPorts {
		return nil, fmt.Errorf("端口过多（%d），上限 %d", len(set), defaultMaxPorts)
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}

// enumerateHosts 列出网段内的主机地址（常规网段去掉网络号与广播地址；/31、/32 全留）。
// 主机数超过 max 时报错，避免把网关当扫描器滥用。
func enumerateHosts(netw *net.IPNet, max int) ([]net.IP, error) {
	base := netw.IP.To4()
	if base == nil {
		return nil, errors.New("只支持 IPv4 网段")
	}
	ones, bits := netw.Mask.Size()
	if bits != 32 {
		return nil, errors.New("只支持 IPv4 网段")
	}
	count := uint64(1) << uint(32-ones)
	if count > uint64(max) {
		return nil, fmt.Errorf("网段主机数 %d 超过上限 %d，请用更小的子网（如 /24 或 /25）", count, max)
	}
	b := binary.BigEndian.Uint32(base)
	out := make([]net.IP, 0, count)
	if count <= 2 {
		for i := uint64(0); i < count; i++ {
			out = append(out, u32ToIP(b+uint32(i)))
		}
		return out, nil
	}
	for x := b + 1; x <= b+uint32(count)-2; x++ {
		out = append(out, u32ToIP(x))
	}
	return out, nil
}

func u32ToIP(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)
	return ip
}

// Scan 对私有网段 + 端口列表做 TCP 连接探测，返回开放端口的候选（尽力判定是否 HTTP）。
// 全程尊重 ctx 取消；并发受 Concurrency 限制；单连接受 Timeout 限制。
func Scan(ctx context.Context, cidr string, ports []int, opts ScanOptions) ([]Candidate, error) {
	opts = opts.withDefaults()
	ok, err := IsPrivateCIDR(cidr)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("只允许扫描私有网段（10/8、172.16/12、192.168/16 或本机回环），拒绝 %q", cidr)
	}
	if len(ports) == 0 {
		return nil, errors.New("端口列表不能为空")
	}
	if len(ports) > opts.MaxPorts {
		return nil, fmt.Errorf("端口过多（%d），上限 %d", len(ports), opts.MaxPorts)
	}
	_, netw, err := net.ParseCIDR(strings.TrimSpace(cidr))
	if err != nil {
		return nil, fmt.Errorf("网段 %q 非法: %w", cidr, err)
	}
	hosts, err := enumerateHosts(netw, maxHostsPerScan)
	if err != nil {
		return nil, err
	}

	type job struct {
		host net.IP
		port int
	}
	jobs := make(chan job)
	var (
		mu  sync.Mutex
		out []Candidate
		wg  sync.WaitGroup
	)
	workers := opts.Concurrency
	if workers > 256 {
		workers = 256
	}
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			dialer := net.Dialer{Timeout: opts.Timeout}
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				addr := net.JoinHostPort(j.host.String(), strconv.Itoa(j.port))
				conn, derr := dialer.DialContext(ctx, "tcp", addr)
				if derr != nil {
					continue
				}
				_ = conn.Close()
				isHTTP, title := probeHTTP(j.host.String(), j.port)
				mu.Lock()
				out = append(out, Candidate{
					Source: "lan", Host: j.host.String(), Port: j.port,
					HTTP: isHTTP, Title: title,
				})
				mu.Unlock()
			}
		}()
	}

	var sendErr error
produce:
	for _, h := range hosts {
		for _, p := range ports {
			if ctx.Err() != nil {
				sendErr = ctx.Err()
				break produce
			}
			select {
			case jobs <- job{host: h, port: p}:
			case <-ctx.Done():
				sendErr = ctx.Err()
				break produce
			}
		}
	}
	close(jobs)
	wg.Wait()
	if sendErr != nil && len(out) == 0 {
		return nil, sendErr
	}
	sort.Slice(out, func(i, k int) bool {
		if out[i].Host != out[k].Host {
			return out[i].Host < out[k].Host
		}
		return out[i].Port < out[k].Port
	})
	return out, nil
}
