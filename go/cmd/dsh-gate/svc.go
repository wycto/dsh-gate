// `dsh-gate svc ...`：管理内网服务登记（与管理台「服务」分页能力对等，P0 部分）。
//
// 与 dsh 子命令的分工：dsh 管「门户拉起的实例」，svc 管「已经存在、只被登记
// 进来的上游」。服务没有进程可启停，所以动作只有登记册的增删与启停挂载、探活。
//
// 全部动作走管理端的本机控制 API（回环 + portal.token，见 localclient.go 的
// localEndpoint）：探活必须由管理端进程去做（它才有探活缓存与上游网络位置），
// 登记类动作也统一走 API，让审计与缓存失效只有一份实现。管理端没在运行时
// 明确提示先启动，不悄悄改库。
//
// 子命令名刻意用 svc 而不是 service：service 已被「系统服务安装」占用。
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// svcCommand 实现 `dsh-gate svc`。
func svcCommand(args []string) int {
	if len(args) == 0 {
		printSvcHelp(os.Stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return svcList(args[1:])
	case "add":
		return svcAdd(args[1:])
	case "rm":
		return svcRefAction("svc.remove", "删除", args[1:])
	case "enable":
		return svcRefAction("svc.enable", "启用", args[1:])
	case "disable":
		return svcRefAction("svc.disable", "停用", args[1:])
	case "probe":
		return svcProbe(args[1:])
	case "-h", "--help", "help":
		printSvcHelp(os.Stdout)
		return 0
	default:
		return usageError("未知的 svc 子命令 %q，可用：list / add / rm / enable / disable / probe", args[0])
	}
}

// printSvcHelp 打印 svc 子命令帮助。
func printSvcHelp(w *os.File) {
	fmt.Fprint(w, `用法: dsh-gate svc <子命令> [参数]

管理内网服务登记：把任意内网 IP:端口 / 内网域名服务登记进管理端，经
/s/<id>/ 子路径代理出去（需登录且被授权）。服务进程不归门户管，这里只
管登记册。全部动作要求管理端正在运行（走本机控制 API）。

  dsh-gate svc list                          列出全部登记的服务
  dsh-gate svc add --name <名字> --upstream <地址>
        [--host-header upstream|gateway] [--insecure-tls] [--note <备注>]
                                             登记服务；地址形如
                                             http://192.168.1.20:8080 或
                                             https://wiki.corp.example/base
  dsh-gate svc rm <名字或 id>                删除登记（不影响服务本身）
  dsh-gate svc enable|disable <名字或 id>    启用 / 停用挂载
  dsh-gate svc probe <名字或 id>             探活：真打一次上游（2s 超时）

公共选项: --state-dir <目录>  --db <路径>  --lang zh-CN|en
`)
}

// ---- 本机 API 的请求/响应形状 ----
//
// 刻意不复用 localclient.go 的 localRequest/localResponse：那两个结构服务于
// 实例动作；服务动作的字段在这里独立定义，JSON 键与服务端 localapi.go 对齐。

// svcLocalRequest 是 svc.* 动作的请求体。
type svcLocalRequest struct {
	Action      string `json:"action"`
	Service     string `json:"service,omitempty"`
	Upstream    string `json:"upstream,omitempty"`
	HostHeader  string `json:"hostHeader,omitempty"`
	Note        string `json:"note,omitempty"`
	InsecureTLS bool   `json:"insecureTLS,omitempty"`
}

// svcLocalService 是响应里的一条服务。
type svcLocalService struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	Note     string `json:"note"`
}

// svcProbeInfo 是探活结果。服务端 ServiceProbe 不带 JSON 标签，
// 编码出来就是字段名原样（首字母大写），这里照单全收。
type svcProbeInfo struct {
	Reachable bool   `json:"Reachable"`
	Status    int    `json:"Status"`
	LatencyMS int64  `json:"LatencyMS"`
	Error     string `json:"Error"`
}

// svcLocalResponse 是 svc.* 动作的响应体。
type svcLocalResponse struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error"`
	Services []svcLocalService `json:"services"`
	Probe    *svcProbeInfo     `json:"probe"`
}

// svcPost 发一个 svc.* 本机控制请求。地址发现与令牌复用 localEndpoint
// （portal.token + 运行登记），TLS 取舍与 localPost 一致：只与本机自己的
// 管理端通信，自签证书跳过校验。
func svcPost(stateDir string, req svcLocalRequest) (svcLocalResponse, error) {
	var out svcLocalResponse
	port, token, scheme, err := localEndpoint(stateDir)
	if err != nil {
		return out, err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	transport := &http.Transport{}
	if scheme == "https" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- 只连本机管理端
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	url := scheme + "://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + localAPIPath
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, fmt.Errorf("连不上管理端本机接口（%s）：%w", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("管理端返回了非 JSON 响应（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !out.OK {
		if out.Error == "" {
			out.Error = fmt.Sprintf("管理端返回 HTTP %d", resp.StatusCode)
		}
		return out, fmt.Errorf("%s", out.Error)
	}
	return out, nil
}

// ---- 各子命令 ----

// svcList 列出全部登记的服务。
func svcList(args []string) int {
	fs := flag.NewFlagSet("svc list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() > 0 {
		return usageError("svc list 不接受位置参数")
	}
	resp, err := svcPost(stateDir, svcLocalRequest{Action: "svc.list"})
	if err != nil {
		return fatal("%v", err)
	}
	if len(resp.Services) == 0 {
		fmt.Println("还没有登记的服务：dsh-gate svc add --name <名字> --upstream <地址>")
		return 0
	}
	fmt.Printf("共 %d 个服务：\n", len(resp.Services))
	for _, svc := range resp.Services {
		state := "启用"
		if !svc.Enabled {
			state = "停用"
		}
		fmt.Printf("  %-20s %-12s %-4s %s\n", svc.Name, svc.ID, state, svc.Upstream)
		fmt.Printf("    挂载 %s\n", svc.URL)
		if strings.TrimSpace(svc.Note) != "" {
			fmt.Printf("    备注 %s\n", svc.Note)
		}
	}
	return 0
}

// svcAdd 登记一条服务。
func svcAdd(args []string) int {
	fs := flag.NewFlagSet("svc add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", "", "服务展示名（必填）")
	upstream := fs.String("upstream", "", "上游地址（必填），http(s)://host[:port][/base]")
	hostHeader := fs.String("host-header", "", "Host 头策略：upstream（默认）/ gateway")
	insecure := fs.Bool("insecure-tls", false, "跳过上游 TLS 证书校验（自签内网服务）")
	note := fs.String("note", "", "备注")
	_, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() > 0 {
		return usageError("svc add 不接受位置参数，请用 --name 与 --upstream")
	}
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*upstream) == "" {
		return usageError("svc add 需要 --name <名字> 与 --upstream <地址>")
	}
	resp, err := svcPost(stateDir, svcLocalRequest{
		Action: "svc.add", Service: *name, Upstream: *upstream,
		HostHeader: *hostHeader, InsecureTLS: *insecure, Note: *note,
	})
	if err != nil {
		return fatal("%v", err)
	}
	if len(resp.Services) == 1 {
		svc := resp.Services[0]
		fmt.Printf("服务 %q 已登记：id=%s，挂载 %s\n", svc.Name, svc.ID, svc.URL)
		fmt.Println("默认仅管理员可见；给其他用户授权请在管理台「服务」分页操作。")
	} else {
		fmt.Printf("服务 %q 已登记。\n", *name)
	}
	return 0
}

// svcRefAction 执行收一个位置参数（名或 id）的动作：rm / enable / disable。
func svcRefAction(action, label string, args []string) int {
	fs := flag.NewFlagSet("svc "+strings.TrimPrefix(action, "svc."), flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageError("svc %s 需要一个服务名或 id", strings.TrimPrefix(action, "svc."))
	}
	if _, err := svcPost(stateDir, svcLocalRequest{Action: action, Service: fs.Arg(0)}); err != nil {
		return fatal("%v", err)
	}
	fmt.Printf("服务 %q 已%s。\n", fs.Arg(0), label)
	return 0
}

// svcProbe 探活：真打一次上游，打印结果。
func svcProbe(args []string) int {
	fs := flag.NewFlagSet("svc probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_, stateDir, err := parsePortalFlagsFull(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageError("svc probe 需要一个服务名或 id")
	}
	resp, err := svcPost(stateDir, svcLocalRequest{Action: "svc.probe", Service: fs.Arg(0)})
	if err != nil {
		return fatal("%v", err)
	}
	if resp.Probe == nil {
		return fatal("管理端没有返回探活结果")
	}
	p := resp.Probe
	if p.Reachable {
		fmt.Printf("服务 %q 可达：HTTP %d，耗时 %dms\n", fs.Arg(0), p.Status, p.LatencyMS)
		return 0
	}
	fmt.Printf("服务 %q 不可达", fs.Arg(0))
	if p.Status > 0 {
		fmt.Printf("：HTTP %d", p.Status)
	}
	if p.Error != "" {
		fmt.Printf("：%s", p.Error)
	}
	fmt.Println()
	return 1
}
