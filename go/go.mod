// 本项目的依赖策略：**只用 Go 标准库，只有一处例外**。
//
// 理由：dsh-gate 会被装到别人的服务器、家用电脑和 Windows 工作机上，
// 依赖树越干净，「go build 就能跑」这个承诺就越可靠，也省掉供应链与
// 交叉编译（systray 之类）带来的麻烦。代价是密码哈希、YAML 解析这类
// 常用库要自己挑标准库替代品 —— 目前 stdlib 的 crypto/pbkdf2 已经够用。
//
// 唯一例外：门户（见 docs/portal.md）的持久化用 modernc.org/sqlite。
// 门户要存用户、实例、授权与审计，还要按条件查询、并发读写，文件存储
// 不再合适。选**纯 Go、无 CGO** 的实现是为了守住「六个平台交叉编译、
// go build 就能跑」这条底线（CGO 版会把它推翻）。除它之外不引入任何
// 第三方库；新增依赖前请先想清楚能不能用标准库解决。
module github.com/wycto/dsh-gate

go 1.24.0

require modernc.org/sqlite v1.45.0

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sys v0.37.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
