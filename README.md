# dsh-gate

**给 [DeepSeek Harness](https://deepseek-harness.github.io/deepseek-harness/guide/quickstart) 加一道局域网 / 异地访问的「管理端」。**

单二进制、只用 Go 标准库（管理端的 SQLite 驱动是唯一第三方依赖，见 [go.mod](go/go.mod)）的 Go 程序。它在一个局域网可达的端口上提供登录页与管理台，鉴权通过后把请求按实例转发到本机的 dsh；dsh 实例由管理端安装、启动、按需拉起，每个实例有自己的 `DSH_HOME`。

![dsh-gate 登录页（深色）](docs/images/login.png)

<sub>手机宽度与浅色模式的效果见 [30 秒上手](#30-秒上手) 末尾。</sub>

English version: [README.en.md](README.en.md)

---

## 目录

- [它解决什么问题](#它解决什么问题)
- [它是怎么工作的](#它是怎么工作的)
  - [实例的启动方式](#实例的启动方式)
- [它到底做了什么](#它到底做了什么)
- [管理端：一个入口，多个 dsh 实例](#管理端一个入口多个-dsh-实例)
  - [管理台里装 dsh：先建记录，再装](#管理台里装-dsh先建记录再装)
- [30 秒上手](#30-秒上手)
  - [登录页在手机与浅色模式下的样子](#登录页在手机与浅色模式下的样子)
- [使用教程](#使用教程)
  - [第 0 步：准备](#第-0-步准备)
  - [第 1 步：安装管理端](#第-1-步安装管理端)
  - [第 2 步：建实例并装好 dsh](#第-2-步建实例并装好-dsh)
  - [第 3 步：访问并登录](#第-3-步访问并登录)
  - [第 4 步：配置模型（不装任何东西也能配）](#第-4-步配置模型不装任何东西也能配)
  - [第 5 步：装插件（可选，但推荐）](#第-5-步装插件可选但推荐)
  - [第 6 步：日常运维](#第-6-步日常运维)
  - [第 7 步：不用了怎么卸](#第-7-步不用了怎么卸)
  - [出问题了先看这三处](#出问题了先看这三处)
- [安装](#安装)
  - [下载预编译包](#下载预编译包)
  - [从源码构建](#从源码构建)
  - [交叉编译打包](#交叉编译打包)
- [命令参考](#命令参考)
  - [dsh-gate install](#dsh-gate-install)
  - [dsh-gate start](#dsh-gate-start)
  - [dsh-gate stop](#dsh-gate-stop)
  - [dsh-gate restart](#dsh-gate-restart)
  - [dsh-gate status](#dsh-gate-status)
  - [dsh-gate uninstall](#dsh-gate-uninstall)
  - [dsh-gate service](#dsh-gate-service)
  - [dsh-gate dsh](#dsh-gate-dsh)
  - [dsh-gate manage](#dsh-gate-manage)
  - [dsh-gate plugin](#dsh-gate-plugin)
  - [dsh-gate model](#dsh-gate-model)
  - [dsh-gate version](#dsh-gate-version)
- [多实例](#多实例)
- [服务器部署（Linux + systemd）](#服务器部署linux--systemd)
- [异地访问（人在家里用公司的 dsh）](#异地访问人在家里用公司的-dsh)
  - [方式一：内网穿透工具（最省事）](#方式一内网穿透工具最省事)
  - [方式二：Tailscale / ZeroTier 这类组网](#方式二tailscale--zerotier-这类组网)
  - [方式三：网关自己跑 HTTPS](#方式三网关自己跑-https)
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

dsh-gate 处理的就是这几件事。它只有一种形态：**一个管理端 + N 个 dsh 实例**。典型场景：

1. **无界面 Linux 服务器**：服务器上装 dsh-gate，任意电脑或手机浏览器打开 `http://服务器IP:12020` 就能用，不需要装任何客户端。
2. **家里 macOS 跑 dsh、公司 Windows 上继续用**（反过来也一样）：管理端装在跑 dsh 的那台机器上，另一台机器用浏览器访问。
3. **手机浏览器**：同一个地址，手机直接打开就能用。
4. **一机多套 dsh**：公司和个人各一套，会话、插件、配置完全分开；一个管理端统一登录、统一授权，不用记多个端口。

dsh-gate 不改 dsh 的任何一行代码，也不要求你改 dsh 的启动方式。它只是站在每个 dsh 前面，多做一次「你是谁」的检查。

---

## 它是怎么工作的

```
   局域网 / 互联网上的任意设备
              |
              |  http://192.168.1.20:12020/
              v
   +--------------------------------------------+
   |  管理端  :12020（0.0.0.0）                  |   <- 唯一对外入口
   |  /            登录页 / 「我的实例」        |
   |  /admin       管理台（实例、用户、授权…）  |
   |  /i/<实例 id>/…  按实例反代；令牌交接      |
   +------+-------------------+-----------------+
          |                   |
          | 127.0.0.1:32101   | 127.0.0.1:32102
          v                   v
   +-------------+     +-------------+
   | dsh alice   |     | dsh bob     |   <- 只听回环，局域网直连不到
   | DSH_HOME=…  |     | DSH_HOME=…  |
   +-------------+     +-------------+
```

四个要点：

- **管理端是唯一对外口**。每个实例的 dsh 默认只绑 `127.0.0.1:<自动分配的端口>`，对外只有管理端一个监听端口。这条边界是整个方案的安全前提。
- **实例由管理端监督**。端口空闲时管理端把 dsh 拉起来；端口已经被某个 dsh 占着时**直接接管**它；被别的程序占着则报错并持续重试（1 秒起步、最长 30 秒一次）。你不需要关心当前是哪种情况。
- **登录页是管理端自己的**：用户只需要一个管理端账号口令，跟 dsh 的启动令牌无关。管理员在 `/admin` 里建实例、装 dsh、授权。
- **多实例互相隔离**：不同的 `DSH_HOME` 就是两套互不干扰的 dsh；实例 id 是随机不透明串，对外路径是 `/i/<id>/`，端口与 id 都由管理端自动分配。

### 实例的启动方式

每个实例有一个「启动方式」，决定管理端启动时要不要把它一起拉起来：

| 启动方式 | 行为 | 适合 |
| --- | --- | --- |
| `auto`（默认） | 管理端启动时一并把实例拉起来，dsh 崩了自动重试 | 常用实例，打开管理端就该能用 |
| `ondemand` | 管理端启动时不拉；用户点进实例的那一刻才拉起 | 偶尔用、想省内存的实例 |

在管理台上**停止**一个实例只是让进程停下，不会改它的启动方式：管理端重启后，`auto` 的实例还会回来。想让它彻底不被拉起，请把它停用或改成 `ondemand`。

---

## 它到底做了什么

dsh 自己有一套认证：每个进程生成一个随机**启动令牌**，启动时打印在 `dsh web:` 那一行 URL 的 `?token=` 上。浏览器访问 `GET /?token=...` 才能换到 dsh 签发的会话 Cookie，之后每个 API 调用和 WebSocket 都校验它。

放到「管理端后面、从浏览器访问」的拓扑里，这套机制有两个麻烦：

1. 令牌不能发给每一个访问者——那等于谁拿到 URL 谁能用 agent；
2. 浏览器自己拿不到令牌，于是页面能打开，但所有 API 调用都返回 401。

dsh-gate 的解法是：**令牌只留在管理端进程内存里，由管理端替浏览器完成交换。** 每个实例挂在自己的 `/i/<id>/` 下，交换流程对每个实例各做一次：

```
1. 浏览器 GET /i/<id>/，身上没有「已交接」标记
      |
      v
2. 管理端给转发给该实例 dsh 的 URL 拼上 token=...
      |
      v
3. dsh 完成交换，写下自己的会话 Cookie，302 跳回干净的 ./
      |
      v
4. 管理端在这次响应里种下该实例的「已交接」标记 Cookie
   （路径改写到 /i/<id>/，所以同一域名下多个实例的标记互不覆盖）
      |
      v
5. 之后浏览器访问 /i/<id>/ 时带着标记，管理端不再重复注入（否则会无限 302）
```

标记是必要的，不是优化：dsh 的 302 目标就是 `/` 本身，没有标记就无法区分「第一次进来」和「刚被重定向回来」。标记里存的是「上次交接用的令牌指纹」：dsh 重启换了令牌之后，浏览器下次打开会自动重新交接一次；会话因为换地址或过期失效（401）时，管理端会清掉标记并重做一次交接，整个过程对用户不可见，表现为「页面自己就好了」。

还有两件事必须做对：

- **子路径的机械层**：dsh 的前端用相对路径取资源，管理端要把 `/i/<id>/xxx` 剥成上游的 `/xxx`；dsh 的会话 Cookie 名由 authority 推导、多个实例在同一个域名下会重名，所以上游 `Set-Cookie` 的 `Path` 必须改写到各自的 `/i/<id>/`，否则实例之间会串会话。`go/tests/portal-e2e.sh` 专门验这一条。
- **`Host` 头要原样转发**。dsh 会用收到的 `Host` 与被接受的 authority 比对，并要求 `Origin` 指向同一 authority（防 DNS rebinding 与跨站）。所以管理端不改写 `Host`，而是给每个实例的 dsh 传 `--trusted-host` 放行对外地址；改写成监听器地址会让 dsh 的 Origin 校验必然失败。

令牌的明文只在内存里流转：**不写日志、不出现在启动横幅上**——管理端转发 dsh 的启动输出时，会先把 `token=xxx` 脱敏成 `token=***` 再记进日志。唯一落盘的是实例自己目录下的一个 `0600` 小文件（`<实例 DSH_HOME>/.dsh-gate-token`），目的是让管理端重启后还能接管仍然活着的 dsh；原因与安全边界见后文「本机控制令牌」一节。

---

> 只想尽快跑起来？看一页纸的 [QUICKSTART.md](QUICKSTART.md)（也随每个发布包一起分发）。

## 管理端：一个入口，多个 dsh 实例

管理端把**账号、实例、授权都提升到管理端一级**：一个入口、一个管理页面、N 个彼此独立的 dsh，每个用户只看到属于自己的实例。

最省事的一条路是在页面里点：

```sh
dsh-gate install --start      # 建管理员、后台启动管理端；装完不拉起任何 dsh
# 打开提示里的 http://<本机>:12020/admin
#   → 「创建实例」→ 点「安装」→ 在「实例授权」里把实例分给用户
```

命令行与页面是同一套数据、同一条代码路径：

```sh
# 1. 建管理端账号（第一个管理员；install 已经建过就跳过）
dsh-gate manage user add admin --role admin --password-stdin

# 2. 建两个实例（各自的 DSH_HOME；不透明 id 与回环端口自动分配）
dsh-gate dsh add alice-main --dsh-home /srv/dsh/alice --owner alice
dsh-gate dsh add bob-main   --dsh-home /srv/dsh/bob   --owner bob

# 3. 为实例安装 dsh（命令行是同步的；也可以之后在管理台点「安装」）
dsh-gate dsh install alice-main --install-mode npx
dsh-gate dsh install bob-main   --install-mode npx

# 4. 把实例授权给对应用户（--owner 只是展示字段，不产生访问权限）
dsh-gate manage grant add alice alice-main
dsh-gate manage grant add bob   bob-main

# 5. 启动管理端（已经把管理端跑起来了就跳过）
dsh-gate start --port 12020
```

打开 `http://<服务器>:12020/` 登录后：普通用户看到「我的实例」卡片，点进去就是那个实例的 dsh（管理端在服务端完成启动令牌交接，浏览器不需要知道任何令牌）；管理员多一个 `/admin` 管理台（用户、实例、授权、运行环境、审计）。

![dsh-gate 管理台](docs/images/admin.png)

<sub>管理台一页看完：用户与角色、实例授权矩阵、运行环境检测、实例与安装、审计。
下图是其中的实例区块（每台实例一张卡片，几种安装方式各一例）与「安装中」状态——
实例安装与运行环境补齐都是后台任务，进度会自动刷新。</sub>

<p>
  <img src="docs/images/admin-instances.png" width="620" alt="实例卡片列表与创建实例表单">
  <img src="docs/images/admin-installing.png" width="620" alt="安装进行中，卡片显示「安装中」徽标与当前步骤「正在安装 dsh」">
</p>

<sub>手机宽度下每台实例仍是一张完整卡片，动作按钮自动排成「进入独占一行、其余均分」
两行，不用横向滚动。</sub>

<p>
  <img src="docs/images/admin-mobile.png" width="300" alt="手机宽度下的实例卡片与动作按钮">
</p>

### 管理台里装 dsh：先建记录，再装

管理台**不需要你事先装好 dsh**。页面打开就做一次**运行环境检测**，然后按实例选择安装方式。顺序是刻意的：**先创建实例记录**（立刻拿到不透明 id 与端口，页面马上可见），**再在后台安装** —— 安装动辄十几分钟，不能把 HTTP 请求挂在那儿等。

管理台底部有五个分页，顺序是 **实例 → 用户 → 实例授权 → 运行环境 → 审计**；**默认停在「实例」**，因为打开管理台最常做的事就是看实例、管实例。

- **实例**：每台实例是一张卡片，卡头是名字与四枚徽标（启用、运行状态（运行中 /
  启动中 / 未运行）、正在跑的 dsh 来源（「网关拉起」或「接管的外部实例」）、安装状态），
  卡身列出端口、实例目录、监听地址、启动方式、进程号、**安装方式**、**dsh 版本**、
  **启动命令**与**启动参数**，以及安装阶段与进度（排队中 / 安装中 / 已完成 / 失败，
  附步骤与报错）；卡底按当前状态给出「启动 / 停止 / 重启」中做得到的按钮，以及
  「安装 / 重装」「编辑」「删除」。
  运行中与启动中的卡片左缘各有一道绿 / 黄竖条。**安装方式与启动命令分两条摆**是刻意的：
  装到一半失败时启动命令还停在上一次那条，只标方式会误判。**dsh 版本**也单独一条：
  钉住的是 `0.2.2-alpha.2` 还是「跟随 latest」决定下一次装出来的是哪一份，装之前就该看得见
  （源码构建那一格会标出「不生效」，它认的是分支与标签）。**启动参数**是运行时在命令
  后面自动追加的那一段（`web --host <监听地址> --port <端口> --no-open`）：启动命令只
  说明 dsh 从哪来，填的端口到底传给了谁看这一行。
  - **编辑**收起在表单里：实例目录、配置目录、工作目录、端口、监听地址、启动方式、
    安装方式、dsh 版本与安装目录。点「保存配置」只落库、**不自动重启**；要让改动生效再点「重启」。
  - **删除**是可选的：表单上可以勾「删数据目录（`DSH_HOME`）」与「删安装目录」，
    默认都不勾（只解除管理端里的登记）。删除不可恢复，勾之前看清路径；管理端只会删
    绝对路径，并拒绝删根目录、当前用户家目录这类目标。实例在运行时，删除会先把它停掉。
- **用户**：列表、新建、改密、停用 / 启用、改角色（管理员 / 普通用户）、删除。第一个
  注册的账号是**超级管理员**（名字下面有徽标）：不能被改角色、停用或删除，
  任何管理员（包括它自己）都不行；改口令保留，否则忘了口令就把管理端锁死。
  改登录名只有命令行入口（`manage user rename`，页面上没有），超级管理员也可以改——
  超管身份由注册顺序决定，改名不改变它；默认账号 `admin` 想换成自己的名字就走这条。
  其余账号里，最后一个管理员不能被停用、降级或删除。
- **实例授权**：用户 × 实例的勾选矩阵，一次提交差异。管理员默认可以进入全部实例。
- **运行环境**：node / npm / npx / pnpm / git / dsh 各自的状态、版本与路径；缺哪个就
  告诉你补装办法。末尾还有平台、管理端自备的 Node 与 pnpm、本机已有的 dsh。
  机器上没有 Node 也不用先去装，管理端会自己下一份到状态目录。
- **审计**：登录、进实例、用户 / 实例 / 授权的增删改、实例启停、失败尝试，最近若干条。
- **缺 pnpm 时可以直接点「安装」**：dsh 的插件安装器走 pnpm，而 Node 只自带 npm/npx，
  干净的服务器上点「添加插件」会报 `spawn pnpm ENOENT`。点一下就装好——装到管理端状态
  目录（`<状态目录>/pnpm`），不碰系统、不需要 root，卸载时随 `uninstall --purge` 一起
  消失。与 dsh 安装一样是后台任务，进度会自动刷新；装好后那一行自己变成版本号。
  （git 不给按钮：它要走系统包管理器，发行版差异太大，给命令更稳妥。）

  ![dsh 运行环境检测](docs/images/admin-env.png)

**创建实例**表单的字段顺序是：名字 → dsh 数据目录（`DSH_HOME`）→ **配置目录** → 工作目录
→ **端口** → **监听地址** → **启动方式** → **安装方式** → **dsh 版本** → 安装目录。目录**只填一个就够**：
实例目录（`DSH_HOME`）是 dsh 唯一的数据根；配置目录是它的另一种写法（填了会顶替它，
一般留空）；安装目录只在 git / pnpm / local 方式下用（npx 走 npm 缓存），git 与 pnpm 留空时
自动用 `<实例目录>/install`；工作目录留空则用 `<实例目录>/workspace`。表单每栏都带
默认值提示，下面还有一块「这些目录分别放什么？」可展开。端口留空就自动分配，id 也是自动生成的。

四种安装方式，按你的机器条件挑：

| 方式 | 适合 | 它做什么 | 装出来的启动命令 |
|---|---|---|---|
| **npx**（默认） | 只想尽快用上，机器上没有 dsh | `npx --yes @deepseek-ai/dsh` 现拉现跑，装完还会冒烟验证一次 | `<自备 Node>/bin/npx --yes @deepseek-ai/dsh` |
| **git** | 要读源码、改代码、跟上游分支 | `git clone` 官方仓库 → `pnpm install` → `pnpm run build` | `node <安装目录>/deepseek-harness/apps/cli/lib/bin.js` |
| **local** | clone / 构建要你自己来（私有镜像、代理、离线环境） | **什么都不装**：只核对目录，然后照着官方 Run from source 的 `pnpm dsh web` 启动 | `pnpm --dir '<源码目录>' run dsh` |
| **pnpm** | 想离线复用、多实例各自固定版本 | 用 pnpm 把 dsh 装到**实例自己的目录**（不是全局） | `<安装目录>/dsh/node_modules/.bin/dsh` |

git、local 与 pnpm 方式会自动备齐 Node 运行时与 pnpm（装到状态目录，不动系统）。命令行与页面
走**同一套数据与代码路径**：

**local（本地源码）是指「克隆和构建我手动搞，网关只负责启动」**：先在别的终端里把官方仓库
clone 下来，跑完 `pnpm install` 与 `pnpm run build`，然后在安装目录里填那个仓库的根目录
（填它的父目录也认，仓库在 `<目录>/deepseek-harness` 就行）。网关不 clone、不装依赖、不构建，
只核对三件事：目录里有带 `dsh` 脚本的 `package.json`、依赖装过（`node_modules` 存在）、
`node` 与 `pnpm` 找得到。条条满足就把 `pnpm --dir '<源码目录>' run dsh` 记成启动命令；
`pnpm dsh web --host … --port …` 由网关在启动时接上。最先端那种「从源码安装总是失败」
（clone 超时、私有镜像、node-gyp）就绕开了——本地能构建成功，网关就能给你跑起来。
版本探测是尽力而为：取得到就显示，取不到不影响启动。

**装哪个版本**是 `dsh 版本` 这一栏（命令行 `--dsh-version`）。默认不用填：留空就是 `latest`。
想用上比 latest 更新的发布（alpha / rc 这类通道），填 npm 上的**版本号**（`0.2.2-alpha.2`）或
**dist-tag**（`next`、`alpha`），npx 与 pnpm 两种方式都认。有哪些名字可填：

```sh
npm view @deepseek-ai/dsh dist-tags        # latest / next / alpha 各指向哪个版本
npm view @deepseek-ai/dsh versions --json  # 全部版本号
```

两条规则要知道：

- **钉住就不回退**。留空时如果 latest 在这台机器上跑不起来，网关会自动退回实测可用的旧版本
  ——那是给「没说要哪个版本，能跑就行」准备的。你点了名，装不上就直接报「这个版本装不上」，
  不会悄悄换成另一个版本，让你以为自己拿到的是点过的那一个。
- **源码方式（git / local）不认这一栏**：git 认分支与标签（`--git-ref`），local 认你填的那个
  目录。同一个表单里两个都填，建实例时就被拦下，不用等安装跑到一半才失败。

版本跟着实例记录走：建实例时填的、编辑配置改的、`dsh install --dsh-version` 装的都写进同一条
记录，实例列表那一格显示当前钉的是哪个（留空显示「跟随 latest」）。**没点过「安装」的实例也认
这个值**——运行时自动探测出的启动命令会带上它，而不是回到 latest。

```sh
# 建记录时就把安装方式定下来（页面上的「创建实例」等价）
dsh-gate dsh add ui-app --dsh-home /srv/ui --config-dir /srv/ui/config \
    --workspace /srv/ui/work --dsh-port 18104 --listen-host 127.0.0.1 \
    --install-mode pnpm --install-dir /srv/ui/inst

# 也可以先建记录、之后单独装（或换方式重装）
dsh-gate dsh install ui-app --install-mode pnpm

# 本地源码：clone + pnpm install + pnpm run build 你自己来，网关只负责 pnpm dsh web 启动
dsh-gate dsh install dev --install-mode local --install-dir /srv/src/deepseek-harness

# 装比 latest 新的那一份：版本号或 dist-tag 都行，会一并写回实例记录
dsh-gate dsh install ui-app --dsh-version 0.2.2-alpha.2

# 取消钉住、回到跟随 latest
dsh-gate dsh edit ui-app --dsh-version ""
```

`dsh install` 在**命令行是同步的**（你人就坐在终端前等，进度直接打屏）；在管理台
是**后台任务**（页面刷新看进度）。失败不影响记录：实例还在，改完再装一次即可。

**要点**

- **子路径挂载**：每个实例挂在 `/i/<不透明 id>/` 下。同一域名下所有实例共用一套
  Cookie 名，所以管理端会把上游 Cookie 的 `Path` 改写到各自的挂载前缀 —— 这是「会话
  不串」的关键，也是 `go/tests/portal-e2e.sh` 重点验的一条。
- **越权与不存在同形**：访问没有授权的实例返回 404，响应体与「实例不存在」完全一致，
  不把「这个实例存在」当信息泄漏出去。
- **每个 dsh 只听回环**：实例的 dsh 绑定 `127.0.0.1:<自动分配的端口>`，对外只有管理端
  一个口。`ondemand` 的实例在用户点进来的那一刻才拉起。
- **命令与页面等价**：`dsh` 子命令覆盖实例的增删改查、安装、启停；`manage user|grant`
  覆盖用户与授权；`--lang zh-CN|en` 切换输出语言（也可用 `DSH_GATE_LANG` / `LC_ALL` / `LANG`）。
- **数据都在管理端数据库里**：`<状态目录>/portal.db`（0600，SQLite）。升级时若存在旧的
  单实例账号表 `users.json`，首次启动会自动导入账号（第一个用户成为管理员），旧口令继续
  可用。老库升级到新 schema 时，缺的字段会按「升级前的行为」补默认值，已有实例不受影响。
- **本机控制 API**：`dsh start|stop|restart|rm` 这类需要动进程的动作走管理端的
  本机接口（`POST http://127.0.0.1:<端口>/_portal/api/local`），只接受回环来源，凭据是
  `<状态目录>/portal.token` 里的随机令牌（每次启动刷新）。所以这些命令要求管理端正在运行，
  CLI 不会绕过管理端去偷偷拉一个没人认识的 dsh。

**目前的限制**（细节见 [docs/portal.md](docs/portal.md)）

- 同一个浏览器先后进入两个实例时，`localStorage` 按域名共享（UI 状态会互相看见，
  凭证不串）；
- 命令行改口令 / 停用用户后，**运行中的管理端不会立刻把人踢下线**（重启管理端，或让管理
  员在页面上做同样操作才会立即吊销会话）；
- PWA / 移动端「添加到主屏幕」在子路径下不承诺表现正常；上游插件若硬编码绝对路径资源，
  子路径下可能 404。

## 30 秒上手

一条命令装好管理端（会逐项询问、汇总确认，建好管理员并按你的选择启动）：

```sh
dsh-gate install --start
```

它会打印 `http://127.0.0.1:<端口>/` 与 `…/admin`。打开管理台，做两件事：

1. **实例**页 →「创建实例」：填名字（必填）、实例目录（留空即 `~/.dsh`）、安装方式；
2. 在实例列表里点「安装」，等后台装完，再点「启动」（第一次会自己下载 Node 运行时，慢一点）。

想把实例分给别人，去「实例授权」页勾选；对方用自己的账号登录后就能在「我的实例」里看到。

脚本 / CI 里不想要交互，就一次给全：

```sh
echo "$PASSWORD" | dsh-gate install --port 12020 --password-stdin --start
# 或让它生成一个随机口令并打印一次
dsh-gate install --random --start
```

`install` 也会把 `dsh-gate` 拷贝一份到固定位置（`--bin-dir` 可改，留空则不拷）：Linux /
macOS 是 `/usr/local/bin`，Windows 是 `%LOCALAPPDATA%\dsh-gate\bin`。之后敲 `dsh-gate`
即可（Windows 那个目录默认不在 `PATH` 里，想直接敲就自己加一下）；它会问你要不要装
开机自启（对应 `--service`）。

管理它：

```sh
dsh-gate status            # 管理端在不在跑、有几个实例、各自状态
dsh-gate dsh list          # 实例清单与运行状态（管理端没在跑也能看定义）
dsh-gate dsh restart app   # 按保存的配置重启某个实例（管理端要在运行）
dsh-gate stop              # 停掉管理端（连同它监督着的实例一起收尾）
```

日志在 `~/.dsh-gate/run/logs/portal.log`。关机重启后要自己起来，就装开机自启：

```sh
dsh-gate service install   # Linux 用 systemd、macOS 用 launchd、Windows 用计划任务
```

### 登录页在手机与浅色模式下的样子

<p>
  <img src="docs/images/login-mobile.png" width="300" alt="手机宽度下的登录页">
  <img src="docs/images/login-light.png" width="480" alt="浅色模式下的登录页">
</p>

登录页是**单个自包含的 HTML**（内嵌样式与脚本、无任何外部请求），因此断网的服务器
上一样能正常显示；配色默认跟随系统，页脚能一键切成亮色或暗色，另有石墨蓝 / 海蓝 /
绛紫 / 墨绿四种色相方案，登录后的界面还能在**控制台 / 总览 / 分栏**三种布局之间切换
（三个维度都记在 Cookie 里，三个页面共用），最小 320px 宽不破版。


## 使用教程

这一节按**真实顺序**走一遍：从拿到一个二进制，到局域网里能用 agent 为止。
每一步都给出「你会看到什么」和「出错了看哪里」。

### 第 0 步：准备

需要的东西很少：

| 条件 | 说明 |
|---|---|
| 一台机器 | 跑 dsh 与管理端的那台。Linux / macOS / Windows 都行，无界面服务器是主要场景 |
| 一个二进制 | 从 [dist](dist/) 取对应平台的包，或自己 `sh go/build.sh` 编译 |
| **不需要** Node.js | 本机没有 Node 时，管理端会自己下载官方运行时（见第 2 步） |
| **不需要** 公网 IP | 局域网内用；要从外面用请见[异地访问](#异地访问人在家里用公司的-dsh) |

把二进制放到一个稳定位置。Linux/macOS 上直接装进 `PATH`：

```sh
# 解压（Windows 用 .zip，解开就是 dsh-gate.exe）
tar -xzf dsh-gate_<版本>_linux_amd64.tar.gz
cd dsh-gate_<版本>_linux_amd64

# 装进 PATH；也可以先 ./dsh-gate 用起来，第 1 步的 install 会再帮你拷一份
sudo install -m755 dsh-gate /usr/local/bin/dsh-gate
dsh-gate version        # 确认能跑
```

> 不确定该下哪个包？`uname -m` 看架构（`x86_64` → amd64，`aarch64`/`arm64` → arm64），
> `uname -s` 看系统。Windows 上取 `windows_amd64`（或 `arm64`）。

### 第 1 步：安装管理端

```sh
dsh-gate install             # 以你自己的身份跑；macOS 装开机自启并不需要 root
```

> **别用 `sudo dsh-gate install`。** sudo 会保留 `$HOME`（macOS 就是如此），
> `~/.dsh-gate` 仍然指向你自己的状态目录，而以 root 写出来的数据库、令牌文件是 root
> 属主（0600）—— 之后你用普通用户跑 `dsh-gate status` / 管理端都会读不了。只是想把命令
> 装进 `PATH` 的话，先 `sudo install -m755 dsh-gate /usr/local/bin/`，再以自己身份运行
> `dsh-gate install`。

交互式终端里它会问这些，回车接受括号里的默认值（**大写字母就是默认值**：`[Y/n]`
回车＝是，`[y/N]` 回车＝否）：

```
监听地址 [0.0.0.0]:
管理端端口 [12020]:
安装目录 [/usr/local/bin]:
配置与数据库目录 [/Users/you/.dsh-gate]:
管理员账号 [admin]:
姓名（给人看，可中文）[admin]:
管理员 admin 的口令（输入显示为 *）:     # 要输两遍，两次必须一致
现在就启动管理端？[y/N]
```

几个容易踩的点：

- **端口会被当场检查**：不是数字、超出范围、或端口已被占用，向导都会当场拒绝并
  要求重填（被占用时还会说清是哪个进程占的）。别忽略这条——等启动失败才发现，
  账号和开机自启都已经装好了，得回头重来。
- **只建管理员，不建实例**：`install` 结束时会打印接下来的两步（建实例、授权），
  它**不会拉起任何 dsh**。实例是管理端的东西，装管理端的时候还没有。
- **重复执行是安全的**：管理端里已有账号时不会改任何人的口令，也不会重复建号；
  向导也不再问账号与姓名，改为在确认页列出已有账号（账号、姓名、角色、状态），
  超级管理员那栏直接写「超级管理员」。
- **口令的来源**：交互式终端里问两遍；脚本里必须用 `--random` 或 `--password-stdin`，
  否则直接报错（不是交互终端时没法安全地读口令）。

结束时它会：建好管理员、把 `dsh-gate` 拷一份到 `PATH`，并按你的选择后台启动
（`--start`）或装开机自启（`--service`，蕴含立即启动）。**Linux 上写 systemd 单元需要
root**：如果以普通用户跑并选了自启，这一步会失败——先以自己身份 `install`，再执行
`sudo dsh-gate service install --state-dir "$HOME/.dsh-gate" --user "$USER"`（见
[服务器部署](#服务器部署linux--systemd)）。

### 第 2 步：建实例并装好 dsh

管理端起来了，但一个 dsh 都还没有。两种做法等价：

**页面（推荐第一次用）**：打开 `http://<本机>:12020/admin`，默认就在「实例」页：

1. 「创建实例」：名字必填，实例目录留空就用 `~/.dsh`，端口留空自动分配，
   安装方式先用默认的 `npx`，**dsh 版本**留空就是 latest（想装更新的就填版本号或 `next`）；
2. 在实例列表里点「安装」——这是后台任务，进度会自动刷新；
3. 装完点「启动」。第一次会下载 Node 运行时与 dsh 包，3–5 分钟；之后重启是秒级。

**命令行**：

```sh
dsh-gate dsh add app --dsh-home ~/.dsh-app --start-mode auto
dsh-gate dsh install app --install-mode npx
dsh-gate dsh start app
```

第一次安装的日志里会依次看到（时间来自干净容器里的实测）：

```
本机没有 npx，正在下载 Node.js 运行时（不改系统，只放进管理端状态目录）   ← 约 15 秒
Node.js 运行时已就绪  bin=~/.dsh-gate/node/node-vXX/bin
pnpm 已就绪（dsh 的插件安装器会用它）                                ← dsh 装插件要用
dsh 启动命令已就绪（npx --yes @deepseek-ai/dsh）
```

**这一步联网**：下 Node 与 dsh 包。官方源慢或连不上时会自动改用国内镜像
（`registry.npmmirror.com`），日志里能看到「改用镜像源重试」。全部就绪通常 3–5 分钟，
之后重启是秒级（Node 不重复下载，dsh 走 npx 缓存）。

管理端**立刻**就在监听，登录页马上能打开；慢的只是实例里 dsh 的准备过程。

### 第 3 步：访问并登录

浏览器打开管理端的**局域网**地址：

```
http://192.168.1.20:12020/
```

1. 打不开 / 连接被拒绝 → 管理端没在跑：`dsh-gate status`；
2. 看到登录页 → 用第 1 步的账号口令登录；
3. 登录后是「我的实例」；管理员还能从右上角进 `/admin`；
4. 点实例进去 → 如果 dsh 还在启动，会看到每 2 秒自刷新的等待页；就绪后直接开始对话。

> 从别的电脑/手机访问不了，但本机能开？多半是防火墙：Linux `ufw allow 12020/tcp`，
> 云服务器还要放行安全组。

### 第 4 步：配置模型（不装任何东西也能配）

模型配在**实例自己的** `DSH_HOME` 里，所以每条命令都要用 `--instance` 指定实例
（`dsh-gate dsh list` 查看实例名），不再靠 `--dsh-home` 猜目录。

**官方 DeepSeek**（DSH 自带这个路由，只需密钥）：

```sh
printf 'sk-你的密钥' | dsh-gate model key set DEEPSEEK_API_KEY --stdin --instance app
dsh-gate model default deepseek-official deepseek-chat --instance app
dsh-gate model list --instance app        # 确认写进去了
```

**第三方 / 自建网关**（任何兼容 OpenAI 协议的服务）：

```sh
printf '你的密钥' | dsh-gate model key set MY_GW_API_KEY --stdin --instance app
dsh-gate model provider add my-gw --base-url https://api.example.com/v1 \
    --name 我的网关 --key-env MY_GW_API_KEY --models qwen3-max,glm-5 --instance app
dsh-gate model default my-gw qwen3-max --instance app
```

改完可能需要重启该实例才读到：`dsh-gate dsh restart app`。

> 浏览器里的「设置 → 模型」页也可以配，但它依赖 `dsh-dock` 插件（见下一步）；
> 插件装不上时，上面这两条命令就是唯一的办法——它们直接改 DSH 自己的设置文件，
> 不需要任何插件或页面。

### 第 5 步：装插件（可选，但推荐）

`dsh-dock` 提供模型设置页、余额、通知等功能坞页面。管理端可以绕过浏览器里的插件页直接装：

```sh
dsh-gate plugin list --instance app               # 看已装了什么
dsh-gate plugin add dsh-dock --instance app       # 官方源失败会自动换镜像源
dsh-gate dsh restart app                                 # 装完要重启该实例才加载
```

`plugin list` 还会顺带检查一件常见故障：浏览器设置页打不开（`settings are unavailable
in this browser`）通常是因为配置档里缺 `@deepseek-ai/dsh-web-app`——清单里会指出来。

### 第 6 步：日常运维

```sh
dsh-gate status                  # 管理端状态 + 实例定义与运行状态
dsh-gate dsh list                # 实例清单（名字、id、端口、运行状态）
dsh-gate dsh stop app            # 停掉某个实例（不影响管理端）
dsh-gate dsh start app           # 启动某个实例
dsh-gate dsh restart app         # 按最新配置重启某个实例
dsh-gate restart                 # 重启管理端（先停再按上次保存的配置拉起）
dsh-gate service status          # 开机自启装了没、单元在哪
```

日志在哪：

| 运行方式 | 日志位置 |
|---|---|
| 前台运行（`start --foreground`，或服务托管） | 终端 / `journalctl -u dsh-gate -f` |
| 后台运行（`start` 默认） | `~/.dsh-gate/run/logs/portal.log` |
| 服务托管（Linux） | `journalctl -u dsh-gate -f` |
| 服务托管（macOS） | `~/Library/Logs/dsh-gate.log` |

实例的 dsh 输出也转发进管理端日志（启动令牌已脱敏）。

### 第 7 步：不用了怎么卸

单个实例：`dsh-gate dsh rm app` 只解除登记、保留数据目录；数据也不要了就加
`--purge`，连安装目录一起删再加 `--purge-install`。

整个管理端：

```sh
dsh-gate uninstall                  # 交互式：先问是否删除所有实例的数据与会话记录
dsh-gate uninstall --yes            # 非交互：默认保留实例数据
dsh-gate uninstall --yes --remove-data          # 连实例数据与会话记录一起删
dsh-gate uninstall --yes --remove-data --purge  # 再删管理端状态目录（账号、日志、运行时）
```

顺序是固定的：**先停止并删除所有实例**（选了删数据才动 DSH_HOME），**再停止管理端**，
**最后才卸载系统服务**。默认不删实例数据目录（那些是你的会话、凭据与工作区），结束时
会把保留的目录列出来。`--purge` 删的是 `~/.dsh-gate`，不可恢复；`dsh-gate` 可执行文件
也不删：按你的安装方式自行处理。

### 出问题了先看这三处

1. **`dsh-gate status`**——管理端到底跑着没、端口是多少、实例是什么状态；
2. **`journalctl -u dsh-gate -f`（或 `~/.dsh-gate/run/logs/portal.log`）**——
   dsh 为什么没起来、下载卡在哪一步、安装任务报了什么错；
3. **`/admin` 的「运行环境」页**——node / npm / npx / pnpm / git / dsh 找不找得到，
   缺什么点一下就能补。

常见症状直接对着[常见问题](#常见问题)查：页面报找不到 npx、插件的 `spawn pnpm ENOENT`、
`settings are unavailable in this browser`、`dsh web authentication required`、
工作目录没生效——每一条都有排查步骤。

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

需要 Go 1.24 或更高版本。`go build` 只会为管理端的 SQLite 驱动解析依赖，其余全部是标准库。

不想手工拷也有现成的两条路：`dsh-gate install` 与 `sudo dsh-gate service install`
默认都会把可执行文件拷一份到 `/usr/local/bin`（`--bin-dir` 可改，留空则不拷），
装完就能直接敲 `dsh-gate`，不必再 `./dsh-gate`。它是**拷贝**而不是软链：原目录
被清理也不影响命令可用，服务单元里记录的也是拷贝后的路径。

服务器上没有 Node.js 也不用先去装：管理端启动时会自己下载官方 Node.js
运行时到状态目录（默认 `~/.dsh-gate/node`，不需要 root、不改系统），并用它
启动实例的 dsh；`uninstall --purge` 会连它一起删掉。

### 交叉编译打包

仓库自带构建脚本，一次产出全部平台的可执行文件与压缩包：

```sh
sh go/build.sh                 # 全部目标
sh go/build.sh linux/arm64     # 只编译指定目标
```

产物在 `dist/<版本号>/`，每个平台一个目录（可执行文件 + LICENSE + README），
外加 `.tar.gz`（Windows 是 `.zip`）与 `SHA256SUMS`：

```
dist/0.3.20/
├── dsh-gate_0.3.20_darwin_arm64/dsh-gate        dsh-gate_0.3.20_linux_arm64/dsh-gate
├── dsh-gate_0.3.20_darwin_arm64.tar.gz          dsh-gate_0.3.20_linux_arm64.tar.gz
├── dsh-gate_0.3.20_darwin_amd64/…               dsh-gate_0.3.20_linux_arm/dsh-gate   （树莓派等 armv7）
├── dsh-gate_0.3.20_darwin_amd64.tar.gz           dsh-gate_0.3.20_windows_amd64/dsh-gate.exe
├── …                                           dsh-gate_0.3.20_windows_arm64/…
└── SHA256SUMS
```

dsh-gate 是**纯 Go**，`CGO_ENABLED=0`（SQLite 驱动也是纯 Go 实现），所以这些二进制都是
静态的：`scp` 到目标机就能跑，不需要目标机装 Go，也不需要任何 C 运行时。

脚本本身很简单，交叉编译也可以直接手写：

```sh
GOOS=windows GOARCH=amd64 go build -o dsh-gate.exe ./cmd/dsh-gate
GOOS=linux   GOARCH=arm64 go build -o dsh-gate      ./cmd/dsh-gate
```

版本号默认取 `go/internal/version/version.go` 里的 `Version`（全项目唯一真源）；
脚本用 `-ldflags` 注入，手工构建时也可以：

```sh
go build -ldflags "-X github.com/wycto/dsh-gate/internal/version.Version=0.3.20" -o dsh-gate ./cmd/dsh-gate
```

---

## 命令参考

dsh-gate 只有一种形态：**管理端**。顶层子命令就是管理端的完整生命周期，加上管理
dsh 实例、用户授权、插件与模型配置的辅助命令。不带子命令时打印总帮助；只给选项不给
子命令会提示先写子命令（不会再猜「默认启动什么」）。

```
dsh-gate install [选项]        安装管理端：建管理员，可后台启动/开机自启
dsh-gate start [选项]          启动管理端（默认后台；装了服务时转给服务管理器）
dsh-gate stop [选项]           停止整个管理端
dsh-gate restart [选项]        重启整个管理端（按保存的配置或服务管理器）
dsh-gate status [选项]         管理端状态 + 实例定义
dsh-gate uninstall [选项]      停止并删除所有实例，然后卸载管理端服务
dsh-gate service <子命令>      开机自启（只托管管理端）
dsh-gate dsh <子命令>          管理 dsh 实例
dsh-gate manage <子命令>       用户与实例授权
dsh-gate plugin / model        插件与模型的辅助命令
dsh-gate version               打印版本号
```

退出码：`0` 成功，`1` 运行失败，`2` 参数用法错误。

公共选项：`--lang zh-CN|en` 可写在任意子命令前（也可用 `DSH_GATE_LANG` / `LC_ALL` /
`LANG`）；`dsh` 与 `manage` 的子命令认 `--db` 与 `--state-dir`。

`--state-dir` 默认 `~/.dsh-gate`；`--db` 默认 `<状态目录>/portal.db`。**运行登记目录**
固定是 `~/.dsh-gate/run`（`--run-dir` 可改），它不跟随 `--state-dir`：「这台机器上跑着
哪个管理端」是机器级事实，固定下来才能让 `status` / `stop` / `restart` 不必先知道配置。

### dsh-gate install

安装管理端：建管理员账号，可选后台启动或设为开机自启。**不拉起任何 dsh 实例**。

交互式终端里逐项询问（回车接受默认值），**答完会汇总确认一遍，回答 `n` 可以带着当前
答案重答**，不用整个重来；管道 / CI 里一个都不问。

```sh
dsh-gate install
# 全程交互

dsh-gate install --port 12099 --random --start
# 脚本模式：非默认项用选项给出，装好并后台运行

echo "$PASSWORD" | dsh-gate install --password-stdin --service
# CI / 脚本里设置口令，并安装开机自启
```

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--host` | `0.0.0.0` | 管理端监听地址 |
| `--port` | `12020` | 管理端监听端口 |
| `--state-dir` | `~/.dsh-gate` | 配置与数据库目录 |
| `--admin` | `admin` | 管理员账号（ASCII；中文填 `--display` 的姓名） |
| `--display` | 空 | 姓名（给人看，可中文；留空回退账号名） |
| `--random` | 关 | 自动生成强口令并打印一次 |
| `--password-stdin` | 关 | 从标准输入读取口令 |
| `--start` | 关 | 装完立即把管理端放到后台运行 |
| `--service` | 关 | 安装开机（登录）自启并立即启动（蕴含 `--start`） |
| `--bin-dir` | Linux/macOS `/usr/local/bin`，Windows `%LOCALAPPDATA%\dsh-gate\bin` | 把 dsh-gate 拷到这个目录；留空则不拷。服务单元 / 计划任务用拷贝后的路径，原目录被清理也不影响自启 |

管理端里已有账号时，`install` 只做启动 / 自启那一步，不会改任何人的口令；重复执行是安全的。
Linux 上 `--service` 需要 root：普通用户请先 `install`，再用
`sudo dsh-gate service install --state-dir "$HOME/.dsh-gate" --user "$USER"` 装自启
（见[服务器部署](#服务器部署linux--systemd)）。

### dsh-gate start

启动管理端。**默认后台运行**：命令很快返回，日志写 `<登记目录>/logs/portal.log`；
`--foreground` 留在前台（服务单元和排查问题用它）。

**不给启动参数时沿用上次保存的配置**（`<状态目录>/portal.json`）——`dsh-gate start`
最自然的预期是「按上次那样起来」，而不是把端口悄悄改回 12020。给了任何一个启动参数，
就按这次给的启动，并把参数保存下来供 `restart` / `status` 使用。

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--host` | `0.0.0.0` | 管理端监听地址 |
| `--port` | `12020` | 管理端监听端口 |
| `--tls-cert` | 空 | HTTPS 证书文件，与 `--tls-key` 成对提供 |
| `--tls-key` | 空 | HTTPS 私钥文件，与 `--tls-cert` 成对提供 |
| `--public-url` | 空 | 对外公告地址（例如 `https://dsh.example.com/`），留空按局域网 IP 推导 |
| `--state-dir` | `~/.dsh-gate` | 管理端状态目录 |
| `--db` | `<状态目录>/portal.db` | 管理端数据库路径 |
| `--npm-registry` | 空 | 安装 dsh 用的 npm 源；留空用默认，连不上时自动改用镜像源重试 |
| `--trusted-host` | 空 | 额外信任的地址（管理端域名），可重复 |
| `--dsh-start-timeout` | `5m` | 每个实例拉起 dsh 后等它监听端口的最长时间；首次启动要下载 dsh 与插件，给短了会边下边被杀、每轮重来 |
| `--foreground` | 关 | 前台运行（默认后台） |

```sh
dsh-gate start                           # 按上次配置后台启动
dsh-gate start --port 8080               # 换端口后台启动（并记住）
dsh-gate start --foreground              # 前台启动
```

管理端是机器级单例：一个管理端数据库对应一个进程。启动时会刷新本机控制令牌
（`<状态目录>/portal.token`），并把该拉起的 `auto` 实例一起拉起。

### dsh-gate stop

停止管理端，**连同它监督着的 dsh 实例**一起收尾。没有在运行时什么都不做（残留的登记会
被清掉，退出码仍是 0）。

```sh
dsh-gate stop
dsh-gate stop --wait 60s     # 多等一会儿它收尾
```

它走管理端自己的停机端点（`POST /__dsh_gate/shutdown`，只接受回环来源），因此三个平台
行为一致：先断新请求，再让实例的 dsh 优雅退出。不用 PID 文件加信号是刻意的：信号要在
每个平台各写一份，而 Windows 上根本没有 SIGTERM，只能退化成强杀。

`stop` 不接受位置参数——**停单个实例用 `dsh-gate dsh stop <名字>`**。

选项：`--wait DUR`（等待端口关闭，默认 20 秒）、`--run-dir DIR`。

### dsh-gate restart

按**上次保存的配置**重启管理端：先停掉，再用 `<状态目录>/portal.json` 里的参数拉起。

```sh
dsh-gate restart
```

没有保存过配置时报错并提示先 `install` / `start`，而不是猜一组默认值：默认端口可能正被
别的程序占着，猜出来的「重启成功」只是把一个空壳服务放到了另一个端口上。

选项：`--state-dir DIR`、`--wait DUR`（默认 20 秒）、`--run-dir DIR`。

### dsh-gate status

查看管理端是否在运行、上次的启动配置，以及已经登记的 dsh 实例。管理端没在跑时也能用
（实例定义在数据库里），这正是排查「为什么打不开」的第一步。

```
$ dsh-gate status
管理端：运行中
  监听地址  0.0.0.0:12020
  访问地址  http://192.168.1.20:12020/
  进程号    51234
  版本      0.3.20
  已运行    2 小时 13 分钟
  日志      /Users/me/.dsh-gate/run/logs/portal.log

实例：共 2 个
  alice-main           k3m9f2qa   :32101  运行中
  bob-main             p7d2xw1c   :32102  未运行

操作：dsh-gate dsh list / dsh-gate dsh restart <名字>；页面：/admin
```

管理端没在跑时，它会显示「上次配置」并提示 `dsh-gate start`；实例的运行状态标注为
「运行状态以管理台为准」。

选项：`--state-dir DIR`、`--db PATH`、`-v`（附带显示管理端启动命令）、`--run-dir DIR`。

### dsh-gate uninstall

卸载整个管理端，顺序固定：

1. 询问是否删除实例数据与会话记录（`--yes` 时由 `--remove-data` 决定）；
2. 停止并删除**所有** dsh 实例（管理端在跑时走本机控制 API，否则直接改库）；
3. 停止管理端（服务托管时走服务管理器，避免被 `Restart=always` 拉回来）；
4. 最后卸载系统服务。

```sh
dsh-gate uninstall                  # 交互式确认（无实例时直接卸载）
dsh-gate uninstall --yes            # 非交互，默认保留实例数据
dsh-gate uninstall --remove-data    # 连实例数据目录与会话记录一起删
dsh-gate uninstall --purge          # 再删管理端状态目录（账号库、日志、自备运行时）
```

**默认不删 dsh 实例的数据目录**：那些是用户的数据（会话、凭据、工作区）。选保留时命令
结束会把目录列出来；实例的 dsh **安装目录从不删除**。`dsh-gate` 可执行文件也不删。

选项：`--yes`、`--remove-data`、`--purge`、`--state-dir DIR`、`--wait DUR`、`--run-dir DIR`。
`--service` 仅为兼容旧写法保留（现在卸载总会移除系统服务）。`uninstall` 不接受位置
参数——**删单个实例用 `dsh-gate dsh rm <名字>`**。

### dsh-gate service

把**管理端**交给系统的服务管理器托管，从而开机（登录）自动启动。三个平台都支持：

| 平台 | 机制 | 安装位置 | 需要 root |
|---|---|---|---|
| Linux | systemd 系统服务 | `/etc/systemd/system/dsh-gate.service` | 是 |
| macOS | launchd 的 LaunchAgent | `~/Library/LaunchAgents/com.wycto.dsh-gate.plist` | 否 |
| Windows | 计划任务（登录时启动） | 任务计划程序里的 `dsh-gate` | 否 |

```sh
dsh-gate service unit [选项]       # 只打印将要安装的配置内容，不做任何改动
sudo dsh-gate service install [选项]   # 安装并立即启动
dsh-gate service status           # 看当前托管状态
sudo dsh-gate service uninstall   # 取消开机自启
```

托管的命令固定是 **`dsh-gate start --foreground`** 加上下面这些参数——服务管理器自己
负责重启与日志，管理端不必也不该再后台化一次：

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--host` | `0.0.0.0` | 管理端监听地址 |
| `--port` | `12020` | 管理端监听端口 |
| `--state-dir` | `~/.dsh-gate` | 管理端状态目录 |
| `--tls-cert` / `--tls-key` | 空 | HTTPS 证书与私钥（成对） |
| `--public-url` | 空 | 对外公告地址 |
| `--db` | `<状态目录>/portal.db` | 管理端数据库路径 |
| `--npm-registry` | 空 | 安装 dsh 用的 npm 源 |
| `--trusted-host` | 空 | 额外信任的地址，可重复 |
| `--dsh-start-timeout` | `5m` | 每个实例等自己的 dsh 监听端口的最长时间（同 `dsh-gate start`） |
| `--user` | 当前用户 | 以哪个用户身份运行（Linux） |
| `--working-dir` | 用户家目录 | 管理端进程的工作目录 |
| `--binary` | 当前运行的真实路径 | dsh-gate 可执行文件的绝对路径 |
| `--bin-dir` | Linux/macOS `/usr/local/bin`，Windows `%LOCALAPPDATA%\dsh-gate\bin` | 把 dsh-gate 拷到这个目录；留空则不拷。服务单元 / 计划任务用拷贝后的路径，原目录被清理也不影响自启 |

单元里的 `ExecStart` 形如：

```
ExecStart=/usr/local/bin/dsh-gate start --foreground --host 0.0.0.0 --port 12020 --state-dir /home/you/.dsh-gate
```

`--bin-dir` 会在写单元之前先把命令拷到目标目录，单元里用拷贝后的路径——将来原目录被
清理也不影响开机自启。

macOS 上是**用户级** LaunchAgent（登录时启动），属于登录用户而不是 root：直接
`dsh-gate service install` 即可，不需要 sudo；在 root 登录的环境里装不了（没有可用的
GUI domain），工具会直接说明这一点。

Windows 上是一条**登录时启动**的计划任务，以当前用户身份、最低权限运行：直接
`dsh-gate service install` 即可，**不需要管理员权限**。注册走的是任务 XML（不是
`schtasks /SC ONLOGON` 那套需要存凭据的开关），`service unit` 打印的就是这份 XML，
也可以在「任务计划程序」里手动导入。

任务的动作用的是 `dsh-gate service run`：它只是一个**启动器**——当场把管理端派成一个
**没有控制台**的子进程（DETACHED_PROCESS），自己随即退出。所以那个终端窗口只闪一下，
而且**没有任何窗口能把它关掉**：挂在控制台上的进程，窗口一关就会被控制台宿主强制
结束（`CTRL_CLOSE_EVENT` 的处理器只有几秒钟，超时直接 TerminateProcess），屏蔽信号
救不了它，只有「根本不在那个控制台上」才行。也不给进程摘控制台（`FreeConsole`）——
控制台归自己独占时摘掉会让 Go 运行时以致命错误当场终止，实测抓到过
`runtime.preemptM: duplicatehandle failed` 与 `runtime: netpoll failed` 两种。
要停就用 `dsh-gate stop`（走管理端自己的停机端点，不靠信号，也与「结束任务」无关）。
日志写在 `<状态目录>/run/logs/portal.log`。命令默认拷到
`%LOCALAPPDATA%\dsh-gate\bin`（`--bin-dir` 可改），计划任务指向这份拷贝——解压出来的
目录删掉也不影响开机自启；升级时安装会先停下运行中的管理端再覆盖它。

> 管理端由服务管理器托管后，`dsh-gate start` / `stop` / `restart` 会自动转给服务管理器
> （systemd 单元有 `Restart=always`，直接杀进程会被拉回来），所以不用再记
> `systemctl stop dsh-gate` 这套；手工操作 systemctl 仍然有效。

### dsh-gate dsh

管理 dsh 实例，与管理台 `/admin` 的实例页能力对等。实例定义存在管理端数据库里；
**启动 / 停止 / 重启 / 删除**这类要动进程的动作走管理端的本机控制 API，所以要求管理端
正在运行（没在运行时 `dsh list` / `add` / `edit` / `rm` 仍能直接读写数据库）。

```
dsh-gate dsh list
dsh-gate dsh add <名字> [字段选项]
dsh-gate dsh edit <名字|id> [同 add 的字段，只改显式给出的]
dsh-gate dsh rename <旧名|id> <新名>
dsh-gate dsh start|stop|restart <名字|id>
dsh-gate dsh install <名字|id> [--install-mode …] [--dsh-version …]
dsh-gate dsh rm <名字|id> [--purge] [--purge-install]
```

- `list`：列出全部实例（名字、不透明 id、端口、运行状态、`DSH_HOME`）。管理端没在跑时
  只显示定义，并提示 `dsh-gate start`。
- `add`：新建实例记录，**不安装 dsh**（安装是下一条命令，或页面上的「安装」按钮）。
  端口留空自动分配，id 自动生成。`--owner` 只是展示字段，不产生访问权限。
- `edit`：只改命令行显式给出的字段，其余保持原值。改完要在实例重启后生效（命令会提示）。
- `rename`：只改展示名；id、授权与已有会话都不变，实例名可以写中文。
- `start|stop|restart`：走本机控制 API，交给管理端的监督者执行。
- `install`：同步安装 / 重装 dsh，装完把启动命令写回实例记录。
- `rm`：删除实例。默认只解除管理端里的登记；`--purge` 删实例数据目录（实际生效的
  `DSH_HOME`），`--purge-install` 删安装目录。管理端在运行时会先停实例再删目录；
  没在运行时直接改库，并提醒你确认有没有残留的 dsh 进程。

`add` / `edit` 的字段选项：

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--dsh-home` | `~/.dsh` | 实例目录（`DSH_HOME`）；留空用 dsh 自己的默认目录 |
| `--config-dir` | 空 | 配置目录；留空沿用实例目录。填了会**顶替** `DSH_HOME`（dsh 没有独立的配置目录） |
| `--workspace` | `<实例目录>/workspace` | 该实例的默认工作目录（agent 能读写的范围） |
| `--dsh-port` | 自动分配 | dsh 回环端口 |
| `--listen-host` | `127.0.0.1` | dsh 监听地址；改成 `0.0.0.0` 才会允许外部直连该 dsh（不推荐） |
| `--start-mode` | `auto` | 启动方式：`auto`（随管理端启动）/ `ondemand`（有访问才拉起） |
| `--on-demand` | 关 | 等价于 `--start-mode ondemand`（`--start-mode` 优先） |
| `--idle-timeout` | `0` | 空闲回收秒数（`0` = 不回收；字段已就位，回收器尚未实现） |
| `--install-mode` | `npx` | `npx` / `git` / `local` / `pnpm` |
| `--install-dir` | `<实例目录>/install` | dsh 程序本体目录（npx 方式不需要；`local` 填你已 clone 并构建好的源码仓库根目录） |
| `--dsh-version` | 空（跟随 latest） | 要装的 dsh 版本号或 dist-tag（`0.2.2-alpha.2`、`next`、`alpha`）。填了就是**钉住**：装不上直接报错，不会自动退回别的版本。源码方式（`git` / `local`）不认这一栏 |

`install` 的选项：`--install-mode`、`--install-dir`、`--dsh-version`
（版本号或 dist-tag，留空沿用实例记录里钉住的那个；装完一并写回记录）、`--npm-registry`
（换镜像源，例如 `https://registry.npmmirror.com`）、`--git-url`、`--git-ref`。

```sh
# 建一个实例（端口与 id 自动分配）
dsh-gate dsh add app --dsh-home ~/.dsh-app --start-mode auto

# 改成按需启动，并把端口固定下来
dsh-gate dsh edit app --start-mode ondemand --dsh-port 18104

# 换一种方式重装（命令行同步等待，进度直接打屏）
dsh-gate dsh install app --install-mode pnpm --npm-registry https://registry.npmmirror.com

# 装比 latest 新的那一份：版本号或 dist-tag 都行
dsh-gate dsh install app --dsh-version next

# 启停与改名
dsh-gate dsh restart app
dsh-gate dsh rename app 我的助手

# 删除：默认保留数据；确认不要了再连带删目录
dsh-gate dsh rm 我的助手 --purge --purge-install
```

`--purge` 的删除有安全检查：只删绝对路径，并拒绝删根目录、当前用户家目录这类目标。

### dsh-gate manage

管理端一级的用户与授权。管理页面在浏览器里（`/admin`），这里是等价能力的命令行入口。

```
dsh-gate manage user add <账号> [--display <姓名>] [--role admin|user] [--random|--password-stdin]
dsh-gate manage user passwd <账号> [--random|--password-stdin]
dsh-gate manage user role <账号> <admin|user>
dsh-gate manage user rename <旧名> <新名>
dsh-gate manage user disable|enable <账号>
dsh-gate manage user list
dsh-gate manage user rm <账号>

dsh-gate manage grant list [<名字>]
dsh-gate manage grant add <用户> <实例名或 id>
dsh-gate manage grant rm  <用户> <实例名或 id>
```

`user add` 的 `--role` 默认 `user`；**表里第一个账号无条件成为管理员兼超级管理员**
（哪怕这里写了 `--role user`）——它是管理端的兜底身份。**账号是登录用的机器接口**：
字母或数字开头，可用字母、数字与 `. _ -`，不支持中文；给人看的中文名走 `--display`
（页面上叫「姓名」，留空回退账号名）。口令交互输入（输两遍，
显示为 `*`），脚本里用 `--random` 或 `--password-stdin`。口令至少 8 个字符、最多 256 个
字符（按字符数计，中文口令不会因为 UTF-8 被算得更长）。

`user rm`、`user disable`、`user role` 都会拒绝操作超级管理员（第一个注册的账号），
也会拒绝让管理端失去最后一个管理员。`user rename` 不在拒绝之列：超管身份的依据是
「第一个注册（id 最小）」而不是登录名，改名不动 id，改完之后它照样是超级管理员——
把默认的 `admin` 换成自己的名字是安全的。改名只改登录名，展示名如果还回退在旧登录名上
会跟着换；该用户已有的登录会话在下次使用时失效，需要用新名字重新登录。

**命令行管不到的边界**：管理端的登录会话在**进程内存**里，所以命令行改口令 / 停用用户
之后，运行中的管理端不会立刻把人踢下线——重启管理端，或让管理员在页面上做同样的操作
才会立即吊销会话。改名不受此限：会话主体是旧登录名，查不到账号时会被就地作废。

### dsh-gate plugin

直接管理某个实例的 dsh 配置档（profile）里的插件：加依赖、装包、写进 bundle 清单。
用 `--instance` 指定实例。做的事与 dsh
自己的「添加插件」完全一样，但会用上管理端自备的 Node/pnpm 与自动换源——**服务器上缺
这些的时候，浏览器里的插件页是装不动的**（实测报 `spawn pnpm ENOENT`，或者官方源
180 秒超时）。

```sh
dsh-gate plugin list --instance app                  # 看当前装了哪些插件
dsh-gate plugin add dsh-dock --instance app          # 装一个插件（失败自动换源）
dsh-gate plugin add dsh-dock @deepseek-ai/dsh-web-app --instance app
```

机制与 dsh 插件管理器一致：在 `<dsh-home>/profiles/web/` 里跑 `pnpm add <包名>`，再把
包名追加到该目录 `package.json` 的 `dsh.profile.bundles`（dsh 靠这份清单决定加载哪些
bundle）。**装完要重启该实例的 dsh 才生效**——profile 解析在进程启动时建立。

配置档目录由 dsh 自己创建（第一次启动时）。还没启动过 dsh 的话，命令会明确告诉
你先启动一次，而不是凭空造一个可能少挂载点的 profile。

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--instance NAME` | 必填 | dsh 实例名或 id（`dsh-gate dsh list` 查看） |
| `--dsh-home DIR` | 空 | 兼容/高级：直接指定实例目录；给了就优先于 `--instance` |
| `--state-dir DIR` | `~/.dsh-gate` | 管理端状态目录（找 portal.db 用） |
| `--db PATH` | 空 | 管理端数据库路径；默认 `<状态目录>/portal.db` |
| `--db PATH` | 空 | 管理端数据库路径；默认 `<状态目录>/portal.db` |
| `--profile NAME` | `web` | 配置档名；管理端拉的是 `dsh web`，所以是 `web` |
| `--npm-registry URL` | 空 | 拉包用的 npm 源；留空自动（官方源失败退镜像） |
| `--state-dir DIR` | `~/.dsh-gate` | 管理端状态目录（自备 pnpm 放这里） |
| `--timeout DUR` | `10m` | 安装超时 |

**顺带能诊断一个常见的「设置页打不开」**：浏览器里的设置页（模型、凭据）由
`@deepseek-ai/dsh-web-app` 挂载的 `ctx.remote.settings` 提供，清单里缺了它，页面上
只会显示 `settings are unavailable in this browser`。`plugin list` 会检查这一点。
注意这个 bundle 属于 dsh 本体、不在 npm 公开包源里（`@deepseek-ai/*` 多数需要授权），
所以缺了就升级/重装 dsh；要立刻能用，装公开的 `dsh-dock` 用它的「模型设置」页，
那正是管理端这条命令擅长的场景。

### dsh-gate model

不装插件也能把模型配好。用 `--instance` 指定实例，写的是**该实例自己的** DSH 设置文件：

- `<dsh-home>/.credentials.yaml` 的 `refs:`——**按环境变量名**存密钥（官方 DeepSeek
  默认读 `DEEPSEEK_API_KEY`，见 DSH 的 `dsh-llm-deepseek-api-key`）；
- `<dsh-home>/settings.yaml` 的 `agent-default-model`（默认服务商与模型）与
  `llm-pi-ai.providers`（自定义服务商）。

```sh
dsh-gate model list --instance app                                   # 看当前配置
dsh-gate model default deepseek-official deepseek-chat --instance app # 设默认
printf 'sk-…' | dsh-gate model key set DEEPSEEK_API_KEY --stdin --instance app
dsh-gate model provider add my-gw --base-url https://api.example.com/v1 \
    --key-env MY_GW_API_KEY --models qwen3-max,glm-5 --instance app  # 自定义服务商
dsh-gate model provider remove my-gw --instance app
```

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--instance NAME` | 必填 | dsh 实例名或 id（`dsh-gate dsh list` 查看） |
| `--dsh-home DIR` | 空 | 兼容/高级：直接指定实例目录；给了就优先于 `--instance` |

密钥**只能从标准输入读**（`--stdin`）：写在命令行上会留在 shell 历史与进程列表里。
命令行也不提供明文参数。

改写是**定点**的：只替换目标键所在的块，其余内容（界面偏好、其它服务商…）一个字节
都不动；写入前留一份 `<文件名>.dsh-gate.bak`，写入后回读校验，对不上会自动还原。
名字或值里带冒号、引号、空格、`#` 的一律当场拒绝——写坏这份文件等于让 DSH 起不来。

改了之后可能需要重启该实例才读到（命令结束时会提示）。

### dsh-gate version

打印版本号。

```sh
$ dsh-gate version
dsh-gate 0.3.20
```

---

## 多实例

一个实例 = 一个 dsh 进程 + 一个 `DSH_HOME` + 一个回环端口。不同的 `DSH_HOME` 就是两套
互不干扰的 dsh：会话历史、插件、配置全部分开。**一台机器一个管理端**（一个管理端数据库
对应一个进程），实例是管理端数据库里的记录，由它安装与监督。

| 概念 | 说明 |
| --- | --- |
| 实例 id | 随机不透明短串（小写字母数字），对外路径 `/i/<id>/` 用它，不泄漏语义 |
| 实例名 | 展示名，支持中文；改名不动 id、授权与会话 |
| `DSH_HOME` | 实例的数据根（会话、插件、凭据、设置）。默认 `~/.dsh` |
| 工作目录 | agent 默认读写的目录，默认 `<DSH_HOME>/workspace` |
| 安装目录 | dsh 程序本体（git / pnpm 方式），默认 `<DSH_HOME>/install` |
| 回环端口 | dsh 自己监听的端口，管理端自动分配（也可指定） |
| 启动方式 | `auto`（随管理端启动）/ `ondemand`（点进才拉起） |

几条约定：

- **账号不跟着实例走**：账号、授权、实例定义都在管理端数据库里（`<状态目录>/portal.db`），
  多个实例共用一套账号；谁能进哪个实例由「实例授权」决定。
- **授权是唯一的访问依据**：`--owner` / 拥有者只是展示字段；要让人进得去必须授权。
  管理员默认可以进入全部实例。
- **越权与不存在同形**：没授权的实例返回 404，与「不存在」完全一样。
- **端口不会撞**：新建实例时管理端在固定区间里挑空闲端口；你也可以用 `--dsh-port`
  或页面上的端口栏指定。
- **会话按挂载路径隔离**：管理端会在代理时改写 dsh Cookie 的 `Path`，实例之间不串会话；
  但 `localStorage` 是按域名的，同一浏览器里两个实例的界面状态会互相看见（凭证不串）。

**用页面还是命令行？** 都一样：页面上的每个动作都有对应的 `dsh` / `manage` 命令，两者
读写同一份数据库、走同一条运行时路径。批量建实例时命令行更省事，看状态、点安装时页面
更直观。

---

## 服务器部署（Linux + systemd）

无界面 Linux 服务器是最典型的场景。完整流程：

```sh
# 1. 把二进制放到 PATH（发行包解压出来的那一个）
sudo install -m755 dsh-gate /usr/local/bin/dsh-gate

# 2. 以你自己的身份安装：建管理员、写状态目录与服务配置
dsh-gate install                    # 交互式；脚本里用 --password-stdin / --random
#    也可以让它在装完后立即后台启动
dsh-gate install --start

# 3. 把管理端交给 systemd（写单元需要 root；--state-dir 与 --user 在 sudo 之前
#    由你的 shell 展开，所以指向你自己的目录与用户，而不是 root）
sudo dsh-gate service install --state-dir "$HOME/.dsh-gate" --user "$USER" --port 12020

# 4. 确认起来了
dsh-gate status
systemctl status dsh-gate
```

> 不要用 `sudo dsh-gate install --service`：sudo 下 `~` 可能变成 root 的家目录，账号库与
> 令牌会被写进 `/root`，之后以普通用户跑 `dsh-gate status` 就读不到。Linux 上把「建账号」
> 与「写系统单元」分成两步（如上），各自用对的用户执行。

`service install` 会把单元写到 `/etc/systemd/system/dsh-gate.service`，然后依次执行
`daemon-reload`、`enable`、`start`。

接下来打开 `http://服务器IP:12020/admin` 建实例、装 dsh、授权（见[第 2 步](#第-2-步建实例并装好-dsh)）。

日常运维：

```sh
dsh-gate status                # 查看管理端与实例状态（内部即 systemctl status）
dsh-gate stop / start / restart    # 停止、启动、重启整个管理端
journalctl -u dsh-gate -f      # 跟踪日志
```

单元的完整内容可以用 `service unit` 打印出来看一眼：

```sh
dsh-gate service unit --port 12020
```

[`examples/dsh-gate.service`](examples/dsh-gate.service) 是一份示例单元，与上面的输出一致。
想换端口或状态目录，不要直接改单元文件，用 `systemctl edit dsh-gate` 写一个 `ExecStart=`
覆盖即可，或者用新参数重跑一次 `service install`。

撤销：

```sh
dsh-gate service uninstall     # 只取消开机自启（disable + 删单元）
dsh-gate uninstall             # 停止并删除所有实例，最后卸载服务（会问是否删数据）
dsh-gate uninstall --yes --remove-data --purge   # 连数据与状态目录一起删，不再询问
```

几点值得知道的：

- **`WorkingDirectory` 是管理端进程自己的 cwd**，默认取当前用户的家目录。实例的工作目录
  各自记录在实例配置里，不受它影响。
- **`User=` 默认是执行安装命令的那个用户名**。想以别的用户跑，安装时用 `--user` 指定。
  注意状态目录与实例目录要对那个用户可写。
- **服务日志用 `journalctl -u dsh-gate -f` 看**，格式与前台运行一致（文本）；管理端自己的
  日志同时写到 `~/.dsh-gate/run/logs/portal.log`。
- **端口 1024 以下需要 root**，普通用户请换一个 1024 以上的端口。
- **多实例不需要多个单元**：一个管理端托管全部实例，实例的启停由管理端负责。

---

## 异地访问（人在家里用公司的 dsh）

**警告：在明文 HTTP 下，dsh 的会话 Cookie 会以明文过网。同一个局域网内可以接受，但不要把明文的网关端口直接暴露到公网。** 下面三种方式按推荐程度排列。

这些参数都是**管理端**的启动参数（`start` / `service install` 的同名选项），
配一次即可，对所有实例生效。

### 方式一：内网穿透工具（最省事）

把管理端端口通过各类内网穿透工具挂到一个公网域名上。隧道本身通常是加密的，流量走的是
加密信道，宿主机上仍然只听明文 HTTP：

```sh
dsh-gate start --port 12020 --public-url https://your-tunnel.example.com
```

穿透工具会给你一个 `https://xxx.example.com` 之类的地址，指定它访问即可。因为对外是
HTTPS，浏览器侧的 Cookie 会带 `Secure` 标记。

### 方式二：Tailscale / ZeroTier 这类组网

让家里和公司在同一个虚拟局域网里，访问地址仍是局域网形态，但走的是加密的虚拟网络。
两端都装上客户端、在同一个 tailnet / network 里即可：

```sh
# 公司机器上
dsh-gate start --port 12020 --public-url http://100.x.y.z:12020

# 家里直接访问
# http://100.x.y.z:12020
```

如果本机没有非回环地址（无界面服务器、容器网络隔离等场景），`--public-url` 需要显式指定，
否则管理端推导不出对外地址。

> **关于 `--public-url`**：这个选项会转成 dsh 的 `--public-url`，而**并非所有 dsh 版本都支持它**
> （实测 `0.2.0-rc.2` 只有 `--host` / `--port` / `--no-open` / `--trusted-host`）。
> 管理端拉起实例前会先跑一次 `dsh web --help` 看它认不认这个选项，不认就跳过并打一条日志——
> 所以升级 dsh 之后它会自动生效，不需要改配置。它只影响 dsh 打印的地址和
> agent 看到的 `DSH_WEB_URL`，**不影响管理端转发**。

### 方式三：网关自己跑 HTTPS

有自己的域名和证书时，直接让管理端提供 HTTPS：

```sh
dsh-gate start \
  --port 443 \
  --tls-cert /etc/letsencrypt/live/dsh.example.com/fullchain.pem \
  --tls-key  /etc/letsencrypt/live/dsh.example.com/privkey.pem \
  --public-url https://dsh.example.com \
  --trusted-host dsh.example.com
```

做成开机自启用 `service install` 的同名选项：

```sh
sudo dsh-gate service install \
  --port 443 \
  --tls-cert /etc/letsencrypt/live/dsh.example.com/fullchain.pem \
  --tls-key  /etc/letsencrypt/live/dsh.example.com/privkey.pem \
  --public-url https://dsh.example.com \
  --trusted-host dsh.example.com
```

两个要点：

- `--tls-cert` 和 `--tls-key` 必须成对提供，只给一个会被拒绝。
- 用了自定义域名，**必须**把它加进 `--trusted-host`，否则 dsh 会拒绝转发（403）。局域网 IP
  是自动加进去的，域名不是。

---

WebSocket 走 HTTP/1.1 升级（浏览器的 `WebSocket` API 本来就如此），因此用 `curl`
调试时要加 `--http1.1`：在 HTTP/2 连接上 `curl` 不会发送 `Upgrade` 头，dsh 会回 404。

## 安全说明

**账号怎么存的。** 管理端数据库是权限 `0600` 的 SQLite 文件，目录 `0700`，默认在
`~/.dsh-gate/portal.db`（管理端自己的状态目录，**不放在任何 dsh 的实例目录里**）。口令用
Go 1.24 标准库的 `crypto/pbkdf2` 做 PBKDF2-HMAC-SHA256，600000 次迭代，每条记录带自己的
随机盐，格式是 `pbkdf2-sha256$迭代次数$盐$摘要`。明文口令从不落盘。

账号不存在时也会跑一遍完整的 KDF 校验，把耗时拉平到和「口令错」同一量级——否则攻击者
可以据此枚举账号。

**会话在哪。** 登录会话只存在管理端进程内存里，**不落盘**。管理端重启即全部失效，所有人
需要重新登录。对一个家用 / 办公用的自建网关来说这完全是可以接受的，换来的则是「重启即
清空」，不存在过期会话长期滞留的问题。会话 Cookie 是 `dsh_gate_sid`，`HttpOnly` +
`SameSite=Lax`，启用 TLS 时额外带 `Secure`。

**登录限速。** 同一 IP 连续失败 5 次后开始锁定，锁定时长从 1 分钟起步，每次再失败翻倍，
封顶 15 分钟。登录成功即清零。刻意不做全局限速：同一办公室几十人共用一个出口 IP，
全局限速会误伤正常用户。

**本机控制令牌。** `dsh-gate dsh start|stop|restart|rm` 这类命令不靠管理员口令，而是用
`<状态目录>/portal.token` 里的一把随机令牌调本机控制 API。令牌文件权限 `0600`、
**每次启动都换一把新的**（旧令牌随上一次进程失效，文件残留也不会变成永久后门），
接口只接受回环来源。CLI 每次调用现读文件，所以换钥匙对用户透明。

**为什么 dsh 的启动令牌要留一个 `0600` 文件。** 它是 dsh 的万能钥匙，所以管理端只在
内存里持有它做交接，不写日志、不出现在启动横幅上，转发 dsh 启动输出时先把
`token=xxx` 脱敏成 `token=***`。但「只存内存」有个实际代价：管理端重启（升级、崩溃后
被计划任务重新拉起）时 dsh 还活着，新进程没有任何途径重新得知令牌——接管到的实例
能打开首页、一对话就 401，用户的体感是「放着不动就连不上了，还得重新登录」。

所以令牌会以 `0600`（Windows 上等价于「仅当前用户可读」）写进**实例自己的目录**：
`<实例 DSH_HOME>/.dsh-gate-token`。先写临时文件再改名，进程被强杀也不会留下半截残钥；
读完不删，dsh 换令牌时覆盖。它所处的信任边界与 `portal.db` 里的口令哈希相同（这台
机器的当前用户）。删掉它不会降低安全性，只是下次管理端重启后要重新拿一次令牌
（可用 `--dsh-token` 直接指定）。令牌仍不进日志、不进审计、不出现在任何页面上。

**为什么默认让 dsh 只听 127.0.0.1。** dsh 自己没有面向局域网的安全设计：明文 HTTP、
启动令牌直接打在 stdout 上。对外只暴露带登录页的管理端这一个入口，是这两者之间最简单
也最难出错的分界。

**容易踩的坑：自己改地址没用。** dsh 会校验请求的 `Host` 与被接受的 authority 是否匹配，
并要求 `Origin` 指向同一 authority。管理端把 `Host` 原样转发给 dsh，所以你在浏览器里用
哪个地址访问，管理端就必须用 `--trusted-host` 把那个地址点名给 dsh。

- 局域网 IP 会自动加进可信名单，一般不用手工指定；
- **主机名、mDNS 名、内网穿透域名不会自动加入**，用到它们时必须显式 `--trusted-host`；
- 没加的表现是每个 API 调用返回 **403**。

同理，实例的 `--listen-host` 接受 `0.0.0.0` 会把那个 dsh 直接暴露到局域网——那是用户
自己的明确选择，但**不要这么做**：dsh 没有登录页，谁连上谁就是 agent。

上游地址固定来自实例记录（本地数据库），不接受请求头或查询参数指定，因此管理端不会变成
一个任意 URL 的跳板。限速只看 TCP 对端 IP，不采信 `X-Forwarded-For` / `X-Real-IP`
（除非你明确知道前面还挂着自家反向代理，否则这些头任何人都能伪造）。

---

## 环境变量

优先级：**命令行 > 环境变量 > 保存的启动配置 > 默认值**。

管理端单形态之后，环境变量的作用面很小：**实例网络参数不再从环境变量读**（它们由
`start` 命令行、保存的 `portal.json` 或实例记录决定），所以下面这几个才是真正会生效的。

| 环境变量 | 对应选项 / 作用 | 说明 |
| --- | --- | --- |
| `DSH_GATE_STATE_DIR` | `--state-dir` | 管理端状态目录（数据库、令牌、登记、自备运行时），默认 `~/.dsh-gate` |
| `DSH_GATE_LANG` | `--lang` | CLI 与页面输出语言：`zh-CN` / `en` |
| `LC_ALL` / `LANG` | 同 `DSH_GATE_LANG` | 两者都没给时的兜底 |

语言选择优先级：`--lang` > `DSH_GATE_LANG` > `LC_ALL` > `LANG` > 默认中文。浏览器侧
按 `?lang=` > Cookie > `Accept-Language` 推导，规则与命令行一致。

`install` / `service install` 生成的服务单元会把监听地址、端口、状态目录等**显式写进
`ExecStart`**，所以这些参数不依赖 systemd 的环境变量——机器上改环境变量不会让已经装好的
服务悄悄换端口。

---

## 常见问题

**进实例后一直「重新连接中」，刷新一下就好。**

dsh 每次启动都会生成**新的启动令牌**，浏览器上那个标签页里的 dsh 会话随之作废。dsh 的前端
遇到这种情况只会显示「重新连接中」并无限重试，**它自己不会重新做令牌交接**——这是 dsh
客户端的行为，管理端改不了（WebSocket 握手被拒时无法重定向）。

刷一下页面即可：刷新是一次导航，管理端会在那次导航里顺手用新令牌重新交接。**所以每次重启
管理端、或实例重启过之后，已经打开的标签页都需要刷新一次。**

**页面能打开，但一直 401。**

dsh 的会话与 authority 绑定。换了访问地址（IP ↔ 主机名 ↔ 穿透域名）、或者 dsh 重启换了
新令牌之后，浏览器手上的 dsh 会话就失效了。管理端会自动清掉交接标记并重试一次，通常表现
为「页面自己就好了」。仍然不行的话，重启一次该实例让管理端重新学习令牌：

```sh
dsh-gate dsh restart app
```

如果这个实例是你自己在外面临时跑过 `dsh web`（管理端接管了它）、而管理端拿不到它的令牌，
把管理端日志里脱敏前的地址找出来也没用——直接让管理端自己拉起它：`pkill -f "dsh web"`
之后 `dsh-gate dsh restart app`。

**页面能打开，但 API 全是 403（进实例后一直「重新连接中」、工作区打不开）。**

`--trusted-host` 没覆盖你正在用的地址。dsh 只信任回环、自身绑定地址和被显式点名的
authority，所以它会把每个 `/api` 请求和 WebSocket 都拒掉。症状很有欺骗性：**页面能正常
打开**（`GET /` 走的是令牌交换，不过那道栅栏），只有进去之后的对话与工作区转圈。

局域网 IP 与主机名会自动加入，一般不用管。**推导不出来的是这三种**：自定义域名、mDNS 名、
内网穿透域名；以及**容器端口映射 / NAT**——管理端在容器里看到的网卡地址不是你在浏览器里
敲的那个，谁也推不出来。加一个即可：

```sh
dsh-gate start --trusted-host dsh.example.com --trusted-host my-mac.local

# 管理端形态：用 --public-url 让管理端把主机名一起推导进去
dsh-gate start --port 12020 --public-url http://192.168.1.20:12090/
# 也可以直接点名（带端口 = 只认这个 authority）
dsh-gate start --port 12020 --trusted-host 192.168.1.20:12090
```

改完 `start` 的参数会保存下来；已经在跑的管理端需要 `dsh-gate restart` 才生效。用服务托管
的话，重跑一次 `service install` 带上同样的选项。

**端口被占。**

`install` 的向导会当场检查端口并让你换；`start` 起不来时日志里会写「地址已被占用」。
先跑 `dsh-gate status` 看管理端是不是已经在跑，或换一个端口：

```sh
dsh-gate start --port 3083
```

端口在 1024 以下时提示的是权限不足，换一个 1024 以上的端口即可。

**Windows 上 `manage user add` 没法输口令。**

不是交互式终端时（比如在 CI 或某些 PowerShell 配置下），程序会直接拒绝并提示改用另外
两种方式：

```sh
dsh-gate manage user add admin --role admin --random
echo "$PASSWORD" | dsh-gate manage user add admin --role admin --password-stdin
```

**首次安装很慢。**

实例第一次安装要下 Node 运行时与 dsh 包，可能要几分钟；启动后 dsh 还要初始化 profile。
这期间管理端与登录页照常可用，实例页显示「安装中 / 启动中」，刷新看进度。日志里能看到
`正在下载官方 Node.js 运行时` → `Node.js 运行时已就绪` → `dsh 启动命令已就绪` 的链路。

**npm 源太慢 / 拉不下 dsh（日志里是「长时间无响应」）。**

管理端会自己换源：官方源（`registry.npmjs.org`）或 nodejs.org 拉不动、超时，就自动改用
国内镜像（`registry.npmmirror.com`）重试一次，并把实际用的源注入 dsh——dsh 自己装插件时
也走同一个源。想固定用某个源（公司内网 registry、私有代理）就显式给出来：

```sh
dsh-gate start --npm-registry https://registry.npmmirror.com     # 管理台安装 dsh 用
dsh-gate dsh install app --npm-registry https://registry.npmmirror.com
```

换源只对「源拉不动」有意义；**入口静默不执行、缺原生绑定、旧 Node 崩溃**这些是运行时不
兼容，管理端会改成实测可用的固定版本 dsh，而不是换源。

**想用比 latest 更新的 dsh（alpha / rc 通道）。**

页面上创建或编辑实例时填「dsh 版本」那一栏，命令行里：

```sh
dsh-gate dsh install app --dsh-version 0.2.2-alpha.2   # 版本号
dsh-gate dsh install app --dsh-version next            # 或 dist-tag（next / alpha）
```

名字以 npm 上实际有的为准：`npm view @deepseek-ai/dsh dist-tags` 看各通道指向哪个版本，
`npm view @deepseek-ai/dsh versions --json` 看全部版本号。**钉住就不再自动回退**——那一个
版本装不上会直接报错（上一条说的「改成实测可用的固定版本」只在留空时发生）。源码构建
（`--install-mode git`）不认这一栏，它用 `--git-ref` 认分支与标签。想取消钉住回到 latest：

```sh
dsh-gate dsh edit app --dsh-version ""
```

**dsh 起不来。**

dsh 起不来时实例不会让管理端退出——管理端继续提供页面，日志里留下原因，监督者按指数退避
持续重试（1 秒起步，最长 30 秒一次），实例页显示最近一次失败的原因。每一轮先等
`--dsh-start-timeout`（默认 5 分钟）才判失败：这一档是给「首次启动下载 dsh 与插件」留的，
给短了会把正在下载的子进程杀掉、下一轮从头再来，怎么重试都起不来。先用「运行环境」页
看 dsh 命令找不找得到；找不到就重新安装一次：

```sh
dsh-gate dsh install app --install-mode npx
```

**服务器上没有 Node.js（页面提示找不到 npx）。**

不用自己去装：管理端启动时会检测 npx，没有就把**官方 Node.js 二进制包**下载到状态目录
（默认 `~/.dsh-gate/node`），校验官方 `SHASUMS256.txt` 后解包使用——不需要 root，也不改
系统包。已经装好的那份会一直复用（不重复下载），卸载时用 `dsh-gate uninstall --purge`
一并删除。Alpine 这类 musl 系统跑不了官方 glibc 二进制，请用 `apk add nodejs`。

**会话的默认工作目录没生效（实际落在 `/root` 之类的地方）。**

实例的「工作目录」留空时默认是 `<实例目录>/workspace`，已经写死在实例记录里，与启动
管理端时的当前目录无关。没生效通常是因为改完没重启：

```sh
dsh-gate dsh edit app --workspace /www/wwwroot/app
dsh-gate dsh restart app        # 保存配置不会自动重启
```

确认实际用的是哪个目录：管理台实例行上的「工作目录」，或 `dsh-gate dsh list` 输出里的
`DSH_HOME` 与该实例配置。

**点了「停止」，实例还在跑（Windows 上尤其明显）。**

0.3.45 之前有两种停不掉，两种都不是按钮坏了：

- 端口上那个 dsh **不是网关启动的**（你自己跑过 `dsh web`，或者网关崩过而它留着）。
  网关只认识它自己拉起的那个进程，接管来的只会「用」不会「结束」——于是停止报了成功，
  进程照旧在跑。
- 停成功之后又被**请求**拉起来：实例是按需启动的，那个还开着的标签页刷新一次就是一次
  请求。症状是「按了停止，它又活了，进程号还变了」。

0.3.45 起两种都修了：接管来的实例会按端口现查进程号、连整棵进程树一起收掉，等端口真的
空出来才报成功；你主动停掉的实例不再被请求复活，页面会明说「实例已停止——要再用它，请回
管理台点启动」。装上新版还停不掉，先看一眼端口到底是谁在听：

```bat
netstat -ano | findstr :32100
tasklist /FI "PID eq 上面那个号"
```

如果那是个精简系统（没有 `taskkill`）里查不到进程号的外部 dsh，管理端会直接承认它停不了，
并把上面这两条命令作为下一步告诉你，而不是假装停过了。

**页面显示「DSH 要求重新登录」（接管了外部启动的 dsh）。**

dsh 的启动令牌**每个进程随机生成、只在 dsh 启动时打印一次、也不落盘**。如果那个 dsh
不是管理端拉起的（比如你自己跑过一个 `dsh web`，或者换了管理端进程而旧 dsh 还留着），
管理端就拿不到它的令牌：首页能打开，但对话会报未授权。让管理端自己把它管起来：

```sh
pkill -f "dsh web"          # 停掉外面那个 dsh（或 ps 找到 PID 后 kill）
dsh-gate dsh restart app    # 管理端重新拉起它 —— 令牌自然就进管理端内存了
```

**页面报 `fork/exec …/node/bin/npx: no such file or directory`。**

管理端自备的 Node.js 运行时目录在准备完成之后不见了（冒烟失败被清理、状态目录被删、
被别的进程换掉），而 PATH 里记的还是那条旧路径。删掉状态目录里的 `node/` 再重启实例
就会重新下载：

```sh
rm -rf ~/.dsh-gate/node      # 或你自定义的 <状态目录>/node
dsh-gate dsh restart app
```

**DSH 里点「添加插件」报 `spawn pnpm ENOENT`（或一直转圈）。**

DSH 的插件安装器走的是 pnpm，而 Node.js 只自带 npm/npx。管理端会在准备 dsh 之前用 npm 把
pnpm 装到状态目录的独立前缀（`<状态目录>/pnpm`，不进系统、不需要 root），并把它前置进
PATH；系统里已有 pnpm 时不动。也可以在管理台的「运行环境」页点一下安装。

**不必依赖浏览器里的插件页**：用管理端自己的命令装，它走同一套 pnpm 与自动换源：

```sh
dsh-gate plugin add dsh-dock --instance app
dsh-gate dsh restart app         # 装完重启该实例才生效
```

**管理台里装 dsh 报 `Ignored build scripts: node-pty, koffi, …`。**

pnpm 10 起默认**拒绝**依赖的 `postinstall` 脚本（供应链安全），而 dsh 的原生依赖正是靠它
准备平台相关的产物（`node-pty` / `koffi` 编译或搬运原生绑定，`protobufjs` / `@google/genai`
生成运行时代码）。不放行时 pnpm 会**以失败退出**（不是警告），dsh 装不起来。

管理端的 pnpm 与 git 方式已经自动用 `--allow-build=<包名>` 逐个放行这几个包，正常不会
看到这条报错。如果你在**手动**跑 pnpm，或者上游新增了原生依赖，照报错里点名的包加参数：

```sh
pnpm add @deepseek-ai/dsh --allow-build=node-pty --allow-build=koffi \
    --allow-build=protobufjs --allow-build=@google/genai \
    --allow-build=@deepseek-ai/dsh-subprocess-local
```

别图省事写成 `--allow-build=*`：那等于把整条依赖链的脚本都放开，正是 pnpm 默认拒绝的初衷。

**git 方式装 dsh 卡在 `pnpm install` 很久，最后报 `ERR_PNPM_FETCH_TIMEOUT`。**

上游 monorepo 依赖很重（实测 clone + install + build 十几分钟），其中个别包（例如
`cdn.sheetjs.com` 上的 `xlsx`）不在 npm 主源上，慢网络下会超时。这不是 dsh-gate 的问题：

```sh
# 换更近的 npm 镜像源（对 npm 上的依赖生效；上游 CDN 依赖仍走原地址）
dsh-gate dsh install app --install-mode git \
    --npm-registry https://registry.npmmirror.com

# 或者干脆用 npx / pnpm 方式，它们只从 npm 拉 dsh，不碰上游 monorepo 的 CDN 依赖
dsh-gate dsh install app --install-mode pnpm

# 或者自己在能跑通的网络/工具链里 clone、安装、构建，再让网关只负责启动
git clone https://github.com/deepseek-ai/deepseek-harness && cd deepseek-harness
pnpm install && pnpm run build
dsh-gate dsh install app --install-mode local --install-dir "$PWD"
```

最后一种就是 **local（本地源码）**：网关不 clone、不装依赖、不构建，只核对目录并生成
`pnpm --dir '<源码目录>' run dsh`，启动时接上 `web --host … --port …`。

管理端只在 **lockfile 与 package.json 不一致**时才把 `--frozen-lockfile` 退回普通
`install`；网络类失败会**原地报错**，不会让你白等一轮重拉同一批包。

**管理台页面上的安装进度不动 / 一直显示「安装中」。**

管理台的状态由页面里那段脚本**自动刷新**（不需要手动刷新浏览器）：它只轮询同源的实时
状态接口、按格子打补丁，不整页重载，所以正在填的表单与展开的面板都不会被刷掉。另外任务
状态只在内存里（管理端进程重启后，「安装中」的任务不会被继承——没有任何东西能继续推进
它），重新发起一次安装即可。装失败也不影响实例记录：记录还在，改个方式再装。

**Alpine 上跑不起来（`exec format error` 或 Node 起不来）。**

官方 Node.js 二进制是 glibc 的，Alpine 是 musl，跑不了。用 `apk add nodejs` 装系统自带的
Node，管理端会优先用它；或者换 Debian / Ubuntu 这类发行版。另外精简镜像常常没装
`ca-certificates`，会报 `x509: certificate signed by unknown authority`，装上
`ca-certificates` 即可。

**页面显示 `dsh web authentication required; reopen the URL printed by dsh web.`**

这是 dsh 的 401 正文（它收到请求时，浏览器信任 Cookie 与当前 authority 对不上）。管理端
的交接标记里存的是「上次交接用的令牌指纹」：dsh 一换令牌，浏览器下次打开 `/` 就自动重新
交接一次；会话在别的场合失效（401）时，管理端会清掉标记并重做交接，整个过程对用户不可见。
若仍然看到这句话，说明管理端手上这次启动的令牌不对（例如接管了外部 dsh 又没拿到令牌），
按上一条「接管了外部启动的 dsh」处理。

**设置页里的「模型」打不开：`加载提供商目录失败: settings are unavailable in this browser`。**

这不是管理端的代理坏了，而是 dsh 那边的设置面没挂上：浏览器里的模型/凭据设置由
`@deepseek-ai/dsh-web-app` 这个 bundle 提供的 `ctx.remote.settings` 支撑，配置档的
bundle 清单里缺了它，前端就只能报这一句（客户端代码里是 `describeFace` 拿不到视图）。
先看清单：

```sh
dsh-gate plugin list --instance app     # 会检查清单里有没有 dsh-web-app
```

这个 bundle 是 dsh 本体的一部分，`@deepseek-ai/*` 多数不在 npm 公开源里（实测
`pnpm view @deepseek-ai/dsh-client-ui-command` 直接要授权），所以缺了它不能用
`plugin add` 补——先升级/重装 dsh。**能立刻用的路是装 `dsh-dock`**：它是公开包
（`dsh-dock@0.14.0`），自带「模型设置」页，用插件的方式配置服务商与密钥：

```sh
dsh-gate plugin add dsh-dock --instance app
dsh-gate dsh restart app
```

**敲 `dsh-gate` 提示 command not found。**

说明它不在 `PATH` 里：要么是直接 `./dsh-gate` 运行，要么只把二进制拷到了某个目录。用
`dsh-gate install` 或 `sudo dsh-gate service install` 装一次（默认拷到 `/usr/local/bin`），
之后就能直接敲；自定义位置用 `--bin-dir`。装完仍找不到，就检查那个目录在不在 `PATH` 里
（`echo $PATH`）。

**管理端重启后所有人都要重新登录。**

这是有意为之：会话只存在内存里，管理端重启即全部失效。

**账号表在哪儿？**

管理端数据库 `<状态目录>/portal.db` 里，默认 `~/.dsh-gate/portal.db`。用 `--db` 或
`--state-dir` 可以改。

**怎么把旧的单实例账号迁过来？**

管理端首次启动时，如果数据库里一个账号都没有、而状态目录里存在旧的 `users.json`，会自动
导入账号（第一个用户成为管理员），旧口令继续可用。实例本身不会自动迁：用 `dsh-gate dsh add`
按原来的 `--dsh-home` 新增一条即可。

### 管理台起来了，但实例一直显示「DSH 正在启动」

先看管理端日志（`journalctl -u dsh-gate -f`，或 `~/.dsh-gate/run/logs/portal.log`）。
首次安装要下载 Node 与 dsh 包，可能要几分钟。如果长时间不成功，日志里会回放 dsh 自己的
报错，常见原因：

- **一次启动没能在预算内跑完**（日志里是「在 5m0s 内没有监听 …」）：每一轮都按
  `--dsh-start-timeout` 等待，超时会把正在下载的子进程杀掉再重来，所以这个预算必须大于
  「下载 dsh 与插件」实际要花的时间。慢机器上直接调大：

  ```sh
  dsh-gate start --dsh-start-timeout 15m                 # 没装系统服务时
  sudo dsh-gate service install --dsh-start-timeout 15m  # 装了开机自启（重写单元）
  ```

  改完 `dsh-gate restart`。给过启动参数就按这次给的起（之前存的端口等要一并带上），
  所以只想调这一项时更省事的是环境变量：`DSH_GATE_DSH_START_TIMEOUT=15m dsh-gate start`。
- **找不到 dsh**：「运行环境」页会告诉你本机有没有找到；没有就点安装，或
  `dsh-gate dsh install app` 重装。
- **端口被别的程序占了**：`dsh-gate dsh edit app --dsh-port <其它端口>` 再重启实例。
- **`trustedHosts entry ... is not a bare host[:port] authority`**：手动给的
  `--trusted-host` 里有裸 IPv6 地址。IPv6 必须写成 `[2408:...::1]` 这种带方括号的形式；
  管理端自动加入的地址已经处理好了。

## 贡献

欢迎提 issue 和 PR。动手之前建议先读一遍 `go/internal/proxy/proxy.go` 的文件头注释——启动
令牌的交接流程是整个项目最值得读懂的部分，绝大多数「页面能开但 API 报错」的问题都能在那里
找到答案；子路径挂载的机械层在 `go/internal/mountproxy/`。

几条约定：

- 除管理端数据库的纯 Go SQLite 驱动外，**不要引入第三方依赖**。
- 改动行为时同步更新 README 与 CHANGELOG。
- Go 代码在 `go/` 子目录，模块名是 `github.com/wycto/dsh-gate`。
- 提交备注用中文，格式是 `类型：备注`。

## License
