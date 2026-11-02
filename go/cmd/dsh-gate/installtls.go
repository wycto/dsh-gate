// install 的 HTTPS 自签证书步骤。
//
// 单独放一个文件，因为它有三处独立的判断要交代清楚：
//
//  1. **要不要开**。交互式向导默认开、脚本模式默认关。脚本里悄悄改成 HTTPS
//     会让已经写好的部署脚本、监控探针、反代配置全部失效，而升级一个版本
//     就改变对外协议是很糟糕的事。
//  2. **用哪张证书**。用户可以用 --tls-cert/--tls-key 指定自己的证书，那种
//     情况下我们只负责把它透传下去，绝不生成也不覆盖。
//  3. **要不要装信任**。装进系统信任库才让浏览器真正信任自签证书；但这一步
//     要改系统状态，所以做成一次明确的询问，失败也不拦安装。
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/wycto/dsh-gate/internal/i18n"
	"github.com/wycto/dsh-gate/internal/netutil"
	"github.com/wycto/dsh-gate/internal/tlscert"
	"github.com/wycto/dsh-gate/internal/trust"
)

// tlsSettings 是 install 收集到的 HTTPS 设置。
type tlsSettings struct {
	// enabled 表示最终要不要以 HTTPS 对外服务。
	enabled bool
	// cert / key 是要传给管理端的证书路径；为空表示不启用。
	cert string
	key  string
	// caCert 是自签场景下需要装进信任库的 CA；用用户自带证书时为空。
	caCert string
	// generated 表示证书是这次生成/维护的（而非用户指定的）。
	generated bool
	// trustRequested 表示用户同意把 CA 装进系统信任库。
	trustRequested bool
	// reissued / newCA 用于提示「这次重签了」「CA 变了要重新装信任」。
	reissued bool
	newCA    bool
	// sans 是证书覆盖的地址，打印给用户看。
	sans []string
}

// tlsInputs 是从命令行收集到的原始 TLS 输入。
type tlsInputs struct {
	on, off         bool
	trust, trustOff bool
	cert, key       string
	// visited 记录用户显式给过哪些 TLS 开关。
	visited map[string]bool
}

// readTLSInputs 从 FlagSet 里取出 TLS 相关输入。
//
// 用 fs.Visit 而不是直接读变量值：Bool 的零值 false 与「用户没说」是两件事。
// 区分不了的话，交互式向导就没法给出「默认开」——它会以为用户已经选了「关」。
func readTLSInputs(fs *flag.FlagSet) tlsInputs {
	in := tlsInputs{visited: map[string]bool{}}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "tls":
			in.on = f.Value.String() == "true"
			in.visited["tls"] = true
		case "no-tls":
			in.off = f.Value.String() == "true"
			in.visited["no-tls"] = true
		case "tls-trust":
			in.trust = f.Value.String() == "true"
			in.visited["tls-trust"] = true
		case "no-tls-trust":
			in.trustOff = f.Value.String() == "true"
			in.visited["no-tls-trust"] = true
		case "tls-cert":
			in.cert = f.Value.String()
			in.visited["tls-cert"] = true
		case "tls-key":
			in.key = f.Value.String()
			in.visited["tls-key"] = true
		}
	})
	return in
}

// validate 检查 TLS 选项是否自洽，返回给人看的错误。
//
// 这些冲突在动手之前就要挡掉：装完账号、启动到一半才发现参数互相打架，
// 用户得自己回滚一次。
func (in tlsInputs) validate() error {
	if in.on && in.off {
		return fmt.Errorf("--tls 与 --no-tls 只能用一个")
	}
	if in.trust && in.trustOff {
		return fmt.Errorf("--tls-trust 与 --no-tls-trust 只能用一个")
	}
	// 指定了自带证书就意味着要开 HTTPS：两者矛盾时直接报错，而不是猜。
	certGiven := strings.TrimSpace(in.cert) != ""
	keyGiven := strings.TrimSpace(in.key) != ""
	if certGiven != keyGiven {
		return fmt.Errorf("--tls-cert 与 --tls-key 必须成对提供")
	}
	if (certGiven || keyGiven) && in.off {
		return fmt.Errorf("给了 --tls-cert/--tls-key 就不要再给 --no-tls")
	}
	// 自带证书时不该再要求装信任：CA 不是我们生成的，我们不知道它该装哪个。
	if (certGiven || keyGiven) && (in.trust || in.visited["tls-trust"]) {
		return fmt.Errorf("--tls-trust 只适用于 install 自己生成的自签证书；" +
			"用 --tls-cert 指定证书时请自行把它对应的 CA 装进信任库")
	}
	return nil
}

// wantsTLS 决定这次要不要启用 HTTPS。
//
// 交互式默认开、脚本默认关（见文件头）。用户显式给过任一开关时以显式为准。
func (in tlsInputs) wantsTLS(interactive bool) bool {
	switch {
	case in.off:
		return false
	case in.on:
		return true
	case strings.TrimSpace(in.cert) != "" || strings.TrimSpace(in.key) != "":
		return true
	}
	return interactive
}

// wantsTrust 决定要不要尝试装信任。
//
// 交互式里由用户回答，脚本模式下默认**不**碰系统状态——CI 里静默修改信任库
// 是最不该做的事。
func (in tlsInputs) wantsTrust(interactive bool) bool {
	switch {
	case in.trustOff:
		return false
	case in.trust:
		return true
	}
	return false
}

// setupTLS 按用户的选择准备证书，返回要传给管理端的参数与给人看的输出。
//
// 它只负责「把证书弄到位」，不负责打印摘要——摘要要和其它安装项一起出现在
// 确认页上，所以这里返回 tlsSettings 由调用方汇总。
func setupTLS(in tlsInputs, stateDir string, interactive bool) (tlsSettings, error) {
	var settings tlsSettings
	if !in.wantsTLS(interactive) {
		return settings, nil
	}

	// 自带证书：原样透传，不生成、不覆盖、不碰信任库。
	if cert := strings.TrimSpace(in.cert); cert != "" {
		settings.enabled = true
		settings.cert = cert
		settings.key = strings.TrimSpace(in.key)
		// 早一点验证文件真的在：等管理端启动时才报「文件不可读」，
		// 用户会以为安装成功了。
		for label, path := range map[string]string{"--tls-cert": settings.cert, "--tls-key": settings.key} {
			if _, err := os.Stat(path); err != nil {
				return settings, fmt.Errorf("%s 指定的文件不可读 %q: %w", label, path, err)
			}
		}
		return settings, nil
	}

	// 自签：生成或沿用 <状态目录>/tls 下由我们维护的那套证书。
	result, err := tlscert.Ensure(tlscert.Options{Dir: tlscert.ManagedDir(stateDir)})
	if err != nil {
		return settings, fmt.Errorf("生成自签证书失败: %w", err)
	}
	settings.enabled = true
	settings.cert = result.Cert
	settings.key = result.Key
	settings.caCert = result.CACert
	settings.generated = true
	settings.reissued = result.Reissued
	settings.newCA = result.NewCA
	settings.sans = result.SANs
	settings.trustRequested = in.wantsTrust(interactive)
	return settings, nil
}

// printTLSResult 打印证书生成结果与信任安装情况。
//
// 分两段：先生成结果（覆盖了哪些地址，用户据此知道该敲哪个 URL），
// 再信任安装（成功还是需要手动）。
func printTLSResult(settings tlsSettings, certDir string, trustStatus trust.Status, port int) {
	if !settings.enabled {
		return
	}
	fmt.Println()
	if settings.generated {
		if settings.reissued {
			sayKey(os.Stdout, i18n.KeyCLIInstallTLSGenerated, certDir, strings.Join(settings.sans, ", "))
		} else {
			sayKey(os.Stdout, i18n.KeyCLIInstallTLSReused, settings.cert)
		}
		if settings.newCA {
			// CA 变了：之前装过信任的浏览器全部失效，必须明说。
			sayKey(os.Stdout, i18n.KeyCLIInstallTLSNewCA, settings.caCert)
		}
	}

	if settings.caCert != "" {
		switch {
		case trustStatus.Trusted:
			sayKey(os.Stdout, i18n.KeyCLIInstallTLSInstalled, trust.BrowserHint())
		case settings.trustRequested:
			sayKey(os.Stdout, i18n.KeyCLIInstallTLSFailed, trustStatus.Message)
			sayKey(os.Stdout, i18n.KeyCLIInstallTLSManual, trust.ManualCommand(runtime.GOOS, settings.caCert))
		default:
			sayKey(os.Stdout, i18n.KeyCLIInstallTLSSkipped, trust.ManualCommand(runtime.GOOS, settings.caCert))
		}
	}

	printReachableURLs(settings, port)
}

// printReachableURLs 列出这次能用的访问地址。
//
// 不猜「哪个是主地址」：一台机器同时连着局域网、VPN、Tailscale 的情况很常见，
// 按字典序或接口顺序挑出来的那个未必是别人能连上的。证书的 SAN 覆盖了哪些
// 地址，就列哪些——用户从里面挑一个自己能用的，比被指到一个连不通的地址好。
func printReachableURLs(settings tlsSettings, port int) {
	scheme := "http"
	if settings.enabled {
		scheme = "https"
	}
	sayKey(os.Stdout, i18n.KeyCLIInstallTLSURLs)
	fmt.Printf(tr(i18n.KeyCLIInstallTLSURLRow)+"\n", scheme, "127.0.0.1", port)
	for _, address := range netutil.DisplayAddresses(0) {
		// IPv6 在 URL 里必须加方括号，否则冒号会被当成端口分隔符。
		host := address
		if strings.Contains(address, ":") {
			host = "[" + address + "]"
		}
		fmt.Printf(tr(i18n.KeyCLIInstallTLSURLRow)+"\n", scheme, host, port)
	}
}

// displayHost 返回给用户看的访问主机：优先局域网地址，没有就退回回环。
//
// 用 DisplayAddresses 而不是 PrimaryAddress：后者按字典序取第一个，于是
// 「VPN 地址排在局域网地址前面」这种情况会让提示里出现一个别人根本连不上的
// 地址。DisplayAddresses 的偏好排序是「私网 IPv4 → 公网 IPv4 → IPv6」，
// 更贴近「用户实际会敲哪个地址」。
func displayHost() string {
	if addresses := netutil.DisplayAddresses(1); len(addresses) > 0 {
		return addresses[0]
	}
	return "127.0.0.1"
}

// askTLS 在交互式向导里询问并准备 HTTPS 证书。
//
// 两问：要不要开、要不要装信任。证书在第一问之后立刻生成——生成结果里的
// 「覆盖地址」要显示在确认页上，用户才能判断自己打算敲的那个地址在不在里面。
//
// 显式给过 --tls / --no-tls / --tls-cert 时不再问第一问；同理 --tls-trust /
// --no-tls-trust 给了就不再问第二问。
func askTLS(in tlsInputs, stateDir string) (tlsSettings, error) {
	// 用户自带证书：不必问要不要开，也不必问装信任（CA 不归我们管）。
	if cert := strings.TrimSpace(in.cert); cert != "" {
		return setupTLS(in, stateDir, true)
	}

	enable := in.on
	if !in.on && !in.off {
		answer, err := askYesNo(tr(i18n.KeyCLIInstallTLSAsk), true)
		if err != nil {
			return tlsSettings{}, err
		}
		enable = answer
	}
	if !enable {
		return tlsSettings{}, nil
	}

	// 先把证书备好：第二问要基于「确实会生成证书」这个事实来问。
	settings, err := setupTLS(tlsInputs{on: true, cert: in.cert, key: in.key, visited: in.visited}, stateDir, true)
	if err != nil {
		return settings, err
	}
	if settings.caCert == "" {
		return settings, nil
	}

	// 第二问：装不装信任。
	//
	// 默认「是」：不装的话浏览器每次都会警告，而录音这类能力可能仍不可用——
	// 那样启用 HTTPS 的意义就丢了大半。但这一步会改系统状态，所以必须问。
	if in.visited["tls-trust"] || in.visited["no-tls-trust"] {
		settings.trustRequested = in.wantsTrust(true)
	} else {
		answer, err := askYesNo(tr(i18n.KeyCLIInstallTLSAskTrust), true)
		if err != nil {
			return settings, err
		}
		settings.trustRequested = answer
	}
	return settings, nil
}

// startArgs 返回要传给 start / service install 的 TLS 参数。
//
// 不启用 HTTPS 时返回 nil：start 侧把「没给 --tls-cert」当作 HTTP，
// 传一个空值进去反而会让它以为用户想要 TLS。
func (s tlsSettings) startArgs() []string {
	if !s.enabled || s.cert == "" {
		return nil
	}
	return []string{"--tls-cert", s.cert, "--tls-key", s.key}
}

// scheme 返回这次安装对外用的协议。
func (s installSettings) scheme() string {
	if s.tls.enabled {
		return "https"
	}
	return "http"
}

// flagHint 返回启动时应当补上的 TLS 参数，供「尚未启动」提示使用。
//
// 带上它，用户照着提示敲的那条命令就会起成与本次安装相同的形态；
// 不带的话，他起出来的是 HTTP，还得再回来问一遍「HTTPS 呢」。
func (s tlsSettings) flagHint() string {
	if !s.enabled || s.cert == "" {
		return ""
	}
	return fmt.Sprintf(" --tls-cert %s --tls-key %s", s.cert, s.key)
}
