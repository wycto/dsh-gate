package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wycto/dsh-gate/internal/registry"
)

// controlAddr 算出访问控制端点的地址。
//
// 管理端绑定 0.0.0.0 时要显式换成回环地址：控制端点只接受回环来源，按通配地址
// 连是连不上的。
func controlAddr(record registry.Record) string {
	host, _, err := net.SplitHostPort(record.Listen)
	if err != nil || host == "" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(record.Port))
}

// listening 报告地址上是否有人在监听。
func listening(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// stopRecord 通过控制端点停掉管理端，返回是否真的停掉了。
//
// 走控制端点而不是「PID 文件 + 发信号」：信号要在每个平台各写一份，而 Windows
// 上根本没有 SIGTERM，只能退化成强杀，管理端来不及收尾。走控制端点则三个平台
// 行为一致，被停的进程也知道自己是被请求停的，会按和 Ctrl-C 一样的流程收尾。
func stopRecord(record registry.Record, wait time.Duration) error {
	if !record.Alive() {
		return fmt.Errorf("端口 %d 没有在监听，管理端应该已经停了", record.Port)
	}

	addr := controlAddr(record)
	client := newHTTPClient(3 * time.Second)
	response, err := client.Post("http://"+addr+"/__dsh_gate/shutdown", "text/plain", nil)
	if err != nil {
		return fmt.Errorf("停机请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("管理端拒绝了停机请求（HTTP %d）", response.StatusCode)
	}

	// 端口关掉才算真的停了；只收到 200 响应是不够的。
	deadline := time.Now().Add(wait)
	for listening(addr, 300*time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("已收到停机请求，但 %s 后端口仍在监听，可能卡住了", wait)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// stopPortal 停掉管理端。quiet 为真且没在运行时不出声（restart 用）。
func stopPortal(runDir registry.Dir, wait time.Duration, quiet bool) int {
	record, ok, err := runDir.Load(portalRecordName)
	if err != nil {
		return fatal("%v", err)
	}
	if !ok {
		if !quiet {
			fmt.Println("管理端没有在运行。")
		}
		return 0
	}
	if !record.Alive() {
		// 残留登记（被强杀、断电）清掉即可：把「本来就停了」报成失败，会让
		// 脚本在崩溃之后莫名其妙地返回非零——而那正是最需要它顺利跑完的时候。
		_ = runDir.Remove(portalRecordName)
		if !quiet {
			fmt.Println("管理端本来就没有在运行，已清理残留登记。")
		}
		return 0
	}
	if err := stopRecord(record, wait); err != nil {
		fmt.Fprintf(os.Stderr, "管理端停止失败：%v\n", err)
		return 1
	}
	_ = runDir.Remove(portalRecordName)
	if !quiet {
		fmt.Printf("管理端已停止（%s）。\n", record.Listen)
	}
	return 0
}

// stopCommand 实现 `dsh-gate stop`：停掉管理端。
func stopCommand(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate stop [选项]

停止整个管理端（连同它监督着的 dsh 实例一起收尾）。没有在运行时什么都不做。
装了系统服务时，stop/start/restart 都会转给服务管理器，避免被它自动拉起。

  dsh-gate stop
  dsh-gate stop --wait 60s     多等一会儿它收尾

选项:
`)
		fs.PrintDefaults()
	}
	runDir := registerRunDirFlag(fs)
	wait := fs.Duration("wait", 20*time.Second, "等待端口关闭的最长时间")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		return usageError("stop 不接受位置参数（实例请用 dsh-gate dsh stop <名字>）")
	}
	if serviceUnitInstalled() {
		if err := serviceStop(); err != nil {
			return fatal("停止系统服务失败：%v", err)
		}
		fmt.Println("管理端由系统服务托管，已停止；用 dsh-gate start 再启动。")
		return 0
	}
	return stopPortal(runDir(), *wait, false)
}

// newHTTPClient 造一个不跟随跳转的短超时客户端。
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// logDir 返回管理端日志目录。
func logDir(runDir registry.Dir) string {
	return filepath.Join(string(runDir.Expand()), "logs")
}
