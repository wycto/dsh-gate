// Package fmtx 提供按**显示宽度**对齐文本的小工具。
//
// 为什么要自己写：Go 的 fmt 用 %-12s 这类宽度是按字符数（或 rune 数）算的，
// 而一个汉字在终端里占两列。于是「账号」「dsh 端口」「HTTPS」混排时必然对不齐
// ——而这恰恰是终端里给人看的输出最需要的那点讲究。
package fmtx

// wideRanges 是占两列的字符区间（中日韩文字、全角标点、常用符号）。
//
// 取的是各终端实际的「东亚宽度 = Wide/Fullwidth」区间，不追求覆盖所有生僻字：
// 输出里出现表外字符时顶多对不齐一格，不会出错。
var wideRanges = [][2]rune{
	{0x1100, 0x115F},   // 谚文字母
	{0x2E80, 0x303E},   // CJK 部首、标点
	{0x3041, 0x33FF},   // 假名、注音、CJK 兼容
	{0x3400, 0x4DBF},   // CJK 扩展 A
	{0x4E00, 0x9FFF},   // CJK 统一表意文字
	{0xA000, 0xA4CF},   // 彝文
	{0xAC00, 0xD7A3},   // 谚文音节
	{0xF900, 0xFAFF},   // CJK 兼容表意
	{0xFE10, 0xFE19},   // 竖排标点
	{0xFE30, 0xFE6F},   // CJK 兼容形式
	{0xFF00, 0xFF60},   // 全角 ASCII
	{0xFFE0, 0xFFE6},   // 全角货币
	{0x1F300, 0x1F64F}, // 常用 emoji
	{0x20000, 0x3FFFD}, // CJK 扩展 B 及以后
}

// RuneWidth 返回单个字符占用的列数。
func RuneWidth(r rune) int {
	for _, span := range wideRanges {
		if r >= span[0] && r <= span[1] {
			return 2
		}
	}
	// 控制字符不占位。
	if r < 0x20 || r == 0x7f {
		return 0
	}
	return 1
}

// Width 返回字符串的显示宽度。
func Width(s string) int {
	total := 0
	for _, r := range s {
		total += RuneWidth(r)
	}
	return total
}

// Pad 在右侧补空格，把字符串补到给定显示宽度。
//
// 已经超过目标宽度时原样返回——与其把长文本截断（丢掉信息），不如让它顶开
// 整列，这在一个「标签 + 值」的列表里更容易接受。
func Pad(s string, width int) string {
	if gap := width - Width(s); gap > 0 {
		return s + spaces(gap)
	}
	return s
}

// PadLeft 在左侧补空格（右对齐）。
func PadLeft(s string, width int) string {
	if gap := width - Width(s); gap > 0 {
		return spaces(gap) + s
	}
	return s
}

func spaces(n int) string {
	const blanks = "                                "
	for n > len(blanks) {
		n -= len(blanks)
	}
	if n <= 0 {
		return ""
	}
	return blanks[:n]
}
