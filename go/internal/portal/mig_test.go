package portal

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationV1ToV2 验证老库（v1 结构）能升到 v2，且老实例行为不变。
func TestMigrationV1ToV2(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portal.db")

	// 手工造一个 v1 库：用 v1 建表语句 + user_version=1。
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("造 v1 库失败: %v", err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatalf("建 v1 结构失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	// 插一条 v1 时代的实例记录（只有老列）。
	now := nowText()
	if _, err := db.Exec(`INSERT INTO instances
		(id, name, owner_id, dsh_home, workspace, dsh_port, dsh_cmd, on_demand, idle_timeout_s, enabled, created_at, updated_at)
		VALUES ('old1','legacy',NULL,'/tmp/legacy','',32000,'',0,0,1,?,?)`, now, now); err != nil {
		t.Fatalf("插老记录失败: %v", err)
	}
	_ = db.Close()

	// 用 Open 打开：应自动迁移到 v2。
	store, err := Open(path)
	if err != nil {
		t.Fatalf("打开并迁移失败: %v", err)
	}
	defer store.Close()

	inst, err := store.InstanceByName("legacy")
	if err != nil {
		t.Fatalf("读老实例失败: %v", err)
	}
	// 老实例的默认值应归一成：npx / auto / 127.0.0.1，且 OnDemand=false。
	if inst.InstallMode != "npx" {
		t.Errorf("老实例 install_mode 应归一为 npx，实际 %q", inst.InstallMode)
	}
	if inst.StartMode != StartModeAuto {
		t.Errorf("老实例 start_mode 应归一为 auto，实际 %q", inst.StartMode)
	}
	if inst.ListenHost != "127.0.0.1" {
		t.Errorf("老实例 listen_host 应归一为 127.0.0.1，实际 %q", inst.ListenHost)
	}
	if inst.OnDemand {
		t.Error("老实例不该被改成按需")
	}
	if inst.DshHome != "/tmp/legacy" {
		t.Errorf("老数据不该丢，实际 %q", inst.DshHome)
	}
}
