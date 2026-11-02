//go:build linux

package trust

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// install 在 Linux 上把 CA 放进发行版的证书目录并刷新。
//
// Linux 没有一个统一的「加信任」接口，只能按发行版挑：Debian 系用
// update-ca-certificates，RHEL 系用 update-ca-trust。两条都需要写
// /usr/local/share/ca-certificates（或 /etc/pki）这类系统目录，因此要 sudo。
//
// 没有 sudo 或没有这些命令时**不失败**：返回一条让用户自己执行的命令。
// 服务器上常见「用普通用户装、之后才 sudo」的场景，替用户提权既做不到也不合适。
func install(opts Options) Status {
	dir, refresh, ok := linuxPlan()
	if !ok {
		return Status{
			Attempted: true,
			Message:   "这个发行版的证书刷新方式不认识，请手动执行下面的命令（或参照发行版文档）。",
		}
	}

	target := filepath.Join(dir, "dsh-gate-ca.crt")
	// 先看能不能写：写不进去就直接给出 sudo 命令，不要执行到一半才失败。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Status{
			Attempted: true,
			Message:   "没有权限写入 " + dir + "，请用 sudo 执行下面的命令。",
		}
	}
	if err := copyFile(opts.CACert, target); err != nil {
		return Status{
			Attempted: true,
			Message:   "复制证书到 " + dir + " 失败（多半需要 sudo），请手动执行下面的命令。",
		}
	}

	cmd := exec.Command("sh", "-c", refresh)
	output, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(output))
		if text == "" {
			text = err.Error()
		}
		return Status{
			Attempted: true,
			Message:   "证书已放进 " + dir + "，但刷新失败：" + text,
		}
	}
	return Status{
		Attempted: true,
		Trusted:   true,
		Message:   "已装进系统信任库（" + dir + "）并刷新。",
	}
}

// linuxPlan 返回证书目录与刷新命令。
//
// 判据是「目录存在」而不是读 /etc/os-release：目录存在就说明这套机制在这个
// 发行版上是真的被用的，比按发行版名字猜更可靠。
func linuxPlan() (dir, refresh string, ok bool) {
	type candidate struct {
		dir     string
		refresh string
	}
	candidates := []candidate{
		// Debian / Ubuntu 及其衍生版
		{"/usr/local/share/ca-certificates", "update-ca-certificates"},
		// RHEL / Fedora / CentOS
		{"/etc/pki/ca-trust/source/anchors", "update-ca-trust extract"},
		// Arch
		{"/etc/ca-certificates/trust-source/anchors", "trust extract-compat"},
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Dir(c.dir)); err == nil {
			return c.dir, c.refresh, true
		}
	}
	return "", "", false
}

// copyFile 把源文件复制到目标路径，权限 0644。
func copyFile(src, dst string) error {
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, content, 0o644)
}
