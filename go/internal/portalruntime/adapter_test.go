package portalruntime

import "testing"

// TestInstallVersionPrefersRequest 安装时版本的取法：请求里填了用填的，没填用实例
// 记录里钉住的。
//
// 反过来不成立（填了空的就用记录里的）是刻意的：安装面板上那一栏留空意味着「按
// 这台实例本来钉的版本重装」，不该顺手把用户之前钉的版本清掉——要取消钉住，去
// 编辑配置里清空并保存。
func TestInstallVersionPrefersRequest(t *testing.T) {
	cases := []struct{ requested, stored, want string }{
		{"next", "0.2.1", "next"},
		{" 0.2.2-alpha.2 ", "", "0.2.2-alpha.2"},
		{"", "0.2.1", "0.2.1"},
		{"   ", " 0.2.1-rc.4", "0.2.1-rc.4"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := installVersion(c.requested, c.stored); got != c.want {
			t.Errorf("installVersion(%q, %q) = %q，期望 %q", c.requested, c.stored, got, c.want)
		}
	}
}
