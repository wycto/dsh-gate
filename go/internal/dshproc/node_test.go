package dshproc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNodeFileName 锁住官方包的命名规则：平台名不同、扩展名也不同。
func TestNodeFileName(t *testing.T) {
	cases := []struct {
		version  string
		platform string
		want     string
	}{
		{"v22.11.0", "linux-x64", "node-v22.11.0-linux-x64.tar.gz"},
		{"v22.11.0", "darwin-arm64", "node-v22.11.0-darwin-arm64.tar.gz"},
		{"v22.11.0", "win-x64", "node-v22.11.0-win-x64.zip"},
	}
	for _, tc := range cases {
		if got := nodeFileName(tc.version, tc.platform); got != tc.want {
			t.Errorf("nodeFileName(%q, %q) = %q，期望 %q", tc.version, tc.platform, got, tc.want)
		}
	}
}

// TestDecodeNodeVersions 解析官方版本列表，坏数据要报错而不是当成空列表。
func TestDecodeNodeVersions(t *testing.T) {
	raw := []byte(`[{"version":"v26.0.0","lts":false},{"version":"v24.2.0","lts":"Krypton"},{"lts":"x"}]`)
	versions, err := decodeNodeVersions(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := []string{"v26.0.0", "v24.2.0"}
	if len(versions) != len(want) || versions[0] != want[0] || versions[1] != want[1] {
		t.Errorf("版本列表 = %v，期望 %v（保持官方的新到旧顺序）", versions, want)
	}
	if _, err := decodeNodeVersions([]byte("不是一个数组")); err == nil {
		t.Error("坏数据应当报错")
	}
}

// TestParseSHASUMS 解析官方校验和列表，星号前缀与多余空行都要能处理。
func TestParseSHASUMS(t *testing.T) {
	text := "abc123  node-v22.11.0-linux-x64.tar.gz\n" +
		"def456 *node-v22.11.0-win-x64.zip\n" +
		"\n" +
		"garbage-with-no-separator\n"
	if got, ok := parseSHASUMS(text, "node-v22.11.0-linux-x64.tar.gz"); !ok || got != "abc123" {
		t.Errorf("解析结果 = %q（ok=%v），期望 abc123", got, ok)
	}
	if got, ok := parseSHASUMS(text, "node-v22.11.0-win-x64.zip"); !ok || got != "def456" {
		t.Errorf("带星号前缀的解析结果 = %q（ok=%v），期望 def456", got, ok)
	}
	if _, ok := parseSHASUMS(text, "node-v99.0.0-linux-x64.tar.gz"); ok {
		t.Error("列表里没有的文件不该解析出校验和")
	}
}

// TestSafeJoinRejectsEscape 压缩包内容来自网络，绝不能写到目标目录之外。
func TestSafeJoinRejectsEscape(t *testing.T) {
	dest := t.TempDir()
	if _, err := safeJoin(dest, "../../etc/passwd"); err == nil {
		t.Error("`../` 逃逸没有被拦住")
	}
	got, err := safeJoin(dest, "node-v22.11.0/bin/npx")
	if err != nil {
		t.Fatalf("正常路径被拒: %v", err)
	}
	if !strings.HasPrefix(got, dest) {
		t.Errorf("拼出来的路径 %q 不在 %q 下", got, dest)
	}
}

// TestStripComponents 官方包外面套着 node-vX-平台/，要剥掉这一层。
func TestStripComponents(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"node-v22.11.0-linux-x64/bin/npx", "bin/npx"},
		{"./node-v22.11.0-linux-x64/", ""},
		{"node-v22.11.0-linux-x64/", ""},
		{"bin/node", "node"},
	}
	for _, tc := range cases {
		if got := stripComponents(tc.name, 1); got != tc.want {
			t.Errorf("stripComponents(%q) = %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

// TestExtractTarGzKeepsSymlinks npx 在官方包里就是符号链接：跳过它等于装了一个
// 没有 npx 的 Node.js，所以这里必须验证软链被原样重建。
func TestExtractTarGzKeepsSymlinks(t *testing.T) {
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)

	write := func(header *tar.Header, body string) {
		t.Helper()
		header.Size = int64(len(body))
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("写 tar 头失败: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("写 tar 内容失败: %v", err)
		}
	}
	write(&tar.Header{Name: "node-v22.11.0-linux-x64/bin/node", Mode: 0o755, Typeflag: tar.TypeReg}, "ELF")
	write(&tar.Header{Name: "node-v22.11.0-linux-x64/lib/npm-cli.js", Mode: 0o644, Typeflag: tar.TypeReg}, "console.log(1)")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "node-v22.11.0-linux-x64/bin/npx",
		Linkname: "../lib/npm-cli.js",
		Typeflag: tar.TypeSymlink,
	}); err != nil {
		t.Fatalf("写软链失败: %v", err)
	}
	tw.Close()
	gz.Close()

	dest := t.TempDir()
	if err := extractTarGz(bytes.NewReader(buffer.Bytes()), dest, 1); err != nil {
		t.Fatalf("解包失败: %v", err)
	}

	node, err := os.Stat(filepath.Join(dest, "bin", "node"))
	if err != nil {
		t.Fatalf("解出的 node 不存在: %v", err)
	}
	if node.Mode().Perm()&0o111 == 0 {
		t.Errorf("可执行权限丢了：%v", node.Mode().Perm())
	}
	link, err := os.Readlink(filepath.Join(dest, "bin", "npx"))
	if err != nil {
		t.Fatalf("npx 不是符号链接: %v", err)
	}
	if link != "../lib/npm-cli.js" {
		t.Errorf("软链目标 = %q，期望 ../lib/npm-cli.js", link)
	}
}

// TestLocalNodeBinFindsProvisionedRuntime 已经装好的运行时要能被认出来，
// 否则每次启动都会重新下载。
func TestLocalNodeBinFindsProvisionedRuntime(t *testing.T) {
	stateDir := t.TempDir()
	if _, ok := LocalNodeBin(stateDir); ok {
		t.Fatal("空目录不该报告已就绪")
	}

	// 样本要像真的：npx 与 node 同在 bin 下。官方包里 npx 是指向
	// ../lib/node_modules/npm/bin/npx-cli.js 的软链，判定「装好了」时会追到那一跳，
	// 只造一个孤零零的 npx 文件不算装好（曾经就这么误判过）。
	extractDir := filepath.Join(stateDir, nodeDirName, "node-v22.11.1-test")
	binDir := nodeBinDir(extractDir)
	libDir := filepath.Join(extractDir, "lib", "node_modules", "npm", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatalf("准备 lib 目录失败: %v", err)
	}
	target := filepath.Join(libDir, "npx-cli.js")
	if err := os.WriteFile(target, []byte("#!/usr/bin/env node\n"), 0o644); err != nil {
		t.Fatalf("准备 npx 目标失败: %v", err)
	}
	if err := os.Symlink(target, nodeNPXPath(binDir)); err != nil {
		t.Fatalf("准备 npx 软链失败: %v", err)
	}
	nodeName := "node"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
	}
	if err := os.WriteFile(filepath.Join(binDir, nodeName), []byte("ELF"), 0o755); err != nil {
		t.Fatalf("准备 node 失败: %v", err)
	}
	got, ok := LocalNodeBin(stateDir)
	if !ok {
		t.Fatal("装好的运行时没有被认出来")
	}
	if got != binDir {
		t.Errorf("bin 目录 = %q，期望 %q", got, binDir)
	}
}

// TestNodePlatformKey 当前平台的映射必须能给出官方平台名（或在明确不支持的
// 架构上报错），其余平台的映射由 nodeFileName 的用例覆盖。
func TestNodePlatformKey(t *testing.T) {
	platform, err := nodePlatformKey()
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		if err != nil {
			t.Fatalf("当前平台映射失败: %v", err)
		}
		if !strings.Contains(platform, "-") {
			t.Errorf("平台名 %q 不像 nodejs.org 的写法", platform)
		}
	default:
		if err == nil {
			t.Logf("平台 %s 映射为 %s", runtime.GOOS, platform)
		}
	}
}

// TestNodeSourcesPreferOfficialThenMirror 下载源顺序：官方优先，镜像兜底。
func TestNodeSourcesPreferOfficialThenMirror(t *testing.T) {
	if len(nodeSources) != 2 || nodeSources[0] != nodeDistBase || nodeSources[1] != nodeMirrorBase {
		t.Fatalf("下载源 = %v，期望 [官方 镜像]", nodeSources)
	}
	if !strings.HasPrefix(nodeMirrorBase, "https://") || !strings.Contains(nodeMirrorBase, "npmmirror.com") {
		t.Errorf("镜像地址看起来不对: %s", nodeMirrorBase)
	}
}

// TestEnsureNodeRuntimeRealDownload 真下载一次官方 Node（约 40MB），验证
// 「下载 → 校验 → 解包 → 能执行」整条链路。
//
// 默认跳过：网络测试不该出现在常规 `go test` 里。需要时用
// DSH_GATE_TEST_NODE_DOWNLOAD=1 跑一次。
func TestEnsureNodeRuntimeRealDownload(t *testing.T) {
	if os.Getenv("DSH_GATE_TEST_NODE_DOWNLOAD") == "" {
		t.Skip("设置 DSH_GATE_TEST_NODE_DOWNLOAD=1 才做真实下载测试")
	}
	// 把 PATH 清空，模拟「服务器上什么都没装」，否则会走「系统已有 npx」的短路。
	t.Setenv("PATH", t.TempDir())

	stateDir := t.TempDir()
	binDir, err := EnsureNodeRuntime(context.Background(), slog.Default(), stateDir)
	if err != nil {
		t.Fatalf("自动准备 Node.js 失败: %v", err)
	}
	if binDir == "" {
		t.Fatal("没有系统 npx 时必须返回自备运行时的 bin 目录")
	}
	if _, err := os.Stat(nodeNPXPath(binDir)); err != nil {
		t.Fatalf("自备运行时里没有 npx: %v", err)
	}
	// 再跑一次应当直接复用，不再下载。
	again, err := EnsureNodeRuntime(context.Background(), slog.Default(), stateDir)
	if err != nil {
		t.Fatalf("复用已装运行时失败: %v", err)
	}
	if again != binDir {
		t.Errorf("第二次返回 %q，期望复用 %q", again, binDir)
	}
}

// TestEnsureNodeRuntimeFallsBackToMirror 官方源不可用时改用镜像源把 Node 下回来。
//
// 与真实下载测试同一个开关：都要联网拉 40MB。
func TestEnsureNodeRuntimeFallsBackToMirror(t *testing.T) {
	if os.Getenv("DSH_GATE_TEST_NODE_DOWNLOAD") == "" {
		t.Skip("设置 DSH_GATE_TEST_NODE_DOWNLOAD=1 才做真实下载测试")
	}
	original := nodeSources
	nodeSources = []string{"https://registry.invalid.invalid", nodeMirrorBase}
	t.Cleanup(func() { nodeSources = original })

	t.Setenv("PATH", t.TempDir()) // 模拟本机什么都没有
	binDir, err := EnsureNodeRuntime(context.Background(), slog.Default(), t.TempDir())
	if err != nil {
		t.Fatalf("换源后仍没能自备 Node.js: %v", err)
	}
	if _, err := os.Stat(nodeNPXPath(binDir)); err != nil {
		t.Fatalf("镜像源装出来的运行时里没有 npx: %v", err)
	}
}
