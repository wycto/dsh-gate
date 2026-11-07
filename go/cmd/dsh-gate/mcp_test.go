package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGateway 起一个冒充 dsh-gate 网关的 httptest 服务，记录最近一次请求的路径与鉴权头。
func fakeGateway(t *testing.T) (*httptest.Server, *string, *string) {
	t.Helper()
	var lastPath, lastAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastAuth = r.Header.Get(`Authorization`)
		w.Header().Set(`Content-Type`, `application/json`)
		if r.Method == http.MethodGet && r.URL.Path == `/api/v1/services` {
			io.WriteString(w, `{"ok":true,"services":[{"id":"abc","name":"demo","upstream":"http://127.0.0.1:8080"}]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, `/boom`) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"ok":false,"error":{"code":"internal","message":"boom"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &lastPath, &lastAuth
}

// mcpCall 跑一次 runMCP，返回按行解析后的响应（忽略通知）。
func mcpCall(t *testing.T, cfg mcpConfig, requests ...string) []map[string]any {
	t.Helper()
	input := strings.Join(requests, `
`) + `
`
	var out bytes.Buffer
	if err := runMCP(strings.NewReader(input), &out, cfg); err != nil {
		t.Fatalf(`runMCP 失败: %v`, err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), `
`)
	resps := make([]map[string]any, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == `` {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf(`响应不是合法 JSON: %s (%v)`, ln, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func TestMCPInitializeToolsAndCall(t *testing.T) {
	ts, lastPath, lastAuth := fakeGateway(t)
	cfg := mcpConfig{BaseURL: ts.URL, APIKey: `dgk_test`}

	resps := mcpCall(t, cfg,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"service_list","arguments":{}}}`,
	)
	// 通知不出响应 → 3 条。
	if len(resps) != 3 {
		t.Fatalf(`期望 3 条响应（通知被忽略），实际 %d`, len(resps))
	}
	// initialize
	initRes, _ := resps[0][`result`].(map[string]any)
	if initRes[`protocolVersion`] != `2024-11-05` {
		t.Errorf(`initialize protocolVersion 不对: %v`, initRes[`protocolVersion`])
	}
	if si, _ := initRes[`serverInfo`].(map[string]any); si[`name`] != `dsh-gate` {
		t.Errorf(`serverInfo.name 不对: %v`, initRes[`serverInfo`])
	}
	// tools/list
	listRes, _ := resps[1][`result`].(map[string]any)
	tools, _ := listRes[`tools`].([]any)
	wantTools := []string{`service_list`, `service_get`, `service_register`, `service_update`,
		`service_remove`, `service_start`, `service_stop`, `service_restart`,
		`service_status`, `service_logs`, `instance_list`, `instance_start`,
		`instance_stop`, `instance_restart`}
	if len(tools) != len(wantTools) {
		t.Fatalf(`工具数应为 %d，实际 %d`, len(wantTools), len(tools))
	}
	names := map[string]bool{}
	for _, tv := range tools {
		if tm, ok := tv.(map[string]any); ok {
			if n, ok := tm[`name`].(string); ok {
				names[n] = true
			}
			if _, ok := tm[`inputSchema`]; !ok {
				t.Errorf(`工具 %v 缺 inputSchema`, tm[`name`])
			}
		}
	}
	for _, w := range wantTools {
		if !names[w] {
			t.Errorf(`tools/list 缺工具 %q`, w)
		}
	}
	// tools/call service_list
	callRes, _ := resps[2][`result`].(map[string]any)
	content, _ := callRes[`content`].([]any)
	if len(content) == 0 {
		t.Fatalf(`service_list 没有 content`)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first[`text`].(string)
	if !strings.Contains(text, `demo`) {
		t.Errorf(`service_list 结果应含 demo，实际: %s`, text)
	}
	if isErr, _ := callRes[`isError`].(bool); isErr {
		t.Errorf(`service_list 不应 isError`)
	}
	if *lastPath != `/api/v1/services` {
		t.Errorf(`应打到 /api/v1/services，实际 %s`, *lastPath)
	}
	if *lastAuth != `Bearer dgk_test` {
		t.Errorf(`应带 Bearer 鉴权，实际 %q`, *lastAuth)
	}
}

func TestMCPToolErrorIsError(t *testing.T) {
	ts, _, _ := fakeGateway(t)
	cfg := mcpConfig{BaseURL: ts.URL, APIKey: `dgk_test`}
	resps := mcpCall(t, cfg,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"service_get","arguments":{"id":"boom"}}}`,
	)
	if len(resps) != 1 {
		t.Fatalf(`期望 1 条响应`)
	}
	callRes, _ := resps[0][`result`].(map[string]any)
	if isErr, _ := callRes[`isError`].(bool); !isErr {
		t.Errorf(`非 2xx 应 isError=true`)
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	ts, _, _ := fakeGateway(t)
	cfg := mcpConfig{BaseURL: ts.URL, APIKey: `dgk_test`}
	resps := mcpCall(t, cfg, `{"jsonrpc":"2.0","id":9,"method":"nope"}`)
	if len(resps) != 1 {
		t.Fatalf(`期望 1 条响应`)
	}
	if _, ok := resps[0][`error`]; !ok {
		t.Errorf(`未知方法应回 JSON-RPC error`)
	}
}

func TestMCPCommandRequiresConfig(t *testing.T) {
	// 无 --base-url 与 --api-key（也清空环境变量）时应返回 2。
	t.Setenv(`DSH_GATE_API_URL`, ``)
	t.Setenv(`DSH_GATE_API_KEY`, ``)
	if code := mcpCommand(nil); code != 2 {
		t.Errorf(`缺少配置应返回 2，实际 %d`, code)
	}
}
