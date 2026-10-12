package main

import (
	"os"
	"strings"
	"testing"
)

// TestServiceUnitWorkingDirectoryFollowsWorkspace 单元的 WorkingDirectory 必须跟着
// --workspace，而不是停在 --working-dir 的默认值（家目录）。
//
// 这是一个「看着对、其实永远短路」的回归：--working-dir 的默认值是家目录，
// 于是 workDir 永远非空，优先级里的第二档永远轮不到。实测在 systemd 容器里
// 表现为 WorkingDirectory=/root 而 ExecStart 里带着 --workspace /srv——网关进程
// 与 dsh 的工作目录分家。
func TestServiceUnitWorkingDirectoryFollowsWorkspace(t *testing.T) {
	home, _ := os.UserHomeDir()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "只给 --workspace：单元跟随它",
			args: []string{"--port", "13199", "--workspace", "/srv/ws", "--bin-dir", ""},
			want: "/srv/ws",
		},
		{
			name: "显式 --working-dir 优先于 --workspace",
			args: []string{"--port", "13199", "--workspace", "/srv/ws", "--working-dir", "/explicit", "--bin-dir", ""},
			want: "/explicit",
		},
		{
			name: "都不给：回落到家目录",
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
			got := workingDirectoryIn(text)
			if got != tc.want {
				t.Errorf("WorkingDirectory = %q，期望 %q\n单元内容:\n%s", got, tc.want, text)
			}
		})
	}
}

// serviceUnitTextForTest 用真实的 serviceCommand 路径生成单元文本。
func serviceUnitTextForTest(t *testing.T, args []string) (string, error) {
	t.Helper()
	// 走 run() 会直接打印到 stdout，这里改为直接构造选项，保证拿到文本。
	opts := serviceOptions{Binary: "/usr/local/bin/dsh-gate"}
	values := map[string]string{}
	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			break
		}
		key := strings.TrimPrefix(args[i], "--")
		if key == "bin-dir" {
			continue
		}
		values[key] = args[i+1]
	}
	if workspace, ok := values["workspace"]; ok {
		delete(values, "workspace")
		opts.Overrides = map[string]string{"workspace": workspace}
	}
	if workDir, ok := values["working-dir"]; ok {
		opts.WorkingDir = workDir
	}
	return serviceUnit(opts)
}

// workingDirectoryIn 从单元文本里取 WorkingDirectory 的值。
func workingDirectoryIn(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		switch {
		case strings.Contains(line, "<key>WorkingDirectory</key>"):
			if i+1 < len(lines) {
				// 注意别用 strings.Trim：它的第二个参数是**字符集**，
				// "<string></string>" 会把首尾的 s、/ 等字符一并啃掉。
				value := strings.TrimSpace(lines[i+1])
				value = strings.TrimPrefix(value, "<string>")
				value = strings.TrimSuffix(value, "</string>")
				return value
			}
		case strings.HasPrefix(line, "WorkingDirectory="):
			return strings.TrimPrefix(line, "WorkingDirectory=")
		}
	}
	return ""
}

// TestWorkingDirectoryInParsesBothPlatforms 解析辅助函数本身要能读两种平台格式，
// 否则测试会在某个平台上「假通过」。
func TestWorkingDirectoryInParsesBothPlatforms(t *testing.T) {
	plist := "<key>WorkingDirectory</key>\n    <string>/srv</string>\n"
	if got := workingDirectoryIn(plist); got != "/srv" {
		t.Errorf("plist 解析 = %q", got)
	}
	unit := "[Service]\nWorkingDirectory=/srv\n"
	if got := workingDirectoryIn(unit); got != "/srv" {
		t.Errorf("unit 解析 = %q", got)
	}
}
