// 本项目刻意保持**零第三方依赖**：只用 Go 标准库。
//
// 理由：dsh-gate 会被装到别人的服务器、家用电脑和 Windows 工作机上，
// 依赖树越干净，「go build 就能跑」这个承诺就越可靠，也省掉供应链与
// 交叉编译（systray 之类）带来的麻烦。代价是密码哈希、YAML 解析这类
// 常用库要自己挑标准库替代品 —— 目前 stdlib 的 crypto/pbkdf2 已经够用。
module github.com/wycto/dsh-gate

go 1.24