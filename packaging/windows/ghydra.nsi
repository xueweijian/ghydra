; GHydra Windows 安装器（W4p2 D4，NSIS 3.x）
; 构建：makensis ghydra.nsi（CI windows runner 自带 NSIS）
; 前置：同目录存在 ghydra.exe（**gui 变体**单二进制，-tags gui 构建）
;
; 安全关键（设计红线，静态断言脚本会 grep 本文件）：
;   1. 卸载先执行 `ghydra.exe off --wait`（还原系统代理 + 停 daemon + 等进程死透）
;   2. off 失败（退出码非 0）→ 弹窗让用户选择中止——绝不静默留下半接管态（M1-D4 安装器版）
;   3. 绝不动 %USERPROFILE%\.ghydra\（用户数据 + 崩溃恢复凭证——误删会让下次安装
;      无法对账还原，用户原代理值永久丢失）
;   4. 不预置开机自启（AutostartManager 单一事实源在产品内，GUI 引导开启）

Unicode true
ManifestDPIAware true
!include "LogicLib.nsh" ; ${If}/${EndIf}

!define APPNAME "GHydra"
!define COMPANY "GHydra Project"
!define EXE "ghydra.exe"

Name "${APPNAME}"
OutFile "..\..\ghydra-windows-amd64-setup.exe"
InstallDir "$PROGRAMFILES64\${APPNAME}"
InstallDirRegKey HKLM "Software\${APPNAME}" "InstallDir"
RequestExecutionLevel admin

Page directory
Page instfiles
UninstPage uninstConfirm
UninstPage instfiles

Section "Install"
  SetOutPath "$INSTDIR"
  File "${EXE}"

  ; 卸载器必须先于文件删除写入（selfdelete 模式的一部分）
  WriteUninstaller "$INSTDIR\uninstall.exe"

  ; 开始菜单
  CreateDirectory "$SMPROGRAMS\${APPNAME}"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\${APPNAME}.lnk" "$INSTDIR\${EXE}"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\Uninstall ${APPNAME}.lnk" "$INSTDIR\uninstall.exe"

  ; 注册卸载信息（Windows「应用与功能」）
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" \
    "DisplayName" "${APPNAME} — GitHub 加速器"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" \
    "UninstallString" "$INSTDIR\uninstall.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" \
    "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" \
    "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}" \
    "NoRepair" 1
SectionEnd

Section "Uninstall"
  ; ── 安全关键序列（红线 1/2）────────────────────────────────────
  ; 先还原系统代理 + 停 daemon + 等进程死透（--wait 内含 10s 超时强杀）。
  ; 非零退出 = 代理未成功还原 → 弹窗，用户可中止。绝不静默。
  ExecWait '"$INSTDIR\${EXE}" off --wait' $0
  ${If} $0 != 0
    MessageBox MB_OKCANCEL|MB_ICONEXCLAMATION \
      "GHydra 无法完成系统代理还原（退出码 $0）。$\n$\n\
      点「取消」中止卸载（推荐，稍后可运行 ghydra off 重试）；$\n\
      点「确定」在系统代理可能未还原的情况下继续卸载（不推荐）。" \
      IDOK +2
    Abort
  ${EndIf}

  Delete "$INSTDIR\${EXE}"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  RMDir "$SMPROGRAMS\${APPNAME}"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}"
  DeleteRegKey HKLM "Software\${APPNAME}"

  ; ── 红线 3：绝不动 %USERPROFILE%\.ghydra\ ──────────────────────
  ; 用户数据（api-token/诊断状态/DB）与托管快照（崩溃恢复凭证）保留。
  ; 快照残留由下次安装/运行时的 ensureReconcile 对账机制消化。
SectionEnd
