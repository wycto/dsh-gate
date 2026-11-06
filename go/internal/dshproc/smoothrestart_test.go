package dshproc

// 本文件回归两个真实故障（用户已确认现象）：
//
//  1. 网关重启/重装后，被接管的 dsh 过一会儿就「实例不可用」。根因之一是 spawn
//     用管道接 dsh 输出，网关被杀后管道读端关闭，dsh（Node）下一次写日志收到
//     EPIPE 即死。修复 A：OutputFile 非空时输出落盘，子进程生死不再系于网关。
//  2. adopt 接管后设 ready=true 就返回，没有任何 goroutine 监视接管进程的存活；
//     接管进程死后 ready 恒 true、监督者永不重拉。修复 B：watchAdopted 定期探活，
//     死了自动重拉。
//
// 沿用 launch_test.go 的「测试二进制自己扮演 dsh」基建（startFakeDSH /
// launchOptions / freePort）。持续输出压测另用一个 helper-process 测试。

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// smoothOutputHelperEnv 标记「这个测试进程此刻扮演一个持续大量输出的假 dsh」。
const smoothOutputHelperEnv = "DSH_GATE_SMOOTH_OUTPUT_HELPER"

// TestSmoothOutputHelper 不是普通测试：它只在被监督者作为子进程重新执行、且带
// smoothOutputHelperEnv=1 时才扮演 dsh——打印带令牌的启动 URL，监听端口，并持续
// 往标准输出写大量日志。父进程里跑到它时（标记未设）直接返回，是个空操作。
//
// 为什么非要持续输出：EPIPE 的触发条件是「读端没了之后子进程还在写」。只有让子
// 进程不停写，才能证明落盘模式下它不会因为网关不读管道而死。
func TestSmoothOutputHelper(t *testing.T) {
	if os.Getenv(smoothOutputHelperEnv) != "1" {
		return
	}
	port := os.Getenv(fakePort)
	token := os.Getenv(fakeToken)
	fmt.Printf("%s127.0.0.1:%s/?token=%s\n", fakePrefix, port, token)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "smooth helper listen: %v\n", err)
		os.Exit(1)
	}
	go func() {
		for i := 0; ; i++ {
			fmt.Printf("smooth-output flood line %d: %s\n", i, strings.Repeat("x", 200))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		_ = conn.Close()
	}
}

// TestOutputFileExtractsTokenAndPersists 验证落盘模式：令牌仍被提到、输出进了文件。
func TestOutputFileExtractsTokenAndPersists(t *testing.T) {
	port := freePort(t)
	token := "output-file-token-1234"
	startFakeDSH(t, port, token)

	outFile := filepath.Join(t.TempDir(), "dsh-output.log")
	opts := launchOptions(t, port)
	opts.OutputFile = outFile

	sup := New(opts)
	if err := sup.Start(t.Context()); err != nil {
		t.Fatalf("落盘模式拉起 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = sup.Shutdown(context.Background()) })

	if !sup.Ready() {
		t.Fatal("应当已就绪")
	}
	// 令牌是从落盘文件里追读出来的（没有管道）。
	deadline := time.Now().Add(5 * time.Second)
	for sup.Token() != token && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := sup.Token(); got != token {
		t.Errorf("应从输出文件提到令牌 %q，实际 %q", token, got)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("输出应当落盘: %v", err)
	}
	if !strings.Contains(string(data), "token="+token) {
		t.Errorf("输出文件应含启动 URL，实际 %q", string(data))
	}
	// 文件权限 0600（输出里可能有敏感信息）。
	if info, err := os.Stat(outFile); err == nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("输出文件权限应为 0600，实际 %o", perm)
		}
	}
}

// TestOutputFileChildSurvivesFlood 是 EPIPE 的直接回归：子进程持续大量写输出，
// 落盘模式下它必须一直活着。管道模式下读端一旦不读，写满 64KB 管道缓冲后子进程
// 就会阻塞乃至 EPIPE 退出；落盘模式没有这个读端依赖。
func TestOutputFileChildSurvivesFlood(t *testing.T) {
	port := freePort(t)
	token := "flood-token-5678"
	// 不用 startFakeDSH（那会设 fakeEnv=1 走 runFakeDSH）；改用 helper-process，
	// 让子进程持续输出。fakePort/fakeToken 仍由 helper 读取。
	t.Setenv(fakePort, strconv.Itoa(port))
	t.Setenv(fakeToken, token)
	t.Setenv(smoothOutputHelperEnv, "1")

	outFile := filepath.Join(t.TempDir(), "flood.log")
	opts := launchOptions(t, port)
	opts.Command = fakeDSHCommand(" -test.run=TestSmoothOutputHelper")
	opts.OutputFile = outFile

	sup := New(opts)
	if err := sup.Start(t.Context()); err != nil {
		t.Fatalf("拉起持续输出的假 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = sup.Shutdown(context.Background()) })

	// 让它写一会儿：2ms 一行、每行约 230 字节，1.5 秒远超 64KB 管道缓冲。
	time.Sleep(1500 * time.Millisecond)

	if !sup.Running() {
		t.Fatal("落盘模式下子进程持续写输出也不该死（管道模式会 EPIPE/阻塞）")
	}
	if !sup.Ready() {
		t.Error("子进程活着且端口在听，应当就绪")
	}
	info, err := os.Stat(outFile)
	if err != nil {
		t.Fatalf("输出文件应存在: %v", err)
	}
	if info.Size() < 64*1024 {
		t.Errorf("输出应已写入远超管道缓冲的量（>64KB），实际 %d 字节", info.Size())
	}
	if got := sup.Token(); got != token {
		t.Errorf("应从落盘输出提到令牌 %q，实际 %q", token, got)
	}
}

// TestOutputFileRotatesWhenLarge 验证超过 8MB 时下一次打开会轮转成 .1 再建新。
func TestOutputFileRotatesWhenLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	// 造一个刚好超过阈值的文件。
	big := make([]byte, outputRotateBytes+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	f, offset, err := openOutputFile(path)
	if err != nil {
		t.Fatalf("打开应成功: %v", err)
	}
	_ = f.Close()
	if offset != 0 {
		t.Errorf("轮转后新文件偏移应为 0，实际 %d", offset)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("旧文件应被轮转成 .1: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Errorf("轮转后应建出空的新文件: %v / %v", err, info)
	}
}

// TestAdoptedInstanceAutoRecovers 是「接管实例死后自动恢复」的回归：起一个假 dsh，
// 监督者接管它，杀掉它，断言监督者在若干秒内自动拉起新进程并就绪——而不是 ready
// 恒 true、永远等用户手动点重启。
func TestAdoptedInstanceAutoRecovers(t *testing.T) {
	port := freePort(t)
	token := "auto-recover-token"
	startFakeDSH(t, port, token)

	// 先把假 dsh 起在端口上，模拟「别处已经在跑的 dsh」。Restart=false：它被杀时
	// 这个 owner 不该重拉，否则端口又被占上，adopter 的自动恢复就没法验。
	ownerOpts := launchOptions(t, port)
	ownerOpts.Restart = false
	owner := New(ownerOpts)
	if err := owner.Start(t.Context()); err != nil {
		t.Fatalf("起假 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = owner.Shutdown(context.Background()) })

	adopter := New(launchOptions(t, port))
	adopter.opts.Token = ""
	if err := adopter.Start(t.Context()); err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	t.Cleanup(func() { _ = adopter.Shutdown(context.Background()) })
	if !adopter.Adopted() || !adopter.Ready() {
		t.Fatal("端口已被占用，应当走接管且立即就绪")
	}

	// 杀掉被接管的那个 dsh，端口让开。
	if err := owner.Shutdown(context.Background()); err != nil {
		t.Fatalf("停掉被接管的 dsh 失败: %v", err)
	}

	// watchAdopted 每 5 秒探一次；给足余量等它自动重拉。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if adopter.Ready() && adopter.Managed() && !adopter.Adopted() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !adopter.Ready() {
		t.Fatal("接管的 dsh 死后监督者应自动重拉并就绪，而不是永远等手动重启")
	}
	if !adopter.Managed() || adopter.Adopted() {
		t.Error("自动重拉后应转为自管（Managed=true、Adopted=false）")
	}
	if adopter.PID() <= 0 {
		t.Error("自动重拉后应掌握新的子进程号")
	}
}

// smoothRetryMarkerEnv 指向一个「存在即让假 dsh 启动失败」的标记文件，用来制造
// 「第一次自动重拉失败、之后成功」的场景，验证监视者的重试循环不会失败一次就放弃。
const smoothRetryMarkerEnv = "DSH_GATE_SMOOTH_RETRY_MARKER"

// TestSmoothRetryHelper 扮演一个「看标记文件决定成败」的假 dsh：标记存在就立刻
// 退出（让监督者的 launch 因端口迟迟不监听而失败）；标记被删掉后则正常监听。
// 父进程里跑到它时（标记环境变量未设）直接返回。
func TestSmoothRetryHelper(t *testing.T) {
	marker := os.Getenv(smoothRetryMarkerEnv)
	if marker == "" {
		return
	}
	if _, err := os.Stat(marker); err == nil {
		// 标记还在：启动即失败，模拟「dsh 暂时起不来」。
		os.Exit(1)
	}
	port := os.Getenv(fakePort)
	token := os.Getenv(fakeToken)
	fmt.Printf("%s127.0.0.1:%s/?token=%s\n", fakePrefix, port, token)
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(1)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		_ = conn.Close()
	}
}

// TestAdoptedRecoveryRetriesAfterFailure 锁定重试循环的回归：接管进程死后第一次
// 自动重拉失败（标记文件让假 dsh 启动即退出），监督者必须记下错误并在下个周期继续
// 重试，而不是失败一次就放弃。删掉标记后，后续重试应成功并就绪。
//
// 这条专门钉住一个曾出现的 bug：转 launch 时监视信号 stop 已被关闭，若重试循环仍
// select stop 会立即命中、失败一次就退出。现在重试只以 stopped 为退出条件。
func TestAdoptedRecoveryRetriesAfterFailure(t *testing.T) {
	port := freePort(t)
	token := "retry-recover-token"
	startFakeDSH(t, port, token)

	ownerOpts := launchOptions(t, port)
	ownerOpts.Restart = false
	owner := New(ownerOpts)
	if err := owner.Start(t.Context()); err != nil {
		t.Fatalf("起假 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = owner.Shutdown(context.Background()) })

	// 标记文件存在 → 重拉用的假 dsh 启动即失败。
	marker := filepath.Join(t.TempDir(), "fail-marker")
	if err := os.WriteFile(marker, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(smoothRetryMarkerEnv, marker)

	// owner 的子进程已带着 fakeEnv=1 起好（走 runFakeDSH）；这里把它清掉，让
	// adopter 之后重拉的孩子按 -test.run 走到 TestSmoothRetryHelper，而不是又被
	// TestMain 截去 runFakeDSH（那样会直接启动成功，制造不出「第一次失败」）。
	t.Setenv(fakeEnv, "0")

	adopterOpts := launchOptions(t, port)
	adopterOpts.Command = fakeDSHCommand(" -test.run=TestSmoothRetryHelper")
	adopterOpts.StartTimeout = 2 * time.Second // 让失败的 launch 快速返回
	adopter := New(adopterOpts)
	adopter.opts.Token = ""
	if err := adopter.Start(t.Context()); err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	t.Cleanup(func() { _ = adopter.Shutdown(context.Background()) })
	if !adopter.Adopted() {
		t.Fatal("应当走接管")
	}

	// 杀掉被接管的 dsh，触发监视者自动重拉（第一次会因标记失败）。
	if err := owner.Shutdown(context.Background()); err != nil {
		t.Fatalf("停掉被接管的 dsh 失败: %v", err)
	}

	// 等到第一次重拉失败被记下（LastError 非空），再删标记放行后续重试。
	deadline := time.Now().Add(20 * time.Second)
	for adopter.LastError() == "" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if adopter.LastError() == "" {
		t.Fatal("第一次自动重拉应当失败并记下 LastError")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("删标记失败: %v", err)
	}

	// 后续重试应成功：监视者没有因第一次失败而放弃。
	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if adopter.Ready() && adopter.Managed() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !adopter.Ready() || !adopter.Managed() {
		t.Fatal("第一次重拉失败后监督者应继续重试并最终就绪，而不是失败一次就放弃")
	}
}

// TestShutdownAdoptedClosesWatch 验证接管态 Shutdown 不泄漏监视 goroutine：
// watchStop 被关闭并置 nil，且重复 Shutdown 不会重复 close 引发 panic。
func TestShutdownAdoptedClosesWatch(t *testing.T) {
	port := freePort(t)
	startFakeDSH(t, port, "shutdown-watch-token")

	ownerOpts := launchOptions(t, port)
	ownerOpts.Restart = false
	owner := New(ownerOpts)
	if err := owner.Start(t.Context()); err != nil {
		t.Fatalf("起假 dsh 失败: %v", err)
	}
	t.Cleanup(func() { _ = owner.Shutdown(context.Background()) })

	adopter := New(launchOptions(t, port))
	if err := adopter.Start(t.Context()); err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	if !adopter.Adopted() {
		t.Fatal("应当走接管")
	}
	adopter.mu.Lock()
	watch := adopter.watchStop
	adopter.mu.Unlock()
	if watch == nil {
		t.Fatal("接管后应已创建监视停止信号")
	}

	if err := adopter.Shutdown(context.Background()); err != nil {
		t.Fatalf("接管态 Shutdown 应成功: %v", err)
	}
	// 信号应被关闭（监视 goroutine 据此退出）。
	select {
	case <-watch:
	default:
		t.Error("Shutdown 后 watchStop 应被关闭，否则监视 goroutine 泄漏")
	}
	adopter.mu.Lock()
	nilAfter := adopter.watchStop == nil
	stopped := adopter.stopped
	adopter.mu.Unlock()
	if !nilAfter {
		t.Error("Shutdown 后 watchStop 应置 nil（保证恰好关一次）")
	}
	if !stopped {
		t.Error("Shutdown 后应标记 stopped")
	}
	// 再来一次不该 panic（重复 close 防护）。
	if err := adopter.Shutdown(context.Background()); err != nil {
		t.Errorf("重复 Shutdown 应安全: %v", err)
	}
}
