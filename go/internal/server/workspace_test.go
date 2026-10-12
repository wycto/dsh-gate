package server

import (
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/config"
)

// TestEffectiveWorkspace 锁住「工作目录」的两条语义：显式指定就用它；
// 没指定时说清楚是跟随网关进程，并且把实际路径带出来——服务托管场景下正是
// 这一行能让人看出「单元里没带 --workspace」。
func TestEffectiveWorkspace(t *testing.T) {
	dir, explicit := effectiveWorkspace(config.Instance{Workspace: "/www/wwwroot"})
	if !explicit || dir != "/www/wwwroot" {
		t.Errorf("显式指定时 = %q（explicit=%v），期望 /www/wwwroot", dir, explicit)
	}

	dir, explicit = effectiveWorkspace(config.Instance{})
	if explicit {
		t.Error("留空时不该报告为「已显式指定」")
	}
	if !strings.Contains(dir, "跟随网关进程") {
		t.Errorf("留空时应说明是跟随网关进程，实际 %q", dir)
	}
}
