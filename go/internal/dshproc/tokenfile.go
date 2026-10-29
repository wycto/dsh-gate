package dshproc

import (
	"os"
	"path/filepath"
	"strings"
)

// 启动令牌的落盘与读回。
//
// 为什么要把令牌写进文件：dsh 每个进程随机生成一个令牌、只在启动时打印一次。
// 网关一旦重启（崩溃、升级、重装服务），而 dsh 还活着，网关就只能「接管」它却
// 不知道令牌——浏览器能打开首页，一对话就报未授权，用户看到的是「过一会儿就
// 访问不到了」。令牌放在实例自己的目录里（0600），重启后的网关读回来就能续上
// 原来的会话，不必杀掉还活着的 dsh。
//
// 这与「令牌不落结构化日志」的约定不冲突：日志是给人看的、会被转发出去；这个
// 文件是 0600、只在实例目录里、和 portal.db 里的口令哈希同一个信任边界。
// 读取一律当作「尽力而为」：文件缺失、内容为空、权限被改坏都不拦启动。

// readTokenFile 读回落盘的启动令牌；读不到就返回空串。
func readTokenFile(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// writeTokenFile 把启动令牌写到 path（0600，先写临时文件再改名）。
//
// 先写临时文件再改名：直接覆盖时如果进程恰好被杀，留下的半截文件会让下一次
// 启动读到一个残缺令牌，比没有更糟（有令牌就不会再去日志里找）。
func writeTokenFile(path, token string) error {
	if strings.TrimSpace(path) == "" || token == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".dsh-token-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}
