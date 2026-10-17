// CLI 的输出语言与文案查表。
//
// 语言优先级与页面一致（显式 --lang > DSH_GATE_LANG > LC_ALL > LANG > 默认中文）：
// 同一台服务器上，浏览器与命令行的语言不该由两套规则决定。
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wycto/dsh-gate/internal/i18n"
)

// cliLang 是本次调用的输出语言，由 main 处理全局 --lang 后设置。
var cliLang = i18n.Default

// setCLILang 依据显式 --lang 与环境变量决定输出语言。
func setCLILang(explicit string) { cliLang = i18n.Detect(explicit, os.Getenv) }

// tr 查一条 CLI 文案。
func tr(key string, args ...any) string { return i18n.T(cliLang, key, args...) }

// sayKey 打印一条文案（到 stdout）。
func sayKey(w io.Writer, key string, args ...any) { fmt.Fprintln(w, tr(key, args...)) }

// failKey 打印一条（已翻译的）错误并返回退出码 1。
func failKey(key string, args ...any) int {
	fmt.Fprintf(os.Stderr, "dsh-gate: %s\n", tr(key, args...))
	return 1
}

// usageKey 打印一条（已翻译的）用法错误并返回退出码 2。
func usageKey(key string, args ...any) int {
	fmt.Fprintf(os.Stderr, "dsh-gate: %s\n", tr(key, args...))
	return 2
}

// extractLang 从 argv 里摘出全局 --lang（`--lang en` 或 `--lang=en`），其余原样返回。
//
// 摘掉而不是留给子命令处理：子命令用 flag 包解析参数，遇到不认识的选项会直接
// 报「flag provided but not defined」，那对用户来说是个很差的提示。
func extractLang(argv []string) (string, []string) {
	lang := ""
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		switch arg := argv[i]; {
		case arg == "--lang" && i+1 < len(argv):
			lang = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--lang="):
			lang = strings.TrimPrefix(arg, "--lang=")
		default:
			out = append(out, arg)
		}
	}
	return lang, out
}
