//go:build windows

package dshproc

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// findPID 用 iphlpapi 查出监听该端口的进程号。
//
// 为什么要专门给 Windows 写一份：接管与停止都要知道「端口上那个进程是谁」——
// 只有 Linux 有实现意味着 Windows 上接管来的实例永远是个未知进程，网关说
// 「已停止」却谁也没被停掉。这里不借道 netstat.exe：它是外部命令，输出要按
// 控制台代码页解码，而且每次探测都要 fork 一个进程；GetExtendedTcpTable 一次
// 调用就把整张表读回来了。
//
// 查不到返回 0，调用方按「不知道」处理。
func findPID(port int) int {
	for _, family := range []uint32{afInet, afInet6} {
		if pid := findPIDInTable(port, family); pid > 0 {
			return pid
		}
	}
	return 0
}

// Windows 的地址族常量：iphlpapi 用的是 AF_INET(2) / AF_INET6(23) 这一组，
// 不是 winsock.h 里的 AF_INET6(28)——传错只会得到一个空表，不会报错。
const (
	afInet  = 2
	afInet6 = 23

	// TCP_TABLE_OWNER_PID_LISTENER：只要 LISTEN 状态的行，表最小也最快。
	tcpTableOwnerPIDListener = 3
	// errorInsufficientBuffer：第一次调用（size=0）必然返回它，用来问要多大缓冲。
	errorInsufficientBuffer = 122

	// 两种行的字节布局（都是 DWORD 对齐，无填充）：
	//   MIB_TCPROW_OWNER_PID  = 状态 + 本地地址 + 本地端口 + 远端地址 + 远端端口 + PID = 24
	//   MIB_TCP6ROW_OWNER_PID = 本地地址[16] + 作用域 + 本地端口 + 远端地址[16] + 作用域 + 远端端口 + 状态 + PID = 56
	ipv4RowSize = 24
	ipv6RowSize = 56
)

var (
	iphlpapi                = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

// findPIDInTable 在指定地址族的监听表里找该端口。
func findPIDInTable(port int, family uint32) int {
	// 表大小随连接数变化，两次调用之间可能又长了：按返回的需要量重试，
	// 而不是赌第一次问到的尺寸够用。
	size := uint32(0)
	for attempt := 0; attempt < 3; attempt++ {
		buf := make([]byte, size)
		var call uintptr
		if size > 0 {
			call = uintptr(unsafe.Pointer(&buf[0]))
		} else {
			call = 0 // size=0 时只为问大小，不给缓冲区
		}
		ret, _, _ := procGetExtendedTcpTable.Call(
			call,
			uintptr(unsafe.Pointer(&size)),
			0, // bOrder=FALSE：不必排序，找的是唯一的那一行
			uintptr(family),
			tcpTableOwnerPIDListener,
			0,
		)
		if ret == errorInsufficientBuffer {
			continue
		}
		// 只认返回值：LazyProc 在成功返回 0 时也可能留着上一次的 lastErr。
		if ret != 0 || size < 4 {
			return 0
		}
		if pid := scanRows(buf[:size], port, family); pid > 0 {
			return pid
		}
		return 0
	}
	return 0
}

// scanRows 遍历监听表，返回监听该端口的进程号。
func scanRows(buf []byte, port int, family uint32) int {
	// buf[0:4] 是 dwNumEntries；先按它算出需要的字节数，短了就是缓冲区不够。
	count := int(binary.LittleEndian.Uint32(buf[:4]))
	rowSize, portOffset, pidOffset := ipv4RowSize, 8, 20
	if family == afInet6 {
		rowSize, portOffset, pidOffset = ipv6RowSize, 20, 52
	}
	if len(buf) < 4+count*rowSize {
		return 0
	}
	for i := 0; i < count; i++ {
		row := buf[4+i*rowSize:]
		// 端口字段是网络字节序的两字节，紧跟两个补零字节；整块按大端取前
		// 两字节即端口号。（本机实测：80、445、3389 与 netstat 完全对得上。）
		if int(binary.BigEndian.Uint16(row[portOffset:portOffset+2])) != port {
			continue
		}
		if pid := int(binary.LittleEndian.Uint32(row[pidOffset:])); pid > 0 {
			return pid
		}
	}
	return 0
}
