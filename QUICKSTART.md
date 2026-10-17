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

# 2. 初始化：逐项询问，回车接受默认值
#    别加 sudo：以 root 写出的 settings.yaml / .credentials.yaml 归 root，
#    你自己的 dsh（包括桌面端）就读不了、改不动了；工具会直接拦下这种调用
./dsh-gate setup

# 3. 浏览器打开它给你的「局域网」地址，用刚设的账号登录
```

`setup` 会顺手把命令装进 `PATH`，之后直接敲 `dsh-gate` 即可。它也会问你要不要装开机自启。

## 只有三件事需要你决定

| 问题 | 怎么答 |
|---|---|
| **对外端口** | 浏览器访问用的端口。默认 3082，被占用会当场让你换 |
| **会话默认工作目录** | **agent 默认能读写哪个目录**。填你真正想让它干活的地方，别填家目录或 `/` |
| **DeepSeek API Key** | 可以直接回车跳过，之后补：`dsh-gate model key set DEEPSEEK_API_KEY --stdin`。跳过就没有可用模型 |

不装任何插件也能配模型（官方 DeepSeek 是 dsh 自带的路由）：

```sh
printf 'sk-你的密钥' | dsh-gate model key set DEEPSEEK_API_KEY --stdin
dsh-gate model default deepseek-official deepseek-chat
dsh-gate model list
```

## 第一次启动要等几分钟

第一次会自己下载 Node.js 运行时与 dsh（本机没装 Node 也没关系），全程 3–5 分钟，
之后重启是秒级。看着日志等：

```sh
journalctl -u dsh-gate -f                              # 装成服务了
tail -f ~/.dsh-gate/run/logs/<实例名>.log              # 后台运行
```

网关**立刻**就在监听，登录页马上能打开；慢的只是 dsh 的就绪过程。

## 出问题先看这三条

```sh
dsh-gate status     # 网关跑着没、端口是多少
dsh-gate check      # 体检：端口被谁占了、该访问哪个地址
journalctl -u dsh-gate -n 50    # dsh 为什么没起来
```

| 现象 | 怎么办 |
|---|---|
| 本机能开、别的机器打不开 | 防火墙：`ufw allow 3082/tcp`；云主机还要放行安全组 |
| 页面一直显示「DSH 正在启动」 | 正常，等它；超过 10 分钟看 journal |
| 页面报 `DSH 要求重新登录` | dsh 不是网关拉起的，拿不到它的令牌：`pkill -f "dsh web"` 再 `dsh-gate restart <实例名>` |
| 页面报 `fork/exec …/npx: no such file or directory` | `rm -rf <状态目录>/node` 再 `dsh-gate restart <实例名>` |
| 报 `settings are unavailable in this browser` | 模型页依赖 dsh-dock；用上面的 `model` 命令绕开 |

更多症状（找不到 npx、`spawn pnpm ENOENT`、`authentication required`、工作目录没生效）
见 [README.md 的常见问题](README.md#常见问题)。

## 常用命令

```sh
dsh-gate status             # 现在跑着哪些实例
dsh-gate restart <实例名>    # 按原参数重启
dsh-gate stop <实例名>       # 停掉（写 all 才是全部）
dsh-gate model list         # 看模型与密钥配置
dsh-gate plugin list        # 看 dsh 配置档里的插件
dsh-gate uninstall <实例名> --service          # 停掉并取消自启，数据留着
dsh-gate uninstall <实例名> --service --purge  # 确认不要了再连数据一起删
```

## 想要「一个入口 + 每人一个 dsh」？

那是**管理端**形态：账号与实例都提到管理端一级，成员登录后只看到属于自己的实例。

```sh
dsh-gate manage user add admin --role admin --password-stdin
dsh-gate manage instance add alice-main --dsh-home /srv/dsh/alice --owner alice
dsh-gate manage grant add alice alice-main
dsh-gate manage run --port 3082
```

浏览器打开管理端地址：普通用户看到「我的实例」，点进去就是那个 dsh；管理员多一个
`/admin` 管理台（用户、实例、授权、审计）。

**管理台里能直接装 dsh，不用你事先装好**：打开 `/admin` 就做一次运行环境检测
（node / npm / npx / pnpm / git / dsh 各自的状态与路径），然后按实例挑安装方式。
顺序是**先创建实例记录**（马上拿到 id 与端口），**再在后台安装** —— 安装要十几分钟，
不能把页面挂住；刷新页面看进度。

| 安装方式 | 适合 | 装出来是什么 |
|---|---|---|
| **npx**（默认） | 只想尽快用上 | `npx --yes @deepseek-ai/dsh` 现拉现跑 |
| **git** | 要读源码 / 改代码 | clone 官方仓库 + `pnpm install` + build |
| **pnpm** | 想离线复用、固定版本 | 装到实例自己的目录，不是全局 |

机器上没有 Node 也不用先去装：git 与 pnpm 方式会自己把 Node 运行时与 pnpm 下到状态
目录（不动系统）。网络慢就换源：

```sh
dsh-gate manage instance install alice-main --install-mode pnpm \
    --npm-registry https://registry.npmmirror.com
```

命令行与页面是同一套数据与代码路径。详见 [README.md](README.md) 的「管理端」一节与
[docs/portal.md](docs/portal.md)。

## 东西都在哪

| 内容 | 位置（默认） |
|---|---|
| 实例登记、账号表、日志 | `~/.dsh-gate/`（`--state-dir` 可改） |
| dsh 数据（会话、插件、模型设置） | `~/.dsh/`（`--dsh-home` 可改） |
| systemd 单元 | `/etc/systemd/system/dsh-gate.service` |
| launchd 配置（macOS） | `~/Library/LaunchAgents/com.wycto.dsh-gate.plist` |

多实例互不干扰：换 `--name`、`--port`、`--dsh-port`、`--dsh-home`、`--state-dir` 即可，
见 [README.md 的多实例](README.md#多实例)。
