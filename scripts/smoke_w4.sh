#!/bin/sh
# smoke_w4.sh 自更新黑盒冒烟（M3-W4 设计 §7 L3-16，CI 三平台矩阵）。
#
# 真二进制升级演练：编译 v1.0.0 与 v1.0.1 两版 ghydra（公钥 ldflags 注入
# 测试钥匙）→ releasesrv fake Release server → 场景：
#   A 正常链：check 可更新 → apply → version 跳 1.0.1 → .old 凭证在场 →
#             状态 confirmed
#   B 回滚链：update rollback → version 回 1.0.0 → bad_version 生效
#   C 投毒链：资产签名后篡改 → apply 拒 + 安装零破坏
#   D daemon 对账：serve 在跑 → apply → 新 pid + /api/status 版本跳变
set -e
set -x   # CI 失败考古：全命令留痕（tee 进 job 日志/失败分支）
cd "$(dirname "$0")/.." || exit 1
ROOT=$PWD
export HOME="${HOME:-/root}"   # W2 教训：GOCACHE 依赖 HOME

BIN=$(mktemp -d /tmp/w4bin.XXXXXX)
D=$(mktemp -d /tmp/w4data.XXXXXX)   # 安装目录 + HOME 沙箱（BIN/D 分离，W2p3 教训）
PUB="$ROOT/engine/selfupdate/testdata/test.pub"
KEY="$ROOT/engine/selfupdate/testdata/test.key"
PORT=19711
PORT_POISON=19712
SRV_PID=""

cleanup() {
  set +e   # set -e 下 kill 对死 pid 返回非零会中断 trap（实证：残留 serve + exit 1）
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null
  for p in $(cat "$D/pids" 2>/dev/null); do kill "$p" 2>/dev/null; done
  # 兜底：rollback/spawn 路径的 detached serve（pid 可能未记录）按特征杀
  pkill -9 -f "$D/" 2>/dev/null
  [ -n "$D2" ] && pkill -9 -f "$D2/" 2>/dev/null
  pkill -9 -f "ghydra serve --listen 127.0.0.1:1971" 2>/dev/null
  rm -rf "$BIN" "$D"
}
trap cleanup EXIT INT TERM

fail() { echo "SMOKE-W4 FAIL: $1" >&2; exit 1; }
ok()   { echo "PASS: $1"; }


# 两版真二进制（win 下 go 输出 .exe 由 GOOS 自动处理；bash on windows）
EXT=""
[ "$(go env GOOS)" = "windows" ] && EXT=".exe"

echo "== 构建 v1.0.0 / v1.0.1 =="
go build -ldflags "-X main.Version=1.0.0 -X main.pubKeyOverrideFile=$PUB" \
  -o "$D/ghydra$EXT" ./engine/cmd/ghydra || fail "构建 v1.0.0"
go build -ldflags "-X main.Version=1.0.1 -X main.pubKeyOverrideFile=$PUB" \
  -o "$BIN/ghydra-new$EXT" ./engine/cmd/ghydra || fail "构建 v1.0.1"
go build -o "$BIN/releasesrv" ./scripts/releasesrv || fail "构建 releasesrv"

API="http://127.0.0.1:$PORT/repos/xueweijian/ghydra"
COMMON="--api $API --trust-host 127.0.0.1 --cdn \"\" --db \"\""
run() { HOME="$D" "$D/ghydra$EXT" "$@"; }

echo "== 场景 A：正常升级链 =="
"$BIN/releasesrv" -listen "127.0.0.1:$PORT" -newbin "$BIN/ghydra-new$EXT" -version v1.0.1 -key "$KEY" >"$D/srv.log" 2>&1 &
SRV_PID=$!
for i in $(seq 1 50); do
  curl -s -o /dev/null "http://127.0.0.1:$PORT/files/checksums.txt" && break
  sleep 0.2
done
curl -s -o /dev/null "http://127.0.0.1:$PORT/files/checksums.txt" || { cat "$D/srv.log"; fail "releasesrv 未就绪"; }

[ "$(run version)" = "ghydra version 1.0.0" ] || fail "初始版本"

run update check --api "$API" --trust-host 127.0.0.1 --cdn "" --db "" | grep -q "可更新: v1.0.1" || fail "check 应报可更新"

run update --api "$API" --trust-host 127.0.0.1 --cdn "" --db "" >"$D/apply.log" 2>&1 || { cat "$D/apply.log"; fail "apply 失败"; }
grep -q "已更新到 v1.0.1" "$D/apply.log" || fail "apply 应成功"
[ "$(run version)" = "ghydra version 1.0.1" ] || fail "升级后版本"
[ -f "$D/ghydra$EXT.old" ] || fail "old 凭证应在场"
grep -q '"confirmed":true' "$D/.ghydra/update.json" || fail "状态应 confirmed"
ok "A 正常链（1.0.0 → 1.0.1，凭证+状态齐）"

echo "== 场景 B：回滚链 + bad_version =="
run update rollback --db "" >"$D/rb.log" 2>&1 || { cat "$D/rb.log"; fail "rollback 失败"; }
[ "$(run version)" = "ghydra version 1.0.0" ] || fail "回滚后版本"
run update check --api "$API" --trust-host 127.0.0.1 --cdn "" --db "" | grep -q "曾启动失败被回滚" || fail "bad_version 应生效"
ok "B 回滚链（1.0.1 → 1.0.0，bad_version 记忆）"

echo "== 场景 C：资产投毒拒 =="
D2=$(mktemp -d /tmp/w4c.XXXXXX)
cp "$D/ghydra$EXT" "$D2/ghydra$EXT" 2>/dev/null || cp "$D/ghydra$EXT.old" "$D2/ghydra$EXT"
# 用未升级的 1.0.0 + 干净状态
cat > "$D2/mk.sh" <<'EOS'
EOS
rm -f "$D2/.ghydra/update.json"
"$BIN/releasesrv" -listen "127.0.0.1:$PORT_POISON" -newbin "$BIN/ghydra-new$EXT" -version v1.0.1 -key "$KEY" -tamper-asset >"$D2/srv.log" 2>&1 &
POISON_PID=$!
for i in $(seq 1 50); do
  curl -s -o /dev/null "http://127.0.0.1:$PORT_POISON/files/checksums.txt" && break
  sleep 0.2
done
HOME="$D2" "$D2/ghydra$EXT" update --api "http://127.0.0.1:$PORT_POISON/repos/xueweijian/ghydra" --trust-host 127.0.0.1 --cdn "" --db "" >"$D2/apply.log" 2>&1 \
  && fail "投毒 apply 应失败" || true
grep -q "校验失败\|ErrChecksumMismatch\|sha256" "$D2/apply.log" || { cat "$D2/apply.log"; fail "应报 sha256 拒"; }
[ "$(HOME="$D2" "$D2/ghydra$EXT" version)" = "ghydra version 1.0.0" ] || fail "投毒拒后版本零破坏"
[ ! -f "$D2/ghydra$EXT.old" ] || fail "投毒拒不应产生 old 凭证"
kill $POISON_PID 2>/dev/null
rm -rf "$D2"
ok "C 投毒链（资产篡改 → sha256 拒 → 零破坏）"

echo "== 场景 D：daemon 对账 =="
SERVE_PORT=19713
HOME="$D" "$D/ghydra$EXT" serve --listen "127.0.0.1:$SERVE_PORT" --db "$D/db.sqlite" --managed >"$D/serve.log" 2>&1 &
SERVE_PID=$!
echo "$SERVE_PID" >> "$D/pids"
for i in $(seq 1 40); do grep -q "PAC: http" "$D/serve.log" 2>/dev/null && break; sleep 0.25; done
ACT_PORT=$(grep -o "PAC: http://127.0.0.1:[0-9]*" "$D/serve.log" | head -1 | grep -o "[0-9]*$")
[ -n "$ACT_PORT" ] || { cat "$D/serve.log"; fail "serve 未就绪"; }
SERVE_PORT=$ACT_PORT
# serve 不自写 serve.json（on 才写）——冒烟手工构造运行态
printf '{"pid":%s,"port":%d,"started_at":%d}\n' "$SERVE_PID" "$SERVE_PORT" "$(date +%s)" > "$D/.ghydra/serve.json"

# 状态复位：bad_version 清掉（B 场景标记过 1.0.1）——bad_version 是防重装
# 语义，daemon 对账演练要真实升级，等价于等待更高版本，这里直接演练
# rollback 后再升（bad 拦截已在 B 验证）→ 用 --allow-downgrade 不适用
# （1.0.1 > 1.0.0 是升级，被 bad 拦）→ 删状态演练
rm -f "$D/.ghydra/update.json"

run update --api "$API" --trust-host 127.0.0.1 --cdn "" --db "$D/db.sqlite" >"$D/apply2.log" 2>&1 \
  || { cat "$D/apply2.log"; cat "$D/serve.log"; fail "daemon 场景 apply 失败"; }
NEW_PID=$(grep -o '"pid":[0-9]*' "$D/.ghydra/serve.json" | head -1 | cut -d: -f2)
[ "$NEW_PID" != "$SERVE_PID" ] || fail "daemon 应换 pid（旧 $SERVE_PID → 新 $NEW_PID）"
TOKEN=$(cat "$D/.ghydra/api-token")
V=$(curl -s -H "X-GHydra-Token: $TOKEN" "http://127.0.0.1:$SERVE_PORT/api/status" | grep -o '"version":"[^"]*"' | head -1 | cut -d'"' -f4)
[ "$V" = "1.0.1" ] || fail "新 daemon 版本应为 1.0.1（得 $V）"
echo "$NEW_PID" >> "$D/pids"
ok "D daemon 对账（serve 换 pid + status 版本 1.0.1）"

echo
echo "SMOKE-W4 ALL PASS"
