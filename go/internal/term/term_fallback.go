//go:build !linux && !darwin && !windows

package term

import "errors"

// disableEcho 在未适配的平台上直接失败。
//
// 于是 ReadPassword 返回 ErrNotTerminal，调用方会提示改用
// `dsh-gate user add --password-stdin` 这样的管道方式——这比「悄悄把口令
// 明文显示在屏幕上」要好得多。
func disableEcho(fd uintptr) (func(), error) {
	return nil, errors.New("该平台尚未实现终端回显控制")
}

// enableRaw 在未适配的平台上直接失败，调用方会退回「不回显」的方式。
func enableRaw(fd uintptr) (func(), error) {
	return nil, errors.New("该平台尚未实现终端原始模式")
}
