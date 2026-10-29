package main

import (
	"os"
	"strings"
	"unicode/utf16"
)

// 本文件把「Windows 计划任务该怎么描述」抽成不依赖平台的纯字符串逻辑：任务
// XML 的生成、参数引用与 UTF-16 编码都在这里。
//
// 为什么要抽出来：Windows 的安装路径没法在开发机（macOS / Linux）上真跑，而这
// 段字符串一旦写错，症状是「任务建好了、一登录就失败」，比建不出来更难查。抽成
// 纯函数之后，XML 结构与转义在任何机器上都能被测试钉住。

// taskXML 生成计划任务的 XML 定义。
//
// 为什么不用 `schtasks /Create /SC ONLOGON ...` 那一套开关：非管理员执行时它
// 直接回一句「错误: 拒绝访问。」——ONLOGON 触发器默认是「不管用户是否登录都
// 运行」，那需要保存凭据，等于要管理员权限。改成 XML 里显式写
// LogonType=InteractiveToken（只在当前用户登录时、以当前用户身份、最低权限
// 运行）之后，普通用户也能注册成功，语义也正好是我们要的「登录时启动」。
//
// userID 是形如 `WIN11\admin` 的账号；计划任务的触发器与运行身份都用它。
func taskXML(opts serviceOptions, userID string) string {
	user := xmlEscape(userID)
	// 动作是 `service run` 而不是 `start --foreground`：计划任务给控制台程序开会
	// 一个可见窗口，用户关掉它就等于把管理端关掉。`service run` 会先隐藏控制台
	// （见 service_console_windows.go）。
	args := opts.taskArgs()

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>` + "\n")
	b.WriteString(`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` + "\n")
	b.WriteString("  <RegistrationInfo>\n")
	if userID != "" {
		b.WriteString("    <Author>" + user + "</Author>\n")
	}
	b.WriteString("    <Description>" + xmlEscape("dsh-gate 管理端（登录时启动）") + "</Description>\n")
	b.WriteString("  </RegistrationInfo>\n")

	b.WriteString("  <Triggers>\n    <LogonTrigger>\n      <Enabled>true</Enabled>\n")
	if userID != "" {
		b.WriteString("      <UserId>" + user + "</UserId>\n")
	}
	b.WriteString("    </LogonTrigger>\n  </Triggers>\n")

	b.WriteString("  <Principals>\n    <Principal id=\"Author\">\n")
	if userID != "" {
		b.WriteString("      <UserId>" + user + "</UserId>\n")
	}
	b.WriteString("      <!-- 只在用户登录时、以该用户身份运行：不必保存凭据，因此普通用户也能注册。 -->\n")
	b.WriteString("      <LogonType>InteractiveToken</LogonType>\n")
	b.WriteString("      <RunLevel>LeastPrivilege</RunLevel>\n")
	b.WriteString("    </Principal>\n  </Principals>\n")

	b.WriteString(`  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <!-- PT0S 表示不限制运行时长：管理端本来就该一直跑着。 -->
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
  </Settings>
`)
	b.WriteString("  <Actions Context=\"Author\">\n    <Exec>\n")
	b.WriteString("      <Command>" + xmlEscape(args[0]) + "</Command>\n")
	if rest := args[1:]; len(rest) > 0 {
		b.WriteString("      <Arguments>" + xmlEscape(joinCommandLine(rest)) + "</Arguments>\n")
	}
	b.WriteString("    </Exec>\n  </Actions>\n</Task>\n")
	return b.String()
}

// joinCommandLine 按 Windows 命令行规则把参数拼成一整行。
//
// 含空格或制表符的参数必须用双引号包住：`C:\Program Files\...` 这类路径在
// Windows 上几乎必然带空格，不引起来任务能建、一启动就失败。参数里本身带的
// 双引号按惯例写成两个双引号。
func joinCommandLine(args []string) string {
	out := make([]string, len(args))
	for i, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t") {
			out[i] = `"` + strings.ReplaceAll(arg, `"`, `""`) + `"`
			continue
		}
		out[i] = arg
	}
	return strings.Join(out, " ")
}

// xmlEscape 转义 XML 文本内容里会破坏结构的三个字符。
//
// Arguments 里出现 `&`、`<`、`>` 并不罕见（口令、路径里都可能带），不转义的话
// 任务 XML 会直接解析失败，而 schtasks 只会回一句「The task XML is malformed.」。
//
// 引号不转义：生成的 XML 没有携带用户输入的属性值，文本内容里的 `"` 也不需要
// 转义；写成 &quot; 只会让 `service unit` 打印出来的命令行变成一串看不懂的
// 实体，用户核对参数时反而更费劲。
func xmlEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return replacer.Replace(value)
}

// writeTaskFile 把任务 XML 写成 schtasks 认识的 UTF-16LE 文件，返回文件路径。
//
// 任务计划程序只接受 UTF-16 的任务 XML：同一份内容存成 UTF-8 时会报
// 「The task XML is malformed.」，所以这里必须自己转码（BOM 也要写）。
func writeTaskFile(dir, text string) (string, error) {
	file, err := os.CreateTemp(dir, "dsh-gate-task-*.xml")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(utf16LEWithBOM(text)); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// utf16LEWithBOM 把文本编码成带 BOM 的 UTF-16LE。
func utf16LEWithBOM(text string) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 0, 2+len(units)*2)
	out = append(out, 0xff, 0xfe)
	for _, unit := range units {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return out
}
