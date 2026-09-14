#!/bin/sh
# check-nsi.sh —— NSIS 卸载序列红线静态断言（W4p2 D4 安全关键）。
# 本地 + CI（windows NSIS job）通用。任何一条挂 = 卸载序列被破坏，禁止出包。
set -e
cd "$(dirname "$0")/.."
NSI=packaging/windows/ghydra.nsi

fail() { echo "FAIL: $1" >&2; exit 1; }
pass() { echo "PASS: $1"; }

# 卸载段必须先执行 off --wait（还原系统代理 + 等进程死透）
grep -q 'off --wait' "$NSI" || fail "卸载段缺 off --wait"
pass "卸载先执行 ghydra off --wait"

# off 失败必须可中止（Abort 存在 + MessageBox 警示）
grep -q 'Abort' "$NSI" || fail "off 失败无 Abort 中止路径"
grep -q 'MessageBox' "$NSI" || fail "off 失败无弹窗指引"
pass "off 失败 → 弹窗 + 可中止（不留半接管态）"

# ExecWait 必须取退出码（$0 判断）
grep -q "ExecWait .*' \$0" "$NSI" || fail "ExecWait 未取退出码判断"
pass "ExecWait 捕获 off 退出码"

# 红线：绝不动 %USERPROFILE%\.ghydra\（用户数据 + 崩溃恢复凭证）
if grep -qE '\.ghydra' "$NSI"; then
  # 允许出现在注释里（红线声明），禁止出现在 Delete/RMDir 指令
  if grep -E '^\s*(Delete|RMDir|RMDir /r).*\.ghydra' "$NSI"; then
    fail "卸载器删除 .ghydra 用户数据（红线 3）"
  fi
fi
pass ".ghydra 用户数据不被卸载器触碰（红线 3）"

# 不预置自启（红线 4：AutostartManager 单一事实源）
if grep -qE 'CurrentVersion\\Run' "$NSI"; then
  fail "安装器写 Run 键预置自启（红线 4）"
fi
pass "安装器不预置开机自启（红线 4）"

echo "=== NSIS 红线断言全绿 ==="
