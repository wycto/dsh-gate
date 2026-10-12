package dshproc

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsurePnpmKeepsSystemPnpm 用户自己装过 pnpm 时不该动任何东西。
func TestEnsurePnpmKeepsSystemPnpm(t *testing.T) {
	bin := t.TempDir()
	writeExec(t, bin, "pnpm", "#!/bin/sh\necho 9.9.9\n")
	t.Setenv("PATH", bin)

	dir, err := EnsurePnpm(context.Background(), slog.New(slog.DiscardHandler), t.TempDir(), "")
	if err != nil {
		t.Fatalf("系统已有 pnpm 时不该报错: %v", err)
	}
	if dir != "" {
		t.Errorf("系统已有 pnpm 时不该返回自定义目录，得到 %q", dir)
	}
}

// TestEnsurePnpmReusesProvisioned 已经装好的那份要能认出来，别重复安装。
func TestEnsurePnpmReusesProvisioned(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // 模拟 PATH 里没有 npm/pnpm

	stateDir := t.TempDir()
	binDir := pnpmBinDir(filepath.Join(stateDir, pnpmDirName))
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, pnpmBinaryName()), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("准备 pnpm 失败: %v", err)
	}

	got, err := EnsurePnpm(context.Background(), slog.New(slog.DiscardHandler), stateDir, "")
	if err != nil {
		t.Fatalf("复用已装 pnpm 失败: %v", err)
	}
	if got != binDir {
		t.Errorf("bin 目录 = %q，期望 %q", got, binDir)
	}
}

// TestEnsurePnpmWithoutNpm 连 npm 都没有时要给出可执行的出路，而不是死等超时。
func TestEnsurePnpmWithoutNpm(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := EnsurePnpm(context.Background(), slog.New(slog.DiscardHandler), t.TempDir(), "")
	if err == nil {
		t.Fatal("既没有 pnpm 也没有 npm 时应当报错")
	}
	if !strings.Contains(err.Error(), "npm") || !strings.Contains(err.Error(), "Node.js") {
		t.Errorf("错误应指路 npm 与 Node.js，实际: %v", err)
	}
}

// TestEnsurePnpmRealInstall 真装一次 pnpm，并用它跑 DSH 插件管理器那条命令。
//
// 这条路径对应的真实故障：干净的服务器上 Node 有了、点「添加插件」却是
// `spawn pnpm ENOENT`。所以这里不只验证 pnpm 能装，还照着 DSH 的调用形态
// 跑一遍 `pnpm view <包> name version --json`。
//
// 默认跳过（要联网）：DSH_GATE_TEST_PNPM=1 时才跑。
func TestEnsurePnpmRealInstall(t *testing.T) {
	if os.Getenv("DSH_GATE_TEST_PNPM") == "" {
		t.Skip("设置 DSH_GATE_TEST_PNPM=1 才做真实安装测试")
	}
	// 只留 npm 所在的目录 + 系统基础目录：前者保证「系统里没有 pnpm」，
	// 后者是 npm 自己能跑起来的前提（它会 spawn sh）。
	npmPath, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("本机没有 npm，跳过")
	}
	t.Setenv("PATH", filepath.Dir(npmPath)+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")

	stateDir := t.TempDir()
	binDir, err := EnsurePnpm(context.Background(), slog.Default(), stateDir, RegistryMirror)
	if err != nil {
		t.Fatalf("安装 pnpm 失败: %v", err)
	}
	if binDir == "" {
		t.Fatal("应当返回自备 pnpm 的 bin 目录")
	}

	cmd := exec.Command(filepath.Join(binDir, pnpmBinaryName()),
		"view", "dsh-dock", "name", "version", "--json", "--registry="+RegistryMirror)
	cmd.Env = os.Environ()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("用自备 pnpm 查询插件包失败: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "dsh-dock") {
		t.Errorf("查询结果不像包元数据: %s", output)
	}
}
