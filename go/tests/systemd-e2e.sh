#!/bin/sh
# systemd 路径的端到端验收（service install / status / uninstall）。
#
# 补的是什么：container-e2e.sh 里没有 init，`systemctl` 是假的，所以
# 「写出的 systemd 单元到底对不对」一直没被验证过——而这正是最容易出问题的地方：
# 实测就发现过单元里的 WorkingDirectory 停在用户家目录，与 dsh 的会话工作目录
# 不一致（网关进程 cwd 与 dsh cwd 分家）。
#
# 这个脚本在 privileged + systemd 作为 PID 1 的容器里跑真实的
# `systemctl enable/restart/disable`，并检查单元内容与实际进程行为。
#
# 用法：
#   sh go/tests/systemd-e2e.sh            # 用 dist/<当前版本> 的 linux 包
#   VER=0.3.6 sh go/tests/systemd-e2e.sh  # 指定版本
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"

version=${VER:-$(sed -n 's/^var Version = "\(.*\)"$/\1/p' go/internal/version/version.go)}
name="dsh-gate-sysd-e2e-$$"
image="dsh-gate-sysd:test"

# 包要跟容器架构对上：Apple Silicon 上塞 amd64 包会报 Dynamic loader not found。
case "$(docker version --format '{{.Server.Arch}}')" in
arm64 | aarch64) arch=arm64 ;;
amd64 | x86_64) arch=amd64 ;;
*) echo "不认识的容器架构；请用 linux/amd64 或 linux/arm64" >&2; exit 1 ;;
esac
pkg="dist/$version/dsh-gate_${version}_linux_${arch}.tar.gz"
dir="dsh-gate_${version}_linux_${arch}"
[ -f "$pkg" ] || { echo "找不到 ${pkg}：先跑 sh go/build.sh linux/${arch}" >&2; exit 1; }

cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "== 构建带 systemd 的镜像"
docker build -q -f go/tests/Dockerfile.systemd -t "$image" go/tests >/dev/null

echo "== 起容器（systemd 作为 PID 1）"
docker run -d --name "$name" --privileged --cgroupns host \
  --tmpfs /run --tmpfs /run/lock \
  -v "$root/$pkg:/tmp/pkg.tar.gz:ro" "$image" >/dev/null

i=0
while [ $i -lt 30 ]; do
  state=$(docker exec "$name" systemctl is-system-running 2>/dev/null | head -1 || true)
  case "$state" in
    running | degraded) break ;;
  esac
  i=$((i + 1)); sleep 2
done
state=$(docker exec "$name" systemctl is-system-running 2>&1 | head -1)
case "$state" in
running | degraded) echo "   systemd 就绪（${state}）" ;;
*) echo "systemd 没起来：${state}" >&2; exit 1 ;;
esac

docker exec "$name" sh -c "
set -e
cd /tmp && tar -xzf pkg.tar.gz 2>/dev/null
install -m755 ${dir}/dsh-gate /usr/local/bin/dsh-gate
mkdir -p /srv/state
dsh-gate user add admin --random --state-dir /srv/state >/dev/null 2>&1
echo '   $(dsh-gate version 2>/dev/null || echo 已装入)'
"

echo "== service install（真实 systemd）"
docker exec "$name" bash -c '
set -e
cd /srv
dsh-gate service install --port 13181 --dsh-port 13180 --name sysdtest \
  --dsh-home /srv/dsh --state-dir /srv/state --workspace /srv --bin-dir /usr/local/bin
'

echo "== 单元与运行状态"
docker exec "$name" bash -c '
set -e
enabled=$(systemctl is-enabled dsh-gate)
active=$(systemctl is-active dsh-gate)
echo "   enabled=${enabled} active=${active}"
[ "$enabled" = enabled ] || { echo "没有开机自启" >&2; exit 1; }
[ "$active" = active ]   || { echo "服务没在运行" >&2; exit 1; }

unit=$(systemctl cat dsh-gate)
echo "$unit" | grep -E "^(ExecStart|WorkingDirectory|User)=" | sed "s/^/   /"

# 单元里的命令与 --workspace 必须同时出现：前者是网关进程怎么起，
# 后者是 dsh 的会话工作目录。
echo "$unit" | grep -q "ExecStart=.*--workspace /srv" || {
  echo "单元的 ExecStart 里没有 --workspace /srv" >&2; exit 1; }
echo "$unit" | grep -qE "^WorkingDirectory=/srv" || {
  echo "单元的 WorkingDirectory 不是 /srv（应为 setup 转发的工作目录）" >&2; exit 1; }
'

echo "== 服务进程与端口"
docker exec "$name" bash -c '
set -e
sleep 3
pid=$(systemctl show -p MainPID --value dsh-gate)
[ "$pid" != 0 ] || { echo "没有主进程" >&2; exit 1; }
echo "   主进程 ${pid}"
cwd=$(readlink /proc/$pid/cwd)
echo "   网关进程 cwd = ${cwd}"
[ "$cwd" = /srv ] || { echo "网关进程 cwd 不是 /srv" >&2; exit 1; }
# 登录页能打开（dsh 还没就绪也没关系，网关先监听）
code=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:13181/)
echo "   网关首页 HTTP ${code}"
[ "$code" = 303 ] || [ "$code" = 200 ] || { echo "首页没响应" >&2; exit 1; }
# journal 里应该有网关的日志
journalctl -u dsh-gate --no-pager -n 5 2>/dev/null | grep -q . || { echo "journal 没有日志" >&2; exit 1; }
echo "   journal 有日志 ✓"
'

echo "== service status"
docker exec "$name" dsh-gate service status 2>&1 | sed -n "1,6p" | sed "s/^/   /"

echo "== restart 之后仍在运行"
docker exec "$name" bash -c '
set -e
systemctl restart dsh-gate
sleep 3
systemctl is-active dsh-gate
dsh-gate status | head -3
'

echo "== service uninstall（应停服务并删单元）"
docker exec "$name" bash -c '
set -e
dsh-gate service uninstall
if systemctl list-unit-files 2>/dev/null | grep -q "^dsh-gate.service"; then
  echo "单元文件还在" >&2; exit 1
fi
echo "   单元已删除 ✓"
'

echo
echo "全部通过。"