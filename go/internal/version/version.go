// Package version 是全项目唯一的版本号真源。
//
// 不放仓库根目录的 VERSION 文件，是因为 Go 的 go:embed 只能读取包目录
// 以内的文件：版本文件放在包里才能被二进制内嵌，避免「文件与常量两处
// 真源、发布时忘记同步」的老问题。发版时用
//
//	go build -ldflags "-X github.com/wycto/dsh-gate/internal/version.Version=1.2.3"
//
// 覆盖即可（不覆盖就用这里的默认值）。
package version

// Version 是 dsh-gate 的版本号，默认值即发版时的版本号。
var Version = "0.4.1"
