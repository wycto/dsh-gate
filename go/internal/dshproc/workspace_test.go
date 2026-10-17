package dshproc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// readState 读回注册表用于断言。
func readState(t *testing.T, dshHome string) WorkspaceStorageUnit {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dshHome, workspaceStorageRel))
	if err != nil {
		t.Fatalf("读注册表失败: %v", err)
	}
	var state WorkspaceStorageUnit
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("解析注册表失败: %v", err)
	}
	return state
}

// TestSeedDefaultWorkspaceCreatesRegistry 验证空实例会被预置一个默认工作区。
//
// 现场是：每个实例第一次进去都停在「选择一个工作区开始」，用户得手动挑一次才
// 能开始对话——而工作目录明明在实例登记里配好了。
func TestSeedDefaultWorkspaceCreatesRegistry(t *testing.T) {
	dshHome := t.TempDir()
	work := filepath.Join(t.TempDir(), "my-work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := SeedDefaultWorkspace(nil, dshHome, work); err != nil {
		t.Fatalf("预置失败: %v", err)
	}

	state := readState(t, dshHome)
	if state.Unit.Name != "workspace" || state.Unit.Version != workspaceStorageVersion {
		t.Fatalf("存储单元头不对: %+v", state.Unit)
	}
	if !state.Global.Initialized {
		t.Error("initialized 应为 true")
	}
	if len(state.Global.WorkspaceIDs) != 1 {
		t.Fatalf("应当恰好登记一个工作区，实际 %v", state.Global.WorkspaceIDs)
	}
	id := state.Global.WorkspaceIDs[0]
	if state.Global.DefaultWorkspaceID != id {
		t.Errorf("默认工作区应当指向刚登记的那个：default=%q id=%q",
			state.Global.DefaultWorkspaceID, id)
	}
	rec, ok := state.Tables.Workspaces[id]
	if !ok {
		t.Fatalf("workspaces 表里没有 %q", id)
	}
	if rec.Path != work {
		t.Errorf("路径应当是 %q，实际 %q", work, rec.Path)
	}
	if rec.Title != "my-work" {
		t.Errorf("标题应当取目录名 my-work，实际 %q", rec.Title)
	}
	if rec.CreatedAt == "" || rec.UpdatedAt == "" {
		t.Error("时间戳不能为空")
	}
	// id 必须是 uuid 的样子（dsh 用 randomUUID 并且明确「不是路径」）。
	if len(id) != 36 || id[8] != '-' || id[13] != '-' {
		t.Errorf("工作区 id 不像 uuid：%q", id)
	}
}

// TestSeedDefaultWorkspaceKeepsUserChoice 是本功能最重要的一条纪律：
// 用户自己挑过工作区之后，网关绝不能再插手覆盖。
func TestSeedDefaultWorkspaceKeepsUserChoice(t *testing.T) {
	dshHome := t.TempDir()
	work := filepath.Join(t.TempDir(), "auto")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaultWorkspace(nil, dshHome, work); err != nil {
		t.Fatalf("预置失败: %v", err)
	}

	// 模拟用户在 dsh 里把网关预置的那个删掉、换成自己挑的目录。
	// 这一条必须**只**由「注册表非空就不插手」来挡——所以要把预置的那条也删掉，
	// 否则「同一个路径已经登记过」那条检查会先返回，测不到真正的守卫。
	path := filepath.Join(dshHome, workspaceStorageRel)
	userID := "11111111-2222-4333-8444-555555555555"
	state := WorkspaceStorageUnit{}
	state.Unit.Name = "workspace"
	state.Unit.Version = workspaceStorageVersion
	state.Global.Initialized = true
	state.Global.DefaultWorkspaceID = userID
	state.Global.WorkspaceIDs = []string{userID}
	state.Global.ArchivedSessionIDs = []string{}
	state.Global.PinnedSessionIDs = []string{}
	state.Tables.Workspaces = map[string]workspaceRecord{userID: {
		Path: "/srv/user-picked", Title: "user-picked",
		SessionIDs: []string{}, CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}}
	raw, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// 再跑一次（每次启动都会跑）——必须原样不动。
	if err := SeedDefaultWorkspace(nil, dshHome, work); err != nil {
		t.Fatalf("第二次预置失败: %v", err)
	}
	after := readState(t, dshHome)
	if after.Global.DefaultWorkspaceID != userID {
		t.Errorf("用户选的默认工作区被覆盖了：%q", after.Global.DefaultWorkspaceID)
	}
	if len(after.Global.WorkspaceIDs) != 1 {
		t.Errorf("不该把预置的工作区又塞回来，实际 %v", after.Global.WorkspaceIDs)
	}
	if _, ok := after.Tables.Workspaces[userID]; !ok {
		t.Error("用户自己的工作区被删掉了")
	}
	if len(after.Tables.Workspaces) != 1 {
		t.Errorf("workspaces 表不该被改动，实际 %d 条", len(after.Tables.Workspaces))
	}
}

// TestSeedDefaultWorkspaceIgnoresUnknownVersion 验证认不出的存储格式就原样放着。
//
// dsh 把存储版本升到 3 之后，旧字段可能已经不兼容——宁可退回「手动挑一次」，
// 也不要把用户的注册表写坏。
func TestSeedDefaultWorkspaceIgnoresUnknownVersion(t *testing.T) {
	dshHome := t.TempDir()
	work := filepath.Join(t.TempDir(), "auto")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dshHome, "storages")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"unit":{"name":"workspace","version":3},"global":{"initialized":true,"workspaceIds":[]},"tables":{"workspaces":{}}}`
	path := filepath.Join(dir, "workspace.json")
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SeedDefaultWorkspace(nil, dshHome, work); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != original {
		t.Errorf("版本不认识时必须原样留着，实际被改成了 %s", raw)
	}
}

// TestSeedDefaultWorkspaceSkipsMissingDir 验证工作目录不存在时不写。
//
// 那种情况写进去只会让 dsh 打开一个不存在的目录。
func TestSeedDefaultWorkspaceSkipsMissingDir(t *testing.T) {
	dshHome := t.TempDir()
	if err := SeedDefaultWorkspace(nil, dshHome, "/srv/definitely/not/here"); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dshHome, workspaceStorageRel)); !os.IsNotExist(err) {
		t.Error("目录不存在时不该写出注册表")
	}
}
