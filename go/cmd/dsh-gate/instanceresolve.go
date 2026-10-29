package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/wycto/dsh-gate/internal/portal"
)

// instanceHomeFromFlag 解析 `--instance`：从管理端数据库取该实例的**实际 DSH_HOME**
// （配置目录填了就以它为准，与运行时 homeOf 同一口径）。
//
// 模型与插件都是「某个 dsh 实例的配置」，默认目录 ~/.dsh 在管理端形态下几乎没有
// 意义：每个实例都有自己的 DSH_HOME。因此这里不给默认值，让命令必须指明实例。
func instanceHomeFromFlag(ref, stateDirFlag, dbFlag string) (home, name string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", errors.New("请用 --instance 指定 dsh 实例（dsh-gate dsh list 查看已有实例）")
	}
	stateDir, err := resolveStateDir(stateDirFlag)
	if err != nil {
		return "", "", err
	}
	dbPath := strings.TrimSpace(dbFlag)
	if dbPath == "" {
		dbPath = filepath.Join(stateDir, "portal.db")
	}
	store, err := portal.Open(dbPath)
	if err != nil {
		return "", "", fmt.Errorf("打开管理端数据库失败（%s）：%v；先 dsh-gate install", dbPath, err)
	}
	defer store.Close()

	inst, err := resolveInstance(store, ref)
	if err != nil {
		return "", "", fmt.Errorf("找不到 dsh 实例 %q（dsh-gate dsh list 查看）", ref)
	}
	home = strings.TrimSpace(inst.ConfigDir)
	if home == "" {
		home = strings.TrimSpace(inst.DshHome)
	}
	if home == "" {
		return "", inst.Name, fmt.Errorf("实例 %q 没有配置 DSH_HOME", inst.Name)
	}
	return home, inst.Name, nil
}
