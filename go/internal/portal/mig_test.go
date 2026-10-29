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

// TestMigrationV2ToV3 验证 v2 库能升到 v3，且老实例**不会**因为升级而被钉住版本。
//
// 默认值必须是空串（= 跟随 latest）：老库里那些实例此前一直跟着 latest 跑，升级
// 之后如果它们突然变成钉在某个版本上，用户不会注意到，只会发现「再也不更新了」。
func TestMigrationV2ToV3(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portal.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("造 v2 库失败: %v", err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatalf("建 v1 结构失败: %v", err)
	}
	if _, err := db.Exec(schemaV2); err != nil {
		t.Fatalf("补 v2 结构失败: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatalf("写版本失败: %v", err)
	}
	now := nowText()
	if _, err := db.Exec(`INSERT INTO instances
		(id, name, owner_id, dsh_home, workspace, dsh_port, dsh_cmd, install_mode, install_dir, on_demand, idle_timeout_s, enabled, created_at, updated_at)
		VALUES ('old2','legacy2',NULL,'/tmp/legacy2','',32001,'','pnpm','/srv/dsh/legacy2',0,0,1,?,?)`, now, now); err != nil {
		t.Fatalf("插 v2 记录失败: %v", err)
	}
	_ = db.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("打开并迁移到 v3 失败: %v", err)
	}
	defer store.Close()

	inst, err := store.InstanceByName("legacy2")
	if err != nil {
		t.Fatalf("读老实例失败: %v", err)
	}
	if inst.DshVersion != "" {
		t.Errorf("升级不该给老实例钉版本，实际 %q", inst.DshVersion)
	}
	if inst.InstallMode != "pnpm" || inst.InstallDir != "/srv/dsh/legacy2" {
		t.Errorf("v2 的安装设置不该丢：实际 %q / %q", inst.InstallMode, inst.InstallDir)
	}

	// 钉住的版本要能写回、能读回（走的是 UPDATE 那条路）。
	inst.DshVersion = "0.2.2-alpha.2"
	if err := store.UpdateInstance(*inst); err != nil {
		t.Fatalf("写入钉住的版本失败: %v", err)
	}
	again, err := store.InstanceByName("legacy2")
	if err != nil {
		t.Fatalf("重读失败: %v", err)
	}
	if again.DshVersion != "0.2.2-alpha.2" {
		t.Errorf("重读应为 0.2.2-alpha.2，实际 %q", again.DshVersion)
	}

	// 新建实例走 INSERT，同样要带上版本。
	fresh := &Instance{Name: "pinned", DshHome: "/tmp/pinned", DshPort: 32002, DshVersion: "next"}
	if err := store.CreateInstance(fresh); err != nil {
		t.Fatalf("新建带版本的实例失败: %v", err)
	}
	created, err := store.InstanceByName("pinned")
	if err != nil {
		t.Fatalf("读新实例失败: %v", err)
	}
	if created.DshVersion != "next" {
		t.Errorf("新实例的钉住版本应为 next，实际 %q", created.DshVersion)
	}

	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("读结构版本失败: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("结构版本应升到 %d，实际 %d", SchemaVersion, version)
	}
}
