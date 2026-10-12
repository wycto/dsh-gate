package dshsettings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sample 是从真实 DSH 设置里取来的形状（键名与缩进都照抄，值是占位）。
const sample = `# 由 DSH 维护，不要手工大改
ui-theme:
  preference: dark
agent-default-model:
  provider: "qwen-token-plan-cn"
  model: "qwen3.8-max"
llm-pi-ai:
  providers:
    jiyuanlvdong:
      displayName: "基元律动"
      api: "openai-completions"
      baseURL: "https://example.invalid/v1"
      apiKeyEnv: "JIYUANLVDONG_API_KEY"
      models:
        - id: "deepseek-v4-pro"
          name: "deepseek-v4-pro"
        - id: "glm-5"
          name: "glm-5"
`

// writeSample 造一份临时 DSH 家目录。
func writeSample(t *testing.T, settings string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	path, _ := Paths(home)
	if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
		t.Fatalf("写入样例失败: %v", err)
	}
	return home
}

// TestSetDefaultModelTouchesOnlyItsNamespace 改默认模型不能动到别的命名空间。
func TestSetDefaultModelTouchesOnlyItsNamespace(t *testing.T) {
	home := writeSample(t, sample)
	if _, err := SetDefaultModel(home, DeepSeekOfficial, "deepseek-chat"); err != nil {
		t.Fatalf("写默认模型失败: %v", err)
	}
	path, _ := Paths(home)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	_ = path
	text := string(raw)
	if !strings.Contains(text, `provider: "deepseek-official"`) || !strings.Contains(text, `model: "deepseek-chat"`) {
		t.Errorf("没有写入新默认值:\n%s", text)
	}
	if strings.Contains(text, "qwen-token-plan-cn") {
		t.Errorf("旧默认值还在:\n%s", text)
	}
	for _, keep := range []string{"preference: dark", "jiyuanlvdong", "baseURL: \"https://example.invalid/v1\""} {
		if !strings.Contains(text, keep) {
			t.Errorf("不该被改动的内容丢了 %q:\n%s", keep, text)
		}
	}
	if !strings.Contains(text, "# 由 DSH 维护") {
		t.Errorf("文件头的注释被删了:\n%s", text)
	}

	ref, err := ReadDefaultModel(home)
	if err != nil || !ref.Set || ref.Provider != DeepSeekOfficial || ref.Model != "deepseek-chat" {
		t.Errorf("回读 = %+v（err=%v）", ref, err)
	}
}

// TestSetDefaultModelCreatesFileWhenMissing DSH 可能还没写过设置文件（第一次启动前）。
func TestSetDefaultModelCreatesFileWhenMissing(t *testing.T) {
	home := t.TempDir()
	if _, err := SetDefaultModel(home, DeepSeekOfficial, "deepseek-chat"); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	ref, err := ReadDefaultModel(home)
	if err != nil || !ref.Set || ref.Provider != DeepSeekOfficial {
		t.Fatalf("回读 = %+v（err=%v）", ref, err)
	}
	info, err := os.Stat(filepath.Join(home, SettingsFile))
	if err != nil {
		t.Fatalf("设置文件没生成: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("设置文件权限应收紧，实际 %v", info.Mode().Perm())
	}
}

// TestSetKeyWritesCredentialsFile 密钥走凭据文件的 refs（环境变量名 → 密钥）。
func TestSetKeyWritesCredentialsFile(t *testing.T) {
	home := writeSample(t, sample)
	if _, err := SetKey(home, DeepSeekKeyEnv, "sk-test-123456"); err != nil {
		t.Fatalf("写密钥失败: %v", err)
	}
	names, err := ListKeys(home)
	if err != nil {
		t.Fatalf("列出密钥失败: %v", err)
	}
	if len(names) != 1 || names[0] != DeepSeekKeyEnv {
		t.Fatalf("密钥名 = %v，期望 [%s]", names, DeepSeekKeyEnv)
	}
	has, err := HasKey(home, DeepSeekKeyEnv)
	if err != nil || !has {
		t.Errorf("HasKey = %v（err=%v）", has, err)
	}
	has, _ = HasKey(home, "OTHER_KEY")
	if has {
		t.Error("不存在的密钥名不该报告存在")
	}

	// 覆盖写：旧值必须消失。
	if _, err := SetKey(home, DeepSeekKeyEnv, "sk-new-654321"); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}
	_, credentials := Paths(home)
	raw, _ := os.ReadFile(credentials)
	if strings.Contains(string(raw), "sk-test-123456") {
		t.Errorf("旧密钥还在:\n%s", raw)
	}
	if !strings.Contains(string(raw), "sk-new-654321") {
		t.Errorf("新密钥没写进去:\n%s", raw)
	}
	if count := strings.Count(string(raw), DeepSeekKeyEnv+":"); count != 1 {
		t.Errorf("同一个密钥名出现了 %d 次", count)
	}
}

// TestSetProviderKeepsOtherProviders 新增服务商不能覆盖别的服务商。
func TestSetProviderKeepsOtherProviders(t *testing.T) {
	home := writeSample(t, sample)
	provider := Provider{
		ID:          "my-gw",
		DisplayName: "我的网关",
		API:         "openai-completions",
		BaseURL:     "https://gw.example.com/v1",
		KeyEnv:      "MY_GW_API_KEY",
		Models:      []Model{{ID: "qwen3-max"}, {ID: "glm-5", Name: "GLM 5"}},
	}
	if _, err := SetProvider(home, provider); err != nil {
		t.Fatalf("写服务商失败: %v", err)
	}
	list, err := ListProviders(home)
	if err != nil {
		t.Fatalf("读服务商失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("服务商数量 = %d，期望 2", len(list))
	}
	var found ProviderInfo
	for _, item := range list {
		if item.ID == "my-gw" {
			found = item
		}
		if item.ID == "jiyuanlvdong" {
			if len(item.Models) != 2 {
				t.Errorf("原有服务商的模型被改坏了: %v", item.Models)
			}
		}
	}
	if found.BaseURL != provider.BaseURL || found.KeyEnv != provider.KeyEnv {
		t.Errorf("回读 = %+v", found)
	}
	if len(found.Models) != 2 || found.Models[0] != "qwen3-max" || found.Models[1] != "glm-5" {
		t.Errorf("模型列表 = %v", found.Models)
	}

	// 删除：只删自己那一个。
	path, removed, err := RemoveProvider(home, "my-gw")
	if err != nil || !removed {
		t.Fatalf("删除失败: removed=%v err=%v", removed, err)
	}
	list, _ = ListProviders(home)
	if len(list) != 1 || list[0].ID != "jiyuanlvdong" {
		t.Errorf("删除后 = %v", list)
	}
	if _, removed, _ := RemoveProvider(home, "my-gw"); removed {
		t.Error("删不存在的服务商不该报告成功")
	}
	_ = path
}

// TestRejectsUnsafeNames 名字里带冒号/引号/空格的必须当场拒绝，而不是写出坏 YAML。
func TestRejectsUnsafeNames(t *testing.T) {
	home := writeSample(t, sample)
	if _, err := SetDefaultModel(home, "bad provider", "m"); err == nil {
		t.Error("带空格的服务商名应当被拒绝")
	}
	if _, err := SetDefaultModel(home, "p", "bad:model"); err == nil {
		t.Error("带冒号的模型名应当被拒绝")
	}
	if _, err := SetKey(home, "BAD KEY", "v"); err == nil {
		t.Error("带空格的环境变量名应当被拒绝")
	}
	if _, err := SetKey(home, "K", "line\nbreak"); err == nil {
		t.Error("含换行的密钥应当被拒绝")
	}
	if _, err := SetDefaultModel(home, "", "m"); err == nil {
		t.Error("空的服务商名应当被拒绝")
	}
}

// TestBackupIsWritten 改写之前一定留备份——写坏了还有得退。
func TestBackupIsWritten(t *testing.T) {
	home := writeSample(t, sample)
	path, _ := Paths(home)
	if _, err := SetDefaultModel(home, DeepSeekOfficial, "deepseek-chat"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	backup := BackupPath(path)
	raw, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("没有生成备份: %v", err)
	}
	if !strings.Contains(string(raw), "qwen-token-plan-cn") {
		t.Errorf("备份里应当是改写前的内容:\n%s", raw)
	}
}

// TestRejectsStructuresItCannotUnderstand 读不懂的结构要报错，不能猜着改。
func TestRejectsStructuresItCannotUnderstand(t *testing.T) {
	home := writeSample(t, "agent-default-model: scalar-inline\n")
	// 标量行不是块，本包不会去改它；写入时按新块追加。
	if _, err := SetDefaultModel(home, DeepSeekOfficial, "deepseek-chat"); err != nil {
		t.Fatalf("追加新块失败: %v", err)
	}
	path, _ := Paths(home)
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "agent-default-model: scalar-inline") {
		t.Errorf("原有的标量行被吃掉了:\n%s", raw)
	}
}

// TestCredentialsFileCarriesVersion DSH 的 credentials-local 缺 `version: 1`
// 就拒绝启动（实测报「uses the pre-release flat layout」），所以新建凭据文件
// 必须带上它。
func TestCredentialsFileCarriesVersion(t *testing.T) {
	home := t.TempDir()
	if _, err := SetKey(home, DeepSeekKeyEnv, "sk-x"); err != nil {
		t.Fatalf("写密钥失败: %v", err)
	}
	_, credentials := Paths(home)
	raw, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.HasPrefix(string(raw), "version: "+CredentialsVersion+"\n") {
		t.Errorf("凭据文件应以 version: %s 开头，实际:\n%s", CredentialsVersion, raw)
	}

	// 已有文件里的 version 不能被我们覆盖掉。
	if _, err := SetKey(home, "OTHER_KEY", "sk-y"); err != nil {
		t.Fatalf("追加密钥失败: %v", err)
	}
	raw, _ = os.ReadFile(credentials)
	if strings.Count(string(raw), "version:") != 1 {
		t.Errorf("version 行应当只有一条:\n%s", raw)
	}
}

// TestWrittenFileEndsWithNewline 写出的文件必须以换行结尾。
//
// 合法但重要：settings.yaml 是给人编辑的，末尾缺换行时手工追加一行就会和
// 最后一行粘在一起（实测发现过）。
func TestWrittenFileEndsWithNewline(t *testing.T) {
	home := t.TempDir()
	if _, err := SetDefaultModel(home, DeepSeekOfficial, "deepseek-chat"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := SetKey(home, DeepSeekKeyEnv, "sk-x"); err != nil {
		t.Fatalf("写密钥失败: %v", err)
	}
	settingsPath, credentialsPath := Paths(home)
	for _, path := range []string{settingsPath, credentialsPath} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		if !strings.HasSuffix(string(raw), "\n") {
			t.Errorf("%s 末尾缺换行:\n%s<<<", path, raw)
		}
		if strings.HasSuffix(string(raw), "\n\n") {
			t.Errorf("%s 末尾多了空行:\n%s<<<", path, raw)
		}
	}
}

// TestWritesCreateMissingHome 写文件之前必须把 dsh 实例目录建出来。
//
// 实测踩到：`dsh-gate model key set --dsh-home <还没建的目录>` 与首次 setup 都可能
// 面对不存在的目录，而写盘是「同目录下建临时文件再改名」，于是用户看到的是
// `open …/.dsh-gate-write-…: no such file or directory`——一句完全指错方向的报错。
func TestWritesCreateMissingHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "还没建的目录", "dsh")

	if _, err := SetDefaultModel(home, DeepSeekOfficial, "deepseek-chat"); err != nil {
		t.Fatalf("目录不存在时也应写入成功: %v", err)
	}
	if _, err := SetKey(home, DeepSeekKeyEnv, "sk-x"); err != nil {
		t.Fatalf("目录不存在时也应写入成功: %v", err)
	}
	if _, err := SetProvider(home, Provider{ID: "gw", BaseURL: "https://x/v1"}); err != nil {
		t.Fatalf("目录不存在时也应写入成功: %v", err)
	}

	// 目录里存的是凭据，权限必须是 0700。
	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("目录没建出来: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("实例目录权限 = %v，期望 0700（里面有凭据）", info.Mode().Perm())
	}

	// 回读确认内容真的落盘了。
	ref, err := ReadDefaultModel(home)
	if err != nil || !ref.Set || ref.Provider != DeepSeekOfficial {
		t.Fatalf("回读默认模型失败: %+v %v", ref, err)
	}
	if has, _ := HasKey(home, DeepSeekKeyEnv); !has {
		t.Error("密钥没写进去")
	}
}
