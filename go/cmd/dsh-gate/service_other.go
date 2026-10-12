//go:build !linux && !darwin && !windows

package main

import "errors"

// 未适配的平台：与其给一个跑不通的 systemd 单元，不如直说没有。
//
// 适配一个新平台要写的就这五个函数，看 Linux / macOS / Windows 任意一份即可。

func serviceFlavor() string       { return "尚未适配" }
func platformServiceHelp() string { return "\n当前平台尚未提供开机自启支持。\n" }

func serviceUnit(serviceOptions) (string, error) {
	return "", errors.New("当前平台尚未适配开机自启")
}

func serviceInstall(serviceOptions) error {
	return errors.New("当前平台尚未适配开机自启")
}

func serviceUninstall(serviceOptions) error {
	return errors.New("当前平台尚未适配开机自启")
}

func serviceStatus() int {
	fmt.Println("当前平台尚未适配开机自启。")
	return 1
}
