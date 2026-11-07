package portal

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openKeyTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAPIKeyCreateAndAuthenticate(t *testing.T) {
	s := openKeyTestStore(t)
	secret, key, err := s.CreateAPIKey("ci-bot", []string{ScopeServicesRead, ScopeServicesWrite}, "10.0.0.0/8", "admin")
	if err != nil {
		t.Fatalf("创建密钥失败: %v", err)
	}
	if len(secret) < 12 || secret[:4] != "dgk_" {
		t.Fatalf("密钥格式不对: %q", secret)
	}
	if key.ID == "" || key.Prefix != secret[:12] {
		t.Errorf("id/prefix 不对: %+v", key)
	}
	if key.Hash == secret || key.Hash == "" {
		t.Error("库里不应存明文")
	}
	if !key.HasScope(ScopeServicesRead) || key.HasScope(ScopeInstancesRead) {
		t.Errorf("scope 判定不对: %v", key.ScopeList())
	}

	// 正确密钥
	got, err := s.AuthenticateAPIKey(secret)
	if err != nil || got.ID != key.ID {
		t.Fatalf("鉴权应成功: %v / %+v", err, got)
	}
	// 错误密钥
	if _, err := s.AuthenticateAPIKey("dgk_" + "deadbeef"); !errors.Is(err, ErrAPIKeyInvalid) {
		t.Errorf("错误密钥应 invalid，实际 %v", err)
	}
	// 非 dgk_ 前缀
	if _, err := s.AuthenticateAPIKey("nope"); !errors.Is(err, ErrAPIKeyInvalid) {
		t.Errorf("非法前缀应 invalid，实际 %v", err)
	}
	// 停用后失效
	if err := s.SetAPIKeyDisabled(key.ID, true); err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if _, err := s.AuthenticateAPIKey(secret); !errors.Is(err, ErrAPIKeyInvalid) {
		t.Errorf("停用后应 invalid，实际 %v", err)
	}
	// 删除
	if err := s.DeleteAPIKey(key.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := s.APIKeyByID(key.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后应查不到")
	}
}

func TestAPIKeyValidationAndList(t *testing.T) {
	s := openKeyTestStore(t)
	if _, _, err := s.CreateAPIKey("", nil, "", ""); err == nil {
		t.Error("空名应拒绝")
	}
	if _, _, err := s.CreateAPIKey("x", []string{"bogus:scope"}, "", ""); err == nil {
		t.Error("非法 scope 应拒绝")
	}
	if _, _, err := s.CreateAPIKey("k1", nil, "", "admin"); err != nil {
		t.Fatalf("无 scope 密钥应允许: %v", err)
	}
	if _, _, err := s.CreateAPIKey("k1", nil, "", "admin"); err == nil {
		t.Error("重名应拒绝")
	}
	list, err := s.ListAPIKeys()
	if err != nil || len(list) != 1 {
		t.Fatalf("列表应 1 条: %v / %d", err, len(list))
	}
}

func TestMatchIPAllowlist(t *testing.T) {
	cases := []struct {
		allow, ip string
		want      bool
	}{
		{"", "1.2.3.4", true},
		{"10.0.0.0/8", "10.1.2.3", true},
		{"10.0.0.0/8", "192.168.1.1", false},
		{"192.168.1.5, 10.0.0.0/8", "192.168.1.5", true},
		{"192.168.1.5", "192.168.1.6", false},
	}
	for _, c := range cases {
		if got := MatchIPAllowlist(c.allow, c.ip); got != c.want {
			t.Errorf("MatchIPAllowlist(%q,%q)=%v want %v", c.allow, c.ip, got, c.want)
		}
	}
}

// TestMigrationV6ToV7 验证 v6 库升级到 v7：老数据不动，api_keys 可用、外部 API 缺省关。
func TestMigrationV6ToV7(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portal.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("造库失败: %v", err)
	}
	for _, schema := range []string{schemaV1, schemaV2, schemaV3, schemaV4, schemaV5, schemaV6} {
		if _, err := db.Exec(schema); err != nil {
			t.Fatalf("建老结构失败: %v", err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 6"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	now := nowText()
	if _, err := db.Exec("INSERT INTO instances (id,name,owner_id,dsh_home,workspace,dsh_port,dsh_cmd,on_demand,idle_timeout_s,enabled,created_at,updated_at) VALUES ('old6','legacy6',NULL,'/tmp/l6','',32006,'',0,0,1,?,?)", now, now); err != nil {
		t.Fatalf("插老记录失败: %v", err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("迁移到 v7 失败: %v", err)
	}
	defer s.Close()
	if _, err := s.InstanceByName("legacy6"); err != nil {
		t.Errorf("老实例不该受影响: %v", err)
	}
	if _, _, err := s.CreateAPIKey("k", []string{ScopeServicesRead}, "", "admin"); err != nil {
		t.Errorf("迁移后应能建密钥: %v", err)
	}
	enabled, err := s.ExternalAPIEnabled()
	if err != nil || enabled {
		t.Errorf("外部 API 缺省应为关: %v / %v", err, enabled)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Errorf("结构版本应为 %d，实际 %d (%v)", SchemaVersion, version, err)
	}
}
