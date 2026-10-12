// Package term 提供「按行读取终端输入」的能力，含不回显的口令读取。
//
// 用标准库实现而不是 golang.org/x/term：本项目坚持零第三方依赖，而这一个
// 功能只需要在三个平台各写十来行系统调用，比引入一整棵依赖划算得多。
//
// 所有读取都逐字节进行、不做任何缓冲预读。这不是性能洁癖：同一进程里会有
// 多次连续读行（口令输两遍、向导连问数题），bufio 的预读会把后面的行扣在
// 自己的缓冲里，下一次读取（尤其是新建缓冲区的那个）就只能等到 EOF——
// 用户把几行答案一起粘贴进终端时必现。
//
// 取不到终端（管道、systemd、CI）时自动退化为 ErrNotTerminal，由调用方决定
// 是改用管道参数还是直接报错——那种场景下本来也没有终端可以回显。
package term

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ErrNotTerminal 表示输入不是交互式终端，无法安全地读取口令。
var ErrInterrupted = errors.New("已取消输入")

var ErrNotTerminal = errors.New("输入不是终端，无法安全地读取口令")

// IsTerminal 判断文件描述符是否连着交互式终端。
func IsTerminal(fd uintptr) bool {
	restore, err := disableEcho(fd)
	if err != nil {
		return false
	}
	restore()
	return true
}

// ReadLine 显示提示并读取一行（回显）。返回的行不含行尾换行，也未做其它修剪。
func ReadLine(in io.Reader, out io.Writer, prompt string) (string, error) {
	file, ok := in.(*os.File)
	if !ok {
		return "", ErrNotTerminal
	}
	fmt.Fprint(out, prompt)
	line, err := readLineUnbuffered(file)
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

// ReadPassword 显示提示并读取一行口令，不回显。
//
// 返回的字节已去掉行尾换行。
func ReadPassword(in io.Reader, out io.Writer, prompt string) ([]byte, error) {
	file, ok := in.(*os.File)
	if !ok {
		return nil, ErrNotTerminal
	}
	restore, err := disableEcho(file.Fd())
	if err != nil {
		return nil, ErrNotTerminal
	}
	defer restore()

	fmt.Fprint(out, prompt)
	line, err := readLineUnbuffered(file)
	// 无论读成功还是出错都要换行，否则终端会停在提示语后面。
	fmt.Fprintln(out)
	if err != nil && line == "" {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r")), nil
}

// ReadPasswordMasked 显示提示并读取一行口令，每个字符回显成 *。
//
// 为什么不用 ReadPassword（完全不回显）：用户在终端里看不到任何反馈，很容易
// 以为程序没在等输入、或者以为自己没敲进去（实测反馈就是「输入了但屏幕上什么
// 都没有」）。星号既是反馈，也不泄露内容长度以外的信息。
//
// 退格可以改错，Ctrl-C 视作放弃（原始模式下 ISIG 被关掉，得自己认这个字节）。
// 拿不到原始模式时返回 ErrNotTerminal，调用方应退回 ReadPassword。
func ReadPasswordMasked(in io.Reader, out io.Writer, prompt string) ([]byte, error) {
	file, ok := in.(*os.File)
	if !ok {
		return nil, ErrNotTerminal
	}
	restore, err := enableRaw(file.Fd())
	if err != nil {
		return nil, ErrNotTerminal
	}
	defer restore()

	fmt.Fprint(out, prompt)
	value, cancelled, err := feedMaskedFrom(file, out)
	if err != nil {
		return nil, err
	}
	if cancelled {
		return nil, ErrInterrupted
	}
	return value, nil
}

// feedMaskedFrom 逐字节读直到回车，同时把每个字符回显成 *。
func feedMaskedFrom(file *os.File, out io.Writer) ([]byte, bool, error) {
	var line []byte
	one := make([]byte, 1)
	for {
		n, readErr := file.Read(one)
		if n > 0 {
			value, cancelled, done, echo := maskedStep(line, one[0])
			line = value
			fmt.Fprint(out, echo)
			if done {
				return line, false, nil
			}
			if cancelled {
				return nil, true, nil
			}
		}
		if readErr != nil {
			if len(line) == 0 {
				return nil, false, readErr
			}
			return line, false, nil
		}
	}
}

// feedMasked 是 feedMaskedFrom 的纯函数版本：从字节切片喂进去，便于测试。
func feedMasked(input []byte, out io.Writer) ([]byte, bool, error) {
	var line []byte
	for _, b := range input {
		value, cancelled, done, echo := maskedStep(line, b)
		line = value
		fmt.Fprint(out, echo)
		if done {
			return line, false, nil
		}
		if cancelled {
			return nil, true, nil
		}
	}
	return line, false, nil
}

// maskedStep 处理一个输入字节，返回新的内容、是否取消、是否结束，以及该回显什么。
//
// 抽成纯函数是为了能测：这套「星号 + 退格 + 回车」的规则是本功能的全部行为，
// 而终端在测试里拿不到。
func maskedStep(line []byte, b byte) (value []byte, cancelled, done bool, echo string) {
	switch b {
	case '\n', '\r':
		return line, false, true, "\n"
	case 0x7f, 0x08: // 退格 / Delete
		if len(line) == 0 {
			return line, false, false, ""
		}
		return line[:len(line)-1], false, false, "\b \b"
	case 0x03: // Ctrl-C
		return nil, true, false, "\n"
	default:
		return append(line, b), false, false, "*"
	}
}

// readLineUnbuffered 逐字节读到换行为止。
//
// 每次 Read 只向内核要一个字节，读过的内容绝不会滞留在用户态缓冲里——这是
// 本包所有读取共享的关键约束，见包注释。
func readLineUnbuffered(file *os.File) (string, error) {
	var line []byte
	one := make([]byte, 1)
	for {
		n, err := file.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return string(line), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if len(line) == 0 {
				return "", err
			}
			return string(line), nil
		}
	}
}
