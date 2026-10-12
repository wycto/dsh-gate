package dshproc

// pnpm 的自备：dsh 的插件安装器走的是 pnpm，而 Node.js 自带的只有 npm/npx。
//
// 实测（干净的服务器 + 网关自备的 Node.js）：在 DSH 页面上点「添加插件」，
// 报的是 `spawn pnpm ENOENT`——Node 有了，pnpm 还是没有。
//
// 装法刻意不碰系统：用 npm 把 pnpm 装到 <状态目录>/pnpm 这个独立前缀下，
// 再把它的 bin 目录前置进 PATH。于是：
//
//   - 系统里已经有 pnpm（用户自己装的）时什么都不做；
//   - 用系统 Node 还是网关自备的 Node 都能装（都靠 PATH 里的 npm）；
//   - 不写全局目录、不需要 root，卸载时随 `uninstall --purge` 一起消失。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// pnpmDirName 是状态目录里存放 pnpm 前缀的那一层。
const pnpmDirName = "pnpm"

// pnpmInstallTimeout 是安装 pnpm 的上限：几十兆的包 + 依赖，慢网络也够。
const pnpmInstallTimeout = 10 * time.Minute

// EnsurePnpm 保证 PATH 里有 pnpm，返回需要前置进 PATH 的目录（没有动作时为空）。
//
// registry 是拉包用的 npm 源（跟 dsh 用同一个，留空表示 npm 默认）。
func EnsurePnpm(ctx context.Context, log *slog.Logger, stateDir, registry string) (string, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// 用户自己装过 pnpm 就用他的，别抢。
	if lookPathAny("pnpm", "pnpm.cmd") != "" {
		return "", nil
	}
	if stateDir == "" {
		return "", errors.New("状态目录为空，无法存放自备的 pnpm")
	}
	prefix := filepath.Join(stateDir, pnpmDirName)
	binDir := pnpmBinDir(prefix)
	if binDir != "" {
		if _, err := os.Stat(filepath.Join(binDir, pnpmBinaryName())); err == nil {
			log.Info("使用网关自备的 pnpm（dsh 装插件用）", "bin", binDir)
			return binDir, nil
		}
	}

	npm := lookPathAny("npm", "npm.cmd")
	if npm == "" {
		return "", errors.New("找不到 npm，无法为 dsh 准备 pnpm（插件安装需要它）；" +
			"请安装 Node.js（自带 npm），或用 --dsh-cmd 指定启动命令")
	}

	log.Info("正在安装 pnpm 到网关状态目录（dsh 的插件安装器需要它，不改系统）", "dir", prefix, "registry", registry)
	installCtx, cancel := context.WithTimeout(ctx, pnpmInstallTimeout)
	defer cancel()

	cmd := exec.CommandContext(installCtx, npm, "install", "--global", "pnpm", "--prefix", prefix)
	cmd.Env = npmEnv(os.Environ(), registry)
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if installCtx.Err() != nil {
			return "", fmt.Errorf("安装 pnpm 超时（%s）：网络或 npm 源太慢；也可用 --npm-registry 换一个源。原始输出：%s",
				pnpmInstallTimeout, tailText(text, 200))
		}
		return "", fmt.Errorf("安装 pnpm 失败：%w。原始输出：%s", err, tailText(text, 200))
	}

	if binDir == "" {
		return "", errors.New("当前平台不支持把 pnpm 装到独立前缀，请手工安装 pnpm")
	}
	if _, err := os.Stat(filepath.Join(binDir, pnpmBinaryName())); err != nil {
		return "", fmt.Errorf("pnpm 安装完成但没找到可执行文件（%s）：%s",
			filepath.Join(binDir, pnpmBinaryName()), tailText(text, 200))
	}
	log.Info("pnpm 已就绪（dsh 的插件安装器会用它）", "dir", binDir)
	return binDir, nil
}

// pnpmBinDir 返回独立前缀下放可执行文件的那一层。
//
// npm 的全局前缀布局两端不同：类 Unix 是 <prefix>/bin，Windows 直接放
// <prefix>（.cmd shim 与 node_modules 同级）。
func pnpmBinDir(prefix string) string {
	if prefix == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		return prefix
	}
	return filepath.Join(prefix, "bin")
}

// pnpmBinaryName 是 pnpm 可执行文件的名字。
func pnpmBinaryName() string {
	if runtime.GOOS == "windows" {
		return "pnpm.cmd"
	}
	return "pnpm"
}
