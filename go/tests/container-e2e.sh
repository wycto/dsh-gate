#!/bin/sh
# 干净 Linux 容器里的端到端验收。
#
# 为什么必须有这个脚本：网关的真实战场是「无界面 Linux 服务器」，而开发者手上
# 是 macOS —— systemd、最小发行版缺库、无 Node 环境这些只在 Linux 上才暴露。
# 实测教训（2026-10-12）：在 ubuntu:24.04 干净容器里，官方 Node 二进制缺
# libatomic.so.1，下载与解包都成功、一执行就报错，而当时的实现只把运行时删掉、
# 不留任何原因，用户看到的只是「找不到 npx」无限重试。这种问题本地永远测不出来。
#
# 用法：
#   sh go/tests/container-e2e.sh            # 默认 ubuntu:24.04；包按容器架构自动选
#   sh go/tests/container-e2e.sh debian:12  # 换基础镜像
#
# 依赖：docker（已登录的 daemon）。
set -eu

image=${1:-ubuntu:24.04}
root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"

version=$(sed -n 's/^var Version = "\(.*\)"$/\1/p' go/internal/version/version.go)

# 容器架构必须和包对上：在 Apple Silicon 上把 amd64 包塞进 arm64 容器，跑起来是
# 「Dynamic loader not found」（OrbStack 没有 x86 模拟层）——报错看着像缺系统库，
# 其实是架构不对。本脚本第一次写就踩了这个坑，白跑两轮。
case "$(docker version --format '{{.Server.Arch}}')" in
arm64 | aarch64) arch=arm64 ;;
amd64 | x86_64) arch=amd64 ;;
*)
  echo "不认识的容器架构；请用 linux/amd64 或 linux/arm64" >&2
  exit 1
  ;;
esac
pkg="dist/$version/dsh-gate_${version}_linux_${arch}.tar.gz"
dir="dsh-gate_${version}_linux_${arch}"
[ -f "$pkg" ] || { echo "找不到 ${pkg}：先跑 sh go/build.sh linux/${arch}" >&2; exit 1; }
echo "== 容器架构 ${arch}，使用包 ${pkg}"

name="dsh-gate-e2e-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

echo "== 起容器 ${name}（${image}，干净系统：没有 Node/npx/pnpm）"
docker run -d --name "$name" -v "$root/$pkg:/tmp/pkg.tar.gz:ro" "$image" sleep infinity >/dev/null
docker exec "$name" sh -c 'command -v node || command -v npx || echo "确认：没有 Node"'

echo "== 装 curl（验收脚本自己要用；业务不依赖它）"
if ! docker exec "$name" sh -c 'command -v curl >/dev/null'; then
  docker exec "$name" sh -c 'apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null' 2>/dev/null || \
  docker exec "$name" sh -c 'apk add --no-cache curl >/dev/null' 2>/dev/null || true
fi

echo "== 解包并安装到 PATH"
docker exec "$name" sh -c "cd /tmp && tar -xzf pkg.tar.gz && install -m755 ${dir}/dsh-gate /usr/local/bin/dsh-gate && dsh-gate version"

echo "== 包内容与快速上手卡片"
docker exec "$name" sh -c "
set -e
cd /tmp/${dir}
for f in QUICKSTART.md README.md LICENSE dsh-gate; do
  [ -e \"\$f\" ] || { echo \"包里缺 \$f\" >&2; exit 1; }
done
echo '   卡片、README、LICENSE、二进制都在 ✓'
# 卡片里出现的每条 dsh-gate 命令都要真实存在：卡片是给人照着敲的，
# 里面写错一个子命令，读者就会在第一步撞墙。这里逐个用 --help 验证。
cmds=\$(grep -oE 'dsh-gate [a-z-]+( [a-z-]+)?' QUICKSTART.md | sort -u)
[ -n \"\$cmds\" ] || { echo '卡片里没找到任何命令，检查一下' >&2; exit 1; }
echo \"\$cmds\" | while read -r line; do
  sub=\${line#dsh-gate }
  case \"\$sub\" in
    # 排除占位符，以及 journalctl -u dsh-gate -f 这类被正则误抓的选项。
    ''|*'<'*|-*) continue ;;
  esac
  # 跳过明显不是子命令的（例如 \"dsh-gate uninstall <实例名>\" 里的占位符）
  set -- \$sub
  if ! dsh-gate \$1 --help >/dev/null 2>&1; then
    echo \"卡片里的命令不存在: \$line\" >&2
    exit 1
  fi
done
echo '   卡片里提到的命令都存在 ✓'
"

echo "== 建账号与模型配置"
docker exec "$name" sh -c "mkdir -p /srv/state /srv/dsh &&
  dsh-gate user add admin --random --state-dir /srv/state >/dev/null &&
  printf 'sk-e2e-test' | dsh-gate model key set DEEPSEEK_API_KEY --stdin --dsh-home /srv/dsh >/dev/null &&
  dsh-gate model default deepseek-official deepseek-chat --dsh-home /srv/dsh >/dev/null &&
  head -1 /srv/dsh/.credentials.yaml"

echo "== 启动网关（期望：网关先监听、Node/dsh 在后台准备）"
docker exec -d "$name" sh -c 'cd /srv && dsh-gate run --background --name e2e --port 13081 \
  --dsh-port 13080 --dsh-home /srv/dsh --state-dir /srv/state --workspace /srv >/dev/null 2>&1'
sleep 6
docker exec "$name" sh -c 'dsh-gate status | head -3' | grep -q "实例 e2e" || { echo "网关没起来" >&2; exit 1; }
echo "   网关已监听，实例可见 ✓"

echo "== 等 dsh 就绪（最多 8 分钟：要下 Node + dsh + 初始化 profile）"
i=0
while [ $i -lt 48 ]; do
  docker exec "$name" sh -c 'grep -q "dsh 已就绪" /root/.dsh-gate/run/logs/e2e.log 2>/dev/null' && break
  i=$((i + 1)); sleep 10
done
docker exec "$name" sh -c 'grep -qE "dsh 已就绪" /root/.dsh-gate/run/logs/e2e.log' || {
  echo "dsh 未在预期时间内就绪，日志尾部：" >&2
  docker exec "$name" sh -c 'tail -5 /root/.dsh-gate/run/logs/e2e.log' >&2
  exit 1
}

echo "== 检查：dsh 进程的工作目录必须是 --workspace 给的那个"
cwd=$(docker exec "$name" sh -c 'for p in $(pgrep -f "dsh web" | head -1); do readlink /proc/$p/cwd; done')
echo "   dsh cwd = $cwd"
[ "$cwd" = "/srv" ] || { echo "工作目录没生效：期望 /srv，实际 $cwd" >&2; exit 1; }
echo "   注意：容器里没有 systemd，这条验证的是「--workspace 能传到子进程」；" \
     "systemd 路径（单元里的 WorkingDirectory / ExecStart）请在真实服务器上用" \
     "systemctl cat 复核。"

echo "== 检查：登录 → 代理 → 拿到 dsh 首页"
docker exec "$name" sh -c '
  set -e
  # 口令随机重置，免得依赖前面 --random 那次打印的内容（脚本里不解析它）。
  dsh-gate user passwd admin --random --users /srv/state/users.json >/dev/null 2>&1 ||     dsh-gate user add admin --random --users /srv/state/users.json >/dev/null 2>&1
  PW=$(dsh-gate user passwd admin --random --users /srv/state/users.json 2>/dev/null | grep -o "[A-Za-z0-9]\{20\}" | head -1)
  [ -n "$PW" ] || { echo "   拿不到口令" >&2; exit 1; }
  J=/tmp/jar; rm -f $J
  code=$(curl -s -o /dev/null -w "%{http_code}" -c $J -b $J -d "username=admin&password=$PW" http://127.0.0.1:13081/__dsh_gate/login)
  echo "   登录响应 ${code}（应为 303）"
  first=$(curl -s -o /dev/null -w "%{http_code}" -c $J -b $J -H "Sec-Fetch-Mode: navigate" http://127.0.0.1:13081/)
  second=$(curl -s -o /dev/null -w "%{http_code}" -c $J -b $J -H "Sec-Fetch-Mode: navigate" http://127.0.0.1:13081/)
  echo "   首次 / → ${first}（应为 303：网关补令牌），再次 / → ${second}（应为 200）"
  [ "$second" = "200" ] || exit 1
  curl -s -c $J -b $J http://127.0.0.1:13081/ | grep -q "DeepSeek" || { echo "   没拿到 dsh 首页" >&2; exit 1; }
  echo "   ✓ 代理链路通"
'

echo
echo "全部通过。容器 $name 保留到脚本结束（退出时自动删除）。"
