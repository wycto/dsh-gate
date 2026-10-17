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

// imageRefPattern 抓 Markdown 图片与 HTML <img src> 两种引用。
// 两种都要抓：README 里同一张图可能用 Markdown 语法（配 alt）也可能用
// <img width=...> 控制尺寸，漏掉任一种就会出现裂图而没人发现。
var imageRefPattern = regexp.MustCompile(`(?:!\[[^\]]*\]\(([^)\s]+)\)|<img[^>]+src="([^"]+)")`)

// TestReadmeImageRefsResolve 检查文档里引用的每张图都真实存在。
//
// 图片是相对路径，改了文件名或挪了目录之后 Markdown 不会报错，只有人打开页面
// 才看得到裂图；而 README 又会随发行包一起分发，裂图会直接发给用户。
func TestReadmeImageRefsResolve(t *testing.T) {
	for _, doc := range []string{readmeZH, readmeEN} {
		text := read(t, doc)
		found := 0
		for _, m := range imageRefPattern.FindAllStringSubmatch(text, -1) {
			ref := m[1]
			if ref == "" {
				ref = m[2]
			}
			// 只检查仓库内的相对路径；外链图（http/https）不归这里管。
			if strings.Contains(ref, "://") {
				continue
			}
			found++
			path := filepath.Join("..", "..", ref)
			info, err := os.Stat(path)
			if err != nil {
				t.Errorf("%s 引用了不存在的图片 %q: %v", doc, ref, err)
				continue
			}
			if info.IsDir() {
				t.Errorf("%s 的图片引用 %q 指向目录而不是文件", doc, ref)
			}
			if info.Size() == 0 {
				t.Errorf("%s 的图片 %q 是空文件", doc, ref)
			}
		}
		if found == 0 {
			t.Errorf("%s 里没找到任何图片引用，检查正则是否失效", doc)
		}
	}
}

// TestPackagedDocsIncludeImages 检查打包脚本会把文档引用的图一起放进发行包。
//
// README 随包分发，图却留在仓库里的话，用户解压出来看到的全是裂图。
// 这里断言 build.sh 里有拷贝 docs/images 的那一步——脚本是 shell，没法直接
// 调它的函数，就核对它确实写了这件事。
func TestPackagedDocsIncludeImages(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "build.sh"))
	if err != nil {
		t.Fatalf("读取 go/build.sh 失败: %v", err)
	}
	script := string(raw)

	for _, want := range []string{"docs/images", "cp docs/images/"} {
		if !strings.Contains(script, want) {
			t.Errorf("build.sh 里应有 %q，否则发行包里的 README 会裂图", want)
		}
	}
	// 截图目录必须真的有图，不然拷贝步骤等于空转。
	entries, err := os.ReadDir(filepath.Join("..", "..", "docs", "images"))
	if err != nil {
		t.Fatalf("读取 docs/images 失败: %v", err)
	}
	if len(entries) == 0 {
		t.Error("docs/images 里没有任何图片")
	}
}
