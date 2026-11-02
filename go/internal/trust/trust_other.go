//go:build !linux && !darwin && !windows

package trust

// install 在未适配平台上不做任何改动，只报告「需要手动安装」。
//
// 与其猜一个可能改坏系统状态的命令，不如明确告诉用户「这里没有自动支持，
// 请照命令手动做」——这些平台的用户本来就少，而系统信任库的差异很大。
func install(opts Options) Status {
	return Status{
		Attempted: true,
		Message:   "当前平台没有自动安装信任的支持，请手动把 CA 证书加入系统信任库。",
	}
}
