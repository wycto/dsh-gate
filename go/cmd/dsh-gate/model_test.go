package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/dshsettings"
)

// isolatedDSHHome 造一个隔离的 dsh 家目录（绝不碰用户真实的 ~/.dsh）。
func isolatedDSHHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "dsh")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	return home
}

// TestModelKeySetFromStdin 密钥必须只从标准输入读：命令行参数会留在 shell 历史里。
func TestModelKeySetFromStdin(t *testing.T) {
	home := isolatedDSHHome(t)
	swapStdin(t, "sk-from-stdin-123456\n")

	code := run([]string{"model", "key", "set", dshsettings.DeepSeekKeyEnv, "--stdin", "--dsh-home", home})
	if code != 0 {
		t.Fatalf("退出码 = %d，期望 0", code)
	}
	names, err := dshsettings.ListKeys(home)
	if err != nil {
		t.Fatalf("读密钥失败: %v", err)
	}
	if len(names) != 1 || names[0] != dshsettings.DeepSeekKeyEnv {
		t.Fatalf("密钥名 = %v", names)
	}
	// 明文确实写进了凭据文件（这是 DSH 的存储方式），但文件权限必须收紧。
	_, credentials := dshsettings.Paths(home)
	info, err := os.Stat(credentials)
	if err != nil {
		t.Fatalf("凭据文件不存在: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("凭据文件权限应收紧，实际 %v", info.Mode().Perm())
	}
}

// TestModelKeySetWithoutStdinRefuses 少了 --stdin 必须拒绝，而不是提示把密钥写在命令行里。
func TestModelKeySetWithoutStdinRefuses(t *testing.T) {
	home := isolatedDSHHome(t)
	code := run([]string{"model", "key", "set", "MY_KEY", "--dsh-home", home})
	if code == 0 {
		t.Fatal("没有 --stdin 时应当拒绝（否则密钥会留在 shell 历史里）")
	}
}

// TestModelDefaultAndList 串起「配密钥 → 设默认模型 → 查看」的完整链路。
func TestModelDefaultAndList(t *testing.T) {
	home := isolatedDSHHome(t)

	if code := run([]string{"model", "default", dshsettings.DeepSeekOfficial, "deepseek-chat", "--dsh-home", home}); code != 0 {
		t.Fatalf("设默认模型失败，退出码 %d", code)
	}
	swapStdin(t, "sk-abc\n")
	if code := run([]string{"model", "key", "set", dshsettings.DeepSeekKeyEnv, "--stdin", "--dsh-home", home}); code != 0 {
		t.Fatalf("写密钥失败，退出码 %d", code)
	}

	ref, err := dshsettings.ReadDefaultModel(home)
	if err != nil || !ref.Set || ref.Provider != dshsettings.DeepSeekOfficial || ref.Model != "deepseek-chat" {
		t.Fatalf("回读默认模型 = %+v（err=%v）", ref, err)
	}
}

// TestModelProviderAddListRemove 自定义服务商的增删查，以及不影响别人的配置。
func TestModelProviderAddListRemove(t *testing.T) {
	home := isolatedDSHHome(t)
	// 先造一份别人的设置，确认我们的写入不会吞掉它。
	settingsPath, _ := dshsettings.Paths(home)
	pre := "ui-theme:\n  preference: dark\n"
	if err := os.WriteFile(settingsPath, []byte(pre), 0o600); err != nil {
		t.Fatalf("准备设置失败: %v", err)
	}

	code := run([]string{
		"model", "provider", "add", "my-gw",
		"--base-url", "https://gw.example.com/v1",
		"--name", "我的网关",
		"--key-env", "MY_GW_API_KEY",
		"--models", "qwen3-max,glm-5",
		"--dsh-home", home,
	})
	if code != 0 {
		t.Fatalf("新增服务商失败，退出码 %d", code)
	}

	raw, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(raw), "preference: dark") {
		t.Errorf("别人的设置被弄丢了:\n%s", raw)
	}
	if strings.Count(string(raw), "ui-theme:") != 1 {
		t.Errorf("出现了重复的顶层键:\n%s", raw)
	}

	list, err := dshsettings.ListProviders(home)
	if err != nil || len(list) != 1 || list[0].ID != "my-gw" {
		t.Fatalf("服务商列表 = %+v（err=%v）", list, err)
	}
	if len(list[0].Models) != 2 {
		t.Errorf("模型列表 = %v", list[0].Models)
	}

	if code := run([]string{"model", "provider", "remove", "my-gw", "--dsh-home", home}); code != 0 {
		t.Fatalf("删除服务商失败，退出码 %d", code)
	}
	list, _ = dshsettings.ListProviders(home)
	if len(list) != 0 {
		t.Errorf("删除后仍有余项: %+v", list)
	}
	raw, _ = os.ReadFile(settingsPath)
	if !strings.Contains(string(raw), "preference: dark") {
		t.Errorf("删除时把别人的设置弄丢了:\n%s", raw)
	}
}

// TestModelRequiresArguments 参数缺失必须给出用法错误，而不是默默改坏配置。
func TestModelRequiresArguments(t *testing.T) {
	home := isolatedDSHHome(t)
	if code := run([]string{"model", "default", "--dsh-home", home}); code != 2 {
		t.Errorf("缺参数时退出码 = %d，期望 2", code)
	}
	if code := run([]string{"model", "provider", "add", "x", "--dsh-home", home}); code != 2 {
		t.Errorf("provider add 缺 --base-url 时退出码 = %d，期望 2", code)
	}
	// 没写任何东西：文件都不该被创建。
	settingsPath, credentialsPath := dshsettings.Paths(home)
	if fileExists(settingsPath) || fileExists(credentialsPath) {
		t.Error("参数不完整时不该留下任何文件")
	}
}

// TestModelSubcommandRequiresArgument model 本身也必须写明子命令。
func TestModelSubcommandRequiresArgument(t *testing.T) {
	if code := run([]string{"model"}); code != 2 {
		t.Errorf("缺子命令时退出码 = %d，期望 2", code)
	}
	if code := run([]string{"model", "nope"}); code != 2 {
		t.Errorf("未知子命令时退出码 = %d，期望 2", code)
	}
}
