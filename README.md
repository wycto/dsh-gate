# dsh-gate

**给 [DeepSeek Harness](https://deepseek-harness.github.io/deepseek-harness/guide/quickstart) 加一道局域网 / 异地访问网关。**

单二进制、零第三方依赖的 Go 程序。它在一个局域网可达的端口上提供登录页，鉴权通过后把请求反向代理到本机的 dsh；dsh 还没起来时，它会顺手把 dsh 拉起来。

![dsh-gate 登录页（深色）](docs/images/login.png)

<sub>手机宽度与浅色模式的效果见 [30 秒上手](#30-秒上手) 末尾。</sub>

English version: [README.en.md](README.en.md)

---

## 目录

- [它解决什么问题](#它解决什么问题)
- [它是怎么工作的](#它是怎么工作的)
- [它到底做了什么](#它到底做了什么)
- [30 秒上手](#30-秒上手)
- [使用教程](#使用教程)
- [安装](#安装)
- [命令参考](#命令参考)
  - [dsh-gate setup](#dsh-gate-setup)
  - [dsh-gate run](#dsh-gate-run)
  - [dsh-gate status](#dsh-gate-status)
  - [dsh-gate stop](#dsh-gate-stop)
  - [dsh-gate restart](#dsh-gate-restart)
  - [dsh-gate user](#dsh-gate-user)
  - [dsh-gate check](#dsh-gate-check)
  - [dsh-gate service](#dsh-gate-service)
  - [dsh-gate uninstall](#dsh-gate-uninstall)
  - [dsh-gate version](#dsh-gate-version)
- [多实例](#多实例)
- [服务器部署（Linux + systemd）](#服务器部署linux--systemd)
- [异地访问（人在家里用公司的 dsh）](#异地访问人在家里用公司的-dsh)
- [安全说明](#安全说明)
- [环境变量](#环境变量)
- [常见问题](#常见问题)
- [贡献](#贡献)
- [License](#license)

---

## 它解决什么问题

dsh 自带的 Web UI **只监听 `127.0.0.1`**。这不是疏忽，是它的安全设计：dsh 的鉴权手段是每次启动生成一个随机令牌、打印在 stdout 上，只有能在那台机器上看到输出的人才拿得到。也就是说，dsh 从来没打算被别的机器直接访问。

结果就是：

- 一台无界面的 Linux 服务器上装了 dsh，你只能 SSH 上去用；
- 家里 macOS 上跑的 dsh，公司里的 Windows 电脑连不上；
- 手机浏览器打不开 `127.0.0.1`。

dsh-gate 处理的就是这几件事。四条典型场景：

1. **无界面 Linux 服务器**：服务器上装 dsh 和 dsh-gate，任意电脑或手机浏览器打开 `http://服务器IP:3082` 就能用，不需要装任何客户端。
2. **家里 macOS 跑 dsh、公司 Windows 上继续用**（反过来也一样）：网关装在跑 dsh 的那台机器上，另一台机器用浏览器访问。
3. **手机浏览器**：同一个地址，手机直接打开就能用。
4. **一人两套 dsh**：公司和个人各一套，会话、插件、配置完全分开，各占一个端口。

dsh-gate 不改 dsh 的任何一行代码，也不要求你改 dsh 的启动方式。它只是站在 dsh 前面，多做一次「你是谁」的检查。

---

## 它是怎么工作的

```
   局域网 / 互联网上的任意设备
              |
              |  http://192.168.1.20:3082/
              v
   +---------------------------+
   |   dsh-gate  :3082         |   <- 对外入口，有登录页
   |  （0.0.0.0 监听）          |
   +---------------------------+
       |            |
  未登录 |            | 已登录
       |            |
       v            v
   登录页        +-----------------------------+
   口令校验      |  反向代理                   |
   按 IP 限速    |  Host 头原样转发            |
                 |  替浏览器交换 dsh 启动令牌  |
                 +-----------------------------+
                          |
                          |  http://127.0.0.1:3080/
                          v
                  +-------------------+
                  |   dsh   :3080     |  <- 仍然只听 127.0.0.1
                  | （DSH_HOME 决定）  |     局域网其它设备访问不到
                  +-------------------+
```

四个要点：

- **dsh 始终只听 `127.0.0.1`**。对外只有一个入口，就是网关端口。dsh 出于安全考虑会直接拒绝 `--host 0.0.0.0`，dsh-gate 也不会去改这个默认值。
- **端口空闲时网关会把 dsh 拉起来**，端口已经被 dsh 占着时**直接接管**那个实例。你不需要关心当前是哪种情况。
- **登录页是网关自己的**，用户只需要一个自己设定的账号口令，跟 dsh 的启动令牌无关。
- **多实例**：不同的 `--dsh-home` 就是两套互不干扰的 dsh，各占一个网关端口。

### dsh 端口的三条路径

dsh-gate 启动时会探一下 dsh 的端口，然后走三条路径中的一条：

| 端口状态 | 行为 |
| --- | --- |
| 空闲，且 `--auto-start=true`（默认） | 启动 dsh 子进程，等它监听成功后开始转发 |
| 已被占用，且是 dsh | 直接接管那个实例，不重复启动（最常见的情况：你已经自己在跑 dsh） |
| 已被占用，但不是 dsh | 默认按接管处理并在日志里明确告警；加 `--adopt=false` 则直接报错退出 |

dsh 意外退出后，网关会按 1 秒起步的指数退避自动重新拉起（最长 30 秒一次）。

---

## 它到底做了什么

dsh 自己有一套认证：每个进程生成一个随机**启动令牌**，启动时打印在 `dsh web:` 那一行 URL 的 `?token=` 上。浏览器访问 `GET /?token=...` 才能换到 dsh 签发的会话 Cookie，之后每个 API 调用和 WebSocket 都校验它。

放到「网关后面、从浏览器访问」的拓扑里，这套机制有两个麻烦：

1. 令牌不能发给每一个访问者——那等于谁拿到 URL 谁能用 agent；
2. 浏览器自己拿不到令牌，于是页面能打开，但所有 API 调用都返回 401。

dsh-gate 的解法是：**令牌只留在网关进程内存里，由网关替浏览器完成交换。** 你看到的只有一个自己设定的账号口令。

完整流程：

```
1. 浏览器 GET / ，身上没有「已交接」标记
      |
      v
2. 网关给转发给 dsh 的 URL 拼上 token=...
      |
      v
3. dsh 完成交换，写下自己的会话 Cookie，302 跳回干净的 ./
      |
      v
4. 网关在这次响应里种下「已交接」标记 Cookie（dsh_gate_ds）
      |
      v
5. 之后浏览器访问 / 时带着标记，网关不再重复注入（否则会无限 302）
```

标记是必要的，不是优化：dsh 的 302 目标就是 `/` 本身，没有标记就无法区分「第一次进来」和「刚被重定向回来」。

如果 dsh 因为会话过期、或用户换了访问地址（IP ↔ 主机名 ↔ 穿透域名）而返回 401，网关会清掉标记并把浏览器送回 `/` 重新交接一次。整个过程对用户不可见，表现为「页面自己就好了」。

还有一件事必须做对：**`Host` 头要原样转发**。dsh 会用收到的 `Host` 与被接受的 authority 比对，并要求 `Origin` 指向同一 authority（防 DNS rebinding 与跨站）。所以网关不改写 `Host`，而是给 dsh 传 `--trusted-host` 放行对外地址；改写成监听器地址会让 dsh 的 Origin 校验必然失败。

令牌的生命周期被严格限制在网关进程内存里：**不落盘、不写日志、不出现在启动横幅上**。dsh-gate 转发 dsh 的启动输出时，会先把 `token=xxx` 脱敏成 `token=***` 再记进日志。

---

> 只想尽快跑起来？看一页纸的 [QUICKSTART.md](QUICKSTART.md)（也随每个发布包一起分发）。

## 30 秒上手

一条命令的走法——`setup` 会逐项询问（回车接受默认值）、最后汇总确认一遍
（答错可以重答）、建好管理员账号，并按你的选择顺带后台启动、安装开机自启：

```sh
dsh-gate setup
```

或者分步来：

```sh
# 1. 创建一个登录账号（交互输入口令，需要 Go 环境里已装好 dsh-gate）
dsh-gate user add admin

# 2. 启动网关
dsh-gate run

# 3. 浏览器打开终端里提示的地址，登录即可
```

管理它：

```sh
dsh-gate status          # 现在跑着哪几个实例、分别在哪个目录
dsh-gate stop personal   # 停掉 personal（写 all 才是全部，不写只列清单）
dsh-gate restart personal # 按原来的参数重启某个实例，日志在 ~/.dsh-gate/run/logs/
```

关掉终端还想让它继续跑，就加 `--background`（或 `-d`）；开机自动启动用
`dsh-gate service install`（Linux 用 systemd、macOS 用 launchd、Windows 用计划任务）：

```sh
dsh-gate run --background              # 后台运行，日志在 ~/.dsh-gate/run/logs/
dsh-gate service install               # 设为开机（登录）自启，并立即启动
dsh-gate service uninstall             # 只取消开机自启
dsh-gate uninstall                     # 不再用了：取消自启 + 停实例（--purge 连数据一起删）
```

第 2 步会打印这样一段启动横幅：

```
dsh-gate 0.3.13 · 共 1 个实例
实例 default 已启动
  访问地址（网关端口 3082）
  本机      http://127.0.0.1:3082/
  局域网    http://192.168.1.20:3082/
  dsh       127.0.0.1:3080（启动中，稍后会自动就绪并打印本机直连地址）
  实例目录  /Users/you/.dsh
  账号      1 个（/Users/you/.dsh-gate/users.json）
按 Ctrl-C 停止。
```

dsh 就绪后，前台运行的终端会紧接着补打一条带启动令牌的本机直连地址（绕过网关
登录用，仅本机可用）：

```
实例 default · dsh 已就绪
  本机直连  http://127.0.0.1:3080/?token=…（绕过网关，仅本机可用）
```

端口 3082 是默认值，避开 dsh 自己的 3080 和常用的 80/443、3000、5173、8080。

不想手工建账号，`--random` 可以让它随机生成一个强口令并打印一次：

```sh
dsh-gate user add admin --random
```

不确定当前状态时跑一下体检：

```sh
dsh-gate check
```

### 登录页在手机与浅色模式下的样子

<p>
  <img src="docs/images/login-mobile.png" width="300" alt="手机宽度下的登录页">
  <img src="docs/images/login-light.png" width="480" alt="浅色模式下的登录页">
</p>

登录页是**单个自包含的 HTML**（内嵌样式与脚本、无任何外部请求），因此断网的服务器
上一样能正常显示；深浅色跟随系统，最小 320px 宽不破版。


## 使用教程

这一节按**真实顺序**走一遍：从拿到一个二进制，到局域网里能用 agent 为止。
每一步都给出「你会看到什么」和「出错了看哪里」。

### 第 0 步：准备

需要的东西很少：

| 条件 | 说明 |
|---|---|
| 一台机器 | 跑 dsh 与网关的那台。Linux / macOS / Windows 都行，无界面服务器是主要场景 |
| 一个二进制 | 从 [dist](dist/) 取对应平台的包，或自己 `sh go/build.sh` 编译 |
| **不需要** Node.js | 本机没有 Node 时，网关会自己下载官方运行时（见第 2 步） |
| **不需要** 公网 IP | 局域网内用；要从外面用请见[异地访问](#异地访问人在家里用公司的-dsh) |

把二进制放到一个稳定位置。Linux/macOS 上直接装进 `PATH`：

```sh
# 解压（Windows 用 .zip，解开就是 dsh-gate.exe）
tar -xzf dsh-gate_<版本>_linux_amd64.tar.gz
cd dsh-gate_<版本>_linux_amd64

# 装进 PATH；也可以先 ./dsh-gate 用起来，第 1 步的 setup 会再帮你拷一份
sudo install -m755 dsh-gate /usr/local/bin/dsh-gate
dsh-gate version        # 确认能跑
```

> 不确定该下哪个包？`uname -m` 看架构（`x86_64` → amd64，`aarch64`/`arm64` → arm64），
> `uname -s` 看系统。Windows 上取 `windows_amd64`（或 `arm64`）。

### 第 1 步：一条命令初始化

```sh
dsh-gate setup               # 以你自己的身份跑；macOS 装开机自启并不需要 root
```

> **别用 `sudo dsh-gate setup`。** sudo 会保留 `$HOME`（macOS 就是如此），`~/.dsh`
> 仍然指向你自己的实例目录，而以 root 写出来的 `settings.yaml`、`.credentials.yaml`
> 是 root 属主（0600）——你自己的 dsh（包括 DSH 桌面端）从此读不了也改不动。
> 工具会**直接拦下**这种调用并给出两条替代做法；确实知情时用 `--allow-root-write`
> 跳过。只是想把命令装进 `PATH` 的话，先 `sudo install -m755 dsh-gate /usr/local/bin/`，
> 再以自己身份运行 `dsh-gate setup`。

它会问四件事，回车接受括号里的默认值（**大写字母就是默认值**：`[Y/n]` 回车＝是，
`[y/N]` 回车＝否）：

```
实例名 [default]: test
dsh 端口 [3080]: 12080            # dsh 自己监听哪个端口（仅本机访问）
对外端口 [3082]: 12081            # 你在浏览器里访问哪个端口
dsh 实例目录 [~/.dsh]:            # dsh 的数据（会话/插件/配置）
会话默认工作目录 [/root]: /www/wwwroot   # ★ agent 默认能读写哪个目录
确认一下: …  确认以上设置？[Y/n]
管理员用户名 [admin]: 
请输入口令:                        # 输入不回显
DeepSeek API Key（输入不回显，直接回车跳过）：
现在安装开机（登录）自启并立即启动？[y/N] y
```

两个容易踩的点：

- **端口会被当场检查**：不是数字、超出范围、两个端口填成一样、或端口已被占用，向导都会当场拒绝并
  要求重填（被占用时还会说清是哪个进程占的）。别忽略这条——等启动失败才发现，账号和开机自启都已经
  装好了，得回头重来。
- **会话默认工作目录**：agent 能读写哪些文件由它决定，**填你想要 agent 干活的那个目录**，
  别用家目录或 `/`。填错不会报错，只会让 agent 在错误的目录里干活——所以网关每次启动
  都会把实际生效的「工作目录」打在横幅上。
- **DeepSeek API Key**：可以现在填，也可以跳过（回车），之后用
  `dsh-gate model key set DEEPSEEK_API_KEY --stdin` 补。跳过的话，dsh 起来了但模型用不了。
- **目录不存在会被自动创建**：`--state-dir`、`--dsh-home`、`--workspace` 指向的目录，
  启动前会建好并打印出来（状态目录 700、工作目录 755）。所以 `mkdir` 这步可以省了。

`setup` 结束时它会：建好账号、配好默认模型（`deepseek-official / deepseek-chat`）、
把 `dsh-gate` 拷一份到 `PATH`、按你的选择装开机自启，并**顺带试装 `dsh-dock` 插件**
（浏览器里的模型设置页由它提供；装不上会给出手动命令）。

### 第 2 步：等 dsh 就绪（第一次会慢）

网关**立刻**就在监听，登录页也立刻能打开；慢的是 dsh 的准备过程，它在后台跑。
在服务器上跟着日志看：

```sh
journalctl -u dsh-gate -f        # 自启方式装的
# 或（前台/后台运行）
tail -f ~/.dsh-gate/run/logs/<实例名>.log
```

第一次会依次看到（时间来自干净容器里的实测）：

```
本机没有 npx，正在下载 Node.js 运行时（不改系统，只放进网关状态目录）   ← 约 15 秒
Node.js 运行时已就绪  bin=~/.dsh-gate/node/node-vXX/bin
pnpm 已就绪（dsh 的插件安装器会用它）                                ← dsh 装插件要用
dsh 启动命令已就绪（npx --yes @deepseek-ai/dsh）
dsh 已就绪  addr=127.0.0.1:12080
```

**这一步联网**：下 Node 与 dsh 包。官方源慢或连不上时会自动改用国内镜像
（`registry.npmmirror.com`），日志里能看到「改用镜像源重试」。全部就绪通常 3–5 分钟，
之后重启是秒级（Node 不重复下载，dsh 走 npx 缓存）。

想先确认配置对不对再启动，用不打任何状态的体检：

```sh
dsh-gate check
```

### 第 3 步：访问并登录

浏览器打开横幅里那个**局域网**地址（或本机地址）：

```
http://192.168.1.20:12081/
```

1. 打不开 / 连接被拒绝 → 网关没在跑：`dsh-gate status`；
2. 看到登录页 → 用第 1 步的账号口令登录；
3. 登录后是「DSH 正在启动」的等待页 → 正常，等 dsh 就绪，页面每 2 秒自刷新；
4. 登录后立刻能用 → 直接开始对话。

> 从别的电脑/手机访问不了，但本机能开？多半是防火墙：Linux `ufw allow 12081/tcp`，
> 云服务器还要放行安全组。

### 第 4 步：配置模型（不装任何东西也能配）

**官方 DeepSeek**（DSH 自带这个路由，只需密钥）：

```sh
printf 'sk-你的密钥' | dsh-gate model key set DEEPSEEK_API_KEY --stdin
dsh-gate model default deepseek-official deepseek-chat
dsh-gate model list                     # 确认写进去了
```

**第三方 / 自建网关**（任何兼容 OpenAI 协议的服务）：

```sh
printf '你的密钥' | dsh-gate model key set MY_GW_API_KEY --stdin
dsh-gate model provider add my-gw --base-url https://api.example.com/v1 \
    --name 我的网关 --key-env MY_GW_API_KEY --models qwen3-max,glm-5
dsh-gate model default my-gw qwen3-max
```

改完可能需要重启 dsh 才读到：`dsh-gate restart <实例名>`。

> 浏览器里的「设置 → 模型」页也可以配，但它依赖 `dsh-dock` 插件（见下一步）；
> 插件装不上时，上面这两条命令就是唯一的办法——它们直接改 DSH 自己的设置文件，
> 不需要任何插件或页面。

### 第 5 步：装插件（可选，但推荐）

`dsh-dock` 提供模型设置页、余额、通知等功能坞页面。网关可以绕过浏览器里的插件页直接装：

```sh
dsh-gate plugin list                                     # 看已装了什么
dsh-gate plugin add dsh-dock                             # 官方源失败会自动换镜像源
dsh-gate restart all                                     # 装完要重启 dsh 才加载
```

`plugin list` 还会顺带检查一件常见故障：浏览器设置页打不开（`settings are unavailable
in this browser`）通常是因为配置档里缺 `@deepseek-ai/dsh-web-app`——清单里会指出来。

### 第 6 步：日常运维

```sh
dsh-gate status                  # 现在跑着哪些实例、端口、目录、运行时长
dsh-gate check                   # 体检：端口占用、dsh 状态、该访问哪个地址
dsh-gate stop test               # 停掉 test（必须写名字，写 all 才是全部）
dsh-gate restart test            # 按原参数重启
dsh-gate service status          # 开机自启装了没、单元在哪
```

日志在哪：

| 运行方式 | 日志位置 |
|---|---|
| 前台运行 | 终端里直接看 |
| `run --background` | `<登记目录>/logs/<实例名>.log`（默认 `~/.dsh-gate/run/logs/`） |
| `service install`（Linux） | `journalctl -u dsh-gate -f` |
| `service install`（macOS） | `~/Library/Logs/dsh-gate.log` |

### 第 7 步：不用了怎么卸

```sh
dsh-gate uninstall test --service            # 停掉 test + 取消自启，数据留着
dsh-gate uninstall test --service --purge    # 确认不要了再连数据一起删
```

只取消开机自启用 `dsh-gate service uninstall`。`uninstall` **必须点名实例**
（写 `all` 才是全部，什么都不写只列清单），这是刻意设计：多实例机器上「默认停全部」
一次手抖就能把别人正在用的实例收走。

### 出问题了先看这三处

1. **`dsh-gate status`**——网关到底跑着没、端口是多少；
2. **`journalctl -u dsh-gate -f`（或日志文件）**——dsh 为什么没起来、下载卡在哪一步；
3. **`dsh-gate check`**——端口被谁占了、dsh 命令找不找得到、该用哪个地址访问。

常见症状直接对着[常见问题](#常见问题)查：页面报找不到 npx、插件的 `spawn pnpm ENOENT`、
`settings are unavailable in this browser`、`dsh web authentication required`、
会话默认工作目录没生效——每一条都有排查步骤。

---

## 安装

### 下载预编译包

不想碰 Go 的话，直接下发行版里对应平台的压缩包（Linux / macOS / Windows ×
amd64、arm64，外加树莓派这类 armv7）：

- GitHub：<https://github.com/wycto/dsh-gate/releases>
- Gitee（打不开 GitHub 时）：<https://gitee.com/wycto/dsh-gate/releases>

解开就是可执行文件，包里带着 `LICENSE`、`README.md` 与一页纸的 `QUICKSTART.md`；
发行版根目录的 `SHA256SUMS` 用来校验下载是否完整：

```sh
shasum -a 256 -c SHA256SUMS      # Linux 上用 sha256sum -c SHA256SUMS
```

目标机不需要 Go 工具链，也不需要任何 C 运行时——这些二进制都是静态的。

### 从源码构建

Go 代码在 `go/` 子目录：

```sh
git clone https://github.com/wycto/dsh-gate.git
cd dsh-gate/go
go build -o dsh-gate ./cmd/dsh-gate
```

产物是 `go/dsh-gate`，拷到目标机的 `PATH` 里即可，目标机不需要 Go 工具链。

需要 Go 1.24 或更高版本。项目只用标准库，`go build` 不会下载任何依赖。

不想手工拷也有现成的两条路：`dsh-gate setup` 与 `sudo dsh-gate service install`
默认都会把可执行文件拷一份到 `/usr/local/bin`（`--bin-dir` 可改，留空则不拷），
装完就能直接敲 `dsh-gate`，不必再 `./dsh-gate`。它是**拷贝**而不是软链：原目录
被清理也不影响命令可用，服务单元里记录的也是拷贝后的路径。

服务器上没有 Node.js 也不用先去装：网关启动时会自己下载官方 Node.js
运行时到状态目录（默认 `~/.dsh-gate/node`，不需要 root、不改系统），并用它
启动 dsh；`uninstall --purge` 会连它一起删掉。

### 交叉编译打包

仓库自带构建脚本，一次产出全部平台的可执行文件与压缩包：

```sh
sh go/build.sh                 # 全部目标
sh go/build.sh linux/arm64     # 只编译指定目标
```

产物在 `dist/<版本号>/`，每个平台一个目录（可执行文件 + LICENSE + README），
外加 `.tar.gz`（Windows 是 `.zip`）与 `SHA256SUMS`：

```
dist/0.3.13/
├── dsh-gate_0.3.13_darwin_arm64/dsh-gate        dsh-gate_0.3.13_linux_arm64/dsh-gate
├── dsh-gate_0.3.13_darwin_arm64.tar.gz          dsh-gate_0.3.13_linux_arm64.tar.gz
├── dsh-gate_0.3.13_darwin_amd64/…               dsh-gate_0.3.13_linux_arm/dsh-gate   （树莓派等 armv7）
├── dsh-gate_0.3.13_darwin_amd64.tar.gz           dsh-gate_0.3.13_windows_amd64/dsh-gate.exe
├── …                                           dsh-gate_0.3.13_windows_arm64/…
└── SHA256SUMS
```

dsh-gate 是**纯 Go**，`CGO_ENABLED=0`，所以这些二进制都是静态的：`scp` 到目标机
就能跑，不需要目标机装 Go，也不需要任何 C 运行时。

脚本本身很简单，交叉编译也可以直接手写：

```sh
GOOS=windows GOARCH=amd64 go build -o dsh-gate.exe ./cmd/dsh-gate
GOOS=linux   GOARCH=arm64 go build -o dsh-gate      ./cmd/dsh-gate
```

版本号默认取 `go/internal/version/version.go` 里的 `Version`（全项目唯一真源）；
脚本用 `-ldflags` 注入，手工构建时也可以：

```sh
go build -ldflags "-X github.com/wycto/dsh-gate/internal/version.Version=0.3.13" -o dsh-gate ./cmd/dsh-gate
```

---

## 命令参考

不带子命令时默认就是 `run`，所以 `dsh-gate` 与 `dsh-gate run` 等价，`dsh-gate --port 8080` 也等价于 `dsh-gate run --port 8080`。

退出码：`0` 成功，`1` 运行失败，`2` 参数用法错误。

### dsh-gate setup

引导式初始设置：创建管理员账号，并可顺带后台启动、安装开机自启。交互式终端里
逐项询问（回车接受默认值），**答完会汇总确认一遍，回答 `n` 可以带着当前答案重答**，
不用整个重来；管道 / CI 里一个都不问，口令必须走 `--random` 或 `--password-stdin`。

提示语里的方括号沿用的是 Unix 惯例：**大写的那个字母就是默认值**——`[Y/n]` 回车＝是
（确认页默认继续），`[y/N]` 回车＝否（自启、后台启动这类有副作用的动作默认不做）。
每一轮的第一行提示也会把这句规则打出来。

向导里先问 `dsh 端口`、再问 `对外端口`：两个都是一串数字，先问后面 check、自启、
接管都关心的那个，填反的机会小得多。

**两个端口的合法性当场就查**：不是数字、超出 1–65535、两个填成一样、或端口已被占用，
都会当场拒绝并要求重填——不会等到启动那一刻才失败（那时账号与开机自启都已经装好了，
还得回头重来）。被占用时还会说清是不是一个 dsh 实例、占用的进程号是多少（Linux 能反查），
并给出下一步。脚本模式（非交互）在动手之前也会做同样的预检。

账号表已有账号时不再问口令，也不会改动现有账号；重复执行是安全的。
实例选项与 `run` 的同名选项一致，会被原样传给要执行的 `run --background` 或
`service install`——设置因此只有一份真源，setup 自己不引入新的配置文件。

```sh
dsh-gate setup
# 全程交互

dsh-gate setup --name company --port 12061 --dsh-home ~/.dsh-company --random
# 脚本模式：非默认项用选项给出，口令自动生成并打印一次

echo "$PASSWORD" | dsh-gate setup --password-stdin
# CI / 脚本里设置口令
```

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--admin` | `admin` | 管理员用户名 |
| `--random` | 关 | 自动生成强口令并打印一次 |
| `--password-stdin` | 关 | 从标准输入读取口令 |
| `--start` | 关 | 完成后立即后台启动 |
| `--service` | 关 | 安装开机（登录）自启并立即启动（蕴含 `--start`） |
| `--bin-dir` | `/usr/local/bin` | 把 dsh-gate 拷到这个目录（需在 `PATH` 里），装完直接敲 `dsh-gate`；留空则不拷 |
| `--plugin` | 空 | 启动后顺带把 dsh 插件装进配置档（可重复）；留空时默认试装 `dsh-dock`，用自备 pnpm 与镜像源，不依赖浏览器里的插件页 |
| `--api-key-stdin` | 关 | 从标准输入读 DeepSeek API Key（脚本 / CI 首选，避免留在 shell 历史里） |
| `--api-key` | 空 | 直接给 DeepSeek API Key（不推荐，同上） |
| `--model` | `deepseek-chat` | 默认模型 id |
| `--model-provider` | `deepseek-official` | 默认服务商；官方 DeepSeek 是 DSH 自带路由，不用另外配置 |
| `--allow-root-write` | 关 | 允许以 root 身份把数据写进别的用户的目录。默认拒绝：那会造出 root 属主的 `settings.yaml` / `.credentials.yaml`，目标用户自己的 dsh（含桌面端）读不了也改不动 |

TLS、公开地址、信任列表等进阶项不在向导范围内，完成初始设置后直接用
`dsh-gate service install` 的同名选项或配置文件即可。

### dsh-gate run

启动网关：监听一个局域网可达的端口，显示登录页，把请求转发给本机的 dsh。

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `--name` | `default` | 实例名，多实例运行时用于区分与日志前缀 |
| `--host` | `0.0.0.0` | 网关监听地址，如 `0.0.0.0` 表示接受局域网所有设备的连接 |
| `--port` | `3082` | 网关监听端口（对外访问用的端口） |
| `--dsh-host` | `127.0.0.1` | dsh 监听地址。必须是某个具体地址，`0.0.0.0` 会被 dsh 拒绝 |
| `--dsh-port` | `3080` | dsh 监听端口（仅本机访问） |
| `--dsh-home` | `~/.dsh` | dsh 实例目录（`DSH_HOME`）。指向不同目录即两套互不干扰的 dsh |
| `--dsh-cmd` | 空 | 启动 dsh 的命令，支持占位符 `{host} {port} {home} {workspace} {url} {trusted}`；留空自动探测 |
| `--dsh-token` | 空 | 已知的 dsh 启动令牌；仅接管外部 dsh 且拿不到令牌时才需要 |
| `--npm-registry` | 空 | 拉取 dsh 用的 npm 源。留空用 npm 默认，源连不上/太慢时自动改用镜像源重试一次；显式给了就只认它 |
| `--workspace` | 空 | dsh 子进程的工作目录（决定会话默认能访问哪些文件）。留空＝沿用网关进程的工作目录：前台运行时就是当前目录，由服务托管时是单元的 `WorkingDirectory`（用户家目录）——**`setup` 会把它写成绝对路径并传给服务单元**，免得这两个「当前目录」不是一回事 |
| `--state-dir` | `~/.dsh-gate` | 网关自己的状态目录（放账号表；本机没有 Node.js 时，自备的 Node.js 运行时也放这里），不会写进 dsh 的实例目录 |
| `--users` | 空 | 账号表路径，留空则放在 `<状态目录>/users.json` |
| `--public-url` | 空 | 对外公告地址，留空按局域网 IP 自动推导 |
| `--trusted-host` | 空 | 额外信任的地址，可重复。局域网 IP 会自动加入，一般不用手工指定 |
| `--tls-cert` | 空 | HTTPS 证书文件，与 `--tls-key` 成对提供 |
| `--tls-key` | 空 | HTTPS 私钥文件，与 `--tls-cert` 成对提供 |
| `--auto-start` | `true` | dsh 端口空闲时由网关拉起 dsh |
| `--adopt` | `true` | dsh 端口已被占用时直接接管那个实例，而不是报错 |
| `--restart` | `true` | dsh 意外退出后自动重新拉起 |
| `--background` / `-d` | 关 | 放到后台运行：关掉终端也继续，日志写入 `<登记目录>/logs/<实例名>.log` |
| `--run-dir DIR` | `~/.dsh-gate/run` | 运行登记目录，`status` / `stop` / `restart` 从这里找实例 |
| `--session-ttl` | `168h` | 登录会话有效期 |
| `--max-login-fails` | `5` | 同一 IP 连续登录失败多少次后开始锁定 |
| `--lockout` | `1m` | 登录失败锁定的基础时长，每次再失败翻倍 |
| `--dsh-start-timeout` | `1m30s` | 拉起 dsh 后等待其就绪的最长时间 |
| `--config` | 空 | 多实例配置文件（JSON）；使用时不要再指定任何实例选项（唯一例外是 `--name`，用来从配置里挑一个实例） |
| `--log-level` | `info` | 日志级别：`debug` / `info` / `warn` / `error` |
| `--log-json` | `false` | 以 JSON 输出日志 |
| `--allow-root-write` | 关 | 允许以 root 身份把数据写进别的用户的目录（默认拒绝，理由见 `setup` 的同名选项）；后台化时会交给子进程 |

示例：

```sh
# 默认：监听 0.0.0.0:3082，转发到 127.0.0.1:3080，实例目录 ~/.dsh
dsh-gate run

# 代理公司的 dsh：对外 8080，本机 8081，实例目录独立
dsh-gate run --port 8080 --dsh-port 8081 --dsh-home ~/.dsh-company --name company

# 按配置文件同时启动多个实例
dsh-gate run --config dsh-gate.json
```

`--dsh-cmd` 支持的占位符：

| 占位符 | 替换成 |
| --- | --- |
| `{host}` | dsh 的监听地址 |
| `{port}` | dsh 的监听端口 |
| `{home}` | dsh 实例目录 |
| `{workspace}` | dsh 子进程的工作目录 |
| `{url}` | 对外公告地址（`--public-url`） |
| `{noopen}` | 字面量 `--no-open` |
| `{trusted}` | 下一个可信地址，可重复出现以带多个值 |

留空时 dsh-gate 直接使用**官方命令** `npx --yes @deepseek-ai/dsh web` 启动 dsh：
npx 每次启动都会对照 npm 检查版本，包不存在或有新版本时需要交互确认安装/更新，
`--yes` 把这个确认自动化（网关拉起的进程没有终端可回答）。

启动前会做一次**冒烟验证**（`web --help` 必须打出帮助、且不带运行时错误），
把「装了但跑不起来」的情况在网关启动前就现出原形。失败时按**证据**给出指引
（引用 dsh 的原始报错），已覆盖三类实测情形：旧 Node.js 崩溃、入口静默不执行
（0.2.0-rc 系列入口按 Bun 构建，需要很新的 Node.js）、缺平台原生绑定（上游
未发布时 Linux 上无法启动，只能向 dsh 上游反馈或用 `--dsh-cmd` 指定替代）。
指引失败不拦启动——网关照常在线，dsh 的启动循环会继续重试。

### dsh-gate status

列出**本机正在运行**的网关实例：名字、网关端口、dsh 端口、dsh 目录、账号表位置、
进程号、已运行时长、访问地址。

```
$ dsh-gate status
实例 personal
  网关端口  0.0.0.0:3082
  dsh 端口  127.0.0.1:3080
  dsh 目录  /Users/me/.dsh-web-home
  账号表    /Users/me/.dsh-gate/users.json
  进程号    51234
  已运行    2 小时 13 分钟
  访问地址  http://192.168.1.20:3082/（明文 HTTP）

共 1 个实例在跑（登记 1 条）。
停止：dsh-gate stop [实例名...]     重启：dsh-gate restart [实例名...]
```

与 `check` 的分工：`check` 问「按这份配置跑会怎样」（端口有没有被占、dsh 命令
在不在），`status` 问「现在到底跑着什么」。前者看配置，后者看运行登记，所以
`status` 不需要你记得当初是怎么启动的。

实例启动时会把自己登记在 `~/.dsh-gate/run/<实例名>.json`（含完整命令行，重启要用），
停机时自动注销。进程被强杀时登记会残留，`status` 会把它标成「未运行」而不是
假装它还在跑。

选项：`--run-dir DIR` 换一个登记目录、`-v` 附带显示启动命令。

### dsh-gate stop

停掉本机正在运行的网关实例，**连同它托管的 dsh**。

**必须写明停哪些**：写实例名停那几个，写 `all` 才是全部。什么都不写**不会执行任何
操作**，只会列出当前在跑的实例供你挑选——一次手抖就该把「全部」打出来，而不是
靠默认值替你做决定。

```
dsh-gate stop personal        停掉 personal
dsh-gate stop personal work   停掉这两个
dsh-gate stop all             停掉全部
```

什么都不写时：

```
请写明要停哪些实例。当前在跑的有：
  personal        0.0.0.0:3082
  company         0.0.0.0:8080

  dsh-gate stop <实例名>...   只停指定的那几个
  dsh-gate stop all           停全部

（什么都不写时不会执行任何操作。）
```

它走的是网关自己的控制端点（`POST /__dsh_gate/shutdown`，只接受回环来源），
因此三个平台行为一致，被停的实例会按和 `Ctrl-C` 一样的流程收尾：先断新请求，
再给 dsh 发 SIGTERM 留 10 秒自己收尾。

不用 PID 文件加信号是刻意的：信号要在每个平台各写一份，而 Windows 上根本没有
SIGTERM，只能退化成强杀，dsh 来不及收尾。

前台运行时按 `Ctrl-C` 也一样，两条路走的是同一套收尾流程。

选项：`--wait DUR` 等待端口关闭的最长时间（默认 20 秒）、`--run-dir DIR`。

### dsh-gate restart

按**当初的启动参数**重启实例。和 `stop` 一样必须写明目标：写实例名重启那几个，
写 `all` 才是全部，什么都不写只列出在跑的实例、不做任何事。

```
dsh-gate restart personal     只重启 personal
dsh-gate restart all          重启全部
```

用「停 + 重新拉起」而不是给运行中的进程发信号：监听端口、账号表、环境变量都要
在启动时确定，热重启做不到这些，只能重来一遍。

新进程**脱离终端**运行，输出写在 `~/.dsh-gate/run/logs/<实例名>.log`，所以命令
执行完就返回、不会占着终端。日志文件一直追加，重启前的历史也还在。

> 用 `--config` 启动时，登记里存的是整条命令；`restart <实例名>` 会用同一份配置
> 重新拉起、并把实例名对齐到登记里的那个。

**后台运行多个实例**时，`--background` 会和 `--config` 配合，**每个实例各起一个
独立进程**（而不是一个进程托管全部）——因为 `stop` / `restart` 都是按实例来的，
一个进程只能整体停：

```sh
dsh-gate run --config dsh-gate.json --background    # 每个实例一个进程，可分别管理
dsh-gate run --config dsh-gate.json                # 一个进程托管全部，适合前台看日志
```

`--config` 是唯一可以和 `--name` 同时出现的场合——那时的 `--name` 不是「设置实例名」，
而是**从配置里挑一个实例**：

```sh
dsh-gate run --config dsh-gate.json --name company --background
```

选项：`--wait DUR` 等待端口重新就绪的最长时间（默认 30 秒）、`--run-dir DIR`。

### 怎么绕过网关直连 dsh（排查用）

网关启动并读到启动令牌后，会在日志里打出一条**带令牌的 dsh 原机地址**：

```
已记录 dsh 启动令牌，本机可直接打开（该地址只在回环上有用）
  本机地址=http://127.0.0.1:3080/?token=…
```

直接用它就能打开 dsh 的界面，完全不经过网关的登录页与令牌交接。当登录进不去、
页面一直转圈、或者要确认「到底是 dsh 的问题还是网关的问题」时，这是最快的分界办法。

这个令牌只在回环上有用——dsh 只监听 `127.0.0.1`，别的机器拿着它也连不上；
dsh 自己启动时也会把同样一条 URL 打在终端上。

后台运行时（`--background`）这条地址在日志文件里：

```sh
grep 本机地址 ~/.dsh-gate/run/logs/<实例名>.log
```

### dsh-gate user

管理登录账号。不带子命令时打印用法。

| 子命令 | 说明 |
| --- | --- |
| `user add <用户名>` | 添加或修改账号（口令交互输入，需输两遍） |
| `user passwd <用户名>` | 修改已有账号的口令（账号不存在时拒绝，不会误建新号） |
| `user list` | 列出账号 |
| `user rm <用户名>` | 删除账号 |

账号表里只有口令的 PBKDF2 散列，谁也读不回明文；忘记口令就用 `user passwd`
（或 `user add`）设置一条新的。**口令与账号的改动对运行中的网关立即生效**，
不需要重启：网关每次登录校验前会检查账号表文件是否被别的进程改过，改过就
重新加载。

`user add` 的选项：

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `--iterations` | `600000` | PBKDF2 迭代次数（调高更安全也更慢） |
| `--random` | `false` | 自动生成一个强随机口令并打印一次 |
| `--password-stdin` | `false` | 从标准输入读取口令（适合脚本） |

`user add` / `list` / `rm` 都接受这两个选项：

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `--users` | 空 | 账号表路径，留空则按实例目录推导 |
| `--dsh-home` | 空 | dsh 实例目录，用于推导账号表路径 |

```sh
# 创建第一个账号
dsh-gate user add admin

# 忘记口令时重设（改完立即生效，不用重启网关）
dsh-gate user passwd admin

# 管理某个实例的账号：该实例用了单独的状态目录或账号表时，带上同一个选项
# （--dsh-home 与账号表位置无关，账号表默认都在网关状态目录里）
dsh-gate user list --state-dir ~/.dsh-gate-company

# 脚本 / CI 里设置口令
echo "$PASSWORD" | dsh-gate user add admin --password-stdin

# 随机口令，省得自己想
dsh-gate user add admin --random

# 列出 / 删除
dsh-gate user list
dsh-gate user rm olduser
```

选项可以写在位置参数前面或后面，`user add admin --dsh-home ~/.dsh-company` 和 `user add --dsh-home ~/.dsh-company admin` 等价。

`user rm` 拒绝删除最后一个账号。

### dsh-gate check

启动前体检：账号、端口占用、dsh 是否就绪、访问地址。不修改任何状态。

选项与 `dsh-gate run` 相同（不含 `--log-level` / `--log-json`），另加 `--config`。退出码非 `0` 表示存在必须先解决的问题。

诊断信息写 stdout、日志走 stderr，所以可以放心地 `dsh-gate check > report.txt`。

### dsh-gate service

把网关交给**系统的服务管理器**托管，从而开机（登录）自动启动。三个平台都支持：

| 平台 | 机制 | 安装位置 | 需要 root |
|---|---|---|---|
| Linux | systemd 系统服务 | `/etc/systemd/system/dsh-gate.service` | 是 |
| macOS | launchd 的 LaunchAgent | `~/Library/LaunchAgents/com.wycto.dsh-gate.plist` | 否 |
| Windows | 计划任务（登录时启动） | 任务计划程序里的 `dsh-gate` | 否 |

```sh
dsh-gate service install [选项]    # 安装并立即启动
dsh-gate service status           # 看当前托管状态
dsh-gate service unit [选项]      # 只打印将要安装的配置内容，不做任何改动
dsh-gate service uninstall        # 取消开机自启
```

选项与 `dsh-gate run` 相同，另加：

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--binary PATH` | 当前运行的真实路径 | dsh-gate 可执行文件的绝对路径 |
| `--bin-dir DIR` | `/usr/local/bin` | 把 dsh-gate 拷到这个目录（需在 `PATH` 里），命令就能直接敲；服务单元也改用它，留空则不拷 |
| `--working-dir DIR` | 见下 | 工作目录，写进服务配置，决定网关进程自己的 cwd |
| `--user NAME` | 当前用户 | 以哪个用户身份运行（Linux 用） |

macOS 上是**用户级** LaunchAgent（登录时启动），所以它属于**登录用户**而不是 root：

- 直接 `dsh-gate service install` 即可，不需要 sudo；
- 如果为了把命令装进 `/usr/local/bin` 而用了 `sudo`，自启仍会装给发起 sudo 的那个
  用户（按 `SUDO_UID` / `SUDO_USER` 判断）。**在 root 登录的环境里装不了**——那时
  没有可用的 GUI domain，工具会直接说明这一点，而不是丢一句 launchd 的报错。

---
服务配置里的工作目录按这个顺序取值：

1. 显式 `--working-dir <目录>`；
2. 实例的 `--workspace <目录>`（setup 问的「会话默认工作目录」就是它）；
3. 都没有时用当前用户家目录。

第二条是实测补上的：手敲 `service install --workspace /srv` 时，如果工作目录留在
`/root`，网关进程与 dsh 的工作目录就分家了——凡是「留空 `--workspace` 时跟随网关进程」
的语义，在服务托管下都会指到家目录。想核对，用 `systemctl cat dsh-gate` 看
`WorkingDirectory=` 与 `ExecStart=` 里的 `--workspace` 是不是同一个目录。

托管的始终是 `dsh-gate run`（前台）——服务管理器自己负责重启与日志，网关不用
再后台化一次。想让某个实例随登录启动，就用和它一样的选项：

```sh
dsh-gate service install --name personal --port 3082 --dsh-home ~/.dsh-web-home
```

**多实例时注意**：一次 `service install` 只托管一条启动命令。要托管多实例，
让它跑配置文件：`dsh-gate service install --config ~/dsh-gate.json`。

日常查看：

| 平台 | 看状态 | 看日志 |
|---|---|---|
| Linux | `systemctl status dsh-gate` | `journalctl -u dsh-gate -f` |
| macOS | `dsh-gate service status` | `log stream --predicate 'process == "dsh-gate"'` |
| Windows | `dsh-gate service status` | `dsh-gate status -v` |

> 实例由服务管理器托管后，请不要再用 `dsh-gate stop` 停它——服务管理器会立刻
> 把它拉起来。要停请用 `systemctl stop`（或 `launchctl bootout`）。

### dsh-gate uninstall

不再用某个实例（或整台机器）时的收尾：**停掉点名的实例**，并按需取消开机自启、
删除数据。三件事严格分开，各自都要显式请求。

```sh
dsh-gate uninstall test                  # 只停 test，其它实例不受影响
dsh-gate uninstall personal work         # 停这两个
dsh-gate uninstall all --service --purge # 整机收尾：停全部 + 取消自启 + 删数据
```

**目标必须写明**（和 `stop` / `restart` 同一条约定）：写实例名就停那几个，写 `all`
才是全部；什么都不写只列出在跑的实例、不执行任何操作（退出码 2）。「默认停全部」
在多实例机器上一次手抖就能把别人正在用的实例收走。

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--service` | 关 | 同时取消开机（登录）自启。**服务单元是机器级的，一个平台只有一个**，所以它没有「按实例」这一说；取消前会把它托管的命令打出来，让你看清动到的是哪一条 |
| `--purge` | 关 | 删除状态目录（账号表、日志、自备 Node/pnpm 运行时）与运行登记目录，不可恢复 |
| `--state-dir DIR` | `~/.dsh-gate` | `--purge` 时要删的状态目录 |
| `--run-dir DIR` | `~/.dsh-gate/run` | 运行登记目录 |
| `--wait DUR` | `20s` | 等待实例端口关闭的最长时间 |

数据**默认保留**——账号表删掉就找不回来；交互式终端里会当场问一句，管道 / CI 里
不猜，要删必须显式写 `--purge`。

还有别的实例在跑时，`--purge` 会被**拒绝**：状态目录与登记目录是机器级共享的，
删掉它，那些实例就丢了身份，`status` / `stop` 再也找不到它们。要整机收尾就写
`uninstall all --purge`，否则把 `--state-dir` 指到别处。

三条边界是刻意的：

- **dsh 自己的数据（`~/.dsh`）从来不碰。** 会话、插件、配置都归 dsh，卸载网关
  不该把它们带走。状态目录里自备的 Node.js 运行时（`~/.dsh-gate/node`）与 pnpm
  （`~/.dsh-gate/pnpm`）属于网关，`--purge` 会一并删掉。
- **dsh-gate 可执行文件不删。** 正在运行的程序删自身在各平台都不干净，而且可能
  是包管理器装的；命令结束时打印它的路径，由你按安装方式处理（`rm`、`brew
  uninstall`、`apt remove`…）。安装时拷进 `PATH` 的那一份（默认
  `/usr/local/bin/dsh-gate`）也一样留着，删掉它才算彻底清理。
- **只动你点名的东西。** `--service` 管的是那一个服务单元，`--purge` 管的是那一个
  状态目录；没写就是不碰。

`--purge` 的路径来自命令行，写错一个字符就可能指向家目录或根，所以对 `/`、家目录
本身这类目标会直接拒绝执行。

> `dsh-gate service uninstall` 是同一个动作的「只做自启那一步」：它取消机器上那一个
> 服务单元（不管它托管的是一个实例还是 `--config` 里的一串），不碰实例与数据。

### dsh-gate plugin

直接管理 dsh 配置档（profile）里的插件：加依赖、装包、写进 bundle 清单。做的事与 dsh
自己的「添加插件」完全一样，但会用上网关自备的 Node/pnpm 与自动换源——**服务器上缺
这些的时候，浏览器里的插件页是装不动的**（实测报 `spawn pnpm ENOENT`，或者官方源
180 秒超时）。

```sh
dsh-gate plugin list                  # 看当前装了哪些插件、bundle 清单对不对
dsh-gate plugin add dsh-dock          # 装一个插件（先官方源，失败自动换镜像源）
dsh-gate plugin add dsh-dock @deepseek-ai/dsh-web-app
```

机制与 dsh 插件管理器一致：在 `<dsh-home>/profiles/web/` 里跑 `pnpm add <包名>`，再把
包名追加到该目录 `package.json` 的 `dsh.profile.bundles`（dsh 靠这份清单决定加载哪些
bundle）。**装完要重启 dsh 才生效**——profile 解析在进程启动时建立，命令结束时会
提示 `dsh-gate restart all`。

配置档目录由 dsh 自己创建（第一次启动时）。还没启动过 dsh 的话，命令会明确告诉
你先启动一次，而不是凭空造一个可能少挂载点的 profile。

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--dsh-home DIR` | `~/.dsh` | dsh 实例目录 |
| `--profile NAME` | `web` | 配置档名；网关跑的是 `dsh web`，所以是 `web` |
| `--npm-registry URL` | 空 | 拉包用的 npm 源；留空自动（官方源失败退镜像） |
| `--state-dir DIR` | `~/.dsh-gate` | 网关状态目录（自备 pnpm 放这里） |
| `--timeout DUR` | `10m` | 安装超时 |

**顺带能诊断一个常见的「设置页打不开」**：浏览器里的设置页（模型、凭据）由
`@deepseek-ai/dsh-web-app` 挂载的 `ctx.remote.settings` 提供，清单里缺了它，页面上
只会显示 `settings are unavailable in this browser`。`plugin list` 会检查这一点。
注意这个 bundle 属于 dsh 本体、不在 npm 公开包源里（`@deepseek-ai/*` 多数需要授权），
所以缺了就升级/重装 dsh；要立刻能用，装公开的 `dsh-dock` 用它的「模型设置」页，
那正是网关这条命令擅长的场景。

### dsh-gate model

不装插件也能把模型配好。写的是 DSH 自己的两个文件：

- `~/.dsh/.credentials.yaml` 的 `refs:`——**按环境变量名**存密钥（官方 DeepSeek
  默认读 `DEEPSEEK_API_KEY`，见 DSH 的 `dsh-llm-deepseek-api-key`）；
- `~/.dsh/settings.yaml` 的 `agent-default-model`（默认服务商与模型）与
  `llm-pi-ai.providers`（自定义服务商）。

```sh
dsh-gate model list                                   # 看当前配置
dsh-gate model default deepseek-official deepseek-chat # 设默认服务商与模型
printf 'sk-…' | dsh-gate model key set DEEPSEEK_API_KEY --stdin   # 配官方密钥
dsh-gate model provider add my-gw --base-url https://api.example.com/v1 \
    --key-env MY_GW_API_KEY --models qwen3-max,glm-5     # 加一个自定义服务商
dsh-gate model provider remove my-gw                   # 删掉
```

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--dsh-home DIR` | `~/.dsh` | dsh 实例目录（与 `run` 的同名选项一致） |

密钥**只能从标准输入读**（`--stdin`）：写在命令行上会留在 shell 历史与进程列表里。
命令行也不提供明文参数。

改写是**定点**的：只替换目标键所在的块，其余内容（界面偏好、其它服务商…）一个字节
都不动；写入前留一份 `<文件名>.dsh-gate.bak`，写入后回读校验，对不上会自动还原。
名字或值里带冒号、引号、空格、`#` 的一律当场拒绝——写坏这份文件等于让 DSH 起不来。

改了之后可能需要重启 dsh 才读到（命令结束时会提示）；插件 `dsh-dock` 装上了的话，
浏览器里的「模型设置」页仍然更顺手。

### dsh-gate version

打印版本号。

```sh
$ dsh-gate version
dsh-gate 0.3.13
```

---

## 多实例

一个实例 = 一个网关端口 + 一个 dsh + 一个 `--dsh-home`。不同的 `--dsh-home` 就是两套互不干扰的 dsh：会话历史、插件、配置全部分开。一个进程里可以同时跑多个。

**命令行写法**，每个实例一条 `dsh-gate run`：

```sh
# 终端一：公司的 dsh
dsh-gate run --name company   --port 8080 --dsh-port 8081 --dsh-home ~/.dsh-company

# 终端二：我自己的 dsh
dsh-gate run --name personal  --port 3082 --dsh-port 3080 --dsh-home ~/.dsh
```

**配置文件写法**，一次跑完所有实例。一个进程、一条命令、Ctrl-C 一起停：

```sh
dsh-gate run --config examples/multi-instance.json
```

配置文件是 JSON，只有 `defaults` 和 `instances` 两个键：

```json
{
  "defaults": {
    "host": "0.0.0.0",
    "dshHost": "127.0.0.1",
    "autoStart": true,
    "adopt": true,
    "restart": true
  },
  "instances": [
    {
      "name": "company",
      "port": 8080,
      "dshPort": 3080,
      "dshHome": "~/.dsh-company",
      "workspace": "~/work/company"
    },
    {
      "name": "personal",
      "port": 3082,
      "dshPort": 3081,
      "dshHome": "~/.dsh",
      "workspace": "~/work/personal"
    }
  ]
}
```

上面就是 [`examples/multi-instance.json`](examples/multi-instance.json) 的内容，可以直接拿去改。几个注意点：

- **字段名是 JSON 的驼峰写法**（`dshHome`、`dshPort`），和命令行选项名 `--dsh-home` 不是一回事。
- 写错的字段名会**直接报错**，不会被静默忽略——把 `dshHome` 拼成 `dshhome` 时你会立刻看到提示，而不是以为配置生效了实际在裸奔。
- 没写的字段保持默认值，不用把整份配置抄全。
- **使用 `--config` 时不能再指定任何实例选项**（`--port` 之类），dsh-gate 会直接拒绝并告诉你把它们写进配置文件。唯一的例外是 `--name`：它用来从配置里挑一个实例（后台化时就是靠它给每个实例拆出独立进程）。
- 同一进程内实例名不能重复；端口也不能撞：两个实例的网关端口、dsh 端口，或一个实例的网关端口撞上另一个实例的 dsh 端口，都会被拒绝（只有主机地址真正有交集时才算冲突，绑不同网卡的同端口实例仍然允许）。
- 账号表**不跟着实例目录走**：它默认放在 `<状态目录>/users.json`（默认 `~/.dsh-gate/users.json`），因为账号表属于网关、不是 dsh 的数据。多个实例想共用一套账号，把 `users` 指向同一个文件即可。
- 路径支持 `~` 开头，展开成当前用户的主目录。
- **有几项只能全局设置，不能写进配置文件**：会话有效期、登录失败限速、dsh 启动/停止等待时间。这些用命令行或 `DSH_GATE_*` 环境变量设一次，对所有实例生效。

---

## 服务器部署（Linux + systemd）

无界面 Linux 服务器是最典型的场景。完整流程：

```sh
# 1. 确认 dsh 能跑起来（dsh-gate 需要能找到它）
npm i -g @deepseek-ai/dsh

# 2. 拷二进制上去，或自行构建
scp dsh-gate server:/usr/local/bin/

# 3. 体检：确认账号、端口、dsh 命令都没问题
dsh-gate check --dsh-home /var/lib/dsh

# 4. 创建账号（交互输入口令；也可以用 --random 让它随机生成）
dsh-gate user add admin --dsh-home /var/lib/dsh

# 5. 安装 systemd 服务并立即启动
sudo dsh-gate service install --dsh-home /var/lib/dsh --port 3082

# 6. 确认起来了
systemctl status dsh-gate
```

`service install` 会把单元写到 `/etc/systemd/system/dsh-gate.service`，然后依次执行 `daemon-reload`、`enable`、`restart`。

日常运维：

```sh
systemctl status dsh-gate      # 查看状态
journalctl -u dsh-gate -f      # 跟踪日志
systemctl restart dsh-gate     # 重启
systemctl stop dsh-gate        # 停止
```

单元的完整内容可以用 `service unit` 打印出来看一眼：

```sh
dsh-gate service unit --dsh-home /var/lib/dsh --port 3082
```

[`examples/dsh-gate.service`](examples/dsh-gate.service) 是一份示例单元，与上面的输出一致。想换端口或实例目录，不要直接改单元文件，用 `systemctl edit dsh-gate` 写一个 `ExecStart=` 覆盖即可。

单元写好后要撤销：

```sh
dsh-gate service uninstall     # 只取消开机自启（systemctl disable --now + 删单元）
dsh-gate uninstall --purge     # 彻底卸载：连账号表、日志、运行登记一起删
```

几点值得知道的：

- **`WorkingDirectory` 不是可有可无的**。dsh 把启动时的当前目录当作会话的默认 workspace 根目录，systemd 的默认 `/` 会让会话看到一个毫无意义的根。`--working-dir` 默认取当前用户的家目录。
- **`User=` 默认是执行安装命令的那个用户名**。如果你想以别的用户跑，安装时用 `--user` 指定。注意 dsh 的实例目录要对那个用户可写。
- **多实例也可以用一份单元**：把配置写进文件，然后 `sudo dsh-gate service install --config /etc/dsh-gate.json`，`ExecStart` 里就是 `--config /etc/dsh-gate.json`。
- **服务日志默认是 JSON**（`service unit` 会加上 `--log-json`），因为 journalctl 对结构化日志的处理更好。手工前台跑时默认是文本。
- 端口 1024 以下需要 root，普通用户请换一个 1024 以上的端口。

---

## 异地访问（人在家里用公司的 dsh）

**警告：在明文 HTTP 下，dsh 的会话 Cookie 会以明文过网。同一个局域网内可以接受，但不要把明文的网关端口直接暴露到公网。** 下面三种方式按推荐程度排列。

### 方式一：内网穿透工具（最省事）

把网关端口通过各类内网穿透工具挂到一个公网域名上。隧道本身通常是加密的，流量走的是加密信道，宿主机上仍然只听明文 HTTP：

```sh
dsh-gate run --port 3082 --public-url https://your-tunnel.example.com
```

穿透工具会给你一个 `https://xxx.example.com` 之类的地址，指定它访问即可。因为对外是 HTTPS，浏览器侧的 Cookie 会带 `Secure` 标记。

### 方式二：Tailscale / ZeroTier 这类组网

让家里和公司在同一个虚拟局域网里，访问地址仍是局域网形态，但走的是加密的虚拟网络。两端都装上客户端、在同一个 tailnet / network 里即可：

```sh
# 公司机器上
dsh-gate run --port 3082 --public-url http://100.x.y.z:3082

# 家里直接访问
# http://100.x.y.z:3082
```

如果本机没有非回环地址（无界面服务器、容器网络隔离等场景），`--public-url` 需要显式指定，否则 dsh-gate 推导不出对外地址。
> **关于 `--public-url`**：这个选项会转成 dsh 的 `--public-url`，而**并非所有 dsh 版本都支持它**
> （实测 `0.2.0-rc.2` 只有 `--host` / `--port` / `--no-open` / `--trusted-host`）。
> 网关启动前会先跑一次 `dsh web --help` 看它认不认这个选项，不认就跳过并打一条日志——
> 所以升级 dsh 之后它会自动生效，不需要改配置。它只影响 dsh 打印的地址和
> agent 看到的 `DSH_WEB_URL`，**不影响网关转发**。


### 方式三：网关自己跑 HTTPS

有自己的域名和证书时，直接让网关提供 HTTPS：

```sh
dsh-gate run \
  --port 443 \
  --tls-cert /etc/letsencrypt/live/dsh.example.com/fullchain.pem \
  --tls-key  /etc/letsencrypt/live/dsh.example.com/privkey.pem \
  --public-url https://dsh.example.com \
  --trusted-host dsh.example.com
```

两个要点：

- `--tls-cert` 和 `--tls-key` 必须成对提供，只给一个会被拒绝。
- 用了自定义域名，**必须**把它加进 `--trusted-host`，否则 dsh 会拒绝转发（403）。局域网 IP 是自动加进去的，域名不是。

---

WebSocket 走 HTTP/1.1 升级（浏览器的 `WebSocket` API 本来就如此），因此用 `curl`
调试时要加 `--http1.1`：在 HTTP/2 连接上 `curl` 不会发送 `Upgrade` 头，dsh 会回 404。

## 安全说明

**口令怎么存的。** 账号表是权限 `0600` 的 JSON 文件，目录 `0700`，默认在 `~/.dsh-gate/users.json`（网关自己的状态目录，**不放在 dsh 的实例目录里**）。口令用 Go 1.24 标准库的 `crypto/pbkdf2` 做 PBKDF2-HMAC-SHA256，600000 次迭代，每条记录带自己的随机盐，格式是 `pbkdf2-sha256$迭代次数$盐$摘要`。明文口令从不落盘。写入是原子的（临时文件 + rename），写一半掉电不会留下半截 JSON 把所有人锁在门外。

账号不存在时也会跑一遍完整的 KDF 校验，把耗时拉平到和「口令错」同一量级——否则攻击者可以据此枚举账号。

**会话在哪。** 登录会话只存在网关进程内存里，**不落盘**。网关重启即全部失效，所有人需要重新登录。对一个家用 / 办公用的自建网关来说这完全是可以接受的，而换来的是「重启即清空」，不存在过期会话长期滞留的问题。代价是多个网关实例之间不共享会话——每个实例本来就该有自己的一套账号。

会话 Cookie 是 `dsh_gate_sid`，`HttpOnly` + `SameSite=Lax`，启用 TLS 时额外带 `Secure`。

**登录限速。** 同一 IP 连续失败 `--max-login-fails`（默认 5）次后开始锁定，锁定时长从 `--lockout`（默认 1 分钟）起步，每次再失败翻倍，封顶 15 分钟。登录成功即清零。刻意不做全局限速：同一办公室几十人共用一个出口 IP，全局限速会误伤正常用户。

**为什么令牌不落盘。** dsh 的启动令牌等价于 dsh 的万能钥匙。它只保存在 dsh-gate 的进程内存里，不落盘、不写日志、不出现在启动横幅上。转发 dsh 的启动输出时，网关会先把 `token=xxx` 脱敏成 `token=***` 再记录。

**为什么默认让 dsh 只听 127.0.0.1。** dsh 自己没有面向局域网的安全设计：明文 HTTP、启动令牌直接打在 stdout 上。对外只暴露带登录页的网关这一个入口，是这两者之间最简单也最难出错的分界。

**容易踩的坑：自己改端口没用。** dsh 会校验请求的 `Host` 与被接受的 authority 是否匹配，并要求 `Origin` 指向同一 authority。网关把 `Host` 原样转发给 dsh，所以你在浏览器里用哪个地址访问，网关就必须用 `--trusted-host` 把那个地址点名给 dsh。

- 局域网 IP 会自动加进可信名单，一般不用手工指定；
- **主机名、mDNS 名、内网穿透域名不会自动加入**，用到它们时必须显式 `--trusted-host`；
- 没加的表现是每个 API 调用返回 **403**。

同理，`--dsh-host` 不接受 `0.0.0.0`——dsh 出于安全考虑会直接拒绝通配监听地址，dsh-gate 提前拦下来并给出可操作的提示。

上游地址固定来自本地配置，不接受请求头或查询参数指定，因此网关不会变成一个任意 URL 的跳板。限速只看 TCP 对端 IP，不采信 `X-Forwarded-For` / `X-Real-IP`（除非你明确知道前面还挂着自家反向代理，否则这些头任何人都能伪造）。

---

## 环境变量

优先级：**命令行 > 环境变量 > 配置文件 > 默认值**。

环境变量的定位是「让 systemd 单元、Docker 容器、临时 shell 不改命令行也能配置好」。

| 环境变量 | 对应选项 | 说明 |
| --- | --- | --- |
| `DSH_GATE_NAME` | `--name` | 实例名 |
| `DSH_GATE_HOST` | `--host` | 网关监听地址 |
| `DSH_GATE_PORT` | `--port` | 网关监听端口 |
| `DSH_GATE_DSH_HOST` | `--dsh-host` | dsh 监听地址 |
| `DSH_GATE_DSH_PORT` | `--dsh-port` | dsh 监听端口 |
| `DSH_GATE_DSH_HOME` | `--dsh-home` | dsh 实例目录 |
| `DSH_GATE_DSH_CMD` | `--dsh-cmd` | 启动 dsh 的命令 |
| `DSH_GATE_DSH_TOKEN` | `--dsh-token` | 已知的 dsh 启动令牌 |
| `DSH_GATE_NPM_REGISTRY` | `--npm-registry` | 拉取 dsh 用的 npm 源；留空自动（默认源失败退镜像） |
| `DSH_GATE_WORKSPACE` | `--workspace` | dsh 子进程的工作目录 |
| `DSH_GATE_STATE_DIR` | `--state-dir` | 网关自己的状态目录 |
| `DSH_GATE_USERS` | `--users` | 账号表路径 |
| `DSH_GATE_PUBLIC_URL` | `--public-url` | 对外公告地址 |
| `DSH_GATE_TRUSTED_HOSTS` | `--trusted-host` | 额外信任的地址，多个用逗号分隔 |
| `DSH_GATE_TLS_CERT` | `--tls-cert` | HTTPS 证书文件 |
| `DSH_GATE_TLS_KEY` | `--tls-key` | HTTPS 私钥文件 |
| `DSH_GATE_AUTOSTART` | `--auto-start` | dsh 端口空闲时是否由网关拉起 dsh，`true` / `false` |
| `DSH_GATE_ADOPT` | `--adopt` | 端口被占用时是否接管，`true` / `false` |
| `DSH_GATE_RESTART` | `--restart` | dsh 意外退出后是否自动重启，`true` / `false` |
| `DSH_GATE_SESSION_TTL` | `--session-ttl` | 登录会话有效期，如 `168h` |
| `DSH_GATE_MAX_LOGIN_FAILS` | `--max-login-fails` | 锁定阈值 |
| `DSH_GATE_LOCKOUT` | `--lockout` | 锁定基础时长，如 `1m` |

使用 `--config` 时这些环境变量不生效，配置以文件为准。

---

## 常见问题

**页面能打开，但一直 401。**

dsh 的会话与 authority 绑定。换了访问地址（IP ↔ 主机名 ↔ 穿透域名）、或者 dsh 重启换了新令牌之后，浏览器手上的 dsh 会话就失效了。网关会自动清掉交接标记并重试一次，通常表现为「页面自己就好了」。仍然不行的话，把 dsh 启动时打印的完整地址粘到网关地址后面访问一次即可：

```
http://192.168.1.20:3082/?token=6f1c...
```

网关学到这个令牌之后就把它记在内存里，不会再出现在任何日志里。接管别人已经启动的 dsh 时，这是唯一可行的令牌来源；也可以直接用 `--dsh-token` 指定。

**页面能打开，但 API 全是 403。**

`--trusted-host` 没覆盖你正在用的地址。dsh 只信任回环、自身绑定地址和被显式点名的 authority。局域网 IP 会自动加入，但主机名、mDNS 名、内网穿透域名不会。加一个即可：

```sh
dsh-gate run --trusted-host dsh.example.com --trusted-host my-mac.local
```

**端口被占。**

```
网关端口 3082 已被占用。
换一个端口：加 --port <其它端口>
或者先停掉占用它的程序。
```

先跑 `dsh-gate check` 看清楚是谁占的。端口在 1024 以下时提示的是权限不足，换一个 1024 以上的端口即可。

**`--dsh-host 0.0.0.0` 被拒绝。**

这是有意的。dsh 不接受通配监听地址，只允许绑定某一个具体地址。默认的 `127.0.0.1` 就是你要的——对外访问交给网关端口。

**Windows 上 `user add` 没法输口令。**

不是交互式终端时（比如在 CI 或某些 PowerShell 配置下），程序会直接拒绝并提示改用另外两种方式：

```sh
dsh-gate user add admin --random
echo "$PASSWORD" | dsh-gate user add admin --password-stdin
```

口令至少 8 个字符、最多 256 个字符（按字符数计，中文口令不会因为 UTF-8 被算得更长）。

**首次启动很慢。**

dsh 第一次启动要下载插件、初始化 profile，可能要一两分钟。这期间网关端口是开着的，你会看到一个每 2 秒自动刷新的「DSH 正在启动」页面，而不是「连接被拒绝」。等多久由 `--dsh-start-timeout` 控制（默认 1 分 30 秒）。

如果本机连 Node.js 都没有，还要先下运行时（约 15 秒）再由 npx 拉 dsh 的包，冷环境实测整条链路要几分钟。这些准备工作**不阻塞服务**：网关先监听、登录页先能打开，`dsh-gate status` 立刻就能看到实例；准备在后台进行，完成后等待页面会自动进入登录页。日志里能看到 `正在下载官方 Node.js 运行时` → `Node.js 运行时已就绪` → `dsh 启动命令已就绪` → `dsh 已就绪`。

**npm 源太慢 / 拉不下 dsh（日志里是「长时间无响应」）。**

网关会自己换源：官方源（`registry.npmjs.org`）或 nodejs.org 拉不动、超时，就自动改用国内镜像（`registry.npmmirror.com`）重试一次，并把实际用的源以 `npm_config_registry` 注入 dsh——dsh 自己装插件时也走同一个源。想固定用某个源（公司内网 registry、私有代理）就显式给出来，显式给了就不会被自动替换：

```sh
dsh-gate run --npm-registry https://registry.npmmirror.com
export DSH_GATE_NPM_REGISTRY=https://registry.npm.example.com   # 或环境变量 / 配置文件
```

换源只对「源拉不动」有意义；**入口静默不执行、缺原生绑定、旧 Node 崩溃**这些是运行时不兼容，网关会改成实测可用的固定版本 dsh，而不是换源。

**dsh 起不来。**

dsh 起不来时网关不会退出——它会按指数退避持续重试（1 秒起步，最长 30 秒一次），等待页面会显示最近一次失败的原因。先用 `dsh-gate check` 看看 dsh 命令能不能找到；找不到就装好 dsh，或者用 `--dsh-cmd` 显式指定启动命令。

**服务器上没有 Node.js（页面提示找不到 npx）。**

不用自己去装：网关启动时会检测 npx，没有就把**官方 Node.js 二进制包**下载到状态目录（默认 `~/.dsh-gate/node`），校验官方 `SHASUMS256.txt` 后解包使用——不需要 root，也不改系统包。下载失败时页面上会给出原因，可以手工装 Node.js（<https://nodejs.org/>），或用 `--dsh-cmd` 指定启动命令。Alpine 这类 musl 系统跑不了官方 glibc 二进制，请用 `apk add nodejs`。

已经装好的那份会一直复用（不重复下载），卸载时用 `dsh-gate uninstall --purge` 一并删除。

**会话的默认工作目录没生效（实际落在 `/root` 之类的地方）。**

最常见的原因是**服务单元里没有 `--workspace`**：setup 里填的路径会随
`service install` 写进单元，但只有**重装过服务**才生效；升级二进制不会改单元。
先确认：

```sh
systemctl cat dsh-gate | grep -E 'ExecStart|WorkingDirectory'
dsh-gate model list 2>/dev/null || true    # 或直接看启动横幅里的「工作目录」
```

没带就按目标目录重装一次（这一步只重写单元并重启，不动账号表与 dsh 数据）：

```sh
cd /www/wwwroot
sudo dsh-gate service install --workspace "$PWD" --dsh-home /www/wwwroot/dsh-test \
  --dsh-port 12080 --name test --port 12081 --state-dir /www/wwwroot/dsh-gate-test
```

0.3.1 起这件事不用再靠猜：网关每次启动的横幅里都会打印「工作目录」，`service install`
装完也会直接告诉你会话默认工作目录是什么、没指定该怎么补。

**会话的默认工作目录没生效（DSH 里提示「无法创建默认工作区」）。**

`--workspace` 留空时的语义是「沿用网关进程的当前目录」，而网关被 systemd/launchd 托管时
那个「当前目录」是单元的 `WorkingDirectory`（默认是用户家目录 `/root`），不是你在哪个目录
敲的 `setup`。向导以前显示「当前目录」却不写进设置，这个承诺就落空了。现在向导显示真实的
绝对路径、回车接受即写死，并随 `--workspace` 传进服务单元；已经装好的按目标目录重跑一次：

```sh
cd /你希望 agent 读写的目录
sudo dsh-gate service install --workspace "$PWD" --dsh-home /www/wwwroot/dsh-test
```

**页面显示「DSH 要求重新登录」（接管了外部启动的 dsh）。**

dsh 的启动令牌**每个进程随机生成、只在 dsh 启动时打印一次、也不落盘**。如果那个 dsh
不是网关拉起的（比如你自己跑过一个 `dsh web`，或者换了网关进程而旧 dsh 还留着），网关
就拿不到它的令牌：首页能打开，但对话会报未授权。

两条出路，第二条一定成功：

```sh
# ① 还留着 dsh 启动时打印的那条带 token 的地址：把它粘到网关地址后面打开一次
#    （也可以直接 --dsh-token <令牌> 重启网关）

# ② 让网关自己把 dsh 管起来
pkill -f "dsh web"          # 停掉外面那个 dsh（或 ps 找到 PID 后 kill）
dsh-gate restart <实例名>    # 重启网关，它会自己拉起 dsh —— 令牌自然就进网关内存了
```

**页面报 `fork/exec …/node/bin/npx: no such file or directory`。**

网关自备的 Node.js 运行时目录在准备完成之后不见了（冒烟失败被清理、状态目录被删、
被别的进程换掉），而 PATH 里记的还是那条旧路径。删掉状态目录里的 `node/` 再重启实例
就会重新下载：

```sh
rm -rf <状态目录>/node      # 例如 /www/wwwroot/dsh-gate-test/node
dsh-gate restart <实例名>
```

**DSH 里点「添加插件」报 `spawn pnpm ENOENT`（或一直转圈）。**

DSH 的插件安装器走的是 pnpm，而 Node.js 只自带 npm/npx。网关会在准备 dsh 之前用 npm 把
pnpm 装到状态目录的独立前缀（`<状态目录>/pnpm`，不进系统、不需要 root），并把它前置进
PATH；系统里已有 pnpm 时不动。日志里能看到 `已自备 pnpm：…`。

**不必依赖浏览器里的插件页**：用网关自己的命令装，它走同一套 pnpm 与自动换源：

```sh
dsh-gate plugin add dsh-dock     # 官方源失败会自动换镜像源
dsh-gate restart all             # 装完重启 dsh 才生效
```

**页面显示 `dsh web authentication required; reopen the URL printed by dsh web.`**

这是 dsh 的 401 正文（它收到请求时，浏览器信任 Cookie 与当前 authority 对不上）。
网关的交接标记以前只是个布尔值，dsh 重启换了令牌、或会话过期 / 换了访问地址之后，
标记还在，于是网关**永远不再补令牌**——刷新、重新登录都没用，只能手动粘一次带
`?token=` 的地址。

现在标记里存的是「上次交接用的令牌指纹」：dsh 一换令牌，浏览器下次打开 `/` 就自动
重新交接一次；会话在别的场合失效（401）时，网关会清掉标记并 303 回 `/` 重做交接，
整个过程对用户不可见。若仍然看到这句话，说明网关手上这次启动的令牌不对（例如接管
了外部 dsh 又没拿到令牌），按页面提示把 dsh 打印的完整地址粘到网关地址后面访问一次，
或 `dsh-gate restart <实例名>` 让网关重新学习令牌。

**设置页里的「模型」打不开：`加载提供商目录失败: settings are unavailable in this browser`。**

这不是网关的代理坏了，而是 dsh 那边的设置面没挂上：浏览器里的模型/凭据设置由
`@deepseek-ai/dsh-web-app` 这个 bundle 提供的 `ctx.remote.settings` 支撑，配置档的
bundle 清单里缺了它，前端就只能报这一句（客户端代码里是 `describeFace` 拿不到视图）。
先看清单：

```sh
dsh-gate plugin list            # 会检查清单里有没有 dsh-web-app
```

这个 bundle 是 dsh 本体的一部分，`@deepseek-ai/*` 多数不在 npm 公开源里（实测
`pnpm view @deepseek-ai/dsh-client-ui-command` 直接要授权），所以缺了它不能用
`plugin add` 补——先升级/重装 dsh。**能立刻用的路是装 `dsh-dock`**：它是公开包
（`dsh-dock@0.14.0`），自带「模型设置」页，用插件的方式配置服务商与密钥：

```sh
dsh-gate plugin add dsh-dock
dsh-gate restart all
```

**敲 `dsh-gate` 提示 command not found。**

说明它不在 `PATH` 里：要么是直接 `./dsh-gate` 运行，要么只把二进制拷到了某个目录。用 `dsh-gate setup` 或 `sudo dsh-gate service install` 装一次（默认拷到 `/usr/local/bin`），之后就能直接敲；自定义位置用 `--bin-dir`。装完仍找不到，就检查那个目录在不在 `PATH` 里（`echo $PATH`）。

**网关重启后所有人都要重新登录。**

这是有意为之：会话只存在内存里，网关重启即全部失效。

**账号表在哪儿？**

默认在 `~/.dsh-gate/users.json`。用 `--users`、`--state-dir`，或配置文件里的 `users` / `stateDir` 字段可以改。

**`--config` 和命令行选项能一起用吗？**

不能。用了 `--config` 就不允许再指定任何实例选项，dsh-gate 会直接报错并告诉你把它们写进配置文件。（`--log-level` 和 `--log-json` 不算实例选项。）

---

### 网关起来了，但页面一直显示「DSH 正在启动」

先看网关的日志（`journalctl -u dsh-gate -f`，或前台运行时直接看终端）。首次启动要
下载插件，可能要一两分钟。如果长时间不成功，日志里会回放 dsh 自己的报错，常见原因：

- **找不到 dsh**：`dsh-gate check` 会告诉你本机有没有找到；没有的话装
  （`npm i -g @deepseek-ai/dsh`）或用 `--dsh-cmd` 指定。
- **dsh 端口被别的程序占了**：换 `--dsh-port`。
- **`trustedHosts entry ... is not a bare host[:port] authority`**：手动给的
  `--trusted-host` 里有裸 IPv6 地址。IPv6 必须写成 `[2408:...::1]` 这种带方括号的形式；
  网关自动加入的地址已经处理好了。

## 贡献

欢迎提 issue 和 PR。动手之前建议先读一遍 `go/internal/proxy/proxy.go` 的文件头注释——启动令牌的交接流程是整个项目最值得读懂的部分，绝大多数「页面能开但 API 报错」的问题都能在那里找到答案。

几条约定：

- 项目只用 Go 标准库。**不要引入第三方依赖。**
- 改动行为时同步更新 README 与 CHANGELOG。
- Go 代码在 `go/` 子目录，模块名是 `github.com/wycto/dsh-gate`。
- 提交备注用中文，格式是 `类型：备注`。

## License

[MIT](LICENSE)。Copyright © 2026 wycto