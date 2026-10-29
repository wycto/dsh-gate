//go:build !windows

package main

import "fmt"

// 托盘是 Windows 独有的形态（Linux / macOS 的服务管理器本来就不给进程挂终端），
// `service install --tray` 在那两个平台上会先被参数检查挡掉。这个文件的存在只是
// 为了让共用代码里那句调用在任何平台都编译得过，并且**永远不该被执行**：
// 真被执行了说明前面的检查漏了，所以这里报错而不是返回一个假路径。
func installTraySiblingToBinDir(binDir, consoleBinary string) (string, error) {
	return "", fmt.Errorf("本平台没有托盘程序")
}
