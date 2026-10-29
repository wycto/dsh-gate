# dsh-gate 快速上手

让局域网里的设备通过一个登录页安全地使用本机 dsh。一页纸，够跑到能用为止；
完整说明见 [README.md](README.md)。

压缩包从[发行版](https://github.com/wycto/dsh-gate/releases)里下载（打不开 GitHub
就用 [Gitee 发行版](https://gitee.com/wycto/dsh-gate/releases)），按自己的平台挑一个。

## 三步

```sh
# 1. 解压（Windows 用 .zip，解开就是 dsh-gate.exe）
tar -xzf dsh-gate_<版本>_linux_amd64.tar.gz
cd dsh-gate_<版本>_linux_amd64

# 2. 安装管理端：逐项询问，回车接受默认值；--start 装完直接后台跑起来
#    别加 sudo：以 root 写出的数据库与令牌归 root，你自己的命令就读不了、改不动了
./dsh-gate install --start

# 3. 浏览器打开它给你的 http://<本机>:12020/admin
#    「实例」页 → 创建实例（名字必填，其余默认）→ 点「安装」→ 装完点「启动」
```

`install` 会顺手把命令拷到固定位置（Linux / macOS 默认 `/usr/local/bin`，Windows 默认
`%LOCALAPPDATA%\dsh-gate\bin`，`--bin-dir` 可改），之后直接敲 `dsh-gate` 即可。它也会问你要不要装开机自启（或直接用 `--service`）；Linux 上写 systemd
单元需要 root，所以先以自己身份 install，再用
`sudo dsh-gate service install --state-dir "$HOME/.dsh-gate" --user "$USER"` 交给服务管理器。

管理台默认第一页就是「实例」，底部依次是 **实例 → 用户 → 实例授权 → 运行环境 → 审计**。
建完实例、装好 dsh，到「实例授权」页把实例勾给你自己和同事，对方登录后就能在
「我的实例」里看到。

## 只有三件事需要你决定

| 问题 | 怎么答 |
|---|---|
| **对外端口** | 浏览器访问用的端口。默认 12020，被占用会当场让你换 |
| **实例目录（每个实例一个）** | dsh 的数据根（`DSH_HOME`），会话、插件、凭据都放这里。默认 `~/.dsh`，多实例就填不同的目录 |
| **DeepSeek API Key** | 每个实例单独配，也可以先跳过，之后补：`dsh-gate model key set DEEPSEEK_API_KEY --stdin --instance <实例名>`。跳过就没有可用模型 |

不装任何插件也能配模型（官方 DeepSeek 是 dsh 自带的路由）：

```sh
printf 'sk-你的密钥' | dsh-gate model key set DEEPSEEK_API_KEY --stdin --instance <实例名>
dsh-gate model default deepseek-official deepseek-chat --instance <实例名>
dsh-gate model list --instance <实例名>
```

实例的「工作目录」留空时是 `<实例目录>/workspace`，改完要点「重启」才生效——它决定
agent 默认能读写哪个目录，别让它落在 `/` 或整个家目录上。

## 第一次安装要等几分钟

实例第一次安装时，管理端会自己下载 Node.js 运行时与 dsh（本机没装 Node 也没关系），
再启动 dsh 初始化 profile；全程 3–5 分钟，之后重启是秒级。管理端与登录页**立刻**就能
打开，慢的只是实例里的 dsh；在「实例」页刷新能看到安装阶段与进度。看着日志等：

```sh
journalctl -u dsh-gate -f                       # 装成服务了
tail -f ~/.dsh-gate/run/logs/portal.log          # 后台运行
```

## 出问题先看这三条

```sh
dsh-gate status                    # 管理端跑着没、端口是多少、实例什么状态
dsh-gate dsh list                  # 实例清单与运行状态
dsh-gate dsh restart <实例名>       # 按最新配置重启某个实例
```

`/admin` 的「运行环境」页会告诉你 node / npm / npx / pnpm / git / dsh 找不找得到，
缺 pnpm 可以直接点一下安装。

| 现象 | 怎么办 |
|---|---|
| 本机能开、别的机器打不开 | 防火墙：`ufw allow 12020/tcp`；云主机还要放行安全组 |
| 实例一直显示「DSH 正在启动」 | 正常，等它；超过 10 分钟看管理端日志 |
| 页面报 `DSH 要求重新登录` | dsh 不是管理端拉起的，拿不到它的令牌：`pkill -f "dsh web"` 再 `dsh-gate dsh restart <实例名>` |
| 页面报 `fork/exec …/npx: no such file or directory` | `rm -rf <状态目录>/node` 再 `dsh-gate dsh restart <实例名>` |
| 报 `spawn pnpm ENOENT` | 到 `/admin` 的「运行环境」页点「安装」补 pnpm，或 `dsh-gate plugin add …` |
| 报 `settings are unavailable in this browser` | 模型页依赖 dsh-dock；用上面的 `model` 命令绕开 |

更多症状（找不到 npx、git 安装超时、`authentication required`、工作目录没生效）
见 [README.md 的常见问题](README.md#常见问题)。

## 常用命令

```sh
dsh-gate status                   # 管理端与实例状态
dsh-gate start                    # 按上次配置启动整个管理端（装了服务时转给服务管理器）
dsh-gate stop                     # 停止整个管理端（连同它监督的实例）
dsh-gate restart                  # 重启整个管理端
dsh-gate service install          # 开机自启
dsh-gate dsh list                 # 实例清单
dsh-gate dsh start|stop <实例名>   # 启停单个实例
dsh-gate dsh install <实例名>      # 安装 / 重装 dsh
dsh-gate dsh rm <实例名>           # 删除实例（默认保留数据目录）
dsh-gate model list --instance <实例名>
dsh-gate plugin list --instance <实例名>
```

不用了怎么卸：

```sh
dsh-gate dsh rm <实例名> --purge --purge-install   # 确认实例数据与安装目录都不要了
dsh-gate uninstall                                  # 交互式：问是否删实例数据，然后停实例、停管理端、卸服务
dsh-gate uninstall --yes --remove-data --purge      # 非交互：连实例数据与账号库一起删
```

`uninstall` 默认不删实例的数据目录，结束时把它们列出来；实例的 dsh 安装目录从不删除，
`dsh-gate` 可执行文件也不删。

## 命令行里也能干完

页面上的每件事都有等价命令（读写同一份管理端数据库）：

```sh
dsh-gate manage user add admin --role admin --password-stdin
dsh-gate dsh add alice-main --dsh-home /srv/dsh/alice --start-mode auto
dsh-gate dsh install alice-main --install-mode npx
dsh-gate manage grant add alice alice-main
dsh-gate dsh start alice-main
```

管理端**不需要你事先装好 dsh**：先建记录（立刻拿到 id 与端口），再装 dsh，最后启动。
四种安装方式按你的机器条件挑：

| 安装方式 | 适合 | 装出来是什么 |
|---|---|---|
| **npx**（默认） | 只想尽快用上 | `npx --yes @deepseek-ai/dsh` 现拉现跑 |
| **git** | 要读源码 / 改代码 | clone 官方仓库 + `pnpm install` + build |
| **local** | 克隆和构建你自己来 | 什么都不装，只把 `pnpm --dir '<源码目录>' run dsh` 记成启动命令 |
| **pnpm** | 想离线复用、固定版本 | 装到实例自己的目录，不是全局 |

`local` 就是「你在别的终端里 clone、`pnpm install`、`pnpm run build` 成功过，网关只负责
起进程」：安装目录填那个仓库的根目录即可，网关只核对目录与依赖，不联网、不构建。

机器上没有 Node 也不用先去装：git、local 与 pnpm 方式会自己把 Node 运行时与 pnpm 下到状态
目录（不动系统）。网络慢就换源：

```sh
dsh-gate dsh install alice-main --install-mode pnpm \
    --npm-registry https://registry.npmmirror.com
```

想跑比 `latest` 更新的发布（alpha / rc 通道），钉个版本：填 npm 的**版本号**或 **dist-tag**
（`npm view @deepseek-ai/dsh dist-tags` 看各通道指向哪个版本）。页面上就是创建/编辑实例时
的「dsh 版本」那一栏，留空跟随 latest。

```sh
dsh-gate dsh install alice-main --dsh-version 0.2.2-alpha.2
```

钉住之后**不再自动回退**：那一个版本装不上就直接报错，不会悄悄换成别的。源码方式
（`git` / `local`）不认这一栏：git 用 `--git-ref` 认分支与标签，local 认你填的那个目录。

详见 [README.md](README.md) 的「管理端」一节与 [docs/portal.md](docs/portal.md)。

## 东西都在哪

| 内容 | 位置（默认） |
|---|---|
| 管理端数据库（账号、实例、授权、审计） | `~/.dsh-gate/portal.db` |
| 本机控制令牌（`dsh` 子命令用） | `~/.dsh-gate/portal.token` |
| 上次启动参数（`restart` 用） | `~/.dsh-gate/portal.json` |
| 运行登记与日志 | `~/.dsh-gate/run/`（日志 `run/logs/portal.log`） |
| 自备运行时 | `~/.dsh-gate/node`、`~/.dsh-gate/pnpm` |
| 实例数据（会话、插件、凭据） | 各实例的 `DSH_HOME`，默认 `~/.dsh` |
| 实例工作目录 | `<DSH_HOME>/workspace`（可单独指定） |
| 实例安装目录（git / pnpm） | `<DSH_HOME>/install`（可单独指定；local 方式指向你的源码目录） |
| systemd 单元 | `/etc/systemd/system/dsh-gate.service` |
| launchd 配置（macOS） | `~/Library/LaunchAgents/com.wycto.dsh-gate.plist` |

`--state-dir` 可以整体改管理端状态目录；实例之间互不干扰，换 `--dsh-home` 就是另一套
dsh，见 [README.md 的多实例](README.md#多实例)。
