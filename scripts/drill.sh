#!/usr/bin/env bash
# GHydra 故障演练（M2-W4）：真实二进制 + 真实进程链的黑盒证明。
#
# 范围（与 Go 库级测试的分工）：
#   - 本脚本证明：serve 进程 + doctor loop + 通道决策器 + get 切道，
#     在"网络层故障"（TLS 握手死/RST）下的端到端行为
#   - 源级 403 语义（需受信 TLS）已由 engine/channel 的故障注入测试覆盖
#   - 恢复（回 A）由 channel 三态单测覆盖；黑盒无法伪造健康 github.com
#     （证书不可信），故不在本脚本断言
#
# 场景：
#   1. fakesite tlsdead 监听 :443 —— 所有 HTTPS 连接秒死（TLS 握手死特征）
#   2. ghydra serve --dial-override github系=127.0.0.1 --doctor-interval 2s
#      （A 路径拨号 + doctor 直连列探针同被压到假源站 → 两列同死）
#   3. 断言：/status 的 channel 在 30s 内 trip（State=open）
#   4. 断言：ghydra get 经 B（fakesite cdn）完整下载，内容逐字节一致
#
# CI：ubuntu-latest 直跑；无需 root（不碰 /etc/hosts——DNS/hosts 在各平台
# 行为不一，dial-override 在拨号层注入，全平台确定性生效）。
set -euo pipefail
cd "$(dirname "$0")/.."

WORK="$(mktemp -d)"
GHYDRA_BIN="$WORK/ghydra"
FAKESITE_BIN="$WORK/fakesite"
PORT_PROXY=19701
PORT_CDN=18701
trap 'kill $(jobs -p) 2>/dev/null; rm -rf "$WORK"' EXIT

echo "== build =="
go build -o "$GHYDRA_BIN" ./engine/cmd/ghydra
go build -o "$FAKESITE_BIN" ./scripts/fakesite

echo "== fakesites =="
"$FAKESITE_BIN" -mode cdn -port "$PORT_CDN" > "$WORK/cdn.log" 2>&1 &
"$FAKESITE_BIN" -mode tlsdead -port 443 > "$WORK/tlsdead.log" 2>&1 &
sleep 0.5

echo "== serve =="
# dial-override 把全部 github 系 host 的上游拨号压到本地假源站（SNI 语义不变，
# scheduler 的 DoH 真实 IP 被覆盖——这是比 /etc/hosts 更精确的网络层故障注入）
OVS=""
for h in github.com api.github.com codeload.github.com avatars.githubusercontent.com objects.githubusercontent.com raw.githubusercontent.com release-assets.githubusercontent.com; do
  OVS="$OVS$h=127.0.0.1,"
done
"$GHYDRA_BIN" serve --listen "127.0.0.1:$PORT_PROXY" --scheduler on \
  --db "$WORK/drill.db" --doctor-interval 2s \
  --dial-override "${OVS%,}" \
  --cdn "http://127.0.0.1:$PORT_CDN/" > "$WORK/serve.log" 2>&1 &
SERVE_PID=$!

# 探活
for i in $(seq 1 50); do
  if curl -s -o /dev/null "http://127.0.0.1:$PORT_PROXY/status"; then break; fi
  sleep 0.2
done
curl -s -o /dev/null "http://127.0.0.1:$PORT_PROXY/status" || { echo "FAIL: serve 未探活"; tail -20 "$WORK/serve.log"; exit 1; }
# 自检：/status 必须是本 drill 的 serve（端口迁移会让轮询打到别人的进程）
GOT=$(curl -s "http://127.0.0.1:$PORT_PROXY/status" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("listen",""))' 2>/dev/null || true)
if [ "$GOT" != "127.0.0.1:$PORT_PROXY" ]; then
  echo "FAIL: $PORT_PROXY 被其他进程占用（listen=$GOT）——清理残留的 ghydra serve 后重试"
  exit 1
fi
echo "serve 已启动（pid $SERVE_PID）"

echo "== 断言①：channel 30s 内 trip =="
TRIPPED=0
for i in $(seq 1 60); do
  STATE=$(curl -s "http://127.0.0.1:$PORT_PROXY/status" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("channel",{}).get("State",0))' 2>/dev/null || echo 0)
  if [ "$STATE" != "0" ]; then
    TRIPPED=1
    echo "channel 已 trip（State=$STATE，${i}x0.5s）"
    break
  fi
  sleep 0.5
done
if [ "$TRIPPED" != "1" ]; then
  echo "FAIL: 30s 内 channel 未 trip"
  curl -s "http://127.0.0.1:$PORT_PROXY/status" | head -c 2000
  tail -30 "$WORK/serve.log"
  exit 1
fi

echo "== 断言②：get 经 B 完整交付 =="
printf 'GHYDRA-DRILL-ASSET-0123456789ABCDEF' > "$WORK/expect.bin"
"$GHYDRA_BIN" get "https://github.com/fake/repo/releases/download/v1/asset.bin" \
  -o "$WORK/asset.bin" --cdn "http://127.0.0.1:$PORT_CDN/" --db "$WORK/drill.db" \
  > "$WORK/get.log" 2>&1
if ! cmp -s "$WORK/expect.bin" "$WORK/asset.bin"; then
  echo "FAIL: get 内容不一致"
  cat "$WORK/get.log"
  exit 1
fi
grep -q "通道 B" "$WORK/get.log" && echo "get 走 B ✓" || { echo "WARN: get 未标注通道（检查日志）"; cat "$WORK/get.log"; }

echo "== 演练全绿 =="
