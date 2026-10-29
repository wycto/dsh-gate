package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/registry"
)

// statusCommand 实现 `dsh-gate status`：管理端在不在跑，以及登记了哪些实例。
//
// 与页面的分工：页面回答「每个实例的实时状态」（它就在运行时里），status 回答
// 「这台机器上的管理端与实例定义是什么样」——管理端没在跑时也能看，正是排查
// 「为什么打不开」的第一步。
func statusCommand(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate status [选项]

查看管理端是否在运行、上次的启动配置，以及已经登记的 dsh 实例。

选项:
`)
		fs.PrintDefaults()
	}
	stateDirFlag := fs.String("state-dir", "", "网关状态目录（默认 ~/.dsh-gate）")
	dbFlag := fs.String("db", "", "管理端数据库路径（默认 <状态目录>/portal.db）")
	verbose := fs.Bool("v", false, "同时显示管理端启动参数")
	runDir := registerRunDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		return usageError("status 不接受位置参数")
	}
	stateDir, err := resolveStateDir(*stateDirFlag)
	if err != nil {
		return fatal("%v", err)
	}

	record, running := portalRecord(runDir())
	if running {
		fmt.Println("管理端：运行中")
		fmt.Printf("  监听地址  %s\n", record.Listen)
		fmt.Printf("  访问地址  %s\n", statusURL(record))
		if record.PublicURL != "" {
			fmt.Printf("  对外公告  %s\n", record.PublicURL)
		}
		fmt.Printf("  进程号    %d\n", record.PID)
		fmt.Printf("  版本      %s\n", record.Version)
		fmt.Printf("  已运行    %s\n", humanSince(record.StartedAt))
		fmt.Printf("  日志      %s\n", filepath.Join(logDir(runDir()), portalRecordName+".log"))
		if *verbose && len(record.Argv) > 0 {
			fmt.Printf("  启动命令  %s\n", strings.Join(record.Argv, " "))
		}
	} else {
		fmt.Println("管理端：未运行")
		if cfg, err := loadPortalConfig(stateDir); err == nil && len(cfg.Args) > 0 {
			fmt.Printf("  上次配置  %s\n", strings.Join(cfg.Args, " "))
			fmt.Println("  启动：dsh-gate start")
		} else {
			fmt.Println("  还没有启动过；先 dsh-gate install 或 dsh-gate start")
		}
	}

	// 实例定义在 SQLite 里，管理端没在跑也能读。运行状态只有管理端知道，
	// 能连上本机控制 API 就问它，连不上就只报定义。
	dbPath := strings.TrimSpace(*dbFlag)
	if dbPath == "" {
		dbPath = filepath.Join(stateDir, "portal.db")
	}
	instances, listErr := listInstancesFromDB(dbPath)
	if listErr != nil {
		fmt.Printf("\n实例：读不到管理端数据库（%s）\n", dbPath)
		return 0
	}
	if len(instances) == 0 {
		fmt.Println("\n实例：还没有创建（在管理页 /admin 或用 dsh-gate dsh add 创建）")
		return 0
	}

	states := map[string]localInstanceInfo{}
	if running {
		if infos, err := localListInstances(stateDir); err == nil {
			for _, info := range infos {
				states[info.ID] = info
			}
		}
	}
	fmt.Printf("\n实例：共 %d 个\n", len(instances))
	for _, inst := range instances {
		stateLabel := "运行状态以管理台为准"
		if info, ok := states[inst.ID]; ok {
			switch {
			case info.Running && info.Ready:
				stateLabel = "运行中"
			case info.Running:
				stateLabel = "启动中"
			default:
				stateLabel = "未运行"
			}
		}
		fmt.Printf("  %-20s %-10s :%d  %s\n", inst.Name, inst.ID, inst.DshPort, stateLabel)
	}
	fmt.Println("\n操作：dsh-gate dsh list / dsh-gate dsh restart <名字>；页面：/admin")
	return 0
}

// listInstancesFromDB 直接读管理端数据库里的实例定义（不启动运行时）。
func listInstancesFromDB(dbPath string) ([]portal.Instance, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	store, err := portal.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.ListInstances()
}

// statusURL 拼管理端的访问地址：优先公告地址，否则用回环地址。
func statusURL(record registry.Record) string {
	scheme := "http"
	if record.TLS {
		scheme = "https"
	}
	if strings.TrimSpace(record.PublicURL) != "" {
		return record.PublicURL
	}
	host, _, err := net.SplitHostPort(record.Listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s://%s:%d/", scheme, host, record.Port)
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
