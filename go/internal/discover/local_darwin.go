//go:build darwin

package discover

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Local 在 macOS 上用 lsof 发现本机监听中的 TCP 服务，并用 ps/lsof 补全命令行与 cwd。
// lsof 不可用时返回 ErrUnsupported。
func Local() ([]Candidate, error) {
	// lsof 在 macOS 装在 /usr/sbin，而服务管理器（launchd）的 PATH 常常不含 /usr/sbin；
	// 因此不能只靠 exec.LookPath，要回退到常见绝对路径再决定。
	lsofPath := resolveTool("lsof", "/usr/sbin/lsof", "/usr/bin/lsof", "/opt/homebrew/bin/lsof")
	if lsofPath == "" {
		return nil, ErrUnsupported
	}
	psPath := resolveTool("ps", "/bin/ps", "/usr/bin/ps")
	raw, err := runCmd(4*time.Second, lsofPath, "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pcn")
	if err != nil {
		return nil, ErrUnsupported
	}

	type proc struct {
		pid   int
		ports map[int]bool
	}
	byPid := map[int]*proc{}
	var order []*proc
	var cur *proc
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, perr := strconv.Atoi(line[1:])
			if perr != nil {
				cur = nil
				continue
			}
			if p, ok := byPid[pid]; ok {
				cur = p
			} else {
				cur = &proc{pid: pid, ports: map[int]bool{}}
				byPid[pid] = cur
				order = append(order, cur)
			}
		case 'n':
			if cur != nil {
				if port := portFromAddr(line[1:]); port > 0 {
					cur.ports[port] = true
				}
			}
		}
	}

	out := make([]Candidate, 0)
	for _, p := range order {
		cmd := ""
		if psPath != "" {
			cmd = strings.TrimSpace(string(softCmd(psPath, "-o", "command=", "-p", strconv.Itoa(p.pid))))
		}
		cwd := darwinCwd(p.pid, lsofPath)
		for port := range p.ports {
			http, title := probeHTTP("127.0.0.1", port)
			out = append(out, Candidate{
				Source: "local", Host: "127.0.0.1", Port: port,
				PID: p.pid, Command: cmd, Cwd: cwd, HTTP: http, Title: title,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// portFromAddr 从 lsof 的 name 字段（如 "*:12020"、"127.0.0.1:8080"、"[::1]:9000"）
// 取端口；取不到返回 0。
func portFromAddr(name string) int {
	ci := strings.LastIndexByte(name, ':')
	if ci < 0 || ci == len(name)-1 {
		return 0
	}
	p, err := strconv.Atoi(name[ci+1:])
	if err != nil || p < 1 || p > 65535 {
		return 0
	}
	return p
}

// darwinCwd 用 lsof 取某进程的当前工作目录。
func darwinCwd(pid int, lsofPath string) string {
	raw, err := runCmd(2*time.Second, lsofPath, "-a", "-d", "cwd", "-p", strconv.Itoa(pid), "-F", "n")
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}

// runCmd 带超时执行命令，返回 stdout（失败连同错误返回）。
func runCmd(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// resolveTool 先按 PATH 找命令，找不到再试给定的绝对路径（应对 launchd 精简 PATH）。
func resolveTool(name string, candidates ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return c
		}
	}
	return ""
}

// softCmd 尽力执行命令，出错返回 nil（用于补全信息，失败不影响发现）。
func softCmd(name string, args ...string) []byte {
	out, _ := runCmd(2*time.Second, name, args...)
	return out
}
