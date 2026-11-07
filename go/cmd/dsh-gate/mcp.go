// dsh-gate mcp：把 dsh-gate 的对外 HTTP API 包成 MCP（Model Context Protocol）工具，
// 供支持 MCP 的客户端/AI 直接发现并调用（docs/service-api.md 第 8 节）。
//
// 形态：stdio MCP server，stdin 每行一条 JSON-RPC 2.0 请求，stdout 每行一条 JSON 响应。
// 它本身是**网关 HTTP API 的客户端**——不在网关进程内跑，只需 base-url + API key，
// 因此可放在任意机器（甚至 AI 所在机器）上，与网关解耦。
//
// 零新增依赖：JSON-RPC 用标准库 encoding/json 手搓。
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/version"
)

// ---- 命令行入口 ----

// mcpCommand 实现 dsh-gate mcp。
func mcpCommand(args []string) int {
	fs := flag.NewFlagSet(`mcp`, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	base := fs.String(`base-url`, os.Getenv(`DSH_GATE_API_URL`),
		`网关地址，如 https://192.168.1.20:12020（或环境变量 DSH_GATE_API_URL）`)
	key := fs.String(`api-key`, os.Getenv(`DSH_GATE_API_KEY`),
		`API 密钥 dgk_…（或环境变量 DSH_GATE_API_KEY；在管理台「API」分页创建）`)
	insecure := fs.Bool(`insecure`, false, `跳过 TLS 证书校验（自签证书时用）`)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	cfg := mcpConfig{
		BaseURL:  strings.TrimRight(strings.TrimSpace(*base), `/`),
		APIKey:   strings.TrimSpace(*key),
		Insecure: *insecure,
	}
	if cfg.BaseURL == `` || cfg.APIKey == `` {
		fmt.Fprintln(os.Stderr, `dsh-gate mcp: 需要 --base-url 与 --api-key（或环境变量 DSH_GATE_API_URL / DSH_GATE_API_KEY）`)
		return 2
	}
	if err := runMCP(os.Stdin, os.Stdout, cfg); err != nil {
		fmt.Fprintf(os.Stderr, `dsh-gate mcp: %v
`, err)
		return 1
	}
	return 0
}

// mcpConfig 是 MCP server 连接网关所需的配置。
type mcpConfig struct {
	BaseURL  string
	APIKey   string
	Insecure bool
}

// ---- JSON-RPC 2.0 基本类型 ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpProtocolVersion 是实现的 MCP 协议版本。
const mcpProtocolVersion = `2024-11-05`

// runMCP 是 MCP server 主循环：逐行读 JSON-RPC、逐行写响应。抽成函数便于测试。
func runMCP(in io.Reader, out io.Writer, cfg mcpConfig) error {
	cl := newMCPClient(cfg)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: `2.0`, Error: &rpcError{Code: -32700, Message: `解析失败: ` + err.Error()}})
			continue
		}
		// 通知（无 id）不出响应：notifications/initialized 等一律忽略。
		if len(req.ID) == 0 || string(req.ID) == `null` {
			continue
		}
		result, rerr := handleMCPMethod(cl, req)
		resp := rpcResponse{JSONRPC: `2.0`, ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// handleMCPMethod 分发一个 RPC 方法。
func handleMCPMethod(cl *mcpClient, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case `initialize`:
		return map[string]any{
			`protocolVersion`: mcpProtocolVersion,
			`capabilities`:    map[string]any{`tools`: map[string]any{}},
			`serverInfo`:      map[string]any{`name`: `dsh-gate`, `version`: version.Version},
		}, nil
	case `ping`:
		return map[string]any{}, nil
	case `tools/list`:
		return map[string]any{`tools`: mcpTools()}, nil
	case `tools/call`:
		return handleToolCall(cl, req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: `未知方法: ` + req.Method}
	}
}

// handleToolCall 执行一次工具调用。工具级错误放在 result.isError，而非 JSON-RPC error。
func handleToolCall(cl *mcpClient, rawParams json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(rawParams) > 0 {
		if err := json.Unmarshal(rawParams, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: `参数不合法: ` + err.Error()}
		}
	}
	if strings.TrimSpace(p.Name) == `` {
		return nil, &rpcError{Code: -32602, Message: `缺少工具名`}
	}
	args := map[string]any{}
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}
	status, text, err := callTool(cl, p.Name, args)
	if err != nil {
		return toolContent(`调用失败: `+err.Error(), true), nil
	}
	return toolContent(text, status < 200 || status >= 300), nil
}

// toolContent 组装 MCP tools/call 的 result。
func toolContent(text string, isError bool) map[string]any {
	return map[string]any{
		`content`: []map[string]any{{`type`: `text`, `text`: text}},
		`isError`: isError,
	}
}

// ---- 网关 HTTP 客户端 ----

type mcpClient struct {
	base string
	key  string
	http *http.Client
}

func newMCPClient(cfg mcpConfig) *mcpClient {
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.Insecure} // #nosec G402 -- 显式 --insecure
	transport := &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}
	return &mcpClient{
		base: cfg.BaseURL,
		key:  cfg.APIKey,
		http: &http.Client{Transport: transport, Timeout: 60 * time.Second},
	}
}

// do 发一次请求，返回状态码与（美化后的）响应正文。
func (c *mcpClient) do(method, path string, query url.Values, body any) (int, string, error) {
	u := c.base + path
	if len(query) > 0 {
		u += `?` + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, ``, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		return 0, ``, err
	}
	req.Header.Set(`Authorization`, `Bearer `+c.key)
	req.Header.Set(`Accept`, `application/json`)
	if body != nil {
		req.Header.Set(`Content-Type`, `application/json`)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, ``, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, prettyJSON(data), nil
}

// prettyJSON 尽量缩进美化；不是 JSON 就原样返回。
func prettyJSON(data []byte) string {
	if len(bytes.TrimSpace(data)) == 0 {
		return ``
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	out, err := json.MarshalIndent(v, ``, `  `)
	if err != nil {
		return string(data)
	}
	return string(out)
}

// ---- 工具定义与分派 ----

// argStr 取字符串参数（不存在或非字符串返回空串）。
func argStr(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ``
}

// argInt 取整数参数（缺省用 def）。
func argInt(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
	}
	return def
}

// serviceBody 把注册/修改参数映射成网关请求体（只带出现过的键）。
func serviceBody(args map[string]any) map[string]any {
	b := map[string]any{}
	strKeys := []string{`name`, `upstream`, `hostHeader`, `note`, `startCmd`, `stopCmd`, `workDir`, `env`, `healthPath`}
	for _, k := range strKeys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				b[k] = s
			}
		}
	}
	for _, k := range []string{`insecureTLS`, `managed`, `autoStart`} {
		if v, ok := args[k]; ok {
			if bv, ok := v.(bool); ok {
				b[k] = bv
			}
		}
	}
	return b
}

// callTool 执行一个工具，返回 (HTTP 状态码, 响应正文, 错误)。
func callTool(cl *mcpClient, name string, args map[string]any) (int, string, error) {
	id := strings.TrimSpace(argStr(args, `id`))
	needID := func() (int, string, error) {
		if id == `` {
			return 0, ``, fmt.Errorf(`缺少 id 参数`)
		}
		return 0, ``, nil
	}
	switch name {
	case `service_list`:
		return cl.do(http.MethodGet, `/api/v1/services`, nil, nil)
	case `service_get`:
		if _, _, err := needID(); err != nil {
			return 0, ``, err
		}
		return cl.do(http.MethodGet, `/api/v1/services/`+url.PathEscape(id), nil, nil)
	case `service_register`:
		return cl.do(http.MethodPost, `/api/v1/services`, nil, serviceBody(args))
	case `service_update`:
		if _, _, err := needID(); err != nil {
			return 0, ``, err
		}
		return cl.do(http.MethodPatch, `/api/v1/services/`+url.PathEscape(id), nil, serviceBody(args))
	case `service_remove`:
		if _, _, err := needID(); err != nil {
			return 0, ``, err
		}
		return cl.do(http.MethodDelete, `/api/v1/services/`+url.PathEscape(id), nil, nil)
	case `service_start`, `service_stop`, `service_restart`:
		if _, _, err := needID(); err != nil {
			return 0, ``, err
		}
		return cl.do(http.MethodPost, `/api/v1/services/`+url.PathEscape(id)+`/`+strings.TrimPrefix(name, `service_`), nil, nil)
	case `service_status`:
		if _, _, err := needID(); err != nil {
			return 0, ``, err
		}
		return cl.do(http.MethodGet, `/api/v1/services/`+url.PathEscape(id)+`/status`, nil, nil)
	case `service_logs`:
		if _, _, err := needID(); err != nil {
			return 0, ``, err
		}
		q := url.Values{}
		q.Set(`tail`, strconv.Itoa(argInt(args, `tail`, 200)))
		return cl.do(http.MethodGet, `/api/v1/services/`+url.PathEscape(id)+`/logs`, q, nil)
	case `instance_list`:
		return cl.do(http.MethodGet, `/api/v1/instances`, nil, nil)
	case `instance_start`, `instance_stop`, `instance_restart`:
		if _, _, err := needID(); err != nil {
			return 0, ``, err
		}
		return cl.do(http.MethodPost, `/api/v1/instances/`+url.PathEscape(id)+`/`+strings.TrimPrefix(name, `instance_`), nil, nil)
	default:
		return 0, ``, fmt.Errorf(`未知工具: %s`, name)
	}
}

// mcpTools 返回全部工具的清单（含 JSON Schema）。
func mcpTools() []map[string]any {
	idProp := map[string]any{`id`: map[string]any{`type`: `string`, `description`: `服务或实例的 id 或名称`}}
	idSchema := map[string]any{`type`: `object`, `properties`: idProp, `required`: []string{`id`}}
	svcProps := map[string]any{
		`name`:        map[string]any{`type`: `string`, `description`: `服务名`},
		`upstream`:    map[string]any{`type`: `string`, `description`: `上游地址 http(s)://host[:port][/base]`},
		`hostHeader`:  map[string]any{`type`: `string`, `enum`: []string{`upstream`, `gateway`}},
		`insecureTLS`: map[string]any{`type`: `boolean`, `description`: `跳过上游 TLS 证书校验`},
		`note`:        map[string]any{`type`: `string`},
		`managed`:     map[string]any{`type`: `boolean`, `description`: `是否由网关托管进程`},
		`startCmd`:    map[string]any{`type`: `string`, `description`: `受管服务的启动命令`},
		`stopCmd`:     map[string]any{`type`: `string`},
		`workDir`:     map[string]any{`type`: `string`},
		`env`:         map[string]any{`type`: `string`, `description`: `环境变量（每行 KEY=VALUE）`},
		`autoStart`:   map[string]any{`type`: `boolean`},
		`healthPath`:  map[string]any{`type`: `string`},
	}
	svcSchema := map[string]any{`type`: `object`, `properties`: svcProps, `required`: []string{`name`, `upstream`}}
	updateProps := map[string]any{}
	for k, v := range svcProps {
		updateProps[k] = v
	}
	updateProps[`id`] = idProp[`id`]
	updateSchema := map[string]any{`type`: `object`, `properties`: updateProps, `required`: []string{`id`}}
	logsProps := map[string]any{
		`id`:   idProp[`id`],
		`tail`: map[string]any{`type`: `integer`, `description`: `读取最近多少行（默认 200）`},
	}
	logsSchema := map[string]any{`type`: `object`, `properties`: logsProps, `required`: []string{`id`}}

	t := func(name, desc string, schema map[string]any) map[string]any {
		return map[string]any{`name`: name, `description`: desc, `inputSchema`: schema}
	}
	return []map[string]any{
		t(`service_list`, `列出已登记的内网服务`, map[string]any{`type`: `object`, `properties`: map[string]any{}}),
		t(`service_get`, `查看单个服务详情`, idSchema),
		t(`service_register`, `登记一个内网服务（可带启动命令成为受管服务）`, svcSchema),
		t(`service_update`, `修改服务配置`, updateSchema),
		t(`service_remove`, `删除服务登记`, idSchema),
		t(`service_start`, `启动受管服务`, idSchema),
		t(`service_stop`, `停止受管服务`, idSchema),
		t(`service_restart`, `重启受管服务`, idSchema),
		t(`service_status`, `查询服务运行状态`, idSchema),
		t(`service_logs`, `读取服务最近日志`, logsSchema),
		t(`instance_list`, `列出 dsh 实例及状态`, map[string]any{`type`: `object`, `properties`: map[string]any{}}),
		t(`instance_start`, `启动实例`, idSchema),
		t(`instance_stop`, `停止实例`, idSchema),
		t(`instance_restart`, `重启实例`, idSchema),
	}
}
