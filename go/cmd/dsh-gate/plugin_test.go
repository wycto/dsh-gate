package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPackageNameOf 从「包名@版本」里取包名：scope 包的 @ 在开头、版本的 @ 在
// 中间，认错就会往 bundle 清单里写一个不存在的名字。
func TestPackageNameOf(t *testing.T) {
	cases := map[string]string{
		"dsh-dock":                       "dsh-dock",
		"dsh-dock@0.14.0":                "dsh-dock",
		"@deepseek-ai/dsh-web-app":       "@deepseek-ai/dsh-web-app",
		"@deepseek-ai/dsh-web-app@1.2.3": "@deepseek-ai/dsh-web-app",
		"  dsh-dock  ":                   "dsh-dock",
		"":                               "",
	}
	for spec, want := range cases {
		if got := packageNameOf(spec); got != want {
			t.Errorf("packageNameOf(%q) = %q，期望 %q", spec, got, want)
		}
	}
}

// TestRegisterBundleIsAdditiveAndIdempotent 写 bundle 清单要幂等，且不能动
// 原有条目——dsh 的 profile 解析靠这份清单，写丢一条等于把插件从启动里摘掉。
func TestRegisterBundleIsAdditiveAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	original := `{
  "name": "dsh-profile-web",
  "private": true,
  "dependencies": {},
  "dsh": { "profile": { "bundles": ["@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app"] } }
}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("准备 profile 失败: %v", err)
	}

	if err := registerBundle(dir, "dsh-dock"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := registerBundle(dir, "dsh-dock"); err != nil {
		t.Fatalf("重复注册失败: %v", err)
	}

	pkg, _, err := loadProfilePackage(dir)
	if err != nil {
		t.Fatalf("重新读取失败: %v", err)
	}
	want := []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app", "dsh-dock"}
	if len(pkg.DSH.Profile.Bundles) != len(want) {
		t.Fatalf("bundles = %v，期望 %v（幂等：不该有重复）", pkg.DSH.Profile.Bundles, want)
	}
	for i, name := range want {
		if pkg.DSH.Profile.Bundles[i] != name {
			t.Errorf("bundles[%d] = %q，期望 %q", i, pkg.DSH.Profile.Bundles[i], name)
		}
	}
	if !pkg.Private || pkg.Name != "dsh-profile-web" {
		t.Errorf("原有字段被改动了: %+v", pkg)
	}
	// 写回后必须仍是合法 JSON（dsh 启动时要读它）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取写回结果失败: %v", err)
	}
	var check map[string]any
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("写回的 package.json 不是合法 JSON: %v\n%s", err, raw)
	}
}

// TestLoadProfilePackageGuidesWhenMissing 配置档不存在时要说清「先启动一次 dsh」，
// 而不是让我们凭空造一个可能少挂载点的 profile。
func TestLoadProfilePackageGuidesWhenMissing(t *testing.T) {
	_, path, err := loadProfilePackage(filepath.Join(t.TempDir(), "profiles", "web"))
	if err == nil {
		t.Fatal("配置档不存在时应当报错")
	}
	if !strings.Contains(err.Error(), "先启动一次 dsh") || !strings.Contains(err.Error(), path) {
		t.Errorf("错误信息应指路「先启动一次 dsh」并给出路径，实际: %v", err)
	}
}

// TestPluginProfilePath 配置档路径要落在 <dsh-home>/profiles/<profile>。
func TestPluginProfilePath(t *testing.T) {
	opts := pluginOptions{DSHHome: "/srv/dsh", Profile: "web"}
	got, err := opts.profilePath()
	if err != nil {
		t.Fatalf("显式 --dsh-home 时不该报错: %v", err)
	}
	if want := filepath.Join("/srv/dsh", "profiles", "web"); got != want {
		t.Errorf("profilePath = %q，期望 %q", got, want)
	}
}

// TestPluginRequiresInstance 没给 --instance 也没给 --dsh-home 时必须明确报错，
// 不能悄悄落到 ~/.dsh——管理端形态下每个实例都有自己的 DSH_HOME。
func TestPluginRequiresInstance(t *testing.T) {
	opts := pluginOptions{Profile: "web"}
	if _, err := opts.profilePath(); err == nil || !strings.Contains(err.Error(), "--instance") {
		t.Fatalf("未指定实例时应报错并提到 --instance，实际: %v", err)
	}
}
