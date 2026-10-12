package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/users"
)

// swapStdin 把标准输入换成一段文本，返回还原函数。
//
// setup 在非交互路径上通过 --password-stdin 从标准输入读口令，测试里用
// 常规文件冒充管道即可；term.IsTerminal 对普通文件一定返回 false，
// 因此整条向导都会走「不提问」的分支。
func swapStdin(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("准备标准输入失败: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开标准输入失败: %v", err)
	}
	original := os.Stdin
	os.Stdin = file
	t.Cleanup(func() {
		os.Stdin = original
		file.Close()
	})
}

// TestSetupCreatesFirstAccount 走通非交互主路径：建账号、不动其它东西。
func TestSetupCreatesFirstAccount(t *testing.T) {
	stateDir := t.TempDir()
	swapStdin(t, "初始口令-abcd1234\n")

	// 显式给空闲端口：默认 3080/3082 在开发机上可能真被占着，而 0.3.5 起
	// setup 会在动手之前检查端口占用——测试不该依赖开发机的端口现状。
	code := run([]string{"setup", "--admin", "boss", "--password-stdin", "--state-dir", stateDir,
		"--port", itoa(freePortForTest(t)), "--dsh-port", itoa(freePortForTest(t))})
	if code != 0 {
		t.Fatalf("setup 退出码 = %d，期望 0", code)
	}

	store, err := users.Open(filepath.Join(stateDir, "users.json"))
	if err != nil {
		t.Fatalf("打开账号表失败: %v", err)
	}
	if store.Count() != 1 {
		t.Fatalf("账号数 = %d，期望 1", store.Count())
	}
	if !store.Verify("boss", "初始口令-abcd1234") {
		t.Error("刚创建的账号无法用它自己的口令通过校验")
	}
}

// TestSetupKeepsExistingAccounts 锁定「初始化不碰已有账号表」。
//
// setup 可能被重复执行（换端口重新初始化之类），如果它在账号表非空时也去
// 动口令，一次手滑就会把正在使用的账号悄悄换掉。
func TestSetupKeepsExistingAccounts(t *testing.T) {
	stateDir := t.TempDir()
	usersPath := filepath.Join(stateDir, "users.json")

	store, err := users.Open(usersPath)
	if err != nil {
		t.Fatalf("准备账号表失败: %v", err)
	}
	if err := store.Set("keeper", "旧口令-abcd1234", users.DefaultIterations); err != nil {
		t.Fatalf("准备账号失败: %v", err)
	}

	swapStdin(t, "随便什么都不该被读走\n")
	code := run([]string{"setup", "--admin", "intruder", "--random", "--state-dir", stateDir,
		"--port", itoa(freePortForTest(t)), "--dsh-port", itoa(freePortForTest(t))})
	if code != 0 {
		t.Fatalf("setup 退出码 = %d，期望 0", code)
	}

	after, err := users.Open(usersPath)
	if err != nil {
		t.Fatalf("重新打开账号表失败: %v", err)
	}
	if after.Count() != 1 {
		t.Fatalf("账号数 = %d，期望保持 1", after.Count())
	}
	if after.Exists("intruder") {
		t.Error("setup 在账号表非空时创建了新账号")
	}
	if !after.Verify("keeper", "旧口令-abcd1234") {
		t.Error("已有账号的口令被 setup 改动了")
	}
}

// TestSetupRejectsForeignFlags 锁定 setup 的选项范围。
//
// --log-level 之类 run 模式选项要么 service install 不认识、要么和实例选项
// 冲突，必须当场拒绝，而不是等转发给子命令时爆出莫名其妙的错误。
func TestSetupRejectsForeignFlags(t *testing.T) {
	swapStdin(t, "")
	for _, argv := range [][]string{
		{"setup", "--log-level", "debug"},
		{"setup", "--config", "/tmp/x.json"},
		{"setup", "--background"},
	} {
		if code := run(argv); code != 2 {
			t.Errorf("run(%v) 退出码 = %d，期望 2（用法错误）", argv, code)
		}
	}
}

// TestSetupRequiresPasswordSource 非交互且没给口令来源时必须提前失败。
func TestSetupRequiresPasswordSource(t *testing.T) {
	swapStdin(t, "")
	code := run([]string{"setup", "--state-dir", t.TempDir(),
		"--port", itoa(freePortForTest(t)), "--dsh-port", itoa(freePortForTest(t))})
	if code != 2 {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
}

// TestFlagArgsStableAndSorted 同一份覆盖项必须生成完全一致的参数：
// service install 每次安装都会重新生成启动命令，顺序漂移会让人以为配置变了。
func TestFlagArgsStableAndSorted(t *testing.T) {
	overrides := map[string]string{"port": "12061", "name": "company", "dsh-home": "~/.dsh-company"}
	want := "--dsh-home ~/.dsh-company --name company --port 12061"
	for i := 0; i < 5; i++ {
		got := strings.Join(flagArgs(overrides), " ")
		if got != want {
			t.Fatalf("第 %d 次 flagArgs = %q，期望 %q", i+1, got, want)
		}
	}
}

// freePortForTest 取一个当前空闲的端口。
func freePortForTest(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// itoa 是 strconv.Itoa 的短名，让测试里的选项拼装好读一点。
func itoa(value int) string { return strconv.Itoa(value) }
