package main

import (
	"os"
	"strings"
	"testing"
)

// TestServiceUnitWorkingDirectory 单元的 WorkingDirectory 必须服从 --working-dir，
// 没给时回落到用户家目录。管理端单形态下没有 --workspace 这个概念了（实例各自
// 有自己的工作目录），所以单元只认显式的 --working-dir。
func TestServiceUnitWorkingDirectory(t *testing.T) {
	home, _ := os.UserHomeDir()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "显式 --working-dir 优先",
			args: []string{"--port", "13199", "--working-dir", "/explicit", "--bin-dir", ""},
			want: "/explicit",
		},
		{
			name: "不给：回落到家目录",
			args: []string{"--port", "13199", "--bin-dir", ""},
			want: home,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, err := serviceUnitTextForTest(t, tc.args)
			if err != nil {
				t.Fatalf("生成单元失败: %v", err)
			}
			if got := workingDirectoryIn(text); got != tc.want {
				t.Errorf("WorkingDirectory = %q，期望 %q\n单元内容:\n%s", got, tc.want, text)
			}
			// 托管的必须是管理端启动命令，而不是旧的单实例 run。
			if !strings.Contains(text, "start") || !strings.Contains(text, "--foreground") {
				t.Errorf("单元应托管 start --foreground，实际:\n%s", text)
			}
			if !strings.Contains(text, "--port") || !strings.Contains(text, "13199") {
				t.Errorf("单元应带上 --port 13199，实际:\n%s", text)
			}
		})
	}
}

// serviceUnitTextForTest 用真实的 serviceOptions 生成单元文本。
func serviceUnitTextForTest(t *testing.T, args []string) (string, error) {
	t.Helper()
	opts := serviceOptions{Binary: "/usr/local/bin/dsh-gate", Overrides: map[string]string{}}
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			break
		}
		key := strings.TrimPrefix(args[i], "--")
		if key == "bin-dir" {
			continue
		}
		if key == "working-dir" {
			opts.WorkingDir = args[i+1]
			continue
		}
		opts.Overrides[key] = args[i+1]
	}
	return serviceUnit(opts)
}

// workingDirectoryIn 从单元文本里取 WorkingDirectory 的值（plist 与 systemd
// 两种格式都覆盖）。
func workingDirectoryIn(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "WorkingDirectory="); ok {
			return strings.TrimSpace(value)
		}
	}
	const key = "<key>WorkingDirectory</key>"
	if i := strings.Index(text, key); i >= 0 {
		rest := text[i+len(key):]
		open := strings.Index(rest, "<string>")
		closeIdx := strings.Index(rest, "</string>")
		if open >= 0 && closeIdx > open {
			return strings.TrimSpace(rest[open+len("<string>") : closeIdx])
		}
	}
	return ""
}
