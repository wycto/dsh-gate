package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wycto/dsh-gate/internal/registry"
)

// TestLocalEndpointFollowsTLS 断言本机接口的协议跟着登记记录走。
//
// 回归的是一类「假故障」：管理端启用 TLS 后回环端口上只有 HTTPS，而客户端
// 写死 http://，于是 dsh list / start / stop / rm 全部报「管理端未运行」——
// 看起来像没启动，其实是请求被 HTTPS 服务端回了一句
// 「Client sent an HTTP request to an HTTPS server」。TLS 是常用配置，不是边角。
func TestLocalEndpointFollowsTLS(t *testing.T) {
	isolateRunDir(t)
	stateDir := t.TempDir()

	// 一个真的在监听的回环端口：Record.Alive() 靠 TCP 拨号判断，不拨通就
	// 会被当成「管理端没在运行」，测不到 scheme。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	// 令牌文件：localEndpoint 缺它就直接报错，走不到 scheme 那一步。
	if err := os.WriteFile(filepath.Join(stateDir, localTokenName), []byte("tok\n"), 0o600); err != nil {
		t.Fatalf("写令牌失败: %v", err)
	}

	dir := registry.DefaultDir()
	for _, tls := range []bool{false, true} {
		want := "http"
		if tls {
			want = "https"
		}
		record := registry.Record{Name: portalRecordName, PID: os.Getpid(), Port: port, TLS: tls}
		if err := dir.Save(record); err != nil {
			t.Fatalf("写登记失败: %v", err)
		}
		if _, _, scheme, err := localEndpoint(stateDir); err != nil {
			t.Fatalf("tls=%v 时 localEndpoint 报错: %v", tls, err)
		} else if scheme != want {
			t.Errorf("tls=%v 时协议应为 %q，实际 %q", tls, want, scheme)
		}
	}
}

// TestLocalPostSpeaksHTTPS 用一个自签的 HTTPS 服务端验证整条链路。
//
// 只测 localEndpoint 的返回值不够：真正的坑在 http.Client 会拒绝自签证书。
// 这里让服务端用 httptest 的自签证书，断言客户端能拿到 JSON 响应——跳过校验
// 是本机回环上的既定取舍（与浏览器里点「继续访问」同义）。
func TestLocalPostSpeaksHTTPS(t *testing.T) {
	isolateRunDir(t)
	stateDir := t.TempDir()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != localAPIPath {
			t.Errorf("请求路径不对: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("令牌不对: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(localResponse{OK: true})
	}))
	defer server.Close()

	host, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("解析服务端地址失败: %v", err)
	}
	_ = host
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("解析端口失败: %v", err)
	}

	if err := os.WriteFile(filepath.Join(stateDir, localTokenName), []byte("tok"), 0o600); err != nil {
		t.Fatalf("写令牌失败: %v", err)
	}
	if err := registry.DefaultDir().Save(registry.Record{Name: portalRecordName, PID: os.Getpid(), Port: port, TLS: true}); err != nil {
		t.Fatalf("写登记失败: %v", err)
	}

	out, err := localPost(stateDir, localRequest{Action: "list"})
	if err != nil {
		t.Fatalf("走 HTTPS 的本机接口失败: %v", err)
	}
	if !out.OK {
		t.Errorf("响应应为 ok，实际 %+v", out)
	}
}
