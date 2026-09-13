# GHydra M3-W0 壳 Spike 报告

> 状态：进行中（2026-09-13 开工）。终局结论以 CI 三平台绿 + 真机清单为准。
> 决策输入：2026-09-13 用户拍板「只用 Wails v3 做壳」（覆盖 DevWorkflow §6 原规则 #2）。

## 0. 版本与锁定

| 项 | 值 | 备注 |
|---|---|---|
| wails/v3 | **v3.0.0-beta.20**（2026-09-10） | go.mod 精确锁定；升级必须 CI 三平台全绿 |
| Go | ≥ 1.25.0（wails v3 硬要求） | 沙箱已升 1.27.1；CI go-version 1.25 |
| 前端 | solid-js ^1.9.5 + vite ^6 + vite-plugin-solid ^2.11 | lock 提交，CI `npm ci` |
| linux 桌面依赖 | gtk+-3.0 + webkit2gtk-4.1（beta.20 cgo 实查） | ubuntu-24.04 `-dev` 包 |

## 1. 三件套验证策略与结果

| 件 | v3 能力（beta.20 API 实查） | CI 机检 | 真机清单 |
|---|---|---|---|
| 托盘常驻 | `SystemTray`：进程内，AttachWindow 点击唤起/隐藏、SetMenu/SetTooltip/SetIcon | linux xvfb：创建+设图标菜单不崩 | win/mac 托盘可见+点击+菜单交互 |
| 单实例唤醒 | `Options.SingleInstance`：UniqueID + OnSecondInstanceLaunch 回调 | —（需跨进程交互） | 双开 → 首实例面板带到前台 |
| 开机自启 | `AutostartManager`：Enable/Disable/IsEnabled/Status（win Run 键 / mac SMAppService/LaunchAgent / XDG） | linux xvfb：Enable→IsEnabled==true→Disable→IsEnabled==false 往返 | win 重启/注销重登后自动拉起 |

> 原方案的自研 autostart/singleinstance（拍板 #5）**作废**——v3 内建实现与自研机制完全同源（Run 键/LaunchAgent/XDG），取原生。

## 2. 交付物

- `gui/` 独立 Go 模块（module github.com/xueweijian/ghydra/gui）：
  - `main.go`：壳本体（窗口 + 托盘 + 关窗隐藏 + 单实例回调）
  - `spikecheck/main.go`：CI 冒烟二进制（linux xvfb 运行）
  - `frontend/`：SolidJS 状态页 spike（轮询 serve /status，daemon 地址可配）
  - 前端 dist 经 go:embed 进壳；dist 同时可被 serve 托管（syncthing 兜底共用）
- `.github/workflows/ci-gui.yml`：前端构建 + 三平台矩阵（windows CGO=0 纯 Go / linux xvfb 冒烟 / mac cgo）+ 壳产物 artifact
- serve 只读端点（/status /pac）加 `Access-Control-Allow-Origin: *`（GUI 壳与 daemon 分源；W3-W1 用 token+Host 校验收紧）

## 3. 架构记录

- 关窗 = 隐藏（托盘常驻），退出走托盘菜单——`events.Common.WindowClosing` hook + `e.Cancel()`。
- GUI 与引擎通信：本 spike 直接轮询既有 `/status`；W3-W1 升级 `/api/*` + SSE + 本机 token。
- 体积记录（无红线，参考）：windows 交叉编译壳 16.4MB（未 strip 前端嵌入版）；前端 gzip 4.65KB。

## 4. 真机手动清单（W6 全新 Windows 验收一并做）

1. 双击运行 → 面板出现；关面板 → 托盘仍在
2. 托盘点击 → 面板唤起/隐藏；右键菜单（打开面板/自启切换/退出）
3. 双开第二个实例 → 自动退出，首实例面板带到前台
4. 开机自启切换 → 注册表 Run 键出现/消失；重启自动拉起
5. daemon 未运行时面板显示「daemon 未连接」引导

## 5. CI 状态

- 首轮：待验证（ci-gui.yml 随本报告推送）。
