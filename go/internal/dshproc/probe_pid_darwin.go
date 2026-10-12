//go:build darwin

package dshproc

import (
	"os/exec"
	"strconv"
	"strings"
)

// findPID 在 macOS 上用 lsof 查监听指定端口的进程号。
//
// Linux 可以读 /proc 自己算出来，macOS 没有这套接口，而 lsof 是系统自带的
// （/usr/sbin/lsof），所以直接调用它。查不到就返回 0——PID 只用于诊断与
// 「端口被占用时问用户要不要停掉它」，拿不到不影响探测与接管。
//
// 只取 LISTEN 的条目：同一个进程可能还有已建立的连接，那些不该算作「占用端口的
// 那个进程」。
func findPID(port int) int {
	output, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}
