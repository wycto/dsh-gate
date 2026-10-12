#!/bin/sh
# 接管场景的端到端验收：dsh 已经在跑，网关后来才接管它，拿不到启动令牌。
#
# 复现的是用户报的真实现场（macOS 本机、3080 上已有 dsh、装完自启后访问报
# 「DSH 要求重新登录」）：
#   1. 网关第一次跑起来，dsh 是它拉起的 —— 一切正常；
#   2. 网关进程被杀掉，dsh 变成「孤儿」继续在跑（用户机器上很常见：换了个进程/
#      装了开机自启，旧 dsh 还留在那儿）；
#   3. 新网关接管这个孤儿 dsh，但它不知道这次启动的令牌（令牌随机生成、只在 dsh
#      启动时打印一次、不落盘）——浏览器就会看到那一页；
#   4. 按页面上说的第二条路恢复：停掉外面那个 dsh、重启网关让它自己拉起 dsh。
#
# 这个脚本要验证两件事：
#   - 那一页**看得懂**：详情里不能是压缩字节（曾经显示成 `? K)?P(OMRH,-` 这种乱码）；
#   - 页面给的恢复步骤**真的能修好**（不是只给一句安慰话）。
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

name="dsh-gate-takeover-$$"
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

echo "== 起容器（干净系统，没有 Node）"
docker run -d --name "$name" -v "$root/$pkg:/tmp/pkg.tar.gz:ro" ubuntu:24.04 sleep infinity >/dev/null
docker exec "$name" sh -c 'command -v node >/dev/null 2>&1 && echo 有 Node || echo "确认：没有 Node"'
docker exec "$name" sh -c 'apt-get update -qq >/dev/null 2>&1; apt-get install -y -qq curl ca-certificates procps psmisc >/dev/null 2>&1' || true

docker exec "$name" sh -c "cd /tmp && tar -xzf pkg.tar.gz 2>/dev/null && install -m755 ${dir}/dsh-gate /usr/local/bin/dsh-gate &&
  mkdir -p /srv/state && dsh-gate user add admin --random --state-dir /srv/state >/dev/null && dsh-gate version"

# 固定口令，脚本后面要登录用。--password-stdin 只读一行（没有二次确认那一套），
# user add 是「没有就建、有就覆盖」，一条命令就够。
PASSWORD="TakeoverTest123"
docker exec "$name" sh -c "printf '%s\n' '$PASSWORD' | dsh-gate user add admin --password-stdin --users /srv/state/users.json >/dev/null"

echo "== 第一次启动网关（dsh 由它拉起，一切正常）"
docker exec -d "$name" sh -c 'cd /srv && dsh-gate run --background --name t1 --port 13381 \
  --dsh-port 13380 --dsh-home /srv/dsh --state-dir /srv/state --workspace /srv >/dev/null 2>&1'

i=0
while [ $i -lt 48 ]; do
  docker exec "$name" sh -c 'grep -q "dsh 已就绪" /root/.dsh-gate/run/logs/t1.log 2>/dev/null' && break
  i=$((i + 1)); sleep 10
done
docker exec "$name" sh -c 'grep -q "dsh 已就绪" /root/.dsh-gate/run/logs/t1.log' || {
  echo "第一次启动没能让 dsh 就绪，日志尾部：" >&2
  docker exec "$name" tail -5 /root/.dsh-gate/run/logs/t1.log >&2
  exit 1
}
echo "   dsh 已就绪 ✓（此时网关手上有令牌）"

echo "== 杀掉网关进程，留下孤儿 dsh（模拟用户现场的成因）"
docker exec "$name" sh -c 'pkill -9 -f "[d]sh-gate run" || true'
sleep 2
docker exec "$name" sh -c 'pgrep -f "[d]sh web" >/dev/null && echo "   dsh 还活着（孤儿）✓" || { echo "dsh 也死了，场景没搭起来" >&2; exit 1; }'

echo "== 新网关接管这个孤儿 dsh（拿不到令牌）"
docker exec -d "$name" sh -c 'cd /srv && dsh-gate run --background --name t2 --port 13391 \
  --dsh-port 13380 --dsh-home /srv/dsh --state-dir /srv/state --workspace /srv >/dev/null 2>&1'
sleep 8
# 注意看的是**日志里的接管提示**而不是启动横幅：横幅在 dsh 接管完成之前就打完了
# （实测如此），所以 NeedsToken 那段在首次启动时基本不会出现在横幅里。这条提示由
# supervisor 在接管成功那一刻写出，日志/ journal 里能看到。
docker exec "$name" sh -c 'grep -q "没有已知启动令牌" /root/.dsh-gate/run/logs/t2.log &&
  echo "   日志里给出了令牌提示 ✓" || { echo "   日志里没有令牌提示" >&2; grep -c . /root/.dsh-gate/run/logs/t2.log; exit 1; }'

echo "== 浏览器访问：应当是「DSH 要求重新登录」，且详情里没有乱码"
docker exec "$name" bash -c '
  set -e
  J=/tmp/jar2; rm -f $J /tmp/page.html
  # 登录网关
  curl -s -o /dev/null -c $J -b $J -d "username=admin&password='"$PASSWORD"'" \
    http://127.0.0.1:13391/__dsh_gate/login
  # 页面导航要走两跳：第一跳网关补令牌、dsh 返回 401 并清掉标记；第二跳才落到说明页
  for i in 1 2 3; do
    curl -s -o /tmp/page.html -c $J -b $J -H "Sec-Fetch-Mode: navigate" \
      http://127.0.0.1:13391/ >/dev/null || true
  done
  grep -q "DSH 要求重新登录" /tmp/page.html || {
    echo "   没看到重新登录页面，页面开头：" >&2
    head -c 300 /tmp/page.html >&2
    exit 1
  }
  echo "   页面标题正确 ✓"

  # 详情里不能有 gzip 魔数（1f 8b）：曾经把压缩字节直接塞进页面，显示成
  # ? K)?P(OMRH,- 那种乱码。这里直接按字节找，不依赖 python。
  if LC_ALL=C grep -q "$(printf "\037\213")" /tmp/page.html; then
    echo "   详情里有 gzip 字节，页面又在显示乱码" >&2
    exit 1
  fi
  # 替换字符（EF BF BD）：按字节截断汉字时会切出这个
  if LC_ALL=C grep -q "$(printf "\357\277\275")" /tmp/page.html; then
    echo "   详情里出现了替换字符，说明截断切在半个汉字上" >&2
    exit 1
  fi
  echo "   详情无 gzip 字节、无替换字符 ✓"
  # 页面必须给出「让网关自己管 dsh」这条一定可行的路，而不只是让人去找旧地址
  grep -q "stop\|停掉\|pkill\|重启网关" /tmp/page.html || {
    echo "   页面没有给出可执行的恢复步骤" >&2
    exit 1
  }
  echo "   页面给出了可执行的恢复步骤 ✓"
'

echo "== 按页面第二条路恢复：停掉外面的 dsh，重启网关让它自己拉起"
docker exec "$name" sh -c '
  # [d] 是必需的：pkill/pgrep 的模式就写在命令行里，不加会匹配到自己，
  # 于是「停掉 dsh」这一步会把执行它的那个 shell 也一起杀掉（实测退出码 137）。
  pkill -f "[d]sh web" || true
  sleep 2
  pgrep -f "[d]sh web" >/dev/null && { echo "旧 dsh 没停掉" >&2; exit 1; }
  echo "   旧 dsh 已停 ✓"
'
docker exec "$name" sh -c 'cd /srv && dsh-gate run --background --name t3 --port 13401 \
  --dsh-port 13380 --dsh-home /srv/dsh --state-dir /srv/state --workspace /srv >/dev/null 2>&1'
i=0
while [ $i -lt 48 ]; do
  docker exec "$name" sh -c 'grep -q "dsh 已就绪" /root/.dsh-gate/run/logs/t3.log 2>/dev/null' && break
  i=$((i + 1)); sleep 10
done
docker exec "$name" sh -c 'grep -q "dsh 已就绪" /root/.dsh-gate/run/logs/t3.log' || {
  echo "恢复后 dsh 没能就绪" >&2
  docker exec "$name" tail -5 /root/.dsh-gate/run/logs/t3.log >&2
  exit 1
}

echo "== 再次访问：应当能进 dsh（200，且拿到首页）"
docker exec "$name" bash -c '
  set -e
  J=/tmp/jar-r; rm -f $J
  curl -s -o /dev/null -c $J -b $J -d "username=admin&password='"$PASSWORD"'" \
    http://127.0.0.1:13401/__dsh_gate/login
  first=$(curl -s -o /dev/null -w "%{http_code}" -c $J -b $J -H "Sec-Fetch-Mode: navigate" http://127.0.0.1:13401/)
  second=$(curl -s -o /dev/null -w "%{http_code}" -c $J -b $J -H "Sec-Fetch-Mode: navigate" http://127.0.0.1:13401/)
  echo "   首次 / → ${first}，再次 / → $second"
  [ "$second" = 200 ] || { echo "恢复后仍然进不去" >&2; exit 1; }
  curl -s -c $J -b $J http://127.0.0.1:13401/ | grep -q "DeepSeek" || { echo "没拿到 dsh 首页" >&2; exit 1; }
  echo "   ✓ 恢复步骤真的能修好"
'

echo
echo "全部通过。"
