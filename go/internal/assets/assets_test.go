// 页面模板自身的结构性约束。
//
// 这里测的是**模板源码**而不是渲染结果：渲染结果只包含「当前有数据的那些行」，
// 空表一行都不渲染，于是「某个表的某一列漏了 data-label」在渲染测试里会因为
// 恰好没数据而蒙混过关。手机端排版是模板层面的约定，就该在模板层面钉住。
package assets

import (
	"regexp"
	"strings"
	"testing"
)

// tdPattern 抓出一个 td 开始标签的属性部分。
//
// `[^>]*` 对模板源码是安全的：属性值里只会出现 {{...}}，不会有裸的 `>`。
var tdPattern = regexp.MustCompile(`<td([^>]*)>`)

// TestAdminTableCellsCarryMobileLabels 保证手机端卡片布局拿得到列名。
//
// 窄屏下管理台的表格被 CSS 改成一张张卡片（admin.html 的 @media (max-width:720px)），
// 每个值显示什么列名，靠的是 td 上的 data-label。漏一个 data-label，手机上就只剩
// 一个没有说明的值——这是「适配手机端」最容易悄悄退化的地方，所以钉一条测试。
// 第一列的标签由 CSS 隐藏（它是卡片标题，不必重复列名），但属性仍然要有，
// 这样这条测试的规则才能简单到「只有动作列与跨列行可以没有 data-label」。
func TestAdminTableCellsCarryMobileLabels(t *testing.T) {
	source, err := files.ReadFile(AdminFile)
	if err != nil {
		t.Fatalf("读取管理台模板失败: %v", err)
	}
	body := string(source)

	var total, missing int
	var offenders []string
	for _, match := range tdPattern.FindAllStringSubmatch(body, -1) {
		attrs := match[1]
		total++
		if strings.Contains(attrs, "data-label=") {
			continue
		}
		// 动作列（按钮/表单）与跨列说明行天然没有对应列名。
		if strings.Contains(attrs, "actions") || strings.Contains(attrs, "colspan") {
			continue
		}
		missing++
		if len(offenders) < 3 {
			offenders = append(offenders, strings.TrimSpace(attrs))
		}
	}
	if total == 0 {
		t.Fatal("管理台模板里一个 td 都没有，测试前提不成立")
	}
	if missing > 0 {
		t.Errorf("有 %d 个 td 缺 data-label（手机端会显示成没有说明的值），例如: %v", missing, offenders)
	}
}

// TestAdminHasMobileCardLayout 保证卡片布局那套 CSS 还在。
//
// 只有 data-label 而没有这段媒体查询，标签就只是摆设：窄屏下仍是表格，
// 按钮继续被挤到屏幕外。
func TestAdminHasMobileCardLayout(t *testing.T) {
	source, err := files.ReadFile(AdminFile)
	if err != nil {
		t.Fatalf("读取管理台模板失败: %v", err)
	}
	body := string(source)
	for _, want := range []string{"max-width:900px", "attr(data-label)", "thead{display:none}"} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少窄屏布局所需片段 %q", want)
		}
	}
}

// TestPagesDeclareMobileViewport 保证每个页面都声明了 viewport。
//
// 少了它，手机上浏览器会按桌面宽度（约 980px）渲染再整体缩小，
// 媒体查询根本不会触发，前面那套手机端布局等于没写。
func TestPagesDeclareMobileViewport(t *testing.T) {
	for _, name := range []string{LoginFile, PortalFile, AdminFile} {
		source, err := files.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		if !strings.Contains(string(source), `name="viewport"`) {
			t.Errorf("%s 没有声明 viewport，手机端会按桌面宽度缩放渲染", name)
		}
	}
}

// TestPagesSupportThemeSwitch 保证三个页面都接上了「服务端下发配色」这套机制。
//
// 配色是服务端定稿、页面零判断：模板必须把 .Theme 写进 <html data-theme>（明暗
// 三态，缺省不写即跟随系统）、把 .Scheme 写进 <html data-scheme>（色相四选一，
// 永远有值），并在自己的 <style> 里注入共享令牌块 {{template "dsh-theme"}}。
// 页脚两枚胶囊（明暗 / 方案）的链接也是结构约定，少一段就少一种选择。
func TestPagesSupportThemeSwitch(t *testing.T) {
	for _, name := range []string{LoginFile, PortalFile, AdminFile} {
		source, err := files.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		body := string(source)
		for _, want := range []string{
			`{{if .Theme}} data-theme="{{.Theme}}"{{end}}`,
			`data-scheme="{{.Scheme}}"`,
			`{{template "dsh-theme" .}}`,
			`class="themeswitch"`,
			`{{range .Themes}}`,
			`{{range .Schemes}}`,
			`aria-current="true"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 缺少配色切换片段 %q", name, want)
			}
		}
		// 读屏只念得出「跟随系统 亮色 暗色」几个孤字，得靠 aria-label 说明这是配色。
		for _, want := range []string{`aria-label="{{.ThemeLabel}}"`, `aria-label="{{.SchemeLabel}}"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 的配色切换缺少 %s", name, want)
			}
		}
	}
}

// cssRuleBody 取出选择器之后第一个 {…} 里的内容（按花括号配对）。
//
// 令牌块的值里不会有花括号，所以这个简化版配对足够用；它也顺便挡住了「以后在
// 令牌里塞一段嵌套规则」这种会让测试失效的改法——那时这里会取到错的内容。
func cssRuleBody(body, selector string) (string, bool) {
	idx := strings.Index(body, selector)
	if idx < 0 {
		return "", false
	}
	open := strings.Index(body[idx:], "{")
	if open < 0 {
		return "", false
	}
	start := idx + open + 1
	depth := 1
	for i := start; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[start:i], true
			}
		}
	}
	return "", false
}

// tokenPattern 抓出一条自定义属性声明。
var tokenPattern = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;]+);`)

// cssTokens 把一个规则块里的自定义属性收成 map。
//
// 值里的空白归一化成一个空格：两份令牌块的缩进深度不同（一份在 media 里多一层），
// 而多行值（阴影）的换行与缩进在各块里并不一致，逐字符比对会误报。
func cssTokens(rule string) map[string]string {
	tokens := map[string]string{}
	for _, match := range tokenPattern.FindAllStringSubmatch(rule, -1) {
		tokens[match[1]] = strings.Join(strings.Fields(match[2]), " ")
	}
	return tokens
}

// TestLightTokensMatchBetweenTriggers 保证共享令牌里每个方案的两种浅色触发
// 方式给出完全一样的令牌。
//
// 浅色令牌必须写两份：`:root[data-theme="light"][data-scheme="X"]`（用户显式
// 选了亮色）与 media 里的 `:root:not([data-theme="dark"])[data-scheme="X"]`
// （没选、但系统是亮色）。两份一旦走样，就会出现「跟系统的亮色」和「手选的
// 亮色」观感不同这种极难复现的问题，而肉眼对比两个相距几十行的 CSS 块并不可靠。
// 令牌收敛进 theme.css 之后，这里对四个方案逐条比对。
func TestLightTokensMatchBetweenTriggers(t *testing.T) {
	source, err := files.ReadFile(ThemeFile)
	if err != nil {
		t.Fatalf("读取共享配色令牌失败: %v", err)
	}
	body := string(source)

	for _, scheme := range []string{"slate", "ocean", "violet", "forest"} {
		explicitRule, ok := cssRuleBody(body, `:root[data-theme="light"][data-scheme="`+scheme+`"]`)
		if !ok {
			t.Errorf("共享令牌缺少方案 %s 的显式亮色块", scheme)
			continue
		}
		mediaRule, ok := cssRuleBody(body, `:root:not([data-theme="dark"])[data-scheme="`+scheme+`"]`)
		if !ok {
			t.Errorf("共享令牌缺少方案 %s 的跟随系统亮色块", scheme)
			continue
		}

		explicit, media := cssTokens(explicitRule), cssTokens(mediaRule)
		// 令牌块至少要有十几个，否则多半是选择器顺序变了、取到了别的规则。
		if len(explicit) < 10 || len(media) < 10 {
			t.Errorf("方案 %s 的浅色令牌块看着不完整（显式 %d 条 / 跟随系统 %d 条）",
				scheme, len(explicit), len(media))
			continue
		}
		for token, want := range explicit {
			if got, ok := media[token]; !ok {
				t.Errorf("方案 %s 的跟随系统亮色缺少令牌 %s", scheme, token)
			} else if got != want {
				t.Errorf("方案 %s 的令牌 %s 两种触发方式不一致：显式 %q，跟随系统 %q", scheme, token, want, got)
			}
		}
		for token := range media {
			if _, ok := explicit[token]; !ok {
				t.Errorf("方案 %s 的显式亮色缺少令牌 %s（跟随系统里有）", scheme, token)
			}
		}
	}
}

// TestThemeFileCoversEveryScheme 保证每个方案都有暗色块与两份亮色块，
// 且缺省方案（slate）的基础令牌写在裸 :root 上——没带 data-scheme 的引用
// （比如有人直接打开嵌了令牌的页面）也能拿到一套完整颜色。
func TestThemeFileCoversEveryScheme(t *testing.T) {
	source, err := files.ReadFile(ThemeFile)
	if err != nil {
		t.Fatalf("读取共享配色令牌失败: %v", err)
	}
	body := string(source)

	// 裸 :root 里必须有一套完整的基础令牌（slate 暗色）。
	base, ok := cssRuleBody(body, ":root{")
	if !ok {
		t.Fatal("共享令牌缺少裸 :root 基础块")
	}
	tokens := cssTokens(base)
	for _, want := range []string{"--desktop-1", "--text", "--accent", "--ok", "--danger", "--aurora-1"} {
		if _, has := tokens[want]; !has {
			t.Errorf("裸 :root 缺少基础令牌 %s", want)
		}
	}

	for _, scheme := range []string{"ocean", "violet", "forest"} {
		dark, ok := cssRuleBody(body, `:root[data-scheme="`+scheme+`"]{`)
		if !ok {
			t.Errorf("共享令牌缺少方案 %s 的暗色块", scheme)
			continue
		}
		// 暗色块只覆写有差异的令牌；至少要覆盖背景、文本、主色三组。
		d := cssTokens(dark)
		for _, want := range []string{"--desktop-1", "--text", "--accent"} {
			if _, has := d[want]; !has {
				t.Errorf("方案 %s 的暗色块缺少令牌 %s", scheme, want)
			}
		}
	}
}

// TestAdminTabShell 保证分页导航的骨架完整。
//
// 管理台靠「隐藏单选钮 + :has()」切页，不刷新也不写脚本。三种布局（底部 Dock、
// 顶栏页签、侧栏）共用同一组钮，所以钮只有一份、由 {{range .NavItems}} 生成，
// 三处导航都只是指向它的 <label>。这套机制的地基是：
//   - 面板显示规则 body:has(#tab-<页>:checked) #panel-<页> 五页齐全；
//   - 钮的 id 与面板 id 同名（tab-<页> ↔ panel-<页>），由同一个 .ID 驱动；
//   - 选中态走 body:has(...) [for=...]——钮与 label 不再相邻，兄弟选择器够不着。
//
// 漏掉任何一段，那一页就永远打不开或永远不高亮，而渲染测试测不出来
// （面板一直都在 DOM 里）。所以在模板源码层面钉住。
func TestAdminTabShell(t *testing.T) {
	source, err := files.ReadFile(AdminFile)
	if err != nil {
		t.Fatalf("读取管理台模板失败: %v", err)
	}
	body := string(source)

	for _, tab := range []string{"users", "grants", "instances", "env", "audit"} {
		panel := `id="panel-` + tab + `"`
		if !strings.Contains(body, panel) {
			t.Errorf("管理台缺少分页面板 %s", panel)
		}
		rule := "body:has(#tab-" + tab + ":checked) #panel-" + tab
		if !strings.Contains(body, rule) {
			t.Errorf("管理台缺少分页显示规则 %q", rule)
		}
	}

	// 单选钮只有一份，且 id 由 .ID 拼出——与面板 id 同一命名法。
	if !strings.Contains(body, `{{range .NavItems}}<input type="radio" name="admintab" id="tab-{{.ID}}"`) {
		t.Error("管理台缺少由 NavItems 生成的共用单选钮组")
	}
	if n := strings.Count(body, `type="radio" name="admintab"`); n != 1 {
		t.Errorf(`admintab 单选钮应当只有一处模板（三套导航共用），实际 %d 处`, n)
	}
	// 三套导航都必须是同一组钮的 label。
	for _, want := range []string{
		`<nav class="dock"`, `<nav class="topnav"`, `<nav class="snav">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少一套导航 %q", want)
		}
	}
	if n := strings.Count(body, `for="tab-{{.ID}}"`); n != 3 {
		t.Errorf(`指向共用钮的 label 应有三处（Dock / 顶栏 / 侧栏），实际 %d`, n)
	}

	// 老浏览器回退：不支持 :has() 时必须平铺全部面板并隐藏底部 Dock，
	// 不能让人面对一块点了没反应的菜单。
	for _, want := range []string{`@supports not selector(:has(*))`, ".panel{display:block}", ".dock{display:none}"} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少无 :has() 回退片段 %q", want)
		}
	}
	// 选中态与键盘焦点圈靠 body:has(...) [for=...] 这条结构约定。
	for _, want := range []string{
		`body:has(#tab-instances:checked) .dock [for="tab-instances"]`,
		`body:has(#tab-instances:focus-visible) .dock [for="tab-instances"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少导航选中/焦点规则 %q", want)
		}
	}
}

// TestAdminLayoutStyles 保证三种布局的开关骨架完整。
//
// 布局由 <html data-style> 决定，三套导航与统计条/侧栏的可见性全靠
// :root[data-style="X"] 的显示规则。少一条规则的症状是「切了没反应」，
// 而渲染测试里 DOM 一直都在，测不出来——钉在模板层面。
func TestAdminLayoutStyles(t *testing.T) {
	source, err := files.ReadFile(AdminFile)
	if err != nil {
		t.Fatalf("读取管理台模板失败: %v", err)
	}
	body := string(source)

	if !strings.Contains(body, `data-style="{{.Style}}"`) {
		t.Error("管理台没有把布局写进 <html data-style>")
	}
	if !strings.Contains(body, `aria-label="{{.StyleLabel}}"`) {
		t.Error("管理台页脚缺少布局切换胶囊")
	}
	// 默认态（console）：顶栏、侧栏、统计条都不占位。
	if !strings.Contains(body, ".topnav,.sidebar,.stats{display:none}") {
		t.Error("缺少默认布局下顶栏/侧栏/统计条的隐藏规则")
	}
	for _, style := range []string{"overview", "split"} {
		// 每个非默认布局都要：亮出自己的导航、收起 Dock。
		for _, want := range []string{
			`:root[data-style="` + style + `"] .dock{display:none}`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("布局 %s 缺少规则 %q", style, want)
			}
		}
	}
	for _, want := range []string{
		`:root[data-style="overview"] .topnav{display:flex}`,
		`:root[data-style="overview"] .stats{display:grid}`,
		`:root[data-style="split"] .sidebar{display:flex}`,
		`:root[data-style="split"] .shell{display:flex`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少布局切换规则 %q", want)
		}
	}
	// 统计条的数字由服务端渲染，自动刷新脚本只做原地替换。
	for _, want := range []string{"{{.CountInstances}}", "{{.CountRunning}}", "{{.CountUsers}}"} {
		if !strings.Contains(body, want) {
			t.Errorf("统计条缺少字段 %s", want)
		}
	}
}

// TestLoginPutsCardBeforePitchOnNarrowScreens 保证手机端登录卡排在介绍前面。
//
// 单列时如果仍按源码顺序（品牌 → 介绍 → 要点 → 登录卡），手机上那几段文案会把
// 整张登录卡挤到首屏之外，连口令框都要滚一屏才看得见。窄屏改用 flex order 重排成
// 「品牌 → 登录卡 → 介绍 → 要点」。这类回归不会让任何运行期测试变红（页面照样能
// 登录），所以只能在模板源码层面钉住：哪条 order 丢了，手机上就悄悄退回旧排版。
func TestLoginPutsCardBeforePitchOnNarrowScreens(t *testing.T) {
	source, err := files.ReadFile(LoginFile)
	if err != nil {
		t.Fatalf("读取登录页模板失败: %v", err)
	}
	body := string(source)

	for _, want := range []string{
		"@media (max-width: 860px)",
		".pitch { display: contents; }",
		".brand { order: 1; }",
		".shell { order: 2;",
		".lede { order: 3; }",
		".feats { order: 4; }",
		"@media (max-width: 640px)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("登录页缺少窄屏排序片段 %q", want)
		}
	}

	// 顺序号必须从 1 到 4 各出现一次：重复或跳号都会让两块内容叠在一起或错位，
	// 而且肉眼在 390px 宽的手机模拟器里未必看得出来。
	for _, order := range []string{"order: 1;", "order: 2;", "order: 3;", "order: 4;"} {
		if n := strings.Count(body, order); n != 1 {
			t.Errorf("登录页的 %q 应恰好出现 1 次（窄屏排序用），实际 %d 次", order, n)
		}
	}
}

// sectionOf 取出模板里某一块 <section id="..."> 到它对应 </section> 之间的源码。
//
// 面板之间会互相干扰：同一个类名可能出现在别的面板里，所以按面板切一刀再断言，
// 比在整份模板里做 strings.Contains 精确得多。
func sectionOf(body, id string) string {
	open := `<section class="panel" id="` + id + `"`
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</section>")
	if j < 0 {
		return ""
	}
	return body[i : i+j]
}

// TestAdminInstancesUseCardGrid 保证实例列表是卡片网格，而不是又退回一张多列表格。
//
// 实例列表曾经是 8 列固定宽度的表格（名字 / ID / DSH_HOME / 端口 / 状态 / 运行状态 /
// 安装状态 / 操作）：右半边大片空白，DSH_HOME 在 140px 的列里被折成两三行，操作列贴右
// 浮动与整行基线不齐，每行还被「保存配置」那条跨整行的折叠面板切成两截——列宽怎么调，
// 都只是换个地方折行。现在改成卡片网格：卡头放名字与状态徽标，配置区短字段两列并排、
// 路径独占整行，动作固定在卡底。渲染测试测不出这种纯排版退化（空表一行都不渲染），
// 所以钉在模板源码层面：
//   - 每台实例是一个 article.inst-card，外面套 div.inst-grid；
//   - 卡片里不得再出现 td/th —— 退回表格就等于把横向挤压带回来；
//   - 网格列宽必须是 minmax(min(...)) 的写法，窄屏才不会横向溢出；
//   - 运行状态靠卡片左缘竖条区分，is-ready / is-starting 两条规则都要在。
func TestAdminInstancesUseCardGrid(t *testing.T) {
	source, err := files.ReadFile(AdminFile)
	if err != nil {
		t.Fatalf("读取管理台模板失败: %v", err)
	}
	body := string(source)

	panel := sectionOf(body, "panel-instances")
	if panel == "" {
		t.Fatal("管理台模板里找不到 panel-instances 面板")
	}

	for _, want := range []string{`class="inst-grid"`, `class="inst-card`, `class="inst-head`, `class="inst-meta"`, `class="inst-actions"`} {
		if !strings.Contains(panel, want) {
			t.Errorf("实例面板缺少卡片网格所需片段 %q", want)
		}
	}
	// 卡片化之后实例面板里不该再有表格单元格：有 td/th 就是又退回横向列挤压。
	for _, unwanted := range []string{"<td", "<th ", "<thead>", "<table>"} {
		if strings.Contains(panel, unwanted) {
			t.Errorf("实例面板里不该再出现 %q（卡片网格不用表格）", unwanted)
		}
	}

	for _, want := range []string{
		".inst-grid{display:grid",
		"minmax(min(390px,100%),1fr)",
		".inst-card{display:flex",
		".inst-card.is-ready{border-left:3px solid var(--ok)}",
		".inst-card.is-starting{border-left:3px solid var(--warning)}",
		".inst-meta .im.wide{grid-column:1/-1}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少实例卡片样式 %q", want)
		}
	}
	// 路径字段必须能整段折行读完，而不是挤在窄列里断成两三行。
	if !strings.Contains(body, ".inst-meta dd.mono{color:var(--muted);font-size:1em;white-space:normal}") {
		t.Error("实例卡片的路径字段缺少 white-space:normal（路径会被截断成一行）")
	}
	// 安装进度与启动失败那两行带着命令和路径，套着 .mono（全局 nowrap）时
	// 必须折行，否则整行会伸出卡片、压到隔壁实例上。
	if !strings.Contains(body, ".inst-note.mono,.inst-note .mono{white-space:normal;overflow-wrap:anywhere}") {
		t.Error("实例卡片的说明行缺少折行规则（长命令会伸出卡片）")
	}
}
