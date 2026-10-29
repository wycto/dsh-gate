package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wycto/dsh-gate/internal/dshproc"
	"github.com/wycto/dsh-gate/internal/term"
)

func runSelf(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		return fatal("无法确定自身路径: %v", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("\n正在执行: dsh-gate %s\n", strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return fatal("执行 dsh-gate %s 失败: %v", strings.Join(args, " "), err)
	}
	return 0
}

func askLine(prompt string) (string, error) {
	line, err := term.ReadLine(os.Stdin, os.Stdout, prompt)
	if err != nil {
		return "", fmt.Errorf("读取输入失败: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// askYesNo 读一个 y/n 回答；空输入按 def 处理（提示语里的 [y/N] / [Y/n] 与之一致）。
func askYesNo(prompt string, def bool) (bool, error) {
	for {
		answer, err := askLine(prompt)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		case "":
			return def, nil
		}
		fmt.Println("  请回答 y 或 n。")
	}
}

type portDisposition int

const (
	// portUsable 可以直接用（空闲，或用户选择接管/已经把它腾出来了）。
	portUsable portDisposition = iota
	// portChange 用户要换一个端口，向导应当重新问这一项。
	portChange
)

func stopOccupant(label, value string, pid int) bool {
	if pid <= 0 {
		fmt.Println("  查不到占用它的进程号，请手动停掉之后重试，或换一个端口。")
		return false
	}
	if err := terminateProcess(pid); err != nil {
		fmt.Printf("  停不掉进程 %d：%v。请手动处理，或换一个端口。\n", pid, err)
		return false
	}
	addr := net.JoinHostPort("127.0.0.1", value)
	for i := 0; i < 20; i++ {
		time.Sleep(250 * time.Millisecond)
		if !dshproc.Busy(addr, 200*time.Millisecond) {
			fmt.Printf("  已停掉进程 %d，%s %s 空出来了。\n", pid, label, value)
			return true
		}
	}
	fmt.Printf("  已向进程 %d 发出停止信号，但 %s %s 还没释放（它可能忽略信号或正在收尾）。\n", pid, label, value)
	fmt.Println("  可以稍后重试、手动处理，或换一个端口。")
	return false
}

// askChoice 问一个单字符选项，回车取默认值。
func askChoice(prompt, fallback string, allowed ...string) (string, error) {
	for {
		answer, err := askLine(prompt)
		if err != nil {
			return "", err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "" {
			return fallback, nil
		}
		for _, item := range allowed {
			if answer == item {
				return answer, nil
			}
		}
		fmt.Printf("  请输入 %s 之一。\n", strings.Join(allowed, " / "))
	}
}
