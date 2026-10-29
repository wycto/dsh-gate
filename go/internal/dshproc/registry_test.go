package dshproc

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistryPlan 锁住换源策略：用户点名了源就只用它，没点名才允许自动
// 换镜像——把用户指定的源静默替换掉是不可接受的。
func TestRegistryPlan(t *testing.T) {
	if got := registryPlan("https://npm.example.com"); len(got) != 1 || got[0] != "https://npm.example.com" {
		t.Errorf("显式源的计划 = %v，应只用用户给的那一个", got)
	}
	got := registryPlan("")
	if len(got) != 2 || got[0] != "" || got[1] != RegistryMirror {
		t.Errorf("默认源的计划 = %v，应为「npm 默认 → 镜像」", got)
	}
}

// TestNpxCommandKeepsArgsClean 源要写在包名之前，别把位置挤到 dsh 的参数上。
func TestNpxCommandKeepsArgsClean(t *testing.T) {
	plain := npxCommand("npx", "", "")
	if plain != "npx --yes "+dshPackage {
		t.Errorf("默认源命令 = %q", plain)
	}
	withSource := npxCommand("npx", "0.1.7-rc.2", RegistryMirror)
	want := "npx --yes --registry=" + RegistryMirror + " " + dshPackage + "@0.1.7-rc.2"
	if withSource != want {
		t.Errorf("命令 = %q，期望 %q", withSource, want)
	}
	if strings.Index(withSource, "--registry") > strings.Index(withSource, dshPackage) {
		t.Error("--registry 必须在包名之前，否则会挤掉传给 dsh 的参数位置")
	}
}

// TestIsNetworkFailure 区分「源拉不动」与「dsh 跑不起来」：只有前者值得换源。
func TestIsNetworkFailure(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		output string
		want   bool
	}{
		{"冒烟超时", ErrSmokeTimeout, "", true},
		{"npm 超时错误码", errors.New("exit status 1"), "npm error code ETIMEDOUT", true},
		{"DNS 解析不了", errors.New("exit status 1"), "request to https://registry.npmjs.org/@deepseek-ai%2fdsh failed, reason: getaddrinfo ENOTFOUND", true},
		{"入口静默不执行", errSmokeNoOutput, "", false},
		{"原生绑定缺失", errors.New("exit status 1"), "No usable native binding found", false},
		{"旧 Node 崩溃", errors.New("exit status 1"), "SyntaxError: parseEnv\nNode.js v18.20.4", false},
		{"没有错误", nil, "whatever", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNetworkFailure(tc.err, tc.output); got != tc.want {
				t.Errorf("isNetworkFailure = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestNpmEnv 注入 npm_config_registry：要覆盖已有值、不留重复项；
// 空源表示用 npm 自己的默认，这时一个字符都不该动。
func TestNpmEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "npm_config_registry=https://old.example.com", "HOME=/root"}

	got := npmEnv(base, RegistryMirror)
	joined := strings.Join(got, "\n")
	if strings.Count(joined, "npm_config_registry=") != 1 {
		t.Fatalf("应当只有一个 npm_config_registry，实际: %v", got)
	}
	if !strings.Contains(joined, "npm_config_registry="+RegistryMirror) {
		t.Errorf("没有注入镜像源: %v", got)
	}
	if !strings.Contains(joined, "HOME=/root") {
		t.Errorf("原有环境变量被弄丢了: %v", got)
	}

	untouched := npmEnv(base, "")
	if strings.Join(untouched, "\n") != strings.Join(base, "\n") {
		t.Errorf("空源不该改动环境变量: %v", untouched)
	}
}

// TestChildEnvCarriesRegistry dsh 子进程必须拿到源：它自己装插件时也走同一个源。
func TestChildEnvCarriesRegistry(t *testing.T) {
	explicit := New(Options{Host: "127.0.0.1", Port: 1, Home: t.TempDir(), NPMRegistry: "https://npm.example.com"})
	if env := strings.Join(explicit.childEnv(), "\n"); !strings.Contains(env, "npm_config_registry=https://npm.example.com") {
		t.Errorf("用户点名的源没有进子进程环境: %s", env)
	}

	auto := New(Options{Host: "127.0.0.1", Port: 1, Home: t.TempDir()})
	if env := strings.Join(auto.childEnv(), "\n"); strings.Contains(env, "npm_config_registry=") {
		t.Errorf("还没换源时不该注入 npm_config_registry: %s", env)
	}
	auto.SetResolvedCommand(Resolution{Command: "npx --yes pkg", Registry: RegistryMirror})
	if env := strings.Join(auto.childEnv(), "\n"); !strings.Contains(env, "npm_config_registry="+RegistryMirror) {
		t.Errorf("自动选中的镜像源没有进子进程环境: %s", env)
	}

	// 用户点名的源优先：自动换源不能盖掉它。
	both := New(Options{Host: "127.0.0.1", Port: 1, Home: t.TempDir(), NPMRegistry: "https://npm.example.com"})
	both.SetResolvedCommand(Resolution{Command: "npx --yes pkg", Registry: RegistryMirror})
	if env := strings.Join(both.childEnv(), "\n"); !strings.Contains(env, "npm_config_registry=https://npm.example.com") {
		t.Errorf("自动换源盖掉了用户点名的源: %s", env)
	}
}

// TestChildEnvHomeIsInstanceWorkspace dsh 的「选择工作区目录」弹窗从宿主家目录开始
// （DSH 用 os.homedir() 解析默认路径，没有配置项可改）。网关以服务方式跑在无头机器上，
// 进程家目录是 /root，于是每个实例点「选择工作区」都落在 /root。所以 HOME 必须指向
// 实例自己的工作目录，弹窗默认就在该实例的工作区里。
func TestChildEnvHomeIsInstanceWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")

	withWorkspace := New(Options{Host: "127.0.0.1", Port: 1, Home: t.TempDir(), Workspace: workspace})
	if got := envValue(withWorkspace.childEnv(), "HOME"); got != workspace {
		t.Errorf("HOME 应为实例工作目录 %q，实际 %q", workspace, got)
	}

	// 没有配工作目录时不动 HOME：沿用网关自己的家目录，不替用户猜。
	noWorkspace := New(Options{Host: "127.0.0.1", Port: 1, Home: t.TempDir()})
	if got := envValue(noWorkspace.childEnv(), "HOME"); got != os.Getenv("HOME") {
		t.Errorf("未配工作目录时不该改动 HOME：期望 %q，实际 %q", os.Getenv("HOME"), got)
	}

	// HOME 只出现一次：网关进程自己的那条必须被替换掉，而不是并存两份。
	env := withWorkspace.childEnv()
	var count int
	for _, item := range env {
		if strings.HasPrefix(item, "HOME=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("HOME 只应出现一次，实际 %d 次", count)
	}

	// npm 缓存必须留在网关侧：HOME 改到工作目录后，npm 的 _npx / _cacache 默认
	// 跟着 HOME 走，会出现在用户的工作区里，把「选择工作区」的列表搞脏。
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("读不到网关家目录: %v", err)
	}
	if want := filepath.Join(home, ".npm"); envValue(env, "npm_config_cache") != want {
		t.Errorf("npm 缓存应固定为 %q，实际 %q", want, envValue(env, "npm_config_cache"))
	}
}

// TestEnsureDSHFallsBackToMirror 真跑一次换源：把环境里的默认源指到一个不存在
// 的域名（模拟「官方源拉不动」），EnsureDSH 应当自动改用镜像源并成功。
//
// 默认跳过（要联网下载 dsh 包）：DSH_GATE_TEST_NPM_FALLBACK=1 时才跑。
func TestEnsureDSHFallsBackToMirror(t *testing.T) {
	if os.Getenv("DSH_GATE_TEST_NPM_FALLBACK") == "" {
		t.Skip("设置 DSH_GATE_TEST_NPM_FALLBACK=1 才做真实换源测试")
	}
	// 默认源指向不存在的域名：npm 会很快以 ENOTFOUND 失败。
	t.Setenv("npm_config_registry", "https://registry.invalid.invalid")

	resolved, err := EnsureDSH(context.Background(), slog.Default(), "")
	if err != nil {
		t.Fatalf("换源后仍然失败: %v", err)
	}
	if resolved.Registry != RegistryMirror {
		t.Fatalf("Registry = %q，期望自动改用镜像 %q", resolved.Registry, RegistryMirror)
	}
	if !strings.Contains(resolved.Command, "--registry="+RegistryMirror) {
		t.Errorf("命令里应带上镜像源，实际: %s", resolved.Command)
	}
}
