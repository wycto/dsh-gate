package proxy

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestShortBodyForPageDecodesGzip 页面细节不能显示压缩字节。
//
// 实测：浏览器发 Accept-Encoding: gzip，网关透传给 dsh，而 Go 只在编码头是自己
// 加的时候才自动解压，于是 401 的响应体是原始 gzip 字节，页面里出现一片
// `? K)?P(OMRH,-` 这样的乱码（与响应体字节一一对应）。用户看到的是天文数字。
func TestShortBodyForPageDecodesGzip(t *testing.T) {
	const message = "dsh web authentication required; reopen the URL printed by dsh web."
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(message)); err != nil {
		t.Fatalf("压不出测试数据: %v", err)
	}
	writer.Close()

	got := shortBodyForPage(buffer.String())
	if got != message {
		t.Fatalf("gzip 响应体应解压后显示\n实际: %q\n期望: %q", got, message)
	}
}

// TestShortBodyForPageHidesBinary 解不出来的二进制干脆不显示：页面上那句说明
// 已经把情况讲清楚了，再贴一段乱码只是噪音。
func TestShortBodyForPageHidesBinary(t *testing.T) {
	binary := string([]byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0x03, 0x04, 0x05, 0x06, 0x07})
	if got := shortBodyForPage(binary); got != "" {
		t.Errorf("二进制内容不该显示，实际 %q", got)
	}
	// 半截 gzip（解压必然失败）也不该原样显示。
	if got := shortBodyForPage("\x1f\x8b\x08\x00garbage-not-really-gzip"); got != "" {
		t.Errorf("坏 gzip 不该原样显示，实际 %q", got)
	}
}

// TestShortBodyForPageKeepsPlainText 正常文本要保留，并把空白压成一行。
func TestShortBodyForPageKeepsPlainText(t *testing.T) {
	got := shortBodyForPage("line one\n\n  line two\t")
	if got != "line one line two" {
		t.Errorf("实际 %q", got)
	}
	if got := shortBodyForPage(""); got != "" {
		t.Errorf("空串应返回空串，实际 %q", got)
	}
}

// TestShortBodyForPageTruncatesAtRuneBoundary 截断不能切在半个汉字上。
//
// 以前按字节截断：中文一个字三字节，从中间切开就是无效 UTF-8，页面上多一个「�」。
func TestShortBodyForPageTruncatesAtRuneBoundary(t *testing.T) {
	long := strings.Repeat("中文报错信息", 40) // 240 个汉字，远超 160 的限制
	got := shortBodyForPage(long)
	if !utf8.ValidString(got) {
		t.Fatalf("截断后不是合法 UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断应带省略号，实际 %q", got)
	}
	if runes := []rune(got); len(runes) != 161 { // 160 个字符 + 省略号
		t.Errorf("截断长度 = %d 个字符，期望 161", len(runes))
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("出现了替换字符，说明切在了半个字符上: %q", got)
	}
}
