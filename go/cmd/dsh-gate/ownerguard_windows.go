//go:build windows

package main

// dirOwnerUID 在 Windows 上没有对应概念：NTFS 的 ACL 与 POSIX 的 uid 不是一回事，
// 也没有 sudo 会让一个用户以另一个人身份写文件。返回 ok=false，调用方放行。
func dirOwnerUID(string) (int, bool) { return 0, false }
