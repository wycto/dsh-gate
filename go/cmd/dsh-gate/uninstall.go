package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/registry"
	"github.com/wycto/dsh-gate/internal/term"
)

// uninstallCommand 实现 `dsh-gate uninstall <实例名...|all>`：停掉**指定**的实例，
// 并按需取消开机自启、删除数据。
//
// 为什么必须写明目标（与 stop / restart 同一条约定）：第一版 uninstall 会停掉
// 登记里的全部实例——多实例机器上一次手抖就把别人正在用的那套收走了。凡是
// 「默认动全部、且不可逆」的命令，默认都必须是空操作，这里也不例外：
// 什么都不写只列清单，`all` 才表示全部。
//
// 三件事严格分开，各自都要显式请求：
//
//   - 停实例：目标写在命令行上（本命令的主体）；
//   - 取消自启：`--service`。服务单元是**机器级**的，一台机器只有一个，
//     所以它没有「按实例」这一说——要确认它托管的是哪个实例，看它打印的命令；
//   - 删数据：`--purge`。账号表、日志、自备运行时删掉就找不回来；而且登记目录
//     是机器级共享的，还有实例在跑时删它会让那些实例丢掉自己的身份，因此
//     那种情况下会直接拒绝。
func uninstallCommand(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `用法: dsh-gate uninstall <实例名...|all> [选项]

停掉指定的实例并注销它们的运行登记，可选地取消开机自启、删除数据。

目标必须写明（和 stop / restart 一样）：写实例名就停那几个，写 all 才是全部；
什么都不写只列出在跑的实例、不执行任何操作——「默认停全部」在多实例机器上
一次手抖就能把别人正在用的实例收走。

  dsh-gate uninstall test                  只卸载 test
  dsh-gate uninstall personal work         卸载这两个
  dsh-gate uninstall all --service --purge 整机收尾：停全部 + 取消自启 + 删数据

dsh 自己的数据（~/.dsh，会话与插件）不属于 dsh-gate，不做改动；
dsh-gate 可执行文件也不删，结束时打印它的路径，由你按安装方式处理。

选项:
`)
		fs.PrintDefaults()
	}

	wantService := fs.Bool("service", false, "同时取消开机（登录）自启；服务单元是机器级的，只有一个")
	purge := fs.Bool("purge", false, "删除状态目录与运行登记（账号表、日志、自备运行时），不可恢复")
	wait := fs.Duration("wait", 20*time.Second, "等待实例端口关闭的最长时间")
	stateDirFlag := fs.String("state-dir", config.DefaultStateDir(),
		"要删除的状态目录（--purge 时用）")
	runDir := registerRunDirFlag(fs)

	// 用 parseFlexible 而不是 fs.Parse：Go 的 flag 遇到第一个位置参数就停止解析，
	// 而 `uninstall test --purge` 这种「实例名在前、选项在后」的顺序是最自然的
	// 写法——用 fs.Parse 会把 --purge 当成实例名，实测踩过。
	if err := parseFlexible(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	dir := runDir()
	names := fs.Args()
	if len(names) == 0 {
		listTargets(dir, "uninstall", "卸载")
		return 2
	}
	records, err := dir.Find(targets(names))
	if err != nil {
		return fatal("%v", err)
	}

	stateDir := config.ExpandPath(*stateDirFlag)
	interactive := term.IsTerminal(os.Stdin.Fd())
	fmt.Println("开始卸载 dsh-gate。")

	failed := false

	// 1. 停掉点名的实例。放在后面几步之前：删数据要读运行登记，删完就没有依据了。
	fmt.Println()
	fmt.Println("1/3 停止指定的实例")
	if uninstallStopsInstances(dir, records, *wait) {
		failed = true
	}

	// 2. 取消自启：机器级的一个服务单元，只有 --service 才动。
	fmt.Println()
	fmt.Println("2/3 开机（登录）自启")
	switch {
	case !*wantService:
		fmt.Println("  未改动（要取消自启请加 --service；它管的是整台机器上那一个服务单元）。")
	default:
		managed := serviceManagedCommand()
		if err := serviceUninstall(serviceOptions{}); err != nil {
			fmt.Fprintf(os.Stderr, "  未能取消自启：%v\n", err)
			fmt.Fprintln(os.Stderr, "  若是权限问题，用 sudo 重跑。")
			failed = true
		} else if managed != "" {
			fmt.Printf("  已取消（它托管的命令：%s）\n", managed)
		} else {
			fmt.Println("  已取消（本来就没装过也算完成）。")
		}
	}

	// 3. 数据：只有显式 --purge（或交互式点头）才删；还有别的实例在跑时拒绝。
	fmt.Println()
	fmt.Println("3/3 网关状态目录")
	purgeTargets := uninstallTargets(stateDir, string(dir.Expand()))
	blockers := otherAliveInstances(dir, records)
	switch {
	case !*purge && len(blockers) == 0 && interactive:
		purgeData, err := decidePurge(false, true, stateDir, purgeTargets)
		if err != nil {
			return fatal("%v", err)
		}
		*purge = purgeData
	}

	if !*purge {
		fmt.Printf("  已保留 %s（要删除请加 --purge）\n", stateDir)
	} else if len(blockers) > 0 {
		// 登记目录是机器级的：删掉它，还在跑的实例就丢了身份，status / stop
		// 再也找不到它们。宁可拒绝，也不能留下一个管不了的进程。
		fmt.Fprintf(os.Stderr, "  未删除：本机还有实例在运行：%s\n", strings.Join(recordNames(blockers), "、"))
		fmt.Fprintln(os.Stderr, "  先停掉它们（dsh-gate uninstall all --purge），或把 --state-dir 指到别处。")
		failed = true
	} else {
		for _, target := range purgeTargets {
			if _, err := os.Stat(target); os.IsNotExist(err) {
				fmt.Printf("  %s 本来就不存在。\n", target)
				continue
			}
			if err := uninstallRemoveAll(target); err != nil {
				fmt.Fprintf(os.Stderr, "  删除 %s 失败：%v\n", target, err)
				failed = true
				continue
			}
			fmt.Printf("  已删除 %s\n", target)
		}
	}

	uninstallTail()
	if failed {
		return 1
	}
	return 0
}

// uninstallStopsInstances 停掉点名的实例，返回是否有失败。
//
// 只动 records 里那几个（它们来自命令行），不碰登记里的其它实例。
func uninstallStopsInstances(dir registry.Dir, records []registry.Record, wait time.Duration) bool {
	if len(records) == 0 {
		fmt.Println("  没有找到这些实例的运行登记（可能已经停过并注销了）。")
		return false
	}

	failed := false
	for _, record := range records {
		if !record.Alive() {
			// 进程被强杀时来不及注销，条目会残留。卸载是最后一次清理机会。
			_ = dir.Remove(record.Name)
			fmt.Printf("  实例 %s 本来就没在跑，已清理残留登记。\n", record.Name)
			continue
		}
		if err := stopRecord(record, wait); err != nil {
			fmt.Fprintf(os.Stderr, "  实例 %s 停止失败：%v\n", record.Name, err)
			failed = true
			continue
		}
		_ = dir.Remove(record.Name)
		fmt.Printf("  实例 %s 已停止（连同它托管的 dsh）。\n", record.Name)
	}
	return failed
}

// otherAliveInstances 返回没被本次点名、却还在运行的实例。
//
// 删数据前的守门人：状态目录/登记目录是机器级共享的，只要还有别人在跑，
// 就不能把它们删掉。
func otherAliveInstances(dir registry.Dir, handled []registry.Record) []registry.Record {
	handledNames := make(map[string]bool, len(handled))
	for _, record := range handled {
		handledNames[record.Name] = true
	}
	all, _ := dir.List()
	var out []registry.Record
	for _, record := range all {
		if handledNames[record.Name] {
			continue
		}
		if record.Alive() {
			out = append(out, record)
		}
	}
	return out
}

// recordNames 取实例名，供提示语使用。
func recordNames(records []registry.Record) []string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
	}
	return names
}

// decidePurge 决定要不要删数据：给了 --purge 就删；没给时只在交互式终端里
// 当场问一句，其余一律保留。
//
// 默认值必须是「保留」：账号表删掉找不回来，而多跑一次 --purge 只是麻烦一点。
// 非交互场景（管道、CI）不猜——猜错的代价不对等。
func decidePurge(purge bool, interactive bool, stateDir string, targets []string) (bool, error) {
	if purge {
		return true, nil
	}
	if !interactive {
		return false, nil
	}
	fmt.Printf("  %s（账号表、日志、自备运行时）\n", stateDir)
	for _, target := range targets {
		if target != stateDir {
			fmt.Printf("  %s（运行登记与日志）\n", target)
		}
	}
	return askYesNo("  一并删除？删掉就找不回来了 [y/N] ", false)
}

// uninstallTargets 汇总要删除的目录，去掉互相嵌套的重复项。
//
// 运行登记默认就在状态目录里（~/.dsh-gate/run），单独再删一次只会多打一行
// 「已删除」；--state-dir 改成别处时两者才真的分开。
func uninstallTargets(stateDir, runDir string) []string {
	stateDir = filepath.Clean(stateDir)
	runDir = filepath.Clean(runDir)
	switch {
	case stateDir == "" || stateDir == ".":
		return []string{runDir}
	case runDir == "" || runDir == ".":
		return []string{stateDir}
	case runDir == stateDir || strings.HasPrefix(runDir, stateDir+string(filepath.Separator)):
		return []string{stateDir}
	default:
		return []string{stateDir, runDir}
	}
}

// uninstallRemoveAll 删除一个目录，带最低限度的安全阀。
//
// --purge 会把路径交给 RemoveAll，而路径来自命令行；写错一个字符就可能指向
// 家目录或根。这里挡住几个「删了一定出事」的目标，宁可不删也不要删错。
func uninstallRemoveAll(path string) error {
	clean := filepath.Clean(path)
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return fmt.Errorf("拒绝删除 %q：这不像一个网关状态目录", path)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && clean == filepath.Clean(home) {
		return fmt.Errorf("拒绝删除 %q：那是家目录本身", clean)
	}
	if err := os.RemoveAll(clean); err != nil {
		return err
	}
	return nil
}

// uninstallTail 打印「还剩下什么」。
func uninstallTail() {
	fmt.Println()
	if exe, err := os.Executable(); err == nil {
		fmt.Printf("dsh-gate 可执行文件未删除：%s\n", exe)
	}
	if copyPath := filepath.Join(defaultBinDir, serviceName); fileExists(copyPath) {
		fmt.Printf("PATH 里还有安装时拷进去的那一份：%s\n", copyPath)
		fmt.Println("  不再需要时删掉它（rm 掉即可），否则敲 dsh-gate 仍会启动这套程序。")
	} else {
		fmt.Println("  不再需要时手工删掉它（以及 PATH 里的软链），或按当初的安装方式卸载。")
	}
	fmt.Println("dsh 自己的数据（~/.dsh）不属于 dsh-gate，未做改动。")
	fmt.Println("卸载完成。重装用 dsh-gate setup，或参考仓库 README.md。")
}
