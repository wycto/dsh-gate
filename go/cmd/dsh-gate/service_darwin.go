//go:build darwin

package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/version"
)

// macOS 上用 launchd 的 LaunchAgent：每个用户一个 plist，登录即启动。
//
// 为什么不用 LaunchDaemon（那个才是真正的「开机」）：它需要 root、装在
// /Library/LaunchDaemons，而这个网关要读写用户家目录里的 dsh 实例，以 root 跑
// 会凭空造出一份 root 拥有的 ~/.dsh来。LaunchAgent 在用户登录时启动，正是
// 「我坐下就开始用」的语义，也不需要任何特权。

// launchdLabel 是 launchd 服务与 LaunchAgent 列表里显示的名字。
// 带点号的反向域名风格是 launchd 的惯例，能保证全局唯一。
const launchdLabel = "com.wycto.dsh-gate"

// agentPath 返回当前用户的 LaunchAgent plist 路径。
func agentPath() (string, error) {
	home := homeDir()
	if home == "" {
		return "", fmt.Errorf("无法确定家目录")
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// serviceFlavor 描述当前平台用的是什么机制。
func serviceFlavor() string { return "launchd 用户级服务（登录即启动）" }

// platformServiceHelp 返回 macOS 特有的说明。
func platformServiceHelp() string {
	return `
macOS 说明:
  装到 ~/Library/LaunchAgents/，不需要 sudo：登录时自动启动。
  日志：log stream --predicate 'process == "dsh-gate"'
  想开机（未登录）就启动：把同一份配置放到 /Library/LaunchDaemons/ 并用 root 安装，
  但那样网关会以 root 身份运行，dsh 的实例目录也会归 root 所有，通常并不划算。
`
}

// plist 是 launchd 的 plist 结构。
//
// 手写类型 + xml.Marshal 而不是拼字符串：plist 是 XML，拼字符串迟早会在某个
// 路径带 & 或引号时产出非法文件，而这里出错只有等到用户下一次登录才会发现。
// 顺序也要保住——plist 的 dict 是有序的，而 Go 的 map 会乱序，所以用切片。
//
// 这几个类型都自己实现 MarshalXML：encoding/xml 对「key/value 交替的匿名结构」
// 没有内建表达，而 `,any` 标签会把元素名写成字段名（<Value>、<Items>），生成出来的
// 根本不是 plist。
type plist struct {
	Version string
	Items   []plistItem
}

type plistItem struct {
	Key   string
	Value plistValue
}

// plistValue 承载三种取值：字符串、字符串数组、嵌套字典。
type plistValue struct {
	Text  string
	Array []string
	Items []plistItem // 嵌套字典；非 nil 时优先于 Text
	// Bool 为真时输出 <true/>。launchd 的 KeepAlive / RunAtLoad 要求**真布尔**，
	// 以前写成 <string>true</string>，语义上是不对的（KeepAlive 的类型要求是
	// 布尔或字典），只是 launchd 有时容忍。
	Bool *bool
}

// MarshalXML 输出 <plist version="1.0"><dict>…</dict></plist>。
func (p plist) MarshalXML(e *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{
		Name: xml.Name{Local: "plist"},
		Attr: []xml.Attr{{Name: xml.Name{Local: "version"}, Value: p.Version}},
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := encodeDict(e, p.Items); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

// encodeDict 输出 <dict> 及其内容。
func encodeDict(e *xml.Encoder, items []plistItem) error {
	start := xml.StartElement{Name: xml.Name{Local: "dict"}}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, item := range items {
		if err := e.EncodeElement(item.Key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		if err := item.Value.encode(e); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// encode 按持有的类型输出取值。
func (v plistValue) encode(e *xml.Encoder) error {
	switch {
	case v.Bool != nil:
		name := "false"
		if *v.Bool {
			name = "true"
		}
		start := xml.StartElement{Name: xml.Name{Local: name}}
		if err := e.EncodeToken(start); err != nil {
			return err
		}
		return e.EncodeToken(start.End())
	case v.Items != nil:
		return encodeDict(e, v.Items)
	case v.Array != nil:
		start := xml.StartElement{Name: xml.Name{Local: "array"}}
		if err := e.EncodeToken(start); err != nil {
			return err
		}
		for _, item := range v.Array {
			if err := e.EncodeElement(item, xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
				return err
			}
		}
		return e.EncodeToken(start.End())
	default:
		return e.EncodeElement(v.Text, xml.StartElement{Name: xml.Name{Local: "string"}})
	}
}

// text 是字符串取值。
func text(value string) plistValue { return plistValue{Text: value} }

// boolean 是真布尔取值（<true/> / <false/>）。
func boolean(value bool) plistValue { return plistValue{Bool: &value} }

// array 是字符串数组取值。
func array(values []string) plistValue { return plistValue{Array: values} }

// dict 是嵌套字典取值。
func dict(items ...plistItem) plistValue { return plistValue{Items: items} }

// serviceUnit 生成 launchd plist。
func serviceUnit(opts serviceOptions) (string, error) {
	args := opts.serviceArgs()
	document := plist{
		Version: "1.0",
		Items: []plistItem{
			{Key: "Label", Value: text(launchdLabel)},
			{Key: "ProgramArguments", Value: array(args)},
			// KeepAlive 负责崩溃后自动拉起，与 Linux 的 Restart=always 同义。
			// 这两个必须是真布尔：写成 <string>true</string> 时 launchd 的
			// 类型校验会不认（KeepAlive 的合法类型是布尔或字典）。
			{Key: "KeepAlive", Value: boolean(true)},
			{Key: "RunAtLoad", Value: boolean(true)},
			{Key: "WorkingDirectory", Value: text(opts.workDirOrDefault())},
			{Key: "StandardOutPath", Value: text(logFilePath(opts))},
			{Key: "StandardErrorPath", Value: text(logFilePath(opts))},
			{Key: "ProcessType", Value: text("Interactive")},
			// launchd 给的 PATH 极简，而网关拉起 dsh 时要找 npx（常在
			// ~/.npm-global 或 Homebrew 里），所以这里显式补全。
			{Key: "EnvironmentVariables", Value: dict(
				plistItem{Key: "PATH", Value: text(strings.Join([]string{
					"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin",
					filepath.Dir(opts.Binary),
				}, ":"))},
			)},
		},
	}

	raw, err := xml.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("生成 plist 失败: %w", err)
	}
	// Go 的 xml 编码器只会写 <true></true>，而 **launchd 只认自闭合的 <true/>**：
	// 实测同一份 plist 里把布尔改成非自闭合形式，bootstrap 直接报
	// `Bootstrap failed: 5: Input/output error`（EIO 就是它说「解析不了」的方式），
	// plutil -lint 却判它合法——这个坑只有真的跑一次 launchctl 才看得见。
	// 所以这里把布尔标签改写掉；内容来自 bool，不可能与其它文本混淆。
	fixed := strings.NewReplacer(
		"<true></true>", "<true/>",
		"<false></false>", "<false/>",
	).Replace(string(raw))

	header := fmt.Sprintf("<!-- 由 dsh-gate %s 的 service unit 生成。改动后 launchctl bootout 再 bootstrap。 -->\n", version.Version)
	return header + fixed + "\n", nil
}

// logFilePath 返回服务日志落点。
//
// 放在 ~/Library/Logs 下而不是跟着登记目录走：服务管理器写日志这件事不该被
// 「换了 --run-dir」影响，日志位置越稳定越好找。
func logFilePath(opts serviceOptions) string {
	home := homeDir()
	if home == "" {
		home = "."
	}
	return filepath.Join(home, "Library", "Logs", serviceName+".log")
}

// serviceInstall 写入 plist 并让 launchd 加载。
func serviceInstall(opts serviceOptions) error {
	path, err := agentPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 LaunchAgents 目录失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(logFilePath(opts)), 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}

	text, err := serviceUnit(opts)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}

	uid, home, elevated := launchingUser()
	domain := "gui/" + uid
	// root 身份下没有可用的 GUI domain（LaunchAgent 属于登录用户）。与其让
	// launchctl 回一句 `Bootstrap failed: 125: Domain does not support specified
	// action`，不如在这里说清楚该怎么办。
	if domain == "gui/0" {
		return fmt.Errorf("当前是 root 身份，而 LaunchAgent 属于**登录用户**，"+
			"gui/0 这个 domain 不存在，装不了开机自启。\n"+
			"两条路：\n"+
			"  1. 不带 sudo 跑一次 %s service install（只装自启，不需要 root）；\n"+
			"  2. 非要用 sudo（例如为了把命令装进 /usr/local/bin），用 `sudo -E` 并"+
			"保留 SUDO_UID：sudo ./dsh-gate service install\n"+
			"命令本身也可以先单独装进 PATH：sudo install -m755 dsh-gate /usr/local/bin/dsh-gate",
			serviceName)
	}
	if elevated {
		// 用户多半是为了写 /usr/local/bin 才 sudo 的，这里说明我们把自启装给了谁。
		fmt.Printf("提示：当前是 sudo，开机自启会装给发起它的用户（uid %s，%s），"+
			"而不是 root——LaunchAgent 属于登录用户。\n", uid, home)
	}
	// 先卸旧的：重装时 launchd 会因为「已经加载过」而拒绝新配置，而用户看不出
	// 为什么。卸载失败无所谓——本来就没装过。
	_, _ = runServiceCommand("launchctl", "bootout", domain+"/"+launchdLabel)

	// bootout 是**异步**的：紧接着 bootstrap 同一个标签，可能撞上「还在拆卸中」，
	// 于是报一句与真实原因毫不相干的错（实测见过 `Bootstrap failed: 125:
	// Domain does not support specified action`），用户完全无从下手。给几次重试，
	// 再退到老的 load -w ——它走另一条代码路径，在 bootstrap 不认的场合常常能成。
	if output, err := bootstrapAgent(domain, path); err != nil {
		return fmt.Errorf("加载 LaunchAgent 失败: %w\n%s\n"+
			"plist 已经写好，可以手动加载：\n"+
			"  launchctl bootstrap %s %s\n"+
			"  launchctl load -w %s\n"+
			"（不影响使用：dsh-gate start 也能把管理端跑起来）",
			err, output, domain, path, path)
	}
	if output, err := runServiceCommand("launchctl", "enable", domain+"/"+launchdLabel); err != nil {
		return fmt.Errorf("启用 LaunchAgent 失败: %w\n%s", err, output)
	}
	if output, err := runServiceCommand("launchctl", "kickstart", "-k", domain+"/"+launchdLabel); err != nil {
		return fmt.Errorf("启动 LaunchAgent 失败: %w\n%s", err, output)
	}
	return nil
}

// bootstrapAgent 把 plist 加载进 launchd，失败时重试并退回老的 load -w。
//
// 两个手段都是为了「同样一条命令，这次不行下次行」这类 launchd 的时序脾气：
// bootout 还在拆卸中、上一条记录没清干净，都会让紧随其后的 bootstrap 失败。
func bootstrapAgent(domain, path string) ([]byte, error) {
	var lastOutput []byte
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		output, err := runServiceCommand("launchctl", "bootstrap", domain, path)
		if err == nil {
			return output, nil
		}
		lastErr, lastOutput = err, output
		// 第一次失败先等等，第二次再等久一点：给 launchd 时间把旧记录拆干净。
		time.Sleep(time.Duration(attempt) * 400 * time.Millisecond)
	}

	if output, err := runServiceCommand("launchctl", "load", "-w", path); err == nil {
		return output, nil
	}
	return lastOutput, lastErr
}

// serviceUninstall 卸载并删除 plist。
func serviceUninstall(serviceOptions) error {
	path, err := agentPath()
	if err != nil {
		return err
	}
	domain := "gui/" + currentUID()
	_, _ = runServiceCommand("launchctl", "bootout", domain+"/"+launchdLabel)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}

// serviceUnitInstalled 报告 LaunchAgent 是否已安装。
func serviceUnitInstalled() bool {
	path, err := agentPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func launchDomain() string { return "gui/" + currentUID() + "/" + launchdLabel }

// serviceStart 让 launchd 启动管理端。
func serviceStart() error {
	if output, err := runServiceCommand("launchctl", "kickstart", launchDomain()); err != nil {
		return fmt.Errorf("launchctl kickstart 失败: %w\n%s", err, output)
	}
	return nil
}

// serviceStop 让 launchd 停掉管理端。
func serviceStop() error {
	if output, err := runServiceCommand("launchctl", "kill", "SIGTERM", launchDomain()); err != nil {
		return fmt.Errorf("launchctl kill 失败: %w\n%s", err, output)
	}
	return nil
}

// serviceRestart 重启 launchd 托管的管理端。
func serviceRestart() error {
	if output, err := runServiceCommand("launchctl", "kickstart", "-k", launchDomain()); err != nil {
		return fmt.Errorf("launchctl kickstart -k 失败: %w\n%s", err, output)
	}
	return nil
}

// serviceStatus 报告 launchd 里的状态。
func serviceStatus() int {
	path, err := agentPath()
	if err != nil {
		return fatal("%v", err)
	}
	fmt.Printf("服务        %s\n", launchdLabel)
	fmt.Printf("配置文件    %s\n", path)
	if _, err := os.Stat(path); err != nil {
		fmt.Printf("开机自启    未安装\n")
		fmt.Println()
		fmt.Println("安装：dsh-gate service install")
		return 0
	}
	fmt.Printf("开机自启    已安装（登录时启动）\n")

	state := "未加载"
	if output, err := runServiceCommand("launchctl", "print", "gui/"+currentUID()+"/"+launchdLabel); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if strings.Contains(line, "state = ") {
				state = strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
			}
		}
	}
	fmt.Printf("当前状态    %s\n", state)
	if state == "running" {
		fmt.Println()
		fmt.Println("提示：实例已由 launchd 托管，请用 launchctl 管理，不要再 dsh-gate stop。")
	}
	return 0
}

// serviceInstalledHint 是安装成功后的提示。
//
// 参数里的 Tray 只有 Windows 用得着：macOS 上托管的是 LaunchAgent 本身，托盘是
// 另一个平台的概念。
func serviceInstalledHint(serviceOptions) string {
	return fmt.Sprintf(`已安装并启动 LaunchAgent（登录时自动运行）

  配置文件    ~/Library/LaunchAgents/%s.plist
  日志        ~/Library/Logs/%s.log
  实时看日志  log stream --predicate 'process == "dsh-gate"'
  立即重启    launchctl kickstart -k gui/%s/%s
  dsh-gate stop / start / restart   停止、启动、重启整个管理端
  dsh-gate service uninstall   只取消开机自启（它管的是这一个服务单元）
  dsh-gate uninstall      停止并删除所有实例，最后卸载管理端服务（会问是否删数据）

`, launchdLabel, serviceName, currentUID(), launchdLabel)
}

// launchingUser 返回「这次操作真正属于哪个用户」。
//
// 为什么不能直接用 os.Getuid()：用户为了把 dsh-gate 装进 /usr/local/bin 很自然地
// 会用 sudo 跑安装向导（旧版提示里也这么建议），而那时 euid 是 0，于是
// launchd 的 domain 变成 **gui/0**——这个 domain 根本不存在，bootstrap 必然失败：
//
//	Bootstrap failed: 125: Domain does not support specified action
//
// 实测就是这样卡住用户的。LaunchAgent 属于**登录用户**，所以 sudo 下要装到
// 发起 sudo 的那个用户名下：uid 取 SUDO_UID，家目录取 SUDO_USER 的家目录。
func launchingUser() (uid string, home string, elevated bool) {
	return resolveLaunchingUser(os.Geteuid(), os.Getenv("SUDO_UID"), os.Getenv("SUDO_USER"))
}

// resolveLaunchingUser 是 launchingUser 的纯函数版本：把 euid 与 SUDO_* 摊成参数，
// 这样 sudo 与 root 两个分支都能在测试里覆盖到——否则它们只有在真机上以 root
// 跑一次才走得到，而「以 root 跑」恰恰是最容易出问题、又最难复现的场景。
func resolveLaunchingUser(euid int, sudoUID, sudoUser string) (uid string, home string, elevated bool) {
	if euid == 0 {
		if trimmed := strings.TrimSpace(sudoUID); trimmed != "" {
			return trimmed, homeOfUser(sudoUser), true
		}
		// 没有 SUDO_UID：真的以 root 登录（不做图形登录的场景）。这时没有可用的
		// GUI domain，由调用方给出说明。
		return "0", homeOfUser(""), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return fmt.Sprintf("%d", euid), home, false
}

// homeOfUser 查某个用户的家目录；查不到时退回当前进程的家目录。
func homeOfUser(name string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		if account, err := user.Lookup(name); err == nil && account.HomeDir != "" {
			return account.HomeDir
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// currentUID 返回 launchd domain 要用的 uid（sudo 下是发起 sudo 的那个用户）。
func currentUID() string {
	uid, _, _ := launchingUser()
	return uid
}

// homeDir 返回 plist 与日志该放的家目录（sudo 下是发起 sudo 的那个用户）。
func homeDir() string {
	_, home, _ := launchingUser()
	return home
}

// runCommand 执行外部命令并合并输出。
func runServiceCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// serviceManagedCommand 返回 LaunchAgent 当前托管的命令行；读不到时返回空串。
func serviceManagedCommand() string {
	path, err := agentPath()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return programArgumentsFromPlist(string(raw))
}
