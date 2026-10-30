// Package config 定义 dsh-gate 的配置模型，并负责「默认值 → 环境变量 →
// 配置文件 → 命令行」的合并与校验。
//
// 一个 Instance 就是一个完整的「网关 + 它托管的那个 dsh」：有自己的监听
// 地址、自己的 dsh 端口、自己的实例目录（DSH_HOME）、自己的账号表。家里
// 一个「公司实例」、一个「个人实例」，就写成两个 Instance；它们共享同一个
// 二进制进程，但状态、会话、账号互不干扰。
//
// 优先级：命令行 > 环境变量 > 配置文件 > 默认值。命令行之所以最高，是因为
// 改一次配置就想立刻验证时，临时覆盖比改文件顺手。
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 默认值。集中放在这里，改行为时只动这一处。
const (
	// DefaultGatePort 是网关监听端口。刻意避开 dsh 的 3080 和常用的 80/443，
	// 也避开很多开发服务器常占的 3000/5173/8080，以及 9080 这类企业中间件
	// （WebSphere、Istio 示例服务）爱用的号段。
	DefaultGatePort = 12020

	// DefaultDSHPort 与 dsh 自己的默认端口一致（dsh web 不带 --port 时）。
	DefaultDSHPort = 3080

	// DefaultBindHost 是网关默认监听地址。dsh-gate 的存在意义就是让别的
	// 机器连进来，所以默认监听所有网卡，而不是像 dsh 那样只听 127.0.0.1。
	DefaultBindHost = "0.0.0.0"

	// DefaultDSHHost 是 dsh 默认监听地址。保持回环：dsh 自己没有面向
	// 局域网的安全设计（明文 HTTP、启动 token 打在 stdout），对外只暴露
	// 带登录页的网关这一个入口。
	DefaultDSHHost = "127.0.0.1"

	// DefaultName 是单实例运行时的默认实例名，用于日志前缀与状态目录。
	DefaultName = "default"

	// DefaultStateDirName 是网关自身状态目录的默认位置。
	//
	// 刻意**不**放在 dsh 的实例目录里：那个目录是 dsh 的数据（会话、插件、
	// 配置），往里塞网关的账号表会让两者的备份、清理、迁移互相牵扯。
	DefaultStateDirName = "~/.dsh-gate"

	// DefaultSessionTTL 是网关会话 cookie 的有效期。dsh 自己的会话是
	// 30 天，网关没必要比它短太多。
	DefaultSessionTTL = 7 * 24 * time.Hour

	// DefaultMaxLoginFails 是同一 IP 连续失败多少次后开始锁定。
	DefaultMaxLoginFails = 5

	// DefaultLockoutBase 是锁定时长的基准，每次失败叠加一倍（有上限）。
	DefaultLockoutBase = time.Minute

	// MaxLockout 是锁定时长的上限。
	MaxLockout = 15 * time.Minute

	// DefaultDSHStartTimeout 是拉起 dsh 后等待端口就绪的最长时间。
	//
	// 给到 5 分钟而不是更短：首次启动要由 npx 把 dsh 包与插件全部下载完才会
	// 监听端口，慢网络远超 90 秒；而等待超时那一刻网关会把正在下载的子进程杀掉，
	// 下一轮从头再来——预算比下载本身还短时，无论重试多少次都起不来。等待页的
	// 文案写着「首次启动可能需要几分钟」，这里的预算得跟它一致。
	DefaultDSHStartTimeout = 5 * time.Minute

	// DefaultDSHStopTimeout 是停止 dsh 时等待优雅退出的时间，超时则强杀。
	DefaultDSHStopTimeout = 10 * time.Second
)

// Instance 是一个网关实例的完整配置。
//
// json tag 对应多实例配置文件里的字段名；配置文件读进来时会先填默认值再
// 反序列化，因此**没写的字段保持默认**，用户不需要把整份配置抄全。
type Instance struct {
	// Name 实例名。多实例运行时用于日志前缀、systemd 服务名与状态行标识。
	Name string `json:"name"`

	// Host / Port 是网关（对外入口）的监听地址。
	Host string `json:"host"`
	Port int    `json:"port"`

	// DSHHome 是 dsh 的实例目录，作为 DSH_HOME 传给子进程。默认 ~/.dsh，
	// 与直接跑 `dsh web` 用的是同一套数据。
	//
	// 这是「公司与家里两套 dsh 互不混淆」的唯一开关：不同的 DSH_HOME 让
	// dsh 读写完全不同的会话历史、插件与配置。代价是每套实例第一次启动
	// 都要下载插件，属于一次性成本。
	DSHHome string `json:"dshHome"`

	// DSHHost / DSHPort 是 dsh 自己的监听地址。
	//
	// DSHHost 不接受通配地址：dsh 出于安全考虑会直接拒绝 --host 0.0.0.0，
	// 这里提前拦下来并给出可操作的提示，而不是把错误留给 dsh 去报。
	DSHHost string `json:"dshHost"`
	DSHPort int    `json:"dshPort"`

	// DSHCommand 是拉起 dsh 使用的命令，支持占位符：
	//
	//	{host} {port} {home} {workspace} {url} {trusted} {noopen}
	//
	// 留空表示自动探测（先找 PATH 里的 dsh，再用 npx @deepseek-ai/dsh）。
	DSHCommand string `json:"dshCmd,omitempty"`

	// ResolvedDSHCommand 是 EnsureDSH 冒烟验证过、确认能执行的启动命令
	// （进程内部使用，不落盘、不接受用户配置）。空表示走自动探测。
	ResolvedDSHCommand string `json:"-"`

	// Workspace 是 dsh 子进程的工作目录。dsh 把启动时的当前目录当作默认
	// workspace 根目录，所以这一项直接决定「会话默认能看到哪些文件」。
	// 留空表示沿用 dsh-gate 自己的当前目录。
	Workspace string `json:"workspace,omitempty"`

	// StateDir 是网关自己的状态目录，默认 ~/.dsh-gate。
	//
	// 它与 DSHHome 是两回事：DSHHome 归 dsh，StateDir 归网关。多实例各自
	// 独立 dsh 时，共用同一个状态目录最省事——账号表可以指向不同文件。
	StateDir string `json:"stateDir,omitempty"`

	// UsersFile 是账号表路径，默认 <StateDir>/users.json。
	UsersFile string `json:"users,omitempty"`

	// PublicURL 是 dsh 对外公告的应用根，会作为 --public-url 传给 dsh，
	// 决定 dsh 打印与 agent 看到的地址。留空时由本包按监听地址推导。
	PublicURL string `json:"publicUrl,omitempty"`

	// TrustedHosts 是追加给 dsh 的 --trusted-host。dsh 只信任回环、自身
	// 绑定地址和这里点名的 authority，而反向代理的 authority（例如
	// 192.168.1.20:8080、内网穿透域名）必须显式点名，否则每个 API 调用 403。
	TrustedHosts []string `json:"trustedHosts,omitempty"`

	// TLSCert / TLSKey 让网关自己提供 HTTPS。两个要么都给，要么都不给。
	TLSCert string `json:"tlsCert,omitempty"`
	TLSKey  string `json:"tlsKey,omitempty"`

	// DSHToken 是已知的 dsh 启动令牌。通常留空——网关拉起 dsh 时会从它的
	// 启动输出里自己读到。只有「接管别人已经启动的 dsh」才需要手动填，
	// 或者把 dsh 打印的完整 URL 粘到网关地址后面让网关自己记住。
	DSHToken string `json:"dshToken,omitempty"`

	// NPMRegistry 是拉取 dsh 时用的 npm 源，会以 npm_config_registry 注入
	// npx 与 dsh 子进程。留空表示用 npm 自己的默认（含用户 ~/.npmrc 里的设置），
	// 默认源连不上时网关会自动改用国内镜像源重试一次——所以「慢」不再是死路，
	// 只有想固定用某个源时才需要显式给这一项。
	NPMRegistry string `json:"npmRegistry,omitempty"`

	// AutoStart 端口空闲时由网关拉起 dsh。关掉之后网关只做鉴权与转发，
	// dsh 得由你自己保证在跑。
	AutoStart bool `json:"autoStart"`

	// Adopt 端口已被占用时接管那个 dsh，而不试图再起一个。这正是
	// 「dsh 端口已经被占用了就直接代理用」需求的开关，默认开着。
	Adopt bool `json:"adopt"`

	// Restart dsh 意外退出后自动重新拉起（指数退避，最长 30 秒一次）。
	Restart bool `json:"restart"`

	// SessionTTL 是网关登录会话的有效期。
	SessionTTL time.Duration `json:"-"`

	// MaxLoginFails / LockoutBase 是登录失败限速参数。
	MaxLoginFails int           `json:"-"`
	LockoutBase   time.Duration `json:"-"`

	// DSHStartTimeout / DSHStopTimeout 是 dsh 生命周期相关的时间预算。
	DSHStartTimeout time.Duration `json:"-"`
	DSHStopTimeout  time.Duration `json:"-"`
}

// Default 返回一个填好默认值的实例。
func Default() Instance {
	return Instance{
		Name:            DefaultName,
		Host:            DefaultBindHost,
		Port:            DefaultGatePort,
		DSHHost:         DefaultDSHHost,
		DSHPort:         DefaultDSHPort,
		AutoStart:       true,
		Adopt:           true,
		Restart:         true,
		SessionTTL:      DefaultSessionTTL,
		MaxLoginFails:   DefaultMaxLoginFails,
		LockoutBase:     DefaultLockoutBase,
		DSHStartTimeout: DefaultDSHStartTimeout,
		DSHStopTimeout:  DefaultDSHStopTimeout,
	}
}

// DefaultStateDir 返回网关状态目录的默认位置（各平台统一放在家目录下）。
func DefaultStateDir() string {
	return ExpandPath(DefaultStateDirName)
}

// StateDirPath 返回网关自己的状态目录。
//
// Normalize 之后这个字段一定有值；这里仍留一层兜底，让手工构造的 Instance
// 也不会拿到空路径。
func (i *Instance) StateDirPath() string {
	if i.StateDir == "" {
		return DefaultStateDir()
	}
	return i.StateDir
}

// ListenAddr 返回网关监听地址，形如 0.0.0.0:12020。
func (i *Instance) ListenAddr() string {
	return net.JoinHostPort(i.Host, strconv.Itoa(i.Port))
}

// DSHAddr 返回 dsh 的监听地址，形如 127.0.0.1:3080。
func (i *Instance) DSHAddr() string {
	return net.JoinHostPort(i.DSHHost, strconv.Itoa(i.DSHPort))
}

// UpstreamURL 返回反向代理的上游地址。
func (i *Instance) UpstreamURL() string {
	return "http://" + i.DSHAddr()
}

// TLSEnabled 表示是否以 HTTPS 对外服务。
func (i *Instance) TLSEnabled() bool {
	return i.TLSCert != "" && i.TLSKey != ""
}

// Scheme 返回对外协议。
func (i *Instance) Scheme() string {
	if i.TLSEnabled() {
		return "https"
	}
	return "http"
}

// Normalize 展开 `~`、补齐派生字段，然后校验。
//
// 顺序是刻意的：先展开路径（后面的校验要用到绝对路径），再推导默认值
// （如账号表路径依赖 DSHHome），最后校验。
func (i *Instance) Normalize() error {
	i.Name = strings.TrimSpace(i.Name)
	i.DSHCommand = strings.TrimSpace(i.DSHCommand)
	i.TLSCert = ExpandPath(i.TLSCert)
	i.TLSKey = ExpandPath(i.TLSKey)
	i.Workspace = ExpandPath(i.Workspace)
	i.StateDir = ExpandPath(strings.TrimSpace(i.StateDir))

	if i.DSHHome == "" {
		i.DSHHome = ExpandPath("~/.dsh")
	} else {
		i.DSHHome = ExpandPath(i.DSHHome)
	}
	if i.StateDir == "" {
		i.StateDir = DefaultStateDir()
	}
	if i.UsersFile == "" {
		i.UsersFile = filepath.Join(i.StateDir, "users.json")
	} else {
		i.UsersFile = ExpandPath(i.UsersFile)
	}
	if i.PublicURL != "" {
		i.PublicURL = strings.TrimRight(strings.TrimSpace(i.PublicURL), "/")
		if i.PublicURL != "" {
			i.PublicURL += "/"
		}
	}
	if i.NPMRegistry != "" {
		i.NPMRegistry = strings.TrimRight(strings.TrimSpace(i.NPMRegistry), "/")
	}
	return i.Validate()
}

// Validate 检查配置是否自洽。启动前调用，把所有能提前发现的错误变成一段
// 可执行的提示，而不是等第一个请求打进来才 502。
func (i *Instance) Validate() error {
	if i.Name == "" {
		return errors.New("实例名不能为空")
	}
	if len(i.Name) > 32 {
		return errors.New("实例名过长（最多 32 个字符）")
	}
	for _, r := range i.Name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("实例名 %q 含非法字符（只允许字母、数字、- 和 _）", i.Name)
		}
	}
	if i.Host == "" {
		return errors.New("监听地址不能为空（--host）")
	}
	if strings.ContainsAny(i.Host, " \t") {
		return fmt.Errorf("监听地址 %q 含空白字符", i.Host)
	}
	if err := checkPort("网关端口", i.Port); err != nil {
		return err
	}
	if i.Port == i.DSHPort && i.DSHHost == i.Host {
		return fmt.Errorf("网关端口与 dsh 端口都是 %d，会自己撞自己", i.Port)
	}
	if err := checkPort("dsh 端口", i.DSHPort); err != nil {
		return err
	}
	if err := checkDSHHost(i.DSHHost); err != nil {
		return err
	}
	if i.DSHHome == "" {
		return errors.New("dsh 实例目录不能为空（--dsh-home）")
	}
	if !filepath.IsAbs(i.DSHHome) {
		return fmt.Errorf("dsh 实例目录 %q 必须是绝对路径", i.DSHHome)
	}
	if i.StateDir == "" || !filepath.IsAbs(i.StateDir) {
		return fmt.Errorf("网关状态目录 %q 必须是绝对路径", i.StateDir)
	}
	if i.UsersFile == "" {
		return errors.New("账号表路径不能为空")
	}
	if !filepath.IsAbs(i.UsersFile) {
		return fmt.Errorf("账号表路径 %q 必须是绝对路径", i.UsersFile)
	}
	if i.Workspace != "" && !filepath.IsAbs(i.Workspace) {
		return fmt.Errorf("工作目录 %q 必须是绝对路径", i.Workspace)
	}
	if i.PublicURL != "" {
		parsed, err := url.Parse(i.PublicURL)
		if err != nil {
			return fmt.Errorf("公开地址 %q 无法解析: %w", i.PublicURL, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("公开地址 %q 的协议必须是 http 或 https", i.PublicURL)
		}
		if parsed.Host == "" {
			return fmt.Errorf("公开地址 %q 缺少主机部分", i.PublicURL)
		}
	}
	for _, host := range i.TrustedHosts {
		if strings.TrimSpace(host) == "" {
			return errors.New("--trusted-host 不能有空值")
		}
	}
	if i.NPMRegistry != "" {
		parsed, err := url.Parse(i.NPMRegistry)
		if err != nil {
			return fmt.Errorf("npm 源 %q 无法解析: %w", i.NPMRegistry, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("npm 源 %q 的协议必须是 http 或 https", i.NPMRegistry)
		}
		if parsed.Host == "" {
			return fmt.Errorf("npm 源 %q 缺少主机部分", i.NPMRegistry)
		}
	}
	if (i.TLSCert == "") != (i.TLSKey == "") {
		return errors.New("--tls-cert 与 --tls-key 必须成对提供（或都不提供）")
	}
	if i.TLSEnabled() {
		for label, path := range map[string]string{"--tls-cert": i.TLSCert, "--tls-key": i.TLSKey} {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("%s 指定的文件不可读 %q: %w", label, path, err)
			}
		}
	}
	if i.SessionTTL <= 0 {
		return errors.New("会话有效期必须为正")
	}
	if i.MaxLoginFails < 1 {
		return errors.New("最大登录失败次数至少为 1")
	}
	if i.LockoutBase <= 0 || i.LockoutBase > MaxLockout {
		return fmt.Errorf("锁定基准时长必须在 0 与 %s 之间", MaxLockout)
	}
	if i.DSHStartTimeout <= 0 {
		return errors.New("dsh 启动等待时间必须为正")
	}
	if i.DSHStopTimeout <= 0 {
		return errors.New("dsh 停止等待时间必须为正")
	}
	return nil
}

// checkPort 校验端口号。
//
// 允许 0：它表示「由系统分配一个空闲端口」，测试与「临时起一个实例」这类场景
// 正好需要，不必先去问内核要一个端口再祈祷它没被别人抢走。
func checkPort(label string, port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("%s %d 超出范围（0-65535，0 表示由系统分配）", label, port)
	}
	return nil
}

// checkDSHHost 提前拦住 dsh 自己会拒绝的取值，并把原因讲清楚。
//
// dsh 出于安全考虑不接受通配监听地址（`--host 0.0.0.0` 直接是用法错误），
// 这条限制在文档里只有一句话，在这里展开成能照着改的提示。
func checkDSHHost(host string) error {
	if host == "" {
		return errors.New("dsh 监听地址不能为空（--dsh-host）")
	}
	if strings.ContainsAny(host, " \t") {
		return fmt.Errorf("dsh 监听地址 %q 含空白字符", host)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// 交给系统解析的主机名（如 localhost）一律放行。
		return nil
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("dsh 不接受通配监听地址 %q：dsh 只允许绑定某一个具体地址，"+
			"默认 127.0.0.1 即可（对外访问交给 dsh-gate 的网关端口）", host)
	}
	return nil
}

// ExpandPath 展开路径开头的 `~`（当前用户主目录）。
//
// 其余路径原样返回：Windows 上的 `%USERPROFILE%` 之类交给系统自己展开，
// 我们不重复发明一套环境变量语法。
func ExpandPath(path string) string {
	if path == "" {
		return ""
	}
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// Apply 把「命令行显式给出的值」覆盖到实例配置上。
//
// overrides 的键就是命令行选项名（去掉前导 --）。之所以传 map 而不是
// 逐个 setter：flag 包能通过 Visit 精确告诉我们哪些选项是用户显式写的，
// 没写的绝不能覆盖——否则默认值就永远没有出头之日。
func Apply(dst *Instance, overrides map[string]string) error {
	for key, raw := range overrides {
		value := strings.TrimSpace(raw)
		switch key {
		case "name":
			dst.Name = value
		case "host":
			dst.Host = value
		case "port":
			port, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("--port 需要一个端口号，收到 %q", raw)
			}
			dst.Port = port
		case "dsh-host":
			dst.DSHHost = value
		case "dsh-port":
			port, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("--dsh-port 需要一个端口号，收到 %q", raw)
			}
			dst.DSHPort = port
		case "dsh-home":
			dst.DSHHome = ExpandPath(value)
		case "dsh-cmd":
			dst.DSHCommand = value
		case "workspace":
			dst.Workspace = ExpandPath(value)
		case "state-dir":
			dst.StateDir = ExpandPath(value)
		case "users":
			dst.UsersFile = ExpandPath(value)
		case "public-url":
			dst.PublicURL = value
		case "trusted-host":
			// 可重复：同名的多个 --trusted-host 累积成一个列表。
			for _, item := range strings.Split(raw, ",") {
				if item = strings.TrimSpace(item); item != "" {
					dst.TrustedHosts = append(dst.TrustedHosts, item)
				}
			}
		case "tls-cert":
			dst.TLSCert = ExpandPath(value)
		case "tls-key":
			dst.TLSKey = ExpandPath(value)
		case "dsh-token":
			dst.DSHToken = value
		case "npm-registry":
			dst.NPMRegistry = value
		case "auto-start":
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("--auto-start 需要 true 或 false，收到 %q", raw)
			}
			dst.AutoStart = enabled
		case "adopt":
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("--adopt 需要 true 或 false，收到 %q", raw)
			}
			dst.Adopt = enabled
		case "restart":
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return fmt.Errorf("--restart 需要 true 或 false，收到 %q", raw)
			}
			dst.Restart = enabled
		case "session-ttl":
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("--session-ttl 需要一个时长（如 168h），收到 %q", raw)
			}
			dst.SessionTTL = d
		case "max-login-fails":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("--max-login-fails 需要一个整数，收到 %q", raw)
			}
			dst.MaxLoginFails = n
		case "lockout":
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("--lockout 需要一个时长（如 1m），收到 %q", raw)
			}
			dst.LockoutBase = d
		case "dsh-start-timeout":
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("--dsh-start-timeout 需要一个时长，收到 %q", raw)
			}
			dst.DSHStartTimeout = d
		default:
			return fmt.Errorf("内部错误：未知配置项 %q", key)
		}
	}
	return nil
}

// ApplyEnv 用环境变量填充**尚未设置**的配置项。
//
// 环境变量的定位是「让 systemd 单元、Docker 容器、临时 shell 也能不改
// 命令行就配置好网关」，因此它的优先级低于命令行、低于配置文件。
func ApplyEnv(dst *Instance, getenv func(string) string) error {
	// 逐项写清楚，而不是遍历反射：新增一项时编译器会提醒你补上，
	// 反射遍历则会在某天静默漏掉一个环境变量。
	if v, ok := lookup(getenv, "DSH_GATE_NAME"); ok {
		dst.Name = v
	}
	if v, ok := lookup(getenv, "DSH_GATE_HOST"); ok {
		dst.Host = v
	}
	if v, ok := lookup(getenv, "DSH_GATE_PORT"); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_PORT 需要一个端口号，收到 %q", v)
		}
		dst.Port = port
	}
	if v, ok := lookup(getenv, "DSH_GATE_DSH_HOST"); ok {
		dst.DSHHost = v
	}
	if v, ok := lookup(getenv, "DSH_GATE_DSH_PORT"); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_DSH_PORT 需要一个端口号，收到 %q", v)
		}
		dst.DSHPort = port
	}
	if v, ok := lookup(getenv, "DSH_GATE_DSH_HOME"); ok {
		dst.DSHHome = ExpandPath(v)
	}
	if v, ok := lookup(getenv, "DSH_GATE_DSH_CMD"); ok {
		dst.DSHCommand = v
	}
	if v, ok := lookup(getenv, "DSH_GATE_DSH_TOKEN"); ok {
		dst.DSHToken = v
	}
	if v, ok := lookup(getenv, "DSH_GATE_NPM_REGISTRY"); ok {
		dst.NPMRegistry = v
	}
	if v, ok := lookup(getenv, "DSH_GATE_WORKSPACE"); ok {
		dst.Workspace = ExpandPath(v)
	}
	if v, ok := lookup(getenv, "DSH_GATE_STATE_DIR"); ok {
		dst.StateDir = ExpandPath(v)
	}
	if v, ok := lookup(getenv, "DSH_GATE_USERS"); ok {
		dst.UsersFile = ExpandPath(v)
	}
	if v, ok := lookup(getenv, "DSH_GATE_PUBLIC_URL"); ok {
		dst.PublicURL = v
	}
	if v, ok := lookup(getenv, "DSH_GATE_TRUSTED_HOSTS"); ok {
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				dst.TrustedHosts = append(dst.TrustedHosts, item)
			}
		}
	}
	if v, ok := lookup(getenv, "DSH_GATE_TLS_CERT"); ok {
		dst.TLSCert = ExpandPath(v)
	}
	if v, ok := lookup(getenv, "DSH_GATE_TLS_KEY"); ok {
		dst.TLSKey = ExpandPath(v)
	}
	if v, ok := lookup(getenv, "DSH_GATE_AUTOSTART"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_AUTOSTART 需要 true 或 false，收到 %q", v)
		}
		dst.AutoStart = enabled
	}
	if v, ok := lookup(getenv, "DSH_GATE_ADOPT"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_ADOPT 需要 true 或 false，收到 %q", v)
		}
		dst.Adopt = enabled
	}
	if v, ok := lookup(getenv, "DSH_GATE_RESTART"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_RESTART 需要 true 或 false，收到 %q", v)
		}
		dst.Restart = enabled
	}
	if v, ok := lookup(getenv, "DSH_GATE_SESSION_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_SESSION_TTL 需要一个时长，收到 %q", v)
		}
		dst.SessionTTL = d
	}
	if v, ok := lookup(getenv, "DSH_GATE_MAX_LOGIN_FAILS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_MAX_LOGIN_FAILS 需要一个整数，收到 %q", v)
		}
		dst.MaxLoginFails = n
	}
	if v, ok := lookup(getenv, "DSH_GATE_LOCKOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_LOCKOUT 需要一个时长，收到 %q", v)
		}
		dst.LockoutBase = d
	}
	if v, ok := lookup(getenv, "DSH_GATE_DSH_START_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DSH_GATE_DSH_START_TIMEOUT 需要一个时长（如 10m），收到 %q", v)
		}
		dst.DSHStartTimeout = d
	}
	return nil
}

// lookup 取环境变量并去掉首尾空白；未设置或只有空白时返回 ok=false。
func lookup(getenv func(string) string, key string) (string, bool) {
	if getenv == nil {
		getenv = os.Getenv
	}
	raw := getenv(key)
	if raw == "" {
		return "", false
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false
	}
	return value, true
}
