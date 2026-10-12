//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// Windows 上用计划任务：登录时启动，失败自动重试。
//
// 不用「启动文件夹里放个快捷方式」：那种方式既没有崩溃重启，也没有统一的
// 查看与卸载入口，用户过几个月就想不起来那东西是哪来的。schtasks 是系统自带的，
// 任务本身也能在「任务计划程序」图形界面里看到和改。

// taskName 是计划任务在系统里的名字。
const taskName = `\` + serviceName

// serviceFlavor 描述当前平台用的是什么机制。
func serviceFlavor() string { return "计划任务（登录时启动）" }

// platformServiceHelp 返回 Windows 特有的说明。
func platformServiceHelp() string {
	return `
Windows 说明:
  通过系统自带的 schtasks 创建任务，登录时启动；不需要管理员权限。
  任务可以在「任务计划程序」图形界面里查看和修改。
  日志：dsh-gate status -v，或直接看 <登记目录>\logs\<实例名>.log
`
}

// taskCommand 返回创建任务用的命令行。
func taskCommand(opts serviceOptions) []string {
	args := opts.serviceArgs()

	return []string{
		"schtasks", "/Create",
		"/TN", taskName,
		"/TR", `"` + strings.Join(quoteForTaskScheduler(args), " ") + `"`,
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F", // 同名任务已存在时覆盖，而不是失败
	}
}

// serviceUnit 在 Windows 上没有「配置文件」可打印，改为打印将执行的命令。
func serviceUnit(opts serviceOptions) (string, error) {
	var b strings.Builder
	b.WriteString("# Windows 上没有配置文件可打印：开机自启由计划任务实现。\n")
	b.WriteString("# 下面是 dsh-gate service install 实际会执行的东西：\n\n")
	for _, arg := range taskCommand(opts) {
		b.WriteString(arg + " ")
	}
	b.WriteString("\n\n# 日志：运行 dsh-gate status -v，或直接看登记目录下的 logs。\n")
	return b.String(), nil
}

// serviceInstall 创建计划任务并立即运行一次。
func serviceInstall(opts serviceOptions) error {
	command := taskCommand(opts)
	if output, err := runServiceCommand(command[0], command[1:]...); err != nil {
		return fmt.Errorf("创建计划任务失败: %w\n%s", err, output)
	}
	// 立刻跑一次，省得用户还要等下次登录。
	if output, err := runServiceCommand("schtasks", "/Run", "/TN", taskName); err != nil {
		return fmt.Errorf("启动任务失败: %w\n%s", err, output)
	}
	return nil
}

// serviceUninstall 删除计划任务。
func serviceUninstall(serviceOptions) error {
	if output, err := runServiceCommand("schtasks", "/Delete", "/TN", taskName, "/F"); err != nil {
		if strings.Contains(string(output), "cannot find") ||
			strings.Contains(string(output), "找不到") {
			return nil // 本来就没装，不算失败
		}
		return fmt.Errorf("删除计划任务失败: %w\n%s", err, output)
	}
	return nil
}

// serviceStatus 报告计划任务的状态。
func serviceStatus() int {
	fmt.Printf("服务        %s\n", serviceName)
	fmt.Printf("计划任务    %s\n", taskName)

	output, err := runServiceCommand("schtasks", "/Query", "/TN", taskName)
	if err != nil {
		fmt.Printf("开机自启    未安装\n")
		fmt.Println()
		fmt.Println("安装：dsh-gate service install")
		return 0
	}

	fmt.Printf("开机自启    已安装（登录时启动）\n")
	if running := strings.Contains(string(output), "Running"); running {
		fmt.Printf("当前状态    运行中\n")
	} else {
		fmt.Printf("当前状态    已就绪，等待下次登录\n")
	}
	return 0
}

// serviceInstalledHint 是安装成功后的提示。
func serviceInstalledHint() string {
	return fmt.Sprintf(`已安装并启动计划任务（登录时自动运行）

  任务名      %s
  查看/修改   任务计划程序 → 任务计划程序库
  立即重启    schtasks /End /TN %s && schtasks /Run /TN %s
  dsh-gate service uninstall   只取消开机自启（它管的是这一个服务单元）
  dsh-gate uninstall <实例名|all>  停掉指定实例；--service 取消自启、--purge 删数据

`, taskName, taskName, taskName)
}

// runCommand 执行外部命令并合并输出。
func runServiceCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// serviceManagedCommand 返回计划任务当前托管的命令行；读不到时返回空串。
func serviceManagedCommand() string {
	output, err := runServiceCommand("schtasks", "/Query", "/TN", taskName, "/XML")
	if err != nil {
		return ""
	}
	return commandFromTaskXML(string(output))
}
