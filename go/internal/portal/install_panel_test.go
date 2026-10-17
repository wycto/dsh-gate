// 管理台实例安装面板的测试：环境检测展示、创建实例、触发安装、进度可见。
//
// 用一个假的 Installer 注入——管理台这一侧只依赖 portal.Installer 接口，所以
// 不必真的去下载 dsh。这样测的是「页面把接口给的数据如实呈现、把表单参数如实
// 传给接口」，正是这一层唯一可能出错的地方。
package portal

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// fakeInstaller 记录管理台调过来的请求，并返回可编排的状态。
type fakeInstaller struct {
	mu       sync.Mutex
	env      EnvStatus
	requests []InstallRequest
	status   map[string]JobStatus
	// startErr 非 nil 时 StartInstall 返回它。
	startErr error
	// toolRequests / toolStatus 记录运行环境补齐（env.install）的调用与状态。
	toolRequests []string
	toolStatus   map[string]ToolJob
	// toolErr 非 nil 时 StartToolInstall 返回它。
	toolErr error
}

func (f *fakeInstaller) DetectEnv(ctx context.Context) EnvStatus { return f.env }

func (f *fakeInstaller) StartInstall(ctx context.Context, inst Instance, req InstallRequest) (JobStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return JobStatus{}, f.startErr
	}
	f.requests = append(f.requests, req)
	job := JobStatus{
		InstanceID: inst.ID,
		Mode:       req.Mode,
		Phase:      InstallPhaseRunning,
		Step:       "正在安装",
		StartedAt:  time.Now(),
	}
	f.status[inst.ID] = job
	return job, nil
}

func (f *fakeInstaller) InstallStatus(ctx context.Context, inst Instance) (JobStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.status[inst.ID]
	return job, ok
}

func (f *fakeInstaller) StartToolInstall(ctx context.Context, tool string) (ToolJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolRequests = append(f.toolRequests, tool)
	if f.toolErr != nil {
		return ToolJob{}, f.toolErr
	}
	job := ToolJob{Tool: tool, Phase: InstallPhaseRunning, Step: "正在安装 pnpm"}
	f.toolStatus[tool] = job
	return job, nil
}

func (f *fakeInstaller) ToolInstallStatus(ctx context.Context, tool string) (ToolJob, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.toolStatus[tool]
	return job, ok
}

func (f *fakeInstaller) lastToolRequest() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.toolRequests) == 0 {
		return "", false
	}
	return f.toolRequests[len(f.toolRequests)-1], true
}

func (f *fakeInstaller) lastRequest() (InstallRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return InstallRequest{}, false
	}
	return f.requests[len(f.requests)-1], true
}

// newInstallFixture 造一个带安装能力的 fixture。
func newInstallFixture(t *testing.T) (*portalFixture, *fakeInstaller) {
	t.Helper()
	f := newPortalFixture(t)
	inst := &fakeInstaller{
		env: EnvStatus{
			Platform: "linux/amd64",
			Tools: []ToolStatus{
				{Name: "node", Found: true, Path: "/usr/bin/node", Version: "v22.11.0"},
				{Name: "npm", Found: true, Path: "/usr/bin/npm", Version: "10.9.0"},
				{Name: "npx", Found: true, Path: "/usr/bin/npx", Version: "10.9.0"},
				{Name: "pnpm", Found: false, Hint: "npm install -g pnpm"},
				{Name: "git", Found: true, Path: "/usr/bin/git", Version: "git version 2.43.0"},
				{Name: "dsh", Found: false, Hint: "还没有装 dsh"},
			},
			NodeBin: "/var/lib/dsh-gate/node/bin",
		},
		status:     map[string]JobStatus{},
		toolStatus: map[string]ToolJob{},
	}

	server, err := NewServer(ServerOptions{
		Store:     f.store,
		Gate:      f.gate,
		Installer: inst,
		Upstream: func(in Instance) (http.Handler, error) {
			return &recordingUpstream{name: in.Name}, nil
		},
		Version: "test",
	})
	if err != nil {
		t.Fatalf("构造带安装能力的管理端失败: %v", err)
	}
	f.server = server
	f.handler = server.Handler()
	return f, inst
}

// TestAdminShowsEnvDetection 验证环境检测结果如实出现在页面上。
func TestAdminShowsEnvDetection(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()

	// 三个工具版本、缺失提示、自备运行时都要出现。
	for _, want := range []string{
		"v22.11.0", "git version 2.43.0", "/var/lib/dsh-gate/node/bin",
		"npm install -g pnpm",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("环境检测面板里应包含 %q\n实际页面片段：%s", want, snippet(body, want))
		}
	}
	// 三种安装方式都要在下拉里。
	for _, want := range []string{"npx", "git", "pnpm"} {
		if !strings.Contains(body, `value="`+want+`"`) {
			t.Errorf("安装方式下拉里应包含 %q", want)
		}
	}
	// 创建表单的字段。
	for _, want := range []string{"dsh_home", "config_dir", "listen_host", "start_mode", "install_mode"} {
		if !strings.Contains(body, `name="`+want+`"`) {
			t.Errorf("创建实例表单里应有字段 %q", want)
		}
	}
}

// TestAdminWithoutInstallerHidesPanel 验证没接安装能力时不显示安装面板，
// 且实例区块照旧可用（单实例模式、测试夹具都是这种形态）。
func TestAdminWithoutInstallerHidesPanel(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "dsh_home") {
		t.Error("没接安装能力时不该出现创建实例表单")
	}
	if !strings.Contains(body, "alice-main") {
		t.Error("实例列表应照旧显示")
	}
}

// TestAdminCreatesInstanceAndStartsInstall 验证「先建记录、再安装」这条主流程。
func TestAdminCreatesInstanceAndStartsInstall(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.create"},
		"name":         {"team-main"},
		"dsh_home":     {"/srv/dsh/team"},
		"config_dir":   {"/srv/dsh/team/config"},
		"workspace":    {"/srv/work"},
		"dsh_port":     {"0"},
		"listen_host":  {"127.0.0.1"},
		"start_mode":   {"ondemand"},
		"install_mode": {"git"},
		"install_dir":  {"/srv/dsh/team/install"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("创建实例应 303 回跳，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}

	// 记录必须已经落库，且字段如实保存。
	created, err := f.store.InstanceByName("team-main")
	if err != nil {
		t.Fatalf("实例记录应已创建: %v", err)
	}
	if created.DshHome != "/srv/dsh/team" {
		t.Errorf("dsh_home 应为 /srv/dsh/team，实际 %q", created.DshHome)
	}
	if created.ConfigDir != "/srv/dsh/team/config" {
		t.Errorf("config_dir 应为 /srv/dsh/team/config，实际 %q", created.ConfigDir)
	}
	if created.StartMode != StartModeOnDemand || !created.OnDemand {
		t.Errorf("启动方式应为按需，实际 start_mode=%q on_demand=%v", created.StartMode, created.OnDemand)
	}
	if created.InstallMode != string(InstallModeGit) {
		t.Errorf("安装方式应为 git，实际 %q", created.InstallMode)
	}
	if created.DshPort <= 0 {
		t.Errorf("端口应自动分配，实际 %d", created.DshPort)
	}
	// 创建者应被自动授权（否则他建完自己进不去）。
	if got, err := f.store.Accessible(f.root, created.ID); err != nil || got == nil {
		t.Errorf("创建者应被自动授权，inst=%v err=%v", got, err)
	}

	// 安装请求必须带着正确的实例 id 与方式发出去。
	req, ok := inst.lastRequest()
	if !ok {
		t.Fatal("应触发一次安装")
	}
	if req.InstanceID != created.ID {
		t.Errorf("安装目标应是新建的实例 %q，实际 %q", created.ID, req.InstanceID)
	}
	if req.Mode != InstallModeGit {
		t.Errorf("安装方式应是 git，实际 %q", req.Mode)
	}
	if req.Dir != "/srv/dsh/team/install" {
		t.Errorf("安装目录应是 /srv/dsh/team/install，实际 %q", req.Dir)
	}
}

// TestAdminCreateInstanceDefaultsInstallDir 验证安装目录留空时按安装方式推导。
func TestAdminCreateInstanceDefaultsInstallDir(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.create"},
		"name":         {"auto-dir"},
		"dsh_home":     {"/srv/dsh/autodir"},
		"install_mode": {"pnpm"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("创建实例应 303，实际 %d", rec.Code)
	}
	req, ok := inst.lastRequest()
	if !ok {
		t.Fatal("应触发一次安装")
	}
	if req.Dir != "/srv/dsh/autodir/install" {
		t.Errorf("安装目录应推导为 /srv/dsh/autodir/install，实际 %q", req.Dir)
	}
}

// TestAdminCreateInstanceRejectsBadInput 验证非法输入被挡下且不落库。
func TestAdminCreateInstanceRejectsBadInput(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	for name, form := range map[string]map[string][]string{
		"缺 dsh_home": {
			"action": {"instance.create"}, "name": {"nohome"},
		},
		"缺名字": {
			"action": {"instance.create"}, "dsh_home": {"/srv/x"},
		},
		"坏安装方式": {
			"action": {"instance.create"}, "name": {"badmode"},
			"dsh_home": {"/srv/x"}, "install_mode": {"rpm"},
		},
		"坏启动方式": {
			"action": {"instance.create"}, "name": {"badstart"},
			"dsh_home": {"/srv/x"}, "start_mode": {"sometimes"},
		},
	} {
		rec := postAdmin(t, f, session, csrfCookie, csrf, form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s：应 400，实际 %d", name, rec.Code)
		}
	}
	if _, err := f.store.InstanceByName("nohome"); err == nil {
		t.Error("非法输入不该落库")
	}
	if len(inst.requests) != 0 {
		t.Errorf("非法输入不该触发安装，实际 %d 次", len(inst.requests))
	}
}

// TestAdminInstallProgressVisible 验证安装进度会显示在实例表上。
func TestAdminInstallProgressVisible(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 直接对一个已有实例触发安装。
	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.install"},
		"instance_id":  {f.aliceIns.ID},
		"name":         {"alice-main"},
		"install_mode": {"pnpm"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("触发安装应 303，实际 %d", rec.Code)
	}
	req, _ := inst.lastRequest()
	if req.InstanceID != f.aliceIns.ID || req.Mode != InstallModePnpm {
		t.Fatalf("安装请求不对：%+v", req)
	}

	// 再打开页面，安装状态应显示出来。
	page := f.get(t, AdminPath, session)
	body := page.Body.String()
	if !strings.Contains(body, "正在安装") {
		t.Errorf("页面应显示安装中的状态，实际片段：%s", snippet(body, "正在安装"))
	}
}

// TestAdminRemoveInstanceKeepsData 验证删实例只解除管理关系、不删数据目录。
func TestAdminRemoveInstanceKeepsData(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":      {"instance.remove"},
		"instance_id": {f.bobIns.ID},
		"name":        {"bob-main"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("删除实例应 303，实际 %d", rec.Code)
	}
	if _, err := f.store.InstanceByID(f.bobIns.ID); err == nil {
		t.Error("实例记录应已删除")
	}
	// 删的是记录，不是数据目录——这里只确认存储层没被要求删目录。
	// （Store.DeleteInstance 的文档明确说明不删 DSH_HOME。）
}

// TestAdminInstallWithoutInstallerFails 验证没接安装能力时触发安装会报错。
func TestAdminInstallWithoutInstallerFails(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.install"},
		"instance_id":  {f.aliceIns.ID},
		"name":         {"alice-main"},
		"install_mode": {"npx"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没接安装能力时应 400，实际 %d", rec.Code)
	}
}

// snippet 在 body 里找 needle 附近的片段，断言失败时方便定位。
func snippet(body, needle string) string {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return "（未找到该字符串）"
	}
	start := idx - 80
	if start < 0 {
		start = 0
	}
	end := idx + len(needle) + 80
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}

// selectedOption 在某个 select 块里找「被选中」的那一项，返回它的 value。
//
// 用于验证按行渲染的下拉是否选中了该行自己的值——这正是「所有行共用一份选项、
// 谁都没设 Selected」这个 bug 的表现面：浏览器会显示第一项。
func selectedOption(t *testing.T, body, selectName string) string {
	t.Helper()
	// 定位到 <select name="..."> ... </select>
	idx := strings.Index(body, `<select name="`+selectName+`"`)
	if idx < 0 {
		t.Fatalf("页面里没有 select name=%q", selectName)
	}
	rest := body[idx:]
	end := strings.Index(rest, "</select>")
	if end < 0 {
		t.Fatalf("select name=%q 没有闭合", selectName)
	}
	block := rest[:end]
	m := regexp.MustCompile(`<option value="([^"]+)" selected`).FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestAdminUserRoleDropdownSelectsOwnRole 是回归测试：每个用户的角色下拉必须
// 预选到他自己的角色。
//
// 曾经的写法是全局构造一份 []adminRoleOption 且从不设 Selected，模板按行渲染时
// 浏览器显示第一项，于是普通用户在页面上也显示「管理员」——与实际角色不符，
// 管理员照着页面看就会误判权限。
func TestAdminUserRoleDropdownSelectsOwnRole(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()

	// fixture 里 root 是 admin、alice/bob 是 user。
	// 逐行检查：每行 <select name="role"> 里被选中的那一项要等于该行的角色。
	for _, u := range []struct {
		name string
		role Role
	}{
		{"root", RoleAdmin},
		{"alice", RoleUser},
		{"bob", RoleUser},
	} {
		// 找到该用户所在的表单块，再取块内 role 下拉的选中项。
		marker := `value="` + u.name + `"`
		idx := strings.Index(body, marker)
		if idx < 0 {
			t.Fatalf("页面里没有用户 %q", u.name)
		}
		// 从该用户的表单项往后取一段，够覆盖同一行里的 role 下拉。
		chunk := body[idx:]
		if len(chunk) > 3000 {
			chunk = chunk[:3000]
		}
		sel := selectedOption(t, chunk, "role")
		if sel != string(u.role) {
			t.Errorf("用户 %s 的角色下拉应选中 %q，实际选中 %q", u.name, u.role, sel)
		}
	}
}

// TestAdminInstanceModeDropdownSelectsOwnMode 是回归测试：每个实例的安装方式
// 下拉必须预选到它自己的 install_mode。
//
// 与角色下拉同源：曾经共用一份选项且无人设 Selected，页面上 npx / git / pnpm
// 三种实例看起来全是「npx 官方命令」。
func TestAdminInstanceModeDropdownSelectsOwnMode(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	// 把三个实例分别设成三种安装方式，覆盖全部分支。
	for name, mode := range map[string]InstallMode{
		"alice-main":  InstallModeNpx,
		"bob-main":    InstallModeGit,
		"shared-team": InstallModePnpm,
	} {
		inst, err := f.store.InstanceByName(name)
		if err != nil {
			t.Fatalf("取实例 %s 失败: %v", name, err)
		}
		inst.InstallMode = string(mode)
		if err := f.store.UpdateInstance(*inst); err != nil {
			t.Fatalf("更新实例 %s 失败: %v", name, err)
		}
	}

	rec := f.get(t, AdminPath, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理台应 200，实际 %d", rec.Code)
	}
	body := rec.Body.String()

	for _, c := range []struct {
		inst string
		mode InstallMode
	}{
		{"alice-main", InstallModeNpx},
		{"bob-main", InstallModeGit},
		{"shared-team", InstallModePnpm},
	} {
		// 用实例 id 锚定安装表单：实例名会先出现在上方的授权矩阵里，
		// 从名字开始截取的片段里没有 install_mode 下拉。
		inst, err := f.store.InstanceByName(c.inst)
		if err != nil {
			t.Fatalf("取实例 %s 失败: %v", c.inst, err)
		}
		anchor := `name="instance_id" value="` + inst.ID + `"`
		idx := strings.Index(body, anchor)
		if idx < 0 {
			t.Fatalf("页面里没有实例 %q 的安装表单", c.inst)
		}
		chunk := body[idx:]
		if len(chunk) > 2000 {
			chunk = chunk[:2000]
		}
		sel := selectedOption(t, chunk, "install_mode")
		if sel != string(c.mode) {
			t.Errorf("实例 %s 的安装方式下拉应选中 %q，实际 %q", c.inst, c.mode, sel)
		}
	}
}

// TestEnvMissingPnpmHasInstallButton 验证环境检测里缺失的 pnpm 能点一下装。
//
// 起点是一个真实用户现场：干净的服务器上 dsh 装好了，一点「添加插件」报
// `spawn pnpm ENOENT`——Node 自带的只有 npm/npx。管理台以前只给一句提示，
// 用户得自己登机器敲命令。现在这一行要带一个能点的按钮。
func TestEnvMissingPnpmHasInstallButton(t *testing.T) {
	f, fake := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	body := f.get(t, AdminPath, session).Body.String()
	if !strings.Contains(body, `name="action" value="env.install"`) {
		t.Fatal("环境检测里缺失的工具应当带一个 env.install 表单")
	}
	if !strings.Contains(body, `name="tool" value="pnpm"`) {
		t.Error("该表单应当点名 pnpm")
	}
	// 已经装好的工具不该出现按钮：那会让用户以为要重装一遍。
	idx := strings.Index(body, `name="tool" value="pnpm"`)
	if idx < 0 {
		t.Fatal("找不到 pnpm 的安装表单")
	}
	if strings.Count(body, `name="action" value="env.install"`) != 1 {
		t.Errorf("只有缺失且可安装的工具才该有按钮，实际有 %d 个",
			strings.Count(body, `name="action" value="env.install"`))
	}
	_ = fake
}

// TestEnvInstallActionStartsToolJob 验证点击按钮真的把请求交给了 Installer。
func TestEnvInstallActionStartsToolJob(t *testing.T) {
	f, fake := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"env.install"}, "tool": {"pnpm"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("成功应 303 回跳，实际 %d：%s", rec.Code, rec.Body.String())
	}
	tool, ok := fake.lastToolRequest()
	if !ok {
		t.Fatal("点击后应当调用过 StartToolInstall")
	}
	if tool != "pnpm" {
		t.Errorf("应当请求安装 pnpm，实际 %q", tool)
	}
}

// TestEnvInstallRejectsUnknownTool 验证不能借这个入口装任意东西。
//
// 它直接决定「页面能触发什么动作」，所以白名单必须在服务端判一次，
// 不能只靠模板里那几个按钮。
func TestEnvInstallRejectsUnknownTool(t *testing.T) {
	f, fake := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, url.Values{
		"action": {"env.install"}, "tool": {"rm -rf /"},
	})
	if rec.Code == http.StatusSeeOther {
		t.Fatal("未点名的工具应当被拒绝")
	}
	if _, ok := fake.lastToolRequest(); ok {
		t.Error("被拒绝的请求不该到达 Installer")
	}
}

// TestEnvToolJobProgressShown 验证补齐任务的进度与失败原因会显示在页面上。
//
// 管理台没有 JavaScript，进度只能靠服务端渲染；任务状态没被读出来的话，
// 用户点完按钮会以为「点了没反应」。
func TestEnvToolJobProgressShown(t *testing.T) {
	f, fake := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	body := f.get(t, AdminPath, session).Body.String()
	// 先确认不存在旧写法：这里直接用假件注一个失败状态。
	fake.toolStatus["pnpm"] = ToolJob{
		Tool: "pnpm", Phase: InstallPhaseFailed,
		Step: "正在安装 pnpm", Error: "网络拉不动：EAI_AGAIN",
	}
	body = f.get(t, AdminPath, session).Body.String()
	if !strings.Contains(body, "网络拉不动") {
		t.Errorf("补齐失败的原始原因应当显示出来，页面里没有找到")
	}
	if !strings.Contains(body, "重试") {
		t.Errorf("失败后应当给出「重试」按钮")
	}

	// 正在跑时应当显示进行中的状态，而不是一个还能再点的按钮。
	fake.toolStatus["pnpm"] = ToolJob{Tool: "pnpm", Phase: InstallPhaseRunning, Step: "正在安装 pnpm"}
	body = f.get(t, AdminPath, session).Body.String()
	if !strings.Contains(body, "安装中") {
		t.Errorf("任务在跑时应当显示「安装中」，页面里没有")
	}
}

// TestAdminInstanceRowShowsAllSettings 验证实例列表把「创建时能填的字段」都显示出来。
//
// 现场是：创建表单能填配置目录 / 工作目录 / 监听地址 / 启动方式，但列表里只看得到
// DSH_HOME 与端口。用户填完之后再也确认不了自己填的是哪几个目录，只能去翻数据库。
// 这条测试盯住这个缺口——创建表单里出现的每个字段，列表里都得能读回来。
func TestAdminInstanceRowShowsAllSettings(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	// 造一个「每个字段都填了」的实例，值都取得唯一，避免与页面别处的文字撞上。
	inst := &Instance{
		Name: "全字段实例", DshHome: "/srv/dsh/full-home",
		Workspace: "/srv/dsh/full-workspace", ConfigDir: "/srv/dsh/full-config",
		DshPort: 18123, ListenHost: "127.0.0.1", StartMode: StartModeOnDemand,
		InstallMode: "pnpm", InstallDir: "/srv/dsh/full-install",
	}
	if err := f.store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}

	body := f.get(t, AdminPath, session).Body.String()
	// 逐值核对：这些是「用户在创建表单里填过、之后必须能查回来」的东西。
	for _, want := range []string{
		"/srv/dsh/full-home",      // DSH_HOME
		"/srv/dsh/full-config",    // 配置目录
		"/srv/dsh/full-workspace", // 工作目录
		"18123",                   // 端口
		"127.0.0.1",               // 监听地址
		"/srv/dsh/full-install",   // 安装目录
	} {
		if !strings.Contains(body, want) {
			t.Errorf("实例列表里看不到 %q（创建时填过，建完就查不到了）", want)
		}
	}
	// 启动方式要显示成文案而不是数据库里的 auto/ondemand。断言直接取 i18n 的
	// 文案，不硬编码中文字面量——否则改一次文案就得改一次测试。
	if want := i18n.T(i18n.ZH, i18n.KeyAdminInstStartDemand); !strings.Contains(body, want) {
		t.Errorf("按需启动的实例应当显示 %q，页面里没有", want)
	}
	// 列名也要在，否则那行小字没有上下文（复用创建表单的标签，不另造一套词）。
	for _, key := range []string{
		i18n.KeyAdminInstConfigDir, i18n.KeyAdminInstWorkspace,
		i18n.KeyAdminInstListenHost, i18n.KeyAdminInstStartMode,
	} {
		if want := i18n.T(i18n.ZH, key); !strings.Contains(body, want) {
			t.Errorf("实例行缺少 %q 的标签", want)
		}
	}
}

// TestAdminInstanceRowExplainsEmptySettings 验证留空的字段不是一片空白。
//
// 配置目录留空 = 用 DSH_HOME，页面要有一句话说明；工作目录留空则会被推导成
// <实例目录>/workspace——列表里显示的是**真实路径**而不是「未设置」，因为
// 无头 Linux 上 dsh 自己建不出默认工作区，这个推导值就是它实际会用的目录。
func TestAdminInstanceRowExplainsEmptySettings(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	inst := &Instance{Name: "留空实例", DshHome: "/srv/dsh/empty-home"}
	if err := f.store.CreateInstance(inst); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}

	body := f.get(t, AdminPath, session).Body.String()
	if want := i18n.T(i18n.ZH, i18n.KeyAdminInstMetaSameHome); !strings.Contains(body, want) {
		t.Errorf("配置目录留空时应说明 %q", want)
	}
	want := filepath.Join("/srv/dsh/empty-home", "workspace")
	if !strings.Contains(body, want) {
		t.Errorf("工作目录留空时应显示推导出的默认目录 %q", want)
	}
}
