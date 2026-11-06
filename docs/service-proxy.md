# dsh-gate 内网服务代理（Service Proxy）设计与实现说明

> 状态：**已定稿**（用户 2026-11-05 拍板：集成进 dsh-gate，按本方案实施）。
> 需求：在 dsh-gate 现有「代理 dsh 实例」之外，把任意内网 IP:端口 / 内网域名服务
> 登记进来、代理出去；像 dsh-services 那样发现/注册服务；其他机器通过一个 IP 端口
> 统一访问与管理内网服务。
>
> **用户补充决议（2026-11-05）：服务与 dsh 实例的页面完全分离**——管理端用独立
> 「服务」分页（不与实例页混排），用户侧「我的服务」是独立页面 `/services`，
> 首页 `/` 仍是「我的实例」，两者只以导航链接互达，列表不混在一起。

## 1. 决策：集成进 dsh-gate，不另起项目

### 1.1 理由

1. 门户形态（docs/portal.md）已是「唯一对外入口 + N 个上游子路径挂载」：
   登录/会话/限速（internal/auth）、grants 授权 + 审计、管理台 + JSON API + CLI 对等、
   TLS（internal/tlscert）、SQLite（modernc.org/sqlite，唯一第三方依赖）、
   internal/mountproxy（剥前缀、Cookie Path 改写、Location 改写、WS/SSE 不缓冲）。
   上游从「门户拉起的 dsh 进程」泛化为「任意登记的 http(s) 目标」只需新增一类对象，
   代理与门禁机制原样复用。
2. 另起项目要重造上述全部，且与「一个入口访问所有服务」的目标冲突（两个网关抢入口）。
3. dsh-services 是 DSH 插件，生命周期绑定 dsh 宿主；入口网关必须在 dsh 未启动时可用。
   两者互补：服务台管本机开发进程（启停/日志/测试账号），网关管对外暴露与门禁。
4. 另起项目的触发条件（现在都不成立）：每主机 agent 组网、非 HTTP 全协议代理、多节点 HA。

### 1.2 产品口径变化

- dsh-gate 定位从「给 DSH 加门禁的网关」扩展为「内网统一入口：dsh 实例 + 内网服务」。
- README 与门户首页文案同步；不改变现有实例行为（/i/<id>/ 一律不动）。

## 2. 总体架构

    浏览器（任意机器）
       |  https://<网关>:12020/           门户首页：我的实例 + 我的服务
       |  https://<网关>:12020/i/<id>/... dsh 实例（现状）
       |  https://<网关>:12020/s/<id>/... 内网服务（新增）
       v
    dsh-gate 门户（登录 / grants / 审计 / TLS）
       +---> http://172.18.x.x:8080        登记的 IP:端口
       +---> https://wiki.corp.example     内网域名（Host/SNI 策略可配）
       +---> 127.0.0.1:32101...            dsh 实例（现状）

- 服务是**纯上游**：门户不拉起、不守护其进程（与实例的本质区别），只做代理与探活。
- 只代理已登记目标，绝不作开放代理（网关是内网跳板，SSRF 面必须收死）。

## 3. 路由模式（关键取舍）

| 模式 | 形态 | 适用 | 代价 |
|---|---|---|---|
| path（默认） | /s/<id>/ 子路径，剥前缀 + Cookie/Location 改写 | 相对路径友好的应用、API 服务 | 硬编码绝对路径 /xxx 的应用会坏；localStorage 按 origin 共享（同门户已知限制） |
| port（兜底） | 网关额外监听一个端口，整口转发到上游，零改写 | 子路径下坏掉的应用（管理台、老系统） | 每个服务占一个对外端口；防火墙要放行 |
| subdomain（P2） | <id>.gate.example 泛解析 + 泛证书 | 有内网 DNS 控制权时最干净 | 依赖 DNS/TLS 配套，本期不做 |

- 每个服务一个 route_mode 字段；默认 path，管理台可切 port（登记时自动探一个空闲端口）。
- **不做 HTML/JS 内容重写**：脆弱、无底洞；坏的应用直接引导切 port 模式。
- path 模式复用门户子路径硬要求清单（剥前缀、Set-Cookie Path 改写、根相对 Location
  改写、HTTPS 链路补 Secure、WS/SSE 不缓冲、无斜杠 303 补斜杠）；令牌交接是 dsh 专属，
  服务不需要。

## 4. 数据模型（SQLite，迁移 v4，加列式）

    services(
      id TEXT PRIMARY KEY,                 -- 不透明短 id（>=8 位随机，同实例口径）
      name TEXT UNIQUE NOT NULL,           -- 展示名
      upstream TEXT NOT NULL,              -- http(s)://host:port 或 http(s)://域名[:port][/base]
      host_header TEXT NOT NULL DEFAULT 'upstream',  -- upstream=原样域名 | gateway=门户 authority
      insecure_tls INTEGER NOT NULL DEFAULT 0,       -- 自签证书豁免（默认校验）
      route_mode TEXT NOT NULL DEFAULT 'path',       -- path | port
      listen_port INTEGER NOT NULL DEFAULT 0,        -- port 模式的对外端口（0=未分配）
      note TEXT NOT NULL DEFAULT '',                 -- 备注
      test_account TEXT NOT NULL DEFAULT '',         -- 测试账号（明文，0600 库，同服务台口径）
      health_interval_s INTEGER NOT NULL DEFAULT 0,  -- 0=只按需探活
      enabled INTEGER NOT NULL DEFAULT 1,
      source TEXT NOT NULL DEFAULT 'manual',         -- manual | discover | import
      created_at TEXT NOT NULL, updated_at TEXT NOT NULL
    );
    service_grants(
      user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
      service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
      created_at TEXT NOT NULL, created_by TEXT NOT NULL DEFAULT '',
      PRIMARY KEY(user_id, service_id)
    );

- 不动 instances 表语义；授权判定沿用「先会话后 grants、未命中 404 同形 + 审计」。
- upstream 带 /base 时代理拼接 base 前缀（上游本身挂子路径的场景）。
- 健康状态不落库（运行态内存缓存 + 按需探测），避免写放大。

## 5. HTTP 面与 CLI

| 路径 | 说明 |
|---|---|
| /s/<id>/... | 服务反向代理：需登录且命中 service_grants（或 admin）；port 模式同样先过门户会话闸 |
| /_portal/api/admin/action | 新增动作：service.create / update / delete / toggle / probe / discover / import / export |
| /_portal/api/admin/state | 增 services 与授权矩阵段 |
| /_portal/api/me | 增「可见服务」列表 |
| /admin?tab=services | 管理台**独立「服务」分页**：卡片（名称/上游/状态/访问地址/备注）+ 登记/编辑表单 + 服务授权矩阵；与实例分页完全分开 |
| /services | 用户侧**独立「我的服务」列表页**（与首页「我的实例」分开，导航互达） |

- 管理 API 两道闸不变：admin + JSON + 同源校验；页面表单走双提交 CSRF 令牌。
- CLI：dsh-gate svc add|list|rm|enable|disable|probe|discover|import|export
  （**service 子命令已被「系统服务安装」占用，必须避开**；走本机控制 API + portal.token）。
- 发现动作仅 admin；探测限速、超时短（TCP connect 1-2s）、并发上限，审计留痕。

## 6. 服务发现（诚实边界）

1. **本机发现**（网关所在机器）：Go 实现 lsof 等价扫描（Linux 可读 /proc/net/tcp + fd 反查，
   macOS 调 lsof），列 pid/命令行/端口；沿用服务台过滤思路：默认只列「像开发服务的」，
   系统端口要显式勾选；勾选批量登记。
2. **远端内网机器**：没有 agent 就拿不到远端进程表——不做假承诺。提供「主机 + 端口段」
   限速 TCP 探测，探通的列出来供人工确认登记（只知端口开着，不知是什么服务）。
3. **dsh-services 桥接**（P1）：约定 JSON 导出格式（名称/端口/地址/备注/测试账号），
   网关 svc import 读入；后续服务台可加「推送到网关」按钮（调网关本机 API），无硬依赖。
4. mDNS/SSDP 广播发现、远端 agent：P2 再议。

## 7. 安全清单

- 注册/发现/导入仅 admin；访问按 grants；未命中 404 同形 + 审计（沿用实例口径）。
- 上游白名单即登记表；禁止任何「动态指定上游」的入口（query/头都不行）。
- 内网域名上游：默认校验 TLS；insecure_tls 按服务显式豁免并在卡片上标红提示。
- Host 头策略显式可选；转发 X-Forwarded-For/Proto 沿用现状。
- 测试账号明文落库：库文件 0600、目录 0700（现状）；文档提示不要存生产口令。
- port 模式对外端口同样先过门户登录闸（不是裸转发）；防火墙提示写进文档。
- 探测动作限速 + 并发上限 + 审计，防止把网关当扫描器滥用。

## 8. 分期

- **P0（可用）**：services/service_grants 表 + 迁移 v4；手动登记（IP:端口/域名）；
  /s/<id>/ path 代理（复用 mountproxy）；登录/grants/审计；管理台服务页 + action API；
  svc CLI 基础命令；按需探活；WS/SSE；文档（含子路径限制说明）。
- **P1（好用）**：本机发现 + 批量登记；主机端口段探测；周期健康检查；备注/测试账号卡片；
  port 兜底模式；dsh-services JSON 导入导出。
- **P2（按需）**：子域名路由；原生 TCP 转发（数据库/SSH，鉴权方式另设计）；
  远端 agent 发现；mDNS。

## 9. 测试与验收（沿用门户口径）

- 单测：迁移 v3→v4 旧库兼容；路由模式分派；Cookie/Location 改写对服务上游的回归。
- e2e（sh go/tests/ 风格）：起两个假上游（一个相对路径友好、一个硬编码绝对路径），
  验证 path 模式前者全通、后者按文档引导切 port 模式后全通；grants 未命中 404；
  WS 回声上游升级成功；自签 TLS 上游默认拒绝、豁免后通过。
- 真机自检：隔离状态目录 + 备用端口，登记本机一个真实服务，从另一台机器经网关访问。

## 10. 决议记录（2026-11-05 用户拍板）

1. 集成进 dsh-gate，不另起项目；按本方案 P0 → P1 → P2 分期。
2. **页面分离**：管理端独立「服务」分页；用户侧独立 `/services` 页；不与 dsh 实例混排。
3. 其余按方案默认口径执行：子命令名 `svc`；port 兜底模式在 P1；可见性默认仅 admin、
   逐用户授权（同实例口径）；测试账号字段 P1 再上界面（表结构本期预留）。

## 11. P0 实施范围（本期）

- 迁移 v4：`services` + `service_grants` 两张表（route_mode / listen_port /
  health_interval_s / test_account 列预留，P0 只走 path 模式）。
- `/s/<id>/` 子路径代理：复用 mountproxy 的 Cookie/Location 改写与 WS/SSE 穿透；
  上游用标准库 httputil.ReverseProxy，Host 头策略与自签 TLS 豁免按服务配置。
- 授权与审计：ServiceAccessible 唯一授权入口，未命中 404 同形 + 审计（同实例口径）。
- 管理台「服务」分页 + 用户 `/services` 页 + JSON API（state/live/me/action）。
- 本机控制 API + `dsh-gate svc` CLI：list / add / rm / enable / disable / probe。
- 按需探活（probe 动作与管理台状态徽标）；周期健康检查留 P1。
- 发现（本机 lsof 式扫描、端口段探测、服务台导入）全部留 P1。
