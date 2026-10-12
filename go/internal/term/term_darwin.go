//go:build darwin

package term

import (
	"syscall"
	"unsafe"
)

// disableEcho 关闭终端回显，返回一个恢复函数。
//
// macOS/BSD 的 termios ioctl 请求码与 Linux 不同（TIOCGETA 而非 TCGETS），
// 所以按平台各写一份。除此之外逻辑完全一致。
func disableEcho(fd uintptr) (func(), error) {
	var original syscall.Termios
	if err := ioctlTermios(fd, syscall.TIOCGETA, &original); err != nil {
		return nil, err
	}
	modified := original
	modified.Lflag &^= syscall.ECHO
	modified.Lflag |= syscall.ICANON | syscall.ISIG
	if err := ioctlTermios(fd, syscall.TIOCSETA, &modified); err != nil {
		return nil, err
	}
	return func() {
		_ = ioctlTermios(fd, syscall.TIOCSETA, &original)
	}, nil
}

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
	if err := ioctlTermios(fd, syscall.TIOCGETA, &original); err != nil {
		return nil, err
	}
	modified := original
	modified.Lflag &^= syscall.ECHO | syscall.ICANON
	modified.Lflag |= syscall.ISIG
	modified.Cc[syscall.VMIN] = 1
	modified.Cc[syscall.VTIME] = 0
	if err := ioctlTermios(fd, syscall.TIOCSETA, &modified); err != nil {
		return nil, err
	}
	return func() {
		_ = ioctlTermios(fd, syscall.TIOCSETA, &original)
	}, nil
}
