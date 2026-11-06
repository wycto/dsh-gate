package portal

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// openTestStore 开一个临时库（与 store_test.go 同一做法）。
func openServiceTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestServiceCRUDRoundtrip(t *testing.T) {
	store := openServiceTestStore(t)
	svc := &Service{
		Name: "内网wiki", Upstream: "https://wiki.corp.example:8443/base/",
		HostHeader: HostHeaderGateway, InsecureTLS: true, Note: "知识库",
	}
	if err := store.CreateService(svc); err != nil {
		t.Fatalf("登记服务失败: %v", err)
	}
	if !ServiceIDPattern.MatchString(svc.ID) {
		t.Fatalf("生成的 id 不满足白名单: %q", svc.ID)
	}
	if svc.Upstream != "https://wiki.corp.example:8443/base" {
		t.Errorf("上游应归一去掉尾斜杠，实际 %q", svc.Upstream)
	}
	if !svc.Enabled {
		t.Error("新建服务应默认启用")
	}
	if svc.Source != ServiceSourceManual || svc.RouteMode != RouteModePath || svc.HostHeader != HostHeaderGateway {
		t.Errorf("默认值归一不对: %+v", svc)
	}

	got, err := store.ServiceByName("内网wiki")
	if err != nil {
		t.Fatalf("按名查服务失败: %v", err)
	}
	if got.ID != svc.ID || !got.InsecureTLS || got.Note != "知识库" {
		t.Errorf("往返不一致: %+v", got)
	}

	got.Name = "wiki"
	got.Upstream = "http://10.0.0.8:8080"
	got.Enabled = false
	got.InsecureTLS = false
	got.HostHeader = ""
	if err := store.UpdateService(*got); err != nil {
		t.Fatalf("更新服务失败: %v", err)
	}
	again, err := store.ServiceByID(got.ID)
	if err != nil {
		t.Fatalf("重读服务失败: %v", err)
	}
	if again.Name != "wiki" || again.Upstream != "http://10.0.0.8:8080" || again.Enabled || again.InsecureTLS {
		t.Errorf("更新未生效: %+v", again)
	}
	if again.HostHeader != HostHeaderUpstream {
		t.Errorf("空 Host 策略应归一为 upstream，实际 %q", again.HostHeader)
	}

	list, err := store.ListServices()
	if err != nil || len(list) != 1 {
		t.Fatalf("列表应为一条: %v / %d", err, len(list))
	}
	if err := store.DeleteService(again.ID); err != nil {
		t.Fatalf("删除服务失败: %v", err)
	}
	if _, err := store.ServiceByID(again.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("删除后应查不到，实际 %v", err)
	}
	if err := store.DeleteService(again.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除应报不存在，实际 %v", err)
	}
}

func TestServiceValidation(t *testing.T) {
	store := openServiceTestStore(t)
	bad := []struct {
		name string
		svc  Service
	}{
		{"空名", Service{Upstream: "http://10.0.0.1:80"}},
		{"非法名", Service{Name: "-lead", Upstream: "http://10.0.0.1:80"}},
		{"空上游", Service{Name: "a"}},
		{"ftp  scheme", Service{Name: "a", Upstream: "ftp://10.0.0.1"}},
		{"带口令", Service{Name: "a", Upstream: "http://user:pass@10.0.0.1"}},
		{"带查询", Service{Name: "a", Upstream: "http://10.0.0.1/?x=1"}},
		{"带碎片", Service{Name: "a", Upstream: "http://10.0.0.1/#frag"}},
		{"坏端口", Service{Name: "a", Upstream: "http://10.0.0.1:99999"}},
		{"坏 Host 策略", Service{Name: "a", Upstream: "http://10.0.0.1", HostHeader: "rewrite"}},
		{"未开放路由", Service{Name: "a", Upstream: "http://10.0.0.1", RouteMode: "port"}},
		{"预留端口", Service{Name: "a", Upstream: "http://10.0.0.1", ListenPort: 9000}},
	}
	for _, tc := range bad {
		svc := tc.svc
		if err := store.CreateService(&svc); err == nil {
			t.Errorf("%s：应拒绝，实际通过了", tc.name)
		}
	}
	ok := &Service{Name: "dup", Upstream: "http://10.0.0.1:80"}
	if err := store.CreateService(ok); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	dup := &Service{Name: "dup", Upstream: "http://10.0.0.2:80"}
	if err := store.CreateService(dup); err == nil {
		t.Error("重名应拒绝")
	}
}

func TestServiceGrantsAndAccessible(t *testing.T) {
	store := openServiceTestStore(t)
	admin, err := store.CreateUser("admin", "pw-123456", RoleAdmin)
	if err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	alice, err := store.CreateUser("alice", "pw-123456", RoleUser)
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if _, err := store.CreateUser("bob", "pw-123456", RoleUser); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	svc := &Service{Name: "wiki", Upstream: "http://10.0.0.1:80"}
	if err := store.CreateService(svc); err != nil {
		t.Fatalf("登记服务失败: %v", err)
	}

	// 未授权：与不存在同形。
	bob, _ := store.UserByName("bob")
	if _, err := store.ServiceAccessible(bob, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("未授权应回 ErrNotFound，实际 %v", err)
	}
	// 管理员天然可见。
	if _, err := store.ServiceAccessible(admin, svc.ID); err != nil {
		t.Errorf("管理员应可进: %v", err)
	}
	visible, err := store.VisibleServices(alice)
	if err != nil || len(visible) != 0 {
		t.Fatalf("授权前 alice 不应看到服务: %v / %d", err, len(visible))
	}

	if err := store.GrantService("alice", svc.ID, "admin"); err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	if err := store.GrantService("alice", svc.ID, "admin"); err != nil {
		t.Errorf("重复授权应幂等: %v", err)
	}
	if _, err := store.ServiceAccessible(alice, svc.ID); err != nil {
		t.Errorf("授权后应可进: %v", err)
	}
	names, err := store.ServiceGrants(svc.ID)
	if err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatalf("服务授权列表不对: %v / %v", err, names)
	}
	visible, err = store.VisibleServices(alice)
	if err != nil || len(visible) != 1 {
		t.Fatalf("授权后 alice 应看到一条: %v / %d", err, len(visible))
	}

	// 矩阵整体覆盖。
	if err := store.SetUserServiceGrants("alice", []string{}, "admin"); err != nil {
		t.Fatalf("清空授权失败: %v", err)
	}
	if _, err := store.ServiceAccessible(alice, svc.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("清空后应不可进: %v", err)
	}

	// 删服务级联删授权；删用户级联删授权。
	if err := store.GrantService("alice", svc.ID, "admin"); err != nil {
		t.Fatalf("再授权失败: %v", err)
	}
	if err := store.DeleteService(svc.ID); err != nil {
		t.Fatalf("删服务失败: %v", err)
	}
	var n int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM service_grants").Scan(&n); err != nil || n != 0 {
		t.Fatalf("删服务应级联删授权: %v / %d", err, n)
	}
	svc2 := &Service{Name: "git", Upstream: "http://10.0.0.2:3000"}
	if err := store.CreateService(svc2); err != nil {
		t.Fatalf("登记第二个服务失败: %v", err)
	}
	if err := store.GrantService("alice", svc2.ID, "admin"); err != nil {
		t.Fatalf("授权失败: %v", err)
	}
	if err := store.DeleteUser("alice"); err != nil {
		t.Fatalf("删用户失败: %v", err)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM service_grants").Scan(&n); err != nil || n != 0 {
		t.Fatalf("删用户应级联删授权: %v / %d", err, n)
	}
	_ = bob
}

// TestMigrationV3ToV4 验证 v3 库升到 v4：老数据不动，新表可用。
func TestMigrationV3ToV4(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portal.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("造 v3 库失败: %v", err)
	}
	for _, schema := range []string{schemaV1, schemaV2, schemaV3} {
		if _, err := db.Exec(schema); err != nil {
			t.Fatalf("建老结构失败: %v", err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	now := nowText()
	if _, err := db.Exec(`INSERT INTO instances
		(id, name, owner_id, dsh_home, workspace, dsh_port, dsh_cmd, on_demand, idle_timeout_s, enabled, created_at, updated_at)
		VALUES ('old3','legacy3',NULL,'/tmp/legacy3','',32003,'',0,0,1,?,?)`, now, now); err != nil {
		t.Fatalf("插老记录失败: %v", err)
	}
	_ = db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("打开并迁移到 v4 失败: %v", err)
	}
	defer store.Close()

	inst, err := store.InstanceByName("legacy3")
	if err != nil || inst.DshPort != 32003 {
		t.Fatalf("老实例不该受影响: %v / %+v", err, inst)
	}
	svc := &Service{Name: "wiki", Upstream: "http://10.0.0.1:80"}
	if err := store.CreateService(svc); err != nil {
		t.Fatalf("迁移后应能登记服务: %v", err)
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读结构版本失败: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("结构版本应升到 %d，实际 %d", SchemaVersion, version)
	}
}
