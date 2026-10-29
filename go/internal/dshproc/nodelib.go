package dshproc

// 自备 Node.js 缺系统库时的自动补装。
//
// 为什么会有这件事：官方 Node 的 Linux 二进制（glibc 版）动态链接了
// libatomic.so.1 等系统库，而**最小安装的 Debian/Ubuntu 镜像里没有它**。
// 实测（ubuntu:24.04 容器，干净无 Node）：下载与解包全部成功，一执行就
//   node: error while loading shared libraries: libatomic.so.1: cannot open
//   shared object file
// 于是「网关替你装好 Node」在最小系统上其实是半截的。
//
// 这里只做一件窄事：认出缺哪个库 → 找到发行版的包管理器 → 装上对应的包 →
// 让调用方重试冒烟。装不上/没有包管理器就如实报错，不猜、不静默。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// libraryPackages 把「缺的 .so」映射到各发行版里的包名。
//
// 只列 Node.js 官方二进制真正会缺、且实测过的几个；没命中的库不猜——宁可
// 报错让人看见，也不要往人家机器上装一个不相干的包。
var libraryPackages = map[string]string{
	"libatomic.so.1": "libatomic1",
	"libstdc++.so.6": "libstdc++6",
	"libgcc_s.so.1":  "libgcc-s1",
}

// installTimeout 是装系统库的上限：apt 在小机器上可能要几十秒。
const installTimeout = 3 * time.Minute

// installMissingLibrary 用发行版的包管理器装上缺的库，返回是否装成功。
//
// 需要 root；非 root 或没有包管理器时返回错误说明，由调用方给出人工指引。
func installMissingLibrary(ctx context.Context, log *slog.Logger, library string) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pkg, ok := libraryPackages[library]
	if !ok {
		return fmt.Errorf("不知道 %s 属于哪个包，请手工安装它（例如 apt-get install -y libatomic1）", library)
	}
	manager, args, err := packageManagerInstall(pkg)
	if err != nil {
		return err
	}

	log.Info("自备的 Node.js 缺系统库，正在用系统包管理器补上",
		"library", library, "package", pkg, "command", manager+" "+strings.Join(args, " "))

	installCtx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	cmd := exec.CommandContext(installCtx, manager, args...)
	applyProbeAttrs(cmd)
	// 关掉一切交互：这一步发生在网关启动流程里，没有终端可以回答问题。
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if installCtx.Err() != nil {
			return fmt.Errorf("安装 %s 超时（%s）；请手工执行：%s %s",
				pkg, installTimeout, manager, strings.Join(args, " "))
		}
		return fmt.Errorf("安装 %s 失败：%w；请手工执行：%s %s。原始输出：%s",
			pkg, err, manager, strings.Join(args, " "), tailLines(string(output), 3))
	}
	log.Info("系统库已补上", "library", library, "package", pkg)
	return nil
}

// packageManagerInstall 给出「装一个包」的命令。
//
// 只支持有把握的几家：同一套参数在 apt 与 dnf/yum 上一致，apk 需要另外的写法
// （而且 Alpine 走的是 musl，官方 Node 二进制本来就跑不了，另有拦截）。
func packageManagerInstall(pkg string) (string, []string, error) {
	if os.Geteuid() != 0 {
		return "", nil, fmt.Errorf("补装系统库需要 root；请用 sudo 重跑，或手工安装 %s", pkg)
	}
	switch {
	case haveExecutable("apt-get"):
		return "apt-get", []string{"install", "-y", pkg}, nil
	case haveExecutable("dnf"):
		return "dnf", []string{"install", "-y", pkg}, nil
	case haveExecutable("yum"):
		return "yum", []string{"install", "-y", pkg}, nil
	case haveExecutable("zypper"):
		return "zypper", []string{"--non-interactive", "install", pkg}, nil
	default:
		return "", nil, fmt.Errorf("找不到可用的包管理器，请手工安装 %s（例如 apt-get install -y %s）", pkg, pkg)
	}
}

// haveExecutable 判断 PATH 里有没有这个命令。
func haveExecutable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// tailLines 取输出末尾几行放进错误里（包管理器的输出很长，前面全是进度）。
func tailLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.TrimSpace(strings.Join(lines, " | "))
}
