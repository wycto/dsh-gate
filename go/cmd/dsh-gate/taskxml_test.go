package main

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

// 本文件的测试都跑在「不依赖平台」的纯字符串函数上，因此 macOS / Linux 的开发机
// 也能跑：Windows 的安装路径在开发机上没法真跑，只能这样把易错处钉住。真出错的
// 后果很具体——任务能建、一登录就失败，或者 schtasks 回一句「XML 格式不对」。

// taskTestOptions 返回一份带空格路径的典型输入。
func taskTestOptions() serviceOptions {
	return serviceOptions{
		Binary: `C:\Program Files\dsh-gate\dsh-gate.exe`,
		Overrides: map[string]string{
			"host":      "0.0.0.0",
			"port":      "12020",
			"state-dir": `C:\Users\demo user\.dsh-gate`,
		},
	}
}

// TestTaskXMLRegistersInteractiveLogonTask 锁住任务 XML 的几处关键声明。
//
// 少了 InteractiveToken，非管理员注册就会被系统拒绝（原来的开关式创建正是
// 栽在这里）；少了 RunLevel=LeastPrivilege 会要求提权；ExecutionTimeLimit
// 不是 PT0S 的话管理端会被计划任务在三小时后掐掉。
func TestTaskXMLRegistersInteractiveLogonTask(t *testing.T) {
	xml := taskXML(taskTestOptions(), `WIN11\demo`)

	for _, want := range []string{
		"<LogonTrigger>",
		`<UserId>WIN11\demo</UserId>`,
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<Command>C:\\Program Files\\dsh-gate\\dsh-gate.exe</Command>",
		`<Arguments>service run --host 0.0.0.0 --port 12020 --state-dir "C:\Users\demo user\.dsh-gate"</Arguments>`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("任务 XML 缺少 %q\n----\n%s", want, xml)
		}
	}
}

// TestTaskXMLOmitsArgumentsWhenThereAreNone 只有程序名时不该写空的 <Arguments>。
func TestTaskXMLOmitsArgumentsWhenThereAreNone(t *testing.T) {
	xml := taskXML(serviceOptions{Binary: `C:\dsh-gate.exe`}, `WIN11\demo`)
	if strings.Contains(xml, "<Arguments></Arguments>") || strings.Contains(xml, "<Arguments/>") {
		t.Errorf("没有参数时不该产出空 Arguments：\n%s", xml)
	}
	if !strings.Contains(xml, "<Command>C:\\dsh-gate.exe</Command>") {
		t.Errorf("程序路径没写进去：\n%s", xml)
	}
}

// TestTaskXMLEscapesSpecialCharacters 覆盖 & < > 与账号里的特殊字符。
//
// 这些字符在路径与口令参数里都可能出现；不转义的话 schtasks 只会回
// 「The task XML is malformed.」，用户完全看不出是哪个字符惹的祸。
func TestTaskXMLEscapesSpecialCharacters(t *testing.T) {
	opts := serviceOptions{
		Binary:    `C:\Tools & More\dsh-gate.exe`,
		Overrides: map[string]string{"state-dir": `C:\a<b>&c`},
	}
	xml := taskXML(opts, `DOM\a&b`)

	for _, leaked := range []string{"<Command>C:\\Tools & More", "a<b>&c", "<UserId>DOM\\a&b"} {
		if strings.Contains(xml, leaked) {
			t.Errorf("未转义的 %q 混进了 XML：\n%s", leaked, xml)
		}
	}
	for _, want := range []string{
		`<Command>C:\Tools &amp; More\dsh-gate.exe</Command>`,
		`<Arguments>service run --state-dir C:\a&lt;b&gt;&amp;c</Arguments>`,
		`<UserId>DOM\a&amp;b</UserId>`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("转义结果缺少 %q\n----\n%s", want, xml)
		}
	}
}

// TestJoinCommandLineQuotesSpacedArguments 覆盖参数引用：Windows 上路径几乎必然
// 带空格，不引起来任务能建、一启动就失败。参数里本身带引号时按惯例写成两个。
func TestJoinCommandLineQuotesSpacedArguments(t *testing.T) {
	got := joinCommandLine([]string{
		"start", "--state-dir", `C:\Users\demo user\.dsh-gate`,
		"--flag", "plain", "--say", `a "b" c`,
	})
	want := `start --state-dir "C:\Users\demo user\.dsh-gate" --flag plain --say "a ""b"" c"`
	if got != want {
		t.Errorf("命令行 = %q，期望 %q", got, want)
	}
}

// TestWriteTaskFileIsUTF16LEWithBOM 锁住编码：任务计划程序只认 UTF-16 的任务
// XML，存成 UTF-8 会报「The task XML is malformed.」。
func TestWriteTaskFileIsUTF16LEWithBOM(t *testing.T) {
	text := "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n<Task>唯一</Task>\n"
	path, err := writeTaskFile(t.TempDir(), text)
	if err != nil {
		t.Fatalf("写任务文件失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回任务文件失败: %v", err)
	}
	if len(raw) < 2 || raw[0] != 0xff || raw[1] != 0xfe {
		t.Fatalf("缺少 UTF-16LE BOM，前 4 字节 = % x", raw[:min(4, len(raw))])
	}
	units := make([]uint16, 0, (len(raw)-2)/2)
	for i := 2; i+1 < len(raw); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(raw[i:i+2]))
	}
	if got := string(utf16.Decode(units)); got != text {
		t.Errorf("解码结果 = %q，期望 %q", got, text)
	}
}
