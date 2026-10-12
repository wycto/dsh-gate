package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/fmtx"
	"github.com/wycto/dsh-gate/internal/server"
	"github.com/wycto/dsh-gate/internal/users"
)

// checkCommand 实现 `dsh-gate check`：不改任何状态的启动前体检。
//
// 它回答的是「现在直接 run 会发生什么」：账号够不够、端口有没有被抢、
// dsh 在不在、dsh 命令能不能找到、别的设备该打开哪个地址。诊断信息写 stdout，
// 日志走 stderr，因此可以放心地 `dsh-gate check > report.txt`。
func checkCommand(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate check [选项]

启动前体检：账号、端口占用、dsh 是否就绪、访问地址。不修改任何状态。

选项与 dsh-gate run 相同；退出码非 0 表示存在必须先解决的问题。

`)
		fs.PrintDefaults()
	}

	hold := registerInstanceFlags(fs)
	var configPath string
	fs.StringVar(&configPath, "config", "", "多实例配置文件（JSON）")

	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	configs, code := resolveConfigs(fs, configPath, hold)
	if code != 0 {
		return code
	}

	fmt.Printf("dsh-gate 体检 · 共 %d 个实例\n", len(configs))
	failed := 0
	for index, cfg := range configs {
		if index > 0 {
			fmt.Println()
		}
		if !checkInstance(cfg) {
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// checkInstance 体检单个实例，返回是否一切正常。
func checkInstance(cfg config.Instance) bool {
	problems := 0
	fmt.Printf("\n实例 %s\n", cfg.Name)

	// 账号：没有账号网关会拒绝启动（fail closed），这是最需要提前发现的事。
	store, err := users.Open(cfg.UsersFile)
	switch {
	case err != nil:
		fmt.Printf("%s无法读取（%v）\n", label("账号"), err)
		problems++
	case store.Count() == 0:
		fmt.Printf("%s0 个（%s）\n", label("账号"), cfg.UsersFile)
		fmt.Printf("%s创建一个账号：dsh-gate user add admin --state-dir %s\n", label(""), cfg.StateDir)
		problems++
	default:
		fmt.Printf("%s%d 个（%s）\n", label("账号"), store.Count(), cfg.UsersFile)
	}

	// 网关端口：被占用不一定是坏事（可能是本机另一个实例），只作提示。
	if dshproc.Busy(cfg.ListenAddr(), 300*1000*1000) {
		fmt.Printf("%s%s 已被占用（如果网关已在运行，这是正常的）\n",
			label("网关端口"), cfg.ListenAddr())
	} else {
		fmt.Printf("%s%s 空闲\n", label("网关端口"), cfg.ListenAddr())
	}

	// dsh 端口：这是决定性的一项。
	state := dshproc.Inspect(cfg.DSHAddr(), 2*1000*1000)
	switch {
	case !state.InUse:
		if cfg.AutoStart {
			fmt.Printf("%s%s 空闲，网关会负责拉起 dsh\n", label("dsh 端口"), cfg.DSHAddr())
			if _, err := dshproc.DetectBinary(); err != nil {
				// 系统里没有 Node.js 不算「必须先解决的问题」：网关启动时会
				// 自己下官方运行时到状态目录，只有连这条路也不通才需要人动手。
				if binDir, ok := dshproc.LocalNodeBin(cfg.StateDirPath()); ok {
					fmt.Printf("%s系统 PATH 里没有 npx，将使用网关自备的运行时（%s）\n", label("dsh 命令"), binDir)
				} else {
					fmt.Printf("%s本机没有 Node.js：网关启动时会自动下载官方运行时到 %s（不改系统、不需 root）\n",
						label("dsh 命令"), filepath.Join(cfg.StateDirPath(), "node"))
					fmt.Printf("%s下载失败时可用 --dsh-cmd 指定启动命令\n", label(""))
				}
			} else {
				binary, _ := dshproc.DetectBinary()
				fmt.Printf("%s已找到（%s）\n", label("dsh 命令"), binary)
			}
		} else {
			fmt.Printf("%s%s 空闲，但当前设置为不自动拉起 dsh\n", label("dsh 端口"), cfg.DSHAddr())
			fmt.Printf("%s请自己先启动 dsh，或加 --auto-start\n", label(""))
			problems++
		}
	case state.DSH:
		fmt.Printf("%s%s %s\n", label("dsh 端口"), cfg.DSHAddr(), state.Detail)
		fmt.Printf("%s网关会接管它\n", label(""))
	default:
		fmt.Printf("%s%s %s\n", label("dsh 端口"), cfg.DSHAddr(), state.Detail)
		fmt.Printf("%s换一个 --dsh-port，或先停掉占用它的程序\n", label(""))
		problems++
	}

	// 对外地址：用户最需要的一段信息。
	fmt.Printf("%s%s\n", label("dsh 目录"), cfg.DSHHome)
	fmt.Printf("%s%s\n", label("网关目录"), cfg.StateDir)
	scheme := cfg.Scheme()
	fmt.Printf("%shttp://127.0.0.1:%d/\n", label("本机访问"), cfg.Port)
	for _, address := range server.DisplayAddresses(6) {
		fmt.Printf("%s%s://%s/\n", label("局域网访问"), scheme, joinHostPort(address, cfg.Port))
	}
	if len(server.DisplayAddresses(0)) == 0 {
		fmt.Printf("%s本机没有非回环地址；异地访问请显式指定 --public-url\n", label("提示"))
	}

	if cfg.TLSEnabled() {
		fmt.Printf("%s已启用（证书 %s）\n", label("HTTPS"), cfg.TLSCert)
	} else {
		fmt.Printf("%s未启用；跨公网访问建议配 --tls-cert/--tls-key\n", label("HTTPS"))
	}
	return problems == 0
}

// label 生成对齐后的输出前缀（中文按两列计算宽度）。
func label(text string) string { return "  " + fmtx.Pad(text, 12) + " " }

// joinHostPort 拼 authority，IPv6 需要方括号。
func joinHostPort(address string, port int) string {
	if strings.Contains(address, ":") {
		return fmt.Sprintf("[%s]:%d", address, port)
	}
	return fmt.Sprintf("%s:%d", address, port)
}
