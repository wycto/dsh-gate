# dsh-gate

**A LAN / remote access gateway for [DeepSeek Harness](https://deepseek-harness.github.io/deepseek-harness/guide/quickstart).**

A single-binary Go program that uses only the Go standard library (the management console's SQLite is the one exception — see [go.mod](go/go.mod)). It serves a login page on a LAN-reachable port and, once you are authenticated, reverse-proxies requests to the local dsh. If dsh is not running yet, it starts it for you.

![dsh-gate login page (dark)](docs/images/login.png)

<sub>Phone width and light mode are shown at the end of the Quick start section.</sub>

中文版：[README.md](README.md)

---

## Table of Contents

- [What problem it solves](#what-problem-it-solves)
- [How it works](#how-it-works)
- [What it actually does](#what-it-actually-does)
- [Getting started in 30 seconds](#getting-started-in-30-seconds)
- [Tutorial](#tutorial)
- [Installation](#installation)
- [Command reference](#command-reference)
  - [dsh-gate run](#dsh-gate-run)
  - [dsh-gate status](#dsh-gate-status)
  - [dsh-gate stop](#dsh-gate-stop)
  - [dsh-gate restart](#dsh-gate-restart)
  - [dsh-gate user](#dsh-gate-user)
  - [dsh-gate manage](#dsh-gate-manage)
  - [dsh-gate check](#dsh-gate-check)
  - [dsh-gate service](#dsh-gate-service)
  - [dsh-gate uninstall](#dsh-gate-uninstall)
  - [dsh-gate version](#dsh-gate-version)
- [Multiple instances](#multiple-instances)
- [Server deployment (Linux + systemd)](#server-deployment-linux--systemd)
- [Remote access (using your office DSH from home)](#remote-access-using-your-office-dsh-from-home)
- [Security notes](#security-notes)
- [Environment variables](#environment-variables)
- [Frequently asked questions](#frequently-asked-questions)
- [Contributing](#contributing)
- [License](#license)

---

## What problem it solves

The Web UI that ships with dsh **only listens on `127.0.0.1`**. That is not an oversight — it is a deliberate security decision. dsh authenticates with a random token generated at startup and printed to stdout, so only someone who can read that terminal gets it. In other words, dsh was never designed to be reached from another machine.

The consequences:

- On a headless Linux server, dsh is only usable over SSH.
- dsh running on a macOS machine at home is unreachable from a Windows PC at work.
- A phone browser cannot open `127.0.0.1`.

dsh-gate exists to address exactly that. Four typical scenarios:

1. **Headless Linux server**: install dsh and dsh-gate on the server, and any PC or phone browser can open `http://server-ip:3082` — nothing to install on the client.
2. **dsh on a macOS at home, used from a Windows PC at the office** (or the other way round): run the gateway on the machine that runs dsh, and browse from the other one.
3. **Phone browser**: same address, opens directly on the phone.
4. **Two sets of dsh for one person**: one for work and one for personal use, with sessions, plugins and configuration fully separated, each on its own port.

dsh-gate does not modify a single line of dsh, and does not require you to change how you start dsh. It simply sits in front of dsh and answers one extra question: who are you.

---

## How it works

```
   any device on the LAN / the internet
              |
              |  http://192.168.1.20:3082/
              v
   +---------------------------+
   |   dsh-gate  :3082         |   <- the public entry point, login page
   |  (listens on 0.0.0.0)     |
   +---------------------------+
       |            |
  not logged in |    | logged in
       |            |
       v            v
   login page    +-----------------------------+
   password      |  reverse proxy              |
   check         |  Host header forwarded as-is|
   rate limit    |  performs the dsh startup   |
   per IP        |  token exchange for the    |
                 |  browser                    |
                 +-----------------------------+
                          |
                          |  http://127.0.0.1:3080/
                          v
                  +-------------------+
                  |   dsh   :3080     |  <- still 127.0.0.1 only;
                  | (per DSH_HOME)    |     unreachable from the LAN
                  +-------------------+
```

Four points worth noting:

- **dsh always listens on `127.0.0.1` only.** The gateway port is the single public entry point. dsh refuses `--host 0.0.0.0` for security reasons, and dsh-gate does not change that default either.
- **When the dsh port is free, the gateway starts dsh**; when dsh already holds the port, it **adopts that instance**. You do not have to care which case applies.
- **The login page belongs to the gateway.** Users need only an account password they chose themselves; it has nothing to do with dsh's startup token.
- **Multiple instances**: different values of `--dsh-home` are two completely separate dsh instances, each with its own gateway port.

### Three paths for the dsh port

On startup dsh-gate probes the dsh port and takes one of three paths:

| Port state | Behaviour |
| --- | --- |
| Free, and `--auto-start=true` (default) | Start dsh as a child process, wait until it is listening, then start forwarding |
| Already in use, and it is dsh | Adopt that instance directly instead of starting a second one (the common case: you were already running dsh yourself) |
| Already in use, but not dsh | Treated as an adoption by default, with an explicit warning in the log; pass `--adopt=false` to fail fast instead |

If dsh exits unexpectedly, the gateway restarts it with exponential backoff starting at 1 second (capped at 30 seconds).

---

## What it actually does

dsh ships with its own authentication: each process generates a random **startup token** and prints it in the `?token=` part of the URL on the `dsh web:` line. A browser visiting `GET /?token=...` exchanges it for a dsh-signed session cookie, and every API call and WebSocket afterwards validates that cookie.

In a "behind a gateway, reached from a browser" topology this creates two problems:

1. The token cannot be handed to every visitor — that would mean anyone with the URL can drive the agent.
2. The browser has no way to obtain the token, so pages load but every API call returns 401.

dsh-gate's answer is: **the token never leaves the gateway process's memory, and the gateway performs the exchange on the browser's behalf.** What you see is only an account password you set yourself.

The full flow:

```
1. Browser GETs / , with no "already exchanged" marker
      |
      v
2. Gateway appends token=... to the URL it forwards to dsh
      |
      v
3. dsh completes the exchange, writes its session cookie, 302s back to a clean ./
      |
      v
4. Gateway sets an "already exchanged" marker cookie (dsh_gate_ds)
      |
      v
5. On later visits to / the browser carries the marker, so the gateway stops
   injecting the token (otherwise it would 302 forever)
```

The marker is necessary, not an optimisation: dsh's own 302 target is `/` itself, so without a marker there is no way to tell "first visit" from "just redirected back".

If dsh returns 401 — because the session expired, or because the user switched between an IP, a hostname and a tunnel domain — the gateway clears the marker and sends the browser back to `/` to repeat the exchange. The whole thing is invisible to the user; from their side the page simply starts working.

One more thing has to be done correctly: **the `Host` header must be forwarded verbatim.** dsh compares the received `Host` against the set of accepted authorities and requires `Origin` to point at the same authority (DNS rebinding and cross-site protection). So the gateway does not rewrite `Host`; instead it passes `--trusted-host` to dsh to allow the public address. Rewriting it to the listener's address would make dsh's Origin check fail unconditionally.

The token's lifetime is strictly confined to the gateway process's memory: **never written to disk, never logged, never shown in the startup banner.** When dsh-gate forwards dsh's startup output, it redacts `token=xxx` to `token=***` before writing anything to the log.

---

> Just want it running? See the one-page [QUICKSTART.md](QUICKSTART.md) (Chinese; it ships
> with every release package).

## Portal: one entry point, many dsh instances

The single-instance setup below is "one gateway + one dsh". The portal lifts **accounts and
instances to portal level**: one entry point, one admin page, N independent dsh instances, and
each user only sees the instances they were granted.

```sh
# 1. Create the first portal account (administrator)
dsh-gate manage user add admin --role admin --password-stdin

# 2. Create instances (each with its own DSH_HOME; opaque id and loopback port are assigned)
dsh-gate manage instance add alice-main --dsh-home /srv/dsh/alice --owner alice
dsh-gate manage instance add bob-main   --dsh-home /srv/dsh/bob   --owner bob

# 3. Grant instances to users
dsh-gate manage grant add alice alice-main
dsh-gate manage grant add bob   bob-main

# 4. Start the portal
dsh-gate manage run --port 3082
```

After logging in at `http://<server>:3082/`, a normal user sees "My instances" cards; opening
one lands straight in that dsh (the portal performs the launch-token handover server-side, so
the browser never needs to know any token). Administrators additionally get `/admin`: user
management, instance creation and dsh installation, a grant checkbox matrix and the audit log.

![The dsh-gate admin page](docs/images/admin.png)

<sub>The whole admin page at a glance: users and roles, the instance grant matrix, the runtime
environment check, instances and installation, and the audit log. Below are the instances
section (one example per install mode) and the "installing" state — both instance installs and
runtime-environment fixes are background jobs; refresh the page to watch progress.</sub>

<p>
  <img src="docs/images/admin-instances.png" width="620" alt="Instance list and the create-instance form">
  <img src="docs/images/admin-installing.png" width="620" alt="A runtime fix in progress, showing “installing pnpm” and “installing”">
</p>

### Installing dsh from the admin page

The admin page **does not require dsh to be installed beforehand**. It runs an environment
check as soon as you open it, then lets you pick an install mode per instance. The order is
deliberate: **create the instance record first** (you immediately get the opaque id and the
port, visible on the page), **then install in the background** — an install takes many minutes
and must not hold an HTTP request open.

`/admin` contains:

- **dsh environment check**: status, version and path for node / npm / npx / pnpm / git / dsh,
  with a hint for anything missing, plus the platform, the gateway's own Node and pnpm, and any
  dsh already on the machine. No system Node needed — the gateway downloads its own.
- **A missing pnpm can be installed with one click**: dsh's plugin installer uses pnpm, while
  Node only ships npm/npx — on a clean server, "add plugin" fails with `spawn pnpm ENOENT`.
  One click fixes it: pnpm goes into the gateway state dir (`<state dir>/pnpm`), the system is
  untouched, no root required, and `uninstall --purge` removes it. Like the dsh install it runs
  in the background — refresh for progress, and the row turns into a version once done.
  (git gets no button: it needs the system package manager, and the distro differences are too
  wide to automate safely, so it stays a command in the hint.)

  ![The dsh environment check](docs/images/admin-env.png)
- **Create instance** form: name → dsh data dir (`DSH_HOME`) → **config dir** → workspace →
  **port** → **listen host** → **start mode** → **install mode** → install dir.
- **Instance list**: each row shows install phase and progress (queued / installing / done /
  failed, with the current step and any error), an install-mode dropdown, the install dir, and
  "install" / "delete" buttons. Re-installing is idempotent: the install dir does not nest
  deeper on every run.
- **Usable on a phone**: on narrow screens the tables turn into cards, each value carrying its
  own column label, action buttons laid out full-width instead of being pushed off-screen, and
  the create form stacked. Breakpoint 720px; the desktop layout is unchanged.

  <img src="docs/images/admin-mobile.png" width="340" alt="The admin console on a phone: each user is a card with labelled fields and full-width buttons">

Three install modes:

| Mode | Best for | What it does | Resulting launch command |
|---|---|---|---|
| **npx** (default) | Getting going fast, no dsh on the machine | `npx --yes @deepseek-ai/dsh`, smoke-verified afterwards | `<own Node>/bin/npx --yes @deepseek-ai/dsh` |
| **git** | Reading or modifying the source | `git clone` the official repo → `pnpm install` → `pnpm run build` | `node <install dir>/deepseek-harness/apps/cli/lib/bin.js` |
| **pnpm** | Offline reuse, a pinned version per instance | pnpm installs dsh into the **instance's own directory** (not globally) | `<install dir>/dsh/node_modules/.bin/dsh` |

git and pnpm modes provision Node and pnpm themselves (into the state dir, never touching the
system). The CLI and the page share the same data and the same code path:

```sh
# Decide the install mode while creating the record (same as the page's "create instance")
dsh-gate manage instance add ui-app --dsh-home /srv/ui --config-dir /srv/ui/config \
    --workspace /srv/ui/work --port 18104 --listen-host 127.0.0.1 \
    --install-mode pnpm --install-dir /srv/ui/inst

# Or create the record first and install later (or reinstall with a different mode)
dsh-gate manage instance install ui-app --install-mode pnpm
```

`instance install` runs **synchronously on the CLI** (you are sitting at the terminal watching
progress print) and as a **background job in the admin page** (refresh to see progress). A
failed install does not remove the record: the instance is still there, fix it and install again.

Key points:

- **Sub-path mounting**: every instance lives under `/i/<opaque id>/`. All instances share one
  hostname and therefore one set of cookie names, so the portal rewrites upstream `Set-Cookie`
  `Path` values to the matching mount prefix. That rewrite is what keeps sessions from bleeding
  across instances, and `go/tests/portal-e2e.sh` verifies it.
- **Denied looks like missing**: an instance you were not granted returns the same 404 response
  as an instance that does not exist.
- **Every dsh stays on loopback**: each dsh binds `127.0.0.1:<auto-assigned port>`; the portal
  is the only public listener. Instances created with `--on-demand` start when a user first
  opens them.
- **CLI parity**: `manage user|instance|grant` covers everything the admin page does (including
  instance creation and dsh installation); `--lang zh-CN|en` switches CLI output (also
  `DSH_GATE_LANG` / `LC_ALL` / `LANG`).
- The portal database is `<state dir>/portal.db` (SQLite, mode 0600). On upgrade, an existing
  single-instance `users.json` is imported on first start (the first user becomes an
  administrator) and existing passwords keep working. Older databases gain the new columns with
  defaults that reproduce the pre-upgrade behaviour, so existing instances are unaffected.

Current limitations (details in [docs/portal.md](docs/portal.md)):

- The admin page can create instances, install dsh, grant access and show the audit log;
  starting, stopping, restarting and reading instance logs are still CLI-only.
- `localStorage` is shared per origin, so one browser visiting two instances shares UI state
  (credentials do not leak).
- Changing a password or disabling a user from the CLI does not immediately disconnect them
  from a running portal (restart it, or use the admin page, which revokes sessions at once).

## Getting started in 30 seconds

```sh
# 1. Create a login account (password is prompted for interactively)
dsh-gate user add admin

# 2. Start the gateway
dsh-gate run

# 3. Open the address printed in the terminal, log in
```

Managing it:

```sh
dsh-gate status           # which instances run, and where each one lives
dsh-gate stop personal    # stop one instance (write all for everything; bare lists only)
dsh-gate restart personal # restart one instance with its original arguments, log in ~/.dsh-gate/run/logs/
```

To keep it running after you close the terminal, add `--background` (or `-d`); to
have it start on boot/login use `dsh-gate service install` (systemd on Linux, launchd
on macOS, Task Scheduler on Windows):

```sh
dsh-gate run --background     # detached, log in ~/.dsh-gate/run/logs/
dsh-gate service install      # start automatically at boot/login, and start now
dsh-gate service uninstall    # turn auto-start off
dsh-gate uninstall            # done with it: turn auto-start off + stop instances
dsh-gate uninstall --purge    # ... and delete the state directory as well
```

Step 2 prints a banner like this:

```
dsh-gate 0.3.14 · 共 1 个实例
实例 default 已启动
  访问地址（网关端口 3082）
  本机      http://127.0.0.1:3082/
  局域网    http://192.168.1.20:3082/
  dsh       127.0.0.1:3080（启动中，稍后会自动就绪）
  实例目录  /Users/you/.dsh
  账号      1 个（/Users/you/.dsh-gate/users.json）
按 Ctrl-C 停止。
```

(The banner is printed in Chinese; it is generated by the program, not by this README.)

Port 3082 is the default. It avoids dsh's own 3080 and the usual 80/443, 3000, 5173 and 8080.

If you would rather not invent a password, `--random` generates a strong one and prints it once:

```sh
dsh-gate user add admin --random
```

To check the current state at any time:

```sh
dsh-gate check
```

### The login page on a phone, and in light mode

<p>
  <img src="docs/images/login-mobile.png" width="300" alt="Login page at phone width">
  <img src="docs/images/login-light.png" width="480" alt="Login page in light mode">
</p>

The login page is a **single self-contained HTML file** (styles and scripts inlined, no
external requests), so it renders fine on an offline server; it follows the system
light/dark preference and does not break down to 320px wide.


## Tutorial

A walk-through in the order you will actually do things: from a downloaded binary to a
usable agent on your LAN. Each step says what you should see and where to look when it
goes wrong.

### Step 0 — What you need

| Requirement | Notes |
|---|---|
| A machine | The one that runs dsh and the gateway. Linux / macOS / Windows; a headless Linux server is the main case |
| One binary | Take the matching package from [dist](dist/), or build it with `sh go/build.sh` |
| **No Node.js needed** | If the machine has no Node, the gateway downloads an official runtime itself (step 2) |
| **No public IP needed** | LAN use; see [Remote access](#remote-access-using-your-office-dsh-from-home) for outside access |

```sh
tar -xzf dsh-gate_<version>_linux_amd64.tar.gz
cd dsh-gate_<version>_linux_amd64
sudo install -m755 dsh-gate /usr/local/bin/dsh-gate
dsh-gate version        # sanity check
```

### Step 1 — One command to initialise

```sh
dsh-gate setup               # run it as yourself; macOS auto-start needs no root
```

> **Do not run `sudo dsh-gate setup`.** `sudo` keeps `$HOME` (that is how macOS behaves),
> so `~/.dsh` still points at your own instance directory, yet the `settings.yaml` and
> `.credentials.yaml` written by root are owned by root (0600) — your own dsh (including
> the DSH desktop app) can then neither read nor change them. The tool refuses such a call
> outright and prints two safe alternatives; pass `--allow-root-write` if you really mean it.
> To merely put the command in `PATH`, run `sudo install -m755 dsh-gate /usr/local/bin/`
> first and then `dsh-gate setup` as yourself.

It asks a few questions; pressing Enter accepts the bracketed default (**the capital letter
is the default**: `[Y/n]` Enter = yes, `[y/N]` Enter = no):

```
实例名 [default]: test
dsh 端口 [3080]: 12080            # the port dsh itself listens on (loopback only)
对外端口 [3082]: 12081            # the port you open in the browser
dsh 实例目录 [~/.dsh]:            # dsh's data (sessions, plugins, config)
会话默认工作目录 [/root]: /www/wwwroot   # ★ which directory the agent may read and write
确认一下: …  确认以上设置？[Y/n]
管理员用户名 [admin]:
请输入口令:                        # input is hidden
DeepSeek API Key（输入不回显，直接回车跳过）：
现在安装开机（登录）自启并立即启动？[y/N] y
```

Two things worth getting right:

- **Ports are validated on the spot**: a non-number, something outside 1–65535, two
  identical ports, or a port that is already in use is rejected right there and you are
  asked again (when the port is busy the wizard also says what holds it). Do not skip
  this — discovering it at startup means the account and auto-start are already installed
  and you have to start over.

- **The session workspace** decides which files the agent can touch. Point it at the
  directory you actually want the agent to work in — not your home directory, not `/`.
  A wrong value is accepted silently, which is why the gateway prints the effective
  「工作目录」 in its startup banner on every run.
- **The DeepSeek API key** can be entered now or skipped (just press Enter) and added
  later with `dsh-gate model key set DEEPSEEK_API_KEY --stdin`. If you skip it, dsh starts
  but has no usable model.
- **Missing directories are created for you**: the paths given to `--state-dir`,
  `--dsh-home` and `--workspace` are created before startup (and printed), so you can
  skip `mkdir`.

When it finishes, `setup` has: created the account, set the default model
(`deepseek-official / deepseek-chat`), copied `dsh-gate` onto your `PATH`, installed
auto-start if you asked, and tried to install the `dsh-dock` plugin (it provides the
in-browser model settings page; if it fails you get the manual command).

### Step 2 — Wait for dsh (the first run is slow)

The gateway listens immediately and the login page opens right away; what takes time is
preparing dsh, and that happens in the background. Follow it in the log:

```sh
journalctl -u dsh-gate -f        # when installed as a service
tail -f ~/.dsh-gate/run/logs/<name>.log   # foreground/background runs
```

You should see this sequence (timings measured in a clean container):

```
本机没有 npx，正在下载 Node.js 运行时（不改系统，只放进网关状态目录）   ← ~15 s
Node.js 运行时已就绪  bin=~/.dsh-gate/node/node-vXX/bin
pnpm 已就绪（dsh 的插件安装器会用它）
dsh 启动命令已就绪（npx --yes @deepseek-ai/dsh）
dsh 已就绪  addr=127.0.0.1:12080
```

**This step needs network access** (it downloads Node and the dsh package). When the
official registry is slow or unreachable the gateway switches to a mirror
(`registry.npmmirror.com`) on its own and logs `改用镜像源重试`. Everything is usually
ready within 3–5 minutes; later restarts take seconds.

To check the configuration without changing anything:

```sh
dsh-gate check
```

### Step 3 — Open it and log in

Open the **LAN** address from the banner (or the loopback one):

```
http://192.168.1.20:12081/
```

1. Connection refused → the gateway is not running: `dsh-gate status`;
2. A login page → log in with the account from step 1;
3. "DSH is starting" page → normal; it refreshes every 2 seconds until dsh is ready;
4. Straight into the app → start talking to the agent.

> Works locally but not from another machine? Usually the firewall: `ufw allow 12081/tcp`
> on Linux, plus the security group on a cloud host.

### Step 4 — Configure a model (works without any plugin)

**Official DeepSeek** (the route ships with dsh; only the key is needed):

```sh
printf 'sk-your-key' | dsh-gate model key set DEEPSEEK_API_KEY --stdin
dsh-gate model default deepseek-official deepseek-chat
dsh-gate model list
```

**Third-party or self-hosted gateway** (anything speaking the OpenAI protocol):

```sh
printf 'your-key' | dsh-gate model key set MY_GW_API_KEY --stdin
dsh-gate model provider add my-gw --base-url https://api.example.com/v1 \
    --name my-gateway --key-env MY_GW_API_KEY --models qwen3-max,glm-5
dsh-gate model default my-gw qwen3-max
```

Restart dsh if the running instance does not pick it up: `dsh-gate restart <name>`.

> The in-browser "Settings → Models" page also works, but it depends on the `dsh-dock`
> plugin (next step). When that plugin cannot be installed, these commands are the only
> way — they edit dsh's own settings files and need no plugin or web page.

### Step 5 — Install the plugin (optional, recommended)

```sh
dsh-gate plugin list                                     # what is installed
dsh-gate plugin add dsh-dock                             # mirror fallback is automatic
dsh-gate restart all                                     # dsh loads it on restart
```

`plugin list` also checks a common failure: a settings page reporting `settings are
unavailable in this browser` usually means the profile is missing
`@deepseek-ai/dsh-web-app` — the list says so.

### Step 6 — Day-to-day

```sh
dsh-gate status                  # which instances run, on which ports and directories
dsh-gate check                   # ports, dsh state, which address to open
dsh-gate stop test               # stop test (name required; `all` means everything)
dsh-gate restart test            # restart with the original arguments
dsh-gate service status          # is auto-start installed, and where is the unit
```

Logs:

| How it runs | Where the log is |
|---|---|
| Foreground | the terminal |
| `run --background` | `<registry dir>/logs/<name>.log` (`~/.dsh-gate/run/logs/` by default) |
| `service install` (Linux) | `journalctl -u dsh-gate -f` |
| `service install` (macOS) | `~/Library/Logs/dsh-gate.log` |

### Step 7 — Removing it

```sh
dsh-gate uninstall test --service            # stop test + remove auto-start, keep the data
dsh-gate uninstall test --service --purge    # only if you really do not need the data
```

`dsh-gate service uninstall` removes auto-start only. `uninstall` **requires a target**
(`all` for everything); passing nothing just lists what is running — deliberately, because
"everything by default" costs one slip of the finger on a machine with several instances.

### Three places to look when something is wrong

1. **`dsh-gate status`** — is the gateway running, and on which port;
2. **`journalctl -u dsh-gate -f`** (or the log file) — why dsh did not start, where the
   download is stuck;
3. **`dsh-gate check`** — who holds the port, whether the dsh command can be found, which
   address to use.

For specific symptoms see [Frequently asked questions](#frequently-asked-questions):
`npx` not found, `spawn pnpm ENOENT` when installing plugins,
`settings are unavailable in this browser`, `dsh web authentication required`, and a
session workspace that did not take effect.

---

## Installation

### Download a prebuilt binary

If you would rather not touch Go, grab the archive for your platform from the release
page (Linux / macOS / Windows on amd64 and arm64, plus armv7 for boards like the
Raspberry Pi):

- GitHub: <https://github.com/wycto/dsh-gate/releases>
- Gitee (for networks that cannot reach GitHub): <https://gitee.com/wycto/dsh-gate/releases>

Unpacking gives you the executable, plus `LICENSE`, `README.md` and the one-page
`QUICKSTART.md`. `SHA256SUMS` in the release root lets you verify the download:

```sh
shasum -a 256 -c SHA256SUMS      # use sha256sum -c SHA256SUMS on Linux
```

The target machine needs neither a Go toolchain nor a C runtime — these binaries are
static.

### Build from source

The Go code lives in the `go/` subdirectory:

```sh
git clone https://github.com/wycto/dsh-gate.git
cd dsh-gate/go
go build -o dsh-gate ./cmd/dsh-gate
```

The result is `go/dsh-gate`. Copy it anywhere on your PATH; the target machine needs no Go toolchain.

Go 1.24 or newer is required. The project uses nothing but the standard library, so `go build` downloads no dependencies.

You do not have to copy it by hand: `dsh-gate setup` and `sudo dsh-gate service install`
both copy the executable to `/usr/local/bin` by default (`--bin-dir` changes that, an
empty value skips it), so `dsh-gate` works as a command right away. It is a **copy**,
not a symlink: the original directory can be cleaned up later, and the service unit
points at the copy.

You also do not need to install Node.js first: when npx is missing, the gateway
downloads the official Node.js runtime into its state directory (`~/.dsh-gate/node` by
default — no root, no system changes) and uses it to start dsh. `uninstall --purge`
removes it along with the rest.

### Cross-compiling and packaging

The repository ships a build script that produces every platform at once:

```sh
sh go/build.sh                 # all targets
sh go/build.sh linux/arm64     # one target
```

Output lands in `dist/<version>/` — one directory per platform (binary + LICENSE +
README), a `.tar.gz` per target (`.zip` on Windows), and `SHA256SUMS`:

```
dist/0.3.14/
├── dsh-gate_0.3.14_darwin_arm64/dsh-gate        dsh-gate_0.3.14_linux_arm64/dsh-gate
├── dsh-gate_0.3.14_darwin_arm64.tar.gz          dsh-gate_0.3.14_linux_arm64.tar.gz
├── dsh-gate_0.3.14_darwin_amd64/…               dsh-gate_0.3.14_linux_arm/dsh-gate   (armv7, e.g. Raspberry Pi)
├── dsh-gate_0.3.14_darwin_amd64.tar.gz           dsh-gate_0.3.14_windows_amd64/dsh-gate.exe
├── …                                           dsh-gate_0.3.14_windows_arm64/…
└── SHA256SUMS
```

dsh-gate is **pure Go** and builds with `CGO_ENABLED=0`, so every binary is static:
`scp` it over and run it — no Go toolchain and no C runtime required on the target.

The script is thin; cross-compiling by hand works just as well:

```sh
GOOS=windows GOARCH=amd64 go build -o dsh-gate.exe ./cmd/dsh-gate
GOOS=linux   GOARCH=arm64 go build -o dsh-gate      ./cmd/dsh-gate
```

Without an override the version falls back to the value in the source; releases inject
it with `-ldflags`:

```sh
go build -ldflags "-X github.com/wycto/dsh-gate/internal/version.Version=0.3.14" -o dsh-gate ./cmd/dsh-gate
```

`go/internal/version/version.go` is the single source of truth for the version — you
can also just edit it.

---

## Command reference

With no subcommand, `run` is assumed: `dsh-gate` is equivalent to `dsh-gate run`, and `dsh-gate --port 8080` is equivalent to `dsh-gate run --port 8080`.

Exit codes: `0` success, `1` runtime failure, `2` usage error.

### dsh-gate run

Starts the gateway: listens on a LAN-reachable port, serves the login page, forwards requests to the local dsh.

| Option | Default | Description |
| --- | --- | --- |
| `--name` | `default` | Instance name, used to distinguish instances and prefix logs in multi-instance runs |
| `--host` | `0.0.0.0` | Gateway listen address; `0.0.0.0` accepts connections from every device on the LAN |
| `--port` | `3082` | Gateway listen port (the port clients use) |
| `--dsh-host` | `127.0.0.1` | dsh listen address. Must be one concrete address; `0.0.0.0` is rejected by dsh |
| `--dsh-port` | `3080` | dsh listen port (local access only) |
| `--dsh-home` | `~/.dsh` | dsh instance directory (`DSH_HOME`). Different directories mean fully independent dsh instances |
| `--dsh-cmd` | empty | Command used to start dsh; supports the placeholders `{host} {port} {home} {workspace} {url} {trusted}`. Empty means auto-detect |
| `--dsh-token` | empty | A known dsh startup token; only needed when adopting an external dsh whose token cannot be obtained |
| `--npm-registry` | empty | npm registry used to fetch dsh. Empty means npm's default, with one automatic retry against a mirror when that source fails or is too slow; setting it explicitly pins the source |
| `--workspace` | empty | Working directory of the dsh child process (decides which files sessions can see by default). Empty means "inherit the gateway process's cwd": the current directory when run in a foreground shell, but the unit's `WorkingDirectory` (the user's home) when managed by a service — **`setup` writes an absolute path and passes it to the service unit** so those two "current directories" cannot diverge |
| `--state-dir` | `~/.dsh-gate` | The gateway's own state directory (holds the account file); never written into the dsh instance directory |
| `--users` | empty | Account file path; empty means `<state-dir>/users.json` |
| `--public-url` | empty | Publicly announced URL; derived from the LAN IP when empty |
| `--trusted-host` | empty | Extra trusted addresses, repeatable. LAN IPs are added automatically and rarely need to be set by hand |
| `--tls-cert` | empty | HTTPS certificate file, must be paired with `--tls-key` |
| `--tls-key` | empty | HTTPS private key file, must be paired with `--tls-cert` |
| `--auto-start` | `true` | Start dsh when its port is free |
| `--adopt` | `true` | Adopt an existing instance when the dsh port is taken, instead of failing |
| `--restart` | `true` | Restart dsh automatically after it exits unexpectedly |
| `--background` / `-d` | off | Run detached: closing the terminal does not stop it, output goes to `<run-dir>/logs/<name>.log` |
| `--run-dir DIR` | `~/.dsh-gate/run` | Run registry directory that `status` / `stop` / `restart` read from |
| `--session-ttl` | `168h` | Login session lifetime |
| `--max-login-fails` | `5` | Number of consecutive failures from one IP before a lockout starts |
| `--lockout` | `1m` | Base lockout duration; each further failure doubles it |
| `--dsh-start-timeout` | `1m30s` | Longest time to wait for a freshly started dsh to become ready |
| `--config` | empty | Multi-instance configuration file (JSON); do not combine it with any instance option |
| `--log-level` | `info` | Log level: `debug` / `info` / `warn` / `error` |
| `--log-json` | `false` | Emit logs as JSON |
| `--allow-root-write` | off | Allow writing data owned by another user while running as root. Off by default: it creates root-owned `settings.yaml` / `.credentials.yaml` that the other user's dsh (including the desktop app) can neither read nor change |

Examples:

```sh
# Default: listen on 0.0.0.0:3082, forward to 127.0.0.1:3080, instance dir ~/.dsh
dsh-gate run

# Proxy the office dsh: public port 8080, local port 8081, separate instance dir
dsh-gate run --port 8080 --dsh-port 8081 --dsh-home ~/.dsh-company --name company

# Start several instances at once from a configuration file
dsh-gate run --config dsh-gate.json
```

Placeholders supported by `--dsh-cmd`:

| Placeholder | Replaced with |
| --- | --- |
| `{host}` | dsh's listen address |
| `{port}` | dsh's listen port |
| `{home}` | dsh instance directory |
| `{workspace}` | Working directory of the dsh child process |
| `{url}` | Publicly announced URL (`--public-url`) |
| `{noopen}` | the literal `--no-open` |
| `{trusted}` | the next trusted address; repeat it to pass several |

When left empty, dsh-gate auto-detects: it looks for `dsh` on `PATH` first, then falls back to `npx @deepseek-ai/dsh`.

### dsh-gate status

Lists the gateway instances **currently running on this machine**: name, gateway port,
dsh port, dsh directory, account file, PID, uptime and access URL.

```
$ dsh-gate status
instance personal
  gateway port  0.0.0.0:3082
  dsh port      127.0.0.1:3080
  dsh dir       /Users/me/.dsh-web-home
  accounts      /Users/me/.dsh-gate/users.json
  pid           51234
  uptime        2 hours 13 minutes
  access URL    http://192.168.1.20:3082/ (plain HTTP)

1 instance running (1 registered).
stop: dsh-gate stop [name...]     restart: dsh-gate restart [name...]
```

How it differs from `check`: `check` answers "what would happen if I ran with this
configuration" (is the port taken, is the dsh command installed); `status` answers
"what is running right now". The first looks at configuration, the second at the run
registry, so `status` works without remembering how anything was started.

An instance registers itself at `~/.dsh-gate/run/<name>.json` (including the full
command line, which restart needs) and deregisters on clean shutdown. A process that
was killed hard leaves its entry behind; `status` marks it as "not running" rather
than pretending it is alive.

Options: `--run-dir DIR` to use a different registry directory, `-v` to also print
the command line.

### dsh-gate stop

Stops gateway instances running on this machine, **together with the dsh they
manage**.

**You must say which ones.** Name them to stop those; `all` means everything. With
no arguments it performs **no action at all** and just lists what is currently
running — "everything" should be something you typed on purpose, not a default.

```
dsh-gate stop personal        stop just personal
dsh-gate stop personal work   stop both
dsh-gate stop all             stop everything
```

It asks the gateway to shut down through its own control endpoint
(`POST /__dsh_gate/shutdown`, loopback only), so the behaviour is identical on all
three platforms and the instance winds down through exactly the same path as Ctrl-C:
new requests stop first, then dsh gets SIGTERM and 10 seconds to dispose itself.

The PID-file-plus-signal approach is deliberately avoided: signals need per-platform
code, and Windows has no SIGTERM at all, so it would degrade to a hard kill and dsh
would never get to finish.

Pressing `Ctrl-C` in a foreground run is equivalent — both go through the same
shutdown path.

Options: `--wait DUR` (default 20s) for how long to wait for the port to close,
`--run-dir DIR`.

### dsh-gate restart

Restarts instances with **the arguments they were started with**. Like `stop`, you
must say which ones: name them, or write `all` for everything. With no arguments it
only lists what is running and changes nothing.

```
dsh-gate restart personal     restart just personal
dsh-gate restart all          restart everything
```

It stops and relaunches rather than signalling the running process: listen address,
account file and environment are all decided at startup, so a hot restart cannot
change them.

The new process runs **detached from the terminal** and writes to
`~/.dsh-gate/run/logs/<name>.log`, so the command returns immediately instead of
holding your terminal. The log is appended to, so history survives restarts.

> When you started with `--config`, the registry stores the whole command line;
> `restart <name>` relaunches with the same configuration and pins the instance name
> back to the registered one.

**Running several instances detached:** with `--background`, `--config` gives **each
instance its own process** (rather than one process hosting them all) — because
`stop` / `restart` work per instance and a single process can only be stopped as a
whole:

```sh
dsh-gate run --config dsh-gate.json --background    # one process per instance
dsh-gate run --config dsh-gate.json                # one process for all (foreground logs)
```

`--config` is the one case where `--name` may also appear; there it does not set the
instance name but **selects one instance out of the file**:

```sh
dsh-gate run --config dsh-gate.json --name company --background
```


Options: `--wait DUR` (default 30s) for how long to wait for the port to come back,
`--run-dir DIR`.

### Bypassing the gateway to reach dsh directly (for debugging)

Once the gateway has read dsh's startup token it logs the **native dsh URL, token
included**:

```
已记录 dsh 启动令牌，本机可直接打开（该地址只在回环上有用）
  本机地址=http://127.0.0.1:3080/?token=…
```

Open it and you are at dsh itself — no login page, no token hand-off. When you
cannot log in, when the page keeps spinning, or when you simply need to know
whether the problem is in dsh or in the gateway, this is the fastest way to tell.

The token only works over loopback: dsh listens on `127.0.0.1`, so the same URL is
useless from another machine. dsh prints the very same URL on its own stdout too.

When running detached (`--background`) the line lands in the log file:

```sh
grep 本机地址 ~/.dsh-gate/run/logs/<name>.log
```

### dsh-gate user

Manages login accounts. With no subcommand it prints usage.

| Subcommand | Description |
| --- | --- |
| `user add <name>` | Add or update an account (password is prompted twice) |
| `user list` | List accounts |
| `user rm <name>` | Remove an account |

Options for `user add`:

| Option | Default | Description |
| --- | --- | --- |
| `--iterations` | `600000` | PBKDF2 iteration count (higher is more secure and slower) |
| `--random` | `false` | Generate a strong random password and print it once |
| `--password-stdin` | `false` | Read the password from standard input (for scripts) |

`user add` / `list` / `rm` all accept these two options:

| Option | Default | Description |
| --- | --- | --- |
| `--users` | empty | Account file path; derived from the instance directory when empty |
| `--dsh-home` | empty | dsh instance directory, used to derive the account file path |

```sh
# Create the first account
dsh-gate user add admin

# Manage the accounts of a specific instance (each instance keeps its own
# account file inside its instance directory)
dsh-gate user add admin --dsh-home ~/.dsh-company

# Setting a password from a script or in CI
echo "$PASSWORD" | dsh-gate user add admin --password-stdin

# Random password, so you do not have to invent one
dsh-gate user add admin --random

# List / remove
dsh-gate user list
dsh-gate user rm olduser
```

Options may appear before or after the positional argument: `user add admin --dsh-home ~/.dsh-company` and `user add --dsh-home ~/.dsh-company admin` are equivalent.

`user rm` refuses to remove the last remaining account.

### dsh-gate manage

The portal form: accounts and instances are lifted to portal level — one entry point and N
independent dsh instances. With no subcommand it prints usage.

```text
# Users
dsh-gate manage user add <name> [--role admin|user] [--random|--password-stdin]
dsh-gate manage user passwd <name> [--random|--password-stdin]
dsh-gate manage user role <name> <admin|user>
dsh-gate manage user disable|enable <name>
dsh-gate manage user list
dsh-gate manage user rm <name>

# Instances
dsh-gate manage instance list
dsh-gate manage instance add <name> --dsh-home <dir> [options]
dsh-gate manage instance install <name> [--install-mode npx|git|pnpm]
dsh-gate manage instance rename <old name or id> <new name>
dsh-gate manage instance rm <name or id>

# Grants
dsh-gate manage grant list [<name>]
dsh-gate manage grant add <user> <instance name or id>
dsh-gate manage grant rm  <user> <instance name or id>

# Common options (accepted by every subcommand)
--db <path>         portal database (default <state dir>/portal.db)
--state-dir <dir>   gateway state directory (default ~/.dsh-gate)
--lang <zh-CN|en>   output language (also DSH_GATE_LANG / LC_ALL / LANG)
```

Options for `instance add`:

| Option | Default | Description |
| --- | --- | --- |
| `--dsh-home` | required | that instance's dsh data directory (`DSH_HOME`) |
| `--config-dir` | empty | config directory; empty means the same as `--dsh-home` |
| `--workspace` | empty | that instance's default working directory (what the agent may read/write) |
| `--port` | auto-assigned | dsh listen port (loopback only) |
| `--listen-host` | `127.0.0.1` | listen address; use `0.0.0.0` to let clients reach that dsh directly |
| `--owner` | empty | default owner (login name). **A display field only — it does not grant access**; use `manage grant add` for that |
| `--on-demand` | `false` | start on demand: launch dsh only when someone opens it |
| `--install-mode` | `npx` | one of `npx` / `git` / `pnpm` |
| `--install-dir` | `<dsh-home>/install` | install directory (not needed for npx) |

Options for `instance install`: `--install-mode`, `--install-dir`, `--npm-registry` (use a
mirror, e.g. `https://registry.npmmirror.com`), `--git-url`, `--git-ref`.

```sh
# Portal account (the first administrator)
dsh-gate manage user add admin --role admin --password-stdin

# Create an instance and fix the install mode at the same time
dsh-gate manage instance add ui-app --dsh-home /srv/ui --config-dir /srv/ui/config \
    --workspace /srv/ui/work --port 18104 --install-mode pnpm --install-dir /srv/ui/inst

# Create the record first, install later (or reinstall with another mode)
dsh-gate manage instance install ui-app --install-mode pnpm

# Slow network: switch registries
dsh-gate manage instance install ui-app --install-mode npx \
    --npm-registry https://registry.npmmirror.com

# Grant and start the portal
dsh-gate manage grant add alice ui-app
dsh-gate manage run --port 3082
```

`instance install` runs **synchronously** on the CLI: progress prints to the terminal and the
command returns when it is done. On the `/admin` page it is a **background job** (refresh to
see progress), because a long install must not hold an HTTP request open. A failed install does
**not** remove the instance record: the record stays, fix the problem and install again.

An instance's **workspace** is pre-registered as dsh's default workspace before dsh is launched, so
you can start chatting straight away instead of picking one from "select a workspace" first. An
instance where you have already picked your own workspace is left untouched.

`instance rm` only unregisters the instance from the portal; it does **not** touch the dsh data
directory (sessions and workspace are the user's data).

### dsh-gate check

A pre-flight check: accounts, port usage, whether dsh is ready, and the addresses clients should use. It changes no state.

The options are the same as for `dsh-gate run` (minus `--log-level` / `--log-json`), plus `--config`. A non-zero exit code means there is a problem that must be resolved first.

Diagnostics go to stdout and logs go to stderr, so `dsh-gate check > report.txt` gives you clean output.

### dsh-gate service

Hands the gateway to the **system's service manager** so it starts automatically at
boot / login. Supported on all three platforms:

| Platform | Mechanism | Where | Needs root |
|---|---|---|---|
| Linux | systemd system service | `/etc/systemd/system/dsh-gate.service` | yes |
| macOS | launchd LaunchAgent | `~/Library/LaunchAgents/com.wycto.dsh-gate.plist` | no |
| Windows | Scheduled task (on logon) | `dsh-gate` in Task Scheduler | no |

```sh
dsh-gate service install [options]  # install and start now
dsh-gate service status             # current managed state
dsh-gate service unit [options]     # just print what would be installed
dsh-gate service uninstall          # turn auto-start off
```

Options are the same as `dsh-gate run`, plus:

| Option | Default | Meaning |
|---|---|---|
| `--binary PATH` | the real path of the running binary | absolute path to the dsh-gate executable |
| `--working-dir DIR` | current user's home | working directory; dsh treats it as the session's default workspace |
| `--user NAME` | current user | which user to run as (Linux) |

What gets managed is always `dsh-gate run` (foreground) — the service manager owns
restarting and logging, so the gateway does not background itself a second time. To
auto-start one specific instance, install with exactly its options:

```sh
dsh-gate service install --name personal --port 3082 --dsh-home ~/.dsh-web-home
```

**Multiple instances**: one `service install` manages a single command line. To
manage several, point it at a config file: `dsh-gate service install --config ~/dsh-gate.json`.

Day-to-day:

| Platform | Status | Logs |
|---|---|---|
| Linux | `systemctl status dsh-gate` | `journalctl -u dsh-gate -f` |
| macOS | `dsh-gate service status` | `log stream --predicate 'process == "dsh-gate"'` |
| Windows | `dsh-gate service status` | `dsh-gate status -v` |

> Once an instance is managed by the service manager, do **not** stop it with
> `dsh-gate stop` — the manager will immediately bring it back. Use
> `systemctl stop` (or `launchctl bootout`) instead.

### dsh-gate uninstall

The clean-up for one instance (or the whole machine): **stop the instances you
name**, and optionally turn auto-start off and delete data. Those three things are
separate and each has to be asked for explicitly.

```sh
dsh-gate uninstall test                  # stop only test; other instances are untouched
dsh-gate uninstall personal work         # stop these two
dsh-gate uninstall all --service --purge # whole machine: stop everything, no auto-start, delete data
```

**You must name a target** (same rule as `stop` / `restart`): instance names stop
those, `all` means everything, and writing nothing only lists the running instances
and does nothing (exit code 2). "Everything by default" costs one slip of the finger
on a machine running several instances.

| Option | Default | Meaning |
|---|---|---|
| `--service` | off | also turn boot/login auto-start off. **The service unit is machine-level — there is exactly one per platform**, so it cannot be per-instance; the command that it managed is printed so you can see exactly what was removed |
| `--purge` | off | delete the state directory (accounts, logs, self-provisioned Node/pnpm) and the run registry directory; irreversible |
| `--state-dir DIR` | `~/.dsh-gate` | the state directory `--purge` removes |
| `--run-dir DIR` | `~/.dsh-gate/run` | the run registry directory |
| `--wait DUR` | `20s` | how long to wait for an instance's port to close |

Data is **kept by default** — a deleted account table cannot be recovered; an
interactive terminal is asked once, and piped/CI runs never guess (use `--purge`
explicitly).

`--purge` is **refused** while other instances are still running: the state and
registry directories are machine-wide, and deleting them would leave those instances
without an identity (they would become invisible to `status` / `stop`). Use
`uninstall all --purge` for a full clean-up, or point `--state-dir` elsewhere.

Three boundaries are deliberate:

- **dsh's own data (`~/.dsh`) is never touched.** Sessions, plugins and config belong
  to dsh; uninstalling the gateway should not take them away. The self-provisioned
  Node.js runtime (`~/.dsh-gate/node`) and pnpm (`~/.dsh-gate/pnpm`) belong to the
  gateway and are removed by `--purge`.
- **The dsh-gate executable is not deleted.** A running program deleting itself is
  messy on every platform, and it may have been installed by a package manager; the
  command prints its path so you can remove it your own way. The copy that was placed
  on your `PATH` (`/usr/local/bin/dsh-gate` by default) is left behind too — remove it
  for a complete clean-up.
- **Only what you name is touched.** `--service` removes that one unit, `--purge`
  removes that one state directory; unmentioned things stay.

`--purge` takes its paths from the command line, so a single typo could point at your
home directory or the root — targets like `/` or the home directory itself are refused.

> `dsh-gate service uninstall` is just the auto-start step on its own: it removes the
> single machine-level service unit (whether it manages one instance or a list from
> `--config`) without touching instances or data.

### dsh-gate plugin

Manage the plugins inside a dsh profile directly: add the dependency, install the package,
register it in the bundle list. It does exactly what dsh's own "Add plugin" button does,
but with the gateway's self-provisioned Node/pnpm and automatic registry fallback —
**when a server lacks those, the browser plugin page cannot install anything**
(measured: `spawn pnpm ENOENT`, or a 180-second timeout against the official registry).

```sh
dsh-gate plugin list                  # what is installed, and is the bundle list sane
dsh-gate plugin add dsh-dock          # official registry first, mirror on failure
dsh-gate plugin add dsh-dock @deepseek-ai/dsh-web-app
```

Mechanism (identical to dsh's plugin manager): run `pnpm add <name>` inside
`<dsh-home>/profiles/web/`, then append the name to `dsh.profile.bundles` in that
directory's `package.json`. **dsh only picks it up after a restart** — the profile is
resolved at process start, and the command tells you to run `dsh-gate restart all`.

The profile directory is created by dsh itself on first start; if it does not exist yet
the command says so instead of inventing a profile that might be missing mount points.

| Option | Default | Meaning |
|---|---|---|
| `--dsh-home DIR` | `~/.dsh` | dsh instance directory |
| `--profile NAME` | `web` | profile name; the gateway runs `dsh web`, hence `web` |
| `--npm-registry URL` | empty | npm registry for the install; empty means automatic (official, then mirror) |
| `--state-dir DIR` | `~/.dsh-gate` | gateway state directory (holds the self-provisioned pnpm) |
| `--timeout DUR` | `10m` | install timeout |

**It also diagnoses a common "settings page is broken"**: the browser settings page
(models, credentials) comes from `ctx.remote.settings`, mounted by the
`@deepseek-ai/dsh-web-app` bundle. Without it in the list, the page can only say
`settings are unavailable in this browser`. `plugin list` checks for that — but note that
this bundle ships with dsh itself and most `@deepseek-ai/*` packages are not public on npm
(measured: `pnpm view @deepseek-ai/dsh-client-ui-command` asks for authorization), so a
missing one means reinstall/upgrade dsh. For immediate use, install the public `dsh-dock`
instead: it ships its own model-settings page, which is exactly what this command installs.

### dsh-gate model

Configure models without installing any plugin. It edits DSH's own two files:

- `~/.dsh/.credentials.yaml` under `refs:` — secrets are stored **by environment
  variable name** (official DeepSeek reads `DEEPSEEK_API_KEY` by default);
- `~/.dsh/settings.yaml` under `agent-default-model` (default provider and model)
  and `llm-pi-ai.providers` (custom providers).

```sh
dsh-gate model list                                   # show the current configuration
dsh-gate model default deepseek-official deepseek-chat # set the default
printf 'sk-…' | dsh-gate model key set DEEPSEEK_API_KEY --stdin
dsh-gate model provider add my-gw --base-url https://api.example.com/v1 \
    --key-env MY_GW_API_KEY --models qwen3-max,glm-5
dsh-gate model provider remove my-gw
```

| Option | Default | Meaning |
|---|---|---|
| `--dsh-home DIR` | `~/.dsh` | dsh instance directory (same flag as `run`) |

Secrets can only be supplied on **standard input** (`--stdin`): passing them on the
command line leaves them in shell history and in the process list.

The rewrite is surgical: only the block of the target key is replaced, everything else
(preferences, other providers…) is left byte-for-byte intact. The previous content is
kept as `<file>.dsh-gate.bak`, the result is read back and verified, and a mismatch
rolls the file back. Names or values containing colons, quotes, spaces or `#` are
rejected outright — a corrupted settings file means DSH cannot start.

### dsh-gate version

Prints the version.

```sh
$ dsh-gate version
dsh-gate 0.3.14
```

---

On macOS the unit is a **per-user** LaunchAgent (started at login), so it belongs to the
**logged-in user**, not to root:

- `dsh-gate service install` needs no sudo;
- if you used sudo to put the binary into `/usr/local/bin`, the agent is still installed
  for the user who invoked sudo (detected via `SUDO_UID` / `SUDO_USER`);
- in a root-only environment it cannot be installed — there is no usable GUI domain, and
  the tool says so explicitly instead of printing a raw launchd error.

## Multiple instances

One instance = one gateway port + one dsh + one `--dsh-home`. Different values of `--dsh-home` are completely independent dsh installations: session history, plugins and configuration all separate. Several can run inside a single process.

**Command line**, one `dsh-gate run` per instance:

```sh
# Terminal 1: the office dsh
dsh-gate run --name company   --port 8080 --dsh-port 8081 --dsh-home ~/.dsh-company

# Terminal 2: my own dsh
dsh-gate run --name personal  --port 3082 --dsh-port 3080 --dsh-home ~/.dsh
```

**Configuration file**, all instances in one process:

```sh
dsh-gate run --config examples/multi-instance.json
```

The configuration file is JSON with exactly two keys, `defaults` and `instances`:

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

That is the contents of [`examples/multi-instance.json`](examples/multi-instance.json), ready to copy and edit. A few things to know:

- **Field names use JSON camelCase** (`dshHome`, `dshPort`) and are *not* the same as the command line option names (`--dsh-home`).
- A misspelled field name is **reported as an error**, never silently ignored — typo `dshhome` gets you an immediate message rather than a silent no-op that leaves you unprotected.
- Fields you leave out keep their defaults; you do not have to spell out the whole configuration.
- **No instance option may be given together with `--config`** (`--port` and friends); dsh-gate rejects the combination and tells you to put them in the file instead.
- Instance names must be unique within one process, and listen addresses must differ.
- The account file does **not** follow the instance directory: it lives at `<state-dir>/users.json` (default `~/.dsh-gate/users.json`) because accounts belong to the gateway, not to dsh. To share one set of accounts across instances, point `users` at the same file.
- Paths may start with `~`, which expands to the current user's home directory.

---

## Server deployment (Linux + systemd)

A headless Linux server is the most typical scenario. The whole flow:

```sh
# 1. Make sure dsh runs (dsh-gate has to be able to find it)
npm i -g @deepseek-ai/dsh

# 2. Copy the binary over, or build it there
scp dsh-gate server:/usr/local/bin/

# 3. Check: accounts, ports and the dsh command
dsh-gate check --dsh-home /var/lib/dsh

# 4. Create an account (interactive; or use --random to generate one)
dsh-gate user add admin --dsh-home /var/lib/dsh

# 5. Install the systemd unit and start it immediately
sudo dsh-gate service install --dsh-home /var/lib/dsh --port 3082

# 6. Confirm it came up
systemctl status dsh-gate
```

`service install` writes the unit to `/etc/systemd/system/dsh-gate.service` and then runs `daemon-reload`, `enable` and `restart`, in that order.

Day-to-day operations:

```sh
systemctl status dsh-gate      # status
journalctl -u dsh-gate -f      # follow the logs
systemctl restart dsh-gate     # restart
systemctl stop dsh-gate        # stop
```

You can print the full unit without installing anything:

```sh
dsh-gate service unit --dsh-home /var/lib/dsh --port 3082
```

[`examples/dsh-gate.service`](examples/dsh-gate.service) is a sample unit matching that output. To change the port or the instance directory, do not edit the unit file directly — use `systemctl edit dsh-gate` and add an `ExecStart=` override.

Worth knowing:

- **`WorkingDirectory` is not optional.** dsh treats the current directory at startup as the default workspace root, and systemd's default of `/` would give sessions a meaningless root. `--working-dir` defaults to the current user's home directory.
- **`User=` defaults to the user who ran the install command.** To run as someone else, pass `--user` at install time. Note that the dsh instance directory must be writable by that user.
- **Multiple instances work with a single unit too**: put the configuration in a file, then `sudo dsh-gate service install --config /etc/dsh-gate.json`, and `ExecStart` becomes `--config /etc/dsh-gate.json`.
- **The service logs JSON by default** (`service unit` adds `--log-json`), because journalctl handles structured logs better. Running in the foreground defaults to text.
- Ports below 1024 require root; pick something above 1024 as an ordinary user.

---

## Remote access (using your office DSH from home)

**Warning: over plain HTTP, dsh's session cookies travel over the network in clear text. That is acceptable inside a LAN, but do not expose a plaintext gateway port directly to the public internet.** The three approaches below are ordered by recommendation.

### Option 1: a tunneling service (least effort)

Publish the gateway port to a public domain with any tunnelling tool. Tunnels are themselves encrypted, so traffic goes over an encrypted channel while the host keeps serving plain HTTP:

```sh
dsh-gate run --port 3082 --public-url https://your-tunnel.example.com
```

The tool hands you an address like `https://xxx.example.com`; just open that. Because the outside is HTTPS, browser-side cookies get the `Secure` flag.

### Option 2: an overlay network such as Tailscale or ZeroTier

Put home and office on the same virtual LAN. The address still looks like a LAN address, but it travels over an encrypted virtual network. Install the client on both ends and join the same tailnet / network:

```sh
# On the office machine
dsh-gate run --port 3082 --public-url http://100.x.y.z:3082

# From home
# http://100.x.y.z:3082
```

If the machine has no non-loopback address (headless servers, isolated container networks), you must set `--public-url` explicitly — otherwise dsh-gate cannot derive a public address.
> **About `--public-url`**: it is passed to dsh as `--public-url`, which **not every dsh
> release accepts** (as of `0.2.0-rc.2` the web profile only has `--host`, `--port`,
> `--no-open` and `--trusted-host`). dsh-gate runs `dsh web --help` once before starting
> dsh and silently skips options the installed version does not know, logging one line
> when it does — so upgrading dsh turns the option on without any config change. It only
> affects the URL dsh prints and the `DSH_WEB_URL` the agent sees; **forwarding is
> unaffected**.


### Option 3: let the gateway serve HTTPS itself

If you have your own domain and certificate, let the gateway terminate HTTPS:

```sh
dsh-gate run \
  --port 443 \
  --tls-cert /etc/letsencrypt/live/dsh.example.com/fullchain.pem \
  --tls-key  /etc/letsencrypt/live/dsh.example.com/privkey.pem \
  --public-url https://dsh.example.com \
  --trusted-host dsh.example.com
```

Two things to note:

- `--tls-cert` and `--tls-key` must be given together; supplying only one is rejected.
- With a custom domain you **must** add it to `--trusted-host`, otherwise dsh refuses to forward (403). LAN IPs are added automatically; domain names are not.

---

WebSocket travels over an HTTP/1.1 upgrade (which is what the browser `WebSocket` API
does anyway), so when debugging with `curl` add `--http1.1`: on an HTTP/2 connection
`curl` does not send the `Upgrade` header and dsh answers 404.

## Security notes

**How passwords are stored.** The account file is JSON with mode `0600` in a `0700` directory, by default at `~/.dsh-gate/users.json` (the gateway's own state directory, **never inside the dsh instance directory**). Passwords are hashed with PBKDF2-HMAC-SHA256 from the Go 1.24 standard library (`crypto/pbkdf2`) at 600,000 iterations, each record with its own random salt, in the format `pbkdf2-sha256$iterations$salt$digest`. Plaintext passwords are never written. Writes are atomic (temp file plus rename), so losing power halfway through cannot leave a half-written file locking everyone out.

Login attempts for accounts that do not exist still run a full KDF verification, matching the timing of a wrong password — otherwise an attacker could enumerate accounts by timing alone.

**Where sessions live.** Login sessions exist only in the gateway process's memory and are **never written to disk**. A gateway restart invalidates every session, so everyone logs in again. For a self-hosted home or office gateway that is entirely acceptable, and it buys "restart clears everything", with no stale sessions lingering. The trade-off is that sessions are not shared between gateway instances — each instance should have its own accounts anyway.

The session cookie is `dsh_gate_sid`, marked `HttpOnly` and `SameSite=Lax`, plus `Secure` when TLS is enabled.

**Login rate limiting.** After `--max-login-fails` consecutive failures (5 by default) from one IP, a lockout begins. The duration starts at `--lockout` (1 minute), doubles with each further failure, and is capped at 15 minutes. A successful login resets the counter. There is deliberately no global rate limit: dozens of people in one office share a single egress IP, and a global limit would lock out legitimate users.

**Why the token is never persisted.** dsh's startup token is the master key to dsh. It exists only in the gateway process's memory: not on disk, not in logs, not in the startup banner. When forwarding dsh's startup output, dsh-gate redacts `token=xxx` to `token=***` before recording anything.

**Why dsh keeps listening on 127.0.0.1 by default.** dsh has no security design aimed at the LAN: plain HTTP, and the startup token printed straight to stdout. Exposing exactly one entry point — the gateway with its login page — is the simplest and least error-prone boundary between the two.

**A pitfall worth internalising: changing the port on your own does nothing.** dsh compares the incoming `Host` against its accepted authorities and requires `Origin` to point at the same authority. The gateway forwards `Host` verbatim, so whichever address you use in the browser must be named to dsh via `--trusted-host`.

- LAN IPs are added to the trusted list automatically and rarely need to be set by hand;
- **hostnames, mDNS names and tunnelling domains are not added automatically** and must be passed explicitly with `--trusted-host`;
- the symptom of a missing entry is **403 on every API call**.

Likewise, `--dsh-host` rejects `0.0.0.0`: dsh refuses wildcard listen addresses for security reasons, and dsh-gate intercepts that early with an actionable message.

The upstream address comes from local configuration only and is never taken from a request header or query parameter, so the gateway cannot be turned into a jump box to arbitrary URLs. Rate limiting uses the TCP peer address and deliberately ignores `X-Forwarded-For` / `X-Real-IP` — unless you know for certain that your own reverse proxy sits in front, those headers are trivially forged.

---

## Environment variables

Precedence: **command line > environment variables > configuration file > defaults.**

Environment variables exist so that systemd units, Docker containers and ad-hoc shells can be configured without touching the command line.

| Environment variable | Option | Description |
| --- | --- | --- |
| `DSH_GATE_NAME` | `--name` | Instance name |
| `DSH_GATE_HOST` | `--host` | Gateway listen address |
| `DSH_GATE_PORT` | `--port` | Gateway listen port |
| `DSH_GATE_DSH_HOST` | `--dsh-host` | dsh listen address |
| `DSH_GATE_DSH_PORT` | `--dsh-port` | dsh listen port |
| `DSH_GATE_DSH_HOME` | `--dsh-home` | dsh instance directory |
| `DSH_GATE_DSH_CMD` | `--dsh-cmd` | Command used to start dsh |
| `DSH_GATE_DSH_TOKEN` | `--dsh-token` | A known dsh startup token |
| `DSH_GATE_NPM_REGISTRY` | `--npm-registry` | npm registry used to fetch dsh; empty means automatic (fall back to a mirror) |
| `DSH_GATE_WORKSPACE` | `--workspace` | Working directory of the dsh child process |
| `DSH_GATE_USERS` | `--users` | Account file path |
| `DSH_GATE_PUBLIC_URL` | `--public-url` | Publicly announced URL |
| `DSH_GATE_TRUSTED_HOSTS` | `--trusted-host` | Extra trusted addresses, comma-separated |
| `DSH_GATE_TLS_CERT` | `--tls-cert` | HTTPS certificate file |
| `DSH_GATE_TLS_KEY` | `--tls-key` | HTTPS private key file |
| `DSH_GATE_AUTOSTART` | `--auto-start` | Whether to start dsh when its port is free; `true` / `false` |
| `DSH_GATE_ADOPT` | `--adopt` | Whether to adopt an instance holding the port; `true` / `false` |
| `DSH_GATE_RESTART` | `--restart` | Whether to restart dsh after an unexpected exit; `true` / `false` |
| `DSH_GATE_SESSION_TTL` | `--session-ttl` | Login session lifetime, e.g. `168h` |
| `DSH_GATE_MAX_LOGIN_FAILS` | `--max-login-fails` | Lockout threshold |
| `DSH_GATE_LOCKOUT` | `--lockout` | Base lockout duration, e.g. `1m` |

When `--config` is used these variables have no effect; the file wins.

---

## Frequently asked questions

**The page loads but every request returns 401.**

dsh's session is bound to the authority. Switching between an IP, a hostname and a tunnelling domain — or dsh restarting with a new token — invalidates the dsh session the browser holds. The gateway clears the marker and retries automatically, which usually looks like "the page fixed itself". If it does not, paste dsh's full startup URL after the gateway address once:

```
http://192.168.1.20:3082/?token=6f1c...
```

The gateway stores that token in memory and never logs it. When adopting a dsh that somebody else started, this is the only workable source of the token; you can also supply it directly with `--dsh-token`.

**Inside an instance it stays on "reconnecting" — reload the page.**

Every dsh launch mints a **new launch token**, which invalidates the dsh session in any tab you
already had open. The dsh frontend only shows "reconnecting" and retries forever; **it does not
redo the token handoff by itself** — that is dsh client behaviour the gateway cannot change (a
rejected WebSocket handshake cannot be redirected).

A page reload is enough: a reload is a navigation, and the gateway performs the handoff with the
new token during it. **So after restarting the gateway, or after an instance restarts, any open
tab needs one reload.**

**The page loads but every API call returns 403 (inside an instance it stays on "reconnecting" and the workspace will not open).**

`--trusted-host` does not cover the address you are using. dsh trusts loopback addresses, its own bind address and the authorities named explicitly, so it rejects every `/api` request and the WebSocket. The symptom is misleading: **the page itself loads fine** (`GET /` is the launch-token exchange, which is not behind that fence); only the conversation and workspace spin.

LAN IPs and hostnames are added automatically, so usually there is nothing to do. Three cases cannot be derived: custom domains, mDNS names and tunnelling domains — plus **container port mapping / NAT**, where the interface address the gateway sees inside the container is not the one you typed in the browser. Name it explicitly:

```sh
dsh-gate run --trusted-host dsh.example.com --trusted-host my-mac.local

# Management form: --public-url makes the gateway derive the hostname too
dsh-gate manage run --port 3082 --public-url http://172.18.98.20:12090/
# Or name the authority directly (with a port = that exact authority only)
dsh-gate manage run --port 3082 --trusted-host 172.18.98.20:12090
```

**The port is already in use.**

```
网关端口 3082 已被占用。
换一个端口：加 --port <其它端口>
或者先停掉占用它的程序。
```

Run `dsh-gate check` first to see who holds it. If the port is below 1024 the message is about permissions; pick something above 1024 instead.

**`--dsh-host 0.0.0.0` is rejected.**

On purpose. dsh refuses wildcard listen addresses and only allows binding one concrete address. The default `127.0.0.1` is what you want — public access belongs to the gateway port.

**I cannot type a password for `user add` on Windows.**

Outside an interactive terminal (in CI, or under some PowerShell configurations) the program refuses outright and points you at the two alternatives:

```sh
dsh-gate user add admin --random
echo "$PASSWORD" | dsh-gate user add admin --password-stdin
```

Passwords must be between 8 and 256 characters, counted in characters so that non-ASCII passwords are not penalised for their UTF-8 encoding.

**The first startup is slow.**

dsh downloads plugins and initialises its profile on first run, which can take a minute or two. The gateway port is already open during that time, so you get a "DSH is starting" page that refreshes every two seconds rather than a connection refused. How long it waits is controlled by `--dsh-start-timeout` (1 minute 30 seconds by default).

If the machine does not even have Node.js, the runtime is downloaded first (about 15 seconds) and npx then fetches the dsh package — a cold machine can take a few minutes end to end. None of that blocks the service: the gateway listens first, the login page opens, and `dsh-gate status` shows the instance immediately. The preparation runs in the background and the waiting page moves on to the login page by itself. The log shows the sequence `正在下载官方 Node.js 运行时` → `Node.js 运行时已就绪` → `dsh 启动命令已就绪` → `dsh 已就绪`.

**The npm registry is too slow, or dsh cannot be fetched (the log says "长时间无响应").**

The gateway switches sources by itself: if the official registry (`registry.npmjs.org`) or
nodejs.org cannot be reached or times out, it retries once against a mirror
(`registry.npmmirror.com`) and injects the source that actually worked into dsh as
`npm_config_registry` — so dsh's own plugin installs use the same source. To pin a source
(a corporate registry, a private proxy) just pass it explicitly; an explicit source is
never replaced automatically:

```sh
dsh-gate run --npm-registry https://registry.npmmirror.com
export DSH_GATE_NPM_REGISTRY=https://registry.npm.example.com   # env var or config file
```

Switching sources only helps when the source is the problem. A silently skipped entry
point, a missing native binding or a crash on an old Node.js are runtime incompatibilities:
there the gateway falls back to a pinned dsh version instead.

**dsh will not start.**

dsh failing to start does not take the gateway down: it keeps retrying with exponential backoff (starting at 1 second, capped at 30) and the waiting page shows the most recent failure reason. Run `dsh-gate check` to see whether the dsh command can be found; if not, install dsh or give an explicit command with `--dsh-cmd`.

**The page shows `dsh web authentication required; reopen the URL printed by dsh web.`**

That is dsh's own 401 body: the browser-trust cookie does not match the current authority.
The gateway's handoff marker used to be a plain boolean, so after dsh restarted with a new
token — or after the session expired or the access authority changed — the marker survived
and the gateway **never injected a token again**: reloading and logging in again both did
nothing. The marker now stores a fingerprint of the token used for the handshake, so a new
dsh token makes every browser re-handshake automatically on its next `/` load, and a 401
from dsh clears the marker and 303s the browser back to `/`. Only if the gateway itself
holds the wrong token (for example it adopted an external dsh without learning the token)
do you need to act: paste dsh's full startup URL once, or run `dsh-gate restart <name>`.

**The server has no Node.js (the page says npx cannot be found).**

You do not have to install it yourself: at startup the gateway checks for npx and, if it
is missing, downloads the **official Node.js binary** into its state directory
(`~/.dsh-gate/node` by default), verifies it against the official `SHASUMS256.txt` and
uses the extracted runtime — no root, no system packages touched. If the download fails,
the page shows why; you can install Node.js by hand (<https://nodejs.org/>) or point at
your own command with `--dsh-cmd`. musl systems such as Alpine cannot run the official
glibc binaries — use `apk add nodejs` there.

An already installed runtime is reused (no repeated downloads) and `dsh-gate uninstall
--purge` removes it.

**The session's default workspace did not take effect (DSH says it cannot create the default workspace).**

With an empty `--workspace` the semantics are "inherit the gateway process's current
directory" — and when the gateway is managed by systemd/launchd that directory is the
unit's `WorkingDirectory` (the user's home, `/root`), not the directory where you ran
`setup`. The wizard used to print "current directory" without recording it, so the
promise was never kept. It now shows the real absolute path, records it, and passes
`--workspace` into the service unit. Already installed? Re-run with the directory you want:

```sh
cd /the/directory/agents/should/use
sudo dsh-gate service install --workspace "$PWD" --dsh-home /www/wwwroot/dsh-test
```

**The page says "DSH 要求重新登录" (an externally started dsh was adopted).**

dsh's startup token is **generated per process, printed once at startup, and never written to
disk**. If the running dsh was not started by the gateway (you ran `dsh web` yourself, or the
gateway process was replaced while the old dsh kept running), the gateway cannot know it: the
home page opens, but conversations report unauthorized.

Two ways out; the second always works:

```sh
pkill -f "dsh web"          # stop the stray dsh
dsh-gate restart <name>     # the gateway starts dsh itself and captures the token
```

**The page shows `fork/exec …/node/bin/npx: no such file or directory`.**

The self-provisioned Node.js runtime disappeared after preparation (cleaned up after a
failed smoke test, state directory deleted, replaced by another process) while PATH still
points at the old path. Delete it and let the gateway fetch it again:

```sh
rm -rf <state-dir>/node      # e.g. /www/wwwroot/dsh-gate-test/node
dsh-gate restart <name>
```

**Installing a plugin in DSH fails with `spawn pnpm ENOENT`.**

DSH's plugin installer uses pnpm, while Node.js only ships npm/npx. The gateway now
installs pnpm into a private prefix inside its state directory (`<state-dir>/pnpm`, no
system changes, no root) before preparing dsh, and prepends its bin directory to PATH.
An existing pnpm is left alone. The log shows `已自备 pnpm：…`; if the install fails the
original npm error is printed (usually a slow registry — switch with `--npm-registry`).

**`dsh-gate: command not found`.**

It is not on your `PATH`: either you are running `./dsh-gate` directly, or the binary was
copied somewhere outside `PATH`. Run `dsh-gate setup` or `sudo dsh-gate service install`
once (they copy it to `/usr/local/bin` by default; `--bin-dir` picks another directory).
If it still is not found, check that the directory is on your `PATH` (`echo $PATH`).

**Everyone has to log in again after a gateway restart.**

By design: sessions live only in memory, and a restart invalidates all of them.

**Where is the account file?**

By default at `~/.dsh-gate/users.json`. Change it with `--users` / `--state-dir`, or with the `users` / `stateDir` fields in the configuration file.

**Can `--config` be combined with command line options?**

No. With `--config`, no instance option is allowed; dsh-gate rejects the combination and tells you to put them in the file instead. (`--log-level` and `--log-json` are not instance options.)

---

### The gateway is up but the page keeps saying "DSH is starting"

Check the gateway log (`journalctl -u dsh-gate -f`, or the terminal if you run it in the
foreground). The first start downloads plugins and can take a minute or two. If it never
succeeds, the log replays dsh's own error output. Common causes:

- **dsh not found** — `dsh-gate check` reports whether it was found. Install it
  (`npm i -g @deepseek-ai/dsh`) or point at it with `--dsh-cmd`.
- **The dsh port is taken by something else** — pick another `--dsh-port`.
- **`trustedHosts entry ... is not a bare host[:port] authority`** — a `--trusted-host`
  value you passed contains a bare IPv6 address. IPv6 must be bracketed:
  `[2408:...::1]`. Addresses the gateway adds automatically are already handled.

## Contributing

Issues and pull requests are welcome. Before you start, it is worth reading the package comment at the top of `go/internal/proxy/proxy.go` — the startup-token exchange is the most interesting part of the project, and almost every "the page loads but the API fails" symptom can be traced back to it.

A few ground rules:

- The project uses the Go standard library only. **Do not add third-party dependencies.**
- Update the README and CHANGELOG alongside any behaviour change.
- The Go code lives in the `go/` subdirectory; the module is `github.com/wycto/dsh-gate`.
- Commit messages are written in Chinese in the form `类型：备注`.

## License

[MIT](LICENSE). Copyright © 2026 wycto