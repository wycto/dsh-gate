package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/registry"
)

// resolveRunDir 决定运行登记目录。
//
// 默认固定在 ~/.dsh-gate/run，**不跟随 --state-dir**：--state-dir 管的是单个
// 实例的账号表，而「本机现在跑着哪些网关」是机器级的事实，固定下来才能让
// status / stop / restart 不必先知道实例配置就能找上门。
func resolveRunDir(override string) registry.Dir {
	if strings.TrimSpace(override) != "" {
		return registry.Dir(override).Expand()
	}
	return registry.DefaultDir()
}

func spawnDetached(exe string, argv []string, logDir, name string) (int, error) {
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, fmt.Errorf("创建日志目录失败: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, name+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("打开日志文件失败: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, argv[1:]...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	applyDetach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动失败: %w", err)
	}
	pid := cmd.Process.Pid
	// 不 Wait：子进程要活得比这条命令久得多；Wait 一返回它就退出或变僵尸了。
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

func waitListening(addr string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if listening(addr, 300*time.Millisecond) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// registerRunDirFlag 注册 --run-dir 选项，返回读取函数。
func registerRunDirFlag(fs *flag.FlagSet) func() registry.Dir {
	value := fs.String("run-dir", string(registry.DefaultDir()),
		"运行登记目录：status / stop / restart 从这里找正在运行的实例")
	return func() registry.Dir { return resolveRunDir(*value) }
}

// logDirFor 返回某个实例的日志目录。
//
// 必须 Expand：runDir 可能带 `~`（默认就是 ~/.dsh-gate/run），而 registry.Dir 的
// Path / Save 内部都会自己展开，唯独这里原来是裸 Join——于是日志落进一个字面量
// 的 `~` 目录，而命令行提示里印的是展开后的路径，用户照着找永远找不到。
func logDirFor(runDir registry.Dir) string {
	return filepath.Join(string(runDir.Expand()), "logs")
}
