package main

import (
	"regexp"
	"strings"
)

// 本文件是「服务单元里到底托管着什么」的解析器。
//
// 取消自启前要把它打出来：服务单元是**机器级**的（一个平台只有一个），用户点
// 的是「取消自启」，但真正想知道的是「刚刚被取消的是哪个实例/哪条命令」。三个
// 平台各有一份格式，解析都写成不依赖平台的纯函数——Windows 那条路径在开发机上
// 跑不了，只有抽出来才钉得住。

// stringElementPattern 匹配 plist 里的 <string>…</string>。
var stringElementPattern = regexp.MustCompile(`<string>(.*?)</string>`)

// programArgumentsFromPlist 从 launchd plist 文本里取出 ProgramArguments 数组。
//
// 自己做这层解析而不是 encoding/xml：plist 的 <dict> 没有元素名，用结构体
// 反序列化要写一堆只此一用的大小写规则；这里只需要「某个 key 后面那段 <string>
// 列表」，正则反而更直白。
func programArgumentsFromPlist(text string) string {
	key := strings.Index(text, "<key>ProgramArguments</key>")
	if key < 0 {
		return ""
	}
	rest := text[key:]
	open := strings.Index(rest, "<array>")
	if open < 0 {
		return ""
	}
	rest = rest[open:]
	closeIdx := strings.Index(rest, "</array>")
	if closeIdx < 0 {
		return ""
	}

	var args []string
	for _, match := range stringElementPattern.FindAllStringSubmatch(rest[:closeIdx], -1) {
		args = append(args, unescapeXMLEntities(match[1]))
	}
	return strings.Join(args, " ")
}

// unescapeXMLEntities 还原 plist 里的实体，够用即可（渲染侧只可能产出这几种）。
func unescapeXMLEntities(value string) string {
	replacer := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#34;", `"`, "&amp;", "&")
	return replacer.Replace(value)
}

// execStartFromUnit 从 systemd 单元文本里取 ExecStart 的值。
func execStartFromUnit(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "ExecStart="); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// commandFromTaskXML 从 schtasks /XML 输出里拼出命令行。
//
// 计划任务把命令与参数分成两个元素，拼起来才是「它到底在跑什么」。
func commandFromTaskXML(text string) string {
	command := firstElementText(text, "Command")
	if command == "" {
		return ""
	}
	if args := firstElementText(text, "Arguments"); args != "" {
		return command + " " + args
	}
	return command
}

// firstElementText 取某个 XML 元素的文本内容。
func firstElementText(text, element string) string {
	open := "<" + element + ">"
	closing := "</" + element + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	rest := text[start+len(open):]
	end := strings.Index(rest, closing)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
