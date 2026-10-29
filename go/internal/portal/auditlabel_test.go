package portal

import (
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/i18n"
)

func TestAuditActionLabel(t *testing.T) {
	if got := auditActionLabel(i18n.ZH, "instance.enter"); got != "进入实例" {
		t.Errorf("instance.enter 应翻译成中文，得到 %q", got)
	}
	if got := auditActionLabel(i18n.EN, "admin.instance.restart"); !strings.Contains(got, "Restart") {
		t.Errorf("英文动作标签不对: %q", got)
	}
	// 未登记的标识原样透出，不能被漏翻译藏掉。
	if got := auditActionLabel(i18n.ZH, "login.ok"); got != "login.ok" {
		t.Errorf("未登记标识应原样返回，得到 %q", got)
	}
}

func TestAuditDetailLabel(t *testing.T) {
	// 进入实例的裸 id 要有上下文。
	if got := auditDetailLabel(i18n.ZH, "instance.enter", "yasa7h4r", true); !strings.Contains(got, "yasa7h4r") || !strings.Contains(got, "实例") {
		t.Errorf("裸实例 id 应补上下文，得到 %q", got)
	}
	// 失败行的错误文本前缀「失败：」，错误本身保留。
	if got := auditDetailLabel(i18n.ZH, "admin.user.create", "some err", false); !strings.HasPrefix(got, "失败") || !strings.Contains(got, "some err") {
		t.Errorf("失败说明不对: %q", got)
	}
	// 成功时已删目录的中文说明原样保留。
	if got := auditDetailLabel(i18n.ZH, "admin.instance.remove", "已删除目录：/srv/a", true); got != "已删除目录：/srv/a" {
		t.Errorf("已删目录说明不该被改写: %q", got)
	}
	// 空说明保持空。
	if got := auditDetailLabel(i18n.ZH, "admin.instance.start", "", true); got != "" {
		t.Errorf("空说明不该凭空造字: %q", got)
	}
}

// 全部已登记动作在两种语言下都必须翻得出中文名——漏登记会在页面上以
// 裸英文标识出现，正是这次改动要消灭的东西。
func TestAuditActionKeysRegistered(t *testing.T) {
	actions := []string{
		"instance.enter", "instance.denied",
		"admin.user.create", "admin.user.passwd", "admin.user.role",
		"admin.user.toggle", "admin.user.delete", "admin.grants.save",
		"admin.instance.create", "admin.instance.install", "admin.instance.remove",
		"admin.instance.start", "admin.instance.stop", "admin.instance.restart",
		"admin.instance.update", "admin.env.install", "admin.csrf-rejected",
	}
	for _, lang := range i18n.Languages() {
		for _, a := range actions {
			got := auditActionLabel(lang, a)
			if got == a {
				t.Errorf("语言 %s 下动作 %q 没有登记翻译", lang, a)
			}
			if strings.TrimSpace(got) == "" {
				t.Errorf("语言 %s 下动作 %q 翻译为空", lang, a)
			}
		}
	}
}
