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

// TestAdminTabShell 保证分页导航的骨架完整。
//
// 管理台靠「隐藏单选钮 + :has()」切页，不刷新也不写脚本。这套机制的地基是：
// 每个面板 id 都有一个同名的单选钮（tab-<页> ↔ panel-<页>）、CSS 里有一条
// 对应的显示规则。漏掉任何一段，那一页就永远打不开——而且渲染测试测不出来
// （面板一直都在 DOM 里，只是没人让它显示）。所以在模板源码层面钉住。
func TestAdminTabShell(t *testing.T) {
	source, err := files.ReadFile(AdminFile)
	if err != nil {
		t.Fatalf("读取管理台模板失败: %v", err)
	}
	body := string(source)
	for _, tab := range []string{"users", "grants", "instances", "env", "audit"} {
		radio := `id="tab-` + tab + `"`
		panel := `id="panel-` + tab + `"`
		if !strings.Contains(body, radio) {
			t.Errorf("管理台缺少分页单选钮 %s", radio)
		}
		if !strings.Contains(body, panel) {
			t.Errorf("管理台缺少分页面板 %s", panel)
		}
		rule := "body:has(#tab-" + tab + ":checked) #panel-" + tab
		if !strings.Contains(body, rule) {
			t.Errorf("管理台缺少分页显示规则 %q", rule)
		}
	}
	// 单选钮必须是同一组（同名），否则点导航选不中、页面切不过去。
	// 9 个：users/grants/instances/audit 两分支各一份 + env 只在有 Installer 的分支。
	if n := strings.Count(body, `type="radio" name="admintab"`); n != 9 {
		t.Errorf(`name="admintab" 的单选钮数量不对（应为 9）: %d`, n)
	}
	// 老浏览器回退：不支持 :has() 时必须平铺全部面板并隐藏底部 Dock，
	// 不能让人面对一块点了没反应的菜单。
	for _, want := range []string{`@supports not selector(:has(*))`, ".panel{display:block}", ".dock{display:none}"} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少无 :has() 回退片段 %q", want)
		}
	}
	// 选中态与键盘焦点圈靠「radio 的相邻兄弟是它自己的标签」这条结构约定，
	// 换掉结构（比如中间插一层容器）这两条规则就会静默失效。
	for _, want := range []string{".tab-input:checked + .tab", ".tab-input:focus-visible + .tab"} {
		if !strings.Contains(body, want) {
			t.Errorf("管理台缺少导航选中/焦点规则 %q", want)
		}
	}
}
