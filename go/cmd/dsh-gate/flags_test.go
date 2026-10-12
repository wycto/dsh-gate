package main

import (
	"flag"
	"io"
	"testing"
)

// TestRunDirIsNotAnInstanceOption 回归：run 自己也注册了 --run-dir，但它是
// 「机器级」设置而不是实例配置。漏进 overrides 之后会被 config.Apply 拒成
// 「内部错误：未知配置项 "run-dir"」——命令支持这个选项却一用就报错。
func TestRunDirIsNotAnInstanceOption(t *testing.T) {
	if !isRunModeFlag("run-dir") {
		t.Fatal("run-dir 应被当作「怎么启动」的选项，而不是实例配置")
	}

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerInstanceFlags(fs)
	fs.String("run-dir", "/tmp/run", "运行登记目录")
	fs.String("log-level", "info", "日志级别")

	if err := fs.Parse([]string{"--run-dir", "/tmp/custom-run", "--log-level", "debug", "--port", "1234"}); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	values, err := overrides(fs, nil)
	if err != nil {
		t.Fatalf("overrides 报错: %v", err)
	}
	if _, ok := values["run-dir"]; ok {
		t.Errorf("--run-dir 不该出现在实例配置里：%v", values)
	}
	if _, ok := values["log-level"]; ok {
		t.Errorf("--log-level 不该出现在实例配置里：%v", values)
	}
	if values["port"] != "1234" {
		t.Errorf("实例选项 port = %q，期望 1234", values["port"])
	}
}
