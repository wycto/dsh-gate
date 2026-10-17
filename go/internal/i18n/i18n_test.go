package i18n

import (
	"regexp"
	"strings"
	"testing"
)

// TestCatalogsCoverAllKeys 是这套 i18n 的核心保证：两种语言的键集合必须与
// allKeys 完全一致，且每条都非空。少一条、多一条、空值都会在这里失败。
func TestCatalogsCoverAllKeys(t *testing.T) {
	want := map[string]bool{}
	for _, k := range Keys() {
		if want[k] {
			t.Fatalf("allKeys 里重复登记了键 %q", k)
		}
		want[k] = true
	}
	if len(want) == 0 {
		t.Fatal("allKeys 是空的：i18n 形同虚设")
	}

	for _, lang := range Languages() {
		got := Catalog(lang)
		if got == nil {
			t.Fatalf("语言 %s 没有目录", lang)
		}
		for k := range want {
			text, ok := got[k]
			if !ok {
				t.Errorf("语言 %s 缺少文案 %q", lang, k)
				continue
			}
			if strings.TrimSpace(text) == "" {
				t.Errorf("语言 %s 的文案 %q 是空的", lang, k)
			}
		}
		for k := range got {
			if !want[k] {
				t.Errorf("语言 %s 里有未登记在 allKeys 的键 %q（要么登记，要么删掉）", lang, k)
			}
		}
	}
}

// TestPlaceholderParity 检查两种语言的占位符一致：翻译时漏掉一个 %s 会让
// 提示语里少一段信息，而这类错误在页面上很难被注意到。
var verbPattern = regexp.MustCompile(`%[sdv]`)

func TestPlaceholderParity(t *testing.T) {
	for _, k := range Keys() {
		want := verbPattern.FindAllString(zh[k], -1)
		got := verbPattern.FindAllString(en[k], -1)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("键 %q 的占位符不一致：zh=%v en=%v", k, want, got)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		raw  string
		want Lang
	}{
		{"", ZH},
		{"zh", ZH},
		{"zh-CN", ZH},
		{"zh-Hans-CN", ZH},
		{"ZH_cn", ZH},
		{"en", EN},
		{"en-US", EN},
		{"EN-GB", EN},
		{"fr", ZH},    // 不支持的语言回默认
		{"de-DE", ZH}, // 同上
		{"en-US,en;q=0.9", EN},
	}
	for _, c := range cases {
		if got := Normalize(c.raw); got != c.want {
			t.Errorf("Normalize(%q) = %q，期望 %q", c.raw, got, c.want)
		}
	}
}

func TestDetectPrecedence(t *testing.T) {
	env := map[string]string{
		"DSH_GATE_LANG": "en",
		"LC_ALL":        "zh_CN.UTF-8",
		"LANG":          "zh_CN.UTF-8",
	}
	getenv := func(name string) string { return env[name] }

	if got := Detect("zh-CN", getenv); got != ZH {
		t.Errorf("显式指定应最优先，得到 %q", got)
	}
	if got := Detect("", getenv); got != EN {
		t.Errorf("DSH_GATE_LANG 应优先于 LC_ALL/LANG，得到 %q", got)
	}
	delete(env, "DSH_GATE_LANG")
	if got := Detect("", getenv); got != ZH {
		t.Errorf("LC_ALL 应被识别，得到 %q", got)
	}
	delete(env, "LC_ALL")
	delete(env, "LANG")
	if got := Detect("", getenv); got != Default {
		t.Errorf("什么都没有时应回默认语言，得到 %q", got)
	}
	if got := Detect("", nil); got != Default {
		t.Errorf("getenv 为空时应回默认语言，得到 %q", got)
	}
}

func TestFromAcceptLanguage(t *testing.T) {
	cases := []struct {
		header string
		want   Lang
		ok     bool
	}{
		{"en-US,en;q=0.9,zh-CN;q=0.8", EN, true},
		{"zh-CN,zh;q=0.9,en;q=0.8", ZH, true},
		{"fr-FR,de;q=0.9", Default, false},
		{"*", Default, false},
		{"", Default, false},
		{"de,en;q=0.5", EN, true}, // 跳过不认识的第一段
	}
	for _, c := range cases {
		got, ok := FromAcceptLanguage(c.header)
		if got != c.want || ok != c.ok {
			t.Errorf("FromAcceptLanguage(%q) = (%q,%v)，期望 (%q,%v)", c.header, got, ok, c.want, c.ok)
		}
	}
}

func TestTranslateAndFallback(t *testing.T) {
	if got := T(ZH, KeyLoginSubmit); got != "登录" {
		t.Errorf("中文「登录」按钮文案不对: %q", got)
	}
	if got := T(EN, KeyLoginSubmit); got != "Sign in" {
		t.Errorf("英文「Sign in」按钮文案不对: %q", got)
	}
	if got := T(EN, KeyLoginErrTooMany, "30s"); !strings.Contains(got, "30s") {
		t.Errorf("带参数的文案没有格式化: %q", got)
	}
	// 未知语言：回退默认语言，而不是回退到 key。
	if got := T(Lang("fr"), KeyLoginSubmit); got != "登录" {
		t.Errorf("未知语言应回退默认语言，得到 %q", got)
	}
	// 未知 key：原样返回 key（并保证测试能发现这种情况）。
	if got := T(EN, "no.such.key"); got != "no.such.key" {
		t.Errorf("未知 key 应原样返回，得到 %q", got)
	}
	if Has(EN, "no.such.key") {
		t.Error("Has 不该对未知 key 返回 true")
	}
}

func TestLoginStringsComplete(t *testing.T) {
	for _, lang := range Languages() {
		l := Login(lang)
		for name, value := range map[string]string{
			"Username": l.Username, "UsernamePlaceholder": l.UsernamePlaceholder,
			"Password": l.Password, "PasswordPlaceholder": l.PasswordPlaceholder,
			"ShowPassword": l.ShowPassword, "HidePassword": l.HidePassword,
			"Submit": l.Submit, "Hint": l.Hint, "FooterPrefix": l.FooterPrefix,
		} {
			if strings.TrimSpace(value) == "" {
				t.Errorf("语言 %s 的登录页文案 %s 是空的", lang, name)
			}
		}
	}
}

func TestDurationTextIsNotChineseOnly(t *testing.T) {
	// 英文时长里不该出现汉字（占位符一致性测不到这种问题）。
	for _, k := range []string{KeyDurationSeconds, KeyDurationMinutes, KeyDurationMinutesSeconds, KeyDurationHours, KeyDurationHoursMinutes} {
		for _, r := range en[k] {
			if r >= 0x4e00 && r <= 0x9fff {
				t.Errorf("英文目录 %q 里出现了汉字: %q", k, en[k])
			}
		}
	}
}
