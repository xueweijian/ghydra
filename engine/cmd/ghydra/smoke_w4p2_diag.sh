#!/bin/sh
# W4p2 P2 diag 冒烟（本地 + CI）：真 serve + 真 diag → zip 结构 + 泄漏扫描
set -e
cd "$(dirname "$0")/../.." # engine/
export HOME="${HOME:-/root}"
BIN=/tmp/ghydra-smoke-bin
D=/tmp/ghydra-smoke-diag
mkdir -p "$BIN"
go build -o "$BIN/ghydra" ./cmd/ghydra

PORT=9879
TOK=diagsmoketoken99deadbeef77
rm -rf "$D"   # BIN/D 分离（W2 教训）
mkdir -p "$D"

HOME_BAK="$HOME"
export HOME="$D"

# 植入敏感串（泄漏扫描断言对象）
mkdir -p "$D/.ghydra"
printf 'dial 20.205.243.166:443 ok\nloaded token=%s\nvia http://diaguser:DiagPass77@10.9.8.7:7890\n' "$TOK" \
  > "$D/.ghydra/serve.log"
printf '%s\n' "$TOK" > "$D/.ghydra/api-token"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; exit 1; }
cleanup() { kill $SRV 2>/dev/null || true; export HOME="$HOME_BAK"; }
trap cleanup EXIT

# 场景 A：serve 运行时 diag
"$BIN/ghydra" serve --listen 127.0.0.1:$PORT --api-token "$TOK" \
  --scheduler=false > "$D/serve.out" 2>&1 &
SRV=$!
i=0
while ! curl -s -o /dev/null http://127.0.0.1:$PORT/pac; do
  i=$((i+1)); [ $i -gt 50 ] && { echo "FAIL: serve 未就绪"; cat "$D/serve.out"; exit 1; }
  sleep 0.2
done

# 构造运行态（W2 同款：serve 前台不写 serve.json；托管路径由 on 写。
# diag 的端口事实源 = serve.json——手工对齐真实 on 场景的落盘形态）
printf '{"pid":%s,"port":%s,"started_at":%s}\n' "$SRV" "$PORT" "$(date +%s)" \
  > "$D/.ghydra/serve.json"

"$BIN/ghydra" diag --out "$D/diag-a.zip"
python3 - "$D/diag-a.zip" <<'PYEOF'
import sys, zipfile, json
z = zipfile.ZipFile(sys.argv[1])
names = set(z.namelist())
assert "pools.json" in names, f"serve 运行时缺 pools.json: {names}"
assert "serve.log.tail" in names and "report.json" in names and "doctor_log.json" in names
rep = json.loads(z.read("report.json"))
assert rep["serve_status"] == "running", rep
blob = b"".join(z.read(n) for n in z.namelist())
for secret in [b"diagsmoketoken99deadbeef77", b"DiagPass77", b"diaguser"]:
    assert secret not in blob, f"泄漏扫描命中: {secret}"
assert b"20.205.243.166" in blob, "IP 应保留（诊断价值）"
print("PASS: 场景A 运行时 diag（pools 在 + serve_status=running + 泄漏零命中 + IP 保留）")
PYEOF

# 场景 B：停 serve + 清运行态（on 的退出 hook 语义）→ not-running
kill $SRV 2>/dev/null || true; wait $SRV 2>/dev/null || true
SRV=""
rm -f "$D/.ghydra/serve.json"
"$BIN/ghydra" diag --out "$D/diag-b.zip"
python3 - "$D/diag-b.zip" <<'PYEOF'
import sys, zipfile, json
z = zipfile.ZipFile(sys.argv[1])
assert "pools.json" not in z.namelist(), "serve 停后不应有 pools.json"
rep = json.loads(z.read("report.json"))
assert rep["serve_status"] == "not-running", rep
print("PASS: 场景B 停服 diag（not-running 标注）")
PYEOF

echo "=== diag 冒烟全绿 ==="
