//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/wycto/dsh-gate/internal/registry"
	"github.com/wycto/dsh-gate/internal/version"
)

// Linux 上用 systemd：这是「装完就能开机自启 + 崩了自动拉起」的通用答案，
// 而无界面服务器场景本来就得靠它。

// unitPath 是单元文件的位置。
func unitPath() string { return "/etc/systemd/system/" + serviceName + ".service" }

// serviceFlavor 描述当前平台用的是什么机制。
func serviceFlavor() string { return "systemd 系统服务" }

// platformServiceHelp 返回 Linux 特有的说明。
func platformServiceHelp() string {
	return `
Linux 说明:
  install 需要 root：sudo dsh-gate service install [选项]
  装好之后：
    systemctl status dsh-gate      查看状态
    journalctl -u dsh-gate -f      跟踪日志
    systemctl restart dsh-gate    重启
  想改端口或实例目录：systemctl edit dsh-gate，写一个 [Service] ExecStart= 覆盖。
`
}

// serviceUnit 生成 systemd 单元文本。
func serviceUnit(opts serviceOptions) (string, error) {
	exec := strings.Join(quoteArgs(opts.serviceArgs()), " ")
	var b strings.Builder
	fmt.Fprintf(&b, "# 由 dsh-gate %s 的 service unit 生成。改动后请 systemctl daemon-reload。\n", version.Version)
	b.WriteString("#\n# 想改端口或实例目录：systemctl edit " + serviceName + "，\n")
	b.WriteString("# 写一个 [Service] ExecStart= 覆盖即可，不必重新生成。\n\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=DSH 局域网网关（dsh-gate " + version.Version + "）\n")
	b.WriteString("Documentation=https://github.com/wycto/dsh-gate\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	if opts.User != "" {
		fmt.Fprintf(&b, "User=%s\n", opts.User)
	}
	// WorkingDirectory 不是可有可无的：dsh 把启动时的当前目录当默认 workspace，
	// systemd 的默认 "/" 会让会话看到一个毫无意义的根。
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", opts.workDirOrDefault())
	fmt.Fprintf(&b, "ExecStart=%s\n", exec)
	b.WriteString("Restart=always\n")
	// 首次启动要下载插件，给足冷启动时间；连续失败 10 秒 3 次才放弃，
	// 免得网络抖动导致服务被判定为起不来。
	b.WriteString("RestartSec=5s\n")
	b.WriteString("TimeoutStopSec=30s\n")
	b.WriteString("KillSignal=SIGTERM\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String(), nil
}

// serviceInstall 写入单元文件并启用。
func serviceInstall(opts serviceOptions) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("安装 systemd 服务需要 root：sudo dsh-gate service install [选项]")
	}
	text, err := serviceUnit(opts)
	if err != nil {
		return err
	}
	if err := os.WriteFile(unitPath(), []byte(text), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", unitPath(), err)
	}
	for _, step := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", serviceName},
		{"systemctl", "restart", serviceName},
	} {
		if output, err := runServiceCommand(step[0], step[1:]...); err != nil {
			return fmt.Errorf("%s %s 失败: %w\n%s", step[0], strings.Join(step[1:], " "), err, output)
		}
	}
	return nil
}

// serviceUninstall 停用并删除单元文件。
func serviceUninstall(serviceOptions) error {
	// 停用即使单元不存在也不算失败，否则「重装系统后想清理」会变成一件需要
	// 记住顺序的事。
	if output, err := runServiceCommand("systemctl", "disable", "--now", serviceName); err != nil &&
		!strings.Contains(string(output), "No such file") {
		return fmt.Errorf("停用服务失败: %w\n%s", err, output)
	}
	if err := os.Remove(unitPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败: %w", unitPath(), err)
	}
	_, _ = runServiceCommand("systemctl", "daemon-reload")
	return nil
}

// serviceStatus 报告 systemd 里的状态。
func serviceStatus() int {
	output, err := runServiceCommand("systemctl", "is-active", serviceName)
	state := strings.TrimSpace(string(output))
	if err != nil && state == "" {
		state = "unknown"
	}

	enabled := "未设置"
	if out, err := runServiceCommand("systemctl", "is-enabled", serviceName); err == nil {
		enabled = strings.TrimSpace(string(out))
	}

	fmt.Printf("服务        %s\n", serviceName)
	fmt.Printf("单元文件    %s\n", unitPath())
	fmt.Printf("开机自启    %s\n", enabled)
	fmt.Printf("当前状态    %s\n", state)
	if state != "active" {
		fmt.Println()
		fmt.Println("安装并启用：sudo dsh-gate service install")
	}
	if state == "active" {
		if _, ok := processRecordExists(); ok {
			fmt.Println()
			fmt.Println("提示：实例已由 systemd 托管，请用 systemctl 管理，不要再 dsh-gate stop。")
		}
	}
	return 0
}

// serviceInstalledHint 是安装成功后的提示。
func serviceInstalledHint() string {
	return fmt.Sprintf(`已安装并启动 %s

  systemctl status %s     查看状态
  journalctl -u %s -f     跟踪日志
  systemctl restart %s    重启
  dsh-gate status         查看各个实例（多实例时更直观）
  dsh-gate service uninstall   只取消开机自启（它管的是这一个服务单元）
  dsh-gate uninstall <实例名|all>  停掉指定实例；--service 取消自启、--purge 删数据

`, serviceName, serviceName, serviceName, serviceName)
}

// quoteArgs 给含空格的参数加引号。
func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		if strings.ContainsAny(arg, " \t\"'") {
			out[i] = `"` + strings.ReplaceAll(arg, `"`, `\"`) + `"`
			continue
		}
		out[i] = arg
	}
	return out
}

// runCommand 执行外部命令并合并输出。
func runServiceCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// processRecordExists 报告本机是否有登记中的实例在跑。
func processRecordExists() (any, bool) {
	records, _ := registry.DefaultDir().List()
	for _, record := range records {
		if record.Alive() {
			return record, true
		}
	}
	return nil, false
}

// serviceManagedCommand 返回 systemd 单元当前托管的命令行；读不到时返回空串。
//
// 取消自启前先把这条打出来：服务单元是机器级的一个文件，用户点的是「取消自启」，
// 但他真正想知道的是「刚刚被取消的是哪个实例」。单元内容是唯一诚实的答案——
// 尤其当它跑的是 `--config` 那种一条命令带多个实例的情况。
func serviceManagedCommand() string {
	raw, err := os.ReadFile(unitPath())
	if err != nil {
		return ""
	}
	return execStartFromUnit(string(raw))
}
