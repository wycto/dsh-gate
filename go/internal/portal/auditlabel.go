// 审计页的显示文案。库里存的一直是稳定的英文短标识（机器可读，JSON API 与
// 排查都靠它），页面渲染时才翻成当前语言——历史数据不用迁移，旧行也能一起
// 显示中文。没登记的新标识原样透出：宁可看到英文标识，也不要被漏翻译藏掉。
package portal

import (
	"github.com/wycto/dsh-gate/internal/i18n"
)

// auditActionKeys 把审计里出现过的全部动作标识映射到文案键。
// 值来自 admin.go（"admin." + 动作名）、server.go（enter/denied）与
// adminapi.go（csrf-rejected）；新增动作时在这里补一行即可。
var auditActionKeys = map[string]string{
	"instance.enter":         i18n.KeyAuditInstanceEnter,
	"instance.denied":        i18n.KeyAuditInstanceDenied,
	"admin.user.create":      i18n.KeyAuditUserCreate,
	"admin.user.passwd":      i18n.KeyAuditUserPasswd,
	"admin.user.role":        i18n.KeyAuditUserRole,
	"admin.user.toggle":      i18n.KeyAuditUserToggle,
	"admin.user.delete":      i18n.KeyAuditUserDelete,
	"admin.grants.save":      i18n.KeyAuditGrantsSave,
	"admin.instance.create":  i18n.KeyAuditInstanceCreate,
	"admin.instance.install": i18n.KeyAuditInstanceInstall,
	"admin.instance.remove":  i18n.KeyAuditInstanceRemove,
	"admin.instance.start":   i18n.KeyAuditInstanceStart,
	"admin.instance.stop":    i18n.KeyAuditInstanceStop,
	"admin.instance.restart": i18n.KeyAuditInstanceRestart,
	"admin.instance.update":  i18n.KeyAuditInstanceUpdate,
	"admin.env.install":      i18n.KeyAuditEnvInstall,
	"admin.csrf-rejected":    i18n.KeyAuditCSRFRejected,
}

// auditActionLabel 翻译动作标识；未登记的返回原标识。
func auditActionLabel(lang i18n.Lang, action string) string {
	if key, ok := auditActionKeys[action]; ok {
		return i18n.T(lang, key)
	}
	return action
}

// auditDetailLabel 翻译说明列：
//   - 失败行加「失败：」前缀（错误文本本身是排查线索，不翻译）；
//   - 进入实例的裸实例 id 补上「实例 id」上下文；
//   - 其余非空说明（如已删目录清单）本来就是中文，原样保留。
func auditDetailLabel(lang i18n.Lang, action, detail string, ok bool) string {
	switch {
	case !ok && detail != "":
		return i18n.T(lang, i18n.KeyAuditDetailFailed, detail)
	case action == "instance.enter" && detail != "":
		return i18n.T(lang, i18n.KeyAuditDetailInstanceID, detail)
	}
	return detail
}
