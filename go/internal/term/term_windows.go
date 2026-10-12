//go:build windows

package term

import (
	"syscall"
	"unsafe"
)

// 控制台模式里与「回显」相关的两个位。
const (
	enableEchoInput   = 0x0004
	enableLineInput   = 0x0002
	enableProcessedIO = 0x0001
)

// disableEcho 关闭控制台回显，返回一个恢复函数。
//
// Windows 上没有 termios，对应的做法是清掉 GetConsoleMode 返回的模式位里的
// ENABLE_ECHO_INPUT。kernel32 通过标准库的 syscall.NewLazyDLL 调用，不需要
// golang.org/x/sys。
func disableEcho(fd uintptr) (func(), error) {
	handle := syscall.Handle(fd)

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	var mode uint32
	if ret, _, _ := getConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode))); ret == 0 {
		return nil, syscall.EINVAL
	}
	original := mode
	modified := mode &^ enableEchoInput
	modified |= enableLineInput | enableProcessedIO
	if ret, _, _ := setConsoleMode.Call(uintptr(handle), uintptr(modified)); ret == 0 {
		return nil, syscall.EINVAL
	}
	return func() {
		_, _, _ = setConsoleMode.Call(uintptr(handle), uintptr(original))
	}, nil
}

// enableRaw 关掉行缓冲与回显：逐字符读取才有可能把输入回显成 *。
func enableRaw(fd uintptr) (func(), error) {
	handle := syscall.Handle(fd)

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	var mode uint32
	if ret, _, _ := getConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode))); ret == 0 {
		return nil, syscall.EINVAL
	}
	original := mode
	modified := mode &^ (enableEchoInput | enableLineInput)
	modified |= enableProcessedIO
	if ret, _, _ := setConsoleMode.Call(uintptr(handle), uintptr(modified)); ret == 0 {
		return nil, syscall.EINVAL
	}
	return func() {
		_, _, _ = setConsoleMode.Call(uintptr(handle), uintptr(original))
	}, nil
}
