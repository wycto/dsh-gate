# dsh-gate 门户（Portal）设计与实现说明

> 状态：**已实现**（P0、P1 的主体），本文同时描述现状与设计约定；尚未实现的部分集中列在
> 第 15 节。历史演进一句话带过：dsh-gate 曾经是「每个实例一个独立网关」的单实例形态，
> 本轮重构后**只保留管理端单形态**，旧命令（按实例名启停、单实例 `run`/`user`/`check` 等）
> 已彻底移除。

## 1. 目标

dsh-gate 现在只有一种形态：**一个管理端（门户）+ N 个 dsh 实例**：

- 管理端自带**管理页面**与**数据库**：启动后浏览器就能访问，不依赖命令行也能完成日常管理。
- **用户体系**：管理员建用户、改密、禁用、给角色；用户登录后只看到**属于自己的实例**。
- **实例管理**：在页面里新建 / 启停 / 重启 / 编辑 / 删除 / 安装 dsh；实例与用户的关系由管理员分配。
- 用户路径：登录管理端 → 「我的实例」→ 点击 → 直接进入该实例的会话（不需要知道端口、不需要令牌）。
- **命令行与页面能力对等**：管理端生命周期由 `install` / `start` / `stop` / `restart` / `status` /
  `uninstall` / `service` 负责；实例由 `dsh` 子命令管理，用户与授权由 `manage` 子命令管理。

非目标（本期不做）：外部 SSO/OIDC 身份源；跨机器实例编排；按用量计费与配额。

## 2. 已确认的决策

| 决策点 | 结论 | 备选与代价 |
|---|---|---|
| 路由模型 | **子路径 `/i/<实例>/`** | 子域名（`<实例>.域名`）origin 天然隔离、实现最干净，但需要通配符 DNS + 通配符证书；端口跳转最省事但要对外开多端口。选子路径 → 必须做「剥前缀 + 改写 Cookie 路径」，见第 4 节 |
| 存储 | **SQLite** | 本仓库原本刻意保持零第三方依赖（`go/go.mod` 注释写明）。引入 SQLite 会**修订这条规则**，必须同步 README「贡献」与 CHANGELOG，并重新验证六目标交叉编译 |
| 用户 ↔ 实例 | **混合**：默认一人一实例，管理员可把同一实例授权给多人 | 纯一人一实例最隔离但每用户一个 dsh 进程；纯共享省资源但同一实例内会话历史共享（dsh 单用户架构） |
| 隔离预期 | 一人一实例时**真隔离**（各自 `DSH_HOME`、会话、插件、配置）；共享实例时**只做访问控制，不隔离** | 这一条必须写进用户文档，避免「分配了实例就等于隔离」的误解 |

### 2.1 最终决议（2026-10-14）

| # | 问题 | 决议 |
|---|---|---|
| 1 | 实例 id | **不透明随机短 id**（小写字母数字，≥8 位；如 `k3m9f2qa`），对用户只展示实例名；路径里出现 id 不泄漏任何语义 |
| 2 | 越权访问他人实例 | **404**（把「无权限」与「不存在」合并回答，不泄漏实例是否存在），并写审计。门户页面不会给出无权链接，因此 UX 损失接近零；管理员本来就能访问全部实例，不受影响 |
| 3 | 角色 | 本期只有 `admin` / `user`；`viewer` 与更细粒度留到 P2 |
| 4 | SQLite 驱动 | **`modernc.org/sqlite`**（纯 Go、无 CGO，六个发行目标仍可交叉编译） |
| 5 | i18n | **本期一并做**：`zh-CN`（默认）+ `en`，覆盖 CLI 输出、登录页、门户页/管理台/等待页；被代理的 dsh 页面不归我们管。设计见第 6.1 节 |


## 3. 架构

```
              浏览器（任意设备）
                    │  https://example.com/              门户页 + 登录
                    │  https://example.com/i/alice/…     实例（子路径挂载）
                    v
   ┌──────────────────────────────────────────────────────────┐
   │ dsh-gate 门户进程（唯一对外监听，TLS）                     │
   │  · 登录 / 会话 / 限速            （复用 internal/auth）    │
   │  · 门户页 + 管理台 + JSON API    （go:embed，零前端依赖）  │
   │  · SQLite：users / instances / grants / audit             │
   │  · 实例编排：supervisor 拉起、重启、按需启停               │
   │  · /i/<id>/ 反向代理：剥前缀、改写 Cookie、令牌交接        │
   └───────┬───────────────────────────────┬──────────────────┘
           │ 127.0.0.1:32101               │ 127.0.0.1:32102
           v                               v
     ┌───────────┐                   ┌───────────┐
     │ dsh alice │（DSH_HOME=…）      │ dsh bob   │
     └───────────┘                   └───────────┘
```

- 门户是**唯一对外口**；每个实例的 dsh 只绑回环端口，只接受门户的代理请求。
- 每个实例 = `{ DSH_HOME, dsh 回环端口, 工作区, 启动命令 }`，由门户按需拉起（P1 起支持空闲回收）。
  `DSH_HOME` 留空时用 dsh 自己的默认目录（`~/.dsh`），落库前展开成**绝对路径**：
  它会原样进子进程的 `DSH_HOME` 环境变量，环境变量不经过 shell，`~` 不会被展开。
- **只有管理端形态**：不存在「一个网关托管一个 dsh」的运行方式。实例的增删改查与启停都经管理端
  （页面或 `dsh` 子命令），本机 CLI 需要动进程时走管理端的本机控制 API（见 6.1 节），
  不会绕过管理端拉起孤儿进程。

**目录口径（只填一个也能跑）**：dsh 只有 `DSH_HOME` 一个数据根，没有独立的
「配置目录」环境变量；管理台上的三栏因此不是三个各管一摊的目录：

| 字段 | 实际含义 | 留空时 | 什么时候填 |
|---|---|---|---|
| 实例目录（`DSH_HOME`） | dsh 的数据根：会话、附件、插件、凭据与设置都在里面 | `~/.dsh`（展开成绝对路径） | 想让实例完全独立时**只填这一栏就够** |
| 配置目录 | `DSH_HOME` 的另一种写法：填了会顶替实例目录（运行时 `homeOf` 以它为准） | 同实例目录 | 一般留空；「配置放这里、数据放实例目录」在 dsh 里分不开 |
| 工作目录 | agent 默认读写的目录（dsh 的默认工作区） | 实例目录下的 `workspace` | 想让 agent 在别的目录干活时填 |
| 安装目录 | dsh **程序本体**（git 的源码 / pnpm 的包），不是数据；`local` 时是你已构建好的源码目录 | npx 不需要；git / pnpm 用实际生效的 `DSH_HOME` 下的 `install`；local 必须填 | 想固定版本、离线复用或改源码时 |

npx 直接跑（不带任何参数）时这三个目录都不存在：程序本体由 npx 放在 npm 缓存
（`~/.npm/_npx`）里，数据只有 `~/.dsh` 一个。管理端把子进程的 `HOME` 指到实例工作区
之后，npm 缓存仍固定回网关自己的家目录（`npm_config_cache`），不会把用户的工作区搞脏；
插件与技能的共享面也留在网关侧，实例之间只共享代码、不共享数据。

**管理端的数据布局**（`<状态目录>` 默认 `~/.dsh-gate`）：

| 路径 | 内容 |
|---|---|
| `<状态目录>/portal.db` | 管理端数据库：用户、实例、授权、审计（0600，SQLite） |
| `<状态目录>/portal.token` | 本机控制令牌（0600；每次启动刷新，`dsh` 子命令用它调本机 API） |
| `<状态目录>/portal.json` | 上次启动参数（`restart` / `status` 用） |
| `<状态目录>/node`、`<状态目录>/pnpm` | 自备 Node 运行时与 pnpm（`uninstall --purge` 一并删除） |
| `~/.dsh-gate/run/` | 运行登记（管理端单例，登记名固定 `portal`）与 `logs/portal.log` |
| 各实例的 `DSH_HOME` | dsh 自己的数据：会话、附件、插件、凭据、设置。**不归管理端状态目录管**，卸载管理端不删 |

## 4. 子路径挂载的六条硬要求（技术核心）

选子路径后，代理必须做到下面六条；每条都对应 dsh 的一个已核实行为。

| # | 要求 | 依据 |
|---|---|---|
| 1 | **剥前缀**：`/i/<id>/xxx` → 上游 `/xxx` | dsh 监听器只认 origin-root 路由；其 `index.html` 用**文档相对**路径引用资源（`./assets/…`、`./manifest.webmanifest`），挂在 `/i/<id>/` 下浏览器会解析成 `/i/<id>/assets/…`，剥掉前缀即可命中 |
| 2 | **改写上游 `Set-Cookie` 的 `Path`**：`Path=/` → `Path=/i/<id>/` | dsh 会话 Cookie 名是 `dsh-auth-<base64url(sha256(authority))>`，属性 `Path=/; HttpOnly; SameSite=Strict`。子路径方案下**所有实例共用同一个 authority** → Cookie 名完全相同；不改路径就会互相覆盖/串会话。这是本方案第一风险 |
| 3 | **挂载根做令牌交接**：`/i/<id>/?token=…`；`/i/<id>`（无斜杠）303 到 `/i/<id>/` | dsh 只在 `GET /` 把启动令牌换成会话 Cookie；`/index.html` 直接命中是静态资源、能开页面但所有 API 401。网关已有「令牌交接 + 令牌指纹 + 401 自愈」，复用它 |
| 4 | **Host / Origin 原样转发 + 点名 authority** | dsh 的 Host/Origin 围栏按收到的 Host 比对，且要求 Origin 同源；每个实例必须收到包含**门户 authority** 的 `--trusted-host`，否则页面能开（`GET /` 是令牌交换）而每个 XHR / WebSocket 都 403，前端表现成「一直重新连接中」「工作区打开失败」。装配处统一用 `server.TrustedHosts` 推导：显式 `--trusted-host` + `--public-url` 主机名 + 本机非回环 IP + 主机名。**容器端口映射 / NAT 下推导不出浏览器里的那个地址，必须显式给 `--public-url`（或 `--trusted-host`）** |
| 5 | **升级与流式不缓冲**：`/i/<id>/api/remote.mux`（WebSocket）、`/i/<id>/plugins/events`（SSE） | 现有代理已支持；注意不要给响应设总超时（现有约定：只设 `ReadHeaderTimeout`） |
| 6 | **门户 HTTPS 链路上补 `Secure`** | 上游是明文 HTTP 时 dsh 签发的 Cookie 不带 `Secure`；门户对外是 HTTPS，必须在改写时补上 |
| 7 | **改写上游的根相对 `Location`**：`Location: /` → `/i/<id>/` | dsh 完成令牌交换后 302 到 `/`。不改写就会出现「点进实例闪一下又回到门户首页」——浏览器照着 `Location: /` 跳出了挂载。只动根相对路径（协议相对 `//…` 与绝对 URL 不动） |

### 已知限制（必须在文档里对用户说明）

- **`localStorage`/`sessionStorage` 按 origin 共享，不按路径隔离**：同一个浏览器先后进入两个实例时，UI 状态（主题、快捷键、上次会话等）会互相看见。凭证不会串（Cookie 按路径隔离）。要彻底避免只能改子域名方案。
- **PWA / manifest**：`manifest.webmanifest` 的 `start_url` 是 `/`，从子路径安装表现可能异常（本期不承诺移动端「添加到主屏幕」）。
- **上游插件的绝对路径资源**：dsh 自身的壳是文档相对的；dsh-dock 的客户端有「fetch 一律文档相对路径」的契约（其测试锁死）。其它插件若硬编码 `/xxx`，子路径下会 404 —— 需要逐个插件确认。

## 5. 数据模型（SQLite）

数据库：`<state-dir>/portal.db`（文件 0600、目录 0700，沿用现有权限约定），WAL 模式，`schema_version` 表。

```sql
users(
  id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL,       -- 登录名
  display_name TEXT NOT NULL DEFAULT '',
  hash TEXT NOT NULL,                                      -- PBKDF2（沿用 internal/users）
  role TEXT NOT NULL DEFAULT 'user',                        -- admin | user
  disabled INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);

instances(
  id TEXT PRIMARY KEY,                                      -- 对外路径用的不透明短 id
  name TEXT UNIQUE NOT NULL,                                -- 展示名，如 alice-main
  owner_id INTEGER REFERENCES users(id),                    -- 默认拥有者（可空）
  dsh_home TEXT NOT NULL, workspace TEXT NOT NULL DEFAULT '',
  dsh_port INTEGER NOT NULL,                                -- 回环端口
  dsh_cmd TEXT NOT NULL DEFAULT '',                         -- 留空=自动探测
  on_demand INTEGER NOT NULL DEFAULT 0,                     -- 按需拉起
  idle_timeout_s INTEGER NOT NULL DEFAULT 0,                -- 空闲回收（0=不回收）
  enabled INTEGER NOT NULL DEFAULT 1,
  install_mode TEXT NOT NULL DEFAULT 'npx',                 -- npx | git | local | pnpm（schema v2；local 为后加取值，不影响老库）
  install_dir TEXT NOT NULL DEFAULT '',                     -- dsh 安装目录（schema v2）
  config_dir TEXT NOT NULL DEFAULT '',                      -- 配置目录；留空回落 dsh_home（schema v2）
  listen_host TEXT NOT NULL DEFAULT '127.0.0.1',            -- 该实例 dsh 的监听地址（schema v2）
  start_mode TEXT NOT NULL DEFAULT 'auto',                  -- auto | ondemand（schema v2）
  dsh_version TEXT NOT NULL DEFAULT '',                     -- 钉住的 dsh 版本/dist-tag；留空=跟随 latest（schema v3）
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);

grants(                                                     -- 用户 ↔ 实例（多对多）
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  instance_id TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL, created_by TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(user_id, instance_id)
);

audit(                                                      -- 追加写，页面可查
  id INTEGER PRIMARY KEY, at TEXT NOT NULL, actor TEXT NOT NULL,
  action TEXT NOT NULL, target TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '',
  ok INTEGER NOT NULL DEFAULT 1
);
```

- 门户自身的会话沿用**内存表**（现状：重启即失效，是刻意取舍），不落库。
- 驱动：**`modernc.org/sqlite`（纯 Go，无 CGO）**，六个发行目标仍可交叉编译。
- 影响面：`go.mod` 出现依赖树；`go/build.sh` 与交叉编译需重新验证；二进制体积变大；README 的「零第三方依赖」表述必须改写为「仅 SQLite 一处第三方依赖，其余标准库」。
- **迁移是加列式的**（`PRAGMA user_version` 记版本，当前 v3）：v2 一次加完 `install_mode`、`install_dir`、
  `config_dir`、`listen_host`、`start_mode`，v3 再加 `dsh_version`，全部写成 `ALTER TABLE ... ADD COLUMN ... NOT NULL DEFAULT`，
  升级是常数时间、不重写数据；老行读出来时由 `applyDefaults()` 归一到与升级前**完全一致**
  的行为（`install_mode` 默认 `npx`、`listen_host` 默认 `127.0.0.1`、`start_mode` 默认 `auto`，
  且 `on_demand` 与 `start_mode` 互为镜像；`dsh_version` 的空串就是「跟随 latest」，所以老实例不会因为
  多了这一列突然钉在某个版本上）。`TestMigrationV1ToV2` 与 `TestMigrationV2ToV3` 各造一个真旧库、
  插旧行，再用新代码打开，断言旧数据保留且默认值正确。

## 6. HTTP 面

| 路径 | 说明 |
|---|---|
| `/__dsh_gate/login|logout|healthz|shutdown` | 保留现状（健康检查不需要登录；门户的 healthz 语义是「门户自己在服务」） |
| `/` | 门户首页：登录后按角色分流到「我的实例」或管理台 |
| `/admin`、`/admin/*` | 管理台页面（仅 `admin`），写操作是普通表单 + PRG，状态由一段内联脚本自动刷新 |
| `/_portal/api/me` | 当前用户、角色、可见实例列表 |
| `/_portal/api/admin/state`（GET） | 管理状态 JSON：用户（**不含哈希**）、实例、授权矩阵、审计、计数 |
| `/_portal/api/admin/live`（GET） | 页面自动刷新用的实时状态：实例运行 / 安装进度、统计、审计增量；`?env=1` 时才做环境探测 |
| `/_portal/api/admin/action`（POST） | 管理动作 JSON：与页面**同一条**实现（`user.create|passwd|role|toggle|delete`、`grants.save`、实例 CRUD 与安装） |
| `/_portal/api/local`（POST） | 本机控制 API：只接受回环来源 + `<状态目录>/portal.token` 的 Bearer 令牌；动作为 `list` / `start` / `stop` / `restart` / `remove`（`remove` 可带 `purgeHome` / `purgeInstall`） |
| `/i/<id>/…` | 实例反向代理（需登录 **且** 命中 grants，或 admin） |

管理 API 的两道闸：**必须是管理员**；写操作**只接受 `application/json`** 并要求同源
（表单发不出 JSON 的 content-type，等于免费挡掉一类 CSRF；同源校验是纵深）。页面走
表单时用的是双提交 CSRF 令牌（见第 6 节末尾）。

授权判定（关键）：**先取会话，再按会话里的 user 查 grants**；路径里的 `<id>` 只当查询键，绝不作为授权依据。实例 id 用不透明短 id（≥8 位随机），避免枚举。**未命中 grants 时返回 404**（与「实例不存在」同形），并把这次尝试写进审计（服务端仍能区分两者的原因）。

### 6.1 i18n（本期一并做）

- **语言集合**：`zh-CN`（默认，回退语言）+ `en`。
- **消息目录**：新增 `go/internal/i18n/`，**零第三方依赖**：Go 源码内的 `map[string]string`（或 `//go:embed` 的 JSON），键名稳定、编译期可查。缺键时回退 `zh-CN` 并打一条 `warn`（不 panic）。
- **客户端语言选择**：命令行按 `--lang` > `DSH_GATE_LANG` > `LC_ALL`/`LANG` 推导；页面按 `?lang=`（一次性写入 Cookie）> `dsh_gate_lang` Cookie > `Accept-Language` > `zh-CN`。
- **覆盖面**：所有 CLI 输出（帮助、错误、状态、`install` / `dsh` / `manage` 等命令）、登录页、门户页、管理台、等待/错误页。**被代理的 dsh 页面不注入、不改写**（那是 DSH 自己的 i18n）。
- **测试**：key 集合一致性（两种语言的键必须完全相同，缺一即失败）；登录页与门户首页按语言各渲染一次断言；`--lang en` 跑一遍现有 CLI 冒烟。
- **代价**：现有硬编码中文要全量迁进目录（一次性工作），此后新增文案必须同时加两种语言。

CSRF：门户 Cookie 维持 `HttpOnly` + `SameSite=Lax`（或 Strict）+ `Path=/`；所有管理类写操作校验 `Origin`/`Referer` 同源，并带一次性 token。门户页 CSP `default-src 'self'`；被代理的 dsh 页面不注入门户 CSP。

## 7. 页面清单

管理台的分页（底部 Dock）顺序固定为 **实例 → 用户 → 实例授权 → 运行环境 → 审计**，
**默认停在「实例」**——打开管理台最常做的事就是看实例、管实例。提交表单后仍回到原来那一页。

- **登录页**（已有，加管理端标识即可）。
- **我的实例**：卡片列表（名称 / 状态：运行中·启动中·已停止·异常 / 最后活动 / 「进入」按钮）；无实例时给出提示文案。冷启动时进入等待页（复用现有等待/重新进入页的风格，2 秒轮询）。
- **管理台 · 用户**：列表、新建、改密、禁用/启用、角色、删除。
- **管理台 · 实例**（默认页）：列表（状态、端口、DSH_HOME、拥有者、运行状态、安装方式、
  钉住的 dsh 版本与安装阶段）、新建（自动分配端口与 id，可同时指定安装方式、dsh 版本与
  安装目录）、每行「启动 / 停止 / 重启」
  「安装 / 重装」「删除」，以及收起的「编辑配置」（可改安装方式与 dsh 版本）。
  启停语义：**停止**只是让进程停下，不改 enabled——机器或管理端重启后它仍按
  `start_mode` 回来；要彻底不拉起请停用它。**保存配置不自动重启**，改完还要点
  「重启」才生效（监督者的端口与目录是构造时固定的），这正是「停止 → 改配置 →
  重启」这条可控链路。
  删除支持两个勾选项：**删数据目录（实际生效的 `DSH_HOME`）** 与 **删安装目录**，
  默认都不勾（只解除登记）。勾选删除时先停实例、再删目录、最后删记录；只删绝对路径，
  拒绝根目录、当前用户家目录这类危险目标，审计 detail 记录删了哪些目录。
- **管理台 · dsh 运行环境检测**：node / npm / npx / pnpm / git / dsh 的状态、版本、路径；缺哪个给补装提示；附平台、管理端自备的 Node 与 pnpm、本机已有 dsh。检测是只读的。
- **管理台 · 实例授权**：用户 × 实例的勾选矩阵（一次提交差异）。
- **管理台 · 审计**：最近 N 条（时间、谁、对谁、动作、结果、IP）。

管理台的写操作都是普通表单 POST + 303 PRG；页面上的**状态自动刷新**（安装进度、运行状态、
环境检测、审计）由一段内联脚本完成。CSP 仍是 `default-src 'none'`，只按脚本内容的 sha256
放行这一段（`script-src 'sha256-…'`）并允许同源 `fetch`（`connect-src 'self'`），外加登录页
同样需要的 `img-src data:`（导航图标的遮罩图）。脚本只轮询
`/_portal/api/admin/live` 并改文本、class、`hidden`，**不整页重载**——正在填的输入框、展开的
折叠面板、选中的分页与滚动位置都不会被打断；标签页在后台时轮询暂停，环境探测仅在该页可见时
按 ≥10 秒的间隔做。安装进度不再需要手动刷新，也不需要前端构建。

## 8. 命令行面（与页面能力对等）

**已实现**：管理端生命周期、实例、用户与授权三层命令，能力与管理台页面一一对应。

```text
# 管理端
dsh-gate install [--host --port --state-dir --admin --random --password-stdin --start --service --bin-dir]
dsh-gate start   [--host --port --tls-cert --tls-key --public-url --state-dir --db --npm-registry --trusted-host --foreground]
dsh-gate stop    [--wait]
dsh-gate restart [--state-dir --wait]
dsh-gate status  [--state-dir --db]
dsh-gate uninstall [--service --purge --state-dir]
dsh-gate service <unit|install|uninstall|status> [...]

# 用户
dsh-gate manage user add <名字> [--role admin|user] [--random|--password-stdin]
dsh-gate manage user passwd <名字> [--random|--password-stdin]
dsh-gate manage user role <名字> <admin|user>
dsh-gate manage user disable|enable <名字>
dsh-gate manage user list
dsh-gate manage user rm <名字>

# 实例
dsh-gate dsh list
dsh-gate dsh add <名字> [--dsh-home <目录>] [--config-dir <目录>] [--workspace <目录>]
                         [--dsh-port N] [--listen-host <地址>] [--start-mode auto|ondemand]
                         [--owner <用户>] [--install-mode npx|git|local|pnpm] [--install-dir <目录>]
                         [--dsh-version <版本|dist-tag>]
dsh-gate dsh edit <名字|id> [同 add 的字段，只改显式给出的]
dsh-gate dsh rename <旧名或 id> <新名>
dsh-gate dsh start|stop|restart <名字或 id>
dsh-gate dsh install <名字或 id> [--install-mode …] [--install-dir …] [--dsh-version …]
                                   [--npm-registry <地址>] [--git-url <地址>] [--git-ref <引用>]
dsh-gate dsh rm <名字或 id> [--purge] [--purge-install]

# 授权
dsh-gate manage grant add <用户> <实例名或 id>
dsh-gate manage grant rm  <用户> <实例名或 id>
dsh-gate manage grant list [<用户>]

# 公共选项（dsh / manage 的每条子命令都认）
--db <路径>         管理端数据库（默认 <状态目录>/portal.db）
--state-dir <目录>  网关状态目录
--lang <zh-CN|en>   输出语言（等同于 DSH_GATE_LANG / LC_ALL / LANG）
```

**为什么实例命令独立成 `dsh`、而不是塞进 `manage`**：`manage` 管的是「人」（用户与授权），
`dsh` 管的是「实例」。混在一起会让「哪条命令动哪份数据」变得含糊，而 `/admin` 页面本来就
按「实例 / 用户 / 授权」分页。命令名与页面分页一一对应，找起来最快。

**为什么动进程的命令要求管理端在运行**：`dsh start|stop|restart` 与 `dsh rm --purge` 走
管理端的本机控制 API（`/_portal/api/local`，只接受回环 + `portal.token`），由监督者执行。
CLI 不绕过管理端直接拉进程，否则会出现「CLI 拉起一个、管理端不认识」的孤儿 dsh。反过来，
只读写数据库的命令（`dsh list|add|edit|rename|rm`、`manage ...`）在管理端没跑时也能用。

**为什么用 `manage` 而不是 `portal` / `admin`**：`/admin` 已经是管理台页面路径、`--role admin`
已经是角色名，再用 `admin` 当命令名会出现「`admin` 的 `admin` 里改 `admin`」这种绕口令；而
「门户」这个词指的是同一个东西的另一个说法。Go 包名 `internal/portal` 与数据库名 `portal.db`
保持不变（改它们只会制造无谓的迁移负担），只是**用户可见的命令名**统一成 `manage`。

**命令行管不到的边界**：管理端的登录会话在进程内存里，所以命令行改口令/停用用户后，运行中的
管理端不会立刻把那个人踢下线（重启管理端，或在页面上做同样操作才会）。页面上做这些动作会立即吊销会话。

**长安装不进 HTTP 请求**：`dsh install` 在命令行**同步**执行（终端用户已经在等），在
`/admin` 页面里则是**进程内后台任务**（页面刷新看进度）。安装动辄十几分钟（git 方式要
clone + install + build），把 HTTP 请求挂在那里等是不行的。任务状态**只在内存里**：这是刻意的
——一个「运行中」的任务如果随管理端重启存活下来，也没有任何东西能继续推进它。

**安装失败不删记录**：创建实例记录与安装 dsh 是两步，记录先落库（拿到 id 与端口，页面马上
可见），安装失败只记日志、记录保留，用户可以改方式重装。`StartInstall` 对同一实例是幂等的
（已在跑就返回现有任务，不会重复 clone）。

**四种安装方式的实现要点**：

| 方式 | 实现 | 备注 |
|---|---|---|
| `npx` | `npx --yes @deepseek-ai/dsh[@<钉住的版本>]` | 装完做一次冒烟验证；自备 Node 的 bin 目录必须进 PATH（npx 的 shebang 是 `#!/usr/bin/env node`） |
| `git` | clone → `pnpm install`（先 `--frozen-lockfile`）→ `pnpm run build` | 启动命令用 `node <repo>/apps/cli/lib/bin.js` 而不是 pnpm 包装器，便于监督进程切分命令；`--frozen-lockfile` **只在 lockfile 不一致时**才回退重装（网络超时回退只会白等一轮） |
| `local` | **什么都不跑**：核对目录里有带 `dsh` 脚本的 `package.json`、`node_modules` 存在、`node` / `pnpm` 可用 | 启动命令是 `pnpm --dir '<源码目录>' run dsh`（官方 Run from source）；`<安装目录>` 既可填仓库根，也可填它的父目录（仓库在 `<目录>/deepseek-harness` 时）；`node_modules` 缺失与脚本缺失在**安装阶段**就报出来，不留到启动 |
| `pnpm` | `pnpm add @deepseek-ai/dsh[@<钉住的版本>] --dir <安装目录>/dsh` | 装到实例自己的目录而不是全局：管理端可能管着多个实例，全局安装会互相覆盖，也让「删实例」带不走自己那份 |

`local` 存在的理由：clone / install / build 是最容易失败的三步（慢网络、私有镜像、node-gyp），
交给用户自己的终端往往一次就过。网关只负责**认出那份源码并把它当服务跑起来**，所以它不联网、
不装依赖，安装几乎是瞬时的；版本探测（`pnpm --dir <repo> run --silent dsh --version`）是尽力而为，
取不到就留空。命令里的路径一律用单引号包住——`SplitCommand` 眼里反斜杠在引号外是转义符，
Windows 路径不引起来会被拆成不存在的假路径。

**钉版本（`dsh_version`）的实现要点**：`npx` 与 `pnpm` 把包名拼成 `@deepseek-ai/dsh@<版本或
dist-tag>`，留空就是不带 `@` 的裸包名（跟随 latest）。取值先过 `ValidateDshVersion`——只认
字母、数字与 `. - + ~ ^ *`，且**不得以 `-` 开头**：这个字符串最终会出现在 npm 包规格里，注入
防护比「像不像个版本号」更重要。留空时如果 latest 跑不起来会回退到实测可用的旧版本（`versionPlan`
返回一串候选），**点了名就只试那一个**，装不上直接报错，不做静默替换。源码方式（`git` / `local`）
不认这一栏：git 认 `--git-ref`，local 认目录。创建实例/`dsh add` 时同时给出两者会被当场拦下，
`dsh install` 阶段发现历史记录里同时存在则报错并给出取消钉住的命令。版本写回实例记录：安装任务
成功时写、CLI `dsh install` 结束时也写，列表那一格显示的始终是当前钉的值；从未点过「安装」的
实例也认它——探测启动命令时带上这个版本，而不是回落到 latest。

**pnpm 10+ 必须显式放行构建脚本**：pnpm 默认拒绝依赖的 `postinstall`（供应链安全），而 dsh 的
原生依赖（`node-pty`、`koffi`、`@deepseek-ai/dsh-subprocess-local`、`protobufjs`、`@google/genai`）
正是靠它准备平台产物。不放行时 pnpm 会报 `Ignored build scripts` 并**以失败退出**（不是警告），
dsh 装不起来。安装命令用 `--allow-build=<包名>` **逐个点名**，不用通配：只放行 dsh 真正需要的
那几个，不把整条依赖链的脚本都放开——这正是 pnpm 默认拒绝的初衷。

**自备运行时必须显式传 bin 目录**：网关会在状态目录里自己下一份 Node（服务器没有 Node 也能跑）。
任何只读**进程 PATH** 的工具查找都会在有自备运行时、但系统 PATH 里没有对应命令的机器上失败
（容器、裸机尤其明显）。`EnsureDSHWithBin` / `EnsurePnpmWithBin` 就是为了把 bin 目录显式传下去，
而不是依赖进程环境。

**实例生命周期已接入**：管理台与 `dsh` 子命令都能启动 / 停止 / 重启实例；管理端退出时按
监督者统一收尾。**尚未实现**：实例日志的独立查看入口（dsh 的输出目前只汇入管理端日志），
以及按需实例的空闲回收（`idle_timeout_s` 字段已就位）。

删除的语义：`dsh rm` 默认只解除管理端里的登记，**不动 dsh 的数据目录**（会话与工作区是用户
的数据）；要删目录必须显式给 `--purge`（数据目录）与 `--purge-install`（安装目录），且只删
绝对路径、拒绝根目录与当前用户家目录。`uninstall` **不删任何实例的数据目录**，只把它们列出来
让用户自己处理。

`stop` / `restart` / `uninstall` 这类机器级命令**不接受位置参数**（停单个实例用
`dsh-gate dsh stop <名字>`），不给你「手抖停全部」的机会。

## 9. 迁移与兼容

1. 首次启动管理端时：若存在 `<state-dir>/users.json`（旧单实例形态的账号表），导入为管理端用户（**第一个用户自动成为 admin**），旧口令继续可用。
2. 旧的多实例配置文件（`--config` JSON）不再被读取：实例一律通过 `dsh add` / 管理台重建，原实例目录可以原样填进 `--dsh-home`。
3. 旧的单实例命令已彻底移除：`run` / `setup` / `check` / `user`，以及按实例名的 `stop` / `restart` / `uninstall`。对应新命令见第 8 节。升级后旧进程不会自动被接管：先 `pkill -f "dsh web"`，再让管理端拉起实例。
4. 回滚：管理端数据独立在 `portal.db`；要回旧版本二进制，先备份 `portal.db` 与各实例的 `DSH_HOME`。

## 10. 安全清单

- 门户对外必须 TLS（明文下 dsh 会话 Cookie 明文过网）；或放在内网穿透工具后面。
- 实例监听**只绑回环**；门户是唯一对外口。
- 每个实例的 `--trusted-host` 必须包含门户 authority（由 `server.TrustedHosts` 统一推导，
  详见第 5 节第 4 条；容器端口映射 / NAT 下必须显式给 `--public-url` 才会包含）。
- Cookie 改写只处理**上游** `Set-Cookie`，绝不动门户自己的会话 Cookie。
- 实例 id 白名单 `^[a-z0-9][a-z0-9-]{5,}$`，且必须命中 grants；防路径穿越。
- 管理端点挂现有 limiter；登录失败限速沿用（同 IP 指数退避）。
- 本机控制 API 只接受回环来源，并要求 `<状态目录>/portal.token`（0600，每次启动刷新）里的
  Bearer 令牌；令牌不写日志、不进命令行参数。
- 密码哈希沿用 PBKDF2；管理员可强制重置并踢下线（会话表按用户索引）。
- 审计：登录、进实例、用户/实例/授权的增删改、实例启停、失败尝试，全部落 `audit`。

## 11. 测试与验收

- **单测**：store（schema/迁移/并发）、授权判定、路径解析（剥前缀/挂载根重定向）、Cookie 改写（只改上游、补 Secure）、CSRF、i18n key 集合一致性。
- **集成**：`go/internal/portal/*_test.go` + `internal/dshtest` 假 dsh：登录 → 列表 → 进入实例 → 代理 → 登出；**越权得 404**；管理员 CRUD 全链路；本机控制 API 的回环校验与令牌校验。
- **端到端**（`go/tests/portal-e2e.sh`，真 dsh）：两个实例 + 两个用户，断言
  1. A 的 Cookie 进不了 B 的挂载路径（**会话不串**，第 4 节要求 2）；
  2. 剥前缀后 `/i/a/assets/…` 命中上游 `/assets/…`；
  3. WebSocket 升级与 SSE 正常（不缓冲）；
  4. 未授权用户访问他人实例得 **404**（与不存在的实例同形，且两种情形都进了审计）。
- **回归**：`go test ./...` 与 `go/tests/portal-e2e.sh` 全绿；文档结构性一致性由 `go/doclint` 校验（锚点、图片、代码围栏、中英文混排）。

## 12. 分期

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P0** | SQLite + 迁移；i18n 骨架（zh-CN/en）；门户登录；我的实例；`/i/<id>/` 代理（剥前缀 + Cookie 改写 + 令牌交接）；管理台用户 CRUD；grant 矩阵；CLI 对等命令 | 一个管理员 + 两个用户 + 两个实例，能各自进入自己的实例；越权 404；`--lang en` 下页面与 CLI 均生效；现有测试全绿 |
| **P1** | 实例 CRUD/启停/重启（✅）；审计页（✅）；按需拉起（✅）；实例日志入口与空闲回收（待办） | 冷启动进入等待页并能自动拉起；空闲回收后再次进入正常 |
| **P2** | 角色细分（viewer）、资源看板、可选外部身份源 | 待定 |

## 13. 影响面与风险

- **依赖规则修订**：`go.mod` 不再是零依赖。这是本项目最大的自我约束变化，需在 README「贡献」里改口径，并在 CHANGELOG 里明确记录。若不想破例，可退回到「文件存储 + 自研查询」，但要放弃 SQL 的并发与查询能力。
- **i18n 的长期成本**：所有新增文案要同时维护两种语言；`doclint` 之外需要一条 key 完整性测试兜底。
- **子路径三大风险**：Cookie 串（靠改写 Path 解决，必须有 e2e 锁死）、`localStorage` 同源共享（已知限制）、上游插件绝对路径（需逐个插件确认）。
- **资源**：每个实例一个 dsh 进程（另有 Node 运行时开销）。P0 先常驻，P1 再按需。
- **升级路径**：管理端单形态。旧形态只保留「首次启动导入 `users.json` 账号」这一条兼容路径，
  不再让两套运行方式长期共存——双形态会让每条命令都要猜「这次动的是哪一张表」。

## 14. 决议记录

第 2.1 节的五项已在 2026-10-14 定稿（实例 id 用不透明随机短 id、越权返回 404、角色先只分 admin/user、用 `modernc.org/sqlite`、本期一并做 i18n）。

实施顺序（P0 内部）：

1. `internal/portal`：SQLite schema + store + 从旧 `users.json` 的账号迁移（含单测）；
2. `internal/i18n`：消息目录 + key 完整性测试，并把现有 CLI/登录页文案迁进去；
3. 门户服务：路由骨架 + 登录复用 + 「我的实例」；
4. 子路径代理：剥前缀、`Set-Cookie` Path 改写（**先写 e2e 再写实现**）、令牌交接、挂载根 303；
5. 管理台（用户 CRUD + grant 矩阵）与 CLI 对等命令；
6. `go/tests/portal-e2e.sh` 与回归。

过程中若发现与第 4 节六条硬要求冲突的实现细节（尤其是 Cookie 改写面），先改本文档再改代码。

## 15. 实施进度

**P0 与 P1 主体已完成**。真机验收：`go/tests/portal-e2e.sh`（两个真实 dsh + 两个用户，
五条断言）在本机跑通；文档结构性一致性（中英文、锚点、图片、代码围栏）由 `go/doclint` 校验。

| 项 | 状态 | 说明 |
|---|---|---|
| 设计文档 | ✅ | 本文档（`9bae07b`） |
| `internal/portal` 持久层：SQLite + 旧账号迁移 | ✅ | `48f31b1`；用户/实例/授权/审计四表 + 12 个用例；后续加列到 schema v2 |
| `internal/i18n` 骨架（zh-CN/en） | ✅ | `72bde9c`；目录 + 键常量 + 完整性/占位符一致性测试；登录页与失败提示已双语 |
| 门户服务（登录复用 + 「我的实例」） | ✅ | `509c9b5`；`auth.Verifier` 接口 + 会话主体（Sessions 绑定登录名）；`portal.Server` 提供 `/` 与 `/_portal/api/me`；语言判定与登录页共用同一套优先级与偏好 Cookie |
| 子路径机械层 `internal/mountproxy` | ✅ | `1cb7afb`；剥前缀 / `Set-Cookie` Path 改写 / 挂载根 303 / 流式与升级穿透；含一个「上游只设响应头不写正文时 Cookie 绕过改写」的真 bug 修复，以及 `392dcd7` 补的**根相对 `Location` 改写** |
| 实例挂载接入门户（授权 + 剥前缀 + Cookie 改写） | ✅ | 先授权再建上游；越权与「不存在」同形 404；进实例与越权都写审计 |
| 管理台（用户 CRUD + 授权矩阵 + 审计页） | ✅ | `6280dad`；写操作是普通表单 + PRG，状态由内联脚本轮询 `/_portal/api/admin/live` 自动刷新；双提交 CSRF + 同源校验；改密/停用/删除同时吊销该用户全部会话；最后一个管理员的操作被拒绝并给出说明 |
| 管理 JSON API | ✅ | `/_portal/api/admin/state`（读，绝不含口令哈希）与 `/_portal/api/admin/action`（写，与页面共用同一份动作实现）；只接受 JSON + 同源校验 |
| 实例运行环境接入（监督 + 令牌交接 + 等待页） | ✅ | `392dcd7`；每实例一个 `dshproc` 监督者 + 一个 `proxy.Handler`；`dsh-gate start` 组装并监听；常驻实例启动时拉起，按需实例进场时惰性拉起；门户的 `healthz` 语义见 `50cb843`（就绪 = 自己在服务） |
| 实例生命周期（启动 / 停止 / 重启 / 删除） | ✅ | 管理台按钮与 `dsh-gate dsh start|stop|restart` 共用 `portalruntime` 的同一条路径；删除实例可勾选删数据目录 / 安装目录；本机控制 API + `portal.token` 见 6.1 与第 8 节 |
| 管理台五页与默认实例页 | ✅ | 分页顺序固定为 实例 → 用户 → 实例授权 → 运行环境 → 审计，默认停在「实例」；编辑配置只落库、不自动重启 |
| CLI 对等命令（`dsh` / `manage`） | ✅ | 实例增删改查、安装、启停、改名；用户与授权；`--db` / `--state-dir` / `--lang` 公共选项 |
| 管理端单形态 CLI 重构 | ✅ | 移除单实例命令（`run` / `setup` / `check` / `user` / 按实例名启停卸载）；新增 `install` / `start` / `stop` / `restart` / `status` / `uninstall` / `service` 与 `dsh` 子命令；启动参数保存到 `portal.json` 供 `restart` 使用 |
| 旧 CLI 输出完整迁入 i18n | 🔶 P2 待办 | 门户相关界面（登录页、门户页、管理台、`dsh` / `manage` 命令）已双语；部分旧命令（`service` / `model` / `plugin` / `uninstall`）的输出与帮助仍是中文 |
| 文档一致性测试（结构） | ✅ | `go/doclint` 校验中英文文档、目录锚点、图片引用与代码围栏 |

### P2 待办

1. 旧 CLI 输出与帮助完整迁入 i18n（约 120 条用户可见串 + 帮助文本 + flag 描述）；
2. 实例日志的独立查看入口（dsh 输出目前只汇入管理端日志）；
3. 按需实例的空闲回收（`idle_timeout_s` 字段已就位，尚未实现回收器）；
4. 在线会话列表与「踢下线」的页面/命令（`Sessions.DestroySubject` 已就位）。

已知与本改造无关的环境问题：Windows 上 `internal/config`（POSIX 路径）、
`internal/dshproc`（软链需特权、进程与端口语义）、`internal/dshsettings` 与
`internal/users`（POSIX 权限语义）、`cmd/dsh-gate` 的部分用例会失败——已在改动前的
提交 `228014a` 上逐条比对确认是既有问题，Linux/macOS 上不受影响。
