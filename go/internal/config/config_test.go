package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wycto/dsh-gate/internal/config"
)

func TestDefaultIsUsable(t *testing.T) {
	cfg := config.Default()
	cfg.DSHHome = t.TempDir()
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("默认配置应当开箱可用，实际报错: %v", err)
	}
	if cfg.ListenAddr() != "0.0.0.0:3082" {
		t.Errorf("默认网关地址 = %q", cfg.ListenAddr())
	}
	if cfg.DSHAddr() != "127.0.0.1:3080" {
		t.Errorf("默认 dsh 地址 = %q", cfg.DSHAddr())
	}
	if cfg.UpstreamURL() != "http://127.0.0.1:3080" {
		t.Errorf("默认上游 = %q", cfg.UpstreamURL())
	}
	// 账号表默认落在**网关自己的**状态目录里，而不是 dsh 的实例目录：
	// 后者是 dsh 的数据（会话、插件、配置），不该混进网关的文件。
	if want := filepath.Join(config.DefaultStateDir(), "users.json"); cfg.UsersFile != want {
		t.Errorf("默认账号表 = %q，期望 %q", cfg.UsersFile, want)
	}
	if strings.HasPrefix(cfg.UsersFile, cfg.DSHHome) {
		t.Errorf("账号表 %q 不应落在 dsh 实例目录 %q 里", cfg.UsersFile, cfg.DSHHome)
	}
	if cfg.StateDirPath() != config.DefaultStateDir() {
		t.Errorf("默认状态目录 = %q", cfg.StateDirPath())
	}
}

func TestStateDirIsConfigurable(t *testing.T) {
	// 网关状态目录可以挪走，且挪走之后账号表跟着走。
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DSHHome = t.TempDir()
	cfg.StateDir = dir
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("Normalize 失败: %v", err)
	}
	if want := filepath.Join(dir, "users.json"); cfg.UsersFile != want {
		t.Errorf("账号表 = %q，期望 %q", cfg.UsersFile, want)
	}
	// 显式指定的账号表路径不受状态目录影响。
	cfg2 := config.Default()
	cfg2.DSHHome = t.TempDir()
	cfg2.StateDir = dir
	cfg2.UsersFile = filepath.Join(dir, "team.json")
	if err := cfg2.Normalize(); err != nil {
		t.Fatalf("Normalize 失败: %v", err)
	}
	if cfg2.UsersFile != filepath.Join(dir, "team.json") {
		t.Errorf("显式账号表被改写了：%q", cfg2.UsersFile)
	}
}

func TestNormalizeExpandsTildeAndTrailingSlash(t *testing.T) {
	cfg := config.Default()
	cfg.DSHHome = "~/some-instance"
	cfg.UsersFile = "~/shared/users.json"
	cfg.PublicURL = "http://example.test:8080"
	cfg.Normalize()

	if strings.HasPrefix(cfg.DSHHome, "~") {
		t.Errorf("实例目录应展开 ~，实际 %q", cfg.DSHHome)
	}
	if !strings.HasPrefix(cfg.UsersFile, "/") {
		t.Errorf("账号表应展开成绝对路径，实际 %q", cfg.UsersFile)
	}
	if cfg.PublicURL != "http://example.test:8080/" {
		t.Errorf("公告地址应补上结尾斜杠，实际 %q", cfg.PublicURL)
	}
}

func TestValidateRejectsWildcardDSHHost(t *testing.T) {
	// dsh 自己会拒绝 0.0.0.0，网关必须提前拦下来并说明原因，
	// 而不是让用户看到 dsh 抛出的英文用法错误。
	for _, host := range []string{"0.0.0.0", "::", "0:0:0:0:0:0:0:0"} {
		cfg := config.Default()
		cfg.DSHHome = t.TempDir()
		cfg.DSHHost = host
		err := cfg.Normalize()
		if err == nil {
			t.Fatalf("%s 应当被拒绝", host)
		}
		if !strings.Contains(err.Error(), "通配") {
			t.Errorf("错误信息应当解释「dsh 不接受通配地址」，实际: %v", err)
		}
	}
}

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Instance)
		wantErr string
	}{
		{"端口越界", func(c *config.Instance) { c.Port = 70000 }, "超出范围"},
		{"负端口", func(c *config.Instance) { c.Port = -1 }, "超出范围"},
		{"实例名非法字符", func(c *config.Instance) { c.Name = "my instance" }, "非法字符"},
		{"实例名过长", func(c *config.Instance) { c.Name = strings.Repeat("a", 40) }, "过长"},
		{"网关与 dsh 同端口同地址", func(c *config.Instance) {
			c.Host = "127.0.0.1"
			c.DSHHost = "127.0.0.1"
			c.Port = 4000
			c.DSHPort = 4000
		}, "自己撞自己"},
		{"证书只有一半", func(c *config.Instance) { c.TLSCert = "/tmp/x.pem" }, "成对"},
		{"公告地址协议不对", func(c *config.Instance) { c.PublicURL = "ftp://x/" }, "http"},
		{"实例目录不是绝对路径", func(c *config.Instance) { c.DSHHome = "relative/dir" }, "绝对路径"},
		{"会话有效期为零", func(c *config.Instance) { c.SessionTTL = 0 }, "必须为正"},
		{"失败次数为零", func(c *config.Instance) { c.MaxLoginFails = 0 }, "至少为 1"},
		// 端口 0 是合法的：交给内核分配，测试与临时实例都用得上。
		{"端口 0 合法", func(c *config.Instance) { c.Port = 0 }, ""},
		{"dsh 主机名放行", func(c *config.Instance) { c.DSHHost = "localhost" }, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.DSHHome = t.TempDir()
			tc.mutate(&cfg)
			err := cfg.Normalize()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错，实际: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当报错")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息应当包含 %q，实际: %v", tc.wantErr, err)
			}
		})
	}
}

func TestApplyOverrides(t *testing.T) {
	cfg := config.Default()
	err := config.Apply(&cfg, map[string]string{
		"port":         "9000",
		"dsh-port":     "9001",
		"dsh-home":     "~/work",
		"trusted-host": "a.test, b.test",
		"auto-start":   "false",
		"lockout":      "30s",
	})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if cfg.Port != 9000 || cfg.DSHPort != 9001 {
		t.Errorf("端口没被覆盖: %d / %d", cfg.Port, cfg.DSHPort)
	}
	if strings.HasPrefix(cfg.DSHHome, "~") {
		t.Errorf("~ 未展开: %q", cfg.DSHHome)
	}
	if len(cfg.TrustedHosts) != 2 || cfg.TrustedHosts[0] != "a.test" || cfg.TrustedHosts[1] != "b.test" {
		t.Errorf("trusted-host 解析错误: %v", cfg.TrustedHosts)
	}
	if cfg.AutoStart {
		t.Error("auto-start 未被关闭")
	}
	if cfg.LockoutBase.String() != "30s" {
		t.Errorf("lockout = %s", cfg.LockoutBase)
	}
	// 没提到的字段必须保持默认值——这是「优先级」能成立的前提。
	if cfg.DSHHost != config.Default().DSHHost {
		t.Errorf("未指定的字段被改动了: %q", cfg.DSHHost)
	}
}

func TestApplyRejectsGarbage(t *testing.T) {
	cfg := config.Default()
	if err := config.Apply(&cfg, map[string]string{"port": "abc"}); err == nil {
		t.Error("非数字端口应当报错")
	}
	if err := config.Apply(&cfg, map[string]string{"unknown-key": "x"}); err == nil {
		t.Error("未知配置项应当报错")
	}
}

func TestApplyEnv(t *testing.T) {
	cfg := config.Default()
	env := map[string]string{
		"DSH_GATE_PORT":          "7777",
		"DSH_GATE_DSH_HOME":      "/tmp/inst",
		"DSH_GATE_TRUSTED_HOSTS": "x.test, y.test",
		"DSH_GATE_RESTART":       "false",
		"DSH_GATE_SESSION_TTL":   "24h",
	}
	getenv := func(key string) string { return env[key] }
	if err := config.ApplyEnv(&cfg, getenv); err != nil {
		t.Fatalf("ApplyEnv 失败: %v", err)
	}
	if cfg.Port != 7777 {
		t.Errorf("端口 = %d", cfg.Port)
	}
	if cfg.DSHHome != "/tmp/inst" {
		t.Errorf("实例目录 = %q", cfg.DSHHome)
	}
	if len(cfg.TrustedHosts) != 2 {
		t.Errorf("trusted hosts = %v", cfg.TrustedHosts)
	}
	if cfg.Restart {
		t.Error("DSH_GATE_RESTART=false 未生效")
	}
	if cfg.SessionTTL.Hours() != 24 {
		t.Errorf("会话有效期 = %s", cfg.SessionTTL)
	}
}

func TestApplyEnvReportsBadValue(t *testing.T) {
	cfg := config.Default()
	err := config.ApplyEnv(&cfg, func(key string) string {
		if key == "DSH_GATE_PORT" {
			return "not-a-port"
		}
		return ""
	})
	if err == nil {
		t.Fatal("非法的环境变量值应当报错")
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "instances.json")
	body := `{
	  "defaults": { "host": "0.0.0.0", "users": "` + filepath.Join(dir, "users.json") + `" },
	  "instances": [
	    { "name": "personal", "port": 8081, "dshPort": 3081, "dshHome": "` + filepath.Join(dir, "personal") + `" },
	    { "name": "company",  "port": 8082, "dshPort": 3082, "dshHome": "` + filepath.Join(dir, "company") + `" }
	  ]
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	instances, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile 失败: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("应当有 2 个实例，实际 %d", len(instances))
	}
	// defaults 里的字段应当被两个实例都继承。
	for _, instance := range instances {
		if instance.Host != "0.0.0.0" {
			t.Errorf("%s 未继承 defaults.host: %q", instance.Name, instance.Host)
		}
		if instance.UsersFile != filepath.Join(dir, "users.json") {
			t.Errorf("%s 未继承 defaults.users: %q", instance.Name, instance.UsersFile)
		}
	}
	if instances[0].Name != "personal" || instances[1].Name != "company" {
		t.Errorf("实例顺序不对: %s, %s", instances[0].Name, instances[1].Name)
	}
	if instances[1].DSHPort != 3082 {
		t.Errorf("dsh 端口 = %d", instances[1].DSHPort)
	}
}

func TestLoadFileAllowsSamePortOnDifferentHosts(t *testing.T) {
	// 一个绑回环、一个绑具体网卡，用同一个端口完全合法——跨主机冲突判定太粗
	// 会把用户逼进「所有实例端口都不能重复」的死路。
	path := filepath.Join(t.TempDir(), "instances.json")
	body := `{"instances":[
	  {"name":"a","host":"127.0.0.1","port":9000,"dshPort":3080,"dshHome":"/tmp/a"},
	  {"name":"b","host":"192.168.1.5","port":9000,"dshPort":3081,"dshHome":"/tmp/b"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadFile(path); err != nil {
		t.Fatalf("不同主机上的同端口应当被允许，实际: %v", err)
	}
}

func TestLoadFileRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"字段拼错":    `{"instances":[{"nmae":"x"}]}`,
		"实例为空":    `{"instances":[]}`,
		"JSON 之外": `{"instances":[{}]} oops`,
		"名字重复":    `{"instances":[{"name":"a","dshHome":"/tmp/a"},{"name":"a","dshHome":"/tmp/b"}]}`,
		"端口相同":    `{"instances":[{"name":"a","port":9000,"dshHome":"/tmp/a"},{"name":"b","port":9000,"dshHome":"/tmp/b"}]}`,
		// 一个实例的网关端口撞上另一个实例的 dsh 端口：两个网关会互相把流量
		// 转发给对方，转过去的结果完全无法解释。
		"跨角色撞端口": `{"instances":[{"name":"a","port":9000,"dshPort":3080,"dshHome":"/tmp/a"},{"name":"b","port":9001,"dshPort":9000,"dshHome":"/tmp/b"}]}`,
		// 两个实例抢同一个 dsh。
		"dsh 端口相同": `{"instances":[{"name":"a","dshPort":3080,"dshHome":"/tmp/a"},{"name":"b","dshPort":3080,"dshHome":"/tmp/b"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "instances.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadFile(path); err == nil {
				t.Fatalf("应当报错")
			}
		})
	}
}
