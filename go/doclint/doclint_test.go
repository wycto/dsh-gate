// Package doclint 是文档的结构性检查。
//
// 它只做机器能替人做的那部分核对，而且每一条都对应一个真实踩过的坑：
//
//   - **中文 README 必须真的是中文的**：有一次用脚本批量改文档时，
//     `open(path,"w").write(...)` 把整份 README.md 写成了英文版，而当时只抽查了
//     自己改过的那几节，没发现主文档已经被换掉。人眼看一遍 800 行文档很难发现
//     这种「整篇换语言」的事，机器一秒就能发现。
//   - **目录里的锚点必须都能命中标题**：README 很长，章节改名后目录不跟着改，
//     点进去就是空白页，而本地预览看不出差别。
//   - **代码围栏必须成对**：少一个反引号会把后面整篇文档渲染成代码块。
package doclint

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

const (
	readmeZH = "README.md"
	readmeEN = "README.en.md"
)

// read 读仓库根目录下的文件。
func read(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(raw)
}

// TestChineseReadmeIsActuallyChinese 防止中文 README 被整篇换成英文。
func TestChineseReadmeIsActuallyChinese(t *testing.T) {
	text := read(t, readmeZH)

	chineseLines := 0
	for _, line := range strings.Split(text, "\n") {
		if countCJK(line) >= 4 {
			chineseLines++
		}
	}
	if chineseLines < 250 {
		t.Errorf("README.md 里成句的中文只有 %d 行，远少于预期——它是不是被换成了别的语言？", chineseLines)
	}
}

// TestNoEnglishProseInChineseReadme 逐段检查有没有大段英文漏在中文文档里。
//
// 判据刻意保守：代码块、表格、行内代码、链接、标题一律放行，只盯「散文」。
func TestNoEnglishProseInChineseReadme(t *testing.T) {
	for _, line := range proseLines(read(t, readmeZH)) {
		t.Errorf("README.md 第 %d 行是大段英文，中文文档里不该出现：%s", line.number, truncate(line.text, 90))
	}
}

// proseLines 去掉代码块与结构行后，仍像英文散文的行。
type proseLine struct {
	number int
	text   string
}

// proseLines 去掉代码块与结构行后，仍像英文散文的行。
func proseLines(text string) []proseLine {
	var out []proseLine

	inFence := false
	for i, raw := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "|") ||
			strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "<!--") {
			continue
		}
		// 去掉行内代码、链接与裸 URL，剩下的才是真正的散文。
		prose := trimmed
		prose = replaceAll(prose, "`[^`]*`", "")
		prose = replaceAll(prose, `\[([^\]]*)\]\([^)]*\)`, "$1")
		prose = replaceAll(prose, `https?://\S+`, "")

		letters, cjk := 0, 0
		for _, r := range prose {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
				letters++
			case countCJK(string(r)) > 0:
				cjk++
			}
		}
		// 判据是「整句没有一个汉字」而不是按比例：中文句子里夹一串产品名
		// （systemd、macOS、launchd…）是正常的，按比例算会误报；而真正被换掉的
		// 英文段落里一个汉字都没有。25 个字母是「这是一句话」与「这是个标签」
		// 之间的分界。
		if letters >= 25 && cjk == 0 {
			out = append(out, proseLine{i + 1, trimmed})
		}
	}
	return out
}

// TestCodeFencesBalanced 检查代码围栏是否成对。
func TestCodeFencesBalanced(t *testing.T) {
	for _, name := range []string{readmeZH, readmeEN} {
		fences := 0
		for _, line := range strings.Split(read(t, name), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				fences++
			}
		}
		if fences%2 != 0 {
			t.Errorf("%s 里有 %d 个代码围栏（奇数），说明少了一个 ```", name, fences)
		}
	}
}

// TestTableAnchorsResolve 检查目录里的锚点都能命中标题。
func TestTableAnchorsResolve(t *testing.T) {
	for _, name := range []string{readmeZH, readmeEN} {
		text := read(t, name)
		headings := map[string]bool{}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "#") {
				continue
			}
			title := strings.TrimLeft(line, "#")
			headings[anchor(title)] = true
		}

		for _, line := range strings.Split(text, "\n") {
			m := regexp.MustCompile(`\[([^\]]+)\]\(#([^)]+)\)`).FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if !headings[m[2]] {
				t.Errorf("%s 里的目录条目 [%s](#%s) 找不到对应标题", name, m[1], m[2])
			}
		}
	}
}

// anchor 按 GitHub 的规则把标题转成锚点。
func anchor(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	var b strings.Builder
	for _, r := range title {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			b.WriteRune(r)
			// 其它标点被 GitHub 直接丢掉
		}
	}
	return strings.Trim(b.String(), "-")
}

// replaceAll 是一次正则替换的薄封装，让上面的散文清洗读起来更直白。
func replaceAll(text, pattern, repl string) string {
	return regexp.MustCompile(pattern).ReplaceAllString(text, repl)
}

// countCJK 数一行里有多少个中日韩统一表意文字。
func countCJK(s string) int {
	count := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			count++
		}
	}
	return count
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
