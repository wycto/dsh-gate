//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// dirOwnerUID 返回路径（或它最近的已存在祖先）的属主 uid。
//
// 路径还不存在时向上找，是为了认出「有人要在别人的家目录里新建一个目录」——
// 那正是 `sudo dsh-gate setup` 第一次运行时的样子。
//
// 找不到（连 / 都 stat 不了，或平台不支持）时返回 ok=false，调用方放行：
// 宁可漏拦，也不要在说不清的时候拦住正常使用。
func dirOwnerUID(path string) (int, bool) {
	p := filepath.Clean(path)
	for {
		info, err := os.Stat(p)
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return 0, false
			}
			return int(stat.Uid), true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, false
		}
		p = parent
	}
}
