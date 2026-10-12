#!/bin/sh
# setup 交互流程的端到端验收：端口被占用时的选择、API Key 的星号回显。
#
# 覆盖两条用户反馈：
#   1. 端口被占用时只丢一句「换一个端口」，而占用者往往就是用户自己刚起的服务——
#      应该让他选：停掉占用者 / 接管 dsh / 换端口；
#   2. API Key 输入时屏幕上一片空白，用户以为程序没在等输入——应该逐字符回显 *。
#
# 为什么必须在容器里做：这两条都发生在**真终端**上（term.IsTerminal 为真、raw 模式
# 生效），而普通管道跑出来的 setup 走的是非交互分支，恰好绕开了要验的代码。
# 这里用 docker exec -it 造一个真 TTY，再由宿主机的 PTY 驱动喂按键。
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"

version=$(sed -n 's/^var Version = "\(.*\)"$/\1/p' go/internal/version/version.go)
case "$(docker version --format '{{.Server.Arch}}')" in
arm64 | aarch64) arch=arm64 ;;
amd64 | x86_64) arch=amd64 ;;
*) echo "不认识的容器架构" >&2; exit 1 ;;
esac
pkg="dist/$version/dsh-gate_${version}_linux_${arch}.tar.gz"
dir="dsh-gate_${version}_linux_${arch}"
[ -f "$pkg" ] || { echo "找不到 ${pkg}：先跑 sh go/build.sh linux/${arch}" >&2; exit 1; }

name="dsh-gate-setup-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

echo "== 起容器"
docker run -d --name "$name" -v "$root/$pkg:/tmp/pkg.tar.gz:ro" ubuntu:24.04 sleep infinity >/dev/null
docker exec "$name" sh -c "cd /tmp && tar -xzf pkg.tar.gz 2>/dev/null && install -m755 ${dir}/dsh-gate /usr/local/bin/dsh-gate &&
  mkdir -p /srv && dsh-gate version"

echo "== 先占用一个端口，扮演「用户自己刚起的服务」"
# 注意：要让 13580 真的被占住，占用者必须**监听**这个端口。网关的 --dsh-port 是
# 上游地址、自己不监听（第一版就踩了这个坑：占用者起来后 13580 其实是空的，
# 于是 setup 根本没进入冲突分支）。这里让占用者用自己的网关端口占住 13580。
docker exec "$name" sh -c 'mkdir -p /srv/occ-state &&
  printf "OccPassw0rd123\n" | dsh-gate user add admin --password-stdin --users /srv/occ-state/users.json >/dev/null'
docker exec -d "$name" sh -c 'dsh-gate run --name occupant --port 13580 --dsh-port 13591 \
  --dsh-home /srv/occ-dsh --state-dir /srv/occ-state --auto-start=false >/srv/occ.log 2>&1'

# 等它真的监听上（bash 的 /dev/tcp 探一下，比看日志可靠）。
i=0
while [ $i -lt 30 ]; do
  if docker exec "$name" bash -c 'exec 3<>/dev/tcp/127.0.0.1/13580' 2>/dev/null; then break; fi
  i=$((i + 1)); sleep 1
done
occupant_pid=$(docker exec "$name" sh -c 'pgrep -f "[d]sh-gate run --name occupant" | head -1')
if [ -z "${occupant_pid}" ]; then
  echo "占用者没起来，日志：" >&2
  docker exec "$name" tail -5 /srv/occ.log >&2
  exit 1
fi
echo "   占用者 PID=${occupant_pid}，正监听 13580"

echo "== 用真 TTY 跑 setup（dsh 端口填被占用的 13580，选择停掉它）"
cd "$root"
# 逐项对应向导的提问顺序：
#   实例名 / dsh 端口 / 冲突选择 / 对外端口 / 实例目录 / 工作目录 /
#   确认 / 管理员名 / 口令 / 口令确认 / 开机自启 / 后台启动 / API Key
FEED='interactive|13580|s|13581|||y||TestPassw0rd!x|TestPassw0rd!x|n|n|SK-SECRET-SHOULD-NEVER-APPEAR' \
  python3 go/tests/pty_drive.py docker exec -i -t "$name" dsh-gate setup \
    --state-dir /srv/newstate --bin-dir /srv/bin > /tmp/setup-pty.log 2>&1 || true

echo "== 检查：端口冲突时给出了选择，并且真的停掉了占用者"
grep -q "已被占用" /tmp/setup-pty.log || { echo "没有提示端口被占用" >&2; tail -20 /tmp/setup-pty.log >&2; exit 1; }
echo "   提示了端口被占用 ✓"
grep -qE "\[s\] 停掉它|停掉它" /tmp/setup-pty.log || { echo "没有给出「停掉它」的选项" >&2; tail -20 /tmp/setup-pty.log >&2; exit 1; }
echo "   给出了「停掉它」的选项 ✓"
grep -q "已停掉进程" /tmp/setup-pty.log || { echo "没有真的停掉占用进程" >&2; tail -20 /tmp/setup-pty.log >&2; exit 1; }
echo "   真的停掉了占用进程 ✓"
docker exec "$name" sh -c "kill -0 $occupant_pid 2>/dev/null" && { echo "占用进程居然还活着" >&2; exit 1; } || echo "   占用进程已退出 ✓"

echo "== 检查：API Key 逐字符回显 *，且原文不出现在屏幕上"
if grep -q "SK-SECRET-SHOULD-NEVER-APPEAR" /tmp/setup-pty.log; then
  echo "密钥原文出现在了终端输出里" >&2
  grep -n "SK-SECRET" /tmp/setup-pty.log | head -3 >&2
  exit 1
fi
echo "   密钥原文没有出现 ✓"
if grep -q '\*\{5,\}' /tmp/setup-pty.log; then
  echo "   看到了星号回显 ✓"
else
  echo "没有看到星号回显（用户又会以为程序没在等输入）" >&2
  tail -20 /tmp/setup-pty.log >&2
  exit 1
fi

echo "== 检查：密钥真的写进了凭据文件（回显是星号，但值要正确）"
if docker exec "$name" sh -c 'grep -rqs "SK-SECRET-SHOULD-NEVER-APPEAR" /root/.dsh/.credentials.yaml /srv 2>/dev/null'; then
  echo "   凭据文件里写入了密钥 ✓"
else
  echo "凭据文件里没有密钥（星号回显了，但值没存下来）" >&2
  docker exec "$name" sh -c 'ls -la /root/.dsh/ 2>/dev/null | head -5' >&2
  exit 1
fi

echo
echo "全部通过。"
