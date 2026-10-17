// 三种安装方式与环境检测的测试。
//
// 这些测试不真的去下载 dsh：用假的可执行文件伪造 node / npx / pnpm / git，验证
// 「选择方式 → 检查前置 → 组装出正确的启动命令」这条逻辑。真正需要网络的那一段
// 交给 Linux 容器端到端验证（见 tmp/ 下的容器脚本）。
package dshproc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// prependPATH 把目录前置到 PATH（保留系统 PATH）。
//
// 不能直接 t.Setenv("PATH", dir)：伪造的 shell 脚本里要用 mkdir / chmod 这些
// 系统工具，把 PATH 换成只有一个目录会让它们全部找不到。
func prependPATH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestParseInstallMode 锁定三种方式的解析与默认值。
func TestParseInstallMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want InstallMode
	}{
		{"", InstallNpx},
		{"npx", InstallNpx},
		{"NPX", InstallNpx},
		{" npx ", InstallNpx},
		{"git", InstallGit},
		{"Git", InstallGit},
		{"pnpm", InstallPnpm},
	} {
		got, err := ParseInstallMode(tc.in)
		if err != nil {
			t.Errorf("ParseInstallMode(%q) 不该报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseInstallMode(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"rpm", "docker", "git2"} {
		if _, err := ParseInstallMode(bad); err == nil {
			t.Errorf("ParseInstallMode(%q) 应报错", bad)
		}
	}
}

// TestInstallModeValidAndLabel 锁定 Valid 与展示名（管理台下拉用 Label）。
func TestInstallModeValidAndLabel(t *testing.T) {
	for _, m := range []InstallMode{InstallNpx, InstallGit, InstallPnpm} {
		if !m.Valid() {
			t.Errorf("%q 应有效", m)
		}
		if m.Label() == "" {
			t.Errorf("%q 的 Label 不该为空", m)
		}
	}
	if InstallMode("rpm").Valid() {
		t.Error("未支持的方式不该有效")
	}
}

// TestInstallDSHRejectsUnknownMode 验证未知方式被挡在最前面。
func TestInstallDSHRejectsUnknownMode(t *testing.T) {
	_, err := InstallDSH(context.Background(), nil, InstallRequest{Mode: "rpm"})
	if err == nil {
		t.Fatal("未知安装方式应报错")
	}
	if !strings.Contains(err.Error(), "rpm") {
		t.Errorf("错误里应点出方式名，实际 %v", err)
	}
}

// TestInstallGitNeedsDir 验证 git 方式必须给安装目录。
func TestInstallGitNeedsDir(t *testing.T) {
	_, err := InstallDSH(context.Background(), nil, InstallRequest{Mode: InstallGit})
	if err == nil || !strings.Contains(err.Error(), "安装目录") {
		t.Fatalf("git 方式缺目录应报错并说明，实际 %v", err)
	}
}

// TestInstallPnpmNeedsDir 验证 pnpm 方式必须给安装目录。
func TestInstallPnpmNeedsDir(t *testing.T) {
	_, err := InstallDSH(context.Background(), nil, InstallRequest{Mode: InstallPnpm})
	if err == nil || !strings.Contains(err.Error(), "安装目录") {
		t.Fatalf("pnpm 方式缺目录应报错并说明，实际 %v", err)
	}
}

// TestInstallGitNeedsGit 验证缺 git 时的指引：这是 git 方式的第一道门槛，
// 错误必须说清「缺什么、怎么补、还能改用哪种方式」。
func TestInstallGitNeedsGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("伪造 PATH 依赖类 Unix 语义")
	}
	dir := t.TempDir()
	// 空 PATH：node / git / pnpm 全都找不到（保留系统 PATH 以便报错信息里
	// 的指引仍然可读）。
	t.Setenv("PATH", dir)

	_, err := InstallDSH(context.Background(), nil, InstallRequest{
		Mode: InstallGit, Dir: filepath.Join(dir, "install"),
	})
	if err == nil {
		t.Fatal("缺 git 时应报错")
	}
	msg := err.Error()
	for _, want := range []string{"git", "npx"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误里应提到 %q，实际：%s", want, msg)
		}
	}
}

// TestInstallPnpmNeedsPnpm 验证缺 pnpm 时给出安装命令与替代方式。
func TestInstallPnpmNeedsPnpm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("伪造 PATH 依赖类 Unix 语义")
	}
	dir := t.TempDir()
	// 只放一个假的 node，不放 pnpm：模拟「有 Node 没 pnpm」。
	writeExec(t, dir, "node", "echo v22.11.0\n")
	// 这里必须真的清空 PATH：系统上装着 pnpm 的话，泄漏进来就测不出「缺 pnpm」
	// 这条分支了。
	t.Setenv("PATH", dir)

	_, err := InstallDSH(context.Background(), nil, InstallRequest{
		Mode: InstallPnpm, Dir: filepath.Join(dir, "install"),
	})
	if err == nil {
		t.Fatal("缺 pnpm 时应报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pnpm") || !strings.Contains(msg, "npm install -g pnpm") {
		t.Errorf("错误里应给出补装办法，实际：%s", msg)
	}
}

// TestInstallPnpmProducesCommand 验证 pnpm 方式装完后返回的启动命令指向
// node_modules/.bin/dsh。
func TestInstallPnpmProducesCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 假的 pnpm：接受 `add <pkg> --dir <prefix>`，把可执行文件造出来。
	writeExec(t, binDir, "pnpm", `
if [ "$1" = "add" ]; then
  prefix=""
  shift
  while [ $# -gt 0 ]; do
    if [ "$1" = "--dir" ]; then prefix="$2"; shift 2; continue; fi
    shift
  done
  mkdir -p "$prefix/node_modules/.bin"
  printf '#!/bin/sh\necho 0.2.1\n' > "$prefix/node_modules/.bin/dsh"
  chmod +x "$prefix/node_modules/.bin/dsh"
  exit 0
fi
echo 11.28.5
exit 0
`)
	prependPATH(t, binDir)

	installDir := filepath.Join(dir, "install")
	result, err := InstallDSH(context.Background(), nil, InstallRequest{
		Mode: InstallPnpm, Dir: installDir,
	})
	if err != nil {
		t.Fatalf("pnpm 方式应成功: %v", err)
	}
	want := filepath.Join(installDir, "dsh", "node_modules", ".bin", "dsh")
	if result.Command != want {
		t.Errorf("启动命令应为 %q，实际 %q", want, result.Command)
	}
	if result.Mode != InstallPnpm {
		t.Errorf("方式应为 pnpm，实际 %q", result.Mode)
	}
	if result.Version != "0.2.1" {
		t.Errorf("版本应为 0.2.1，实际 %q", result.Version)
	}
}

// TestInstallGitProducesCommand 验证 git 方式的三步（clone / install / build）
// 都跑过，且启动命令用 node 直跑构建产物。
func TestInstallGitProducesCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 假的 git：`clone ... <dir>` 造出 .git 与 apps/cli/lib/bin.js。
	writeExec(t, binDir, "git", `
if [ "$1" = "clone" ]; then
  # 最后一个参数是目标目录
  for last; do :; done
  mkdir -p "$last/.git" "$last/apps/cli/lib"
  printf '#!/bin/sh\necho 0.2.1-alpha.2\n' > "$last/apps/cli/lib/bin.js"
  chmod +x "$last/apps/cli/lib/bin.js"
  exit 0
fi
echo "git version 2.43.0"
exit 0
`)
	// 假的 pnpm：`install` 与 `run build` 都成功。
	writeExec(t, binDir, "pnpm", `
echo "pnpm 11.28.5"
exit 0
`)
	// 假的 node：用于探测版本，也作为最终命令的一部分。
	writeExec(t, binDir, "node", `echo "v22.11.0"`)
	prependPATH(t, binDir)

	installDir := filepath.Join(dir, "install")
	result, err := InstallDSH(context.Background(), nil, InstallRequest{
		Mode: InstallGit, Dir: installDir,
	})
	if err != nil {
		t.Fatalf("git 方式应成功: %v", err)
	}
	// 入口文件必须被真的造出来，命令要指向它。
	entry := filepath.Join(installDir, "deepseek-harness", "apps", "cli", "lib", "bin.js")
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("构建产物应存在: %v", err)
	}
	if !strings.Contains(result.Command, entry) {
		t.Errorf("启动命令应指向 %q，实际 %q", entry, result.Command)
	}
	// lookPathAny 返回的是 PATH 里的裸名字（由 exec 自己解析），所以命令前缀
	// 是 "node"，不是绝对路径。
	if !strings.HasPrefix(result.Command, "node ") {
		t.Errorf("启动命令应以 node 开头，实际 %q", result.Command)
	}
	// Dir 是用户指定的安装根目录，不是 clone 出来的子目录：管理台会把它写回
	// 实例记录、下次重装读出来当 req.Dir，回填子目录会让每次重装都多嵌一层。
	if result.Dir != installDir {
		t.Errorf("安装目录应为用户指定的根目录 %q，实际 %q", installDir, result.Dir)
	}
}

// TestInstallGitIdempotentOnExistingRepo 验证重复安装不会把已有源码删掉重来
// （用户可能在里面改过东西）。
func TestInstallGitIdempotentOnExistingRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// git 一旦被调用 clone 就留下痕迹（用于断言「没再 clone」）。
	writeExec(t, binDir, "git", `
if [ "$1" = "clone" ]; then
  echo "CLONE-CALLED" >&2
  exit 1
fi
exit 0
`)
	writeExec(t, binDir, "pnpm", `exit 0`)
	writeExec(t, binDir, "node", `echo v22.11.0`)
	prependPATH(t, binDir)

	installDir := filepath.Join(dir, "install")
	repo := filepath.Join(installDir, "deepseek-harness")
	// 预置一个「已经 clone 过」的仓库（有 .git）与构建产物。
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "apps", "cli", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(repo, "apps", "cli", "lib", "bin.js")
	if err := os.WriteFile(entry, []byte("#!/bin/sh\necho 0.2.1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 用户改过的文件：不该被动。
	keep := filepath.Join(repo, "user-change.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := InstallDSH(context.Background(), nil, InstallRequest{
		Mode: InstallGit, Dir: installDir,
	})
	if err != nil {
		t.Fatalf("已有仓库时应成功（跳过 clone）: %v", err)
	}
	if !strings.Contains(result.Command, entry) {
		t.Errorf("命令应指向已有入口，实际 %q", result.Command)
	}
	// 用户改过的文件还在。
	if data, err := os.ReadFile(keep); err != nil || string(data) != "mine" {
		t.Errorf("已有源码不该被动，读到 %q err=%v", data, err)
	}
}

// TestDetectEnvReportsTools 验证环境检测如实报告各工具。
func TestDetectEnvReportsTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("伪造 PATH 依赖类 Unix 语义")
	}
	dir := t.TempDir()
	writeExec(t, dir, "node", `echo v22.11.0`)
	writeExec(t, dir, "npm", `echo 10.9.0`)
	writeExec(t, dir, "npx", `echo 10.9.0`)
	writeExec(t, dir, "git", `echo "git version 2.43.0"`)
	prependPATH(t, dir)

	report := DetectEnv(context.Background(), t.TempDir())

	if got := report.Tool("node"); !got.Found || got.Version != "v22.11.0" {
		t.Errorf("node 应被检测到且带版本，实际 %+v", got)
	}
	if got := report.Tool("git"); !got.Found {
		t.Errorf("git 应被检测到，实际 %+v", got)
	}
	// pnpm 不在这里断言：开发机上 PATH 里可能已经有 pnpm（例如 dsh 仓库的
	// node_modules/.bin），伪造环境无法保证隔离。它的存在与否不影响本测试要
	// 验证的「检测如实报告」这一点。
	// dsh 没放：应报缺失。
	if got := report.Tool("dsh"); got.Found {
		t.Errorf("dsh 没放却报存在，实际 %+v", got)
	}
	// 能力判断应与工具状态一致。
	if !report.CanNpx() {
		t.Error("有 node 应能用 npx 方式")
	}
	if !report.CanGit() {
		t.Error("有 node+git 应能用 git 方式")
	}
	if !report.CanPnpm() {
		t.Error("有 node 应能用 pnpm 方式（pnpm 可以现装）")
	}
	if report.Platform == "" {
		t.Error("平台标识不该为空")
	}
}

// TestDetectEnvWithoutTools 验证一台什么都没有的机器上报得清清楚楚，
// 而不是抛错（管理台据此把选项标成不可用）。
func TestDetectEnvWithoutTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("伪造 PATH 依赖类 Unix 语义")
	}
	dir := t.TempDir()
	// 真的把 PATH 清空：这个测试要的就是「一台什么都没有的机器」。
	t.Setenv("PATH", dir)

	report := DetectEnv(context.Background(), filepath.Join(dir, "state"))
	for _, name := range []string{"node", "npm", "npx", "pnpm", "git", "dsh"} {
		if got := report.Tool(name); got.Found {
			t.Errorf("%s 不该被检测到，实际 %+v", name, got)
		}
	}
	if report.CanNpx() || report.CanGit() || report.CanPnpm() {
		t.Error("什么工具都没有时三种方式都不该可用")
	}
}

// TestVersionHelpers 锁定版本解析的边界（探测输出的格式各工具都不一样）。
func TestVersionHelpers(t *testing.T) {
	for _, tc := range []struct{ in, num string }{
		{"v22.11.0", "22.11.0"},
		{"10.9.0", "10.9.0"},
		{"git version 2.43.0", "2.43.0"},
		{"pnpm 11.28.5", "11.28.5"},
		{"0.2.1-alpha.2", "0.2.1"},
		{"v20", "20"},
	} {
		if got := versionNumber(tc.in); got != tc.num {
			t.Errorf("versionNumber(%q) = %q，期望 %q", tc.in, got, tc.num)
		}
	}
	for _, tc := range []struct {
		in    string
		major int
	}{
		{"v22.11.0", 22},
		{"20.0.0", 20},
		{"", 0},
		{"abc", 0},
	} {
		if got := majorVersion(tc.in); got != tc.major {
			t.Errorf("majorVersion(%q) = %d，期望 %d", tc.in, got, tc.major)
		}
	}
}

// TestFirstLine 锁定版本探测只取第一行（有的工具会多打几行）。
func TestFirstLine(t *testing.T) {
	if got := firstLine("v22.11.0\nnode 的其他输出"); got != "v22.11.0" {
		t.Errorf("firstLine 应只取第一行，实际 %q", got)
	}
	if got := firstLine("  单行  "); got != "单行" {
		t.Errorf("firstLine 应去掉空白，实际 %q", got)
	}
}

// TestInstallNpxUsesSelfProvidedNode 锁定一个真实踩过的坑：
//
// 机器上没有任何系统级 Node（npx 自然也没有），但网关已经把官方运行时下载到了
// 状态目录。此时安装必须能用**自备运行时里**的 npx，而不是报「找不到 npx」——
// 早先的 EnsureDSH 只看进程自己的 PATH，会得出「明明刚下好 Node 却说没装」，
// 在容器与无 Node 的服务器上必然踩到。
func TestInstallNpxUsesSelfProvidedNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	// 自备运行时的 bin 目录：里面只有 npx（模拟刚下载好的 Node 发行版）。
	selfBin := filepath.Join(dir, "self-node", "bin")
	if err := os.MkdirAll(selfBin, 0o755); err != nil {
		t.Fatal(err)
	}
	// 假 npx：冒烟调用是 `npx --yes @deepseek-ai/dsh web --help`，要打出能被
	// smokeOK 认出来的帮助文本。
	writeExec(t, selfBin, "npx", `
# 调用形态：npx --yes @deepseek-ai/dsh web --help（$3=web $4=--help）
if [ "$3" = "web" ] && [ "$4" = "--help" ]; then
  echo "Usage: dsh web [--port <port>] [--host <host>]"
  echo "  --no-open   do not open the browser"
  exit 0
fi
exit 0
`)
	writeExec(t, selfBin, "node", `echo v26.12.0`)

	// 进程 PATH 里没有 npx（也没有 node）：这正是容器里的情况。
	t.Setenv("PATH", filepath.Join(dir, "empty"))

	result, err := InstallDSH(context.Background(), nil, InstallRequest{
		Mode:    InstallNpx,
		NodeBin: selfBin,
	})
	if err != nil {
		t.Fatalf("有自备 Node 时 npx 方式应成功，实际 %v", err)
	}
	if !strings.Contains(result.Command, "npx") {
		t.Errorf("启动命令应走 npx，实际 %q", result.Command)
	}
}

// TestInstallNpxWithoutNodeBinStillFails 验证没有任何 Node 时仍然明确报错，
// 而不是拿一个空路径去执行（那会得到一句含糊的 ENOENT）。
func TestInstallNpxWithoutNodeBinStillFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("伪造 PATH 依赖类 Unix 语义")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	_, err := InstallDSH(context.Background(), nil, InstallRequest{Mode: InstallNpx})
	if err == nil {
		t.Fatal("完全没有 Node 时应报错")
	}
	if !strings.Contains(err.Error(), "npx") {
		t.Errorf("错误里应点出缺的是 npx，实际 %v", err)
	}
}

// TestEnsurePnpmUsesSelfProvidedNode 锁定与 npx 同源的坑：
//
// 机器上没有系统级 npm，但网关已把官方运行时下载到状态目录。此时准备 pnpm
// 必须能用**自备运行时里**的 npm——git / pnpm 两种安装方式都要先过这一步，
// 早先只看进程 PATH 会直接报「找不到 npm」，让这两种方式在无 Node 的服务器
// 与容器上完全不可用。
func TestEnsurePnpmUsesSelfProvidedNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	selfBin := filepath.Join(dir, "self-node", "bin")
	if err := os.MkdirAll(selfBin, 0o755); err != nil {
		t.Fatal(err)
	}
	// 假 npm：接受 `install --global pnpm --prefix <prefix>`，把 pnpm 造出来。
	//
	// 脚本只用 shell 内建命令（printf）：这个测试把 PATH 清空了，mkdir / chmod
	// 这类外部命令一律找不到，所以目标目录由测试自己先建好。
	writeExec(t, selfBin, "npm", `
prefix=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--prefix" ]; then prefix="$2"; shift 2; continue; fi
  shift
done
printf '#!/bin/sh\necho 11.28.5\n' > "$prefix/bin/pnpm"
exit 0
`)
	writeExec(t, selfBin, "node", `echo v26.12.0`)

	// 进程 PATH 里没有 npm / node。
	t.Setenv("PATH", filepath.Join(dir, "empty"))

	stateDir := filepath.Join(dir, "state")
	// 预建 pnpm 的目标 bin 目录：假 npm 脚本里不能用 mkdir（PATH 是空的）。
	if err := os.MkdirAll(filepath.Join(stateDir, "pnpm", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin, err := EnsurePnpmWithBin(context.Background(), nil, stateDir, "", selfBin)
	if err != nil {
		t.Fatalf("有自备 Node 时准备 pnpm 应成功，实际 %v", err)
	}
	if bin == "" {
		t.Fatal("应返回 pnpm 所在目录")
	}
	if _, err := os.Stat(filepath.Join(bin, "pnpm")); err != nil {
		t.Errorf("返回的目录里应有 pnpm：%v", err)
	}
}

// TestEnsurePnpmWithoutNodeBinStillFails 验证完全没有 Node 时明确报错。
func TestEnsurePnpmWithoutNodeBinStillFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("伪造 PATH 依赖类 Unix 语义")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	_, err := EnsurePnpm(context.Background(), nil, filepath.Join(dir, "state"), "")
	if err == nil {
		t.Fatal("完全没有 Node 时应报错")
	}
	if !strings.Contains(err.Error(), "npm") {
		t.Errorf("错误里应点出缺的是 npm，实际 %v", err)
	}
}

// TestDetectEnvFoldsSelfPnpmIntoTable 锁定一个会让用户困惑的不一致：
//
// 网关自备的 pnpm 装在状态目录里、不在 PATH 上。如果只把它写进摘要行、表格里
// 却显示「pnpm 缺失」，用户看到的就是自相矛盾的两句话。自备运行时里的命令
// 必须和自备 Node 一样折算进工具表。
func TestDetectEnvFoldsSelfPnpmIntoTable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	// 自备 pnpm 的布局：<状态目录>/pnpm/bin/pnpm
	selfPnpm := filepath.Join(stateDir, "pnpm", "bin")
	if err := os.MkdirAll(selfPnpm, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, selfPnpm, "pnpm", "echo 11.28.5\n")
	// PATH 里什么都没有。
	t.Setenv("PATH", filepath.Join(dir, "empty"))

	report := DetectEnv(context.Background(), stateDir)
	if report.SelfPnpmBin == "" {
		t.Fatalf("应发现自备 pnpm，实际 %+v", report)
	}
	got := report.Tool("pnpm")
	if !got.Found {
		t.Errorf("自备 pnpm 应折算进工具表，实际 %+v", got)
	}
	if got.Version != "11.28.5" {
		t.Errorf("版本应为 11.28.5，实际 %q", got.Version)
	}
	if got.Path != filepath.Join(selfPnpm, "pnpm") {
		t.Errorf("路径应指向自备 pnpm，实际 %q", got.Path)
	}
	if !got.Available() {
		t.Errorf("自备 pnpm 应可用，实际 %+v", got)
	}
}

// TestLockfileMismatch 锁定「哪种失败才值得退回普通 install」。
//
// 上游 monorepo 的 lockfile 会随源码漂移，所以 --frozen-lockfile 失败要能退；
// 但网络超时退回去只会重拉同一批包、白等十几分钟，必须原地报错。
func TestLockfileMismatch(t *testing.T) {
	yes := []string{
		"ERR_PNPM_OUTDATED_LOCKFILE  Cannot install with \"frozen-lockfile\" because pnpm-lock.yaml is not up to date",
		"ERROR  Cannot install with frozen-lockfile because pnpm-lock.yaml is not up to date with package.json",
		"The lockfile is not up to date",
		"lockfile is not up-to-date",
	}
	for _, out := range yes {
		if !lockfileMismatch(out) {
			t.Errorf("应判定为 lockfile 不一致：%s", out)
		}
	}
	no := []string{
		// 实测踩到的这条：cdn.sheetjs.com 超时，与 lockfile 无关。
		"[ERR_PNPM_FETCH_TIMEOUT] GET https://cdn.sheetjs.com/xlsx-0.20.3/xlsx-0.20.3.tgz: timed out",
		"ERR_PNPM_NO_MATCHING_VERSION No matching version found",
		"ENOSPC: no space left on device",
		"",
	}
	for _, out := range no {
		if lockfileMismatch(out) {
			t.Errorf("不该判定为 lockfile 不一致：%s", out)
		}
	}
}

// TestAllowBuildArgs 锁定「放行哪些原生依赖的构建脚本」。
//
// pnpm 10 起默认拒绝依赖的 postinstall 脚本，而 dsh 的原生依赖正是靠它准备
// 平台产物；不放行会以 "Ignored build scripts" 失败退出。这里锁住名单本身，
// 免得有人图省事改成 `--allow-build=*`——那等于把整条依赖链的脚本都放开，
// 正是 pnpm 默认拒绝的初衷所在。
func TestAllowBuildArgs(t *testing.T) {
	args := allowBuildArgs()
	if len(args) != len(dshBuildScriptPackages) {
		t.Fatalf("参数个数应与名单一致：%d vs %d", len(args), len(dshBuildScriptPackages))
	}
	seen := map[string]bool{}
	for _, a := range args {
		if !strings.HasPrefix(a, "--allow-build=") {
			t.Errorf("参数应以 --allow-build= 开头，实际 %q", a)
		}
		name := strings.TrimPrefix(a, "--allow-build=")
		if name == "*" {
			t.Error("不该用通配放行全部构建脚本")
		}
		if seen[name] {
			t.Errorf("名单里有重复项：%s", name)
		}
		seen[name] = true
	}
	// 实测踩到的几个必须在内：缺任何一个，pnpm 都会以 Ignored build scripts 失败。
	for _, want := range []string{"node-pty", "koffi", "protobufjs"} {
		if !seen[want] {
			t.Errorf("名单里应有 %s", want)
		}
	}
}

// TestProbeVersionDoesNotReturnErrors 锁定一个真实踩到的显示 bug：
//
// 装出来的 dsh 是 `#!/usr/bin/env node` 脚本，PATH 里没有 node 时它会打出
// "exec: node: not found"。早先的 probeVersion 把这个报错**当成版本号**返回，
// 管理台上就显示「版本：<路径>: exec: node: not found」。
func TestProbeVersionDoesNotReturnErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	// 一个「失败但仍有输出」的假命令：正是踩到的形态。
	writeExec(t, dir, "broken", "echo 'exec: node: not found' >&2\nexit 1\n")
	path := filepath.Join(dir, "broken")

	if got := probeVersion(context.Background(), path, "--version"); got != "" {
		t.Errorf("失败时不该把报错当版本，实际 %q", got)
	}
	// 成功时正常返回版本。
	writeExec(t, dir, "good", "echo v22.11.0\n")
	if got := probeVersion(context.Background(), filepath.Join(dir, "good"), "--version"); got != "v22.11.0" {
		t.Errorf("成功时应返回版本，实际 %q", got)
	}
}

// TestInstallDirDoesNotNestOnReinstall 锁定一个实测踩到的 bug：
//
// 管理台会把安装结果里的 Dir 写回实例记录，重装时再读出来当 req.Dir。若安装
// 结果回填的是内部推导出的子目录（pnpm 的 <Dir>/dsh、git 的 <Dir>/deepseek-harness），
// 每重装一次就多嵌一层：/srv/pnpm/inst → /srv/pnpm/inst/dsh → /srv/pnpm/inst/dsh/dsh。
func TestInstallDirDoesNotNestOnReinstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 shell 脚本伪造命令，仅类 Unix 平台")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, binDir, "pnpm", `
if [ "$1" = "add" ]; then
  prefix=""
  shift
  while [ $# -gt 0 ]; do
    if [ "$1" = "--dir" ]; then prefix="$2"; shift 2; continue; fi
    shift
  done
  mkdir -p "$prefix/node_modules/.bin"
  printf '#!/bin/sh\necho 0.2.1\n' > "$prefix/node_modules/.bin/dsh"
  chmod +x "$prefix/node_modules/.bin/dsh"
  exit 0
fi
exit 0
`)
	writeExec(t, binDir, "node", `echo v26.12.0`)
	prependPATH(t, binDir)

	// 模拟「装 → 把 Dir 写回记录 → 用记录里的 Dir 再装」两轮。
	installDir := filepath.Join(dir, "inst")
	first, err := InstallDSH(context.Background(), nil, InstallRequest{Mode: InstallPnpm, Dir: installDir})
	if err != nil {
		t.Fatalf("第一轮应成功: %v", err)
	}
	second, err := InstallDSH(context.Background(), nil, InstallRequest{Mode: InstallPnpm, Dir: first.Dir})
	if err != nil {
		t.Fatalf("第二轮应成功: %v", err)
	}
	if second.Dir != first.Dir {
		t.Errorf("重装后安装目录不该变：%q -> %q", first.Dir, second.Dir)
	}
	if second.Dir != installDir {
		t.Errorf("安装目录应始终是用户指定的根目录 %q，实际 %q", installDir, second.Dir)
	}
}
