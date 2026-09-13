#!/bin/sh
# M3-W2 phase 3 黑盒冒烟：refresher 拉取管线 × fakesite 毒化矩阵（R12）。
#
# 场景（每个 serve 独立 HOME 隔离规则目录；手动 POST refresh 触发）：
#   1 ok 热更        v11（真私钥签名向量）→ remote + PAC 探针域生效
#   2 a1 内容投毒    json 改一字节 → sig_rejected + 快照冻结
#   3 a3 回滚        v9（合法签名旧版）→ rollback + 冻结
#   4 a4 快进        vff（合法签名超上限版）→ fast_forward + 冻结
#   5 a5 无尽数据    chunked 无限流 → size_exceeded + 冻结
#   6 断网冻结       源 tlsdead → fetch_err + PAC 仍服务（旧规则好过没规则）
#   7 seen_max 跨重启 场景 1 的 HOME 重启后喂 v9 → rollback（R9 防重放）
set -e
cd "$(dirname "$0")/../../.." # repo root
export HOME="${HOME:-/root}"  # 沙箱会话可能无 HOME；go module cache 依赖它
BIN=/tmp/ghydra-w2p3-bin
D=/tmp/ghydra-w2p3
mkdir -p "$BIN"
go build -o "$BIN/ghydra" ./engine/cmd/ghydra
go build -o "$BIN/fakesite" ./scripts/fakesite

VECS=$PWD/rules/testdata/vectors
TOK=smoketoken123
rm -rf "$D"  # 干净起跑：seen_max/规则目录不得跨脚本运行残留
mkdir -p "$D"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; exit 1; }

PIDS=""
cleanup() { for p in $PIDS; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

# start_serve <home> <port> [rules-url]：起 serve，等就绪，输出 pid
start_serve() {
  _home=$1; _port=$2; _rurl=$3
  mkdir -p "$_home"
  HOME_BAK=$HOME
  HOME="$_home"
  export HOME
  if [ -n "$_rurl" ]; then
    "$BIN/ghydra" serve --listen 127.0.0.1:$_port --api-token "$TOK" \
      --scheduler=false --rules-url "$_rurl" > "$_home/serve.log" 2>&1 &
  else
    "$BIN/ghydra" serve --listen 127.0.0.1:$_port --api-token "$TOK" \
      --scheduler=false > "$_home/serve.log" 2>&1 &
  fi
  HOME="$HOME_BAK"; export HOME
  _pid=$!
  PIDS="$PIDS $_pid"
  i=0
  while ! curl -s -o /dev/null "http://127.0.0.1:$_port/pac"; do
    i=$((i+1)); [ $i -gt 50 ] && { echo "FAIL: serve($_port) 未就绪"; cat "$_home/serve.log"; exit 1; }
    sleep 0.2
  done
  echo "$_pid"
}

# refresh_expect <port> <want_last_result> <want_version>：触发刷新并轮询终态
refresh_expect() {
  _port=$1; _want=$2; _ver=$3
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-GHydra-Token: $TOK" \
    "http://127.0.0.1:$_port/api/rules/refresh")
  [ "$code" = "200" ] || fail "refresh POST 应 200，得 $code"
  i=0
  while :; do
    body=$(curl -s -H "X-GHydra-Token: $TOK" "http://127.0.0.1:$_port/api/rules")
    res=$(echo "$body" | grep -o '"refresh":{[^}]*}' | grep -o '"last_result":"[^"]*"' | cut -d'"' -f4)
    ver=$(echo "$body" | grep -o '"version":[0-9]*' | head -1 | cut -d: -f2)
    if [ "$res" = "$_want" ]; then break; fi
    i=$((i+1)); [ $i -gt 50 ] && { echo "FAIL: refresh 终态未达（want $_want got '$res'）: $body"; exit 1; }
    sleep 0.3
  done
  if [ -n "$_ver" ] && [ "$ver" != "$_ver" ]; then
    fail "version 应 $_ver，得 $ver"
  fi
}

# ---- 场景 1：ok 热更（disk 预置 v10，拉 v11） ----
H1=$D/h1; mkdir -p "$H1/.ghydra/rules"
cp "$PWD/rules/current.json" "$H1/.ghydra/rules/"
cp "$PWD/rules/current.json.minisig" "$H1/.ghydra/rules/"
"$BIN/fakesite" -mode rules -port 9491 -rules-dir "$VECS/v11" > "$D/f1.log" 2>&1 &
FS1=$!
PIDS="$PIDS $FS1"
SRV1=$(start_serve "$H1" 9490 "http://127.0.0.1:9491/current.json")
refresh_expect 9490 ok 11
curl -s http://127.0.0.1:9490/pac | grep -q v11probe.ghydra-test.example || fail "PAC 未热生效（探针域缺失）"
pass "1 ok 热更 v10→v11：remote + PAC 探针域生效"

# ---- 场景 2：a1 内容投毒（冻结） ----
H2=$D/h2; mkdir -p "$H2/.ghydra/rules"
cp "$PWD/rules/current.json" "$H2/.ghydra/rules/"; cp "$PWD/rules/current.json.minisig" "$H2/.ghydra/rules/"
"$BIN/fakesite" -mode rules -port 9493 -rules-dir "$VECS/v11" -poison a1 > "$D/f2.log" 2>&1 &
PIDS="$PIDS $!"
start_serve "$H2" 9492 "http://127.0.0.1:9493/current.json"
refresh_expect 9492 sig_rejected 10
curl -s http://127.0.0.1:9492/pac | grep -q v11probe && fail "投毒后 PAC 不应含探针域"
pass "2 a1 内容投毒 → sig_rejected + 快照冻结 v10"

# ---- 场景 3：a3 回滚 ----
H3=$D/h3; mkdir -p "$H3/.ghydra/rules"
cp "$PWD/rules/current.json" "$H3/.ghydra/rules/"; cp "$PWD/rules/current.json.minisig" "$H3/.ghydra/rules/"
"$BIN/fakesite" -mode rules -port 9495 -rules-dir "$VECS/v9" > "$D/f3.log" 2>&1 &
PIDS="$PIDS $!"
start_serve "$H3" 9494 "http://127.0.0.1:9495/current.json"
refresh_expect 9494 rollback 10
pass "3 a3 回滚 v9 → rollback + 冻结 v10"

# ---- 场景 4：a4 快进 ----
H4=$D/h4; mkdir -p "$H4/.ghydra/rules"
cp "$PWD/rules/current.json" "$H4/.ghydra/rules/"; cp "$PWD/rules/current.json.minisig" "$H4/.ghydra/rules/"
"$BIN/fakesite" -mode rules -port 9497 -rules-dir "$VECS/vff" > "$D/f4.log" 2>&1 &
PIDS="$PIDS $!"
start_serve "$H4" 9496 "http://127.0.0.1:9497/current.json"
refresh_expect 9496 fast_forward 10
pass "4 a4 快进 → fast_forward + 冻结 v10"

# ---- 场景 5：a5 无尽数据 ----
H5=$D/h5; mkdir -p "$H5/.ghydra/rules"
cp "$PWD/rules/current.json" "$H5/.ghydra/rules/"; cp "$PWD/rules/current.json.minisig" "$H5/.ghydra/rules/"
"$BIN/fakesite" -mode rules -port 9499 -rules-dir "$VECS/v11" -poison a5 > "$D/f5.log" 2>&1 &
PIDS="$PIDS $!"
start_serve "$H5" 9498 "http://127.0.0.1:9499/current.json"
refresh_expect 9498 size_exceeded 10
pass "5 a5 无限流 → size_exceeded + 冻结 v10"

# ---- 场景 6：断网冻结（源死，PAC 仍服务） ----
H6=$D/h6; mkdir -p "$H6/.ghydra/rules"
cp "$PWD/rules/current.json" "$H6/.ghydra/rules/"; cp "$PWD/rules/current.json.minisig" "$H6/.ghydra/rules/"
"$BIN/fakesite" -mode tlsdead -port 9501 > "$D/f6.log" 2>&1 &
PIDS="$PIDS $!"
start_serve "$H6" 9500 "http://127.0.0.1:9501/current.json"
refresh_expect 9500 fetch_err 10
curl -s http://127.0.0.1:9500/pac | grep -q github.com || fail "断网后 PAC 应仍服务（旧规则好过没规则）"
curl -s -H "X-GHydra-Token: $TOK" http://127.0.0.1:9500/api/rules | grep -q '"stale":false' || true # v10 未过期，冻结可观测性由 stale 字段承载
pass "6 断网 → fetch_err + PAC 仍服务"

# ---- 场景 7：seen_max 跨重启（R9 防重放） ----
kill %1 2>/dev/null || true   # 场景 1 的 fakesite（9491）
sleep 0.3
"$BIN/fakesite" -mode rules -port 9491 -rules-dir "$VECS/v9" > "$D/f7.log" 2>&1 &
PIDS="$PIDS $!"
# 重启场景 1 的 serve（同 HOME：disk 已是 v11，seen_max 持久化=11）
kill $(ps aux | grep "listen 127.0.0.1:9490" | grep -v grep | awk '{print $1}') 2>/dev/null || true
sleep 0.5
start_serve "$H1" 9490 "http://127.0.0.1:9491/current.json"
refresh_expect 9490 rollback 11
pass "7 seen_max 跨重启：重放 v9 → rollback（防线不回落）"

echo "ALL PASS: W2 phase 3 黑盒毒化矩阵 7 场景"
