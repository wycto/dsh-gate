// 管理台自动刷新的测试：实时端点返回什么、页面怎么放行那段脚本。
//
// 自动刷新最容易坏在两处：端点给的数据与页面初始渲染对不上（文案、状态各说
// 一套），以及脚本改了却忘了同步 CSP 哈希（页面看起来只是「不刷新」）。这两点
// 都在这里钉住。
package portal

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/assets"
	"github.com/wycto/dsh-gate/internal/i18n"
)

// getLive 拉一次实时状态并解析。
func getLive(t *testing.T, f *portalFixture, session *http.Cookie, query string) adminLiveJSON {
	t.Helper()
	rec := f.get(t, AdminLivePath+query, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("实时状态应 200，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("实时状态应是 JSON，实际 %q", ct)
	}
	var live adminLiveJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("解析实时状态失败: %v", err)
	}
	if !live.OK {
		t.Fatal("ok 应为 true")
	}
	return live
}

func TestAdminLiveRequiresAdmin(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.login(t, "alice", "alice-password-123")
	if rec := f.get(t, AdminLivePath, alice); rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户读实时状态应 403，实际 %d", rec.Code)
	}

	root := f.login(t, "root", "root-password-123")
	rec := f.get(t, AdminLivePath, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员读实时状态应 200，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "pbkdf2") {
		t.Fatal("实时状态里泄漏了口令哈希")
	}
}

// TestAdminLiveReflectsRuntime 验证实例状态的变化会如实进到实时端点：
// 页面脚本只负责把值贴回去，所以这里对了，页面就是对的。
func TestAdminLiveReflectsRuntime(t *testing.T) {
	f, _ := newLifespanFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	live := getLive(t, f, session, "")
	if live.Stats.Instances != 3 || live.Stats.Running != 0 {
		t.Fatalf("初始计数不对: %+v", live.Stats)
	}
	if live.Env != nil {
		t.Error("不请求 ?env=1 时不该做环境探测")
	}
	if !strings.Contains(live.Stats.InstancesNote, "0") || live.Stats.RunningNote == "" {
		t.Errorf("统计卡说明应拼好文案: %+v", live.Stats)
	}

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":      {"instance.start"},
		"instance_id": {f.aliceIns.ID},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("启动应 303 回跳，实际 %d", rec.Code)
	}

	live = getLive(t, f, session, "")
	if live.Stats.Running != 1 {
		t.Errorf("启动后运行中计数应为 1，实际 %d", live.Stats.Running)
	}
	var found bool
	for _, inst := range live.Instances {
		if inst.ID != f.aliceIns.ID {
			continue
		}
		found = true
		if !inst.Running || !inst.Ready {
			t.Errorf("启动后应报运行中: %+v", inst)
		}
		if inst.PID != 4242 {
			t.Errorf("PID 应来自运行时，实际 %d", inst.PID)
		}
		if want := i18n.T(i18n.ZH, i18n.KeyAdminRunRunning); inst.RunLabel != want {
			t.Errorf("运行文案应为 %q，实际 %q", want, inst.RunLabel)
		}
		if inst.RunSource == "" {
			t.Error("运行中应标明来源（网关拉起 / 接管）")
		}
	}
	if !found {
		t.Fatalf("实时状态里没有 %s: %+v", f.aliceIns.ID, live.Instances)
	}
	// 启动动作会写审计：版本号应前进、新行应出现在快照里（页面据此重建表格）。
	if live.Audit.Rev == 0 || len(live.Audit.Rows) == 0 {
		t.Errorf("动作后审计快照应带版本号与行: %+v", live.Audit)
	}
}

// TestAdminLiveEnvGated 验证环境探测的开关与状态文案：没带 ?env=1 不做探测，
// 带了才给出工具行，且补装任务前后按钮与「安装中」标记互斥。
func TestAdminLiveEnvGated(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	if live := getLive(t, f, session, ""); live.Env != nil {
		t.Fatal("不带 ?env=1 时不该给出环境行")
	}

	live := getLive(t, f, session, "?env=1")
	if live.Env == nil || len(live.Env.Tools) == 0 {
		t.Fatal("带 ?env=1 时应给出环境行")
	}
	tools := map[string]adminLiveToolJSON{}
	for _, tool := range live.Env.Tools {
		tools[tool.Name] = tool
	}
	node, ok := tools["node"]
	if !ok || node.StatusClass != "on" || node.StatusText != "v22.11.0" || node.ShowInstall {
		t.Errorf("node 应显示已找到的版本: %+v", node)
	}
	pnpm, ok := tools["pnpm"]
	if !ok || pnpm.StatusClass != "off" || !pnpm.ShowInstall || pnpm.ShowDuring {
		t.Errorf("pnpm 缺失时应给出可安装状态: %+v", pnpm)
	}
	if pnpm.StatusText != i18n.T(i18n.ZH, i18n.KeyAdminInstEnvMissing) {
		t.Errorf("缺失文案不对: %q", pnpm.StatusText)
	}
	if pnpm.ButtonLabel == "" {
		t.Error("可安装时按钮文案不该为空")
	}

	// 点一次补齐：下一次轮询应看到「安装中」，按钮换成进度标记。
	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action": {"env.install"},
		"tool":   {"pnpm"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("补齐工具应 303 回跳，实际 %d", rec.Code)
	}
	live = getLive(t, f, session, "?env=1")
	for _, tool := range live.Env.Tools {
		if tool.Name != "pnpm" {
			continue
		}
		if tool.ShowInstall || !tool.ShowDuring {
			t.Errorf("任务在跑时按钮应让位给「安装中」: %+v", tool)
		}
		if tool.StatusText == "" || tool.StatusClass == "on" {
			t.Errorf("安装中状态文案不对: %+v", tool)
		}
	}
}

// stalePIDLifespan 模拟真实监督者：停止之后仍保留上一次的进程号。
type stalePIDLifespan struct {
	running bool
	pid     int
}

func (l stalePIDLifespan) InstanceStatus(Instance) InstanceStatus {
	return InstanceStatus{Running: l.running, Ready: l.running, PID: l.pid}
}
func (l stalePIDLifespan) StartInstance(context.Context, Instance) error   { return nil }
func (l stalePIDLifespan) StopInstance(context.Context, Instance) error    { return nil }
func (l stalePIDLifespan) RestartInstance(context.Context, Instance) error { return nil }

// TestInstanceLiveStateHidesStalePID 钉住「没在跑就不显示 PID」：监督者会保留
// 上一次的进程号，停掉之后卡片上还挂着一个死 PID，看起来像没停下来。
func TestInstanceLiveStateHidesStalePID(t *testing.T) {
	s := &Server{opts: ServerOptions{Lifespan: stalePIDLifespan{pid: 4242}}}
	if live := s.instanceLiveState(context.Background(), i18n.ZH, Instance{}); live.PID != 0 {
		t.Errorf("已停止时不该给出 PID，实际 %d", live.PID)
	}
	s.opts.Lifespan = stalePIDLifespan{running: true, pid: 4242}
	if live := s.instanceLiveState(context.Background(), i18n.ZH, Instance{}); live.PID != 4242 {
		t.Errorf("运行中应给出 PID，实际 %d", live.PID)
	}
}

// TestAdminPageAutoRefreshScript 验证页面带的脚本与放行它的 CSP 哈希一致。
//
// 两件事一起钉：脚本逐字节进页面（模板没转义坏），CSP 放行的正是这一份。
// 否则表现只是「页面不刷新」，在浏览器控制台之外完全看不出来。
func TestAdminPageAutoRefreshScript(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	start := strings.Index(body, "<script>")
	end := strings.Index(body, "</script>")
	if start < 0 || end <= start {
		t.Fatal("页面里应有自动刷新脚本")
	}
	inline := body[start+len("<script>") : end]

	std, _, err := assets.AdminLiveScript()
	if err != nil {
		t.Fatalf("读取内嵌脚本失败: %v", err)
	}
	if inline != std {
		t.Error("页面里的脚本与内嵌资源不一致（模板改写了内容）")
	}
	sum := sha256.Sum256([]byte(inline))
	want := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src '"+want+"'") {
		t.Errorf("CSP 应放行页面里这段脚本的哈希 %s，实际 %q", want, csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP 应允许同源轮询，实际 %q", csp)
	}
	if strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Error("不该为自动刷新放开 unsafe-inline")
	}
	if !strings.Contains(body, `data-live="`+AdminLivePath+`"`) {
		t.Error("页面应把轮询端点写给脚本")
	}
	if !strings.Contains(body, `data-audit-rev="`) {
		t.Error("页面应把审计版本号写给脚本")
	}
}
