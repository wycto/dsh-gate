package main

import "testing"

// 取消自启前要把「它到底托管着什么」打给用户看，三个平台各有一份格式。
// 这些解析器都不依赖平台，所以能在任何机器上钉住——Windows 与 systemd 的
// 真实单元在开发机上根本不存在。

func TestExecStartFromUnit(t *testing.T) {
	unit := `# 由 dsh-gate 0.2.5 的 service unit 生成。
[Unit]
Description=DSH 局域网网关

[Service]
Type=simple
User=root
WorkingDirectory=/root
ExecStart=/usr/local/bin/dsh-gate start --foreground --host 0.0.0.0 --port 12081 --state-dir /srv/state
Restart=always
`
	got := execStartFromUnit(unit)
	want := "/usr/local/bin/dsh-gate start --foreground --host 0.0.0.0 --port 12081 --state-dir /srv/state"
	if got != want {
		t.Errorf("ExecStart = %q，期望 %q", got, want)
	}
	if execStartFromUnit("[Service]\nType=simple\n") != "" {
		t.Error("没有 ExecStart 时应返回空串")
	}
}

func TestProgramArgumentsFromPlist(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
  <dict>
    <key>Label</key>
    <string>com.wycto.dsh-gate</string>
    <key>ProgramArguments</key>
    <array>
      <string>/usr/local/bin/dsh-gate</string>
      <string>start</string>
      <string>--foreground</string>
      <string>--name</string>
      <string>personal&amp;work</string>
    </array>
    <key>RunAtLoad</key>
    <string>true</string>
  </dict>
</plist>
`
	got := programArgumentsFromPlist(plist)
	want := "/usr/local/bin/dsh-gate start --foreground --name personal&work"
	if got != want {
		t.Errorf("ProgramArguments = %q，期望 %q", got, want)
	}
	if programArgumentsFromPlist("<plist><dict></dict></plist>") != "" {
		t.Error("没有 ProgramArguments 时应返回空串")
	}
}

func TestCommandFromTaskXML(t *testing.T) {
	output := `<Task version="1.2">
  <Actions Context="Author">
    <Exec>
      <Command>C:\Program Files\dsh-gate\dsh-gate.exe</Command>
      <Arguments>run --log-json --name work</Arguments>
    </Exec>
  </Actions>
</Task>`
	got := commandFromTaskXML(output)
	want := `C:\Program Files\dsh-gate\dsh-gate.exe run --log-json --name work`
	if got != want {
		t.Errorf("任务命令 = %q，期望 %q", got, want)
	}
	if commandFromTaskXML("<Task></Task>") != "" {
		t.Error("没有 Command 时应返回空串")
	}
}
