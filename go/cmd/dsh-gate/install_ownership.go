package main

// 安装收尾的「属主修正」：sudo 安装时，账号库、证书与私钥都是在 root 身份下
// 创建的，而服务单元会以真实用户身份运行——不 chown 的话，真实用户读不到私钥、
// 写不了数据库，服务一起来就报权限错误。
//
// 只 chown 安装期自建的路径（状态目录整棵 + 拷进 bin-dir 的二进制），**绝不**
// 递归 chown 用户整个家目录——那是一颗会误伤其他文件的雷。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// chownTree 递归把 root 下所有文件与目录（含符号链接本身）的属主改为 uid:gid，
// 返回失败的路径列表（为空表示全部成功）。
//
// 单个失败不中断：个别文件（例如正被别的进程占用）改不动，也要把其余的都改完，
// 再由调用方汇总告警——总比「第一个失败就整体放弃、剩下的全留 root」好。
func chownTree(root string, uid, gid int) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	var failed []string
	// WalkDir 会跟随错误：root 不存在等情况下回调收到 err，也当作失败路径记下。
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			failed = append(failed, path)
			return nil
		}
		// Lchown 不跟随符号链接：改的是链接本身，不会把目标文件的属主改掉。
		if chErr := os.Lchown(path, uid, gid); chErr != nil {
			failed = append(failed, path)
		}
		return nil
	})
	if err != nil {
		failed = append(failed, root)
	}
	return failed
}

// chownPath 改单个路径的属主；成功返回 true。
func chownPath(path string, uid, gid int) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return true
	}
	return os.Lchown(path, uid, gid) == nil
}

// parseID 把 user.User 的字符串 uid/gid 转成 chown 要的整数。空串或非法返回 false。
func parseID(raw string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}

// firstN 返回前 n 个元素（用于把过长的失败列表截断成一行告警）。
func firstN(items []string, n int) []string {
	if n <= 0 || len(items) <= n {
		return items
	}
	return items[:n]
}
