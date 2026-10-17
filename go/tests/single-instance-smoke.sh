#!/bin/sh
# 单实例形态回归冒烟：确认门户改造没有动到老的 `dsh-gate run` 行为。
#
# 与 portal-e2e.sh 对称：这个脚本走「一个网关 + 一个 dsh」那条老路，
# 验「登录 → 令牌交接 → 拿到 dsh 页面」仍然成立。
set -eu
root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t dsh-gate-single)
port=13091
dsh_port=13092
pid=""
cleanup() {
  [ -n "$pid" ] && { kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }
  rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

fail() { echo "✗ $*" >&2; tail -n 40 "$tmp/log" >&2 2>/dev/null || true; exit 1; }

echo "== 构建并建账号（单实例形态）"
(cd go && go build -o "$tmp/dsh-gate" ./cmd/dsh-gate) || fail "构建失败"
echo "single-password-123" | "$tmp/dsh-gate" user add admin --state-dir "$tmp/state" --password-stdin >/dev/null

echo "== 启动单实例网关（$port → dsh $dsh_port）"
# --run-dir 指到临时目录：运行登记是机器级的（默认 ~/.dsh-gate/run），冒烟不该在
# 用户的真实登记里留下一条指向临时进程的记录。
"$tmp/dsh-gate" run --host 127.0.0.1 --port "$port" --dsh-port "$dsh_port" \
  --dsh-home "$tmp/dsh" --state-dir "$tmp/state" --run-dir "$tmp/run" \
  --log-level warn --lang en >"$tmp/log" 2>&1 &
pid=$!

base="http://127.0.0.1:$port"
i=0
while [ "$i" -lt 60 ]; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$base/__dsh_gate/login" 2>/dev/null || echo 000)
  [ "$code" = "200" ] && break
  i=$((i + 1))
  sleep 1
done
[ "$(curl -s -o /dev/null -w '%{http_code}' "$base/__dsh_gate/login" || echo 000)" = "200" ] || fail "登录页没起来"
echo "✓ 登录页可用"

curl -sf -c "$tmp/jar" -o /dev/null -d 'username=admin&password=single-password-123' "$base/__dsh_gate/login" || fail "登录失败"
echo "✓ 登录成功"

deadline=$(( $(date +%s) + ${WAIT_SECONDS:-300} ))
entered=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  code=$(curl -s -o "$tmp/page.html" -w '%{http_code}' -b "$tmp/jar" -c "$tmp/jar" "$base/" || true)
  [ "$code" = "200" ] && { entered=1; break; }
  sleep 2
done
[ "$entered" = "1" ] || fail "等待 ${WAIT_SECONDS:-300}s 仍没拿到 dsh 页面"

grep -q 'dsh-auth-' "$tmp/jar" || fail "交接后应拿到 dsh 自己的会话 Cookie"
grep -q 'dsh_gate_ds' "$tmp/jar" || fail "交接后应种下 dsh_gate_ds 标记"
echo "✓ 单实例形态：登录 → 令牌交接 → 拿到 dsh 页面"
echo
echo "全部通过：老的单实例 run 行为未受影响。"
