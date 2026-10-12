package main

import (
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/dshtest"
)

// freePort 拿一个当前空闲的端口号。
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

// TestCheckPortUsableRejectsBadValues 非数字、越界、两个端口撞车都要当场拒绝。
func TestCheckPortUsableRejectsBadValues(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		value   string
		chosen  map[string]string
		wantSub string
	}{
		{"不是数字", "port", "abc", nil, "数字"},
		{"超出范围", "port", "70000", nil, "1-65535"},
		{"零", "dsh-port", "0", nil, "1-65535"},
		{"与另一个端口相同", "port", "12080", map[string]string{"dsh-port": "12080"}, "不能相同"},
		{"反向撞车", "dsh-port", "12081", map[string]string{"port": "12081"}, "不能相同"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPortUsable(tc.key, tc.value, tc.chosen)
			if err == nil {
				t.Fatalf("应当拒绝，却通过了")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误信息 %q 应包含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestCheckPortUsableIgnoresOccupancy 值校验只看数字、范围、撞车；占用不归它管
// （占用要问用户怎么办，见 resolvePortConflict）。
func TestCheckPortUsableIgnoresOccupancy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := checkPortUsable("port", port, nil); err != nil {
		t.Fatalf("值本身合法就不该在这里被拒: %v", err)
	}
}

// TestResolvePortConflictNonInteractive 脚本模式问不了用户：占用就报错（并带上
// 能做的事），但「占用者是 dsh」仍然允许——那是 --adopt 的既有语义。
func TestResolvePortConflictNonInteractive(t *testing.T) {
	// 空闲端口：放行。
	if disposition, err := resolvePortConflict("port", strconv.Itoa(freePort(t)), false); err != nil || disposition != portUsable {
		t.Fatalf("空闲端口应放行，得到 %v / %v", disposition, err)
	}

	// 被普通监听者占用：报错。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	busy := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	disposition, err := resolvePortConflict("port", busy, false)
	if err == nil {
		t.Fatal("被占用时应报错")
	}
	if disposition != portChange {
		t.Errorf("应当让调用方换端口，实际 %v", disposition)
	}
	if !strings.Contains(err.Error(), "已被占用") {
		t.Errorf("错误信息应说明被占用: %v", err)
	}

	// 被 dsh 占用：允许接管（写明了「会直接接管」）。
	fake := dshtest.Start()
	defer fake.Close()
	if disposition, err := resolvePortConflict("dsh-port", strconv.Itoa(fake.Port()), false); err != nil || disposition != portUsable {
		t.Fatalf("接管 dsh 端口应放行，得到 %v / %v", disposition, err)
	}
}

// TestResolvePortConflictAdoptKeepsDSHRunning 交互模式下选「接管」时**不能**把
// dsh 停掉：用户要的就是直接用它。
func TestResolvePortConflictAdoptKeepsDSHRunning(t *testing.T) {
	fake := dshtest.Start()
	defer fake.Close()
	swapStdin(t, "t\n")
	disposition, err := resolvePortConflict("dsh-port", strconv.Itoa(fake.Port()), true)
	if err != nil || disposition != portUsable {
		t.Fatalf("接管应当放行，得到 %v / %v", disposition, err)
	}
	if !dshproc.Busy(fake.Addr(), 500*time.Millisecond) {
		t.Fatal("选「接管」却把 dsh 停掉了")
	}
}

// TestResolvePortConflictChangeKeepsOccupant 选「换端口」时不能动占用者。
func TestResolvePortConflictChangeKeepsOccupant(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	swapStdin(t, "c\n")
	disposition, err := resolvePortConflict("port", port, true)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if disposition != portChange {
		t.Fatalf("应当让调用方换端口，实际 %v", disposition)
	}
	if !dshproc.Busy("127.0.0.1:"+port, 500*time.Millisecond) {
		t.Fatal("选「换端口」不该动占用者")
	}
}

// TestResolvePortConflictStopsOccupant 选「停掉它」要真的把占用进程停掉，并等端口
// 释放之后再返回——发完信号就返回的话，紧接着启动网关会再撞一次。
//
// 占用者用子进程扮演：本进程自己监听的话，SIGTERM 会把测试进程一起带走。
func TestResolvePortConflictStopsOccupant(t *testing.T) {
	if port := os.Getenv("DSH_GATE_TEST_OCCUPANT_PORT"); port != "" {
		listener, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			os.Exit(3)
		}
		defer listener.Close()
		// 占着不放，等父进程发信号。
		time.Sleep(60 * time.Second)
		return
	}

	port := strconv.Itoa(freePort(t))
	occupant := exec.Command(os.Args[0], "-test.run=TestResolvePortConflictStopsOccupant")
	occupant.Env = append(os.Environ(), "DSH_GATE_TEST_OCCUPANT_PORT="+port)
	occupant.Stdout, occupant.Stderr = io.Discard, io.Discard
	if err := occupant.Start(); err != nil {
		t.Fatalf("起占用进程失败: %v", err)
	}
	defer func() { _ = occupant.Process.Kill() }()

	// 等它真的占上端口。
	addr := "127.0.0.1:" + port
	deadline := time.Now().Add(5 * time.Second)
	for !dshproc.Busy(addr, 200*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("占用进程没能在 5 秒内监听端口")
		}
		time.Sleep(100 * time.Millisecond)
	}

	swapStdin(t, "s\n")
	disposition, err := resolvePortConflict("port", port, true)
	if err != nil {
		t.Fatalf("停掉占用者后应当放行，得到错误: %v", err)
	}
	if disposition != portUsable {
		t.Fatalf("停掉之后端口应当可用，实际 %v", disposition)
	}
	if dshproc.Busy(addr, 300*time.Millisecond) {
		t.Error("端口还被占着，说明没真的停下来")
	}

	done := make(chan error, 1)
	go func() { done <- occupant.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("占用进程没有被停掉")
	}
}

// TestCheckPortUsableAcceptsFreePort 空闲端口要放行。
func TestCheckPortUsableAcceptsFreePort(t *testing.T) {
	port := strconv.Itoa(freePort(t))
	if err := checkPortUsable("port", port, map[string]string{"dsh-port": "1"}); err != nil {
		t.Fatalf("空闲端口被拒了: %v", err)
	}
}

// TestCheckPortsForScriptRejectsOccupied 非交互模式（脚本）也必须预检：
// 没有机会当场重问，就更要在动手之前失败。
func TestCheckPortsForScriptRejectsOccupied(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer listener.Close()
	busy := listener.Addr().(*net.TCPAddr).Port

	cfg := config.Default()
	cfg.DSHPort = busy
	cfg.Port = freePort(t)
	err = checkPortsForScript(cfg)
	if err == nil {
		t.Fatal("dsh 端口被占用时应当拒绝")
	}
	if !strings.Contains(err.Error(), "dsh 端口") {
		t.Errorf("应指明是哪个端口，实际: %v", err)
	}

	// 两个端口相同也要拦（脚本里最容易复制粘贴出错）。
	same := freePort(t)
	cfg = config.Default()
	cfg.Port, cfg.DSHPort = same, same
	if err := checkPortsForScript(cfg); err == nil {
		t.Error("两个端口相同时应当拒绝")
	}

	// 都空闲时放行。
	cfg = config.Default()
	cfg.Port, cfg.DSHPort = freePort(t), freePort(t)
	if err := checkPortsForScript(cfg); err != nil {
		t.Errorf("空闲端口被拒: %v", err)
	}
}

// TestCheckPortsForScriptAllowsAdoptingExistingDSH dsh 端口上已经有一个 dsh、
// 而配置允许接管（默认）时，这不是冲突而是文档写明的正常用法：网关接管它、
// 不重复启动。以前这里会一刀切地拦住，把用户赶去改用 run。
func TestCheckPortsForScriptAllowsAdoptingExistingDSH(t *testing.T) {
	dsh := dshtest.Start()
	defer dsh.Close()
	port := dsh.Port()

	cfg := config.Default()
	cfg.DSHPort = port
	cfg.Port = freePort(t)
	cfg.Adopt = true
	if err := checkPortsForScript(cfg); err != nil {
		t.Fatalf("允许接管时不该拦：%v", err)
	}

	// 关掉接管就必须拦：那时启动会直接失败。
	cfg.Adopt = false
	if err := checkPortsForScript(cfg); err == nil {
		t.Error("--adopt=false 时应当拦下被 dsh 占用的端口")
	}
}
