package main

import (
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/tlscert"
)

// newTLSTestFlags 造一个带 TLS 选项的 FlagSet，并解析给定参数。
func newTLSTestFlags(t *testing.T, args ...string) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.Bool("tls", false, "")
	fs.Bool("no-tls", false, "")
	fs.Bool("tls-trust", false, "")
	fs.Bool("no-tls-trust", false, "")
	fs.String("tls-cert", "", "")
	fs.String("tls-key", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("解析参数失败: %v", err)
	}
	return fs
}

// TestWantsTLSDefaults 验证「交互式默认开、脚本默认关」这条核心规则。
//
// 这是整个功能里最容易被改坏的一处：脚本里默认改成 HTTPS，会让已经写好的
// 部署脚本、监控探针、反向代理配置全部失效，而升级一个版本就改变对外协议
// 是很糟糕的事。
func TestWantsTLSDefaults(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		interactive bool
		want        bool
	}{
		{"交互式默认开", nil, true, true},
		{"脚本默认关", nil, false, false},
		{"显式开（脚本）", []string{"--tls"}, false, true},
		{"显式关（交互式）", []string{"--no-tls"}, true, false},
		{"给了自带证书就开", []string{"--tls-cert", "a.crt", "--tls-key", "a.key"}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := readTLSInputs(newTLSTestFlags(t, c.args...))
			if got := in.wantsTLS(c.interactive); got != c.want {
				t.Fatalf("wantsTLS(interactive=%v) = %v，期望 %v", c.interactive, got, c.want)
			}
		})
	}
}

// TestWantsTrustDefaults 验证「脚本模式不碰系统信任库」。
//
// CI 里静默修改系统信任库是最不该做的事：它影响的是整台机器，而构建脚本
// 的调用者并不知道自己顺手改了什么。
func TestWantsTrustDefaults(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		interactive bool
		want        bool
	}{
		{"脚本默认不装信任", nil, false, false},
		{"显式要求装信任", []string{"--tls", "--tls-trust"}, false, true},
		{"显式拒绝装信任", []string{"--tls", "--no-tls-trust"}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := readTLSInputs(newTLSTestFlags(t, c.args...))
			if got := in.wantsTrust(c.interactive); got != c.want {
				t.Fatalf("wantsTrust(interactive=%v) = %v，期望 %v", c.interactive, got, c.want)
			}
		})
	}
}

// TestTLSInputsValidate 验证互相矛盾的选项在动手之前就被挡掉。
func TestTLSInputsValidate(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"tls 与 no-tls 冲突", []string{"--tls", "--no-tls"}, "--tls 与 --no-tls"},
		{"trust 与 no-trust 冲突", []string{"--tls-trust", "--no-tls-trust"}, "--tls-trust 与 --no-tls-trust"},
		{"只给 cert 不给 key", []string{"--tls-cert", "a.crt"}, "必须成对"},
		{"只给 key 不给 cert", []string{"--tls-key", "a.key"}, "必须成对"},
		{"给了证书又 --no-tls", []string{"--no-tls", "--tls-cert", "a.crt", "--tls-key", "a.key"}, "不要再给 --no-tls"},
		{"自带证书又要装信任", []string{"--tls-cert", "a.crt", "--tls-key", "a.key", "--tls-trust"}, "只适用于"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := readTLSInputs(newTLSTestFlags(t, c.args...)).validate()
			if err == nil {
				t.Fatalf("应当报错，实际通过了")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("错误信息应包含 %q，实际: %v", c.wantErr, err)
			}
		})
	}
	// 正常组合不应报错。
	if err := readTLSInputs(newTLSTestFlags(t, "--tls", "--tls-trust")).validate(); err != nil {
		t.Fatalf("合法组合不该报错: %v", err)
	}
}

// TestSetupTLSGeneratesManagedCert 验证自签路径真的生成了可用证书，
// 并且返回的路径能被 start 侧认作「归我们托管」。
func TestSetupTLSGeneratesManagedCert(t *testing.T) {
	stateDir := t.TempDir()
	settings, err := setupTLS(tlsInputs{on: true}, stateDir, false)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}
	if !settings.enabled || !settings.generated {
		t.Fatalf("应当生成自签证书，实际 %+v", settings)
	}
	if settings.caCert == "" {
		t.Fatal("自签场景必须给出 CA 路径（装信任要用它）")
	}
	for _, path := range []string{settings.cert, settings.key, settings.caCert} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("文件 %q 应当存在: %v", path, err)
		}
	}
	if !tlscert.IsManaged(settings.cert, stateDir) {
		t.Fatal("生成的证书应当被认作托管，start 才能自动重签")
	}
	if len(settings.sans) == 0 {
		t.Fatal("应当返回覆盖的地址列表，供确认页与提示使用")
	}
}

// TestSetupTLSDisabledByDefaultInScriptMode 验证脚本模式不加 --tls 时完全不生成证书。
//
// 「什么都没做」这件事也要测：默认关是这次改动对既有用户的承诺。
func TestSetupTLSDisabledByDefaultInScriptMode(t *testing.T) {
	stateDir := t.TempDir()
	settings, err := setupTLS(tlsInputs{}, stateDir, false)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if settings.enabled || settings.cert != "" {
		t.Fatalf("脚本模式默认不该启用 HTTPS，实际 %+v", settings)
	}
	// 连证书目录都不该建出来。
	if _, err := os.Stat(tlscert.ManagedDir(stateDir)); !os.IsNotExist(err) {
		t.Error("没启用 HTTPS 时不该创建证书目录")
	}
}

// TestSetupTLSUsesUserCert 验证用户自带证书时原样透传，不生成、不覆盖。
func TestSetupTLSUsesUserCert(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "my.crt")
	keyPath := filepath.Join(dir, "my.key")
	if err := os.WriteFile(certPath, []byte("cert"), 0o644); err != nil {
		t.Fatalf("写证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("key"), 0o600); err != nil {
		t.Fatalf("写私钥失败: %v", err)
	}

	settings, err := setupTLS(tlsInputs{cert: certPath, key: keyPath}, dir, false)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if settings.cert != certPath || settings.key != keyPath {
		t.Fatalf("应原样透传用户给的路径，实际 %+v", settings)
	}
	if settings.generated {
		t.Error("用户自带证书时不该标记为「已生成」")
	}
	if settings.caCert != "" {
		t.Error("用户自带证书时不该给出 CA 路径——那会让 install 去装一个不该装的信任")
	}
}

// TestSetupTLSRejectsMissingUserCert 验证自带证书路径不存在时提前失败。
//
// 等管理端启动时才报「文件不可读」，用户会以为安装已经成功了。
func TestSetupTLSRejectsMissingUserCert(t *testing.T) {
	dir := t.TempDir()
	_, err := setupTLS(tlsInputs{
		cert: filepath.Join(dir, "nope.crt"),
		key:  filepath.Join(dir, "nope.key"),
	}, dir, false)
	if err == nil {
		t.Fatal("文件不存在时应当报错")
	}
	if !strings.Contains(err.Error(), "不可读") {
		t.Fatalf("错误信息应说明文件不可读，实际: %v", err)
	}
}

// TestTLSSettingsStartArgs 验证 TLS 参数会被正确拼进启动命令。
func TestTLSSettingsStartArgs(t *testing.T) {
	disabled := tlsSettings{}
	if args := disabled.startArgs(); len(args) != 0 {
		t.Fatalf("未启用时不该带任何参数，实际 %v", args)
	}

	enabled := tlsSettings{enabled: true, cert: "/tmp/c.pem", key: "/tmp/k.pem"}
	args := enabled.startArgs()
	want := []string{"--tls-cert", "/tmp/c.pem", "--tls-key", "/tmp/k.pem"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("启动参数不对：%v，期望 %v", args, want)
	}
}

// TestDisplayHostNeverEmpty 验证访问地址里不会出现空主机名。
func TestDisplayHostNeverEmpty(t *testing.T) {
	if got := displayHost(); got == "" {
		t.Fatal("访问主机名不能为空")
	}
}

// TestInteractiveWizardAsksTLSAndTrust 验证交互式向导会问 HTTPS 与信任安装，
// 且回车采纳「默认开、默认装信任」。
//
// 直接驱动 collectInstallSettings（而不是跑 install 命令）是因为它需要一个
// 真 TTY：install 用 term.IsTerminal 判断要不要走向导，测试里拿不到 TTY。
// 现有测试也是这么做的，见 TestCollectInstallSettingsExistingAccountsSkipsAccountQuestions。
func TestInteractiveWizardAsksTLSAndTrust(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	free := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("host", "0.0.0.0", "")
	fs.Int("port", 12020, "")
	fs.String("state-dir", "", "")
	fs.String("admin", "admin", "")
	fs.String("display", "", "")
	fs.Bool("start", false, "")
	fs.Bool("service", false, "")
	fs.String("bin-dir", binDirDefault(), "")

	// 只留 HTTPS 的两问与确认页；其余都算「已给过」。
	visited := map[string]bool{"host": true, "port": true, "bin-dir": true, "state-dir": true, "start": true, "service": true}
	// 两个回车＝默认（开 HTTPS、装信任），最后 y 确认。
	withStdin(t, "\n\ny\n")

	var settings installSettings
	out, code := captureStdout(t, func() int {
		var err error
		settings, err = collectInstallSettings(fs, visited, "0.0.0.0", free, dir, "admin", "",
			true, false, nil, 0, false, false, tlsInputs{}, "")
		if err != nil {
			t.Errorf("向导不应报错: %v", err)
			return 1
		}
		return 0
	})
	if code != 0 {
		t.Fatalf("向导应走完，输出：%s", out)
	}
	if !settings.tls.enabled {
		t.Fatal("交互式向导回车应当默认启用 HTTPS")
	}
	if !settings.tls.trustRequested {
		t.Fatal("交互式向导回车应当默认同意装信任")
	}
	if !strings.Contains(out, "https://") {
		t.Errorf("确认页应显示 https 地址，实际输出：%s", out)
	}
	if !strings.Contains(out, "自签证书") {
		t.Errorf("确认页应提到自签证书，实际输出：%s", out)
	}
}

// TestInteractiveWizardCanDeclineTLS 验证交互式向导能明确关掉 HTTPS。
func TestInteractiveWizardCanDeclineTLS(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起监听失败: %v", err)
	}
	free := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.String("host", "0.0.0.0", "")
	fs.Int("port", 12020, "")
	fs.String("state-dir", "", "")
	fs.String("admin", "admin", "")
	fs.String("display", "", "")
	fs.Bool("start", false, "")
	fs.Bool("service", false, "")
	fs.String("bin-dir", binDirDefault(), "")

	visited := map[string]bool{"host": true, "port": true, "bin-dir": true, "state-dir": true, "start": true, "service": true}
	withStdin(t, "n\ny\n")

	var settings installSettings
	out, code := captureStdout(t, func() int {
		var err error
		settings, err = collectInstallSettings(fs, visited, "0.0.0.0", free, dir, "admin", "",
			true, false, nil, 0, false, false, tlsInputs{}, "")
		if err != nil {
			t.Errorf("向导不应报错: %v", err)
			return 1
		}
		return 0
	})
	if code != 0 {
		t.Fatalf("向导应走完，输出：%s", out)
	}
	if settings.tls.enabled {
		t.Fatal("回答 n 时不该启用 HTTPS")
	}
	if strings.Contains(out, "https://") {
		t.Errorf("未启用 HTTPS 时确认页不该出现 https 地址，实际输出：%s", out)
	}
}
