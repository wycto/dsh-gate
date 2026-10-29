package proxy

import (
	"html"
	"net/http"
	"strconv"
	"strings"
)

// 本文件是网关自己兜底时返回的三张页面：dsh 还在启动、dsh 连不上、以及
// dsh 会话失效需要重新交接。
//
// 它们刻意不依赖任何外部资源：出问题的时刻往往正是网络不通、磁盘吃紧的
// 时刻，那时候最不能要的就是一个还要从 CDN 拉 CSS 的页面。
//
// 三张页面共用同一套极简样式，与登录页保持同一视觉语言，用户不会觉得
// 「怎么换了个界面」。

// page 渲染一张自包含的提示页。
//
// refresh 是自动刷新间隔（秒）；0 表示不自动刷新。redirect 非空时页面会立即
// 跳到该地址（而不是等一个刷新周期）——「重新进入」这类一步到位的跳转用它。
func page(title, message, detail string, refresh int, redirect string) string {
	var refreshTag string
	if refresh > 0 {
		refreshTag = `<meta http-equiv="refresh" content="` + strconv.Itoa(refresh) + `">`
	}
	var detailHTML string
	if detail != "" {
		detailHTML = `<p class="detail">` + html.EscapeString(detail) + `</p>`
	}
	var script string
	switch {
	case redirect != "":
		// 立即跳转。用 location.replace 而不是 href，是为了让这次跳转不留在
		// 浏览���历史里——用户按「后退」时不该又被弹回来一次。
		script = `<script>location.replace(` + jsString(redirect) + `);</script>`
	case refresh > 0:
		script = `<script>document.documentElement.classList.add('auto');</script>`
	}

	return `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
` + refreshTag + `
<title>` + html.EscapeString(title) + `</title>
<style>
  :root {
    color-scheme: dark;
    --bg: #0b0e14;
    --card: #151a23;
    --fg: #e6edf3;
    --muted: #8b98a9;
    --accent: #4f8cff;
  }
  @media (prefers-color-scheme: light) {
    :root {
      color-scheme: light;
      --bg: #f6f8fb;
      --card: #ffffff;
      --fg: #1b2430;
      --muted: #5b6b7d;
      --accent: #2f6fe4;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 24px;
    background: radial-gradient(120% 120% at 50% 0%, #172033 0%, var(--bg) 60%);
    color: var(--fg);
    font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI",
      "PingFang SC", "Microsoft YaHei", sans-serif;
  }
  @media (prefers-color-scheme: light) {
    body { background: radial-gradient(120% 120% at 50% 0%, #e8eefb 0%, var(--bg) 60%); }
  }
  .card {
    width: min(100%, 420px);
    padding: clamp(28px, 6vw, 44px);
    border-radius: 18px;
    background: var(--card);
    border: 1px solid rgba(127, 145, 170, 0.18);
    box-shadow: 0 20px 60px rgba(0, 0, 0, 0.35);
    text-align: center;
  }
  h1 { margin: 0 0 12px; font-size: clamp(20px, 4.5vw, 24px); font-weight: 600; letter-spacing: .01em; }
  p { margin: 0; color: var(--muted); font-size: 15px; line-height: 1.7; }
  .detail {
    margin-top: 14px;
    font-size: 13px;
    padding: 10px 12px;
    border-radius: 10px;
    background: rgba(127, 145, 170, 0.12);
    word-break: break-word;
  }
  .spinner {
    width: 34px; height: 34px;
    margin: 0 auto 22px;
    border-radius: 50%;
    border: 3px solid rgba(127, 145, 170, 0.22);
    border-top-color: var(--accent);
    animation: spin 0.9s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) {
    .spinner { animation: none; }
  }
</style>
` + script + `
</head>
<body>
  <main class="card">
    <div class="spinner" aria-hidden="true"></div>
    <h1>` + html.EscapeString(title) + `</h1>
    <p>` + html.EscapeString(message) + `</p>
    ` + detailHTML + `
  </main>
</body>
</html>
`
}

// renderWaiting 返回「dsh 还在启动」页面。
//
// 这不是错误页，而是一个每 2 秒自动重试的等待页：Linux 服务器上装好之后
// 第一次启动要下载插件、初始化 profile，可能要一两分钟，此时页面上明确写着
// 「正在启动」比让人对着一个 502 发呆要好得多。
func renderWaiting(w http.ResponseWriter, reason string) {
	writePage(w, http.StatusServiceUnavailable,
		page("DSH 正在启动", "网关已经就绪，正在等待 DSH 实例完成启动。首次启动可能需要几分钟。", reason, 2, ""))
}

// renderAuthRequired 返回「DSH 要求重新登录」页面。
//
// 只在「网关自己也没法把浏览器送进 DSH」时出现：带着交接标记的浏览器会被
// 303 直接送回 / 重做交接，不会看到这一页。看到它说明网关这次启动的令牌换成
// 不了会话——最常见的是**接管了一个外部启动的 dsh 却拿不到它的令牌**：dsh 的
// 令牌每个进程随机生成、只在启动时打印一次、也不落盘，别人拉起的 dsh 我们无从得知。
//
// 所以这里必须给出**确定可行**的那条路，而不是只让人去找那个早就没了的地址。
func renderAuthRequired(detail string, dshPort int) string {
	message := "网关没能完成与 DSH 的令牌交接。" +
		"如果这台 dsh 是别处启动的（不是网关拉起的），网关就拿不到它的启动令牌——" +
		"令牌每个进程随机生成、只在 dsh 启动时打印一次，也不写进任何文件。两条出路："
	if dshPort > 0 {
		message += "① 你还留着 dsh 启动时打印的地址：把它整条粘进地址栏，把端口换成网关的（形如 http://127.0.0.1:3080/?token=… → 换成网关地址）；" +
			"② 让网关自己把 dsh 管起来（一定成功）：先停掉外面那个 dsh，再重启网关实例，" +
			"这样 dsh 由网关拉起、令牌自然就进了网关内存。"
	} else {
		message += "① 还留着 dsh 启动时打印的那条带 token 的地址就粘进地址栏；" +
			"② 让网关自己把 dsh 管起来（一定成功）：先停掉外面那个 dsh，再重启网关实例。"
	}
	return page("DSH 要求重新登录", message, detail, 0, "")
}

// StoppedPage 返回「实例已被停止」页面处理器。
//
// 为什么单独一张、而不是复用等待页：等待页每 2 秒自动刷新，等于浏览器替用户
// 把「启动」按了一遍又一遍——刚点完停止的实例会被自己那个还开着的标签页重新
// 拉起来。这一页不刷新，也直说下一步该做什么。
func StoppedPage(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		message := "这个实例已经被停止，网关不会因为它收到请求就自作主张地把它重新拉起。"
		if name != "" {
			message = "实例「" + name + "」已经被停止。网关不会因为它收到请求就自作主张地重新拉起。"
		}
		writePage(w, http.StatusServiceUnavailable,
			page("实例已停止", message+"要再用它，请回管理台点「启动」。", "", 0, ""))
	})
}

// renderUpstreamDown 返回「dsh 连不上」页面。
func renderUpstreamDown(w http.ResponseWriter, err error) {
	detail := ""
	if err != nil {
		// 只保留错误串里跟连接有关的部分：完整的 net/url 错误很长，
		// 直接贴给用户反而更难懂。
		detail = firstClause(err.Error())
	}
	writePage(w, http.StatusBadGateway,
		page("DSH 实例暂时不可用", "网关无法连接到 DSH，它可能正在重启。页面会自动重试。", detail, 3, ""))
}

// writePage 写出一张页面并加上「不要缓存」。
func writePage(w http.ResponseWriter, status int, body string) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// firstClause 截取错误串的第一句，避免把一长串 URL 错误甩到页面上。
func firstClause(text string) string {
	if idx := strings.IndexAny(text, "\n"); idx >= 0 {
		return text[:idx]
	}
	return text
}

// jsString 把地址渲染成可以直接放进 JS 字面量的形式。
//
// 目标只有 "/" 这类固定值，但仍然转义一遍：这里是唯一一处把字符串拼进
// <script> 的地方，习惯性地转义比日后有人改成拼路径时再想起要转义可靠。
func jsString(value string) string {
	replaced := strings.NewReplacer(
		"\\", "\\\\",
		`"`, `\"`,
		"</", `<\/`,
	).Replace(value)
	return `"` + replaced + `"`
}
