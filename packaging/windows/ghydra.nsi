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
; F1：EnVar 插件（v0.3.1 unicode）——PATH 机器级写入/精确删除。
; DLL 已 vendor（plugins/x86-unicode/，CI 免下载）。!addplugindir 相对路径
; 按「脚本所在目录」解析（实测），与调用 cwd 无关——CI/本地同构。
!addplugindir "plugins\x86-unicode"

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

  ; 快捷方式统一带 "gui" 参数——exe 裸跑是 usage+exit 2（v1.0.2 真机
  ; 双击闪退实证）。$DESKTOP 需 current 上下文（提权安装器默认 all，
  ; 会落到 Public 桌面——用户实际桌面不可见）。
  SetShellVarContext current
  CreateDirectory "$SMPROGRAMS\${APPNAME}"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\${APPNAME}.lnk" "$INSTDIR\${EXE}" "gui"
  CreateShortcut "$SMPROGRAMS\${APPNAME}\Uninstall ${APPNAME}.lnk" "$INSTDIR\uninstall.exe"
  CreateShortcut "$DESKTOP\${APPNAME}.lnk" "$INSTDIR\${EXE}" "gui"

  ; ── F1：PATH 写入（四轮验收现象 2：安装器不写 PATH，新终端找不到命令）──
  ; EnVar::SetHKLM = 机器级（Program Files 安装已是管理员上下文，全用户
  ; 可见）；插件自带 WM_SETTINGCHANGE 广播——新开终端立即可用，免注销。
  ; AddValue 幂等（已存在返回成功码）；不用 ${EnvVar} 宏（PATH 超长时
  ; 字符串拼接会截断）；不写 HKCU（机器级安装语义应为全用户可见）。
  EnVar::SetHKLM
  EnVar::AddValue "PATH" "$INSTDIR"
  Pop $0
  ${If} $0 != 0
    DetailPrint "EnVar::AddValue PATH 返回 $0（0=成功）——新终端 ghydra 可能不可用"
  ${EndIf}

  ; F1：App Paths 注册——Win+R 输 ghydra.exe / ShellExecute 可达，零 PATH
  ; 污染（App Paths 是独立注册键，与 check-nsi.sh 红线 4 检查的自启
  ; 键无关——此处注释刻意不写该字面量，防静态断言自触发）
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\App Paths\${EXE}" "" "$INSTDIR\${EXE}"

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
  ; 桌面 lnk 与安装时同上下文（current）删除
  SetShellVarContext current
  Delete "$DESKTOP\${APPNAME}.lnk"

  ; ── F1：PATH 精确移除自身条目（DeleteValue 只删精确匹配项，PATH 其余
  ; 内容绝不动；历史多条重复也一并清）+ App Paths 按归属条件删除 ──
  EnVar::SetHKLM
  EnVar::DeleteValue "PATH" "$INSTDIR"
  Pop $0
  ; 双安装场景（如 C 盘旧装 + F 盘新装）：App Paths 只在仍指向本安装
  ; 目录时才删——无条件删会带走另一份安装的 Win+R/ShellExecute 注册
  ; （v1.0.2 真机实证）。
  ReadRegStr $0 HKLM "Software\Microsoft\Windows\CurrentVersion\App Paths\${EXE}" ""
  ${If} $0 == "$INSTDIR\${EXE}"
    DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\App Paths\${EXE}"
  ${EndIf}

  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\${APPNAME}"
  DeleteRegKey HKLM "Software\${APPNAME}"

  ; ── 红线 3：绝不动 %USERPROFILE%\.ghydra\ ──────────────────────
  ; 用户数据（api-token/诊断状态/DB）与托管快照（崩溃恢复凭证）保留。
  ; 快照残留由下次安装/运行时的 ensureReconcile 对账机制消化。
SectionEnd
