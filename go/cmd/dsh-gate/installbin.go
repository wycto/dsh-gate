package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// defaultBinDir 是命令拷贝的默认落点。
//
// 选 /usr/local/bin 是因为它在几乎所有发行版与 macOS 的默认 PATH 里，且
// 语义就是「本地管理员装的软件」。Windows 不走这条路（见 installSelfToBinDir）。
const defaultBinDir = "/usr/local/bin"

// errBinDirUnsupported 表示当前平台不做命令拷贝。
var errBinDirUnsupported = errors.New("当前平台不在 PATH 目录里安装命令拷贝")

// installSelfToBinDir 把当前可执行文件拷到 dir 下，返回落点与是否发生了改动。
//
// 为什么是「拷一份」而不是软链：用户可能把 dsh-gate 放在
// /www/wwwroot/dsh-gate 这种随时会被清理的部署目录里；拷到 PATH 目录之后，
// 删掉原目录也不影响命令可用（service install 会把拷贝后的路径写进服务单元）。
// 代价是升级时要再跑一次安装——所以内容一致时这里什么都不做，只报「已是最新」。
//
// 任何一步失败都不留半成品：先写临时文件、设好权限、再原子改名。
func installSelfToBinDir(dir string) (string, bool, error) {
	if dir == "" {
		return "", false, nil
	}
	if runtime.GOOS == "windows" {
		return "", false, errBinDirUnsupported
	}

	exe, err := os.Executable()
	if err != nil {
		return "", false, fmt.Errorf("无法确定自身路径: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	target := filepath.Join(dir, serviceName)
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
		return "", false, fmt.Errorf("安装到 %s 失败: %w", target, err)
	}
	return target, true, nil
}

// printInstallSelfResult 执行拷贝并把结果打出来，返回最终该用的可执行文件路径。
//
// 拷不过去（没有写权限、目标只读…）不算失败：当前目录里的 ./dsh-gate 照样能用，
// 这里只提示一句，绝不拦住启动或装服务。
func printInstallSelfResult(dir string) string {
	path, changed, err := installSelfToBinDir(dir)
	switch {
	case errors.Is(err, errBinDirUnsupported):
		return ""
	case err != nil:
		fmt.Fprintf(os.Stderr, "提示：未能把 dsh-gate 放进 %s：%v\n", dir, err)
		if os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "  那是需要写权限的目录：可以 sudo 重跑，或用 --bin-dir 换一个位置。")
		}
		return ""
	case path == "":
		return ""
	case changed:
		fmt.Printf("命令已安装 %s（可直接敲 dsh-gate）\n", path)
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
