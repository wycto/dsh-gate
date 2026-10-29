//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Windows 上用计划任务：登录时启动，失败自动重试。
//
// 不用「启动文件夹里放个快捷方式」：那种方式既没有崩溃重启，也没有统一的
// 查看与卸载入口，用户过几个月就想不起来那东西是哪来的。schtasks 是系统自带的，
// 任务本身也能在「任务计划程序」图形界面里看到和改。
//
// 注册走任务 XML（见 taskxml.go）而不是 `schtasks /SC ONLOGON` 那几个开关：
// 开关式创建在非管理员下会被系统拒绝（「错误: 拒绝访问。」），因为 ONLOGON
// 默认按「不管用户是否登录都运行」来配，需要存凭据；XML 里写
// InteractiveToken + 最低权限就不需要管理员了，语义也正好是「登录时启动」。

// taskName 是计划任务在系统里的名字。
const taskName = `\` + serviceName

// serviceFlavor 描述当前平台用的是什么机制。
func serviceFlavor() string { return "计划任务（登录时启动）" }

// platformServiceHelp 返回 Windows 特有的说明。
func platformServiceHelp() string {
	return `
Windows 说明:
  用系统自带的计划任务实现「登录时启动」：任务以当前用户身份、最低权限运行，
  因此注册时不需要管理员权限。
  计划任务执行的是 ` + "`dsh-gate service run`" + `：它会隐藏控制台，
  所以不会出现「关掉那个终端窗口服务就停了」，也没有终端窗口可以误关。
  加 ` + "`--tray`" + ` 则改成托盘形态：任务动作换成 ` + "`dsh-gate-tray.exe tray`" + `（GUI 子系统，
  系统根本不会给它开控制台），登录时起来的是通知区图标，由它保证管理端在跑；
  双击图标打开管理台，右键菜单可以启动 / 停止 / 重启，退出托盘不停服务。
  任务可以在「任务计划程序」图形界面里查看和修改。
  日志：dsh-gate status -v，或直接看 <登记目录>\logs\<实例名>.log
`
}

// taskUserID 返回计划任务要用的账号，形如 `WIN11\admin`。
//
// 触发器与运行身份都写成这个账号：任务只在「这个用户登录」时启动，以这个用户
// 的身份跑，因此不需要保存口令（存了口令才需要管理员）。
func taskUserID() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	if name := os.Getenv("USERNAME"); name != "" {
		if domain := os.Getenv("USERDOMAIN"); domain != "" {
			return domain + `\` + name
		}
		return name
	}
	return ""
}

// serviceUnit 在 Windows 上没有「配置文件」可打印，改为打印任务 XML。
func serviceUnit(opts serviceOptions) (string, error) {
	var b strings.Builder
	b.WriteString("# Windows 上没有配置文件：开机自启由计划任务实现。\n")
	b.WriteString("# 下面是 dsh-gate service install 注册的任务 XML：\n\n")
	b.WriteString(taskXML(opts, taskUserID()))
	b.WriteString("\n# 注册命令（XML 必须以 UTF-16 编码保存）：\n")
	b.WriteString("#   schtasks /Create /TN " + taskName + ` /XML <文件> /F` + "\n")
	return b.String(), nil
}

// serviceInstall 注册计划任务并立即运行一次。
func serviceInstall(opts serviceOptions) error {
	// 先把已经在跑的管理端停掉：/Create /F 会把任务整个替换掉，旧任务里的进程
	// 于是变成孤儿继续占着端口，新实例一启动就 bind 失败（用户看到「装完了却
	// 打不开」）。stopPortal 走的是管理端自己的停机接口，不管当初是谁启动的都管。
	stopPortal(resolveRunDir(""), 10*time.Second, true)

	path, err := writeTaskFile("", taskXML(opts, taskUserID()))
	if err != nil {
		return fmt.Errorf("写临时任务文件失败: %w", err)
	}
	defer os.Remove(path)

	if output, err := runServiceCommand("schtasks", "/Create", "/TN", taskName, "/XML", path, "/F"); err != nil {
		return fmt.Errorf("创建计划任务失败: %w%s\n\n%s", err, childFailureText(output), taskCreateHint)
	}
	// 立刻跑一次，省得用户还要等下次登录。
	if err := startTask(); err != nil {
		return fmt.Errorf("%w\n\n任务已经注册，下次登录会自动启动；现在也可以手动运行：schtasks /Run /TN %s", err, taskName)
	}
	return nil
}

// taskCreateHint 是创建计划任务失败时的自救提示。
const taskCreateHint = `可以改用图形界面手动导入：先运行
  dsh-gate service unit > task.xml
再把内容另存为 UTF-16 编码的 task.xml，在「任务计划程序」里「导入任务」；
或者不装自启，直接用 dsh-gate start 手动启动。`

// serviceUninstall 删除计划任务。
func serviceUninstall(serviceOptions) error {
	// 先看有没有：没装就直接返回，不必去猜 schtasks 那句本地化的「找不到」。
	if !serviceUnitInstalled() {
		return nil
	}
	if output, err := runServiceCommand("schtasks", "/Delete", "/TN", taskName, "/F"); err != nil {
		return fmt.Errorf("删除计划任务失败: %w%s", err, childFailureText(output))
	}
	return nil
}

// serviceUnitInstalled 报告计划任务是否已安装。
func serviceUnitInstalled() bool {
	_, err := runServiceCommand("schtasks", "/Query", "/TN", taskName)
	return err == nil
}

// serviceStart 启动计划任务托管的管理端。
//
// 已经在跑就直接返回：schtasks /Run 对正在运行的任务会报错，而「启动一个已经
// 启动的服务」不该算失败。
//
// 托盘形态下不能走 /Run：任务动作本身是个长驻进程，任务因此一直处于「正在运行」，
// 而它的 MultipleInstancesPolicy 是 IgnoreNew——再触发一次什么也不会发生，管理端
// 永远起不来，`dsh-gate start` 还会在 30 次重试之后报「启动计划任务失败」。这种
// 时候按保存的配置直接后台拉起管理端就够了：托盘自己的定时刷新会看见它、点亮图标。
func serviceStart() error {
	if taskAlive() {
		return nil
	}
	if trayTaskInstalled() {
		return startPortalDetached()
	}
	return startTask()
}

// trayTaskInstalled 报告已注册的计划任务是不是托盘形态（动作指向 dsh-gate-tray.exe）。
func trayTaskInstalled() bool {
	return commandHostsTray(serviceManagedCommand())
}

// startPortalDetached 按保存的配置后台拉起管理端，完全不经过计划任务。
//
// 只用于托盘形态：那种形态下计划任务里跑的是托盘，触发它既不可能（见 serviceStart）
// 也不合理（我们要的是管理端，不是第二个托盘）。
func startPortalDetached() error {
	stateDir, err := resolveStateDir("")
	if err != nil {
		return err
	}
	cfg, err := loadPortalConfig(stateDir)
	if err != nil || len(cfg.Args) == 0 {
		return fmt.Errorf("没有找到管理端的启动配置（%s）：先手动跑一次 dsh-gate start 或 dsh-gate install",
			portalConfigPath(stateDir))
	}
	if code := startFromConfig(cfg); code != 0 {
		return fmt.Errorf("启动管理端失败（退出码 %d），详见 %s", code,
			filepath.Join(logDirFor(resolveRunDir("")), portalRecordName+".log"))
	}
	return nil
}

// serviceStop 停掉计划任务托管的管理端，并等进程真的退出。
//
// 不再用 `schtasks /End`：任务里跑的那个进程只是启动器，它把管理端派成没有控制台
// 的子进程之后就退出了（见 service_console_windows.go 的 detachServiceConsole），
// `/End` 结束的是这个已经不在的启动器，管理端一根毛都掉不到。改成走管理端自己的
// 停机端点，与 Linux / macOS 以及没装自启时的 `stop` 同一条路，收尾流程也就只有一份。
//
// 要等：调用方（restart）紧接着启动，端口没释放就会撞上「地址已被占用」。
func serviceStop() error {
	return stopTaskPortal(10 * time.Second)
}

// stopTaskPortal 优雅停掉登记在案的管理端；端点叫不动才直接结束进程。
func stopTaskPortal(wait time.Duration) error {
	record, running := portalRecord(resolveRunDir(""))
	if !running {
		return nil
	}
	if code := stopPortal(resolveRunDir(""), wait, true); code == 0 {
		waitTaskDeath(taskStopTimeout)
		return nil
	}
	// 端点不回应说明进程卡死。以前 /End 还能兜底，现在它已经不是终止手段了。
	// 强杀会把 dsh 实例孤立掉，但下次启动会按令牌接管回来（见 tokenfile）。
	if err := terminateProcess(record.PID); err != nil {
		return fmt.Errorf("停机端点无回应，结束进程 %d 也失败: %w", record.PID, err)
	}
	waitTaskDeath(taskStopTimeout)
	return nil
}

// serviceRestart 重启计划任务：先停稳，再启动。
func serviceRestart() error {
	if err := serviceStop(); err != nil {
		return err
	}
	return serviceStart()
}

// taskStopTimeout 是「结束任务」最多等多久。正常只要几十毫秒，给到 15 秒是
// 为了容忍实例多、落盘慢的机器。
const taskStopTimeout = 15 * time.Second

// taskAlive 报告登记目录里是否有活着的管理端进程。
//
// 不解析 schtasks 的状态文本：它是按系统语言本地化的，靠不住；登记目录里的
// pid 才是我们自己写的。
func taskAlive() bool {
	_, running := portalRecord(resolveRunDir(""))
	return running
}

// waitTaskDeath 等到管理端进程退出，最多等 timeout。
func waitTaskDeath(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !taskAlive() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startTask 触发计划任务，并确认进程真的起来了。
//
// 只调一次 /Run 不够：上一个进程可能还没完全释放端口，新进程起来后 bind 失败
// 直接退出（用户看到的就是「重启后打不开」）。所以这里「启动 → 等进程出现」
// 循环重试，直到登记目录里出现活着的管理端。
func startTask() error {
	var last error
	for attempt := 0; attempt < 30; attempt++ {
		out, err := runServiceCommand("schtasks", "/Run", "/TN", taskName)
		if err == nil {
			if waitTaskAlive(3 * time.Second) {
				return nil
			}
			last = fmt.Errorf("任务已触发，但管理端进程没有起来（端口可能仍被上一个进程占着）")
		} else {
			last = fmt.Errorf("%w%s", err, childFailureText(out))
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("启动计划任务失败: %w", last)
}

// waitTaskAlive 等到管理端进程出现，最多等 timeout。
func waitTaskAlive(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if taskAlive() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// serviceStatus 报告计划任务的状态。
func serviceStatus() int {
	fmt.Printf("服务        %s\n", serviceName)
	fmt.Printf("计划任务    %s\n", taskName)

	if !serviceUnitInstalled() {
		fmt.Printf("开机自启    未安装\n\n")
		fmt.Println("安装：dsh-gate service install")
		return 0
	}

	// 形态也要报出来：两种自启的用户体验差别就在「有没有一个窗口」，
	// 装了哪一版不该让人靠回忆去猜。
	form := "启动器形态，登录时终端窗口闪一下"
	if trayTaskInstalled() {
		form = "托盘形态，通知区图标，没有终端窗口"
	}
	fmt.Printf("开机自启    已安装（登录时启动，%s）\n", form)
	// 不解析 schtasks 的状态文本：它是按系统语言本地化的，中文 Windows 上既没有
	// "Running" 也未必有「正在运行」；直接看登记目录里有没有活着的管理端更可靠。
	if _, running := portalRecord(resolveRunDir("")); running {
		fmt.Printf("当前状态    运行中\n")
	} else {
		fmt.Printf("当前状态    已就绪，等待下次登录\n")
	}
	return 0
}

// serviceInstalledHint 是安装成功后的提示。
//
// 两种形态要说的话不一样，而且差别正是用户最在意的那件事（有没有一个窗口要守
// 着），所以这里分叉而不是写一段含糊的通用话。
func serviceInstalledHint(opts serviceOptions) string {
	window := `  启动时 Windows 会给任务开一个终端窗口，它只闪一下：那个窗口里跑的是启动器，
  它把管理端派到没有控制台的后代进程里就退出了。所以没有「顺手关掉窗口就把
  服务关了」这回事，停用请用上面的命令。
`
	if opts.Tray {
		window = `  通知区里会有一个 dsh-gate 图标：圆点是管理端的运行状态（绿色在跑、灰色停了），
  双击打开管理台，右键可以启动 / 停止 / 重启。图标只负责看与点，管理端是独立的
  进程——退出托盘（右键最后一项）不会停服务，也没有任何终端窗口需要守着。
`
	}
	form := "计划任务"
	if opts.Tray {
		form = "托盘"
	}
	return fmt.Sprintf(`已安装并启动%s（登录时自动运行）

  任务名      %s
  查看/修改   任务计划程序 → 任务计划程序库
  停止/重启   dsh-gate stop / start / restart
  dsh-gate service uninstall   只取消开机自启（它管的是这一个服务单元）
  dsh-gate uninstall      停止并删除所有实例，最后卸载管理端服务（会问是否删数据）

%s
`, form, taskName, window)
}

// runServiceCommand 执行外部命令并合并输出。
func runServiceCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// childFailureText 把外部命令的失败输出变成可以安全显示的一段文字。
//
// schtasks 这类系统工具按控制台代码页输出（中文 Windows 是 GBK），把它的字节
// 原样写进 UTF-8 终端就是「����: �ܾ����ʡ�」。合法 UTF-8 才附上原文，否则只报
// 退出码：看得懂比看得全重要。
func childFailureText(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" || !utf8.ValidString(text) {
		return ""
	}
	return "\n" + text
}

// serviceManagedCommand 返回计划任务当前托管的命令行；读不到时返回空串。
func serviceManagedCommand() string {
	output, err := runServiceCommand("schtasks", "/Query", "/TN", taskName, "/XML")
	if err != nil {
		return ""
	}
	return commandFromTaskXML(string(output))
}
