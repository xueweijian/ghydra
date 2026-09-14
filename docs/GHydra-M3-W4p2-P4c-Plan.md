# GHydra M3-W4p2 · P4c 实施方案（D7 前端 + D8 + 壳 supervisor）

> 状态：方案落盘待实施（2026-09-14）。前置：p4a（b3ee13b）+ p4b（b5d0080）已收官。
> 上游依据：GHydra-M3-W4p2-Design.md §P4 实施规划（测试计划总表已冻结，本文为其 p4c 子集细化）。
> 铁律：测试先行——每个分解步骤先写测试再实现；改装配必须回归既有 smoke 全集。

## 1. 目标与范围

把 p4b 的 update 后端能力接通到用户可见面，完成 W4p2 最后一段：

1. **Settings 更新卡片**（检查/版本/changelog/进度/结果横幅）+ 运行参数从只读改可编辑（p4a 后端早已就绪，前端欠账）；
2. **GUI 壳 supervisor**：daemon 死亡 + pending → spawn 继任 `serve --managed` → 轮询新版就绪（两段式的第二段，D1 hybrid 的兑现）；
3. **壳启动自动拉起 daemon**（设计 D1 步骤 2，P1 未实现，本 phase 补齐）;
4. **pending-boot 托盘 tooltip**（D8）+ 壳模式 token/端口自动注入（W4 UX 遗留）;
5. **smoke_w4p2_p4.sh** 全链路 + **GUI 渲染走查**（W3 方法论）。

不在范围：自动更新强推、refresher 热更 API、Linux GUI 发布。

## 2. 现状盘点（读码结论）

| # | 事实 | 出处 |
|---|---|---|
| F1 | `/api/update/check\|apply\|status` + SSE status 帧 `update` 子对象 + 两段式第一段（apply→pending_boot→shutdownCh 优雅退出）**已交付**；runner nil=503 | updateapi.go、api.go:254-276 |
| F2 | **serve 内 updater 的 apiBase/trustHosts 硬编码空**——smoke 无法把 serve 内 apply 指向 fake release server | updateapi.go `newUpdateRunner` |
| F3 | `ghydra gui` = 直通 `guiapp.Run()`，**无探活/无拉起/无 supervisor/无 token 注入**；gui.go 托盘 tooltip 静态 | gui_on.go、gui.go |
| F4 | `restoreSnapshot`（managed 退出 hook）恢复用户代理后 **DeleteSnapshot + removeDaemonState**——更新优雅退出后：接管态消失、serve.json 消失、唯一残留凭证 = update.json pending | main.go:827-855 |
| F5 | spawn 三课已有实现范本（selfPath 预捕获 / fd Start 后关 / 日志 append serve.log），但都在 package main，guiapp 不可 import | daemon.go spawnServeAt、selfupdateglue.go |
| F6 | `daemonControlProd.WaitVersion` 语义（每轮重读 serve.json 跟随端口迁移 + token 走本机文件）是 supervisor 等待逻辑的现成蓝本 | selfupdateglue.go |
| F7 | 前端 types.ts **没有** UpdateInfo/UpdateStatus，ApiStatus 无 `update?` 字段；golden.test.ts 未断言 update_status.json（p4b 只做了 Go 侧 golden） | types.ts、golden.test.ts |
| F8 | Settings.tsx 运行参数仍只读、仍走 setCDN 单字段；p4a 的 ConfigSetResp/ConfigPatch 类型已在 types.ts 但无 UI 消费 | Settings.tsx |
| F9 | `ghydra version` 双行输出（含 update 引导）p4b 已交付，smoke_w4 已适配 | main.go versionCmd |
| F10 | wart：spawnServeAt 把 `--managed` append 了两次（bool flag 重复无害，行为不变） | daemon.go:159-163 |
| F11 | wails beta.20 具备 `WebviewWindow.ExecJS` + `events.Common.WindowRuntimeReady(1043)`——token/端口注入可行 | module cache 实查 |
| F12 | SSE 断线由浏览器 EventSource 原生重连；`connected` 信号已有——「重启中」卡片态可以「曾 applying + connected=false」判定，不依赖最后一帧 | sse.ts |

**关键推论（两段式的用户可见时序）**：pending_boot 存活 <1s（setState 后立即 shutdownCh）——前端大概率**看不到** pending_boot 帧，而是「apply 进度帧 → 连接断开 → （壳拉继任）→ SSE 重连 → 版本跳变」。卡片状态机必须按这个现实设计，"重启生效"按钮**不需要**（自动重启）——这是对设计 D7 原文的架构演进，P4 实施规划的两段式已隐含。

## 3. 架构决策

### S1 supervisor 放哪：internal/guiapp，决策内核纯函数化

- `internal/guiapp/supervisor.go`（`//go:build gui`）+ 平台 spawn 拆 `supervisor_{unix,windows}.go`（模式同 spawn_*：setsid / DETACHED+NEW_PROCESS_GROUP）。
- **决策内核注入式、零 wails 依赖**（headless 可测）：

```go
type Inputs struct {
    Probe   func() (alive bool, version string) // 读 serve.json → GET /api/status（token 读文件，不创建）
    Pending func() string                       // selfupdate.LoadState → PendingVersion（未确认才非空）
    Spawn   func() error                        // spawn 捕获 exe `serve --managed`
    Logf    func(string, ...any)
}
type Action int // ActionNone | ActionSpawnSuccessor | ActionSpawnInitial
func (s *Sup) Tick(startup bool) (Action, SupObs)   // 纯决策：不碰 wails
func (s *Sup) Loop(ctx)                              // 2s tick + WaitHealthy(30s) + OnState 回调
```

- **状态机**（每个 tick 一判）：
  - `startup && !alive` → `SpawnInitial`（D1 步骤 2：pending 与否都拉，BootHook 自会自检/确认/回滚）；
  - `!startup && !alive && pending != ""` → `SpawnSuccessor` + WaitHealthy（want=pending 版本）；
  - `!startup && !alive && pending == ""` → **None**（用户 `off`/前台退出绝不复活；崩溃恢复仍是 ensureReconcile 的职责）;
  - pending 由非空转空（继任 BootHook 已 Confirm）→ OnState(updated_to=<version>)。
- **继承 spawn 三课**（F5）：exe 路径 Run() 入口 `os.Executable()` 预捕获一次（交换后路径指向新二进制，与 daemonControlProd 同理）；日志 append `~/.ghydra/serve.log`；fd 在 Start() 之后关。
- **端口迁移自洽**：Probe 每轮重读 serve.json（F6 蓝本），不缓存端口。
- wails 接线（薄）：OnState 回调里只调 `tray.SetTooltip`（v3 公开 setter，内部主线程封送）。

### S2 更新后接管状态（已拍板 2026-09-14：默认关，不自动恢复）

F4：更新优雅退出会"撤接管"且删快照。**用户拍板：更新后 Boost 默认关，不自动恢复**——语义最简单、无隐式副作用；用户在面板看到「加速已关闭」手动重开即可。
落地：更新完成横幅带一句提示「系统代理已恢复直连，可重新开启加速」（browser 模式同句改为「运行 `ghydra on` 恢复加速」）。不做自动 POST /api/on。

### S3 壳模式 token/端口注入（W4 UX 遗留收口）

`WindowRuntimeReady` 钩子 + `win.ExecJS`：把 `~/.ghydra/api-token` 读出写入 `localStorage.ghydra.token`，并把 `ghydra.daemonBase` 写成 `http://127.0.0.1:<serve.json 端口>`（修掉壳模式写死 9801 与持久化自定义端口的矛盾）。读不到（文件不存在/未 healthy）→ 静默跳过，Settings 手动路径永远保留。healthy 后 supervisor 再补注一次（首注时 daemon 可能还没起来）。

### S4 serve 增加 update override 旗标（测试基建，F2）

`serve --update-api <url>` / `--update-trust-host <h>`（默认空 = 生产行为零变化），接线进 `newUpdateRunner`（签名扩参）。仅测试/演练用，与 updateCmd 的同名旗标语义一致。

### S5 清理

F10 wart（`--managed` 双写）顺手修；version 提示行（F9）不再动。

## 4. 分解（TDD 串行）

### p4c-1 后端小项（S4+S5）
测试先行：L2 `TestServeUpdateOverrideFlags`（serve 装配层：--update-api 指向 fake releasesrv → runner.Check has_update=true；trust-host 生效）+ wart 修复的既有测试回归。
**退出**：go test 全绿 + smoke_w1/w2p3/w4/w4p2_diag 本地回归绿。

### p4c-2 supervisor（S1）
测试先行（`go test -tags gui ./internal/guiapp/`，headless 安全——不 import wails 的文件才能进测试编译面）：
- L1 决策矩阵表驱动：startup×alive×pending 全排列 → Action；WaitHealthy 超时 → OnState(failed)；
- L2 集成（linux CI 可跑）：假 exe = shell 脚本（写 serve.json + 起 /api/status 假 HTTP）→ 杀之 → pending 文件就位 → Tick 判 SpawnSuccessor → Spawn 真拉脚本 → WaitHealthy 返回版本；无 pending 不拉。
实现后接 wails 薄线（Run 装配 + tooltip 三态：默认 `GHydra vX — GitHub 加速器` / pending `更新已安装，正在重启生效…` / 恢复默认）。
**退出**：L1+L2 绿；ci-gui.yml 加 `go test -tags gui` step（linux）。

### p4c-3 前端契约 + 卡片逻辑（S2 + F7/F8）
测试先行：
- types.ts 补 `UpdateInfo`/`UpdateStatus`/`ApiStatus.update?`；golden.test.ts 补 `update_status.json` import+断言（五级向上 import 已是惯例）；
- `src/lib/updateState.ts` 纯函数卡片状态机 + vitest 表驱动：
  `hidden(503) / idle / checking / uptodate / available(info) / applying(status.update pct) / restarting(曾 applying+断连) / done(重连版本跳变) / failed(error)`；
  browser vs shell 模式的 restart 后文案分叉（S2）；
- `src/lib/configForm.ts`：patch diff → 预期 requires_restart 提示集（cdn 不在列）+ vitest。
**退出**：vitest + tsc + vite build 绿（/tmp 全新目录 npm ci 铁律）。

### p4c-4 Settings 页 UI + 壳接线（S2/S3 落 UI）
- 更新卡片：版本行（SSE）+ 检查更新按钮（cached 徽章）+ changelog + 「下载并安装」→ 进度（SSE status.update）→ 断连「重启中…」→ 重连横幅「已更新到 vX（Boost 已恢复）」/ browser 指引文案；
- 运行参数表单：5 字段 + requires_restart 徽章（「重启 daemon 生效」）+ cdn 即时生效提示；
- guiapp：WindowRuntimeReady ExecJS 注入 + supervisor Loop 启动。
**退出**：tsc/vitest/build 绿；`go build -tags gui`（win CGO=0 交叉冒烟）绿。

### p4c-5 smoke_w4p2_p4.sh（挂进 ci.yml selfupdate-smoke job，复用 setup 双版本构建）
- **场景 A（核心：两段式全链）**：serve --update-api releasesrv 起 → `POST /api/update/apply` → 轮询 /api/update/status 至 pending_boot → **serve 进程退出**（serveAlive false）→ 断言 update.json `pending_version=1.0.1 且未 confirmed` 且未回滚 → 脚本级模拟壳：spawn `serve --managed` → 轮询 /api/status（serve.json 每轮重读）→ version=1.0.1 + update.json confirmed；
- **场景 B（p4a 持久化黑盒）**：POST /api/config 设 rules_url/listen → 停 serve → 重拉 → GET /api/config 新值生效；
- **场景 C**：`ghydra version` 含「检查更新」行（D8）。
CI 每场景独立 step（失败步骤名即定位，W4 模式）；三平台。
**退出**：CI 全绿 + 回归铁律（既有 smoke 全集同轮绿）。

### p4c-6 GUI 渲染走查（W3 方法论，沙箱可做）
serve --gui-dist + 内置浏览器逐状态截图：更新卡片 idle/available/applying/restarting（用 fake releasesrv 驱动真状态）/done + 运行参数表单 + requires_restart 徽章。
**退出**：截图走查无布局/交互缺陷；发现的问题当场修。

## 5. 测试计划（冻结，先于实现）

| 层 | 对象 | 用例 | 落点 |
|---|---|---|---|
| L1 | supervisor 决策内核 | startup×alive×pending 矩阵 → Action 全排列；healthy 超时→failed；pending 空→不复活 | guiapp `-tags gui` |
| L1 | updateState.ts | 卡片 8 态转移表驱动；browser/shell 文案分叉 | vitest |
| L1 | configForm.ts | patch diff → requires_restart 集合（cdn 例外） | vitest |
| L2 | serve update override | --update-api/--update-trust-host → runner 命中 fake；空旗标零行为变化 | cmd/ghydra |
| L2 | supervisor 集成 | 假 exe 死亡+pending → spawn+WaitHealthy 真链路；无 pending 不拉 | guiapp `-tags gui`（linux） |
| L2 | golden 双端 | update_status.json ↔ UpdateInfo/UpdateStatus TS 断言（补齐 F7 欠账） | golden.test.ts |
| L3 smoke | p4 全链路 | 场景 A 两段式（apply→exit→pending 落盘→继任 BootHook→confirmed）/ B config 持久化 / C version 提示行 | smoke_w4p2_p4.sh × 3 平台 |
| L3 真机走查 | GUI 渲染 | 更新卡片全状态 + 参数表单截图走查 | 沙箱 serve --gui-dist |

回归面（改装配铁律）：smoke_w1 / smoke_w2p3 / smoke_w4 / smoke_w4p2_diag 全集绿。

## 6. 风险

| # | 风险 | 缓解 |
|---|---|---|
| R1 | ExecJS/RuntimeReady 时序在真机 webview2 与 webkit 差异 | 注入失败静默 + 手动路径永存；healthy 后补注；走查覆盖 |
| R2 | supervisor goroutine 调 wails setter 的线程面 | OnState 只调公开 setter（v3 内部封送主线程）；spikecheck 模式同源 |
| R3 | PRoot segfault 拖慢本地验证 | rc=139 重试包装（p1_verify 模式）+ 机械验证先行、CI 平台证明 |
| R4 | pending_boot 不可见导致卡片状态误判 | 卡片不以 pending_boot 为锚，用「曾 applying+断连」判定（F12） |
| R5 | 继任拉起撞 Windows 端口 TIME_WAIT 迁移 | Probe 每轮重读 serve.json（F6 语义），healthy 判定跟端口走 |
| R6 | 前端 golden import 已 5 级，新增文件继续踩布局坑 | /tmp 全新目录 npm ci（node_modules 不搬） |

## 7. 退出标准（p4c = W4p2 代码面收官）

1. L1/L2 全绿（含 `-tags gui` 测试进 ci-gui）；
2. smoke_w4p2_p4 三场景三平台 CI 绿 + 既有 smoke 全集回归绿；
3. vitest + tsc + vite build 绿（含 golden update 断言补齐）;
4. GUI 走查截图过审；
5. 托盘 tooltip 三态逻辑就位（真机验收项归 P5 §7 清单）。

## 8. 拍板记录

**① 更新后自动恢复接管**：**已拍板（2026-09-14）＝不做，更新后默认关**；完成横幅带恢复指引（见 S2 修订）。
（壳启动自动拉起 daemon 是 D1 冻结设计，不设拍板，直接做。）
