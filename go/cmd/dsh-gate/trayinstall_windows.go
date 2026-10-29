//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 托盘是**另一个二进制**，不是同一份可执行文件加个开关：GUI 子系统（链接期
// `-H=windowsgui`）决定「系统要不要给它分配控制台」，而这件事只能在编译时定，
// 运行时改不了。所以 `--tray` 装的是 build.sh 一起产出的 `dsh-gate-tray.exe`。

// exeExt 是 Windows 可执行文件后缀。本文件只在 Windows 上编译，所以直接写死。
const exeExt = ".exe"

// canonicalTrayName 返回托盘二进制的标准文件名（不带目录）。
func canonicalTrayName() string {
	return trayBinaryBase(serviceName) + exeExt
}

// trayBinaryBase 把控制台程序名换成对应的托盘程序名。
//
// 为什么要跟着实际文件名走而不写死 `dsh-gate-tray.exe`：用户手上的文件可能带版本
// 号（发行版附件里很常见），此时同目录里的托盘也叫同名后缀的那一个。按前缀替换
// 才找得到它。
func trayBinaryBase(name string) string {
	const prefix = "dsh-gate"
	stem, ext := name, ""
	if lower := strings.ToLower(name); strings.HasSuffix(lower, exeExt) {
		stem, ext = name[:len(name)-len(exeExt)], exeExt
	}
	if strings.HasPrefix(strings.ToLower(stem), prefix) {
		return prefix + "-tray" + stem[len(prefix):] + ext
	}
	return prefix + "-tray" + ext
}

// installTraySiblingToBinDir 找到与 consoleBinary 配套的托盘二进制，必要时拷进
// binDir，返回计划任务该用的绝对路径。
//
// 找不到就直接失败而不是退化成启动器形态：用户点 `--tray` 要的就是「没有那个终端
// 窗口」，悄悄装成有窗口的版本，他会以为修复没生效。装了个跑不起来的任务比装不上
// 更糟，所以这里不兜底。
func installTraySiblingToBinDir(binDir, consoleBinary string) (string, error) {
	source, err := findTraySibling(consoleBinary)
	if err != nil {
		return "", err
	}
	if binDir == "" {
		return source, nil
	}
	target := filepath.Join(binDir, filepath.Base(source))
	if samePath(source, target) {
		return target, nil
	}
	same, err := sameContent(source, target)
	if err != nil {
		return "", err
	}
	if same {
		return target, nil
	}
	if err := installFileTo(source, target); err != nil {
		return "", err
	}
	fmt.Printf("托盘程序已安装 %s\n", target)
	return target, nil
}

// findTraySibling 在控制台二进制与当前运行文件所在目录里找托盘二进制。
//
// 两个目录都要找：`service install` 在写任务之前已经把控制台程序拷进 binDir，
// 于是 opts.Binary 指向 binDir；托盘若是随 zip 解在别处的，只查 binDir 就找不到。
func findTraySibling(consoleBinary string) (string, error) {
	var searched []string
	for _, dir := range uniqueStrings([]string{filepath.Dir(consoleBinary), executableDir()}) {
		if dir == "" {
			continue
		}
		for _, name := range trayCandidateNames(consoleBinary) {
			candidate := filepath.Join(dir, name)
			searched = append(searched, candidate)
			if fileExists(candidate) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("找不到托盘程序 %s（已在 %s 里找过）。\n"+
		"它和 dsh-gate 是两个二进制：请用同一次构建产出的那一套文件，或去掉 --tray 改装启动器形态（登录时终端窗口闪一下，之后进程独立）",
		canonicalTrayName(), strings.Join(searched, ", "))
}

// trayCandidateNames 返回要试的托盘文件名，先试与 consoleBinary 同名前缀的那一个。
func trayCandidateNames(consoleBinary string) []string {
	derived := trayBinaryBase(filepath.Base(consoleBinary))
	if strings.EqualFold(derived, canonicalTrayName()) {
		return []string{derived}
	}
	return []string{derived, canonicalTrayName()}
}

// executableDir 返回当前运行文件所在目录，取不到时返回空串。
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

// installFileTo 把 source 拷到 target：先写临时文件再原子改名，失败不留半成品。
//
// 改名失败时不重试「停掉正在跑的管理端」那套：这里可能是托盘自己正在跑，而它没有
// 停机端点可叫。直接报错，让用户从托盘菜单里退出——那才是它自己的停法。
func installFileTo(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", filepath.Dir(target), err)
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", source, err)
	}
	defer file.Close()

	tmp, err := os.CreateTemp(filepath.Dir(target), ".dsh-gate-tray-*")
	if err != nil {
		return fmt.Errorf("在 %s 里创建临时文件失败: %w", filepath.Dir(target), err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		return fmt.Errorf("写入 %s 失败: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("设置权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("安装到 %s 失败: %w（托盘可能正在运行：先在托盘图标上右键「退出托盘」，再重跑一次）", target, err)
	}
	return nil
}

// uniqueStrings 去掉重复项，保持原有顺序。
func uniqueStrings(values []string) []string {
	var out []string
	for _, value := range values {
		if !containsString(out, value) {
			out = append(out, value)
		}
	}
	return out
}
