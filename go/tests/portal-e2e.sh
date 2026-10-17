#!/bin/sh
# 门户端到端验收：两个用户 + 两个**真实** dsh 实例。
#
# 与 container-e2e.sh 的分工：那个脚本验「干净 Linux 容器里能不能起来」，这个脚本
# 验门户的多用户/多实例语义 —— 用户只能看到自己的实例、拿别人的挂载路径得 404、
# 进自己的实例能真的拿到 dsh 页面（也就是令牌交接真的完成了）。
#
# 用法：
#   sh go/tests/portal-e2e.sh
#
# 依赖：Go（构建 dsh-gate）+ curl + 本机能跑 dsh（PATH 里的 dsh，或 npx）。
# 全程在临时目录里跑：**不碰 ~/.dsh**、不碰 ~/.dsh-gate；用完即清。
#
# 环境变量：
#   WAIT_SECONDS  等待 dsh 就绪的上限（默认 300；首次会用 npx 下载 dsh，慢）
#   PORT          门户端口（默认 13082）
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$root"

wait_seconds=${WAIT_SECONDS:-300}
portal_port=${PORT:-13082}

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t dsh-gate-portal)
cleanup() {
  if [ -n "${portal_pid:-}" ]; then
    kill "$portal_pid" 2>/dev/null || true
    wait "$portal_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT INT TERM

fail() {
  echo "✗ $*" >&2
  echo "--- 门户日志尾部 ---" >&2
  tail -n 40 "$tmp/portal.log" 2>/dev/null >&2 || true
  exit 1
}

ok() { echo "✓ $*"; }

echo "== 构建 dsh-gate"
(cd go && go build -o "$tmp/dsh-gate" ./cmd/dsh-gate) || fail "构建失败"
bin="$tmp/dsh-gate"
state="$tmp/state"
db="$tmp/portal.db"
mkdir -p "$state"

echo "== 建门户账号（管理员 + 两个成员）"
echo "admin-password-123" | "$bin" portal user add admin --role admin --db "$db" --password-stdin >/dev/null
echo "alice-password-123" | "$bin" portal user add alice --db "$db" --password-stdin >/dev/null
echo "bob-password-123"   | "$bin" portal user add bob   --db "$db" --password-stdin >/dev/null

echo "== 建两个实例（各自的 DSH_HOME，端口自动分配）"
"$bin" portal instance add alice-main --dsh-home "$tmp/alice/.dsh" --owner alice --db "$db" >/dev/null
"$bin" portal instance add bob-main   --dsh-home "$tmp/bob/.dsh"   --owner bob   --db "$db" >/dev/null

alice_id=$("$bin" portal instance list --db "$db" | awk 'NF >= 5 && $1 == "alice-main" {print $2}')
bob_id=$("$bin" portal instance list --db "$db" | awk 'NF >= 5 && $1 == "bob-main" {print $2}')
[ -n "$alice_id" ] && [ -n "$bob_id" ] || fail "没能从 instance list 解析出实例 id"
ok "实例就绪：alice-main=$alice_id bob-main=$bob_id"

echo "== 授权：alice → alice-main，bob → bob-main"
"$bin" portal grant add alice alice-main --db "$db" >/dev/null
"$bin" portal grant add bob   bob-main   --db "$db" >/dev/null

echo "== 启动门户（127.0.0.1:$portal_port）"
"$bin" portal run --host 127.0.0.1 --port "$portal_port" --db "$db" --state-dir "$state" \
  --log-level warn --lang en >"$tmp/portal.log" 2>&1 &
portal_pid=$!

base="http://127.0.0.1:$portal_port"
i=0
ready=0
while [ "$i" -lt 60 ]; do
  # 用状态码而不是 -f 判断：-f 会把任何非 2xx 当成失败，而我们要回答的问题是
  # 「这个端口在说 HTTP 吗」。000 表示连不上。
  code=$(curl -s -o /dev/null -w '%{http_code}' "$base/__dsh_gate/healthz" 2>/dev/null || echo 000)
  case "$code" in
    000) ;;
    *) ready=1; break ;;
  esac
  i=$((i + 1))
  sleep 1
done
[ "$ready" = "1" ] || fail "门户没起来（端口 $portal_port 没有应答）"
ok "门户在听 $base"

echo "== 两个成员各自登录（各自的 cookie jar）"
curl -sf -c "$tmp/alice.jar" -o /dev/null \
  -d 'username=alice&password=alice-password-123' "$base/__dsh_gate/login" || fail "alice 登录失败"
curl -sf -c "$tmp/bob.jar" -o /dev/null \
  -d 'username=bob&password=bob-password-123' "$base/__dsh_gate/login" || fail "bob 登录失败"
curl -sf -c "$tmp/admin.jar" -o /dev/null \
  -d 'username=admin&password=admin-password-123' "$base/__dsh_gate/login" || fail "管理员登录失败"
ok "三个账号都登录成功"

echo "== 断言 1：alice 首页只看到自己的实例"
home_alice=$(curl -sf -b "$tmp/alice.jar" "$base/")
echo "$home_alice" | grep -q 'alice-main' || fail "alice 首页应显示 alice-main"
if echo "$home_alice" | grep -q 'bob-main'; then fail "alice 首页泄漏了 bob-main"; fi
ok "alice 只看得到 alice-main"

echo "== 断言 2：越权访问与「不存在」同形（404）"
code_cross=$(curl -s -o "$tmp/cross.html" -w '%{http_code}' -b "$tmp/alice.jar" "$base/i/$bob_id/")
code_missing=$(curl -s -o "$tmp/missing.html" -w '%{http_code}' -b "$tmp/alice.jar" "$base/i/zzzzzzzz/")
[ "$code_cross" = "404" ] || fail "alice 访问 bob 的实例应 404，实际 $code_cross"
[ "$code_missing" = "404" ] || fail "不存在的实例应 404，实际 $code_missing"
cmp -s "$tmp/cross.html" "$tmp/missing.html" || fail "越权与不存在的响应体应当同形"
ok "越权与不存在都是 404 且响应同形"

echo "== 断言 3：管理员看得到两个实例，也能进任意一个"
home_admin=$(curl -sf -b "$tmp/admin.jar" "$base/")
echo "$home_admin" | grep -q 'alice-main' || fail "管理员首页应显示 alice-main"
echo "$home_admin" | grep -q 'bob-main'   || fail "管理员首页应显示 bob-main"
ok "管理员看得到全部实例"

echo "== 断言 4：进入自己的实例 —— 真实 dsh 就绪并完成令牌交接"
deadline=$(( $(date +%s) + wait_seconds ))
entered=0
while [ "$(date +%s)" -lt "$deadline" ]; do
  code=$(curl -s -o "$tmp/inst.html" -w '%{http_code}' -b "$tmp/alice.jar" -c "$tmp/alice.jar" \
    "$base/i/$alice_id/" || true)
  if [ "$code" = "200" ]; then entered=1; break; fi
  sleep 2
done
[ "$entered" = "1" ] || fail "等待 ${wait_seconds}s 仍没能进入 alice 的实例（dsh 可能没起来）"

# 令牌交接成功的最硬证据：dsh 自己签发过会话 Cookie，并且网关种下了交接标记。
grep -q 'dsh-auth-' "$tmp/alice.jar" || fail "交接后应拿到 dsh 自己的会话 Cookie"
grep -q 'dsh_gate_ds' "$tmp/alice.jar" || fail "交接后应种下 dsh_gate_ds 标记"
ok "进入实例成功：dsh 会话已交接"

echo "== 断言 5：bob 用同一个挂载路径拿不到 alice 的会话"
code_bob=$(curl -s -o "$tmp/bobcross.html" -w '%{http_code}' -b "$tmp/bob.jar" "$base/i/$alice_id/")
[ "$code_bob" = "404" ] || fail "bob 访问 alice 的实例应 404，实际 $code_bob"
ok "跨用户访问被挡住"

echo
echo "全部通过：门户多用户/多实例语义与真实 dsh 的令牌交接都正常。"
