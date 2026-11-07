package discover

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestParsePorts(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		bad  bool
	}{
		{"80,443", []int{80, 443}, false},
		{"3000-3002,80", []int{80, 3000, 3001, 3002}, false},
		{" 8080 , 8080 ", []int{8080}, false},
		{"", nil, true},
		{"0", nil, true},
		{"70000", nil, true},
		{"abc", nil, true},
		{"10-5", nil, true},
		{"1000-3000", nil, true}, // 2001 个 > 1024 上限
		{"1-65535", nil, true},   // 区间过大，快速拒绝
	}
	for _, c := range cases {
		got, err := ParsePorts(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParsePorts(%q) 应报错，实际 %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePorts(%q) 意外错误: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParsePorts(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestIsPrivateCIDR(t *testing.T) {
	cases := []struct {
		cidr string
		want bool
		bad  bool
	}{
		{"10.1.2.0/24", true, false},
		{"172.16.0.0/12", true, false},
		{"172.20.5.0/24", true, false},
		{"192.168.1.0/24", true, false},
		{"127.0.0.1/32", true, false},
		{"8.8.8.0/24", false, false},
		{"172.32.0.0/16", false, false},
		{"11.0.0.0/8", false, false},
		{"nonsense", false, true},
		{"2001:db8::/32", false, true},
	}
	for _, c := range cases {
		got, err := IsPrivateCIDR(c.cidr)
		if c.bad {
			if err == nil {
				t.Errorf("IsPrivateCIDR(%q) 应报错", c.cidr)
			}
			continue
		}
		if err != nil {
			t.Errorf("IsPrivateCIDR(%q) 意外错误: %v", c.cidr, err)
			continue
		}
		if got != c.want {
			t.Errorf("IsPrivateCIDR(%q)=%v want %v", c.cidr, got, c.want)
		}
	}
}

func TestExtractTitle(t *testing.T) {
	body := []byte("<html><head><title> Hello   World </title></head></html>")
	if got := extractTitle(body); got != "Hello World" {
		t.Errorf("extractTitle=%q want Hello World", got)
	}
	if got := extractTitle([]byte("<html>no title</html>")); got != "" {
		t.Errorf("无标题应空，实际 %q", got)
	}
}

func TestScanLoopbackOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Scan(ctx, "127.0.0.1/32", []int{port}, ScanOptions{Timeout: 300 * time.Millisecond, Concurrency: 4})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	found := false
	for _, c := range got {
		if c.Port == port && c.Host == "127.0.0.1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应发现本机回环开放端口 %d，实际 %+v", port, got)
	}
}

func TestScanRejectsPublic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Scan(ctx, "8.8.8.0/24", []int{80}, ScanOptions{}); err == nil {
		t.Error("公网段应被拒绝")
	}
}

func TestLocalBestEffort(t *testing.T) {
	got, err := Local()
	if errors.Is(err, ErrUnsupported) {
		t.Skip("本机发现该平台不支持")
	}
	if err != nil {
		t.Fatalf("Local 失败: %v", err)
	}
	for _, c := range got {
		if c.Port <= 0 {
			t.Errorf("候选端口应为正: %+v", c)
		}
		if c.Source != "local" {
			t.Errorf("Source 应为 local: %+v", c)
		}
	}
}
