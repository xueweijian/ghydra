#!/bin/sh
# smoke_w4p2_p4.sh —— P4c 黑盒冒烟：serve 内 apply 两段式全链 + config 持久化
# + version 提示行（W4p2 设计 §7 测试计划 L3 场景）。
#
# 与 smoke_w4.sh 的差异：w4 验证 CLI 外部编排（update apply 子命令），
# 本脚本验证 **GUI 壳依赖的真实生产路径**——
#   A serve 内 apply：POST /api/update/apply → daemon 优雅退出（pending
#     留盘未确认）→ 继任拉起（模拟壳 supervisor）→ BootHook 自检 Confirm
#     → /api/status 版本跳变（两段式全链，W4p2 p4b 契约）
#   B config 持久化：POST /api/config 持久化字段 → 重启 daemon → GET
#     /api/config 仍为持久化值（W4p2 p4a 契约，重启跨越大）
#   C version 提示行（D8：静态 update 引导行）
#
# 子命令模式（CI 每场景独立 step——失败步骤名即定位）：
#   setup | scenario-a | scenario-b | scenario-c | teardown
# 全量单步跑法（本地）：sh scripts/smoke_w4p2_p4.sh all
set -e
cd "$(dirname "$0")/.." || exit 1
ROOT=$PWD
export HOME="${HOME:-/root}"   # W2 教训：GOCACHE 依赖 HOME

STATE=/tmp/w4p4state.env
PORT=19721

fail() {
  echo "SMOKE-W4P4 FAIL: $1" >&2
  exit 1
}
ok() { echo "PASS: $1"; }

load_state() { . "$STATE"; }
GOOS_L="${GOOS_L:-}"
# exe 侧 HOME 必须是原生路径（git-bash /tmp 与 Windows 进程视角分叉，CI 实证）。
home_native() {
  if [ -n "$D_NATIVE" ]; then echo "$D_NATIVE"; else echo "$D"; fi
}
run() {
  if [ "$GOOS_L" = "windows" ]; then
    HOME="$(home_native)" USERPROFILE="$(home_native)" "$D/ghydra$EXT" "$@"
  else
    HOME="$(home_native)" "$D/ghydra$EXT" "$@"
  fi
}
# gobuild 本地 PRoot go1.25 间歇 segfault 规避：139 重试（CI 恒绿，重试
# 永不触发）。双判定铁律（P3 教训）：rc 与产物存在性缺一不可——segfault
# 偶发把 $? 读成 0，只有产物在才算成；赋值必须放 if 条件上下文（裸赋值
# 会把 rc=139 传播给赋值语句，set -e 直接杀脚本）。
gobuild() {
  dst="$1"; shift
  i=0
  while :; do
    rm -f "$dst"
    if out=$("$@" 2>&1); then rc=0; else rc=$?; fi
    if [ "$rc" -eq 0 ] && [ -f "$dst" ]; then return 0; fi
    if [ "$rc" -eq 139 ] && [ "$i" -lt 4 ]; then
      i=$((i + 1)); sleep 5; continue
    fi
    echo "$out" | tail -5 >&2
    return 1
  done
}
# read_serve <字段>：从 serve.json 抽 pid/port（运行态真相自写——每轮重读，
# 端口迁移跟随，W4p1 教训）。文件缺席时返回 0 + 空输出——裸赋值 SP=$(…)
# 若带非零 rc 会被 set -e 杀脚本（P3 教训的变体，CI 三平台实证）。
read_serve() {
  f="$GHDIR/serve.json"
  [ -f "$f" ] || return 0
  sed -n "s/.*\"$1\":\([0-9]*\).*/\1/p" "$f" | head -1
}
kill_serve() {
  p=$(read_serve pid) || return 0
  [ -n "$p" ] || return 0
  if [ "$GOOS_L" = "windows" ]; then
    taskkill //F //PID "$p" >/dev/null 2>&1 || true
  else
    kill "$p" >/dev/null 2>&1 || true
  fi
}

cmd_setup() {
  BIN=$(mktemp -d /tmp/w4p4bin.XXXXXX)
  D=$(mktemp -d /tmp/w4p4data.XXXXXX)   # 安装目录 + HOME 沙箱（BIN/D 分离）
  PUB="$ROOT/engine/selfupdate/testdata/test.pub"
  KEY="$ROOT/engine/selfupdate/testdata/test.key"
  if [ "$(go env GOOS)" = "windows" ]; then
    PUB=$(cd "$ROOT/engine/selfupdate/testdata" && pwd -W)/test.pub
    KEY=$(cd "$ROOT/engine/selfupdate/testdata" && pwd -W)/test.key
  fi
  EXT=""
  GOOS_L="$(go env GOOS)" || fail "go env 不可用"
  [ "$GOOS_L" = "windows" ] && EXT=".exe"

  echo "== 构建 v1.0.0 / v1.0.1 / releasesrv =="
  gobuild "$D/ghydra$EXT" \
    go build -ldflags "-X main.Version=1.0.0 -X main.pubKeyOverrideFile=$PUB" \
    -o "$D/ghydra$EXT" ./engine/cmd/ghydra || fail "构建 v1.0.0"
  gobuild "$BIN/ghydra-new$EXT" \
    go build -ldflags "-X main.Version=1.0.1 -X main.pubKeyOverrideFile=$PUB" \
    -o "$BIN/ghydra-new$EXT" ./engine/cmd/ghydra || fail "构建 v1.0.1"
  gobuild "$BIN/releasesrv$EXT" \
    go build -o "$BIN/releasesrv$EXT" ./scripts/releasesrv || fail "构建 releasesrv"

  "$BIN/releasesrv$EXT" -listen "127.0.0.1:$PORT" -newbin "$BIN/ghydra-new$EXT" \
    -version v1.0.1 -key "$KEY" >"$D/srv.log" 2>&1 &
  SRV_PID=$!
  for _ in $(seq 1 50); do
    curl -s -o /dev/null "http://127.0.0.1:$PORT/files/checksums.txt" && break
    sleep 0.2
  done
  curl -s -o /dev/null "http://127.0.0.1:$PORT/files/checksums.txt" || { cat "$D/srv.log"; fail "releasesrv 未就绪"; }

  [ "$(run version | head -1)" = "ghydra version 1.0.0" ] || fail "初始版本应为 1.0.0"
  # C 前置顺带：提示行在场（D8）
  run version | sed -n '2p' | grep -q "检查更新" || fail "version 应含 update 提示行"

  D_NATIVE="$D"
  if [ "$(go env GOOS)" = "windows" ]; then
    D_NATIVE=$(cd "$D" && pwd -W 2>/dev/null || echo "$D")
  fi

  cat > "$STATE" <<EOF
ROOT='$ROOT'
BIN='$BIN'
D='$D'
D_NATIVE='$D_NATIVE'
KEY='$KEY'
EXT='$EXT'
GOOS_L='$GOOS_L'
PORT=$PORT
SRV_PID=$SRV_PID
EOF
  ok "setup（双版本 + releasesrv 就绪，初始 1.0.0，提示行在场）"
}

# wait_api <次数>：等 serve.json+token 就绪并探活；回显 "port token"。
wait_api() {
  GHDIR="$(home_native)/.ghydra"
  i=0
  while [ $i -lt "$1" ]; do
    SP=$(read_serve port)
    if [ -n "$SP" ] && [ -f "$GHDIR/api-token" ]; then
      TOK=$(cat "$GHDIR/api-token" 2>/dev/null) || true
      if [ -n "$TOK" ] && curl -s -o /dev/null -m 1 -H "X-GHydra-Token: $TOK" "http://127.0.0.1:$SP/api/status"; then
        echo "$SP $TOK"
        return 0
      fi
    fi
    i=$((i+1)); sleep 0.2
  done
  return 1
}

# 场景 A：serve 内 apply 两段式全链（GUI 壳依赖的生产路径）。
cmd_scenario_a() {
  load_state
  API="http://127.0.0.1:$PORT/repos/xueweijian/ghydra"
  GHDIR="$(home_native)/.ghydra"

  run serve --managed --update-api "$API" --update-trust-host 127.0.0.1 \
    >"$D/serve.log" 2>&1 &
  read SP TOK <<EOF || true
$(wait_api 100)
EOF
  [ -n "$SP" ] && [ -n "$TOK" ] || { cat "$D/serve.log" 2>/dev/null; fail "serve 未就绪"; }

  # check（同步 + 缓存）：has_update
  curl -s -m 30 -H "X-GHydra-Token: $TOK" "http://127.0.0.1:$SP/api/update/check" \
    | grep -q '"has_update":true' || fail "API check 应报有更新"

  # apply 受理（异步）
  curl -s -m 10 -X POST -H "X-GHydra-Token: $TOK" "http://127.0.0.1:$SP/api/update/apply" \
    | grep -q '"started":true' || fail "apply 应受理（202 面）"

  # 等 daemon 自退（两段式：swap 后 pending_boot → 优雅退出）
  i=0; dead=0
  while [ $i -lt 300 ]; do
    curl -s -o /dev/null -m 1 "http://127.0.0.1:$SP/api/status" 2>/dev/null || { dead=1; break; }
    i=$((i+1)); sleep 0.2
  done
  [ "$dead" = 1 ] || { tail -20 "$D/serve.log"; fail "daemon 应在 apply 后自行退出"; }

  # 第一段契约：pending 留盘、未确认
  grep -q '"pending_version":"1.0.1"' "$GHDIR/update.json" || { cat "$GHDIR/update.json"; fail "应 pending 1.0.1"; }
  grep -q '"confirmed":false' "$GHDIR/update.json" || fail "应未确认（Confirm 归 BootHook）"

  # 第二段（模拟壳 supervisor）：同路径拉继任
  run serve --managed >>"$D/serve.log" 2>&1 &

  # 继任 BootHook 自检→Confirm→探活版本跳变（每轮重读 serve.json 跟随迁移；
  # curl 失败是常态——|| true 防 set -e 杀循环）
  i=0; got=""
  while [ $i -lt 150 ]; do
    SP2=$(read_serve port)
    if [ -n "$SP2" ]; then
      body=$(curl -s -m 1 -H "X-GHydra-Token: $TOK" "http://127.0.0.1:$SP2/api/status" 2>/dev/null || true)
      case "$body" in
        *'"version":"1.0.1"'*) got=1; break ;;
      esac
    fi
    i=$((i+1)); sleep 0.2
  done
  [ -n "$got" ] || { tail -30 "$D/serve.log"; cat "$GHDIR/update.json" 2>/dev/null; fail "继任应为 1.0.1"; }
  grep -q '"confirmed":true' "$GHDIR/update.json" || fail "BootHook 应已确认"

  kill_serve
  ok "A serve 内 apply 两段式全链（apply→自退→pending→继任 BootHook→confirmed 1.0.1）"
}

# 场景 B：config 持久化跨重启（p4a 契约）。
cmd_scenario_b() {
  load_state
  GHDIR="$(home_native)/.ghydra"

  run serve --managed >"$D/serve.log" 2>&1 &
  read SP TOK <<EOF || true
$(wait_api 100)
EOF
  [ -n "$SP" ] && [ -n "$TOK" ] || { cat "$D/serve.log" 2>/dev/null; fail "serve 未就绪（B）"; }

  # 持久化两字段（响应回显的是重启前运行态——新值落 config 表，重启后
  # 生效；requires_restart 用旗标名 doctor_interval，非字段名 doctor_every_s）
  body=$(curl -s -m 10 -X POST -H "X-GHydra-Token: $TOK" -H "Content-Type: application/json" \
    -d '{"doctor_every_s":7200,"rules_url":"https://example.test/rules.json"}' \
    "http://127.0.0.1:$SP/api/config" || true)
  echo "$body" >"$D/cfg.json"
  echo "$body" | grep -q '"requires_restart":\[[^]]*"rules_url"' \
    || { echo "$body"; fail "requires_restart 应含 rules_url"; }
  echo "$body" | grep -q '"requires_restart":\[[^]]*"doctor_interval"' \
    || { echo "$body"; fail "requires_restart 应含 doctor_interval"; }

  # 重启 daemon（kill_serve = 管理面终止；无快照 → 退出 hook 无害）
  kill_serve
  sleep 1
  run serve --managed >>"$D/serve.log" 2>&1 &
  read SP TOK <<EOF || true
$(wait_api 100)
EOF
  [ -n "$SP" ] && [ -n "$TOK" ] || { cat "$D/serve.log" 2>/dev/null; fail "重启 serve 未就绪"; }

  body=$(curl -s -m 5 -H "X-GHydra-Token: $TOK" "http://127.0.0.1:$SP/api/config" || true)
  echo "$body" | grep -q '"doctor_every_s":7200' || { echo "$body"; fail "doctor_every_s 应跨重启持久"; }
  echo "$body" | grep -q '"rules_url":"https://example.test/rules.json"' || fail "rules_url 应跨重启持久"

  kill_serve
  ok "B config 持久化跨重启（7200 + rules_url 存活）"
}

# 场景 C：version 提示行（D8；setup 已断言 1.0.0 形态，此处升级后复核）。
cmd_scenario_c() {
  load_state
  [ "$(run version | head -1)" = "ghydra version 1.0.1" ] || fail "A 后版本应 1.0.1"
  run version | sed -n '2p' | grep -q "检查更新" || fail "升级后提示行应在场"
  ok "C version 提示行（升级前后双形态）"
}

cmd_teardown() {
  load_state 2>/dev/null || true
  kill_serve 2>/dev/null || true
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null || true
  [ -n "$SERVE_BG" ] && kill "$SERVE_BG" 2>/dev/null || true
  rm -rf "$D" "$BIN" 2>/dev/null || true
  rm -f "$STATE"
  ok "teardown"
}

case "${1:-all}" in
  setup)      cmd_setup ;;
  scenario-a) cmd_scenario_a ;;
  scenario-b) cmd_scenario_b ;;
  scenario-c) cmd_scenario_c ;;
  teardown)   cmd_teardown ;;
  all)
    cmd_setup
    cmd_scenario_a
    cmd_scenario_b
    cmd_scenario_c
    cmd_teardown
    echo "ALL-PASS"
    ;;
  *) echo "用法: $0 setup|scenario-a|scenario-b|scenario-c|teardown|all" >&2; exit 2 ;;
esac
