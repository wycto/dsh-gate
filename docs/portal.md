# dsh-gate 门户（Portal）设计草案

> 状态：**设计草案，待确认后实施**。本文只描述目标与设计，不代表已实现。
> 结论未定处集中列在最后一节「待确认」。

## 1. 目标

把 dsh-gate 从「每个实例一个独立网关」升级成 **一个门户 + N 个 dsh 实例**：

- 门户自带**管理页面**与**数据库**：启动后浏览器就能访问，不依赖命令行也能完成日常管理。
- **用户体系**：管理员建用户、改密、禁用、给角色；用户登录后只看到**属于自己的实例**。
- **实例管理**：在页面里新建 / 启停 / 重启 / 看日志；实例与用户的关系由管理员分配。
- 用户路径：登录门户 → 「我的实例」→ 点击 → 直接进入该实例的会话（不需要知道端口、不需要令牌）。
- **命令行与页面能力对等**：原有 `run` / `user` / `service` / `stop` 等命令保持兼容，新增门户与实例管理子命令。

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
- 现有单实例形态（`dsh-gate run --dsh-home …`，一个监听口 + 一个 dsh）**行为完全不变**。

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
  install_mode TEXT NOT NULL DEFAULT 'npx',                 -- npx | git | pnpm（schema v2）
  install_dir TEXT NOT NULL DEFAULT '',                     -- dsh 安装目录（schema v2）
  config_dir TEXT NOT NULL DEFAULT '',                      -- 配置目录；留空回落 dsh_home（schema v2）
  listen_host TEXT NOT NULL DEFAULT '127.0.0.1',            -- 该实例 dsh 的监听地址（schema v2）
  start_mode TEXT NOT NULL DEFAULT 'auto',                  -- auto | ondemand（schema v2）
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
- 驱动建议：**`modernc.org/sqlite`（纯 Go，无 CGO）**，六个发行目标仍可交叉编译。
- 影响面：`go.mod` 出现依赖树；`go/build.sh` 与交叉编译需重新验证；二进制体积变大；README 的「零第三方依赖」表述必须改写为「仅 SQLite 一处第三方依赖，其余标准库」。
- **迁移是加列式的**（`PRAGMA user_version` 记版本，当前 v2）：`ALTER TABLE ... ADD COLUMN ... NOT NULL DEFAULT`
  五列一次加完，升级是常数时间、不重写数据；老行读出来时由 `applyDefaults()` 归一到与升级前**完全一致**
  的行为（`install_mode` 默认 `npx`、`listen_host` 默认 `127.0.0.1`、`start_mode` 默认 `auto`，
  且 `on_demand` 与 `start_mode` 互为镜像）。`TestMigrationV1ToV2` 造真 v1 库、插旧行、再用新代码
  打开，断言旧数据保留且默认值正确。

## 6. HTTP 面

| 路径 | 说明 |
|---|---|
| `/__dsh_gate/login|logout|healthz|shutdown` | 保留现状（健康检查不需要登录；门户的 healthz 语义是「门户自己在服务」） |
| `/` | 门户首页：登录后按角色分流到「我的实例」或管理台 |
| `/admin`、`/admin/*` | 管理台页面（仅 `admin`），无 JS 的普通表单 + PRG |
| `/_portal/api/me` | 当前用户、角色、可见实例列表 |
| `/_portal/api/admin/state`（GET） | 管理状态 JSON：用户（**不含哈希**）、实例、授权矩阵、审计、计数 |
| `/_portal/api/admin/action`（POST） | 管理动作 JSON：与页面**同一条**实现（`user.create|passwd|role|toggle|delete`、`grants.save`） |
| `/i/<id>/…` | 实例反向代理（需登录 **且** 命中 grants，或 admin） |

管理 API 的两道闸：**必须是管理员**；写操作**只接受 `application/json`** 并要求同源
（表单发不出 JSON 的 content-type，等于免费挡掉一类 CSRF；同源校验是纵深）。页面走
表单时用的是双提交 CSRF 令牌（见第 6 节末尾）。

授权判定（关键）：**先取会话，再按会话里的 user 查 grants**；路径里的 `<id>` 只当查询键，绝不作为授权依据。实例 id 用不透明短 id（≥8 位随机），避免枚举。**未命中 grants 时返回 404**（与「实例不存在」同形），并把这次尝试写进审计（服务端仍能区分两者的原因）。

### 6.1 i18n（本期一并做）

- **语言集合**：`zh-CN`（默认，回退语言）+ `en`。
- **消息目录**：新增 `go/internal/i18n/`，**零第三方依赖**：Go 源码内的 `map[string]string`（或 `//go:embed` 的 JSON），键名稳定、编译期可查。缺键时回退 `zh-CN` 并打一条 `warn`（不 panic）。
- **客户端语言选择**：命令行按 `--lang` > `DSH_GATE_LANG` > `LC_ALL`/`LANG` 推导；页面按 `?lang=`（一次性写入 Cookie）> `dsh_gate_lang` Cookie > `Accept-Language` > `zh-CN`。
- **覆盖面**：所有 CLI 输出（帮助、错误、状态、`setup` 向导）、登录页、门户页、管理台、等待/错误页。**被代理的 dsh 页面不注入、不改写**（那是 DSH 自己的 i18n）。
- **测试**：key 集合一致性（两种语言的键必须完全相同，缺一即失败）；登录页与门户首页按语言各渲染一次断言；`--lang en` 跑一遍现有 CLI 冒烟。
- **代价**：现有硬编码中文要全量迁进目录（一次性工作），此后新增文案必须同时加两种语言。

CSRF：门户 Cookie 维持 `HttpOnly` + `SameSite=Lax`（或 Strict）+ `Path=/`；所有管理类写操作校验 `Origin`/`Referer` 同源，并带一次性 token。门户页 CSP `default-src 'self'`；被代理的 dsh 页面不注入门户 CSP。

## 7. 页面清单

- **登录页**（已有，加管理端标识即可）。
- **我的实例**：卡片列表（名称 / 状态：运行中·启动中·已停止·异常 / 最后活动 / 「进入」按钮）；无实例时给出提示文案。冷启动时进入等待页（复用现有等待/重新进入页的风格，2 秒轮询）。
- **管理台 · 用户**：列表、新建、改密、禁用/启用、角色、删除。
- **管理台 · 实例**：列表（状态、端口、DSH_HOME、拥有者、安装方式与安装阶段）、新建（自动分配端口与 id，可同时指定安装方式与安装目录）、每行「安装」「删除」、编辑。
- **管理台 · dsh 运行环境检测**：node / npm / npx / pnpm / git / dsh 的状态、版本、路径；缺哪个给补装提示；附平台、管理端自备的 Node 与 pnpm、本机已有 dsh。检测是只读的。
- **管理台 · 授权**：用户 × 实例的勾选矩阵（一次提交差异）。
- **管理台 · 审计**：最近 N 条（时间、谁、对谁、动作、结果、IP）。

管理台**没有 JavaScript**（CSP `default-src 'none'`）：所有写操作都是普通表单 POST + 303 PRG。
代价是安装进度只能靠**刷新页面**看到（服务端渲染），换来的是不需要前端构建、也没有 XSS 面。

## 8. 命令行面（与页面能力对等）

**已实现**（管理端命令统一挂在 `dsh-gate manage` 下；`portal` 作为兼容别名保留）：

```text
# 用户
dsh-gate manage user add <名字> [--role admin|user] [--random|--password-stdin]
dsh-gate manage user passwd <名字> [--random|--password-stdin]
dsh-gate manage user role <名字> <admin|user>
dsh-gate manage user disable|enable <名字>
dsh-gate manage user list
dsh-gate manage user rm <名字>

# 实例
dsh-gate manage instance list
dsh-gate manage instance add <名字> --dsh-home <目录> [--config-dir <目录>] [--workspace <目录>]
                                   [--owner <用户>] [--port N] [--listen-host <地址>] [--on-demand]
                                   [--install-mode npx|git|pnpm] [--install-dir <目录>]
dsh-gate manage instance install <名字> [--install-mode npx|git|pnpm] [--install-dir <目录>]
                                        [--npm-registry <地址>] [--git-url <地址>] [--git-ref <引用>]
dsh-gate manage instance rename <旧名或 id> <新名>
dsh-gate manage instance rm <名字或 id>

# 授权
dsh-gate manage grant add <用户> <实例名或 id>
dsh-gate manage grant rm  <用户> <实例名或 id>
dsh-gate manage grant list [<用户>]

# 公共选项（每条子命令都认）
--db <路径>         管理端数据库（默认 <状态目录>/portal.db）
--state-dir <目录>  网关状态目录
--lang <zh-CN|en>   输出语言（等同于 DSH_GATE_LANG / LC_ALL / LANG）
```

**为什么用 `manage` 前缀而不是复用 `dsh-gate user`**：现有的 `user` 子命令管的是单实例形态的
`users.json`，而管理端账号在 SQLite 里，两套语义不同。让同一条命令按「有没有 portal.db」猜模式，
会在迁移中途出现最难查的那类问题（同一条命令今天改这份表、明天改那份表）。多打一个词换来的是
「哪条命令动哪份数据」永远确定。

**为什么用 `manage` 而不是 `portal` / `admin`**：`/admin` 已经是管理台页面路径、`--role admin`
已经是角色名，再用 `admin` 当命令名会出现「`admin` 的 `admin` 里改 `admin`」这种绕口令；而
「门户」这个词指的是同一个东西的另一个说法。Go 包名 `internal/portal` 与数据库名 `portal.db`
保持不变（改它们只会制造无谓的迁移负担），只是**用户可见的命令名**统一成 `manage`。

**命令行管不到的边界**：管理端的登录会话在进程内存里，所以命令行改口令/停用用户后，运行中的
管理端不会立刻把那个人踢下线（重启管理端，或在页面上做同样操作才会）。页面上做这些动作会立即吊销会话。

**长安装不进 HTTP 请求**：`instance install` 在命令行**同步**执行（终端用户已经在等），在
`/admin` 页面里则是**进程内后台任务**（页面刷新看进度）。安装动辄十几分钟（git 方式要
clone + install + build），把 HTTP 请求挂在那里等是不行的。任务状态**只在内存里**：这是刻意的
——一个「运行中」的任务如果随管理端重启存活下来，也没有任何东西能继续推进它。

**安装失败不删记录**：创建实例记录与安装 dsh 是两步，记录先落库（拿到 id 与端口，页面马上
可见），安装失败只记日志、记录保留，用户可以改方式重装。`StartInstall` 对同一实例是幂等的
（已在跑就返回现有任务，不会重复 clone）。

**三种安装方式的实现要点**：

| 方式 | 实现 | 备注 |
|---|---|---|
| `npx` | `npx --yes @deepseek-ai/dsh` | 装完做一次冒烟验证；自备 Node 的 bin 目录必须进 PATH（npx 的 shebang 是 `#!/usr/bin/env node`） |
| `git` | clone → `pnpm install`（先 `--frozen-lockfile`）→ `pnpm run build` | 启动命令用 `node <repo>/apps/cli/lib/bin.js` 而不是 pnpm 包装器，便于监督进程切分命令；`--frozen-lockfile` **只在 lockfile 不一致时**才回退重装（网络超时回退只会白等一轮） |
| `pnpm` | `pnpm add @deepseek-ai/dsh --dir <安装目录>/dsh` | 装到实例自己的目录而不是全局：管理端可能管着多个实例，全局安装会互相覆盖，也让「删实例」带不走自己那份 |

**pnpm 10+ 必须显式放行构建脚本**：pnpm 默认拒绝依赖的 `postinstall`（供应链安全），而 dsh 的
原生依赖（`node-pty`、`koffi`、`@deepseek-ai/dsh-subprocess-local`、`protobufjs`、`@google/genai`）
正是靠它准备平台产物。不放行时 pnpm 会报 `Ignored build scripts` 并**以失败退出**（不是警告），
dsh 装不起来。安装命令用 `--allow-build=<包名>` **逐个点名**，不用通配：只放行 dsh 真正需要的
那几个，不把整条依赖链的脚本都放开——这正是 pnpm 默认拒绝的初衷。

**自备运行时必须显式传 bin 目录**：网关会在状态目录里自己下一份 Node（服务器没有 Node 也能跑）。
任何只读**进程 PATH** 的工具查找都会在有自备运行时、但系统 PATH 里没有对应命令的机器上失败
（容器、裸机尤其明显）。`EnsureDSHWithBin` / `EnsurePnpmWithBin` 就是为了把 bin 目录显式传下去，
而不是依赖进程环境。

**尚未实现**：`instance start|stop|restart|logs`（管理台与命令行都还没有；实例的生命周期
由管理端进程在用户访问时接管）。

约定不变：`stop` / `restart` / `rm` 这类「不写目标会动到多个」的命令，仍然必须写明目标，不写只列清单（退出码 2）。
`dsh-gate manage instance rm` 只解除管理端里的登记，**不动 dsh 的数据目录**（会话与工作区是用户的数据）。
兼容：单实例形态 `dsh-gate run --dsh-home ~/.dsh --port 3082` 行为完全不变。

## 9. 迁移与兼容

1. 首次启动门户时：若存在 `<state-dir>/users.json`（现状的实例级账号表），导入为门户用户（**第一个用户自动成为 admin**），并为原实例生成一条 grant（该用户 → 该实例）——升级后行为不变。
2. `--config` 多实例 JSON：一次性导入 `instances` 表（保留文件，不再作为运行期真源）。
3. 原 `run` 单实例路径完全保留；不启动门户时不读 SQLite。
4. 回滚：迁移前把 `users.json` 复制为 `users.json.bak`；门户数据独立在 `portal.db`，删掉即回到旧形态。

## 10. 安全清单

- 门户对外必须 TLS（明文下 dsh 会话 Cookie 明文过网）；或放在内网穿透工具后面。
- 实例监听**只绑回环**；门户是唯一对外口。
- 每个实例的 `--trusted-host` 必须包含门户 authority（由 `server.TrustedHosts` 统一推导，
  详见第 5 节第 4 条；容器端口映射 / NAT 下必须显式给 `--public-url` 才会包含）。
- Cookie 改写只处理**上游** `Set-Cookie`，绝不动门户自己的会话 Cookie。
- 实例 id 白名单 `^[a-z0-9][a-z0-9-]{5,}$`，且必须命中 grants；防路径穿越。
- 管理端点挂现有 limiter；登录失败限速沿用（同 IP 指数退避）。
- 密码哈希沿用 PBKDF2；管理员可强制重置并踢下线（会话表按用户索引）。
- 审计：登录、进实例、用户/实例/授权的增删改、实例启停、失败尝试，全部落 `audit`。

## 11. 测试与验收

- **单测**：store（schema/迁移/并发）、授权判定、路径解析（剥前缀/挂载根重定向）、Cookie 改写（只改上游、补 Secure）、CSRF、i18n key 集合一致性。
- **集成**：扩展现有 `go/internal/server/integration_test.go` + `internal/dshtest` 假 dsh：登录 → 列表 → 进入实例 → 代理 → 登出；**越权得 404**；管理员 CRUD 全链路。
- **端到端**（新增 `go/tests/portal-e2e.sh`，真 dsh）：两个实例 + 两个用户，断言
  1. A 的 Cookie 进不了 B 的挂载路径（**会话不串**，第 4 节要求 2）；
  2. 剥前缀后 `/i/a/assets/…` 命中上游 `/assets/…`；
  3. WebSocket 升级与 SSE 正常（不缓冲）；
  4. 未授权用户访问他人实例得 **404**（与不存在的实例同形，且两种情形都进了审计）。
- **回归**：现有 4 个 e2e 脚本与全部单测继续绿；`run` 单实例形态行为不变（同一套断言跑一遍旧形态）。

## 12. 分期

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P0** | SQLite + 迁移；i18n 骨架（zh-CN/en）；门户登录；我的实例；`/i/<id>/` 代理（剥前缀 + Cookie 改写 + 令牌交接）；管理台用户 CRUD；grant 矩阵；CLI 对等命令 | 一个管理员 + 两个用户 + 两个实例，能各自进入自己的实例；越权 404；`--lang en` 下页面与 CLI 均生效；现有测试全绿 |
| **P1** | 实例 CRUD/启停/重启/日志；在线会话踢人；审计页；按需拉起 + 空闲回收 | 冷启动进入等待页并能自动拉起；空闲回收后再次进入正常 |
| **P2** | 角色细分（viewer）、资源看板、可选外部身份源 | 待定 |

## 13. 影响面与风险

- **依赖规则修订**：`go.mod` 不再是零依赖。这是本项目最大的自我约束变化，需在 README「贡献」里改口径，并在 CHANGELOG 里明确记录。若不想破例，可退回到「文件存储 + 自研查询」，但要放弃 SQL 的并发与查询能力。
- **i18n 的长期成本**：所有新增文案要同时维护两种语言；`doclint` 之外需要一条 key 完整性测试兜底。
- **子路径三大风险**：Cookie 串（靠改写 Path 解决，必须有 e2e 锁死）、`localStorage` 同源共享（已知限制）、上游插件绝对路径（需逐个插件确认）。
- **资源**：每个实例一个 dsh 进程（另有 Node 运行时开销）。P0 先常驻，P1 再按需。
- **升级路径**：门户与旧 `run` 形态共存，避免「必须一次切干净」。

## 14. 决议记录

第 2.1 节的五项已在 2026-10-14 定稿（实例 id 用不透明随机短 id、越权返回 404、角色先只分 admin/user、用 `modernc.org/sqlite`、本期一并做 i18n）。

实施顺序（P0 内部）：

1. `internal/portal`：SQLite schema + store + 从 `users.json` / `--config` 的迁移（含单测）；
2. `internal/i18n`：消息目录 + key 完整性测试，并把现有 CLI/登录页文案迁进去；
3. 门户服务：路由骨架 + 登录复用 + 「我的实例」；
4. 子路径代理：剥前缀、`Set-Cookie` Path 改写（**先写 e2e 再写实现**）、令牌交接、挂载根 303；
5. 管理台（用户 CRUD + grant 矩阵）与 CLI 对等命令；
6. `go/tests/portal-e2e.sh` 与回归。

过程中若发现与第 4 节六条硬要求冲突的实现细节（尤其是 Cookie 改写面），先改本文档再改代码。

## 15. 实施进度

**P0 已完成**（除下表标注的那一条外）。真机验收：`go/tests/portal-e2e.sh`（两个真实
dsh + 两个用户，五条断言）与 `go/tests/single-instance-smoke.sh`（老的单实例 `run`
回归）都在 Windows + Git Bash 上退出码 0。

| P0 项 | 状态 | 说明 |
|---|---|---|
| 设计文档 | ✅ | 本文档（`9bae07b`） |
| `internal/portal` 持久层：SQLite + 旧账号迁移 | ✅ | `48f31b1`；用户/实例/授权/审计四表 + 12 个用例 |
| `internal/i18n` 骨架（zh-CN/en） | ✅ | `72bde9c`；目录 + 键常量 + 完整性/占位符一致性测试；登录页与失败提示已双语 |
| 门户服务（登录复用 + 「我的实例」） | ✅ | `509c9b5`；`auth.Verifier` 接口 + 会话主体（Sessions 绑定登录名）；`portal.Server` 提供 `/` 与 `/_portal/api/me`；语言判定与登录页共用同一套优先级与偏好 Cookie |
| 子路径机械层 `internal/mountproxy` | ✅ | `1cb7afb`；剥前缀 / `Set-Cookie` Path 改写 / 挂载根 303 / 流式与升级穿透；含一个「上游只设响应头不写正文时 Cookie 绕过改写」的真 bug 修复，以及 `392dcd7` 补的**根相对 `Location` 改写** |
| 实例挂载接入门户（授权 + 剥前缀 + Cookie 改写） | ✅ | 先授权再建上游；越权与「不存在」同形 404；进实例与越权都写审计 |
| 管理台（用户 CRUD + 授权矩阵 + 审计页） | ✅ | `6280dad`；无 JS 的普通表单 + PRG；双提交 CSRF + 同源校验；改密/停用/删除同时吊销该用户全部会话；最后一个管理员的操作被拒绝并给出说明 |
| 管理 JSON API | ✅ | `/_portal/api/admin/state`（读，绝不含口令哈希）与 `/_portal/api/admin/action`（写，与页面共用同一份动作实现）；只接受 JSON + 同源校验 |
| CLI 对等命令（`portal user/instance/grant`） | ✅ | `f877bec`；实例名与不透明 id 两种写法都认；`--db`/`--state-dir`/`--lang` 公共选项 |
| 实例运行环境接入（监督 + 令牌交接 + 等待页） | ✅ | `392dcd7`；每实例一个 `dshproc` 监督者 + 一个 `proxy.Handler`；`dsh-gate manage run` 组装并监听；常驻实例启动时拉起，按需实例进场时惰性拉起；门户的 `healthz` 语义见 `50cb843`（就绪 = 自己在服务） |
| 真实 dsh 端到端 + 单实例回归 | ✅ | `50cb843` 与本次提交；两个脚本都在本机真机跑通 |
| 旧 CLI 输出迁入 i18n | 🔶 **P1 待办** | 门户相关界面（登录页、门户页、管理台、`portal` 命令）已双语；`setup`/`service`/`model`/`plugin`/`uninstall` 等旧命令的输出与帮助仍是中文 |

### P1 待办（不在 P0 范围）

1. 旧 CLI 输出与帮助迁入 i18n（约 120 条用户可见串 + 帮助文本 + flag 描述）；
2. 管理台上的实例 CRUD / 启停 / 日志，以及 `portal instance start|stop|restart|logs`；
3. 按需实例的空闲回收（`idle_timeout_s` 字段已就位，尚未实现回收器）；
4. 在线会话列表与「踢下线」的页面/命令（`Sessions.DestroySubject` 已就位）。

已知与本改造无关的环境问题：Windows 上 `internal/config`（POSIX 路径）、
`internal/dshproc`（软链需特权、进程与端口语义）、`internal/dshsettings` 与
`internal/users`（POSIX 权限语义）、`cmd/dsh-gate` 的部分用例会失败——已在改动前的
提交 `228014a` 上逐条比对确认是既有问题，Linux/macOS 上不受影响。
