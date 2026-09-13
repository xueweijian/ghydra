#!/bin/sh
# W1 serve 集成冒烟（本地 + CI）：API 面 + 双模式托管 + 旧端点回归
set -e
cd "$(dirname "$0")/../.." # engine/
export HOME="${HOME:-/root}"  # 沙箱会话 HOME 可能空（GOCACHE 依赖）
BIN=/tmp/ghydra-smoke-bin
D=/tmp/ghydra-smoke
mkdir -p "$BIN"
go build -o "$BIN/ghydra" ./cmd/ghydra

PORT=9877
TOK=smoketoken123
rm -rf "$D"  # 干净起跑：场景 16 落盘的规则/库不得跨运行残留
mkdir -p "$D/dist"
echo '<html><body>GHydra W1 panel</body></html>' > "$D/dist/index.html"

HOME_BAK="$HOME"
export HOME="$D"   # token 落 $D/.ghydra/api-token

"$BIN/ghydra" serve --listen 127.0.0.1:$PORT --api-token "$TOK" \
  --scheduler=false --gui-dist "$D/dist" > "$D/serve.log" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null || true' EXIT

# 等 serve 就绪
i=0
while ! curl -s -o /dev/null http://127.0.0.1:$PORT/pac; do
  i=$((i+1)); [ $i -gt 50 ] && { echo "FAIL: serve 未就绪"; cat "$D/serve.log"; exit 1; }
  sleep 0.2
done

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; exit 1; }

# 1. 无 token → 401
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/api/status)
[ "$code" = "401" ] || fail "无 token 应 401，得 $code"
pass "无 token → 401"

# 2. 错 token → 401；对 token → 200
code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-GHydra-Token: wrong" http://127.0.0.1:$PORT/api/status)
[ "$code" = "401" ] || fail "错 token 应 401"
body=$(curl -s -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/status)
echo "$body" | grep -q '"api_version":1' || fail "status 形状: $body"
pass "token 读 /api/status → 200 + 形状"

# 3. Host 伪造 → 403
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: evil.example.com" -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/status)
[ "$code" = "403" ] || fail "Host 伪造应 403，得 $code"
pass "Host 校验 → 403"

# 4. CORS 预检 → 204
code=$(curl -s -o /dev/null -w '%{http_code}' -X OPTIONS -H "Origin: http://wails.localhost" http://127.0.0.1:$PORT/api/get/start)
[ "$code" = "204" ] || fail "预检应 204，得 $code"
acao=$(curl -s -i -X OPTIONS http://127.0.0.1:$PORT/api/status | grep -i 'access-control-allow-origin' | tr -d '\r')
echo "$acao" | grep -q '\*' || fail "ACAO 头: $acao"
pass "CORS 预检 → 204 + ACAO:*"

# 5. config GET/POST（cdn 热更）
curl -s -X POST -H "X-GHydra-Token: $TOK" -d '{"cdn":"https://gh-proxy.com/"}' http://127.0.0.1:$PORT/api/config | grep -q '"cdn":"https://gh-proxy.com/"' || fail "cdn 热更"
body=$(curl -s -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/status)
echo "$body" | grep -q '"cdn":"https://gh-proxy.com/"' || fail "status 未反映热更: $body"
pass "POST /api/config cdn 热更 → status 反映"

# 6. 未知字段 → 400
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-GHydra-Token: $TOK" -d '{"listen":"x"}' http://127.0.0.1:$PORT/api/config)
[ "$code" = "400" ] || fail "未知字段应 400"
pass "严格 JSON → 400"

# 7. SSE：query token + hello + 首帧 status
frames=$(curl -s -N --max-time 2 "http://127.0.0.1:$PORT/api/events?token=$TOK" | head -c 2000 || true)
echo "$frames" | grep -q 'event: hello' || fail "SSE hello 缺失"
echo "$frames" | grep -q 'event: status' || fail "SSE 首帧 status 缺失"
pass "SSE hello + 首帧 status（query token）"

# 8. MITM 桩
body=$(curl -s -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/mitm/status)
echo "$body" | grep -q '"available":false' || fail "mitm status: $body"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/mitm/enable)
[ "$code" = "501" ] || fail "mitm enable 应 501"
pass "MITM 桩 → status false / enable 501"

# 9. 旧端点回归：/status /pac 免 token
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/status)
[ "$code" = "200" ] || fail "/status 应 200"
body=$(curl -s http://127.0.0.1:$PORT/pac)
echo "$body" | grep -q 'FindProxyForURL' || fail "/pac 内容"
pass "旧端点 /status /pac 回归"

# 10. --gui-dist 托管：GET / 200 + 内容
body=$(curl -s http://127.0.0.1:$PORT/)
echo "$body" | grep -q 'W1 panel' || fail "dist 托管: $body"
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$PORT/some/spa/route)
[ "$code" = "200" ] || fail "SPA fallback 应 200，得 $code"
pass "gui-dist 托管 + SPA fallback"

# 11. token 文件生成 + 0600（ghydra token 负责生成；serve 显式注入不落盘是正确行为）
out=$("$BIN/ghydra" token --file "$D/.ghydra/api-token2" --json)
echo "$out" | grep -q '"token"' || fail "token 子命令 json: $out"
perm=$(ls -l "$D/.ghydra/api-token2" | cut -c1-10)
[ "$perm" = "-rw-------" ] || fail "token 权限: $perm"
pass "token 文件生成 + 0600"

# 12. ghydra token 回读一致（默认路径 load-or-create）
out2=$("$BIN/ghydra" token --file "$D/.ghydra/api-token2" | head -1)
tok2=$(echo "$out" | sed 's/.*"token": *"\([^"]*\)".*/\1/')
[ "$out2" = "$tok2" ] || fail "token 回读不一致: $out2 vs $tok2"
pass "ghydra token 回读一致"

echo "=== W1 serve 冒烟全绿 ==="

# 13. M3-W2 规则地板：无磁盘规则 → /api/rules = embedded v1
#     phase 3：refresher 已装配 → next_at 有调度计划（RFC3339 "2…"开头）
body=$(curl -s -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/rules)
echo "$body" | grep -q '"source":"embedded"' || fail "无磁盘规则应 embedded: $body"
echo "$body" | grep -q '"version":1' || fail "内嵌版本 1: $body"
echo "$body" | grep -q '"refresh":{"last_result":"","last_at":"","next_at":"2' || fail "refresh 形态（next_at 应有值）: $body"
echo "$body" | grep -q '"running":false' || fail "refresh 不应在跑: $body"
pass "GET /api/rules → embedded 地板 + refresher 已装配"

# 14. status 帧携带规则摘要（单一真相：version/source 与 /api/rules 一致）
body=$(curl -s -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/status)
echo "$body" | grep -q '"rules":{"version":1,"source":"embedded","stale":false}' || fail "status.rules: $body"
pass "status.rules 摘要与快照一致"

# 15. rules/refresh 已装配（phase 3）→ 200 异步触发；紧接第二发
#     撞单飞窗口 409 / 已完成 200（ErrBusy 精确语义由 api 单测覆盖）
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/rules/refresh)
[ "$code" = "200" ] || fail "refresh 已装配应 200，得 $code"
curl -s -X POST -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/rules/refresh | grep -qE '"refresh_id"|refresh already in flight' || fail "refresh 响应应为 refresh_id 或 busy"
pass "POST /api/rules/refresh → 200 异步触发（phase 3 接真）"

# 16. 真签名链端到端：repo 真源对（keys.go 冻结公钥签名）落盘 →
#     重启 serve 加载 disk 源 + PAC 含真源域名
kill $SRV 2>/dev/null || true
wait $SRV 2>/dev/null || true
mkdir -p "$D/.ghydra/rules"
cp ../rules/current.json "$D/.ghydra/rules/"
cp ../rules/current.json.minisig "$D/.ghydra/rules/"
RV=$(python3 -c "import json;print(json.load(open('$D/.ghydra/rules/current.json'))['version'])" 2>/dev/null || echo "?")
"$BIN/ghydra" serve --listen 127.0.0.1:$PORT --api-token "$TOK" \
  --scheduler=false --gui-dist "$D/dist" > "$D/serve2.log" 2>&1 &
SRV=$!
i=0
while ! curl -s -o /dev/null http://127.0.0.1:$PORT/pac; do
  i=$((i+1)); [ $i -gt 50 ] && { echo "FAIL: serve2 未就绪"; cat "$D/serve2.log"; exit 1; }
  sleep 0.2
done
body=$(curl -s -H "X-GHydra-Token: $TOK" http://127.0.0.1:$PORT/api/rules)
echo "$body" | grep -q '"source":"disk"' || fail "真源落盘后应 disk: $body"
echo "$body" | grep -q "\"version\":$RV," || fail "disk 版本 $RV: $body"
pac=$(curl -s http://127.0.0.1:$PORT/pac)
echo "$pac" | grep -q 'githubusercontent' || fail "PAC 应含真源域名"
grep -q "磁盘规则已加载 v$RV" "$D/serve2.log" || fail "serve 日志应有加载行"
pass "真签名链端到端：落盘 → disk 源 v$RV + PAC 生效 + 日志"

echo "=== W2 冒烟：rules API 四断言完成 ==="
