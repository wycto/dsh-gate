package proxy_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/wycto/dsh-gate/internal/cookie"
	"github.com/wycto/dsh-gate/internal/dshtest"
	"github.com/wycto/dsh-gate/internal/proxy"
)

// gateHost 是测试里模拟的「网关对外地址」。
//
// 刻意用一个既不是回环也不是上游地址的名字：只有这样才能验证网关确实把
// 浏览器看到的 Host 原样转发给了 dsh——如果被改写成监听器地址，下面的
// 「会话与 authority 绑定」这些断言就都失去意义了。
const gateHost = "gate.test"

// fakeTokens 是测试用的令牌来源。
type fakeTokens struct {
	mu    sync.Mutex
	token string
}

func (f *fakeTokens) Token() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}

func (f *fakeTokens) SetToken(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = token
}

// newGate 搭起「假 dsh + 网关处理器」并返回网关的地址。
func newGate(t *testing.T) (*dshtest.Server, *fakeTokens, string) {
	t.Helper()

	upstreamDSH := dshtest.Start(gateHost)
	t.Cleanup(upstreamDSH.Close)

	tokens := &fakeTokens{token: upstreamDSH.Token}
	upstream, err := url.Parse(upstreamDSH.URL())
	if err != nil {
		t.Fatalf("解析上游地址失败: %v", err)
	}
	handler, err := proxy.New(proxy.Options{
		Upstream: upstream,
		Tokens:   tokens,
		Ready:    func() bool { return true },
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	return upstreamDSH, tokens, gateway.URL
}

// browser 是一个极简的「浏览器」：自己维护 Cookie，**不自动跟随跳转**。
//
// 不用 cookiejar 是因为这里必须看清楚每一步的中间响应——尤其是 dsh 换完
// Cookie 之后的那个 302，没有它就没法验证「只交接一次」这件事。
type browser struct {
	t        *testing.T
	base     string
	cookies  map[string]string
	client   *http.Client
	response *http.Response
}

func newBrowser(t *testing.T, base string) *browser {
	return &browser{
		t:       t,
		base:    base,
		cookies: map[string]string{},
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// get 发起一次 GET，并把响应里的 Cookie 存下来。
func (b *browser) get(path string) *http.Response {
	b.t.Helper()
	request, err := http.NewRequest(http.MethodGet, b.base+path, nil)
	if err != nil {
		b.t.Fatalf("构造请求失败: %v", err)
	}
	request.Host = gateHost
	b.applyCookies(request)
	response, err := b.client.Do(request)
	if err != nil {
		b.t.Fatalf("请求失败: %v", err)
	}
	b.absorb(response)
	return response
}

// getNavigate 发起一次「页面导航」语义的 GET。
//
// 浏览器的导航请求带 Sec-Fetch-Mode: navigate；网关据此决定要不要把 401 换成
// 自愈页面。测试必须显式带上这个头，否则会被当成 XHR。
func (b *browser) getNavigate(path string) *http.Response {
	b.t.Helper()
	request, err := http.NewRequest(http.MethodGet, b.base+path, nil)
	if err != nil {
		b.t.Fatalf("构造请求失败: %v", err)
	}
	request.Host = gateHost
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	b.applyCookies(request)
	response, err := b.client.Do(request)
	if err != nil {
		b.t.Fatalf("请求失败: %v", err)
	}
	b.absorb(response)
	return response
}

// post 发起一次 POST（带表单）。
func (b *browser) post(path, body string) *http.Response {
	b.t.Helper()
	request, err := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatalf("构造请求失败: %v", err)
	}
	request.Host = gateHost
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b.applyCookies(request)
	response, err := b.client.Do(request)
	if err != nil {
		b.t.Fatalf("请求失败: %v", err)
	}
	b.absorb(response)
	return response
}

func (b *browser) applyCookies(request *http.Request) {
	for name, value := range b.cookies {
		request.AddCookie(&http.Cookie{Name: name, Value: value})
	}
}

func (b *browser) absorb(response *http.Response) {
	for _, item := range response.Cookies() {
		// Go 的 Cookie.String() 会把「删除」编码成 Max-Age=0 + 过去的 Expires，
		// 解析回来 MaxAge 就是 0，因此删除条件必须把 0 也算进去。
		if item.MaxAge <= 0 && item.Value == "" {
			delete(b.cookies, item.Name)
			continue
		}
		b.cookies[item.Name] = item.Value
	}
}

func (b *browser) body(response *http.Response) string {
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		b.t.Fatalf("读取响应体失败: %v", err)
	}
	return string(raw)
}

func (b *browser) has(name string) bool {
	_, ok := b.cookies[name]
	return ok
}

// TestTokenExchange 是本项目最重要的一条测试：完整走一遍令牌交接。
//
// 覆盖了四件事，缺一不可：
//  1. 第一次访问 / 时网关自动补上令牌（用户什么都不用知道）；
//  2. dsh 的会话 Cookie 与 authority 绑定，且网关原样转发了 Host；
//  3. 交接过一次之后不再重复注入——否则会被 302 成死循环；
//  4. 会话失效后能自愈：401 → 清除标记 → 回到 / 重新交接。
func TestTokenExchange(t *testing.T) {
	dsh, _, gateway := newGate(t)
	client := newBrowser(t, gateway)

	// --- 第一步：裸着访问 /，网关应当自动完成令牌交换 ---
	first := client.get("/")
	if first.StatusCode != http.StatusFound {
		t.Fatalf("第一次访问 / 应得到 dsh 的 302 重定向，实际是 %d", first.StatusCode)
	}
	if got := dsh.Exchanges(); got != 1 {
		t.Fatalf("网关应向 dsh 发起一次令牌交换，实际 %d 次", got)
	}
	if !client.has(cookie.Handoff) {
		t.Fatalf("网关应在交接完成后种下 %q 标记", cookie.Handoff)
	}
	if !client.has("dsh.sid") {
		t.Fatal("dsh 应当已经签发了会话 Cookie")
	}

	// --- 第二步：跟着重定向再访问 /，这次不应再触发交换 ---
	second := client.get("/")
	body := client.body(second)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("第二次访问 / 应直接拿到页面，实际是 %d", second.StatusCode)
	}
	if !strings.Contains(body, "DeepSeek") {
		t.Fatalf("第二次访问没有拿到 dsh 首页，内容是：%q", body)
	}
	if got := dsh.Exchanges(); got != 1 {
		t.Fatalf("已交接过的浏览器不应再次触发令牌交换，实际累计 %d 次", got)
	}

	// --- 第三步：带着会话访问 API ---
	api := client.get("/api/session")
	if api.StatusCode != http.StatusOK {
		t.Fatalf("带着会话访问 API 应成功，实际是 %d", api.StatusCode)
	}
	client.body(api)

	// --- 第四步：会话丢失后自愈 ---
	// 模拟浏览器清掉 dsh 的 Cookie（换浏览器、换 authority、dsh 重启都会这样），
	// 只留下网关的交接标记——这是最刁钻的一种状态。
	delete(client.cookies, "dsh.sid")

	// 页面导航：网关清掉交接标记，并 303 把浏览器送回 / 重做交接。
	// （以前这里返回一张自动跳转的 HTML，现在直接用 303：少一步 JS，也不会
	// 让人停在那句「正在进入」上。）
	failed := client.getNavigate("/api/session")
	if failed.StatusCode != http.StatusSeeOther {
		t.Fatalf("会话失效时导航请求应 303 回 /，实际是 %d", failed.StatusCode)
	}
	if loc := failed.Header.Get("Location"); loc != "/" {
		t.Fatalf("重新交接应回到 /，实际 Location=%q", loc)
	}
	if client.has(cookie.Handoff) {
		t.Fatal("会话失效时网关必须清掉交接标记，否则下次无法重新交接")
	}

	// 但 XHR 必须拿到真正的 401：给前端一个 200 的 HTML 比给 401 更糟。
	xhr := client.get("/api/session")
	if xhr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("会话失效时 XHR 应当原样收到 401，实际是 %d", xhr.StatusCode)
	}
	client.body(xhr)

	recovered := client.get("/")
	if recovered.StatusCode != http.StatusFound {
		t.Fatalf("重新访问 / 应再次触发令牌交换，实际是 %d", recovered.StatusCode)
	}
	if got := dsh.Exchanges(); got != 2 {
		t.Fatalf("自愈后应恰好再交换一次，实际累计 %d 次", got)
	}
	// 自愈之后 API 必须真的能用，而不只是看起来恢复了。
	ok := client.get("/api/session")
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("自愈后访问 API 应成功，实际是 %d", ok.StatusCode)
	}
	client.body(ok)
}

// TestHostPreserved 验证网关不会把 Host 改写成监听器地址。
//
// 这是 dsh 的硬性要求：它拿 Host 与被接受的 authority 比对，同时要求 Origin
// 同源。一旦被改写，每个 API 调用都会 403，而且报错信息完全指不到真因。
func TestHostPreserved(t *testing.T) {
	dsh, _, gateway := newGate(t)
	client := newBrowser(t, gateway)

	client.get("/")
	client.get("/api/session")

	// 直接问 dsh：它收到的会话 Cookie 是按哪个 authority 签的。
	// 如果网关改了 Host，签发 audience 就会是上游地址而不是 gate.test。
	response := client.get("/api/whoami")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("API 调用失败: %d", response.StatusCode)
	}
	if got := dsh.APICalls(); got == 0 {
		t.Fatal("请求没有到达 dsh")
	}
}

// TestUntrustedHostRejected 验证 dsh 的 Host 栅栏在网关后面依然生效。
//
// 它能通过，说明网关确实把原始 Host 递了过去，而不是「顺手帮 dsh 放行」。
func TestUntrustedHostRejected(t *testing.T) {
	_, _, gateway := newGate(t)

	request, err := http.NewRequest(http.MethodGet, gateway+"/api/session", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Host = "evil.test" // 不在 dsh 的 trustedHosts 里
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("未点名的 authority 应当被 dsh 拒绝，实际是 %d", response.StatusCode)
	}
}

// TestIndexRedirect 验证直接访问 /index.html 会被导回 /。
//
// dsh 的令牌交换只发生在 GET /，所以从 /index.html 进来会「页面能开、API 全 401」。
// 网关必须把这条路径也拉回正确的入口。
func TestIndexRedirect(t *testing.T) {
	dsh, _, gateway := newGate(t)
	client := newBrowser(t, gateway)

	response := client.get("/index.html")
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("访问 /index.html 应当 303 到 /，实际是 %d", response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != "/" {
		t.Fatalf("跳转目标应当是 /，实际是 %q", got)
	}
	client.body(response)
	if got := dsh.Exchanges(); got != 0 {
		t.Fatalf("这一步不应触发令牌交换，实际 %d 次", got)
	}
}

// TestLearnTokenFromURL 验证「把 dsh 打印的地址粘到网关后面」这条自救路径。
//
// 接管别人已经启动的 dsh 时，网关读不到它的 stdout，这是唯一能拿到令牌的途径。
func TestLearnTokenFromURL(t *testing.T) {
	dsh, tokens, gateway := newGate(t)
	tokens.SetToken("") // 模拟：接管了一个网关并不掌握的令牌

	client := newBrowser(t, gateway)
	response := client.get("/?token=" + dsh.Token)
	client.body(response)
	if response.StatusCode != http.StatusFound {
		t.Fatalf("带着正确令牌访问应当完成交换，实际是 %d", response.StatusCode)
	}
	if tokens.Token() != dsh.Token {
		t.Fatalf("网关应当从 URL 里学到令牌，实际学到 %q", tokens.Token())
	}
}

// TestWaitingPage 验证 dsh 未就绪时给的是「正在启动」页而不是 502。
func TestWaitingPage(t *testing.T) {
	upstreamDSH := dshtest.Start(gateHost)
	defer upstreamDSH.Close()

	upstream, _ := url.Parse(upstreamDSH.URL())
	handler, err := proxy.New(proxy.Options{
		Upstream: upstream,
		Tokens:   &fakeTokens{token: upstreamDSH.Token},
		Ready:    func() bool { return false },
		Status:   func() string { return "插件安装失败" },
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	response, err := http.Get(gateway.URL)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dsh 未就绪时应当返回 503，实际是 %d", response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	page := string(raw)
	if !strings.Contains(page, "DSH 正在启动") {
		t.Fatalf("应当返回等待启动页面，实际内容：%q", truncate(page, 200))
	}
	if !strings.Contains(page, "插件安装失败") {
		t.Fatalf("等待页面应当显示失败原因，实际内容：%q", truncate(page, 400))
	}
}

// TestUpstreamDown 验证 dsh 彻底连不上时的兜底页面。
func TestUpstreamDown(t *testing.T) {
	upstream, _ := url.Parse("http://127.0.0.1:1") // 保留端口，必定连不上
	handler, err := proxy.New(proxy.Options{
		Upstream: upstream,
		Tokens:   &fakeTokens{},
		Ready:    func() bool { return true },
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	response, err := http.Get(gateway.URL + "/api/session")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游连不上时应当返回 502，实际是 %d", response.StatusCode)
	}
	raw, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(raw), "暂时不可用") {
		t.Fatalf("应当给出可读的错误页面，实际是 %q", truncate(string(raw), 200))
	}
}

// truncate 截断字符串，便于断言失败时打印。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("...（共 %d 字节）", len(s))
}

// newFencingGate 起一个「像真实 dsh 那样对 / 也把关」的上游。
//
// dshtest 的首页刻意是匿名可访问的（身份探测探针要用它），而真实的 dsh 在
// dsh-client-connection 里对所有请求先过一道浏览器信任栅栏：信任 Cookie 与当前
// authority 不匹配就回 401，正文正是
// 「dsh web authentication required; reopen the URL printed by dsh web.」。
// 用户在网关后面看到的就是这一句，所以这条路径必须照着真实行为来测。
// 返回的两个令牌来源分别代表「上游 dsh 认哪个令牌」与「网关手上是哪个令牌」：
// 两者刻意分开，否则「网关拿错令牌」这种状态根本造不出来。
func newFencingGate(t *testing.T, token string) (upstreamTokens, gatewayTokens *fakeTokens, gatewayURL string) {
	t.Helper()
	upstreamTokens = &fakeTokens{token: token}
	gatewayTokens = &fakeTokens{token: token}

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trusted := func() bool {
			c, err := r.Cookie("dsh.sid")
			return err == nil && c.Value == "session-"+r.Host
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			if got := r.URL.Query().Get("token"); got != "" {
				if got != upstreamTokens.Token() {
					http.Error(w, "unauthorized: invalid startup token", http.StatusUnauthorized)
					return
				}
				http.SetCookie(w, &http.Cookie{Name: "dsh.sid", Value: "session-" + r.Host, Path: "/"})
				http.Redirect(w, r, "./", http.StatusFound)
				return
			}
		}
		if !trusted() {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "dsh web authentication required; reopen the URL printed by dsh web.\n")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html><head><title>DeepSeek Harness</title></head></html>")
	})

	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("解析上游地址失败: %v", err)
	}
	handler, err := proxy.New(proxy.Options{
		Upstream: parsed,
		Tokens:   gatewayTokens,
		Ready:    func() bool { return true },
	})
	if err != nil {
		t.Fatalf("构造代理失败: %v", err)
	}
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	return upstreamTokens, gatewayTokens, gateway.URL
}

// TestStaleSessionOnRootSelfHeals 是实测踩出来的回归：网关登录之后浏览器被送回
// 的是 `/` 本身，而它手上的 dsh 会话已经作废（dsh 重启、会话过期、换了访问地址
// 都会这样）。真实 dsh 会回 401 加那句
// 「dsh web authentication required; reopen the URL printed by dsh web.」，
// 而以前网关特意放过 `/`，于是这句话被原样透给用户，刷新与重新登录都没用。
func TestStaleSessionOnRootSelfHeals(t *testing.T) {
	_, _, gateway := newFencingGate(t, "token-a")
	client := newBrowser(t, gateway)

	// 先正常交接一次。
	first := client.get("/")
	if first.StatusCode != http.StatusFound {
		t.Fatalf("第一次访问 / 应触发令牌交换，实际是 %d", first.StatusCode)
	}
	client.body(first)
	if !client.has(cookie.Handoff) {
		t.Fatal("交接成功后应种下标记")
	}

	// 会话作废（标记还在——浏览器不会自己清）。
	client.cookies["dsh.sid"] = "stale"

	// 导航到 / 本身：必须 303 回 / 并清掉标记。
	reentry := client.getNavigate("/")
	if reentry.StatusCode != http.StatusSeeOther {
		t.Fatalf("会话失效时访问 / 应 303 重新交接，实际是 %d（原样透传就是用户看到的那句英文报错）", reentry.StatusCode)
	}
	if loc := reentry.Header.Get("Location"); loc != "/" {
		t.Fatalf("Location = %q，期望 /", loc)
	}
	if client.has(cookie.Handoff) {
		t.Fatal("自愈时必须清掉交接标记")
	}
	client.body(reentry)

	// 跟着重定向：这次没有标记，网关补令牌 → dsh 302 → 会话回来。
	again := client.get(reentry.Header.Get("Location"))
	if again.StatusCode != http.StatusFound {
		t.Fatalf("重新交接应触发令牌交换，实际是 %d", again.StatusCode)
	}
	client.body(again)

	home := client.get("/")
	if home.StatusCode != http.StatusOK {
		t.Fatalf("自愈后访问 / 应正常，实际是 %d", home.StatusCode)
	}
	if body := client.body(home); !strings.Contains(body, "DeepSeek") {
		t.Fatalf("自愈后应拿到 dsh 首页，内容：%q", truncate(body, 200))
	}
}

// TestDSHRestartTriggersFreshExchange 锁住标记里的令牌指纹：dsh 每次启动都会换
// 令牌，浏览器手里的旧标记必须被认出来「这已经是上一代 dsh 的会话了」，于是下次
// 打开 / 直接重新交接一次——而不是先吃到 401 再靠自愈兜。
func TestDSHRestartTriggersFreshExchange(t *testing.T) {
	upstream, gatewayTokens, gateway := newFencingGate(t, "token-a")
	client := newBrowser(t, gateway)

	first := client.get("/")
	if first.StatusCode != http.StatusFound {
		t.Fatalf("第一次访问 / 应触发令牌交换，实际是 %d", first.StatusCode)
	}
	client.body(first)
	staleMarker := client.cookies[cookie.Handoff]
	if staleMarker == "" {
		t.Fatal("交接成功后应有标记")
	}

	// dsh 重启：上游与网关都换成新令牌；浏览器什么都没变（旧会话、旧标记）。
	upstream.SetToken("token-b")
	gatewayTokens.SetToken("token-b")

	again := client.getNavigate("/")
	if again.StatusCode != http.StatusFound {
		t.Fatalf("dsh 换令牌后应直接重新交接（302），实际是 %d", again.StatusCode)
	}
	client.body(again)
	if client.cookies[cookie.Handoff] == staleMarker {
		t.Fatal("重新交接后标记应当换成新令牌的指纹")
	}

	home := client.get("/")
	if home.StatusCode != http.StatusOK {
		t.Fatalf("重新交接后访问 / 应正常，实际是 %d", home.StatusCode)
	}
	client.body(home)
}

// TestAuthRequiredPageInsteadOfRedirectLoop 没有交接标记却还是 401 时不能再跳一次：
// 那会和 dsh 的 401 来回打转。这时给一张说明页（200），写清怎么人工恢复；而且
// **失败的交接绝不能种标记**，否则这台浏览器会被永久标成「已交接」。
func TestAuthRequiredPageInsteadOfRedirectLoop(t *testing.T) {
	_, gatewayTokens, gateway := newFencingGate(t, "token-a")
	client := newBrowser(t, gateway)

	// 网关手上的令牌是错的（比如接管了外部 dsh 又没拿到令牌）。
	gatewayTokens.SetToken("wrong-token")

	resp := client.getNavigate("/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("无法交接时应给出说明页（200），实际是 %d", resp.StatusCode)
	}
	body := client.body(resp)
	if !strings.Contains(body, "重新登录") {
		t.Fatalf("说明页应讲清需要重新登录/恢复令牌，实际：%q", truncate(body, 200))
	}
	if strings.Contains(body, "location.replace") {
		t.Fatal("说明页不能自动跳转，否则会和 dsh 的 401 来回打转")
	}
	if client.has(cookie.Handoff) {
		t.Fatal("交接失败却种下了标记：这台浏览器会永远卡在 401")
	}
}
