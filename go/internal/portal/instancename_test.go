package portal

import (
	"strings"
	"testing"
)

// TestValidateInstanceNameAcceptsUnicode 保证实例展示名可以用中文。
//
// 实例名是**展示名**：它出现在管理台的授权矩阵、实例列表与用户的「我的实例」里，
// 而对外路径用的是不透明 id（`/i/<id>/`）——既不进文件路径也不进命令行参数。
// 最初的白名单只放行 ASCII，后果是中文界面上只能出现 alice-main 这类英文标识，
// 用户想给实例取个中文名字直接被拒。这条测试把这个决定钉住。
func TestValidateInstanceNameAcceptsUnicode(t *testing.T) {
	ok := []string{
		"爱丽丝的主实例",
		"生产环境",
		"测试实例-01",
		"张三的.dsh",
		"实例A",
		"日本語のインスタンス",
		"a",
		"1",
	}
	for _, name := range ok {
		if err := ValidateInstanceName(name); err != nil {
			t.Errorf("实例名 %q 应当被接受，实际报错: %v", name, err)
		}
	}
}

// TestValidateInstanceNameRejectsDangerous 保证放行 Unicode 之后，那些真的会出事的
// 形状仍然被挡在门外。
//
// 每条都对应一个具体后果：开头的 `-` 会被 CLI 当成选项；空白与引号在复制粘贴进
// 命令行时要额外转义；路径分隔符与 `..` 是路径穿越的经典形状；控制字符会污染
// 日志与页面。名字本身不进路径，但这些形状没有任何正当用途，挡掉比放行便宜。
func TestValidateInstanceNameRejectsDangerous(t *testing.T) {
	bad := []struct{ name, why string }{
		{"-实例", "开头的 - 会被 CLI 当成选项"},
		{"--dsh-home", "同上，甚至能伪装成选项"},
		{".隐藏实例", "开头的 . 容易与相对路径混淆"},
		{"../etc/passwd", "路径穿越"},
		{"a/b", "路径分隔符"},
		{`a\b`, "Windows 路径分隔符"},
		{"两个 词", "空白字符在命令行里要额外转义"},
		{"带\t制表符", "控制字符"},
		{"带\n换行", "控制字符"},
		{"", "空名"},
		{strings.Repeat("字", 65), "超过 64 个字符"},
	}
	for _, item := range bad {
		if err := ValidateInstanceName(item.name); err == nil {
			t.Errorf("实例名 %q 应当被拒绝（%s），实际被接受", item.name, item.why)
		}
	}
}

// TestValidateInstanceNameCountsRunesNotBytes 保证 64 的长度上限按**字符**算。
//
// 一个中文字在 UTF-8 下是 3 个字节。若上限按字节算，中文名只能写 21 个字，
// 而报错里写的是「最长 64 个字符」——文档与行为对不上，用户会以为程序坏了。
func TestValidateInstanceNameCountsRunesNotBytes(t *testing.T) {
	// 64 个汉字：应当刚好通过。
	name := strings.Repeat("实", 64)
	if err := ValidateInstanceName(name); err != nil {
		t.Errorf("64 个汉字的实例名应当被接受，实际报错: %v", err)
	}
	if err := ValidateInstanceName(name + "实"); err == nil {
		t.Error("65 个汉字的实例名应当被拒绝")
	}
}
