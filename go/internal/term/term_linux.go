//go:build linux

package term

import (
	"syscall"
	"unsafe"
)

// disableEcho 关闭终端回显，返回一个恢复函数。
//
// 用 ioctl 读写 termios：关掉 ECHO 位，再把原样设置回去。这是不引入
// 任何依赖就能在 Linux 上做到「输入口令不显示」的标准做法。
func disableEcho(fd uintptr) (func(), error) {
	var original syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &original); err != nil {
		return nil, err
	}
	modified := original
	modified.Lflag &^= syscall.ECHO
	modified.Lflag |= syscall.ICANON | syscall.ISIG
	if err := ioctlTermios(fd, syscall.TCSETS, &modified); err != nil {
		return nil, err
	}
	return func() {
		// 恢复失败时没什么补救手段，也不值得为此 panic：终端会在用户按下
		// 回车后由 shell 重置，这里尽力恢复即可。
		_ = ioctlTermios(fd, syscall.TCSETS, &original)
	}, nil
}

// ioctlTermios 对终端 fd 执行一次 termios ioctl。
func ioctlTermios(fd uintptr, request uintptr, value *syscall.Termios) error {
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		fd,
		request,
		uintptr(unsafe.Pointer(value)),
		0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// enableRaw 关闭行缓冲与回显，进入「逐字节可读」的状态，返回恢复函数。
//
// 与 disableEcho 的区别在于它同时关掉 ICANON：口令要逐个字符回显成 *，就必须
// 每按一个键立刻拿到那个字节，而不是等用户敲回车。VMIN=1/VTIME=0 表示「至少
// 有一个字节可读就立刻返回」。
func enableRaw(fd uintptr) (func(), error) {
	var original syscall.Termios
	if err := ioctlTermios(fd, syscall.TCGETS, &original); err != nil {
		return nil, err
	}
	modified := original
	modified.Lflag &^= syscall.ECHO | syscall.ICANON
	modified.Lflag |= syscall.ISIG
	modified.Cc[syscall.VMIN] = 1
	modified.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(fd, syscall.TCSETS, &modified); err != nil {
		return nil, err
	}
	return func() {
		_ = ioctlTermios(fd, syscall.TCSETS, &original)
	}, nil
}
