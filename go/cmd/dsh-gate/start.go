package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/registry"
)

// `dsh-gate start`：启动管理端。
//
// 管理端是机器级单例（一个 portal.db 对应一个进程），所以没有「起第几个」的
// 问题，参数只决定监听地址、状态目录与 TLS。默认后台化：命令自己很快返回，
// 日志写 <登记目录>/logs/portal.log；--foreground 留在前台，供服务单元与调试用。
//
// 启动参数会存到 <状态目录>/portal.json：stop 之后登记条目就删了，restart 与
// status 只能靠这份文件知道「上次是怎么启动的」。存完整的 `portal run` 参数而
// 不是重新拼一份，是为了不引入第二套默认值。
const portalConfigName = "portal.json"

// portalConfig 是保存下来的管理端启动参数。
type portalConfig struct {
	// Args 是传给 `portal run` 的参数（不含程序名与子命令）。
	Args []string `json:"args"`
}

// portalConfigPath 返回管理端启动参数的保存位置。
func portalConfigPath(stateDir string) string {
	return filepath.Join(stateDir, portalConfigName)
}

// loadPortalConfig 读取保存的启动参数。
func loadPortalConfig(stateDir string) (portalConfig, error) {
	var cfg portalConfig
	raw, err := os.ReadFile(portalConfigPath(stateDir))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("解析 %s 失败: %w", portalConfigPath(stateDir), err)
	}
	return cfg, nil
}

// savePortalConfig 保存启动参数（0600：里面可能有 TLS 路径、内网地址）。
func savePortalConfig(stateDir string, cfg portalConfig) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(portalConfigPath(stateDir), raw, 0o600)
}

// resolveStateDir 把 --state-dir（可空）解析成绝对路径，与 portal 数据库的推导
// 口径保持一致（config.DefaultStateDir + 环境变量）。
func resolveStateDir(raw string) (string, error) {
	cfg := config.Default()
	if err := config.ApplyEnv(&cfg, os.Getenv); err != nil {
		return "", err
	}
	set := map[string]string{}
	if strings.TrimSpace(raw) != "" {
		set["state-dir"] = strings.TrimSpace(raw)
	}
	if err := config.Apply(&cfg, set); err != nil {
		return "", err
	}
	if err := cfg.Normalize(); err != nil {
		return "", err
	}
	return cfg.StateDirPath(), nil
}

// buildStartArgs 把 start 的输入整理成 `portal run` 的参数。
//
// 全部显式写出（包括默认值），保存下来的配置就能独立复原，不受将来默认值变化
// 影响；restart 只认这份文件，不会再读一次环境变量。唯一的例外是
// startTimeout：它与默认值相同时不写——这样将来调默认值能惠及老配置，而用户
// 真给过一个值时又一定被记住。
func buildStartArgs(host string, port int, tlsCert, tlsKey, publicURL, stateDir, db, npmRegistry string, trusted []string, startTimeout time.Duration) []string {
	args := []string{
		"--host", host,
		"--port", strconv.Itoa(port),
		"--state-dir", stateDir,
	}
	if tlsCert != "" {
		args = append(args, "--tls-cert", tlsCert)
	}
	if tlsKey != "" {
		args = append(args, "--tls-key", tlsKey)
	}
	if publicURL != "" {
		args = append(args, "--public-url", publicURL)
	}
	if db != "" {
		args = append(args, "--db", db)
	}
	if npmRegistry != "" {
		args = append(args, "--npm-registry", npmRegistry)
	}
	if startTimeout != config.DefaultDSHStartTimeout {
		args = append(args, "--dsh-start-timeout", startTimeout.String())
	}
	for _, host := range trusted {
		if strings.TrimSpace(host) != "" {
			args = append(args, "--trusted-host", host)
		}
	}
	return args
}

// portalFlags 是管理端的那组启动参数。
//
// 为什么抽出来：`start`、Windows 的 `service run` 与 `tray` 都要接受同一组选项，
// 而后两者是把这份参数**原样写进计划任务 XML** 的。少定义一个选项不是「那个选项
// 用不上」，是任务一跑就报「flag provided but not defined」，托盘当场退出，症状却
// 表现为「装了自启、什么也没起来」。一处定义就没这个机会。
type portalFlags struct {
	host        string
	port        int
	tlsCert     string
	tlsKey      string
	publicURL   string
	stateDir    string
	dbPath      string
	npmRegistry string
	trusted     stringList
	// startTimeout 是每个实例等它自己的 dsh 端口就绪的最长时间。默认取
	// config.DefaultDSHStartTimeout，与 `portal run` 一致；与默认值相同时
	// 不写进保存的启动参数（见 buildStartArgs）。
	startTimeout time.Duration
}

// registerPortalFlags 把管理端的启动参数注册进 fs。
func registerPortalFlags(fs *flag.FlagSet) *portalFlags {
	pf := &portalFlags{}
	fs.StringVar(&pf.host, "host", "0.0.0.0", "管理端监听地址")
	fs.IntVar(&pf.port, "port", 12020, "管理端监听端口")
	fs.StringVar(&pf.tlsCert, "tls-cert", "", "HTTPS 证书（与 --tls-key 成对）")
	fs.StringVar(&pf.tlsKey, "tls-key", "", "HTTPS 私钥（与 --tls-cert 成对）")
	fs.StringVar(&pf.publicURL, "public-url", "", "对外公告地址，如 https://dsh.example.com/")
	fs.StringVar(&pf.stateDir, "state-dir", "", "网关状态目录（默认 ~/.dsh-gate）")
	fs.StringVar(&pf.dbPath, "db", "", "管理端数据库路径（默认 <状态目录>/portal.db）")
	fs.StringVar(&pf.npmRegistry, "npm-registry", "", "安装 dsh 用的 npm 源；留空用默认")
	fs.DurationVar(&pf.startTimeout, "dsh-start-timeout", config.DefaultDSHStartTimeout,
		"每个实例拉起 dsh 后等它监听端口的最长时间（如 10m）")
	fs.Var(&pf.trusted, "trusted-host", "额外信任的地址（管理端域名），可重复")
	return pf
}

// startArgs 把这组参数整理成 `portal run` 的参数（stateDir 已经解析成绝对路径）。
func (pf *portalFlags) startArgs(stateDir string) []string {
	return buildStartArgs(pf.host, pf.port, pf.tlsCert, pf.tlsKey, pf.publicURL,
		stateDir, strings.TrimSpace(pf.dbPath), strings.TrimSpace(pf.npmRegistry), pf.trusted, pf.startTimeout)
}

// startCommand 实现 `dsh-gate start`。
func startCommand(args []string) int {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate start [选项]

启动管理端（默认后台运行，日志写 <登记目录>/logs/portal.log）。

--foreground 留在前台：服务单元与排查问题时用它。管理端的监听地址、状态目录、
TLS 等参数与 install 时一致；不给就用上次保存的配置。

  dsh-gate start                       按上次配置后台启动；装了系统服务时转给服务管理器
  dsh-gate start --port 8080           换端口后台启动（并记住）
  dsh-gate start --foreground          前台启动（服务单元内部用）

选项:
`)
		fs.PrintDefaults()
	}
	pf := registerPortalFlags(fs)
	foreground := fs.Bool("foreground", false, "前台运行（默认后台）")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		return usageError("start 不接受位置参数，多出来的是 %q", fs.Arg(0))
	}

	// 管理端由系统服务托管时，start/stop/restart 都转给服务管理器：
	// 否则 systemd 的 Restart=always 会把刚停掉的进程又拉起来。
	if !*foreground && serviceUnitInstalled() {
		explicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "state-dir" && f.Name != "foreground" {
				explicit = true
			}
		})
		if explicit {
			return usageError("管理端已由系统服务托管：改端口/地址/启动等待时间请编辑服务单元" +
				"（如 systemctl edit dsh-gate，或 sudo dsh-gate service install 再带一次参数），再用 dsh-gate restart")
		}
		if err := serviceStart(); err != nil {
			return fatal("启动系统服务失败：%v", err)
		}
		if waitPortalRegistered(20 * time.Second) {
			fmt.Println("管理端由系统服务托管，已启动；用 dsh-gate status 查看。")
		} else {
			fmt.Println("已请求启动系统服务，但 20 秒内没看到运行登记；用 dsh-gate status 或 journalctl -u dsh-gate 查看。")
		}
		return 0
	}

	stateDir, err := resolveStateDir(pf.stateDir)
	if err != nil {
		return fatal("%v", err)
	}
	if record, running := portalRecord(resolveRunDir("")); running {
		return fatal("管理端已经在运行（%s，进程号 %d）：要重启请用 dsh-gate restart", record.Listen, record.PID)
	}

	// 一个启动参数都没给时沿用上次保存的配置：`dsh-gate start` 最自然的预期是
	// 「按上次那样起来」，而不是把端口悄悄改回 12020。
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "state-dir" && f.Name != "foreground" {
			explicit = true
		}
	})
	if !explicit {
		if cfg, err := loadPortalConfig(stateDir); err == nil && len(cfg.Args) > 0 {
			runArgs := append([]string{}, cfg.Args...)
			if !*foreground {
				runArgs = append(runArgs, "--background")
			}
			return portalRunCommand(runArgs)
		}
	}

	runArgs := pf.startArgs(stateDir)

	// 先保存再启动：后台化后父进程马上退出，子进程不该再负责「记住配置」。
	// 写失败不拦启动（配置还能从命令行给），但要明确提示。
	if err := savePortalConfig(stateDir, portalConfig{Args: runArgs}); err != nil {
		fmt.Fprintf(os.Stderr, "提示：保存启动配置失败（restart/status 可能找不到上次的参数）：%v\n", err)
	}
	if !*foreground {
		runArgs = append(runArgs, "--background")
	}
	return portalRunCommand(runArgs)
}

// startFromConfig 按保存的配置在后台启动管理端。
func startFromConfig(cfg portalConfig) int {
	args := append([]string{}, cfg.Args...)
	args = append(args, "--background")
	return portalRunCommand(args)
}

// restartCommand 实现 `dsh-gate restart`：停掉管理端，再按上次保存的参数拉起。
//
// 没有保存的配置就报错而不是猜一组默认值：默认端口可能正被别的程序占着，
// 猜出来的「重启成功」只是把一个空壳服务放到了另一个端口上。
func restartCommand(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate restart [选项]

重启整个管理端：装了系统服务时转给服务管理器，否则按上次 start/install 保存的配置重启。

选项:
`)
		fs.PrintDefaults()
	}
	stateDirFlag := fs.String("state-dir", "", "网关状态目录（默认 ~/.dsh-gate）")
	wait := fs.Duration("wait", 20*time.Second, "等待管理端端口关闭的最长时间")
	runDir := registerRunDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if serviceUnitInstalled() {
		if err := serviceRestart(); err != nil {
			return fatal("重启系统服务失败：%v", err)
		}
		if waitPortalRegistered(20 * time.Second) {
			fmt.Println("管理端由系统服务托管，已重启；用 dsh-gate status 查看。")
		} else {
			fmt.Println("已请求重启系统服务，但 20 秒内没看到运行登记；用 dsh-gate status 或 journalctl -u dsh-gate 查看。")
		}
		return 0
	}
	stateDir, err := resolveStateDir(*stateDirFlag)
	if err != nil {
		return fatal("%v", err)
	}
	if code := stopPortal(runDir(), *wait, true); code != 0 {
		return code
	}
	cfg, err := loadPortalConfig(stateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fatal("没有找到上次的启动配置（%s）：先 dsh-gate install 或 dsh-gate start", portalConfigPath(stateDir))
		}
		return fatal("%v", err)
	}
	if len(cfg.Args) == 0 {
		return fatal("启动配置为空（%s）：先 dsh-gate install 或 dsh-gate start", portalConfigPath(stateDir))
	}
	return startFromConfig(cfg)
}

// waitPortalRegistered 等服务托管的管理端写完运行登记。
//
// systemctl start 只保证进程起来了，管理端还要打开数据库、监听端口、写登记；
// 立刻跑 status 会看到「未运行」。这里等到登记出现再返回，让 start / restart
// 的提示与用户的下一条命令看到的状态一致。
func waitPortalRegistered(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if _, ok := portalRecord(resolveRunDir("")); ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// portalRecord 取管理端的运行登记，ok=false 表示没在运行（含残留条目）。
func portalRecord(runDir registry.Dir) (registry.Record, bool) {
	record, ok, err := runDir.Load(portalRecordName)
	if err != nil || !ok {
		return registry.Record{}, false
	}
	if !record.Alive() {
		return record, false
	}
	return record, true
}
