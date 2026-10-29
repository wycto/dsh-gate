package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// defaultBinDir 是 Linux / macOS 下命令拷贝的默认落点。
//
// 选 /usr/local/bin 是因为它在几乎所有发行版与 macOS 的默认 PATH 里，且
// 语义就是「本地管理员装的软件」。Windows 的默认落点不同（见 binDirDefault）。
const defaultBinDir = "/usr/local/bin"

// binDirDefault 返回当前平台的默认安装目录；空串表示「这个平台不问、也不拷」。
//
// Linux / macOS 用 /usr/local/bin：它在默认 PATH 里，装完就能直接敲命令。
//
// Windows 没有这样的目录：System32 要管理员权限，Program Files 也不是普通用户
// 能写的。而计划任务里的命令必须指向一个不会被清理的位置——用户从 zip 解出来
// 的目录、甚至临时目录随时会消失，开机自启就跟着失效（这正是用户从解压目录
// 装完、删掉目录后自启打不开的情形）。所以这里选 %LOCALAPPDATA%\dsh-gate\bin：
// 当前用户可写、不会被系统清理、卸载时也好找。它不在 PATH 里，安装结束时会
// 提示一句「想直接敲 dsh-gate 就把它加进用户 PATH」。
func binDirDefault() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			// 极少数环境没有 LOCALAPPDATA（比如某些服务账号），退到 %AppData%。
			if dir, err := os.UserConfigDir(); err == nil {
				base = dir
			}
		}
		if base == "" {
			return ""
		}
		return filepath.Join(base, serviceName, "bin")
	}
	return defaultBinDir
}

// binTarget 返回拷贝到 dir 之后的文件路径。Windows 上可执行文件要带 .exe。
func binTarget(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, serviceName+".exe")
	}
	return filepath.Join(dir, serviceName)
}

// installSelfToBinDir 把当前可执行文件拷到 dir 下，返回落点与是否发生了改动。
//
// 为什么是「拷一份」而不是软链：用户可能把 dsh-gate 放在
// /www/wwwroot/dsh-gate 这种随时会被清理的部署目录里；拷到安装目录之后，
// 删掉原目录也不影响命令可用（service install 会把拷贝后的路径写进服务单元
// 或 Windows 计划任务）。
// 代价是升级时要再跑一次安装——所以内容一致时这里什么都不做，只报「已是最新」。
//
// 任何一步失败都不留半成品：先写临时文件、设好权限、再原子改名。
func installSelfToBinDir(dir string) (string, bool, error) {
	if dir == "" {
		return "", false, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", false, fmt.Errorf("无法确定自身路径: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	target := binTarget(dir)
	// 已经跑在目标位置（例如服务单元就指着它）就不用自己拷自己。
	if samePath(exe, target) {
		return target, false, nil
	}

	same, err := sameContent(exe, target)
	if err != nil {
		return "", false, err
	}
	if same {
		return target, false, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("创建 %s 失败: %w", dir, err)
	}
	source, err := os.Open(exe)
	if err != nil {
		return "", false, fmt.Errorf("打开自身失败: %w", err)
	}
	defer source.Close()

	tmp, err := os.CreateTemp(dir, ".dsh-gate-*")
	if err != nil {
		return "", false, fmt.Errorf("在 %s 里创建临时文件失败: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, source); err != nil {
		tmp.Close()
		return "", false, fmt.Errorf("写入 %s 失败: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return "", false, fmt.Errorf("设置权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", false, fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		// Windows 不允许覆盖正在运行的可执行文件。目标很可能就是当前管理端在跑的
		// 那一份（升级 / 重装场景）：先请它正常停下，再试一次。安装流程随后会把
		// 它重新启动，所以这里停一下不影响用户。
		if stopErr := stopRunningPortal(); stopErr == nil {
			if err = os.Rename(tmpPath, target); err == nil {
				return target, true, nil
			}
		}
		return "", false, fmt.Errorf("安装到 %s 失败: %w", target, err)
	}
	return target, true, nil
}

// stopRunningPortal 停掉正在跑的管理端，好让安装覆盖它自己的可执行文件。
//
// 返回 nil 表示「现在没有在跑的，或者已经停下了」——两种都算可以继续。
func stopRunningPortal() error {
	runDir := resolveRunDir("")
	record, ok, err := runDir.Load(portalRecordName)
	if err != nil {
		return err
	}
	if !ok || !record.Alive() {
		return nil
	}
	if err := stopRecord(record, 10*time.Second); err != nil {
		return err
	}
	_ = runDir.Remove(portalRecordName)
	return nil
}

// printInstallSelfResult 执行拷贝并把结果打出来，返回最终该用的可执行文件路径。
//
// 拷不过去（没有写权限、目标只读…）不算失败：当前目录里的 ./dsh-gate 照样能用，
// 这里只提示一句，绝不拦住启动或装服务。
func printInstallSelfResult(dir string) string {
	path, changed, err := installSelfToBinDir(dir)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "提示：未能把 dsh-gate 放进 %s：%v\n", dir, err)
		if runtime.GOOS != "windows" && os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "  那是需要写权限的目录：可以 sudo 重跑，或用 --bin-dir 换一个位置。")
		}
		return ""
	case path == "":
		return ""
	case changed:
		fmt.Printf("命令已安装 %s\n", path)
		if runtime.GOOS == "windows" {
			fmt.Printf("  提示：这个目录不在 PATH 里；想直接敲 dsh-gate，把它加进用户环境变量 PATH 即可。\n")
		}
	default:
		fmt.Printf("命令已是最新 %s\n", path)
	}
	return path
}

// samePath 比较两个路径是否指向同一个文件。
func samePath(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

// sameContent 比较两个文件内容是否一致；目标不存在时返回 false。
//
// 先比大小再比哈希：二进制十几兆，绝大多数情况下「大小不同」就已经给出答案。
func sameContent(source, target string) (bool, error) {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return false, fmt.Errorf("读取 %s 失败: %w", source, err)
	}
	targetInfo, err := os.Stat(target)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取 %s 失败: %w", target, err)
	}
	if sourceInfo.Size() != targetInfo.Size() {
		return false, nil
	}
	sourceSum, err := fileSum(source)
	if err != nil {
		return false, err
	}
	targetSum, err := fileSum(target)
	if err != nil {
		return false, err
	}
	return bytes.Equal(sourceSum, targetSum), nil
}

// fileSum 算一个文件的 sha256。
func fileSum(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}
