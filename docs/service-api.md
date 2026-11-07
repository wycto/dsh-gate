# dsh-gate 对外 API、受管服务与 MCP 适配 设计方案

> 状态：草案 v1（2026-11-06），待用户确认后开工。
> 需求（用户 2026-11-06）：dsh-gate 提供**对外接口**与**接口鉴权管理**；可通过接口添加/管理
> 服务、登记服务；服务支持**设置启动命令**与**启动 / 重启 / 停止**；再做一个**接口调用 skill**
> 以便 AI 直接调用；并讨论「这是不是 MCP」。

## 1. 先厘清：这不是「要么 API 要么 MCP」

| 概念 | 是什么 | 作用 |
|---|---|---|
| HTTP API | 一组 REST/JSON 端点 + 鉴权 | **能力本体**，任何语言/脚本/AI 都能调 |
| MCP | 「工具清单 + JSON-RPC」的标准协议 | 接口的一种**包装形态**，让支持 MCP 的客户端自动发现工具 |
| Skill | 一份 `SKILL.md` 说明书 | **不提供能力**，只教 agent 怎么调那套接口 |

**结论：做「HTTP API 本体」，再包一层「MCP server」，再写「skill」」。** API 是根，MCP 与
skill 是两种外壳——支持 MCP 的客户端自动发现工具，不支持的 agent 照着 skill 用 curl 调。
本方案据此分三层实现。

## 2. 目标与非目标

目标：
- 一套**外部 HTTP API**（`/api/v1/...`），用 **API Key** 鉴权，多个 key、按 scope 授权、可吊销；
- 通过 API **登记 / 查询 / 修改 / 删除服务**；
- 服务升级为可选**受管进程**：配启动命令，网关负责**启动 / 重启 / 停止**、日志落盘、状态查询；
- 提供 **MCP server**（stdio）与 **skill**，让 AI 直接调用。

非目标（本期不做）：OAuth/OIDC 身份源；跨机器 agent 组网；非 HTTP 的 TCP/UDP 全协议代理；
Webhook 事件回调；API 网关级多租户配额计费。

## 3. 现状（已核对代码）

- 现有三套鉴权：管理台用**登录会话**（`/_portal/api/admin/*`、页面）；本机 CLI 用
  **回环 + `<状态目录>/portal.token`**（`/_portal/api/local`）；挂载代理用会话 + grants。
  **没有**适合外部系统/AI 远程调用的「API Key」通道。
- 「服务」（`services` 表，结构版本 v4 引入）是**纯代理型**：只登记一个已经存在的上游
  `http(s)://host[:port][/base]`，网关不拉起、不守护，也没有启动命令；`svc.*` 仅
  list/add/remove/enable/disable/probe。
- 存储结构当前 `SchemaVersion = 6`（v5 用户类别开关、v6 门户全局总开关）。

## 4. 总体架构

    外部系统 / AI / MCP 客户端
        │  Authorization: Bearer dgk_…      （外部 API，默认关闭，开关打开才生效）
        │  https://<网关>/api/v1/…          （与浏览器同一 TLS 监听）
        v
    ┌──────────────────────── dsh-gate 管理端进程 ────────────────────────┐
    │ portal.Server                                                       │
    │  · /api/v1/*        ← 新增：API Key 鉴权 + scope + 限速 + 审计        │
    │  · /_portal/*       ← 现有：管理台会话 / 本机 token                   │
    │  · /i/ /s/          ← 现有：实例 / 服务挂载代理                       │
    │  服务监督器（新增，Go）← 受管服务的 start/stop/restart/status/logs    │
    │  SQLite：users/instances/services/api_keys/settings/audit           │
    └─────────────────────────────────────────────────────────────────────┘
        ↑
    dsh-gate mcp（stdio，独立小进程）──HTTP──┘  （MCP 客户端 ↔ 网关）

要点：MCP server 是**网关 HTTP API 的客户端**（不在网关进程内跑），只需 base-url + API key，
可跑在任意机器上（甚至 AI 所在机器），与网关解耦。

## 5. 对外 API 设计

### 5.1 鉴权与开关
- **默认关闭**：`settings.external_api_enabled = 0`。管理员在管理台开启后才生效（网关是暴露
  内网服务的入口，默认不额外开口）。
- 头：`Authorization: Bearer dgk_<随机>`。缺失/错误 → 401；scope 不足 → 403；未知路径 404。
- 路由：`/api/v1/` 前缀在 `Server.Handler()` 外层的 Gate 之前分流——**不走登录会话**，
  改走 API Key 校验（与现有 `/_portal/api/local` 的「外挂」方式同构）。
- 限速：每 key 默认 60 次/分（可配），超限 429。
- 审计：每次写操作与生命周期调用写 audit，actor = `api:<key 名>`，含来源 IP。

### 5.2 端点（v1）

| 方法 | 路径 | scope | 说明 |
|---|---|---|---|
| GET | /api/v1/services | services:read | 列出服务（含受管/代理、运行状态） |
| POST | /api/v1/services | services:write | 登记服务（可带启动命令→受管） |
| GET | /api/v1/services/{id} | services:read | 单个服务详情 |
| PATCH | /api/v1/services/{id} | services:write | 修改（名称/上游/启动命令等） |
| DELETE | /api/v1/services/{id} | services:write | 删除登记（受管服务先停进程） |
| POST | /api/v1/services/{id}/start | services:lifecycle | 启动（受管服务）；代理型返回不支持 |
| POST | /api/v1/services/{id}/stop | services:lifecycle | 停止 |
| POST | /api/v1/services/{id}/restart | services:lifecycle | 重启 |
| GET | /api/v1/services/{id}/status | services:read | 运行状态（pid/state/uptime/last_error） |
| GET | /api/v1/services/{id}/logs?tail=200 | services:read | 读最近日志 |
| GET | /api/v1/instances | instances:read | 列出 dsh 实例及状态 |
| POST | /api/v1/instances/{id}/start|stop|restart | instances:lifecycle | 实例启停重启 |
| GET | /api/v1/health | （无鉴权，仅本机可访问可选） | 健康探针 |

scopes：`services:read`、`services:write`、`services:lifecycle`、`instances:read`、
`instances:lifecycle`。写/生命周期 scope **不含**任何读权限时按最小授权处理（各自独立）。

### 5.3 响应与错误
- 成功：`{"ok":true,...}`；错误：`{"ok":false,"error":{"code":"...","message":"..."}}`，
  code ∈ unauthorized/forbidden/not_found/invalid_argument/conflict/rate_limited/unsupported/internal。
- `{id}` 用不透明短 id（与页面一致），也接受服务名（解析优先级 id > 名）。

## 6. 数据模型（迁移 v7）

```sql
CREATE TABLE api_keys (
  id           TEXT PRIMARY KEY,            -- 不透明短 id
  name         TEXT NOT NULL,               -- 展示名（审计里用）
  prefix       TEXT NOT NULL,               -- 密钥前 8 位，用于查找与展示（明文只显示一次）
  hash         TEXT NOT NULL,               -- sha256(完整密钥) 的十六进制
  scopes       TEXT NOT NULL DEFAULT '',    -- 逗号分隔
  ip_allowlist TEXT NOT NULL DEFAULT '',    -- 逗号分隔 CIDR/IP，空=不限
  disabled     INTEGER NOT NULL DEFAULT 0,
  created_at   TEXT NOT NULL, created_by TEXT NOT NULL DEFAULT '',
  last_used_at TEXT NOT NULL DEFAULT ''
);

ALTER TABLE services ADD COLUMN managed       INTEGER NOT NULL DEFAULT 0; -- 1=受管进程
ALTER TABLE services ADD COLUMN start_cmd     TEXT    NOT NULL DEFAULT '';
ALTER TABLE services ADD COLUMN stop_cmd      TEXT    NOT NULL DEFAULT ''; -- 空=按进程组终止
ALTER TABLE services ADD COLUMN work_dir      TEXT    NOT NULL DEFAULT '';
ALTER TABLE services ADD COLUMN env           TEXT    NOT NULL DEFAULT ''; -- 每行 KEY=VALUE
ALTER TABLE services ADD COLUMN auto_start    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE services ADD COLUMN health_path   TEXT    NOT NULL DEFAULT ''; -- 就绪探测路径（空=探测端口）
```

- 密钥不落明文：库里只有 `hash` 与 `prefix`；创建时明文只返回一次。
- 受管服务的**运行态不落库**（pid/state/uptime/last_error 存内存 + 日志文件），避免写放大；
  与实例监督器（dshproc）同一取舍。
- 审计沿用现有 `audit` 表（已有 action/detail/ip/ok）。

## 7. 受管服务：进程生命周期

- 新增 `internal/svclife`（通用进程监督器，参考 `dshproc` 但更简单、与 dsh 无关）：
  - `Start/Stop/Restart/Status/Logs`；子进程 `Setpgid` 独立进程组，停止时按进程组终止；
  - stdout/stderr 追加到 `<状态目录>/services/logs/<id>.log`（0600，超限轮转）；
  - 就绪判定：`health_path` 非空则 GET 该路径 2xx/3xx；否则探测 `upstream` 的 host:port 监听；
  - 重启策略：`auto_start` 时随网关启动拉起；进程意外退出按退避重启（可配上限）。
- **受管服务的 upstream 指向本机端口**（如 `http://127.0.0.1:8080`），代理照旧走 `/s/<id>/`。
  纯代理服务 `managed=0`，行为完全不变。
- 生命周期接口仅对 `managed=1` 生效；对纯代理服务调用 start/stop/restart → `unsupported`。
- 新增管理台「服务」分页的受管分支：登记表单加「启动命令 / 工作目录 / 环境变量 / 自动启动 /
  健康路径」，卡片加「启动 / 停止 / 重启 / 日志」按钮；纯代理服务不显示这些。
- 新增本机 CLI：`dsh-gate svc start|stop|restart|logs <名或id>`（走本机控制 API，仅本机）。

## 8. MCP 适配（P2）

- 子命令 `dsh-gate mcp`：以 **stdio** 提供 MCP server（JSON-RPC），工具清单：
  `service_list / service_get / service_register / service_update / service_remove /
  service_start / service_stop / service_restart / service_status / service_logs /
  instance_list / instance_start / instance_stop / instance_restart`。
- 它本身是 HTTP API 的**客户端**：读 `--base-url`（或 `DSH_GATE_API_URL`）、
  `--api-key`（或 `DSH_GATE_API_KEY`）；每个 tool 把参数映射成一个 API 调用并回传结构化结果。
- 好处：MCP 客户端（Claude Desktop、本 harness 若支持等）自动发现工具；不把网关进程塞进 MCP
  传输层，解耦、可远程。HTTP/SSE 形态的 MCP 端点留作后续。

## 9. Skill（P2，放父仓库 `.agents/skills/dsh-gate-api/`）

- `SKILL.md` 写清：base URL、`Authorization: Bearer`、各端点与参数、curl 示例、错误码、
  取得 API Key 的方式、scope 说明。目标是「不会 MCP 的 agent 照着就能调」。
- description 写明「什么时候用」：需要以程序方式登记/管理 dsh-gate 服务或实例时。

## 10. 安全清单（重点）

- **外部 API 默认关闭**，管理员在管理台显式开启；开启后必须至少有一个 key 才能用。
- **API Key**：高熵随机、库里只存哈希、明文只显示一次、按 scope 最小授权、可吊销、可 IP 白名单。
- **受管服务的 `start_cmd` 等于是远程命令执行**：写/生命周期 scope 必须显式授予，绝不能给匿名
  或低权 key；网关只应暴露给可信网络；文档显著标注。
- 限速 + 审计（actor=`api:<名>`、来源 IP）+ 失败尝试也记审计。
- `start_cmd`/`env`/`work_dir` 校验：拒绝换行注入到日志之外的地方；`work_dir` 必须是绝对路径且
  可写；env 只接受 `KEY=VALUE` 行。
- 复用现有 TLS（同监听）；不额外开明文端口。必要时可只绑内网地址。

## 11. 分期

- **P0**：迁移 v7（api_keys + services 生命周期列 + external_api_enabled 开关）；API Key 管理与
  管理台「API」分页（创建/吊销/看 scope）；`/api/v1` 骨架 + 鉴权/限速/审计；服务 CRUD 与读接口。
- **P1**：受管服务监督器 + start/stop/restart/logs + 自动启动；管理台受管表单与按钮；本机
  `svc start|stop|restart|logs`；API 生命周期端点。
- **P2**：`dsh-gate mcp`（stdio）+ `.agents/skills/dsh-gate-api/SKILL.md` + `docs/openapi.yaml`。

## 12. 测试与验收

- 单测：v6→v7 迁移兼容（老服务 managed=0 行为不变）；API Key 校验/scope/限速/IP 白名单；受管
  监督器（假命令起停、崩溃恢复、日志、端口就绪）。
- e2e：起真实网关，用 key 调 `POST /api/v1/services`（受管：一个 `python3 -m http.server` 之类）
  → start → 经 `/s/<id>/` 访问到 → restart → stop；错误 key 401、越权 403、纯代理服务 start 返回
  unsupported、外部 API 关闭时 404。
- MCP：用 MCP 客户端（或脚本）握手、列工具、调用 service_list / service_start 验证连通。
- 脱敏：示例 key 用带连字符的假值，不用真实格式；不写真实内网地址。

## 13. 决策记录（默认值，待确认）

| # | 问题 | 默认结论 |
|---|---|---|
| 1 | MCP 层要不要 | **要**：API 本体 + MCP 适配 + skill 三件套 |
| 2 | 外部 API 监听 | 复用网关同一 HTTPS 监听，`/api/v1/` 前缀，**默认关闭** |
| 3 | 受管服务是否纳入本期 | **纳入**（P1）：start_cmd + 启停重启 + 日志 |
| 4 | API Key 用什么哈希 | sha256（密钥高熵，无需慢哈希），比较用常数时间 |
| 5 | MCP 形态 | 先做 **stdio 客户端**（`dsh-gate mcp`），HTTP/SSE 留后续 |
| 6 | 外部 API 谁能开 | 管理员在管理台「API」分页开启并建 key；首个 key 也可本机 CLI 建 |

## 14. 新增/改动文件概览（实现参考）

- 存储：`internal/portal/store.go`（v7）、新增 `internal/portal/apikeys.go`、`services.go` 扩展列。
- 对外 API：新增 `internal/portal/api.go`（路由 + 鉴权中间件 + 限速 + 审计），`server.go` 分流。
- 受管监督：新增 `internal/svclife/`（+ 适配到 `ServerOptions.ServiceLifespan` 接口）。
- 管理台：`admin.go`/`admin.html`/`adminapi.go` 加「API」分页与服务受管字段。
- CLI：`cmd/dsh-gate/svc.go` 扩展 start/stop/restart/logs；新增 `cmd/dsh-gate/apikey.go`、`cmd/dsh-gate/mcp.go`。
- 父仓库：新增 `.agents/skills/dsh-gate-api/SKILL.md`。


## 15. 服务发现（2026-11-07 增补）

目标：像 dsh-services 的「服务台」一样，一键发现可登记的服务并批量登记。分两种来源：

- **本机发现**：网关所在机器上正在监听的 TCP 服务。Linux 读 /proc/net/tcp{,6} + /proc/<pid>/fd 反查 pid、读 /proc/<pid>/cmdline 与 cwd；macOS 用 lsof -nP -iTCP -sTCP:LISTEN + ps；其它平台返回「本机发现不可用」。候选带 pid/命令/工作目录，可与 dsh-services 对齐。
- **局域网网段扫描**：管理员填 CIDR（只允许私有网段 10/8、172.16/12、192.168/16）+ 端口列表/区间；对每个 host:port 做短超时 TCP 连接探测，命中后再 HTTP GET / 判断是否 HTTP 并取标题。无进程信息，只能按 host:port 识别。并发上限 64、单连接超时 500ms、端口数上限 1024，避免把网关当扫描器滥用。

候选登记默认是**纯代理服务**（managed=0，因为多数是别人的服务）；本机候选可选填启动命令升级为受管。已登记（同 upstream）的候选标记 alreadyRegistered 并跳过。

接口（scope）：
- POST /api/v1/discover/local（services:read）→ {candidates:[...]}
- POST /api/v1/discover/scan {cidr, ports:[...]}（services:read）→ {candidates:[...]}
- POST /api/v1/discover/register {candidates:[...]}（services:write）→ 批量 CreateService，回 {created:[...], skipped:[...]}

安全：发现/登记仅管理员（管理台）或带 scope 的 API Key；只允许私有网段；限速与审计；这是「列出网络上的目标」的能力，不开放给匿名。管理台「服务」分页加「发现服务」面板（扫描 + 候选勾选 + 批量登记）；本机 CLI：dsh-gate svc discover [--local] [--cidr ... --ports ...] [--add]。
