package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/registry"
)

// stopCommand 实现 `dsh-gate stop`：停掉本机正在运行的网关实例。
//
// 走的是网关自己的控制端点（只接受回环来源），因此三个平台行为一致，且被停的
// 实例会按和 Ctrl-C 一样的流程收尾：先断新请求，再让托管的 dsh 退出。
func stopCommand(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate stop [实例名...] [选项]

停掉本机正在运行的网关实例（连同它托管的 dsh）。

必须写明停哪些：写实例名停那几个，写 all 才是全部。什么都不写不会执行任何操作，
只会列出当前在跑的实例供你挑选——「手一抖全停掉」这种事，不该由默认值来决定。

  dsh-gate stop personal        停掉 personal
  dsh-gate stop personal work   停掉这两个
  dsh-gate stop all             停掉全部

选项:
`)
	}

	runDir := registerRunDirFlag(fs)
	wait := fs.Duration("wait", 20*time.Second, "等待端口关闭的最长时间")
	// parseFlexible：允许「实例名在前、选项在后」的写法（见 flags.go）。
	if err := parseFlexible(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	names := fs.Args()
	if len(names) == 0 {
		listTargets(runDir(), "stop", "停")
		return 2
	}

	records, err := runDir().Find(targets(names))
	if err != nil {
		return fatal("%v", err)
	}
	if len(records) == 0 {
		fmt.Println("本机没有正在运行的网关实例。")
		return 0
	}

	failed := 0
	for _, record := range records {
		// 登记还在、进程已经没了（被强杀、断电）时，清理掉残留即可。
		// 把「本来就停了」报成失败，会让脚本里的 stop 在崩溃之后莫名其妙地
		// 返回非零——而那恰恰是最需要它顺利跑完的时候。
		if !record.Alive() {
			_ = runDir().Remove(record.Name)
			fmt.Printf("实例 %s 本来就没有在运行，已清理残留登记。\n", record.Name)
			continue
		}
		if err := stopRecord(record, *wait); err != nil {
			fmt.Fprintf(os.Stderr, "实例 %s 停止失败：%v\n", record.Name, err)
			failed++
			continue
		}
		// 清掉登记：实例没了还留着条目，status 会一直显示一条「未运行」的
		// 残留，看起来像出了问题。
		_ = runDir().Remove(record.Name)
		// 管理端（DSHPort 为 0）不托管 dsh，说「连同它托管的 dsh」是错的：
		// 实例各自由 portalruntime 按需拉起，停管理端不等于停那些 dsh。
		if record.DSHPort > 0 {
			fmt.Printf("实例 %s 已停止（%s，连同它托管的 dsh）。\n", record.Name, record.Listen)
		} else {
			fmt.Printf("实例 %s 已停止（%s；它托管的实例由各实例自己管理）。\n", record.Name, record.Listen)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// restartCommand 实现 `dsh-gate restart`：按当初的启动参数把实例再拉起来。
//
// 用「停 + 重新拉起」而不是给运行中的进程发信号：网关的监听端口、账号表、
// 环境变量都要在启动时确定，热重启做不到这些，只能重来一遍。
func restartCommand(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate restart [实例名...] [选项]

按当初的启动参数重启本机正在运行的网关实例。

和 stop 一样必须写明目标：写实例名重启那几个，写 all 才是全部，什么都不写只
列出在跑的实例、不做任何事。

重启会先停干净再拉起；新进程脱离终端运行，输出写在
<登记目录>/logs/<实例名>.log，所以这条命令执行完就返回，不会占着终端。

  dsh-gate restart personal     只重启 personal
  dsh-gate restart all          重启全部

选项:
`)
	}

	runDir := registerRunDirFlag(fs)
	wait := fs.Duration("wait", 30*time.Second, "等待端口重新就绪的最长时间")
	// parseFlexible：允许「实例名在前、选项在后」的写法（见 flags.go）。
	if err := parseFlexible(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	names := fs.Args()
	if len(names) == 0 {
		listTargets(runDir(), "restart", "重启")
		return 2
	}

	records, err := runDir().Find(targets(names))
	if err != nil {
		return fatal("%v", err)
	}
	if len(records) == 0 {
		fmt.Println("本机没有正在运行的网关实例，没有可重启的对象。")
		return 0
	}

	logDir := logDirFor(runDir())
	failed := 0
	for _, record := range records {
		// 已经不在运行（例如上次被强杀）时不必再停，直接从登记里的命令行重新
		// 拉起——「崩了就重启一下」正是 restart 最该顶上的场景，把它报成
		// 「停止失败」等于在最需要它的时候罢工。
		if !record.Alive() {
			fmt.Printf("实例 %s 已不在运行，直接重新拉起。\n", record.Name)
		} else if err := stopRecord(record, *wait); err != nil {
			fmt.Fprintf(os.Stderr, "实例 %s 停止失败，已放弃重启：%v\n", record.Name, err)
			failed++
			continue
		}
		// 端口关掉 ≠ 收尾完成：dsh 还在退出中就把新网关拉起来，新网关会把那个
		// 正在死的 dsh 接管过来，而接管的实例不由它看守——dsh 一死就再没人拉起，
		// 页面会永远停在「DSH 实例暂时不可用」。见 awaitStopped。
		if err := awaitStopped(record, *wait); err != nil {
			fmt.Fprintf(os.Stderr, "实例 %s 未能完成收尾：%v\n", record.Name, err)
			failed++
			continue
		}
		_ = runDir().Remove(record.Name)

		if err := launchRecord(record, logDir); err != nil {
			fmt.Fprintf(os.Stderr, "实例 %s 重新启动失败：%v\n", record.Name, err)
			failed++
			continue
		}
		addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", record.Port))
		if !waitListening(addr, *wait) {
			fmt.Fprintf(os.Stderr, "实例 %s 已重新启动，但 %s 后端口仍未就绪，看 %s\n",
				record.Name, *wait, filepath.Join(logDir, record.Name+".log"))
			failed++
			continue
		}
		fmt.Printf("实例 %s 已重启（%s）。日志：%s\n",
			record.Name, record.Listen, filepath.Join(logDir, record.Name+".log"))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// statusCommand 实现 `dsh-gate status`：本机正在运行哪些实例。
//
// 与 check 的分工：check 问的是「按这份配置跑会怎样」（端口有没有被占、dsh
// 命令在不在），status 问的是「现在到底跑着什么」。前者看配置，后者看登记，
// 所以 status 不需要你记得当时是怎么启动的。
func statusCommand(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate status [选项]

列出本机正在运行的网关实例：名字、端口、dsh 实例目录、账号表位置、启动时间。

选项:
`)
	}

	runDir := registerRunDirFlag(fs)
	verbose := fs.Bool("v", false, "同时显示完整命令行")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	records, problems := runDir().List()
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "警告：%v\n", problem)
	}
	if len(records) == 0 {
		fmt.Println("本机没有正在运行的网关实例。")
		fmt.Println("用 dsh-gate run 启动一个；启动之后就能用 stop / restart 管理。")
		return 0
	}

	running := 0
	for _, record := range records {
		if !record.Alive() {
			// 进程被强杀时来不及注销，条目会残留。留着它是因为下一次
			// restart 还要靠里面的命令行；但必须标出来，不能让人以为在跑。
			fmt.Printf("实例 %s：未运行（%s 的端口 %d 没有在监听，可能是被强杀或已经停止）\n",
				record.Name, record.Listen, record.Port)
			continue
		}
		running++
		fmt.Printf("实例 %s\n", record.Name)
		fmt.Printf("  网关端口  %s\n", record.Listen)
		// 管理端（DSHPort 为 0）不托管任何 dsh：实例各自由 portalruntime 按需拉起。
		// 打一个「127.0.0.1:0」出来只会让人以为配错了端口。
		if record.DSHPort > 0 {
			fmt.Printf("  dsh 端口  127.0.0.1:%d\n", record.DSHPort)
		} else {
			fmt.Printf("  dsh 端口  无（管理端不托管 dsh，实例按需启动）\n")
		}
		if record.DSHHome != "" {
			fmt.Printf("  dsh 目录  %s\n", record.DSHHome)
		}
		if record.UsersFile != "" {
			fmt.Printf("  账号表    %s\n", record.UsersFile)
		}
		fmt.Printf("  进程号    %d\n", record.PID)
		fmt.Printf("  已运行    %s\n", humanSince(record.StartedAt))
		if record.TLS {
			fmt.Printf("  访问地址  %s\n", accessURL(record, true))
		} else {
			fmt.Printf("  访问地址  %s（明文 HTTP）\n", accessURL(record, false))
		}
		if *verbose && len(record.Argv) > 0 {
			fmt.Printf("  启动命令  %s\n", strings.Join(record.Argv, " "))
		}
		fmt.Println()
	}

	fmt.Printf("共 %d 个实例在跑（登记 %d 条）。\n", running, len(records))
	fmt.Println("停止：dsh-gate stop [实例名...]     重启：dsh-gate restart [实例名...]")
	return 0
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

// humanSince 把启动至今的时长渲染成人话。
func humanSince(start time.Time) string {
	if start.IsZero() {
		return "未知"
	}
	d := time.Since(start)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时 %d 分钟", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// accessURL 拼一个可访问地址：优先用登记里的公告地址，否则用回环地址兜底。
func accessURL(record registry.Record, tls bool) string {
	scheme := "http"
	if tls {
		scheme = "https"
	}
	if record.PublicURL != "" {
		return record.PublicURL
	}
	return fmt.Sprintf("%s://127.0.0.1:%d/", scheme, record.Port)
}

// allKeyword 是「全部实例」的唯一写法。
//
// 曾经允许「不写实例名 = 全部」，但那意味着一次手抖、或一条从别处复制来的命令，
// 就会把包括公司那套正在用的实例在内的一切都停掉。「全部」必须是一个能被看见
// 的意图，而不是默认值。
const allKeyword = "all"

// targets 把命令行里的实例名转成 Find 认识的形式：nil 表示「全部」。
//
// 写了 all 就返回 nil（取全部），不管它和别的名字混没混在一起——
// 「all personal」就是全部，这里不必为此报错。
func targets(names []string) []string {
	for _, name := range names {
		if name == allKeyword {
			return nil
		}
	}
	return names
}

// listTargets 在没有指定目标时列出在跑的实例，让用户照着拼命令。
//
// 这条路径**不做任何改动**：命令的默认行为不该是「影响所有正在服务的东西」。
func listTargets(dir registry.Dir, action, verb string) {
	records, problems := dir.List()
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "警告：%v\n", problem)
	}

	var running []registry.Record
	for _, record := range records {
		if record.Alive() {
			running = append(running, record)
		}
	}
	if len(running) == 0 {
		fmt.Println("本机没有正在运行的网关实例。")
		return
	}

	fmt.Printf("请写明要%s哪些实例。当前在跑的有：\n", verb)
	for _, record := range running {
		fmt.Printf("  %-16s %s\n", record.Name, record.Listen)
	}
	fmt.Println()
	fmt.Printf("  dsh-gate %s <实例名>...   只%s指定的那几个\n", action, verb)
	fmt.Printf("  dsh-gate %s %s           %s全部\n", action, allKeyword, verb)
	fmt.Println()
	fmt.Println("（什么都不写时不会执行任何操作。）")
}
