package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMaybeInjectTitle(t *testing.T) {
	h := &Handler{opts: Options{TitlePrefix: func() string { return "office-gw" }}}
	body := "<html><head><title>DSH</title></head><body>x</body></html>"
	res := &http.Response{
		Request:       &http.Request{Method: http.MethodGet},
		StatusCode:    200,
		Header:        http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	h.maybeInjectTitle(res)
	out, _ := io.ReadAll(res.Body)
	if !bytes.Contains(out, []byte("data-dsh-gate-title")) {
		t.Fatalf("应注入标题脚本，实际: %s", out)
	}
	if !bytes.Contains(out, []byte("</head>")) {
		t.Error("应保留 </head>")
	}
	if res.Header.Get("Content-Encoding") != "" {
		t.Error("不应留下 Content-Encoding")
	}

	// 非 html 不注入
	res2 := &http.Response{
		Request:    &http.Request{Method: http.MethodGet},
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader("{}")),
		StatusCode: 200,
	}
	h.maybeInjectTitle(res2)
	o2, _ := io.ReadAll(res2.Body)
	if bytes.Contains(o2, []byte("data-dsh-gate-title")) {
		t.Error("非 html 不应注入")
	}

	// 空前缀不注入
	h3 := &Handler{opts: Options{TitlePrefix: func() string { return "" }}}
	res3 := &http.Response{Request: &http.Request{Method: http.MethodGet}, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader("<html><head></head></html>")), StatusCode: 200}
	h3.maybeInjectTitle(res3)
	o3, _ := io.ReadAll(res3.Body)
	if bytes.Contains(o3, []byte("data-dsh-gate-title")) {
		t.Error("空前缀不应注入")
	}
}
