// Package assets 内嵌 dsh-gate 自带的静态资源。
//
// 目前只有登录页一个文件。之所以内嵌而不是从磁盘读：网关常常被拷到只有一
// 个二进制的机器上跑，内嵌资源让它没有任何外部依赖，也杜绝了「部署时忘了
// 拷 resources 目录」这类事故。
//
// 页面里没有任何外部请求（无 CDN、无外链字体），因此在一台断网的服务器上
// 也一样好看。
package assets

import (
	"embed"
	"fmt"
	"html/template"
)

//go:embed login.html
var files embed.FS

// LoginFile 是登录页模板在 embed 文件系统里的名字。
const LoginFile = "login.html"

// LoginTemplate 解析登录页模板。
//
// 每次调用都重新解析，而不是包级变量 + sync.Once：模板解析只需要几十微秒，
// 而包级初始化会让「模板写错了」这种错误提前到任意包被 import 的时刻才爆，
// 报错现场反而更难读。
func LoginTemplate() (*template.Template, error) {
	// 注意用 ParseFS 而不是 New("...").ParseFS：后者会因为 ParseFS 按**文件名**
	// 注册模板而留下一具空壳，用 New 起的那个名字执行时只会得到
	// "incomplete or empty template"。这里返回的模板可以直接 Execute。
	tmpl, err := template.ParseFS(files, LoginFile)
	if err != nil {
		return nil, fmt.Errorf("解析登录页模板失败: %w", err)
	}
	return tmpl, nil
}
