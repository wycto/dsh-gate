//go:build linux

package dshproc

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// findPID 通过 /proc 查出监听该端口的进程号。
//
// 为什么只有 Linux 有这个能力：Linux 的 /proc 同时暴露了「端口 → socket
// inode」与「进程 → socket inode」两张表，交叉一下就能得到 PID，纯用户态、
// 不需要 root。其它平台要拿到同样的信息得走 lsof/netstat 之类的外部命令，
// 反而更脆。
//
// 查不到就返回 0 —— 这只是一条诊断信息，不影响任何控制流程。
func findPID(port int) int {
	inodes := listeningInodes(port)
	if len(inodes) == 0 {
		return 0
	}
	for _, inode := range inodes {
		if pid := pidForInode(inode); pid > 0 {
			return pid
		}
	}
	return 0
}

// listeningInodes 从 /proc/net/tcp{,6} 里取出监听指定端口的 socket inode。
//
// /proc/net/tcp 的每行形如：
//
//	0: 0100007F:0BB8 00000000:0000 0A ... 12345 1 ...
//	│  │           │              │         │
//	│  │           │              │         └─ inode
//	│  │           │              └─ 状态，0A 表示 LISTEN
//	│  │           └─ 本地端口（十六进制）
//	│  └─ 本地地址（十六进制，主机字节序）
//	└─ sl
func listeningInodes(port int) []string {
	want := strings.ToUpper(strconv.FormatInt(int64(port), 16))
	var found []string
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		file, err := os.Open(path)
		if err != nil {
			continue // 该内核没编进 IPv6 表等情况，跳过即可
		}
		scanner := bufio.NewScanner(file)
		scanner.Scan() // 跳过表头
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			local := fields[1]
			colon := strings.LastIndex(local, ":")
			if colon < 0 || strings.ToUpper(local[colon+1:]) != want {
				continue
			}
			if inode := fields[9]; inode != "0" {
				found = append(found, inode)
			}
		}
		_ = file.Close()
	}
	return found
}

// pidForInode 反查持有某个 socket inode 的进程号。
func pidForInode(inode string) int {
	want := fmt.Sprintf("socket:[%s]", inode)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // 权限不足或进程已退出
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err == nil && target == want {
				return pid
			}
		}
	}
	return 0
}
