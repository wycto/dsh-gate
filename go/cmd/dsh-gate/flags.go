package main

import (
	"flag"
	"strings"
)

// stringList 是一个可以重复出现的字符串选项。
//
// 用来实现 `--trusted-host a --trusted-host b`：flag 包的 StringVar 只会保留
// 最后一次出现的值，而 dsh 的 --trusted-host 恰恰需要多个值，所以这里自己实现
// 一个 flag.Value。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func parseFlexible(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(arg) > 1 && strings.HasPrefix(arg, "-") {
			options = append(options, arg)
			name := strings.TrimLeft(arg, "-")
			if strings.Contains(name, "=") {
				continue // 值已经内联
			}
			// 取值型选项后面紧跟的那个词属于它，得一起搬过去；布尔选项没有值。
			flagDef := fs.Lookup(name)
			if flagDef == nil {
				continue // 交给 flag 包报「未知选项」
			}
			if boolFlag, ok := flagDef.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
				continue
			}
			if i+1 < len(args) {
				i++
				options = append(options, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return fs.Parse(append(append(options, "--"), positional...))
}
