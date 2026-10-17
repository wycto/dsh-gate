package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

// controlAddr 算出访问控制端点的地址。
//
// 网关绑定 0.0.0.0 时要显式换成回环地址：控制端点只接受回环来源，按通配地址
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

// stopRecord 通过控制端点停掉一个实例，返回是否真的停掉了。
//
// 走控制端点而不是「PID 文件 + 发信号」：信号要在每个平台各写一份，而 Windows
// 上根本没有 SIGTERM，只能退化成强杀，dsh 来不及收尾。走控制端点则三个平台
// 行为一致，被停的进程也知道自己是被请求停的，会按和 Ctrl-C 一样的流程收尾。
func stopRecord(record registry.Record, wait time.Duration) error {
	if !record.Alive() {
		return fmt.Errorf("端口 %d 没有在监听，实例应该已经停了", record.Port)
	}

	addr := controlAddr(record)
	client := newHTTPClient(3 * time.Second)
	response, err := client.Post("http://"+addr+"/__dsh_gate/shutdown", "text/plain", nil)
	if err != nil {
		return fmt.Errorf("停机请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("网关拒绝了停机请求（HTTP %d）", response.StatusCode)
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

// dshReleaseGrace 是旧网关退出后，额外留给它托管的 dsh 释放端口的时间。
//
// 网关退出时 dsh 通常已经跟着走了；这点宽限是给「网关在收尾超时后才退出」那种
// 边界情况——那时 SIGTERM 刚发出去，dsh 还要几百毫秒才松口。给多了会拖慢
// 「重启一个接管外部 dsh 的实例」（外部 dsh 根本不会退出），所以取小值。
const dshReleaseGrace = 3 * time.Second

// awaitStopped 等上一代实例**彻底消失**，再让调用方拉起新的。
//
// 为什么不能只看网关端口关没关：Instance.Shutdown 的顺序是「先断新请求，再收尾
// dsh」，而 dsh 的收尾要等它自己的宽限期（默认 10 秒，实测里能拖到 20 秒）。
// 端口一关就拉新网关的话，新网关会看到 3080 上还有一个 dsh，于是把它「接管」
// 过来；而接管的实例不由它看守（supervisor 的重启循环只盯自己拉起的孩子），
// dsh 紧接着一死，就再也没有人把它拉起来——页面永远停在「DSH 实例暂时不可用 /
// dial tcp 127.0.0.1:3080: connect: connection refused」。
//
// 接管外面自己起的 dsh 是正常用法（那种 dsh 不会因为网关退出而退出），所以
// dsh 端口这一步只是「给一点释放时间」，超时就放行，绝不拦。
func awaitStopped(record registry.Record, wait time.Duration) error {
	deadline := time.Now().Add(wait)

	// 第一步：等旧网关进程退出。它的退出意味着收尾流程已经走完。
	if record.PID > 0 {
		for processAlive(record.PID) {
			if time.Now().After(deadline) {
				return fmt.Errorf("旧进程（进程号 %d）在 %s 内没有退出，已放弃重启："+
					"它可能卡在收尾里。用 dsh-gate status 看看状态，或手动结束它再重试",
					record.PID, wait)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// 第二步：旧网关退出前后，它托管的 dsh 可能还在最后收尾，把端口让出来。
	if record.DSHPort <= 0 {
		return nil
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(record.DSHPort))
	release := time.Now().Add(dshReleaseGrace)
	if release.After(deadline) {
		release = deadline
	}
	for listening(addr, 200*time.Millisecond) {
		if time.Now().After(release) {
			// 还占着：那是外面自己起的 dsh，接管它是正常用法，放行。
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil
}

// launchRecord 按登记里保存的命令行把实例重新拉起。
func launchRecord(record registry.Record, logDir string) error {
	// 登记里存的是「当时怎么启动的」，但实例名是登记的身份，重启时要对齐，
	// 否则 --config 场景下会起出一个同名冲突的实例。
	argv := withName(record.Argv, record.Name)

	exe := record.Exe
	if exe == "" || !fileExists(exe) {
		if len(record.Argv) == 0 {
			return errors.New("运行记录里没有保存启动命令，无法自动重启；请手动重新运行一次")
		}
		exe = record.Argv[0] // 登记过期或程序被移动了，退回记录里的写法
	}
	_, err := spawnDetached(exe, argv, logDir, record.Name)
	return err
}

// spawnDetached 脱离终端地拉起一个网关进程，把输出写进日志文件，返回其进程号。
//
// 脱离是必须的：命令自己很快就要返回，终端随即交还给用户，子进程还挂在同一个
// 终端上的话，用户一关终端它就跟着没了。
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

// withName 让重新拉起时用的实例名与登记一致。
//
// **只替换、不追加**：命令行里原本没有 --name 时不补。补了会和 `--config` 打架
// （那种模式下实例名写在配置文件里，再从命令行塞一个就成了「混用了实例选项」），
// 而且本来就是多余的——没写 --name 说明当初用的是默认名，重启时照样解析成同一个。
func withName(argv []string, name string) []string {
	out := slices.Clone(argv)
	for i := 1; i < len(out); i++ {
		if out[i] == "--name" && i+1 < len(out) {
			out[i+1] = name
			return out
		}
		if strings.HasPrefix(out[i], "--name=") {
			out[i] = "--name=" + name
			return out
		}
	}
	return out
}

// waitListening 等端口重新被监听，用于确认重启真的起来了。
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
