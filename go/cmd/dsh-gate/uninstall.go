package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/portal"
	"github.com/wycto/dsh-gate/internal/term"
)

// uninstallCommand 实现 `dsh-gate uninstall`：卸载整个管理端。
//
// 顺序是固定的，先处理数据、最后才动服务本身：
//
//  1. 询问是否删除实例数据目录与会话记录（--yes 时由 --remove-data 决定）；
//  2. 停止所有 dsh 实例，并删除实例登记（选了删数据才一起删 DSH_HOME）；
//  3. 停止管理端进程；
//  4. 卸载开机自启的服系统服务单元。
//
// 管理端不运行时实例本来也没在跑，第 2 步会直接改数据库；管理端在运行时走
// 本机控制 API，复用管理台同一条「先停 → 再删目录 → 最后删记录」的路径。
func uninstallCommand(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate uninstall [选项]

卸载整个管理端：先停止并删除所有 dsh 实例，再停止管理端，最后卸载系统服务。

  dsh-gate uninstall                 交互式：会问是否删除实例数据与会话记录
  dsh-gate uninstall --yes           非交互：默认保留实例数据
  dsh-gate uninstall --remove-data   连实例数据目录与会话记录一起删除
  dsh-gate uninstall --purge         再删除管理端状态目录（账号库、日志、自备运行时）

实例的 dsh 安装目录不会被删除；需要的话请自行处理。

选项:
`)
		fs.PrintDefaults()
	}
	yes := fs.Bool("yes", false, "跳过确认；非交互模式下必须给出，是否删数据由 --remove-data 决定")
	removeData := fs.Bool("remove-data", false, "删除所有实例的数据目录与会话记录（不可恢复）")
	purge := fs.Bool("purge", false, "删除管理端状态目录（账号库、日志、自备运行时），不可恢复")
	_ = fs.Bool("service", false, "兼容旧参数：卸载总是会移除系统服务")
	stateDirFlag := fs.String("state-dir", "", "网关状态目录（默认取保存的配置或 ~/.dsh-gate）")
	wait := fs.Duration("wait", 20*time.Second, "等待管理端端口关闭的最长时间")
	runDir := registerRunDirFlag(fs)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		return usageError("uninstall 不接受位置参数（实例请用 dsh-gate dsh rm）")
	}

	stateDir, err := resolveStateDir(*stateDirFlag)
	if err != nil {
		return fatal("%v", err)
	}
	dbPath := filepath.Join(stateDir, "portal.db")
	instances := loadPortalInstances(dbPath)

	deleteData := *removeData
	if len(instances) > 0 && !*yes {
		if !term.IsTerminal(os.Stdin.Fd()) {
			return usageError("非交互模式请加 --yes；是否删除实例数据用 --remove-data 显式指定")
		}
		fmt.Printf("将停止并删除所有 dsh 实例（共 %d 个），然后卸载管理端。\n", len(instances))
		answer, err := term.ReadLine(os.Stdin, os.Stdout, "是否同时删除实例数据目录与会话记录？[y/N] ")
		if err != nil {
			return fatal("读取确认失败：%v", err)
		}
		deleteData = isYes(answer)
	}

	_, portalRunning := portalRecord(resolveRunDir(""))
	if len(instances) > 0 {
		if code := removeInstancesForUninstall(stateDir, dbPath, instances, deleteData, portalRunning); code != 0 {
			return code
		}
	}

	serviceWasInstalled := serviceUnitInstalled()

	// 先停管理端：由系统服务托管时必须走服务管理器，否则 Restart=always 会把它拉回来。
	if serviceWasInstalled {
		if err := serviceStop(); err != nil {
			fmt.Fprintf(os.Stderr, "停止系统服务失败：%v\n", err)
		} else {
			fmt.Println("管理端已停止。")
		}
	} else if code := stopPortal(runDir(), *wait, false); code != 0 {
		return code
	}
	_ = runDir().Remove(portalRecordName)

	// 最后才卸载服务本身。
	if serviceWasInstalled {
		if err := serviceUninstall(serviceOptions{}); err != nil {
			fmt.Fprintf(os.Stderr, "卸载系统服务失败：%v\n（需要 root 时请用 sudo 重跑）\n", err)
		} else {
			fmt.Println("系统服务已卸载。")
		}
	}

	if *purge {
		if err := purgeStateDir(stateDir); err != nil {
			return fatal("%v", err)
		}
		fmt.Printf("已删除管理端状态目录：%s\n", stateDir)
	}

	if len(instances) > 0 && !deleteData {
		fmt.Println("\n以下实例数据目录与会话记录已保留：")
		for _, inst := range instances {
			fmt.Printf("  %s  %s\n", inst.Name, instanceDataHome(inst))
		}
	}
	fmt.Println("\ndsh-gate 可执行文件没有删除：按你的安装方式（发行包 / 自己编译）自行处理。")
	fmt.Println("管理端已卸载。")
	return 0
}

// loadPortalInstances 读管理端数据库里的实例。读不到（没装过 / 已删）返回空。
func loadPortalInstances(dbPath string) []portal.Instance {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	store, err := portal.Open(dbPath)
	if err != nil {
		return nil
	}
	defer store.Close()
	instances, err := store.ListInstances()
	if err != nil {
		return nil
	}
	return instances
}

// removeInstancesForUninstall 停止并删除所有实例。
func removeInstancesForUninstall(stateDir, dbPath string, instances []portal.Instance, deleteData, portalRunning bool) int {
	if portalRunning {
		failed := 0
		for _, inst := range instances {
			_, err := localPost(stateDir, localRequest{
				Action:    "remove",
				Instance:  inst.Name,
				PurgeHome: deleteData,
			})
			if err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "删除实例 %q 失败：%v\n", inst.Name, err)
			}
		}
		if failed > 0 {
			return 1
		}
		fmt.Printf("已停止并删除 %d 个实例。\n", len(instances))
		return 0
	}

	// 管理端没在运行：实例本来也没在跑，直接删登记（选了删数据才动 DSH_HOME）。
	store, err := portal.Open(dbPath)
	if err != nil {
		return fatal("打开管理端数据库失败：%v", err)
	}
	defer store.Close()
	failed := 0
	for _, inst := range instances {
		if deleteData {
			home := instanceDataHome(inst)
			if home != "" {
				if err := safeRemoveDir(home); err != nil {
					failed++
					fmt.Fprintf(os.Stderr, "删除实例数据目录 %s 失败：%v\n", home, err)
					continue
				}
			}
		}
		if err := store.DeleteInstance(inst.ID); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "删除实例 %q 失败：%v\n", inst.Name, err)
		}
	}
	if failed > 0 {
		return 1
	}
	fmt.Printf("已删除 %d 个实例。\n", len(instances))
	return 0
}

// instanceDataHome 返回实例实际生效的 DSH_HOME：配置目录填了以它为准。
func instanceDataHome(inst portal.Instance) string {
	if home := strings.TrimSpace(inst.ConfigDir); home != "" {
		return home
	}
	return strings.TrimSpace(inst.DshHome)
}

// isYes 判断确认回答是否为肯定。
func isYes(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "是":
		return true
	default:
		return false
	}
}

// purgeStateDir 删除管理端状态目录。
func purgeStateDir(stateDir string) error {
	if err := safeRemoveDir(stateDir); err != nil {
		return fmt.Errorf("删除状态目录失败: %w", err)
	}
	return nil
}
