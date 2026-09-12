#!/bin/sh
# GHydra W1 真网冒烟：起 serve → CONNECT 隧道过 GitHub 六域名 → 放行路径 → 事件日志
# 前提：用户已关闭魔法（VPN），网络为真实大陆直连态。
set -e
BIN=/tmp/ghydra
PROXY=127.0.0.1:19801
LOG=/tmp/ghydra_serve.log

cd /var/minis/workspace/ghydra && go build -o $BIN ./engine/cmd/ghydra

$BIN serve --listen $PROXY > $LOG 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null' EXIT
sleep 1

echo "== 真实出口环境（不经代理）=="
curl -s --max-time 8 https://ipinfo.io/json 2>/dev/null | head -c 300 || echo "(ipinfo 不可达)"
echo; echo

echo "== 加速域名（CONNECT 隧道 → W1 直连域名本身）=="
for d in github.com api.github.com codeload.github.com avatars.githubusercontent.com objects.githubusercontent.com raw.githubusercontent.com; do
  out=$(curl -x http://$PROXY -s -o /dev/null -w '%{http_code} %{time_total}s' --max-time 20 "https://$d/" 2>/dev/null) || out="ERR/超时"
  printf "  %-38s %s\n" "$d" "$out"
done

echo
echo "== 放行路径（非加速域名，验证零打扰）=="
out=$(curl -x http://$PROXY -s -o /dev/null -w '%{http_code} %{time_total}s' --max-time 15 https://www.baidu.com/ 2>/dev/null) || out="ERR/超时"
printf "  %-38s %s\n" "www.baidu.com（放行）" "$out"

sleep 1
echo
echo "== serve 事件日志 =="
grep -E 'conn#' "$LOG" | head -12
echo "总连接数: $(grep -c 'conn#' "$LOG" 2>/dev/null || echo 0)"
