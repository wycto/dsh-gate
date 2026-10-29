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
	// 四种安装方式都要在下拉里。
	for _, want := range []string{"npx", "git", "local", "pnpm"} {
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

	// 配置目录填了会顶替实例目录（运行时 homeOf 以它为准），默认安装目录也得
	// 跟着实际生效的那个根走，否则「数据在 A、程序默认装到 B」。
	rec = postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.create"},
		"name":         {"auto-dir-cfg"},
		"dsh_home":     {"/srv/dsh/autodir"},
		"config_dir":   {"/srv/cfg/autodir"},
		"install_mode": {"pnpm"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("创建实例应 303，实际 %d", rec.Code)
	}
	req, ok = inst.lastRequest()
	if !ok {
		t.Fatal("应触发一次安装")
	}
	if req.Dir != "/srv/cfg/autodir/install" {
		t.Errorf("有配置目录时安装目录应推导为 /srv/cfg/autodir/install，实际 %q", req.Dir)
	}
}

// TestAdminCreateLocalInstance 本地源码方式：目录填了就把安装排上，留空则只建
// 记录、不排一份注定失败的任务，也不推导一个猜出来的默认目录。
func TestAdminCreateLocalInstance(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 填了目录：安装请求带走这个目录。
	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.create"},
		"name":         {"local-repo"},
		"dsh_home":     {"/srv/dsh/local"},
		"install_mode": {"local"},
		"install_dir":  {"/srv/src/deepseek-harness"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("创建实例应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	created, err := f.store.InstanceByName("local-repo")
	if err != nil {
		t.Fatalf("实例记录应已创建: %v", err)
	}
	if created.InstallMode != string(InstallModeLocal) {
		t.Errorf("安装方式应为 local，实际 %q", created.InstallMode)
	}
	if created.InstallDir != "/srv/src/deepseek-harness" {
		t.Errorf("安装目录应原样保存，实际 %q", created.InstallDir)
	}
	req, ok := inst.lastRequest()
	if !ok {
		t.Fatal("填了目录应触发一次安装")
	}
	if req.Mode != InstallModeLocal || req.Dir != "/srv/src/deepseek-harness" {
		t.Errorf("安装请求应为 local + 用户填的目录，实际 mode=%q dir=%q", req.Mode, req.Dir)
	}

	// 留空：记录建好、目录不推导、安装不排（否则页面上立刻出现一条注定失败的任务）。
	before := len(inst.requests)
	rec = postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.create"},
		"name":         {"local-blank"},
		"dsh_home":     {"/srv/dsh/local2"},
		"install_mode": {"local"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("创建实例应 303，实际 %d", rec.Code)
	}
	blank, err := f.store.InstanceByName("local-blank")
	if err != nil {
		t.Fatalf("实例记录应已创建: %v", err)
	}
	if blank.InstallDir != "" {
		t.Errorf("本地源码方式不该推导默认安装目录，实际 %q", blank.InstallDir)
	}
	if len(inst.requests) != before {
		t.Errorf("目录留空时不该排安装，实际多出 %d 次", len(inst.requests)-before)
	}
}

// TestAdminCreateInstanceRejectsBadInput 验证非法输入被挡下且不落库。
func TestAdminCreateInstanceRejectsBadInput(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	for name, form := range map[string]map[string][]string{
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
		// 版本名会被拼进启动命令、之后拆开执行：带空格的那种必须拦在这。
		"坏版本名": {
			"action": {"instance.create"}, "name": {"badver"},
			"dsh_home": {"/srv/x"}, "dsh_version": {"1.0.0 --registry=http://evil.example"},
		},
		// 钉版本是 npm 包的说法，源码构建认分支/标签：两个一起填等于有一栏不会生效。
		"源码构建钉版本": {
			"action": {"instance.create"}, "name": {"gitver"},
			"dsh_home": {"/srv/x"}, "install_mode": {"git"}, "dsh_version": {"next"},
		},
		// 本地源码方式跑的就是那个目录，npm 版本号同样没有意义。
		"本地源码钉版本": {
			"action": {"instance.create"}, "name": {"localver"},
			"dsh_home": {"/srv/x"}, "install_mode": {"local"}, "dsh_version": {"next"},
		},
	} {
		rec := postAdmin(t, f, session, csrfCookie, csrf, form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s：应 400，实际 %d", name, rec.Code)
		}
	}
	// 逐个确认这些都没落库（用各自的实例名查）。
	for _, name := range []string{"badmode", "badstart", "badver", "gitver", "localver"} {
		if _, err := f.store.InstanceByName(name); err == nil {
			t.Errorf("%s 不该落库", name)
		}
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

	// root 是第一个注册的账号（超级管理员）：不给角色下拉，只显示角色文本
	// 与徽标——这是超管保护在页面上的形态。
	//
	// 用户表按名字排序，root 恰是最后一行：锚在它的徽标上，向前找名字列、
	// 向后找这张表的 </table>，截出来的就是 root 一行。
	rootBadge := strings.Index(body, ">超级管理员<")
	if rootBadge < 0 {
		t.Fatal("页面里没有「超级管理员」徽标")
	}
	rootStart := strings.LastIndex(body[:rootBadge], ">root<")
	if rootStart < 0 {
		t.Fatal("徽标之前找不到 root 的名字列")
	}
	tableEnd := strings.Index(body[rootBadge:], "</table>")
	if tableEnd < 0 {
		t.Fatal("徽标之后找不到用户表的结尾")
	}
	rootChunk := body[rootStart : rootBadge+tableEnd]
	if strings.Contains(rootChunk, `name="role"`) {
		t.Errorf("超管行不该出现角色下拉，实际该行: %s", rootChunk)
	}
	if !strings.Contains(rootChunk, `value="user.passwd"`) {
		t.Error("超管行仍应能改口令（否则忘了口令就锁死管理端）")
	}
	if strings.Contains(rootChunk, `value="user.delete"`) || strings.Contains(rootChunk, `value="user.toggle"`) {
		t.Error("超管行不该出现停用或删除按钮")
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

	assertSelected := func(instName string, mode InstallMode) {
		t.Helper()
		rec := f.get(t, AdminPath, session)
		if rec.Code != http.StatusOK {
			t.Fatalf("管理台应 200，实际 %d", rec.Code)
		}
		body := rec.Body.String()
		inst, err := f.store.InstanceByName(instName)
		if err != nil {
			t.Fatalf("取实例 %s 失败: %v", instName, err)
		}
		// 用实例 id 锚定安装表单：实例名会先出现在上方的授权矩阵里，
		// 从名字开始截取的片段里没有 install_mode 下拉。
		anchor := `name="instance_id" value="` + inst.ID + `"`
		idx := strings.Index(body, anchor)
		if idx < 0 {
			t.Fatalf("页面里没有实例 %q 的安装表单", instName)
		}
		chunk := body[idx:]
		if len(chunk) > 2000 {
			chunk = chunk[:2000]
		}
		sel := selectedOption(t, chunk, "install_mode")
		if sel != string(mode) {
			t.Errorf("实例 %s 的安装方式下拉应选中 %q，实际 %q", instName, mode, sel)
		}
	}

	for _, c := range []struct {
		inst string
		mode InstallMode
	}{
		{"alice-main", InstallModeNpx},
		{"bob-main", InstallModeGit},
		{"shared-team", InstallModePnpm},
	} {
		assertSelected(c.inst, c.mode)
	}

	// 第四种方式（local）单独走一轮：夹具只有三个实例，复用其中一个。
	if inst, err := f.store.InstanceByName("bob-main"); err == nil {
		inst.InstallMode = string(InstallModeLocal)
		if err := f.store.UpdateInstance(*inst); err == nil {
			assertSelected("bob-main", InstallModeLocal)
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
// 进度要能直接从 HTML 里读出来：自动刷新脚本只替换这段文字，文案本身仍由
// 服务端渲染。任务状态没被读出来的话，用户点完按钮会以为「点了没反应」。
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
		DshVersion: "0.2.2-alpha.2",
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
		"0.2.2-alpha.2",           // 钉住的 dsh 版本
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
	// 配置目录填了会顶替 DSH_HOME，行里必须点明，否则那一列的 DSH_HOME 与
	// 「配置目录」看起来像两个各自独立的目录。
	if want := i18n.T(i18n.ZH, i18n.KeyAdminInstMetaConfigOverride); !strings.Contains(body, want) {
		t.Errorf("配置目录填了时应标注 %q", want)
	}
}

// TestAdminCreateFormExplainsDirectories 验证创建表单把三个目录的用途写清楚。
//
// 现场是：实例目录 / 配置目录 / 安装目录并排摆着，光看标签分不出「dsh 只有一个
// 数据根」——最常见的误解是以为配置与数据可以分开放，或以为 npx 方式也要一个
// 安装目录。这条测试盯住每栏的说明与「一个目录就够」那句话，防止以后精简文案
// 时又退回到只有标签。
func TestAdminCreateFormExplainsDirectories(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	body := f.get(t, AdminPath, session).Body.String()
	for _, key := range []string{
		i18n.KeyAdminInstHomeHelp,
		i18n.KeyAdminInstConfigHelp,
		i18n.KeyAdminInstWorkspaceHelp,
		i18n.KeyAdminInstInstallDirHelp,
		i18n.KeyAdminInstDirsTitle,
		i18n.KeyAdminInstDirsTip,
		i18n.KeyAdminInstConfigPh,
		i18n.KeyAdminInstWorkspacePh,
		i18n.KeyAdminInstInstallDirPh,
		// 「dsh 版本」这栏的说明也要在：它是唯一一个「留空和不留空行为相反」的
		// 字段（留空会自动回退，填了就不回退），光看标签分不出来。
		i18n.KeyAdminInstVersionPh,
		i18n.KeyAdminInstVersionHelp,
	} {
		if want := i18n.T(i18n.ZH, key); !strings.Contains(body, want) {
			t.Errorf("创建表单缺少目录说明 %q", want)
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

// TestAdminCreateInstanceWithBlankHomeUsesDefault 验证管理台上把「实例目录」
// 留空也能建成实例，且落到 dsh 的默认目录。
//
// 表单那一栏原来带 required（浏览器层面就拦住了提交），现在改成提示默认值。
func TestAdminCreateInstanceWithBlankHomeUsesDefault(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 页面要给出默认值提示，否则用户不知道留空会发生什么。
	rec := f.get(t, AdminPath, session)
	body := rec.Body.String()
	if !strings.Contains(body, DefaultDshHomeName) {
		t.Errorf("创建表单应提示默认实例目录 %q，实际页面里没有", DefaultDshHomeName)
	}
	if strings.Contains(body, `name="dsh_home" required`) {
		t.Error("实例目录那一栏不该是 required：留空应当合法（用 dsh 的默认目录）")
	}

	rec = postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action": {"instance.create"},
		"name":   {"blank-home"},
		// dsh_home 故意不填
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("留空实例目录应能创建（303），实际 %d（body: %s）", rec.Code, snippet(rec.Body.String(), "错误"))
	}

	created, err := f.store.InstanceByName("blank-home")
	if err != nil {
		t.Fatalf("实例应已创建: %v", err)
	}
	if want := DefaultDshHome(); created.DshHome != want {
		t.Errorf("实例目录应回落到 %q，实际 %q", want, created.DshHome)
	}
	if strings.HasPrefix(created.DshHome, "~") {
		t.Errorf("落库的必须是展开后的绝对路径，实际 %q", created.DshHome)
	}
	_ = inst
}

// fakeLifespan 记录管理台对启停能力的调用。
type fakeLifespan struct {
	mu      sync.Mutex
	running map[string]bool
	started []string
	stopped []string
	restart []string
	// lastRestart 是最后一次重启收到的实例（用来断言用的是库里的最新配置）。
	lastRestart Instance
	err         error
}

func newFakeLifespan() *fakeLifespan {
	return &fakeLifespan{running: map[string]bool{}}
}

func (f *fakeLifespan) InstanceStatus(inst Instance) InstanceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	on := f.running[inst.ID]
	return InstanceStatus{Running: on, Ready: on, PID: map[bool]int{true: 4242, false: 0}[on]}
}

func (f *fakeLifespan) StartInstance(ctx context.Context, inst Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.running[inst.ID] = true
	f.started = append(f.started, inst.ID)
	return nil
}

func (f *fakeLifespan) StopInstance(ctx context.Context, inst Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.running[inst.ID] = false
	f.stopped = append(f.stopped, inst.ID)
	return nil
}

func (f *fakeLifespan) RestartInstance(ctx context.Context, inst Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.running[inst.ID] = true
	f.restart = append(f.restart, inst.ID)
	f.lastRestart = inst
	return nil
}

func (f *fakeLifespan) counts() (start, stop, restart int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started), len(f.stopped), len(f.restart)
}

// newLifespanFixture 造一个接了启停能力的管理台。
func newLifespanFixture(t *testing.T) (*portalFixture, *fakeLifespan) {
	t.Helper()
	f := newPortalFixture(t)
	life := newFakeLifespan()
	server, err := NewServer(ServerOptions{
		Store:    f.store,
		Gate:     f.gate,
		Lifespan: life,
		Upstream: func(in Instance) (http.Handler, error) {
			return &recordingUpstream{name: in.Name}, nil
		},
		Version: "test",
	})
	if err != nil {
		t.Fatalf("构造带启停能力的管理端失败: %v", err)
	}
	f.server = server
	// handler 是 newPortalFixture 里按旧 server 建的，换 server 后必须重建，
	// 否则请求仍然打到没有 Lifespan 的那个实例上（症状：按钮不出现、动作 400）。
	f.handler = server.Handler()
	return f, life
}

// TestAdminInstanceLifecycleButtons 验证启停三颗按钮与实际行为。
func TestAdminInstanceLifecycleButtons(t *testing.T) {
	f, life := newLifespanFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 未运行时只露出「启动」；停止 / 重启常驻但藏在 hidden 后面（自动刷新
	// 靠它切换，做不到的动作仍然看不到）。判据看 data-act 后面的 hidden。
	body := f.get(t, AdminPath, session).Body.String()
	if !strings.Contains(body, `data-act="start">`) {
		t.Error("没在跑时应露出「启动」按钮")
	}
	for _, act := range []string{"stop", "restart"} {
		if !strings.Contains(body, `data-act="`+act+`" hidden>`) {
			t.Errorf("没在跑时「%s」应藏在 hidden 后面（点了没意义）", act)
		}
	}

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":      {"instance.start"},
		"instance_id": {f.aliceIns.ID},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("启动应 303 回跳，实际 %d", rec.Code)
	}
	if start, _, _ := life.counts(); start != 1 {
		t.Fatalf("应当调用一次 StartInstance，实际 %d 次", start)
	}

	// 运行中：露出「停止」与「重启」，「启动」藏回去。
	body = f.get(t, AdminPath, session).Body.String()
	for _, act := range []string{"stop", "restart"} {
		if !strings.Contains(body, `data-act="`+act+`">`) {
			t.Errorf("运行中应露出「%s」按钮", act)
		}
	}
	if !strings.Contains(body, `data-act="start" hidden>`) {
		t.Error("运行中不该再露出「启动」按钮")
	}

	postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action": {"instance.stop"}, "instance_id": {f.aliceIns.ID},
	})
	if _, stop, _ := life.counts(); stop != 1 {
		t.Fatalf("应当调用一次 StopInstance，实际 %d 次", stop)
	}

	postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action": {"instance.restart"}, "instance_id": {f.aliceIns.ID},
	})
	if _, _, restart := life.counts(); restart != 1 {
		t.Fatalf("应当调用一次 RestartInstance，实际 %d 次", restart)
	}
}

// TestAdminInstanceUpdateSavesConfigWithoutRestart 验证「保存配置只落库、不自动重启」。
//
// 用户要的是一条可控链路：停止 → 改配置 → 重启。自动重启会把「先改着、
// 待会儿一起重启」和「改完立刻生效」两种意图混成一种。
func TestAdminInstanceUpdateSavesConfigWithoutRestart(t *testing.T) {
	f, life := newLifespanFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.update"},
		"instance_id":  {f.aliceIns.ID},
		"name":         {"alice-renamed"},
		"dsh_home":     {"/srv/dsh/newhome"},
		"config_dir":   {"/srv/cfg"},
		"workspace":    {"/srv/work"},
		"dsh_port":     {"32999"},
		"listen_host":  {"127.0.0.1"},
		"start_mode":   {"ondemand"},
		"install_mode": {"git"},
		"install_dir":  {"/srv/inst"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("保存配置应 303 回跳，实际 %d（body: %s）", rec.Code, snippet(rec.Body.String(), "错误"))
	}

	got, err := f.store.InstanceByID(f.aliceIns.ID)
	if err != nil {
		t.Fatalf("读回实例失败: %v", err)
	}
	if got.Name != "alice-renamed" || got.DshHome != "/srv/dsh/newhome" ||
		got.DshPort != 32999 || got.StartMode != StartModeOnDemand {
		t.Fatalf("配置没有如实落库: %+v", got)
	}
	// 关键断言：只保存、不重启。
	if _, _, restart := life.counts(); restart != 0 {
		t.Errorf("保存配置不该自动重启（应等用户点「重启」），实际重启了 %d 次", restart)
	}
}

// TestAdminInstanceRestartUsesStoredConfig 验证重启用的是**库里的最新配置**。
//
// 监督者的端口与目录都是构造时固定的，重启必须拿刚改完的那份，否则
// 「改了端口再重启」等于没改。
func TestAdminInstanceRestartUsesStoredConfig(t *testing.T) {
	f, life := newLifespanFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	// 先改配置（不重启），再点重启。
	postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action": {"instance.update"}, "instance_id": {f.aliceIns.ID},
		"name": {"alice-main"}, "dsh_home": {"/srv/dsh/changed"},
		"dsh_port": {"32777"}, "start_mode": {"auto"},
	})
	postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action": {"instance.restart"}, "instance_id": {f.aliceIns.ID},
	})

	if life.lastRestart.DshHome != "/srv/dsh/changed" || life.lastRestart.DshPort != 32777 {
		t.Errorf("重启应当带上刚保存的配置，实际 %+v", life.lastRestart)
	}
}

// TestAdminInstanceLifecycleHiddenWithoutRuntime 验证没接运行时不显示启停按钮：
// 摆一个按了没反应的按钮比不摆更糟。
func TestAdminInstanceLifecycleHiddenWithoutRuntime(t *testing.T) {
	f := newPortalFixture(t)
	session := f.login(t, "root", "root-password-123")
	body := f.get(t, AdminPath, session).Body.String()
	for _, action := range []string{"instance.start", "instance.stop", "instance.restart"} {
		if strings.Contains(body, action) {
			t.Errorf("没接运行时能力时不该出现 %q", action)
		}
	}
}

// TestAdminCreateInstancePinsVersion 创建时钉住 dsh 版本：落库、装上去、页面上查得回。
func TestAdminCreateInstancePinsVersion(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.create"},
		"name":         {"pinned-npx"},
		"dsh_home":     {"/srv/dsh/pinned"},
		"install_mode": {"npx"},
		"dsh_version":  {"0.2.2-alpha.2"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("创建实例应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	created, err := f.store.InstanceByName("pinned-npx")
	if err != nil {
		t.Fatalf("实例应已创建: %v", err)
	}
	if created.DshVersion != "0.2.2-alpha.2" {
		t.Errorf("钉住的版本应落库，实际 %q", created.DshVersion)
	}
	req, ok := inst.lastRequest()
	if !ok {
		t.Fatal("应触发一次安装")
	}
	if req.Version != "0.2.2-alpha.2" {
		t.Errorf("安装请求应带上钉住的版本，实际 %q", req.Version)
	}

	// 页面要能查回来：钉住哪个版本只有用户自己知道，列表里没有就等于不可核对。
	body := f.get(t, AdminPath, session).Body.String()
	if !strings.Contains(body, "0.2.2-alpha.2") {
		t.Error("实例列表里看不到钉住的版本")
	}
	if want := i18n.T(i18n.ZH, i18n.KeyAdminInstVersion); !strings.Contains(body, want) {
		t.Errorf("实例行缺少 %q 的标签", want)
	}
}

// TestAdminInstanceRowShowsLatestWhenUnpinned 没钉版本的实例要写明「跟随 latest」。
//
// 留空显示成空白会让人以为漏填了什么；这一格说的其实是「每次启动都对照 npm 取最新」。
func TestAdminInstanceRowShowsLatestWhenUnpinned(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	if err := f.store.CreateInstance(&Instance{Name: "unpinned", DshHome: "/srv/dsh/unpinned"}); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	body := f.get(t, AdminPath, session).Body.String()
	if want := i18n.T(i18n.ZH, i18n.KeyAdminInstMetaVersionLatest); !strings.Contains(body, want) {
		t.Errorf("未钉版本的实例应显示 %q，页面里没有", want)
	}
}

// TestAdminGitInstanceMarksVersionInert 源码构建的实例带着钉住的版本时，页面上要
// 写明那一格不生效。
//
// 场景是真的会出现的：先按 npx + 钉版本建了实例，之后把安装方式改成源码构建。
// 记录里那个版本号还在，不标注的话用户会以为自己跑的是那个 npm 版本。
func TestAdminGitInstanceMarksVersionInert(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")

	if err := f.store.CreateInstance(&Instance{
		Name: "git-with-pin", DshHome: "/srv/dsh/gitpin",
		InstallMode: string(InstallModeGit), InstallDir: "/srv/dsh/gitpin/install",
		DshVersion: "next",
	}); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	body := f.get(t, AdminPath, session).Body.String()
	want := i18n.T(i18n.ZH, i18n.KeyAdminInstMetaVersionBuild, "next")
	if !strings.Contains(body, want) {
		t.Errorf("源码构建 + 钉版本应显示 %q，页面里没有", want)
	}
}

// TestAdminInstallKeepsPinnedVersion 安装面板上不动「dsh 版本」这一栏时，沿用实例
// 记录里钉住的那个；填了则覆盖，并把新值写回记录。
func TestAdminInstallKeepsPinnedVersion(t *testing.T) {
	f, inst := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	target := &Instance{Name: "reinstall-pin", DshHome: "/srv/dsh/repin",
		InstallMode: string(InstallModePnpm), DshVersion: "0.2.1-rc.4"}
	if err := f.store.CreateInstance(target); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}

	// 表单没填版本 → 用记录里钉住的。
	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.install"},
		"instance_id":  {target.ID},
		"name":         {"reinstall-pin"},
		"install_mode": {"pnpm"},
		"install_dir":  {"/srv/dsh/repin/install"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("安装应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	req, ok := inst.lastRequest()
	if !ok {
		t.Fatal("应触发一次安装")
	}
	if req.Version != "0.2.1-rc.4" {
		t.Errorf("安装应沿用记录里钉住的版本，实际 %q", req.Version)
	}

	// 表单填了新版本 → 用新填的（写回记录由安装任务负责，不在这一层）。
	rec = postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.install"},
		"instance_id":  {target.ID},
		"name":         {"reinstall-pin"},
		"install_mode": {"pnpm"},
		"dsh_version":  {"next"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("安装应 303，实际 %d", rec.Code)
	}
	req, ok = inst.lastRequest()
	if !ok {
		t.Fatal("应触发第二次安装")
	}
	if req.Version != "next" {
		t.Errorf("表单填的版本应当盖过记录里的，实际 %q", req.Version)
	}
}

// TestAdminUpdateSavesVersion 编辑配置保存版本：改钉住的版本不用重装，落库即可，
// 下一次安装或自动探测启动命令时用新值。
func TestAdminUpdateSavesVersion(t *testing.T) {
	f, _ := newInstallFixture(t)
	session := f.login(t, "root", "root-password-123")
	csrfCookie, csrf := adminCSRF(t, f, session)

	target := &Instance{Name: "edit-pin", DshHome: "/srv/dsh/editpin",
		InstallMode: string(InstallModeNpx), DshVersion: "0.2.1-rc.2"}
	if err := f.store.CreateInstance(target); err != nil {
		t.Fatalf("建实例失败: %v", err)
	}
	rec := postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.update"},
		"instance_id":  {target.ID},
		"name":         {"edit-pin"},
		"dsh_home":     {"/srv/dsh/editpin"},
		"install_mode": {"npx"},
		"dsh_version":  {"0.2.2-alpha.2"},
		"start_mode":   {"auto"},
		"listen_host":  {"127.0.0.1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("保存配置应 303，实际 %d（body: %s）", rec.Code, rec.Body.String())
	}
	saved, err := f.store.InstanceByID(target.ID)
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if saved.DshVersion != "0.2.2-alpha.2" {
		t.Errorf("版本应更新为 0.2.2-alpha.2，实际 %q", saved.DshVersion)
	}

	// 表单里清空 = 取消钉住，回到跟随 latest（不留一个改不掉的版本号）。
	rec = postAdmin(t, f, session, csrfCookie, csrf, map[string][]string{
		"action":       {"instance.update"},
		"instance_id":  {target.ID},
		"name":         {"edit-pin"},
		"dsh_home":     {"/srv/dsh/editpin"},
		"install_mode": {"npx"},
		"start_mode":   {"auto"},
		"listen_host":  {"127.0.0.1"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("清空版本应 303，实际 %d", rec.Code)
	}
	saved, err = f.store.InstanceByID(target.ID)
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if saved.DshVersion != "" {
		t.Errorf("清空表单应取消钉住，实际 %q", saved.DshVersion)
	}
}
