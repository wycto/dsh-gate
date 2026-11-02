package tlscert

import (
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEnsureCreatesUsableChain 验证首次生成出来的证书链真的能用：
// 服务器证书由 CA 签、SAN 覆盖回环与给定地址、私钥配得上。
func TestEnsureCreatesUsableChain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	res, err := Ensure(Options{
		Dir:       dir,
		Hostname:  "test-host",
		Addresses: []string{"192.168.1.20", "10.0.0.5"},
	})
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	if !res.Reissued {
		t.Fatal("首次调用应当签发了服务器证书")
	}
	if !res.NewCA {
		t.Fatal("首次调用应当新建了 CA")
	}

	caPEM, err := os.ReadFile(res.CACert)
	if err != nil {
		t.Fatalf("读 CA 失败: %v", err)
	}
	caCert, err := parseCert(caPEM)
	if err != nil {
		t.Fatalf("解析 CA 失败: %v", err)
	}
	if !caCert.IsCA {
		t.Fatal("CA 证书的 IsCA 应为真")
	}

	certPEM, err := os.ReadFile(res.Cert)
	if err != nil {
		t.Fatalf("读服务器证书失败: %v", err)
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		t.Fatalf("解析服务器证书失败: %v", err)
	}
	// 用 CA 验一遍：这是「浏览器会不会信」在代码里最接近的等价检查。
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   "192.168.1.20",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("服务器证书无法用 CA 验证: %v", err)
	}

	// SAN 必须同时覆盖回环、主机名与局域网地址。
	for _, want := range []string{"127.0.0.1", "localhost", "test-host", "192.168.1.20", "10.0.0.5"} {
		if !sansCovered(cert, []string{want}) {
			t.Errorf("SAN 缺少 %q，实际 DNS=%v IP=%v", want, cert.DNSNames, cert.IPAddresses)
		}
	}
	// 私钥权限必须是 0600：它就在用户家目录里。
	info, err := os.Stat(res.Key)
	if err != nil {
		t.Fatalf("读私钥信息失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("私钥权限应为 0600，实际 %o", perm)
	}
}

// TestEnsureReusesExisting 验证重复调用不会换证书：
// install 重跑一次就换一张证书，会让用户已经装好的信任和浏览器例外全部失效。
func TestEnsureReusesExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	opts := Options{Dir: dir, Hostname: "test-host", Addresses: []string{"192.168.1.20"}}

	first, err := Ensure(opts)
	if err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	before, err := os.ReadFile(first.Cert)
	if err != nil {
		t.Fatalf("读证书失败: %v", err)
	}

	second, err := Ensure(opts)
	if err != nil {
		t.Fatalf("二次调用失败: %v", err)
	}
	if second.Reissued || second.NewCA {
		t.Fatal("SAN 与有效期都没变时不该重新签发")
	}
	after, err := os.ReadFile(second.Cert)
	if err != nil {
		t.Fatalf("读证书失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("证书内容变了：重复 install 不应换证书")
	}
}

// TestEnsureReissuesOnAddressChange 验证 IP 变化后会重签。
//
// 这正是 DHCP 换地址、连上 VPN、换网卡之后的真实场景：不重签的话，用户按新
// IP 访问会看到证书无效，录音又不可用，而原因极难查。
func TestEnsureReissuesOnAddressChange(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if _, err := Ensure(Options{Dir: dir, Hostname: "test-host", Addresses: []string{"192.168.1.20"}}); err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}

	// 地址变了：新地址不在旧证书的 SAN 里。
	res, err := Ensure(Options{Dir: dir, Hostname: "test-host", Addresses: []string{"192.168.1.99"}})
	if err != nil {
		t.Fatalf("重签失败: %v", err)
	}
	if !res.Reissued {
		t.Fatal("地址变化后应当重签")
	}
	if res.NewCA {
		t.Fatal("地址变化不该重建 CA——那样用户得重新装一次信任")
	}

	certPEM, err := os.ReadFile(res.Cert)
	if err != nil {
		t.Fatalf("读证书失败: %v", err)
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if !sansCovered(cert, []string{"192.168.1.99"}) {
		t.Fatal("重签后的证书应当覆盖新地址")
	}
}

// TestEnsureRenewsNearExpiry 验证进入续期窗口后会重签。
func TestEnsureRenewsNearExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	base := time.Now()
	if _, err := Ensure(Options{Dir: dir, Hostname: "h", Addresses: []string{"10.0.0.5"}, Now: base}); err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	// 跳到服务器证书快过期的那天。
	future := base.AddDate(0, 0, serverDays-10)
	res, err := Ensure(Options{Dir: dir, Hostname: "h", Addresses: []string{"10.0.0.5"}, Now: future})
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if !res.Reissued {
		t.Fatal("临近过期应当重签服务器证书")
	}
	if res.NewCA {
		t.Fatal("续期不该重建 CA")
	}
}

// TestEnsureRebuildsBrokenCA 验证 CA 文件被删或损坏时会重建，而不是直接报错。
func TestEnsureRebuildsBrokenCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if _, err := Ensure(Options{Dir: dir, Hostname: "h", Addresses: []string{"10.0.0.5"}}); err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatalf("删除 CA 私钥失败: %v", err)
	}

	res, err := Ensure(Options{Dir: dir, Hostname: "h", Addresses: []string{"10.0.0.5"}})
	if err != nil {
		t.Fatalf("CA 缺失时应重建而不是失败: %v", err)
	}
	if !res.NewCA {
		t.Fatal("CA 私钥丢了就必须重建 CA")
	}
}

// TestWantedSANsOrderAndDedup 验证 SAN 列表：回环在最前、重复项被去掉。
func TestWantedSANsOrderAndDedup(t *testing.T) {
	sans := WantedSANs(Options{
		Hostname:  "test-host",
		Addresses: []string{"192.168.1.20", "127.0.0.1"},
		Extra:     []string{"localhost", "dsh.example.com"},
	})
	if len(sans) < 4 {
		t.Fatalf("SAN 太少: %v", sans)
	}
	// 回环固定在最前两位：本机浏览器访问是最常见场景。
	if sans[0] != "127.0.0.1" || sans[1] != "localhost" {
		t.Fatalf("回环地址应排在最前，实际 %v", sans)
	}
	count := map[string]int{}
	for _, san := range sans {
		count[san]++
	}
	for san, n := range count {
		if n > 1 {
			t.Errorf("%q 在 SAN 里出现了 %d 次", san, n)
		}
	}
}

// TestIsManaged 验证「归我们托管」的判定：只有自己生成的目录才算。
func TestIsManaged(t *testing.T) {
	stateDir := t.TempDir()
	dir := ManagedDir(stateDir)
	if _, err := Ensure(Options{Dir: dir, Hostname: "h", Addresses: []string{"10.0.0.5"}}); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !IsManaged(filepath.Join(dir, serverCertFile), stateDir) {
		t.Fatal("自己生成的证书应当被认作托管")
	}
	// 用户自己放的证书：目录里没有标记文件，不能被当成我们的。
	elsewhere := filepath.Join(t.TempDir(), "server.crt")
	if err := os.WriteFile(elsewhere, []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if IsManaged(elsewhere, stateDir) {
		t.Fatal("用户自己放的证书不该被认作托管")
	}
	if IsManaged("", stateDir) {
		t.Fatal("空路径不该被认作托管")
	}
}

// TestSANsCovered 验证地址覆盖判断对 DNS 与 IP 两类条目都正确。
func TestSANsCovered(t *testing.T) {
	cert := &x509.Certificate{
		DNSNames:    []string{"localhost", "Example.COM"},
		IPAddresses: []net.IP{net.ParseIP("10.0.0.5")},
	}

	if !sansCovered(cert, []string{"LOCALHOST"}) {
		t.Error("DNS 名应当大小写不敏感")
	}
	if !sansCovered(cert, []string{"10.0.0.5"}) {
		t.Error("IP 地址应被识别为已覆盖")
	}
	if sansCovered(cert, []string{"10.0.0.6"}) {
		t.Error("未覆盖的 IP 不该被判定为覆盖")
	}
	if sansCovered(cert, []string{"missing.example"}) {
		t.Error("未覆盖的域名不该被判定为覆盖")
	}
}

// TestEnsureRejectsEmptyDir 验证没有目录时不静默生成到奇怪的地方。
func TestEnsureRejectsEmptyDir(t *testing.T) {
	if _, err := Ensure(Options{}); err == nil {
		t.Fatal("空目录应当报错")
	} else if !strings.Contains(err.Error(), "证书目录") {
		t.Fatalf("错误信息应说明是目录问题，实际: %v", err)
	}
}
