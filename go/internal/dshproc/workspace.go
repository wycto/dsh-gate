// 在实例的 DSH_HOME 里预置一个默认工作区。
//
// 为什么需要它：dsh 的工作区是**用户自己挑**的，注册表一开始是空的。于是每个实例
// 第一次进去都停在「选择一个工作区开始」，用户得手动挑一次才能开始对话——网关
// 明明已经在实例登记里配好了工作目录，却要人再挑一遍。
//
// 这里在拉起 dsh 之前把那个目录写进 dsh 的工作区注册表并设为 default。
//
// 两条纪律：
//
//   - **只在空注册表上写**。用户挑过工作区之后（workspaceIds 非空），dsh 自己会
//     维护这份状态，网关再插手就会覆盖用户的选择。
//   - **只认 version 2 的格式**。dsh 换了存储版本就原样放着不动——宁可退回「手动挑
//     一次」，也不要把人家的注册表写坏。
package dshproc

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// workspaceStorageRel 是工作区注册表相对 DSH_HOME 的位置。
//
// 这不是 dsh 的公开接口，是它的内部存储文件。所以下面每一步都做了格式校验，
// 认不出来就退出，不做「尽力猜」。
var workspaceStorageRel = filepath.Join("storages", "workspace.json")

// workspaceStorageVersion 是认识的存储版本（dsh 的 workspace domain spec 里写的 2）。
const workspaceStorageVersion = 2

// SeedDefaultWorkspace 在 dshHome 下预置一个指向 workspace 的默认工作区。
//
// 已经有工作区、格式不认识、或路径为空时什么都不做（返回 nil）——这是「补齐」，
// 不是「重置」。真出错时返回 err，由调用方决定是否记日志继续（拉 dsh 不该因为
// 预置工作区失败而失败）。
func SeedDefaultWorkspace(log *slog.Logger, dshHome, workspace string) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dshHome = strings.TrimSpace(dshHome)
	workspace = strings.TrimSpace(workspace)
	if dshHome == "" || workspace == "" {
		return nil
	}
	// 工作区目录本身由 Supervisor 负责建；这里只要它存在，否则 dsh 打开会失败。
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		return nil
	}

	path := filepath.Join(dshHome, workspaceStorageRel)
	state, ok := readWorkspaceStorage(path)
	if !ok {
		// 格式不认识 / 读不动：不动它。
		return nil
	}
	if len(state.Global.WorkspaceIDs) > 0 || len(state.Tables.Workspaces) > 0 {
		// 已经有工作区（多半是用户自己挑的），不插手。
		return nil
	}
	// 里面已经登记过同一个路径时也不重复加。
	for _, rec := range state.Tables.Workspaces {
		if filepath.Clean(rec.Path) == filepath.Clean(workspace) {
			return nil
		}
	}

	id, err := newWorkspaceID()
	if err != nil {
		return fmt.Errorf("生成工作区 id 失败: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	state.Global.Initialized = true
	state.Global.DefaultWorkspaceID = id
	state.Global.WorkspaceIDs = []string{id}
	if state.Tables.Workspaces == nil {
		state.Tables.Workspaces = map[string]workspaceRecord{}
	}
	state.Tables.Workspaces[id] = workspaceRecord{
		Path:       workspace,
		Title:      workspaceTitle(workspace),
		SessionIDs: []string{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建工作区存储目录失败: %w", err)
	}
	if err := writeJSONAtomic(path, state); err != nil {
		return err
	}
	log.Info("已为实例预置默认工作区", "workspace", workspace, "file", path)
	return nil
}

// workspaceTitle 取目录名当标题，与 dsh 自己的 defaultWorkspaceTitle 一致
// （根目录就用 "/"）。
func workspaceTitle(path string) string {
	cleaned := filepath.Clean(path)
	base := filepath.Base(cleaned)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return cleaned
	}
	return base
}

// WorkspaceStorageUnit 是存储文件的外壳（unit/global/tables）。
type WorkspaceStorageUnit struct {
	Unit struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	} `json:"unit"`
	Global workspaceGlobal `json:"global"`
	Tables workspaceTables `json:"tables"`
}

type workspaceGlobal struct {
	Initialized        bool     `json:"initialized"`
	DefaultWorkspaceID string   `json:"defaultWorkspaceId,omitempty"`
	WorkspaceIDs       []string `json:"workspaceIds"`
	ArchivedSessionIDs []string `json:"archivedSessionIds"`
	PinnedSessionIDs   []string `json:"pinnedSessionIds"`
}

type workspaceTables struct {
	Workspaces map[string]workspaceRecord `json:"workspaces"`
}

type workspaceRecord struct {
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	SessionIDs []string `json:"sessionIds"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}

// readWorkspaceStorage 读注册表；文件不存在时返回一份干净的骨架。
//
// 第二个返回值为 false 表示「这份文件我们不认识」，调用方必须原样放着不动。
func readWorkspaceStorage(path string) (*WorkspaceStorageUnit, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			state := &WorkspaceStorageUnit{}
			state.Unit.Name = "workspace"
			state.Unit.Version = workspaceStorageVersion
			state.Global.WorkspaceIDs = []string{}
			state.Global.ArchivedSessionIDs = []string{}
			state.Global.PinnedSessionIDs = []string{}
			state.Tables.Workspaces = map[string]workspaceRecord{}
			return state, true
		}
		return nil, false
	}
	var state WorkspaceStorageUnit
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, false
	}
	if state.Unit.Name != "workspace" || state.Unit.Version != workspaceStorageVersion {
		return nil, false
	}
	if state.Tables.Workspaces == nil {
		state.Tables.Workspaces = map[string]workspaceRecord{}
	}
	return &state, true
}

// writeJSONAtomic 先写临时文件再改名：这份文件是 dsh 启动时要读的注册表，
// 写坏一半等于让实例起不来。
func writeJSONAtomic(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化工作区注册表失败: %w", err)
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写工作区注册表失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("提交工作区注册表失败: %w", err)
	}
	return nil
}

// newWorkspaceID 生成工作区 id。
//
// dsh 用 randomUUID() 并且明确「不是路径」——同一个目录可以注册成多个工作区，
// 所以这里也必须每注册一次换一个新 id，不能拿路径算哈希。
func newWorkspaceID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10
	hexs := make([]byte, 36)
	const digits = "0123456789abcdef"
	pos := 0
	for i, b := range buf {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			hexs[pos] = '-'
			pos++
		}
		hexs[pos] = digits[b>>4]
		hexs[pos+1] = digits[b&0x0f]
		pos += 2
	}
	return string(hexs[:pos]), nil
}
