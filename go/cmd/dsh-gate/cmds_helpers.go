package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wycto/dsh-gate/internal/term"
)

// stripBackgroundFlags 从命令行里去掉后台开关。
//
// 后台化时子进程必须**不带** --background，否则它会再 forks 一个自己没有监听
// 的管理端，然后打印「已在后台运行」退出——父进程等端口就绪时永远等不到。
func stripBackgroundFlags(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		switch arg {
		case "--background", "-d", "--background=true", "--background=false":
			continue
		}
		out = append(out, arg)
	}
	return out
}

// joinHostPort 拼 authority，IPv6 需要方括号。
func joinHostPort(address string, port int) string {
	if strings.Contains(address, ":") {
		return fmt.Sprintf("[%s]:%d", address, port)
	}
	return fmt.Sprintf("%s:%d", address, port)
}

// acquirePassword 取得要设置的口令：交互输入（两遍）、管道、或自动生成。
func acquirePassword(randomMode, fromStdin bool) (string, error) {
	switch {
	case randomMode:
		return randomPassword(), nil
	case fromStdin:
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return "", fmt.Errorf("读取标准输入失败: %w", err)
		}
		password := strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			return "", fmt.Errorf("标准输入里没有口令")
		}
		return password, nil
	}

	if !term.IsTerminal(os.Stdin.Fd()) {
		return "", fmt.Errorf(`当前不是交互式终端，无法安全地输入口令。
改用以下任一方式:
  dsh-gate manage user add <用户名> --password-stdin < 口令文件
  dsh-gate manage user add <用户名> --random`)
	}

	password, err := readNewPassword("请输入口令（输入显示为 *）: ", "请再输入一次: ")
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	return password, nil
}

// randomPassword 生成一个便于手抄的随机口令。
func randomPassword() string {
	// 去掉 0/O/1/l/I 这类易混字符，再取 20 位：熵依然远超任何口令字典，
	// 同时不至于长到没法粘贴到手机上。
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		// 随机源不可用时直接失败，而不是退化成可预测的口令。
		panic("读取系统随机数失败，无法生成口令: " + err.Error())
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}
