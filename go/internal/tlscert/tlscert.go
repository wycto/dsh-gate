// Package tlscert 为管理端生成并维护自签 HTTPS 证书。
//
// 为什么需要它：dsh 自带的 Web UI 只监听 127.0.0.1，用户是通过管理端从手机或
// 另一台电脑访问的。而浏览器的「安全上下文」只认 HTTPS 与回环地址——在
// http://192.168.1.20:12020 上，麦克风、语音输入这类能力直接不可用，报错却是
// 「当前浏览器不支持录音」，用户完全猜不到根因是协议。给管理端配一张能被信任
// 的自签证书，这条路才走得通。
//
// 为什么自己生成而不是让用户去敲 openssl：自签证书要真正有用，必须满足两个
// 条件——SAN 里钉的地址正是用户会在地址栏里敲的那个，且 CA 被装进信任库。
// 前者随本机 IP 变化，后者跨平台差异极大。两件事都交给 install，用户就不必懂
// 这些；而它们恰恰是最容易被做错、做错后症状又最难查的两处。
package tlscert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/netutil"
)

// 文件名与有效期。
//
// 服务器证书 825 天是 Apple 平台对服务器证书的硬上限：超过这个长度，macOS 与
// iOS 会直接判定证书无效，而这两个平台正是「手机浏览器访问」的主场。CA 用
// 十年——它只是本机信任链的锚点，换一次就要让用户重新装一遍信任，越久越省事。
const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"

	// managedMarker 标记这个目录里的证书是 install 自动生成的。
	//
	// 有了它，start 侧才敢在每次启动时按当前 IP 重签（见 IsManaged）：用户自己
	// 放的证书绝不会被我们覆盖，而 DHCP 换了 IP 之后也不必重新跑一遍 install。
	managedMarker = "managed"

	caYears     = 10
	serverDays  = 825
	renewBefore = 30 * 24 * time.Hour

	caCommonName = "dsh-gate Local CA"
	orgName      = "dsh-gate"
	keyBits      = 2048
)

// Options 是生成证书的输入。
type Options struct {
	// Dir 是证书目录，通常取 <状态目录>/tls（见 ManagedDir）。
	Dir string
	// Extra 是额外要写进 SAN 的地址或主机名（用户通过域名访问时用得上）。
	Extra []string
	// Now 便于测试注入时间；零值表示当前时间。
	Now time.Time
	// Addresses 覆盖自动探测的本机地址（测试用）；为空则取 netutil.LocalAddresses。
	Addresses []string
	// Hostname 覆盖自动探测的主机名（测试用）；为空则取 netutil.Hostname。
	Hostname string
}

// Result 是 Ensure 的结果。
type Result struct {
	// CACert 是 CA 证书路径。要装进系统信任库的就是它。
	CACert string
	// Cert / Key 是服务器证书与私钥，直接喂给 http.Server.ServeTLS。
	Cert string
	Key  string
	// SANs 是这张证书覆盖的地址，打印给用户看「能敲哪些地址」。
	SANs []string
	// Reissued 表示这次重新签发了服务器证书（首次生成、SAN 变了、或快到期）。
	Reissued bool
	// NewCA 表示这次新建了 CA——用户需要重新装一次信任。
	NewCA bool
}

// ManagedDir 返回管理端自动生成证书的目录。
func ManagedDir(stateDir string) string {
	return filepath.Join(stateDir, "tls")
}

// IsManaged 报告 certPath 是否落在 stateDir 下由我们托管的证书目录里。
//
// start 侧据此决定要不要在启动时重签：只有自己生成的文件才归我们维护。
func IsManaged(certPath, stateDir string) bool {
	certPath = strings.TrimSpace(certPath)
	if certPath == "" || strings.TrimSpace(stateDir) == "" {
		return false
	}
	dir := ManagedDir(stateDir)
	abs, err := filepath.Abs(certPath)
	if err != nil {
		return false
	}
	dirAbs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dirAbs, managedMarker)); err != nil {
		return false
	}
	return filepath.Dir(abs) == dirAbs
}

// Ensure 保证目录里有一张「覆盖当前地址」的可用证书，必要时生成或重签。
//
// 三种情况会重新签发服务器证书：文件不存在、SAN 没覆盖全想要的地址、或者已经
// 进入续期窗口。CA 只在不存在或本身已失效时重建——换 CA 意味着用户得重新装
// 一遍信任，能不动就不动。
func Ensure(opts Options) (Result, error) {
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return Result{}, fmt.Errorf("证书目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("创建证书目录 %q 失败: %w", dir, err)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	sans := WantedSANs(opts)

	caCertPath := filepath.Join(dir, caCertFile)
	caKeyPath := filepath.Join(dir, caKeyFile)
	certPath := filepath.Join(dir, serverCertFile)
	keyPath := filepath.Join(dir, serverKeyFile)

	// 已有证书还能用就直接返回：重复执行 install 不该每次换一张证书，
	// 那会让用户已经装好的信任和浏览器里记住的例外全部失效。
	//
	// 复用之前必须先把 CA 读出来：只有服务器证书、没有 CA 的目录是不完整的
	// （CA 私钥被删、CA 文件损坏、或者有人手动清了半个目录），此时复用旧证书
	// 会留下一张「浏览器永远信不了」的证书，症状却只是「还是提示不安全」。
	caCert, caKey, caErr := readCA(caCertPath, caKeyPath, now)
	if caErr == nil {
		if ok, err := usable(certPath, keyPath, caCert, sans, now); err == nil && ok {
			return Result{CACert: caCertPath, Cert: certPath, Key: keyPath, SANs: sans}, nil
		}
	}

	caCert, caKey, newCA, err := loadOrCreateCA(caCertPath, caKeyPath, now)
	if err != nil {
		return Result{}, err
	}
	if newCA {
		// CA 换了，旧证书必然验不过：先删掉，免得下面「写失败」时留下半套。
		_ = os.Remove(certPath)
		_ = os.Remove(keyPath)
	}

	certPEM, keyPEM, err := issueServerCert(caCert, caKey, sans, now)
	if err != nil {
		return Result{}, err
	}
	if err := writeFile(certPath, certPEM, 0o644); err != nil {
		return Result{}, err
	}
	if err := writeFile(keyPath, keyPEM, 0o600); err != nil {
		return Result{}, err
	}
	// 标记放在最后：前面任何一步失败，这个目录都不该被认作「归我们托管」。
	if err := writeFile(filepath.Join(dir, managedMarker), []byte("managed by dsh-gate install\n"), 0o644); err != nil {
		return Result{}, err
	}

	return Result{
		CACert:   caCertPath,
		Cert:     certPath,
		Key:      keyPath,
		SANs:     sans,
		Reissued: true,
		NewCA:    newCA,
	}, nil
}

// WantedSANs 返回这张证书应当覆盖的地址，顺序按「最可能被敲到的排在前面」。
//
// 回环地址排在最前不是随意：本机浏览器访问是最常见场景，而 127.0.0.1 与
// localhost 是两个不同的 SAN，漏了任何一个，用户换个写法就报警告。
func WantedSANs(opts Options) []string {
	var entries []string
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		entries = append(entries, value)
	}

	add("127.0.0.1")
	add("localhost")

	hostname := strings.TrimSpace(opts.Hostname)
	if hostname == "" {
		hostname = netutil.Hostname()
	}
	if hostname != "" {
		add(hostname)
		// macOS 的 .local（Bonjour）名字：局域网里别的设备敲 hostname.local
		// 就能找到这台机器，比记 IP 方便，也很常用。
		if !strings.Contains(hostname, ".") {
			add(hostname + ".local")
		}
	}

	addresses := opts.Addresses
	if addresses == nil {
		addresses = netutil.LocalAddresses()
	}
	for _, address := range addresses {
		add(address)
	}
	for _, extra := range opts.Extra {
		add(extra)
	}
	return entries
}

// usable 判断已有证书能否直接复用。
//
// 只在「文件读得通、私钥配得上、由这个 CA 签发、SAN 覆盖全、且没进续期窗口」
// 时才返回 true；任何一项存疑都重新签发——多签一张证书的代价，远小于让用户
// 面对一个说不清原因的证书警告。
func usable(certPath, keyPath string, caCert *x509.Certificate, sans []string, now time.Time) (bool, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return false, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return false, err
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		return false, err
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return false, err
	}
	// 私钥与证书必须是一对：换过其中一半的目录里，二者对不上。
	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return false, fmt.Errorf("证书与私钥不匹配")
	}
	// 必须由当前 CA 签发。CA 被重建过而服务器证书没跟着重签时，这里会失败
	// ——否则会留下一张浏览器永远信不了的证书。
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return false, fmt.Errorf("证书不是当前 CA 签发的: %w", err)
	}
	if now.After(cert.NotAfter.Add(-renewBefore)) {
		return false, fmt.Errorf("证书即将或已经过期")
	}
	if !sansCovered(cert, sans) {
		return false, fmt.Errorf("证书没有覆盖全部地址")
	}
	return true, nil
}

// readCA 读取 CA 证书与私钥，并确认它还能用来签发。
//
// 与 loadOrCreateCA 的「读」那一半共用同一套判据：CA 必须是 CA、私钥配得上、
// 且没进续期窗口。任何一项不满足，调用方都应当走重建路径。
func readCA(certPath, keyPath string, now time.Time) (*x509.Certificate, *rsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, nil, err
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA {
		return nil, nil, fmt.Errorf("CA 证书的 IsCA 为假")
	}
	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return nil, nil, fmt.Errorf("CA 证书与私钥不匹配")
	}
	if now.After(cert.NotAfter.Add(-renewBefore)) {
		return nil, nil, fmt.Errorf("CA 即将或已经过期")
	}
	return cert, key, nil
}

// sansCovered 报告证书是否覆盖了全部想要的地址。
func sansCovered(cert *x509.Certificate, sans []string) bool {
	have := map[string]bool{}
	for _, name := range cert.DNSNames {
		have[strings.ToLower(name)] = true
	}
	for _, ip := range cert.IPAddresses {
		have[ip.String()] = true
	}
	for _, want := range sans {
		if ip := net.ParseIP(want); ip != nil {
			if !have[ip.String()] {
				return false
			}
			continue
		}
		if !have[strings.ToLower(want)] {
			return false
		}
	}
	return true
}

// loadOrCreateCA 读取或创建 CA。newCA 为真表示这次新建了（或重建了）CA。
//
// 「读取」与「创建」共用 readCA 的判据：CA 文件缺失、损坏、私钥对不上、或者
// 已进续期窗口，都走重建。重建 CA 意味着用户要重新装一次信任，但那是唯一
// 能让浏览器重新信任的正确做法——留着一套坏掉的 CA 只会让症状更费解。
func loadOrCreateCA(certPath, keyPath string, now time.Time) (cert *x509.Certificate, key *rsa.PrivateKey, newCA bool, err error) {
	if cert, key, err = readCA(certPath, keyPath, now); err == nil {
		return cert, key, false, nil
	}

	key, err = rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, nil, false, fmt.Errorf("生成 CA 私钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, false, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{orgName},
			CommonName:   caCommonName,
		},
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.AddDate(caYears, 0, 0),
		KeyUsage:  x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		// 只签服务器证书这一层，不允许它再往下签：CA 私钥就在本机磁盘上，
		// 把它限制成「一层」能缩小万一泄露的影响面。
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, false, fmt.Errorf("生成 CA 证书失败: %w", err)
	}
	if err := writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, nil, false, err
	}
	keyPEMOut, err := marshalKey(key)
	if err != nil {
		return nil, nil, false, err
	}
	if err := writeFile(keyPath, keyPEMOut, 0o600); err != nil {
		return nil, nil, false, err
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, false, fmt.Errorf("解析刚生成的 CA 证书失败: %w", err)
	}
	return cert, key, true, nil
}

// issueServerCert 用 CA 签一张服务器证书。
func issueServerCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, sans []string, now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, nil, fmt.Errorf("生成服务器私钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}

	var dnsNames []string
	var ips []net.IP
	for _, san := range sans {
		if ip := net.ParseIP(san); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, san)
	}

	subject := pkix.Name{Organization: []string{orgName}}
	if len(dnsNames) > 0 {
		subject.CommonName = dnsNames[0]
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               subject,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 0, serverDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              dnsNames,
		IPAddresses:           ips,
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("签发服务器证书失败: %w", err)
	}
	keyPEM, err = marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

// parseCert 解析 PEM 里的第一张证书。
func parseCert(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("不是 PEM 证书")
	}
	return x509.ParseCertificate(block.Bytes)
}

// parseKey 解析 PEM 私钥。
func parseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("不是 PEM 私钥")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("私钥不是 RSA")
		}
		return key, nil
	}
	return nil, fmt.Errorf("不认识的私钥格式 %q", block.Type)
}

// marshalKey 把私钥编码成 PKCS#1 PEM。
func marshalKey(key *rsa.PrivateKey) ([]byte, error) {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), nil
}

// publicKeysEqual 比较证书里的公钥与私钥推导出的公钥是否一致。
func publicKeysEqual(a, b any) bool {
	pub, ok := a.(*rsa.PublicKey)
	if !ok {
		return false
	}
	other, ok := b.(*rsa.PublicKey)
	if !ok {
		return false
	}
	return pub.Equal(other)
}

// randomSerial 生成 128 位随机序列号。
//
// 不用固定的 1：多台机器各签各的证书时，序列号撞车会让「吊销/替换」这类
// 后续操作失去区分度。
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("生成证书序列号失败: %w", err)
	}
	return serial, nil
}

// writeFile 原子地写一个文件（先写临时文件再改名），并设置权限。
func writeFile(path string, content []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, perm); err != nil {
		return fmt.Errorf("写入 %q 失败: %w", path, err)
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("设置 %q 权限失败: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("保存 %q 失败: %w", path, err)
	}
	return nil
}

// SortSANs 返回排序后的副本，便于在提示里稳定地列出地址。
func SortSANs(sans []string) []string {
	out := append([]string(nil), sans...)
	sort.Strings(out)
	return out
}
