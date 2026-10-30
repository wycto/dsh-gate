package main

import (
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
)

// TestBuildStartArgsStartTimeout 是「--dsh-start-timeout 记不记得住」的规矩：
//
//   - 等于默认值时不写进保存的启动配置——将来把默认值调大能惠及已经装好的机器；
//   - 用户点名过的时长必须原样带上，否则 restart 之后又变回默认值。
func TestBuildStartArgsStartTimeout(t *testing.T) {
	build := func(timeout time.Duration) string {
		args := buildStartArgs("0.0.0.0", 12020, "", "", "", "/tmp/state", "", "",
			[]string(nil), timeout)
		return strings.Join(args, " ")
	}

	if got := build(config.DefaultDSHStartTimeout); strings.Contains(got, "--dsh-start-timeout") {
		t.Errorf("默认值不该写进启动参数，实际: %s", got)
	}
	got := build(10 * time.Minute)
	if !strings.Contains(got, "--dsh-start-timeout 10m0s") {
		t.Errorf("点名的时长应当写进启动参数，实际: %s", got)
	}
}

// TestRegisterPortalFlagsStartTimeout 检查这组参数认得 --dsh-start-timeout
// （start、service run、tray 共用它，少一个就是「flag provided but not defined」）。
func TestRegisterPortalFlagsStartTimeout(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	pf := registerPortalFlags(fs)
	if pf.startTimeout != config.DefaultDSHStartTimeout {
		t.Fatalf("默认值 = %s，期望 %s", pf.startTimeout, config.DefaultDSHStartTimeout)
	}
	if err := fs.Parse([]string{"--dsh-start-timeout", "10m"}); err != nil {
		t.Fatalf("解析 --dsh-start-timeout 失败: %v", err)
	}
	if pf.startTimeout != 10*time.Minute {
		t.Errorf("--dsh-start-timeout 未生效 = %s", pf.startTimeout)
	}
	if got := strings.Join(pf.startArgs("/tmp/state"), " "); !strings.Contains(got, "--dsh-start-timeout 10m0s") {
		t.Errorf("startArgs 应当带上点名的时长，实际: %s", got)
	}
}
