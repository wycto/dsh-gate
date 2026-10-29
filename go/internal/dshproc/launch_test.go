package dshproc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 本文件用「让测试二进制自己扮演 dsh」的方式验证**拉起**这条路径。
//
// 为什么非要真起一个进程：拉起路径上有太多只有真进程才暴露的问题——
// 命令拼错、子进程起不来、端口迟迟不监听、启动输出里没有令牌、子进程退出
// 后就绪状态没复位。这些在「接管一个现成 dsh」的测试里一条都碰不到。
//
// 做法沿用 Go 测试的标准套路：设置环境变量后重新执行测试二进制，让它在
// TestMain 里改行当服务器。

const (
	fakeEnv    = "DSH_GATE_FAKE_DSH"
	fakePort   = "DSH_GATE_FAKE_PORT"
	fakeToken  = "DSH_GATE_FAKE_TOKEN"
	fakeHome   = "DSH_GATE_FAKE_HOME"
	fakePrefix = "dsh web: http://"
)

// TestMain 在设置环境变量时把测试二进制变成一个假 dsh。
func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "1" {
		os.Exit(m.Run())
	}
	runFakeDSH()
}

// runFakeDSH 模拟 dsh web 的对外行为：打印带令牌的启动 URL，然后按
// dsh 的规则响应请求（令牌换 Cookie、API 校验会话、Homepage 是 SPA）。
func runFakeDSH() {
	port := os.Getenv(fakePort)
	token := os.Getenv(fakeToken)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			if got := r.URL.Query().Get("token"); got != "" {
				if got != token {
					http.Error(w, "unauthorized: invalid startup token", http.StatusUnauthorized)
					return
				}
				http.SetCookie(w, &http.Cookie{
					Name: "dsh.sid", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
				})
				http.Redirect(w, r, "./", http.StatusSeeOther)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>DeepSeek Harness</title></head><body><div id="root"></div></body></html>`)
	})
	mux.HandleFunc("/api/remote.mux", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upgrade required", http.StatusUpgradeRequired)
	})

	fmt.Printf("%s127.0.0.1:%s/?token=%s\n", fakePrefix, port, token)
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "fake dsh stopped: %v\n", err)
		os.Exit(1)
	}
}

// freePort 向内核要一个当前空闲的端口。
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// startFakeDSH 安排好环境变量，让子进程扮演 dsh。
func startFakeDSH(t *testing.T, port int, token string) {
	t.Helper()
	t.Setenv(fakeEnv, "1")
	t.Setenv(fakePort, strconv.Itoa(port))
	t.Setenv(fakeToken, token)
}

// quoteCommandArg 把一个路径写成 SplitCommand 眼里的一个字面量参数。
//
// 命令模板按 POSIX 引号规则拆、反斜杠是转义符，而 Windows 上的路径全是
// 反斜杠——裸写会被拆成一个不存在的假路径。套单引号即可：单引号内一切按
// 字面量处理，两个平台都成立。
func quoteCommandArg(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// fakeDSHCommand 把「测试二进制自己」写成一段 SplitCommand 认得的话。
//
// 这一批用真进程验证拉起路径的测试都靠它：少了引号，Windows 上 fork/exec 报
// 「找不到文件」，整批拉起测试一起变成假失败。
func fakeDSHCommand(suffix string) string {
	return quoteCommandArg(filepath.Clean(os.Args[0])) + suffix
}

// launchOptions 构造一次拉起所需的配置。
func launchOptions(t *testing.T, port int) Options {
	return Options{
		Command:      fakeDSHCommand(" --host {host} --port {port} --no-open"),
		Host:         "127.0.0.1",
		Port:         port,
		Home:         t.TempDir(),
		AutoStart:    true,
		Adopt:        true,
		Restart:      false,
		StartTimeout: 20 * time.Second,
		StopTimeout:  3 * time.Second,
	}
}

// TestLaunchBecomesReady 覆盖「网关自己把 dsh 拉起来」这条主路径。
func TestLaunchBecomesReady(t *testing.T) {
	port := freePort(t)
	token := "launch-token-1234"
	startFakeDSH(t, port, token)

	supervisor := New(launchOptions(t, port))
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatalf("拉起 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = supervisor.Shutdown(t.Context()) })

	if !supervisor.Ready() {
		t.Error("端口已监听之后必须处于就绪状态，否则网关会一直显示「正在启动」")
	}
	if !supervisor.Managed() {
		t.Error("自己拉起的 dsh 应当被标记为托管")
	}
	if supervisor.Adopted() {
		t.Error("自己拉起的 dsh 不应被标记为接管")
	}
	if supervisor.PID() <= 0 {
		t.Error("应当拿到子进程号")
	}
	if got := supervisor.Token(); got != token {
		t.Errorf("应当从启动输出里读到令牌 %q，实际 %q", token, got)
	}
	if got := supervisor.LastError(); got != "" {
		t.Errorf("没有失败过就不该有错误记录，实际 %q", got)
	}

	// WaitReady 必须已经能立刻返回。
	readyCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := supervisor.WaitReady(readyCtx); err != nil {
		t.Errorf("WaitReady 应当立即返回，实际: %v", err)
	}
}

// TestSecondSupervisorAdopts 验证「端口已经被占用 → 直接代理，不重复启动」。
//
// 这正是需求里那句「如果 dsh 端口已经被 dsh 启动使用了，那么直接代理使用」。
func TestSecondSupervisorAdopts(t *testing.T) {
	port := freePort(t)
	token := "adopt-token-5678"
	startFakeDSH(t, port, token)

	first := New(launchOptions(t, port))
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("拉起 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = first.Shutdown(t.Context()) })

	second := New(launchOptions(t, port))
	second.opts.Token = "" // 接管时并不知道令牌
	if err := second.Start(t.Context()); err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	t.Cleanup(func() { _ = second.Shutdown(t.Context()) })

	if !second.Ready() {
		t.Error("接管后应当立即就绪")
	}
	if second.Managed() {
		t.Error("接管的实例不应被标记为托管")
	}
	if !second.Adopted() {
		t.Error("接管后应当被标记为已接管")
	}
	if second.PID() != 0 {
		t.Error("接管的实例不掌握子进程，PID 应当为 0")
	}
	if second.Token() != "" {
		t.Errorf("接管时没有令牌来源，不该凭空有令牌：%q", second.Token())
	}
}

// TestStopAdoptedFreesPort 验证「停掉一个接管来的实例」真的能把端口让开。
//
// 这是用户报的那个 bug 的直接回归：管理台上点了停止，屏幕上写着「已停止」，
// 端口却照旧有人监听。原因是接管来的实例没有子进程句柄，Shutdown 对它是趟空跑；
// 而 Windows 上连「端口上是哪个进程」都查不出来，于是连想停都没有入口。
func TestStopAdoptedFreesPort(t *testing.T) {
	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	startFakeDSH(t, port, "adopt-stop-token")

	// 先把假 dsh 起在端口上，模拟「别处已经在跑的 dsh」。Restart=false 是关键：
	// 它被外力杀掉时那个监督者不该重拉，否则端口又被人占上，断言就没法做。
	ownerOptions := launchOptions(t, port)
	ownerOptions.Restart = false
	owner := New(ownerOptions)
	if err := owner.Start(t.Context()); err != nil {
		t.Fatalf("起假 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = owner.Shutdown(context.Background()) })

	if findPID(port) <= 0 {
		t.Skip("这台机器查不到端口上的进程号，接管实例没有「按进程号停止」这条路可验")
	}

	adopter := New(launchOptions(t, port))
	adopter.opts.Token = "" // 接管时并不知道令牌
	if err := adopter.Start(t.Context()); err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	if !adopter.Adopted() {
		t.Fatal("端口已被占用，应当走接管")
	}

	if err := adopter.StopAdopted(t.Context()); err != nil {
		t.Fatalf("停掉接管实例应当成功，实际报错: %v", err)
	}
	if Busy(addr, 500*time.Millisecond) {
		t.Errorf("StopAdopted 之后端口 %s 仍有人监听——用户看到的「停不掉」就是这样", addr)
	}
	if adopter.Adopted() {
		t.Error("确认端口让开之后不该还标着「接管」：否则状态页把已经停掉的进程说成还在")
	}
}

// TestStopAdoptedLeavesOwnChildAlone 验证只停接管实例，不动网关自己拉起的子进程。
//
// 这条边界很重要：管理端自己退出时也会走停止路径，那时把用户自己在跑的 dsh
// 顺手杀掉是没礼貌的；反过来，网关自己托管的孩子必须归 Shutdown 管，
// StopAdopted 对它应该是空操作。
func TestStopAdoptedLeavesOwnChildAlone(t *testing.T) {
	port := freePort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	startFakeDSH(t, port, "own-child-token")

	supervisor := New(launchOptions(t, port))
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatalf("拉起 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = supervisor.Shutdown(context.Background()) })

	if err := supervisor.StopAdopted(t.Context()); err != nil {
		t.Fatalf("对自家子进程应当是空操作，实际报错: %v", err)
	}
	if !Busy(addr, 500*time.Millisecond) {
		t.Error("网关自己拉起的 dsh 不该被 StopAdopted 停掉")
	}
}

// TestStartRefusesWhenPortTaken 验证 --adopt=false 时端口冲突会直接报错。
func TestStartRefusesWhenPortTaken(t *testing.T) {
	port := freePort(t)
	startFakeDSH(t, port, "refuse-token")

	first := New(launchOptions(t, port))
	if err := first.Start(t.Context()); err != nil {
		t.Fatalf("拉起 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = first.Shutdown(t.Context()) })

	second := New(launchOptions(t, port))
	second.opts.Adopt = false
	err := second.Start(t.Context())
	if err == nil {
		t.Fatal("端口被占用且禁止接管时应当报错")
	}
	if !strings.Contains(err.Error(), "已被占用") {
		t.Errorf("错误信息应当说明端口被占用，实际: %v", err)
	}
}

// TestStartRefusesWhenNotAutoStart 验证「不自动拉起 + 端口空闲」时的提示。
func TestStartRefusesWhenNotAutoStart(t *testing.T) {
	port := freePort(t)
	options := launchOptions(t, port)
	options.AutoStart = false
	options.Command = "" // 没有命令可用，更不能指望它能起来

	err := New(options).Start(t.Context())
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "auto-start") {
		t.Errorf("错误信息应当提示 --auto-start，实际: %v", err)
	}
}

// TestShutdownStopsChild 验证停机时子进程确实被收走。
//
// 这是「Ctrl-C 之后不留一堆僵尸 dsh」的保证：dsh 本身会在退出时 dispose
// 插件树，被强杀可能留下半写的会话索引。
func TestShutdownStopsChild(t *testing.T) {
	port := freePort(t)
	startFakeDSH(t, port, "shutdown-token")

	supervisor := New(launchOptions(t, port))
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatalf("拉起 dsh 失败: %v", err)
	}
	pid := supervisor.PID()

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := supervisor.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if supervisor.Ready() {
		t.Error("停止后不应再处于就绪状态")
	}
	if supervisor.Running() {
		t.Error("停止后子进程不应还在运行")
	}
	// 端口应当被释放。
	deadline := time.Now().Add(5 * time.Second)
	for Busy("127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if Busy("127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond) {
		t.Errorf("进程 %d 停止后端口仍被占用", pid)
	}
}

// TestFailedLaunchRecordsError 验证 dsh 起不来时错误被记下来。
//
// 等待页面会把它显示给用户——没有这条，一次「打不开」就只剩下一个永远
// 转圈的圈。
func TestFailedLaunchRecordsError(t *testing.T) {
	port := freePort(t)
	// 让子进程立刻失败：命令存在，但参数让它退出。
	options := Options{
		Command:      fakeDSHCommand(" --unsupported-flag-xyz"),
		Host:         "127.0.0.1",
		Port:         port,
		Home:         t.TempDir(),
		AutoStart:    true,
		Adopt:        false,
		Restart:      false,
		StartTimeout: 3 * time.Second,
		StopTimeout:  time.Second,
	}
	supervisor := New(options)

	err := supervisor.Start(t.Context())
	if err == nil {
		t.Skip("这条命令恰好能跑起来，跳过")
	}
	if !strings.Contains(err.Error(), "没有监听") {
		t.Errorf("错误应当说明 dsh 没有开始监听，实际: %v", err)
	}
	if supervisor.Ready() {
		t.Error("失败后不应处于就绪状态")
	}
	if got := supervisor.LastError(); got == "" {
		t.Error("失败原因应当被记录下来，供等待页面显示")
	}
}

// TestEnsureWorkspaceDirCreatesMissing 验证工作目录不存在时会被建出来。
//
// 为什么值得一条测试：实例的工作目录是子进程的 cwd，缺了它 dsh 根本起不来。
// 而报错完全指错方向——「fork/exec /path/to/npx: no such file or directory」
// 让人以为 npx 没装，实际上缺的是目录。这条测试把这个坑钉住。
func TestEnsureWorkspaceDirCreatesMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested", "workspace")

	if err := ensureWorkspaceDir(dir, nil); err != nil {
		t.Fatalf("目录不存在时应当自动创建，实际报错: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("自动创建后目录仍不存在: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("%s 应当是目录", dir)
	}

	// 再调一次必须幂等：重启实例会反复走这条路。
	if err := ensureWorkspaceDir(dir, nil); err != nil {
		t.Errorf("目录已存在时不应报错，实际: %v", err)
	}
}

// TestEnsureWorkspaceDirRejectsFile 验证路径是文件时给出说得清的报错。
//
// 这种情况建不出目录来，只能报错；关键是错误信息要指向**目录**，
// 而不是让人去查命令。
func TestEnsureWorkspaceDirRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := ensureWorkspaceDir(path, nil)
	if err == nil {
		t.Fatal("路径是文件时应当报错")
	}
	if !strings.Contains(err.Error(), "不是目录") {
		t.Errorf("错误应当说明它不是目录，实际: %v", err)
	}
	if strings.Contains(err.Error(), "fork/exec") {
		t.Errorf("不应出现误导性的 fork/exec 报错，实际: %v", err)
	}
}

// TestEnsureWorkspaceDirEmptyIsNoop 验证空工作目录不算错。
//
// 空串表示沿用网关自己的当前目录，是合法配置（bob-main 这类实例就是这样）。
func TestEnsureWorkspaceDirEmptyIsNoop(t *testing.T) {
	if err := ensureWorkspaceDir("", nil); err != nil {
		t.Errorf("空工作目录不应报错，实际: %v", err)
	}
}

// TestStartWithMissingWorkspaceSucceeds 是端到端的回归：工作目录不存在时，
// dsh 仍然应当被拉起来——网关自己把目录补上，而不是让用户对着一条
// 指错对象的报错发呆。
func TestStartWithMissingWorkspaceSucceeds(t *testing.T) {
	port := freePort(t)
	token := "missing-workspace-token"
	startFakeDSH(t, port, token)

	options := launchOptions(t, port)
	options.Workspace = filepath.Join(t.TempDir(), "not", "created", "yet")

	supervisor := New(options)
	if err := supervisor.Start(t.Context()); err != nil {
		t.Fatalf("工作目录不存在时应当自动创建并成功拉起，实际: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = supervisor.Shutdown(ctx)
	}()
	if !supervisor.Ready() {
		t.Fatal("应当已就绪")
	}
	if _, err := os.Stat(options.Workspace); err != nil {
		t.Errorf("拉起后工作目录应当存在，实际: %v", err)
	}
}
