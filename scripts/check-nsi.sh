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

# ── v1.0.3：启动直达 + 双安装安全（真机实证三项）─────────────────
# 快捷方式必须带 gui 参数——裸跑是 usage+exit 2（双击闪退，v1.0.2 真机）
grep -q 'CreateShortcut.*\\${EXE}" "gui"' "$NSI" || fail "主快捷方式缺 gui 启动参数（双击闪退）"
pass "快捷方式带 gui 参数（双击直达面板）"

# 必须建桌面快捷方式（v1.0.2 装完桌面无图标，用户找不到入口）
grep -q 'CreateShortcut "\$DESKTOP' "$NSI" || fail "缺桌面快捷方式"
pass "安装创建桌面快捷方式"

# App Paths 删除必须条件化（双安装场景：卸 A 不应删 B 的注册，真机实证）
grep -q 'ReadRegStr \$0.*App Paths' "$NSI" || fail "App Paths 删除未条件化（双安装误删）"
pass "App Paths 删除按归属条件化"

# ── v1.0.3 rc 演练实证（第二轮）──────────────────────────────────
# InstallDir 键必须有写入（InstallDirRegKey 依赖它决定重装默认目录；
# 从未写入 → 静默/升级重装漂移到默认盘，双安装根源）
# （-F 固定串：本环境 grep 为 ugrep，\$ 正则转义行为与 GNU 不同）
grep -qF 'WriteRegStr HKLM "Software\${APPNAME}" "InstallDir"' "$NSI" || fail "InstallDir 键未写入（重装目录漂移）"
pass "InstallDir 键写入（重装记住目录）"

# 桌面 lnk 删除必须条件化（双安装共享路径：卸 A 不得删 B 的图标，真机实证）
grep -q 'nsExec::ExecToStack' "$NSI" || fail "桌面 lnk 删除未条件化（双安装连坐误删）"
if grep -qE '^\s*Delete "\$DESKTOP' "$NSI"; then
  fail "桌面 lnk 仍有无条件 Delete"
fi
pass "桌面 lnk 删除按归属条件化"

echo "=== NSIS 红线断言全绿 ==="
