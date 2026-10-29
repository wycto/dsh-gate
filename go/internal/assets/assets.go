// Package assets 内嵌 dsh-gate 自带的静态资源（登录页、管理端首页、管理台）。
//
// 之所以内嵌而不是从磁盘读：网关常常被拷到只有一个二进制的机器上跑，内嵌资源
// 让它没有任何外部依赖，也杜绝了「部署时忘了拷 resources 目录」这类事故。
//
// 页面里没有任何外部请求（无 CDN、无外链字体、无脚本框架），因此在一台断网的
// 服务器上也一样好看。内联脚本只有两处：登录页的「显示口令」开关，以及管理台的
// 自动刷新（admin-live.js，按内容哈希单独放行）；管理端首页不带脚本。
package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
)

//go:embed login.html portal.html admin.html theme.css admin-live.js
var files embed.FS

// LoginFile 是登录页模板在 embed 文件系统里的名字。
const LoginFile = "login.html"

// PortalFile 是管理端首页（「我的实例」）模板在 embed 文件系统里的名字。
const PortalFile = "portal.html"

// AdminFile 是管理台模板在 embed 文件系统里的名字。
const AdminFile = "admin.html"

// ThemeFile 是共享配色令牌的嵌入名。它不是页面：三张页面在自己的 <style> 里
// {{template "dsh-theme" .}} 注入同一份令牌，改配色只动这一个文件。
const ThemeFile = "theme.css"

// ThemeTemplateName 是令牌块注册进模板集时用的名字。
const ThemeTemplateName = "dsh-theme"

// AdminLiveFile 是管理台自动刷新脚本的嵌入名。它不是页面，是那段内联进
// 管理台的唯一脚本：页面里的动态状态（运行 / 安装进度 / 环境 / 审计）全靠它轮询。
const AdminLiveFile = "admin-live.js"

// AdminLiveScript 返回管理台自动刷新脚本与它的 CSP 源表达式（sha256-…）。
//
// 哈希随脚本内容在调用时算出：脚本一改，放行规则跟着变，不会出现「改了脚本
// 却被 CSP 静默挡住、页面看起来只是不刷新」这种最难查的故障。
func AdminLiveScript() (string, string, error) {
	script, err := files.ReadFile(AdminLiveFile)
	if err != nil {
		return "", "", fmt.Errorf("读取管理台自动刷新脚本失败: %w", err)
	}
	sum := sha256.Sum256(script)
	return string(script), "sha256-" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// parsePage 解析一个页面模板，并把共享的配色令牌块挂进同一个模板集。
//
// 令牌内容在解析时一次性读入（go:embed 本来就是内存里的字节）；页面里
// {{template "dsh-theme"}} 不带数据，所以令牌块不引用任何字段，三页共用的
// 这段 CSS 逐字节一致。
//
// 每次调用都重新解析，而不是包级变量 + sync.Once：模板解析只需要几十微秒，
// 而包级初始化会让「模板写错了」这种错误提前到任意包被 import 的时刻才爆，
// 报错现场反而更难读。
func parsePage(page, label string) (*template.Template, error) {
	theme, err := files.ReadFile(ThemeFile)
	if err != nil {
		return nil, fmt.Errorf("读取共享配色令牌失败: %w", err)
	}
	base, err := template.New(page).
		Parse(`{{define "` + ThemeTemplateName + `"}}` + string(theme) + `{{end}}`)
	if err != nil {
		return nil, fmt.Errorf("解析共享配色令牌失败: %w", err)
	}
	tmpl, err := base.ParseFS(files, page)
	if err != nil {
		return nil, fmt.Errorf("解析%s模板失败: %w", label, err)
	}
	// ParseFS 按**文件名**注册模板；返回的这具无名空壳留着只会误导调用方。
	return tmpl.Lookup(page), nil
}

// LoginTemplate 解析登录页模板。
func LoginTemplate() (*template.Template, error) {
	return parsePage(LoginFile, "登录页")
}

// PortalTemplate 解析管理端首页模板。
func PortalTemplate() (*template.Template, error) {
	return parsePage(PortalFile, "管理端首页")
}

// AdminTemplate 解析管理台模板。
func AdminTemplate() (*template.Template, error) {
	return parsePage(AdminFile, "管理台")
}
