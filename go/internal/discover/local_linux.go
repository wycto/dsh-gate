//go:build linux

package discover

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Local 在 Linux 上通过 /proc 发现本机监听中的 TCP 服务：
// /proc/net/tcp{,6} 的 LISTEN(st=0A) 建立 inode→端口；遍历 /proc/<pid>/fd 的
// socket:[inode] 反查 pid；读 /proc/<pid>/cmdline 与 cwd。对每个端口尽力做一次 HTTP 探测。
// /proc 不可用时返回 ErrUnsupported。
func Local() ([]Candidate, error) {
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		return nil, ErrUnsupported
	}
	inodePort := map[string]int{}
	_ = readProcNetTCP("/proc/net/tcp", inodePort)
	_ = readProcNetTCP("/proc/net/tcp6", inodePort)

	// 每个监听端口 → 一个 pid（多个进程共享端口时取第一个）。
	portPID := map[int]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, ErrUnsupported
	}
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		fdDir := "/proc/" + e.Name() + "/fd"
		fds, ferr := os.ReadDir(fdDir)
		if ferr != nil {
			continue // 权限不足或进程已退出
		}
		for _, fd := range fds {
			target, lerr := os.Readlink(fdDir + "/" + fd.Name())
			if lerr != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			if port, ok := inodePort[inode]; ok {
				if _, done := portPID[port]; !done {
					portPID[port] = pid
				}
			}
		}
	}

	// 所有监听端口（含查不到 pid 的，pid 记 0）。
	allPorts := map[int]bool{}
	for _, port := range inodePort {
		allPorts[port] = true
	}
	out := make([]Candidate, 0, len(allPorts))
	for port := range allPorts {
		c := Candidate{Source: "local", Host: "127.0.0.1", Port: port}
		if pid := portPID[port]; pid > 0 {
			c.PID = pid
			c.Command = readCmdline(pid)
			if cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
				c.Cwd = cwd
			}
		}
		c.HTTP, c.Title = probeHTTP("127.0.0.1", port)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// readProcNetTCP 解析 /proc/net/tcp{,6}：LISTEN 行建立 socket inode → 端口。
func readProcNetTCP(path string, inodePort map[string]int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first { // 首行是列名
			first = false
			continue
		}
		flds := strings.Fields(line)
		if len(flds) < 10 {
			continue
		}
		if flds[3] != "0A" { // 只看 LISTEN
			continue
		}
		local := flds[1]
		ci := strings.LastIndexByte(local, ':')
		if ci < 0 {
			continue
		}
		port64, perr := strconv.ParseInt(local[ci+1:], 16, 32)
		if perr != nil {
			continue
		}
		inodePort[flds[9]] = int(port64)
	}
	return sc.Err()
}

// readCmdline 读 /proc/<pid>/cmdline（NUL 分隔）转成空格分隔的一行。
func readCmdline(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	s := strings.ReplaceAll(string(raw), "string(rune(0))", " ")
	return strings.TrimSpace(s)
}
