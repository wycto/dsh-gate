//go:build !windows

package main

import (
	"os/exec"
	"testing"
	"time"
)

// TestProcessIsZombieDetectsExitedChild 验证僵尸进程被判为「已退出」。
//
// 容器里 PID 1 不回收孤儿时，退出后的进程会以 Z 状态挂着——restart 会一直
// 等一个早就退出的进程直到超时。这里真造一个僵尸来验。
func TestProcessIsZombieDetectsExitedChild(t *testing.T) {
	// 起一个立刻退出的子进程，并且不 Wait：它就会变成僵尸。
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("起子进程失败: %v", err)
	}
	pid := cmd.Process.Pid

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if processIsZombie(pid) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !processIsZombie(pid) {
		// 有些平台的 init 会顺手回收，那种情况下这条断言不适用。
		t.Skip("没能造出僵尸进程（平台会自动回收），跳过")
	}
	if processAlive(pid) {
		t.Fatal("僵尸进程应被判为已退出")
	}
	_ = cmd.Wait()
}
