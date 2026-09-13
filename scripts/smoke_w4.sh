#!/bin/sh
# smoke_w4.sh 自更新黑盒冒烟（M3-W4 设计 §7 L3-16）。
#
# 真二进制升级演练：编译 v1.0.0 与 v1.0.1 两版 ghydra（公钥 ldflags 注入
# 测试钥匙）→ releasesrv fake Release server → 四场景：
#   A 正常链：check 可更新 → apply → version 跳 1.0.1 → .old 凭证 + confirmed
#   B 回滚链：rollback → 回 1.0.0 → bad_version 生效
#   C 投毒链：资产签名后篡改 → apply 拒 + 安装零破坏
#   D daemon 对账：serve 在跑 → apply → 换 pid + /api/status 版本跳变
#
# 子命令模式（CI 每场景独立 step——失败步骤名即定位，无需日志权限）：
#   setup | scenario-a | scenario-b | scenario-c | scenario-d | teardown
# 跨 step 状态经 /tmp/w4state.env（同一 job 的 VM 内进程与 /tmp 共享）。
# 全量单步跑法（本地）：sh scripts/smoke_w4.sh all
set -e
cd "$(dirname "$0")/.." || exit 1
ROOT=$PWD
export HOME="${HOME:-/root}"   # W2 教训：GOCACHE 依赖 HOME

STATE=/tmp/w4state.env
PORT=19711
PORT_POISON=19712

fail() {
  echo "SMOKE-W4 FAIL: $1" >&2
  dump_on_fail || true   # 现场转储（CI 考古；本地无害）
  exit 1
}
ok() { echo "PASS: $1"; }

# dump_on_fail 把场景日志提交到 ci-failure-log（凭据 = checkout 注入的
# credential helper；尽力而为）。四轮 job 级推送未落地后的脚本级通道。
dump_on_fail() {
  [ -n "$CI" ] || return 0
  set +e
  NAME="smoke-w4-fail-${RUNNER_OS:-local}.txt"
  OUT="$ROOT/$NAME"
  { echo "== smoke_w4 fail: $1 =="
    cat "$STATE" 2>/dev/null
    for f in apply.log apply2.log serve.log srv.log rb.log; do
      echo "==== $f ===="
      tail -50 "$D/$f" 2>/dev/null
      tail -50 /tmp/w4c.*/"$f" 2>/dev/null
    done } > "$OUT" 2>&1
  cd "$ROOT" || return 0
  git config user.email "ci@ghydra.local"
  git config user.name "ci-bot"
  git checkout -B ci-failure-log 2>>"$OUT"
  git add -f "$NAME" 2>>"$OUT"
  git commit -m "smoke_w4 fail dump ($RUNNER_OS)" 2>>"$OUT"
  git push -f origin ci-failure-log >>"$OUT" 2>&1
  cd - >/dev/null
  # push 结果本身也写进文件——若下次 push 成功即可读成败记录
  rm -f "$ROOT/$NAME"
  return 0
}

load_state() { . "$STATE"; }
# exe 侧 HOME 必须是原生路径：git-bash 的 /tmp 与 Windows 进程视角
# （C:\tmp\...）分叉会让状态/断言两边各写各的（CI windows 实证）。
home_native() {
  if [ -n "$D_NATIVE" ]; then echo "$D_NATIVE"; else echo "$D"; fi
}
run()  { HOME="$(home_native)" "$D/ghydra$EXT" "$@"; }

cmd_setup() {
  BIN=$(mktemp -d /tmp/w4bin.XXXXXX)
  D=$(mktemp -d /tmp/w4data.XXXXXX)   # 安装目录 + HOME 沙箱（BIN/D 分离）
  PUB="$ROOT/engine/selfupdate/testdata/test.pub"
  KEY="$ROOT/engine/selfupdate/testdata/test.key"
  EXT=""
  [ "$(go env GOOS)" = "windows" ] && EXT=".exe"

  echo "== 构建 v1.0.0 / v1.0.1 / releasesrv =="
  go build -ldflags "-X main.Version=1.0.0 -X main.pubKeyOverrideFile=$PUB" \
    -o "$D/ghydra$EXT" ./engine/cmd/ghydra || fail "构建 v1.0.0"
  go build -ldflags "-X main.Version=1.0.1 -X main.pubKeyOverrideFile=$PUB" \
    -o "$BIN/ghydra-new$EXT" ./engine/cmd/ghydra || fail "构建 v1.0.1"
  go build -o "$BIN/releasesrv$EXT" ./scripts/releasesrv || fail "构建 releasesrv"

  "$BIN/releasesrv$EXT" -listen "127.0.0.1:$PORT" -newbin "$BIN/ghydra-new$EXT" \
    -version v1.0.1 -key "$KEY" >"$D/srv.log" 2>&1 &
  SRV_PID=$!
  for _ in $(seq 1 50); do
    curl -s -o /dev/null "http://127.0.0.1:$PORT/files/checksums.txt" && break
    sleep 0.2
  done
  curl -s -o /dev/null "http://127.0.0.1:$PORT/files/checksums.txt" || { cat "$D/srv.log"; fail "releasesrv 未就绪"; }

  cat > "$STATE" <<EOF
ROOT='$ROOT'
BIN='$BIN'
D='$D'
PUB='$PUB'
KEY='$KEY'
EXT='$EXT'
PORT=$PORT
PORT_POISON=$PORT_POISON
SRV_PID=$SRV_PID
EOF
  [ "$(run version)" = "ghydra version 1.0.0" ] || fail "初始版本应为 1.0.0"

  # Windows：转原生路径（pwd -W → C:/...），exe 侧 HOME 用
  D_NATIVE="$D"
  if [ "$(go env GOOS)" = "windows" ]; then
    D_NATIVE=$(cd "$D" && pwd -W 2>/dev/null || echo "$D")
  fi

  cat > "$STATE" <<EOF
ROOT='$ROOT'
BIN='$BIN'
D='$D'
D_NATIVE='$D_NATIVE'
PUB='$PUB'
KEY='$KEY'
EXT='$EXT'
PORT=$PORT
PORT_POISON=$PORT_POISON
SRV_PID=$SRV_PID
EOF
  ok "setup（双版本 + releasesrv 就绪，初始 1.0.0）"
}

cmd_scenario_a() {
  load_state
  API="http://127.0.0.1:$PORT/repos/xueweijian/ghydra"
  run update check --api "$API" --trust-host 127.0.0.1 --cdn "" --db "" | grep -q "可更新: v1.0.1" \
    || { echo "--- check 输出 ---"; run update check --api "$API" --trust-host 127.0.0.1 --cdn "" --db "" 2>&1; fail "check 应报可更新"; }
  run update --api "$API" --trust-host 127.0.0.1 --cdn "" --db "" >"$D/apply.log" 2>&1 \
    || { cat "$D/apply.log"; cat "$D/srv.log"; fail "apply 失败"; }
  grep -q "已更新到 v1.0.1" "$D/apply.log" || { cat "$D/apply.log"; fail "apply 应成功"; }
  [ "$(run version)" = "ghydra version 1.0.1" ] || fail "升级后版本应 1.0.1（得 $(run version 2>&1)）"
  [ -f "$D/ghydra$EXT.old" ] || fail "old 凭证应在场"
  grep -q '"confirmed":true' "$D/.ghydra/update.json" || fail "状态应 confirmed"
  ok "A 正常链（1.0.0 → 1.0.1，凭证+状态齐）"
}

cmd_scenario_b() {
  load_state
  API="http://127.0.0.1:$PORT/repos/xueweijian/ghydra"
  run update rollback --db "" >"$D/rb.log" 2>&1 || { cat "$D/rb.log"; fail "rollback 失败"; }
  [ "$(run version)" = "ghydra version 1.0.0" ] || fail "回滚后版本应 1.0.0"
  run update check --api "$API" --trust-host 127.0.0.1 --cdn "" --db "" 2>&1 | grep -q "曾启动失败被回滚" \
    || fail "bad_version 应生效"
  ok "B 回滚链（1.0.1 → 1.0.0，bad_version 记忆）"
}

cmd_scenario_c() {
  load_state
  D2=$(mktemp -d /tmp/w4c.XXXXXX)
  # B 回滚后主位已是 1.0.0（RollbackSwap: old→exe, exe→.bad）
  cp "$D/ghydra$EXT" "$D2/ghydra$EXT"
  [ "$(HOME="$D2" "$D2/ghydra$EXT" version)" = "ghydra version 1.0.0" ] || fail "C 前置：主位应仍为 1.0.0"
  "$BIN/releasesrv$EXT" -listen "127.0.0.1:$PORT_POISON" -newbin "$BIN/ghydra-new$EXT" \
    -version v1.0.1 -key "$KEY" -tamper-asset >"$D2/srv.log" 2>&1 &
  P_PID=$!
  for _ in $(seq 1 50); do
    curl -s -o /dev/null "http://127.0.0.1:$PORT_POISON/files/checksums.txt" && break
    sleep 0.2
  done
  HOME="$D2" "$D2/ghydra$EXT" update --api "http://127.0.0.1:$PORT_POISON/repos/xueweijian/ghydra" \
    --trust-host 127.0.0.1 --cdn "" --db "" >"$D2/apply.log" 2>&1 && fail "投毒 apply 应失败" || true
  { grep -q "校验失败" "$D2/apply.log" || grep -q "sha256" "$D2/apply.log"; } \
    || { cat "$D2/apply.log"; fail "应报 sha256 拒"; }
  [ "$(HOME="$D2" "$D2/ghydra$EXT" version)" = "ghydra version 1.0.0" ] || fail "投毒拒后版本零破坏"
  [ ! -f "$D2/ghydra$EXT.old" ] || fail "投毒拒不应产生 old 凭证"
  kill $P_PID 2>/dev/null || true
  rm -rf "$D2"
  ok "C 投毒链（资产篡改 → sha256 拒 → 零破坏）"
}

cmd_scenario_d() {
  load_state
  API="http://127.0.0.1:$PORT/repos/xueweijian/ghydra"
  SERVE_PORT=19713
  HOME="$(home_native)" "$D/ghydra$EXT" serve --listen "127.0.0.1:$SERVE_PORT" --db "$D/db.sqlite" --managed >"$D/serve.log" 2>&1 &
  SERVE_PID=$!
  for _ in $(seq 1 40); do grep -q "PAC: http" "$D/serve.log" 2>/dev/null && break; sleep 0.25; done
  ACT_PORT=$(grep -o "PAC: http://127.0.0.1:[0-9]*" "$D/serve.log" | head -1 | grep -o "[0-9]*$")
  [ -n "$ACT_PORT" ] || { cat "$D/serve.log"; fail "serve 未就绪"; }
  SERVE_PORT=$ACT_PORT
  # serve 不自写 serve.json（on 才写）——冒烟手工构造运行态
  mkdir -p "$D/.ghydra"
  printf '{"pid":%s,"port":%d,"started_at":%d}\n' "$SERVE_PID" "$SERVE_PORT" "$(date +%s)" > "$D/.ghydra/serve.json"
  # 状态复位（B 标记过 bad 1.0.1；bad 拦截语义已在 B 验证）
  rm -f "$D/.ghydra/update.json"

  run update --api "$API" --trust-host 127.0.0.1 --cdn "" --db "$D/db.sqlite" >"$D/apply2.log" 2>&1 \
    || { cat "$D/apply2.log"; cat "$D/serve.log"; fail "daemon 场景 apply 失败"; }
  NEW_PID=$(grep -o '"pid":[0-9]*' "$D/.ghydra/serve.json" | head -1 | cut -d: -f2)
  [ "$NEW_PID" != "$SERVE_PID" ] || fail "daemon 应换 pid（旧 $SERVE_PID → 新 $NEW_PID）"
  TOKEN=$(cat "$D/.ghydra/api-token")
  V=$(curl -s -H "X-GHydra-Token: $TOKEN" "http://127.0.0.1:$SERVE_PORT/api/status" | grep -o '"version":"[^"]*"' | head -1 | cut -d'"' -f4)
  [ "$V" = "1.0.1" ] || fail "新 daemon 版本应为 1.0.1（得 $V）"
  kill "$NEW_PID" 2>/dev/null || true
  ok "D daemon 对账（serve 换 pid + status 版本 1.0.1）"
}

cmd_teardown() {
  # 幂等：任一步失败后的 always 兜底
  set +e
  [ -f "$STATE" ] && . "$STATE"
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null
  pkill -9 -f "w4data." 2>/dev/null
  pkill -9 -f "w4bin." 2>/dev/null
  pkill -9 -f "w4c." 2>/dev/null
  pkill -9 -f "ghydra serve --listen 127.0.0.1:1971" 2>/dev/null
  rm -rf "$BIN" "$D" "$STATE" /tmp/w4c.* 2>/dev/null
  echo "teardown 完成"
}

case "${1:-all}" in
  setup) cmd_setup ;;
  scenario-a) cmd_scenario_a ;;
  scenario-b) cmd_scenario_b ;;
  scenario-c) cmd_scenario_c ;;
  scenario-d) cmd_scenario_d ;;
  teardown) cmd_teardown ;;
  all)
    cmd_setup
    cmd_scenario_a
    cmd_scenario_b
    cmd_scenario_c
    cmd_scenario_d
    cmd_teardown
    echo
    echo "SMOKE-W4 ALL PASS"
    ;;
  *) echo "用法: $0 setup|scenario-a|...|scenario-d|teardown|all" >&2; exit 2 ;;
esac
