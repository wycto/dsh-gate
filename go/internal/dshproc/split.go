package dshproc

import (
	"errors"
	"strings"
)

// SplitCommand 把命令行拆成参数数组。
//
// 为什么要自己拆而不是交给 shell：dsh-gate 从不经过 shell 启动子进程
// （exec.Command 直接执行），用户给的模板因此**不会被 shell 展开**——没有
// 变量替换、没有重定向、也没有注入面。代价是必须自己处理引号，而这一步
// 只做一次，写清楚比留个安全口子划算。
//
// 规则（与 POSIX shell 的引号语义一致的部分）：
//
//	单引号  '...'  内部全部字面量，'' 表示一个单引号
//	双引号  "..."  内部 \ " 可转义
//	反斜杠  \x     转义下一个字符（引号外）
//
// 未闭合的引号返回错误，而不是静默吞掉——那几乎一定是用户写错了。
func SplitCommand(line string) ([]string, error) {
	var (
		args    []string
		current strings.Builder
		hasArg  bool
	)
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case ' ', '\t', '\n', '\r':
			if hasArg {
				args = append(args, current.String())
				current.Reset()
				hasArg = false
			}
		case '\'':
			hasArg = true
			closed := false
			for i++; i < len(runes); i++ {
				if runes[i] == '\'' {
					closed = true
					break
				}
				current.WriteRune(runes[i])
			}
			if !closed {
				return nil, errors.New("单引号没有闭合")
			}
		case '"':
			hasArg = true
			closed := false
			for i++; i < len(runes); i++ {
				if runes[i] == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\') {
					i++
					current.WriteRune(runes[i])
					continue
				}
				if runes[i] == '"' {
					closed = true
					break
				}
				current.WriteRune(runes[i])
			}
			if !closed {
				return nil, errors.New("双引号没有闭合")
			}
		case '\\':
			if i+1 >= len(runes) {
				return nil, errors.New("命令以孤立的反斜杠结尾")
			}
			i++
			current.WriteRune(runes[i])
			hasArg = true
		default:
			current.WriteRune(c)
			hasArg = true
		}
	}
	if hasArg {
		args = append(args, current.String())
	}
	return args, nil
}
