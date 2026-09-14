# GHydra M3-W4p2 设计 · 打包分发与 v1.0 收官

> 状态：设计中（2026-09-13 起草，待用户拍板 §8）。前置：W4p1 自更新引擎已交付（ce652dc，CI 18/18 绿）。
> PRD 映射：M3 交付物「v1.0 公测（开源）」；WBS 最后一周。
> 本文档冻结后按 Phase 串行实施，每 Phase 测试先行（L1/L2/L3 分层）。

## 0. 范围与目标

把一个**功能完整但只能 `go build` 的工程**变成**普通用户可安装、可更新、可卸载的产品**。

不在范围（明确排除）：
- Linux GUI 构建（Linux 产物 CLI-only，GTK/CGO 矩阵复杂度不值 v1.0；登记 M4+ 池）
- 代码签名证书（v1.0 拍板过：无签名 + README 信任教学；minisign 签名即信任链）
- 自动更新强推（只手动检查 + 用户确认）
- macOS 公证（notarization 需开发者账号，README 教学绕行）

## 1. 现状盘点（W4p2 的地基，全部已就绪）

| 能力 | 状态 | 位置 |
|---|---|---|
| 自更新引擎（下载/验签/交换/回滚/状态机） | ✅ W4p1 | `engine/selfupdate` + `internal/minisign` |
| CLI 更新命令 | ✅ `ghydra update check/apply/rollback` | `cmd/ghydra` |
| fake Release server（L2 测试基建） | ✅ | selfupdate 测试 |
| GUI 壳（wails v3 beta.20 + SolidJS 六页） | ✅ W0/W3 | `gui/`（独立 module） |
| 前端 dist 与 serve 托管共用 | ✅ | `gui/dist` go:embed |
| 托盘/单实例/自启（wails v3 原生） | ✅ W0 spike 验证 | `gui/` spikecheck |
| daemon 托管拉起/对账 | ✅ M3-W1 | `ghydra on/off` + serve.json |
| CI 三平台矩阵 + gui 独立 workflow | ✅ | `ci.yml` / `ci-gui.yml` |
| release 签名钥匙对（与 dev 规则钥匙分离） | ✅ 指纹 F0716070E4E793E7 | `shared/ghydra-keys/`（用户侧） |

## 2. 设计决策

### D1 GUI 进主二进制：build tag 隔离，单二进制达成（对 M3-Plan D1 的落地）

**目标形态**：一个 `ghydra(.exe)`，`ghydra gui` 子命令直接可用。

- `gui/` 从独立 module **并回主 module**（`go.mod` 合一），壳代码进 `engine/gui/`，全部文件 `//go:build gui`。
- 默认构建（无 tag）：`ghydra gui` 打印「此构建未包含 GUI，请下载带 GUI 的发行版」——子命令桩 + 构建期 `guiSupported=false`（ldflags 或 tag 内常量）。
- 发布构建 `-tags gui`：
  - **Windows：CGO_ENABLED=0 纯 Go**（go-webview2，W0 已验证）✅ 主力平台最优路径
  - **macOS：CGO + WKWebView**（runner 自带 Xcode）
  - **Linux：v1.0 不构建 GUI 版**（GTK CGO 矩阵不值），CLI-only
- 体积参考：W0 实测壳 11.8–12.7MB；体积红线已取消（2026-09-13 拍板），预期 win 全量 ~20MB 可接受。

**子命令行为**（`ghydra gui`）：
1. 探活 daemon（serve.json 端口，`/api/status`）；
2. 未运行 → 按托管机制拉起（与 `ghydra on` 同路径，`--managed`）；
3. 启动 wails 壳（前端 dist go:embed；连 `location.origin`? 壳模式回落 9801——W3 已定）；
4. 关窗 = 隐藏到托盘（W0 已验 RegisterHook + Cancel）。

### D2 版本注入：ldflags 三变量，`-X main.*`（W4p1 实证唯一可行形式）

```
-X main.version=<tag> -X main.commit=<sha> -X main.date=<ISO8601>
```
- main 包持有变量；`ghydra version` / selfupdate 比对 / `/api/status` / GUI 关于页统一消费。
- 本地 `go build`（无注入）= `dev`——selfupdate 对 dev 构建直接拒绝 apply（W4p1 已有防线，补测试锁定）。
- 踩坑回放：本沙箱 go1.23.9 上 `-X importpath.Var` **静默失效**，必须 `-X main.Var`；CI 与本地脚本统一封装在 `scripts/build.sh`。

### D3 `engine/diag`：一键脱敏诊断包

`ghydra diag --out <path.zip>`（默认 `ghydra-diag-<yyyymmdd>.zip`）：

| 内容 | 来源 | 脱敏 |
|---|---|---|
| 版本/commit/平台 | runtime | 无 |
| doctor_log 近 7 天 | store | 保留（GitHub IP 非隐私，排障必需） |
| 调度器池快照 | /api/status | 保留 IP |
| 通道/规则版本/更新状态 | 各模块 | 无 |
| serve 日志尾 200 行 | 日志文件 | 正则脱敏 |
| 托管快照**存在性** | store | 只导出「是否接管」，**不导出原值**（用户原代理 URL 可能带凭据） |

**绝不导出**：api-token、系统代理原值、环境变量、家目录外路径。
**脱敏函数**：`Redact(s)`——token 模式（`[A-Za-z0-9_-]{20,}` 在 token 上下文）、家目录→`~`、用户名。表驱动 L1。

**泄漏扫描测试**（L2）：测试植入已知 token/代理凭据 → 生成 diag → 断言 zip 内全文检索 0 命中。此测试是 D3 的核心验收。

### D4 packaging：三平台产物形态

| 平台 | 产物 | 要点 |
|---|---|---|
| Windows | `ghydra_<v>_windows_amd64.zip`（便携）+ `..._setup.exe`（NSIS） | NSIS 走 CI windows runner（自带 makensis） |
| macOS | `ghydra_<v>_darwin_universal.zip` | amd64+arm642 合 1（lipo）；无公证，README 教学 |
| Linux | `ghydra_<v>_linux_{amd64,arm64}.tar.gz` | CLI-only |

**NSIS 安装器（安全关键逻辑写死）**：
1. 安装：Program Files + 开始菜单 + PATH 可选；
2. **卸载序列**：先执行 `ghydra off --wait`（还原系统代理/删托管快照）→ 超时 10s 强杀 → **保留** `%USERPROFILE%\.ghydra\`（用户数据 + 崩溃恢复凭证）→ 删程序文件；
3. `ghydra off` 失败时卸载器**中止并弹窗**指引（绝不静默留下半接管态——M1-D4 原则的安装器版）；
4. 自启注册交给产品内功能（wails AutostartManager），安装器不碰注册表 Run 键（单一事实源）。

### D5 release.yml：tag 驱动全链路

```
push tag v* → build(3平台×产物, ldflags) → NSIS(linux交叉免,win原生) → smoke(产物可执行性) 
→ checksums.txt → Release draft → 通知本地签名
```

- 产物命名/布局 golden 锁定（测试断言 release 清单结构，防 selfupdate 下载路径漂移）。
- selfupdate 期望的布局（W4p1 已定）与 release 产物**同名同构**——`ghydra update` 直接吃自家 Release。
- **签名流程（拍板点 §8-1）**：推荐 CI 出包到 Release draft → 沙箱/本地用 `shared/ghydra-keys` 私钥跑 `engine/cmd/sign-release`（复用 internal/minisign 的 ED 签名逻辑）→ 上传 `checksums.txt.minisig` + 每产物 `.minisig` → publish。私钥永不出用户设备/沙箱，CI 零 secret。

### D6 flags 持久化：SQLite config 表

- `config(key TEXT PRIMARY KEY, value TEXT, updated_at)`；serve 启动合并优先级：**显式 CLI flag > 持久化值 > 默认**（判断「显式」用 flag.Visit）。
- 落点：`--cdn`、`--rules-url`、`--rules-interval`、`--listen`（GUI Settings 改的就是这几项）。
- 写入路径：`POST /api/config`（已存在 cdn 热更，扩展到 flags 集）+ CLI `ghydra config set`。
- 托管拉起（`ghydra on` → serve --managed）时**不传 flags** → 自动吃持久化值——GUI 改完重启生效闭环。

### D7 GUI 更新流：/api/update/* + Settings 页

| 端点 | 语义 |
|---|---|
| `GET /api/update/check` | 同步返回 latest（缓存 5min），含 changelog 摘要 |
| `POST /api/update/apply` | 异步单飞（409 语义同 rules refresh），走 selfupdate.ApplyPlan |
| `GET /api/update/status` | 状态机快照（idle/checking/downloading/verifying/swapping/pending-boot/failed） |

- SSE：status 帧携带 update 子对象（复用现有 1s diff 通道）。
- GUI：Settings →「检查更新」→ 版本卡片（当前/最新/changelog）→「下载并安装」→ 进度 → 「重启生效」按钮（触发 daemon 重启 + 壳自重启）。
- golden 扩展：`update_status.json` 三态（idle/进度中/失败）。

### D8 W4p1 遗留收口

- `ghydra version` 输出含 update 通道提示（有新版时 `ghydra update check` 引导）；
- pending-boot 状态在 GUI 托盘 tooltip 可见（W4p1 状态机的最后一块可观测面）。

## 3. Phase 分解（串行，每 Phase 退出才进下一个）

### P1 GUI 并轨（D1+D2）

> **实施记录（2026-09-14，本地全绿后 push）**：
> - 搬迁：`gui/` 独立 module 消灭——main.go→`internal/guiapp/gui.go`（`//go:build gui`，`func Run()`）、
>   assets/frontend 随迁、spikecheck→`cmd/spikecheck`（同样 `//go:build gui`）；icon 单一事实源化
>   `internal/guiapp/icon.go`（**修复潜伏 bug：spikecheck 的 assets/icon.png 从未入库**，CI checkout 后
>   embed 必挂——被 paths filter 掩盖至今）。
> - `ghydra gui`：`gui_on.go`（tag gui→guiapp.Run）/`gui_off.go`（!gui→引导+退出码 2，表驱动测试）。
> - **go.mod go 1.25.0 + `toolchain go1.25.0` 锁定**：wails beta.20 硬性要求 go≥1.25.0；
>   **go1.26.8 runtime 在 PRoot/Android 沙箱必现 Segmentation fault（rc=139），go1.25.0 间歇性**——
>   GOTOOLCHAIN=auto 会挑最新版，必须 toolchain 指令锁 1.25；本地所有 go 命令套 rc=139 重试包装
>   （最高实测 7 次重试通过）。CI setup-go "1.25" 全部对齐。
> - MVS 连锁升级：sqlite v1.34.5→v1.44.3、x/crypto→v0.53.0、x/sys→v0.46.0（wails 依赖图抬轿）——
>   全量测试通过确认无行为回归。
> - golden 测试 import 4 级→**5 级**（目录深一层），12 处；vitest 23/23 + tsc + vite build 本地绿。
> - **Windows gui 变体 CGO=0 交叉编译实证通过：23.3MB 单二进制**（dist/.gitkeep stub 使无 npm 产物
>   也可编译；CI ci.yml windows 目标加 gui 变体冒烟 + linux 产物 ldflags `-X main.Version` 注入验证）。

- gui module 并回主 module；`engine/gui/` + build tag；`ghydra gui` 子命令（无 tag 桩 + 有 tag 真身）；scripts/build.sh 统一 ldflags；CI：win `-tags gui` CGO=0 构建 + spikecheck 冒烟进主 ci.yml。
- **退出**：win/mac `-tags gui` 构建绿 + 冒烟绿 + `ghydra version` 三变量注入断言 + 无 tag 构建的桩提示断言。

### P2 diag（D3）
- engine/diag + Redact + zip 组装 + 泄漏扫描测试 + `ghydra diag` 子命令 + doctor 集成（排障一键化）。
- **退出**：L1 表驱动 + L2 泄漏扫描（植入 token 0 命中）+ smoke（真跑一次 diag 断言 zip 结构）。

> **实施记录（2026-09-14，本地全绿后 push）**：
> - `engine/diag`：`Redact`（URL 凭据/Authorization/token 键值对/裸 32 hex/家目录→~/用户名→<user>；
>   **RE2 无 lookbehind/lookahead**——用户名用 \b 词边界、32hex 用捕获组兜两侧防 40hex sha 误伤）+
>   `Build`（entry 名稳定排序输出可复现）。
> - **双防线**：①Redact 正则 ②`Secrets` 精确串扫描——**必须在压缩前明文 entry 上做**
>   （zip deflate 会打散明文，压缩后字节扫描漏报）；命中即报错拒导（`TestBuildRejectsUnredactableSecret`）。
> - `ghydra diag`：store（doctor 汇总/接管**存在性**，绝不读快照原值）+ serve.json 端口事实源
>   （fetchStatusJSON ?token=，token 只用于本机请求不写包）+ serve.log 尾 200 行 + selfupdate 摘要；
>   裸环境不崩（首次安装报障场景）；api-token 原文只作扫描基准。
> - 泄漏扫描测试三件套：`TestLeakScanZeroHit`（包级核心验收）/ `TestDiagCmdEndToEndLeakScan`
>   （端到端植入）/ smoke_w4p2_diag.sh（运行时+停服+泄漏三场景，CI api-smoke job）。
> - smoke 教训重演：serve 前台不写 serve.json（--managed 才写）——手工构造运行态（W2 同款）。


### P3 packaging + release.yml（D4+D5）
- NSIS 脚本（卸载序列安全关键）；release.yml tag 驱动；产物命名 golden；`v0.9.0-rc1` tag 演练全链路（含签名脚本）。
- **退出**：rc1 tag → Release draft 产物齐 + checksums + minisig 全链路绿 + selfupdate 从 rc1 升 rc2 演练通过（fake 或真 tag）。

> **实施记录（2026-09-14，本地六产物装配全绿后 push）**：
> - **命名修订（实现服从 selfupdate 冻结实现）**：D4 表 `ghydra_<v>_windows_amd64.zip` 为笔误——
>   SelectAsset 前缀 = `ghydra-<goos>-<goarch>.`（连字符无版本）；GUI 变体 `-gui.zip` 后缀
>   与该前缀的 "." 不匹配，CLI/GUI 互不误选 ✓。**darwin 分架构 tar.gz 替代 universal**：
>   SelectAsset 无 universal fallback，改冻结代码不值（D4 表作废以此为准）。
> - 归档布局：根级单文件 `ghydra`/`ghydra.exe`（= selfupdate Files 白名单；PlanSwap
>   ErrStagingIncomplete 兜底）。package.sh 内置解包回读自检。
> - **NSIS 红线四条**（scripts/check-nsi.sh 静态断言，CI windows job 门禁）：①卸载先
>   `off --wait` ②off 失败弹窗可中止 ③绝不碰 `%USERPROFILE%\.ghydra\`（崩溃恢复凭证，
>   误删=用户原代理值永久丢失）④不预置自启。**offExitCode 契约**：--wait 模式恢复失败
>   exit 3（NSIS 依据）；交互模式保持 exit 0 历史语义（表驱动测试）。
> - **BusyBox ash 两个诡异坑（package.sh gobuild 实证）**：①if 条件上下文里 segfault
>   子进程的 `$?` 读成 **0**（命令替换上下文才正常 139）→ 改为产物存在性+rc 双判定；
>   ②裸赋值 `out=$(...)` 会把 rc=139 传播给赋值语句本身 → `set -e` 直接杀脚本 →
>   改 if 条件上下文赋值。
> - **遗留到 P4/D7**：gui 变体的自更新寻址——gui 二进制跑 update 会按前缀匹配到 CLI
>   归档，交换后 gui 消失；D7 需给 Updater 加 variant（寻址 `-gui` 后缀资产）。


### P4 flags 持久化 + GUI 更新流（D6+D7+D8）
- config 表 + 合并优先级 + /api/config 扩展 + /api/update/* + SSE + Settings 页 + 托盘 tooltip。
- **退出**：L1 优先级矩阵 + L2 fake-release 端到端（check→apply→pending→boot）+ GUI 真机渲染走查（W3 方法论）+ golden 双端。

> ### P4 实施规划（2026-09-14 落盘，测试计划冻结）
>
> **现状盘点**（读码结论）：
> - serve flags 全集在 `main.go serveCmd`；`POST /api/config` 仅 cdn 热更（ConfigPatch{CDN}）；
> - `spawnServe`（ghydra on 拉起）硬编码传 `--listen 127.0.0.1:port --doctor-interval 1h --managed`；
> - store 有 `schemaExtras` 追加 DDL 钩子（rules_state.go 为范本）；
> - selfupdate.Updater 无 variant 概念（gui 二进制 check 会按前缀误选 CLI 归档——W4p1 遗留）；
> - Refresher 无 SetURL/SetInterval（rules_url/interval 热更需改内部循环）；
> - 双 v 根源：release.yml/package.sh 注入 `main.Version=tag`（含 v）+ 展示层 `v%s`。
>
> **D6 细化**：
> - `config(key TEXT PRIMARY KEY, value TEXT, updated_at INTEGER)`，随 schemaExtras 注册（`store/config.go`）；
> - key 集：`cdn` / `rules_url` / `rules_interval`（Go duration 串）/ `listen` / `doctor_interval`；
> - 合并优先级：**flag.Visit 判显式 > 持久化值 > 默认**；`spawnServe` **不再传 --doctor-interval**——
>   改为 serve 内部规则：**managed 模式下 doctor-interval 默认 1h**（保持首启行为不变）；
> - `/api/config` 扩展（ConfigPatch 加 4 字段）：`cdn` 热更（现状）；其余写表 + 响应带
>   `requires_restart: [...]`（**P4 不做 refresher 热更 API**——改内部 tick 循环的 risk 大于收益，
>   rules_url 日常用户不动）；**改 listen 时校验回环地址**（安全模型不变：仅 127.0.0.1）；
> - CLI `ghydra config set|get|list`（直接写表，提示重启生效）；
> - GUI Settings「运行参数」区从只读改可编辑（cdn/rules_url/interval/doctor），保存后显示
>   「重启 daemon 生效」徽章（v1.0 不做自动重启按钮——off{shutdown} 会撤接管，语义不匹配）。
>
> **D7 细化（关键架构决策——serve 内 apply 的自重启死结）**：
> - `ghydra update apply`（CLI，外部进程编排 Stop→swap→Start）W4p1 已测绿，**保持不动**；
> - **serve 内 apply**：daemon 进程内不能编排自己的 Stop/Start（Stop=自杀，后续步骤死掉）。
>   定案**两段式**：
>   ① daemon 内：Check→下载→验签→swap（rename 对运行中进程安全，W4p1 实证）→写 pending
>     状态→SSE 推 `pending-boot`→**优雅退出**（shutdownCh，managed 退出 hook 恢复系统代理）；
>   ② **继任拉起归 GUI 壳**（D1 hybrid 本义：壳是 daemon 的 supervisor）——壳 watch daemon
>     死亡 + update.json pending → spawn 自身 exe `serve --managed` → 轮询 /api/status 直到
>     新版本起来（BootHook 自检→Confirm）→ 显示「已更新到 vX」。
>   CLI-only 用户路径不变：`ghydra update apply`（外部编排）或重启后 `ghydra on`。
> - **端点**：`GET /api/update/check`（同步 + 5min 内存缓存，`cached` 标记）/
>   `POST /api/update/apply`（异步单飞 409，语义同 rules refresh）/ `GET /api/update/status`
>   （状态机快照）；
> - **状态机**（SSE status 帧新增 `update` 子对象，复用 1s diff 通道）：
>   `idle → checking → downloading → verifying → swapping → pending-boot`；
>   `checking → uptodate`（无更新）；任意态 → `failed`（带 error 串）；
> - **variant 寻址**（W4p1 遗留收口）：Updater 加 `Variant string`（""=CLI / "gui"）；
>   release.yml 构建 gui 变体时 `-X main.buildVariant=gui`（package.sh 参数化）；
>   SelectAsset variant=gui 时前缀改 `ghydra-<goos>-<goarch>-gui.`（darwin-amd64 无 gui 资产
>   → 清晰报错）；gui zip 根级单文件与 Files=[ghydra(.exe)] 自洽（P3 已定型）；
> - **双 v 修复**：package.sh `VER="${1#v}"` strip + main.go init normalize（TrimPrefix 防手滑）。
>
> **D8 细化**：
> - `ghydra version` 加静态提示行「检查更新: ghydra update check」——**不做网络 check**
>   （version 是 BootHook 自检子进程契约，必须快）；
> - pending-boot 托盘 tooltip：壳从 SSE update 帧取态，pending 时
>   `SetTooltip("GHydra — 更新已安装，重启后生效")`。
>
> **P4 内部拆步（串行）**：
> - **p4a D6 落地**：store/config.go + serve 合并 + spawnServe 去 flag + /api/config 扩展 +
>   CLI config 子命令 + golden（config.json 扩展）；
> - **p4b D7 后端**：variant + 双 v + /api/update/* + SSE update 帧 + serve 内 updater 装配 +
>   apply→pending→退出链路 + golden（update_status.json + status.json 增 update）；
> - **p4c D7 前端 + D8**：Settings 更新卡片（版本/检查/进度/重启按钮态）+ 壳 supervisor 拉起 +
>   托盘 tooltip + version 提示行 + smoke_w4p2_p4.sh + GUI 渲染走查。
>
> **P4 测试计划（冻结，先于实现）**：
> | 层 | 对象 | 用例 |
> |---|---|---|
> | L1 | flags 合并 | 显式>持久化>默认 5 key × 3 来源全排列；flag.Visit 判显式；managed 下 doctor 默认 1h |
> | L1 | config 表 | Set/Get 幂等、updated_at 单调、坏 duration 拒绝 |
> | L1 | SelectAsset variant | gui 前缀命中/CLI 不误选 gui 资产/darwin-amd64+gui 报错 |
> | L1 | semver 展示 | 双 v normalize（v 前缀注入后输出单 v） |
> | L2 | /api/config | cdn 热更即时生效；rules_url 写表+requires_restart；listen 非回环拒绝 |
> | L2 | /api/update/check | fake release：正常/无更新（uptodate）/5min 缓存命中（cached=true） |
> | L2 | /api/update/apply | 单飞 409；状态机全转移（idle→…→pending-boot）；失败态带 error |
> | L2 | apply 退出链 | fake release 端到端：apply→swap→pending 写盘→serve 优雅退出 |
> | L2 | 持久化重启 | POST /api/config 设 cdn/listen → kill serve → 重拉 → 新值生效 |
> | L2 | golden 双端 | config.json/update_status.json/status.json(update 字段) ↔ types.ts |
> | L3 smoke | p4 全链路 | smoke_w4p2_p4.sh：config set→重启生效 + update apply 端到端 + version 提示 |
> | L3 真机 | GUI 渲染 | Settings 更新卡片/进度/重启拉起 走查（W3 方法论：serve --gui-dist 截图） |

### P5 v1.0 收官
- CHANGELOG + README 发布态（安装教学/信任教学/更新教学）；`v1.0.0` tag → 发布；**用户 Windows 真机验收清单**（§7）。
- **退出**：验收清单全过 + 真机更新演练（v1.0.0→v1.0.1 假版本，走完 apply→boot→confirmed，再 rollback 演练一次）。

## 4. 测试计划（冻结）

| 层 | 对象 | 关键用例 |
|---|---|---|
| L1 | Redact 脱敏 | token/家目录/用户名/代理凭据 各 ≥3 向量；非敏感不误伤（IP 保留） |
| L1 | flags 合并 | 显式>持久化>默认 全排列矩阵；flag.Visit 判「显式」 |
| L1 | gui 桩 | 无 tag 构建 `ghydra gui` 退出码+文案；guiSupported=false |
| L2 | diag 泄漏扫描 | 植入 token/代理 URL → zip 全文 0 命中（**核心验收**） |
| L2 | update API | fake release server：check 缓存/apply 单飞 409/状态机全转移 |
| L2 | 持久化重启 | set flag → 停 daemon → `ghydra on` 重拉 → 生效 |
| L2 | release 产物 golden | 清单结构/命名/selfupdate 可寻址 |
| L3 smoke | gui 构建 | win CGO=0 `-tags gui` 产物 spikecheck（托盘/单实例/自启往返） |
| L3 smoke | NSIS | CI windows makensis 构建 + 脚本静态断言（卸载段含 `ghydra off`） |
| L3 真机 | 用户验收 | §7 清单（不可替代） |

## 5. 风险表

| # | 风险 | 概率 | 缓解 |
|---|---|---|---|
| R1 | NSIS 卸载时 `ghydra off` 失败留半接管态 | 中 | 卸载器中止+弹窗指引（绝不静默）；快照文件保留供手工恢复；真机验收必测 |
| R2 | wails v3 beta API 变动破坏构建 | 低 | go.mod 锁 beta.20；升级=独立 CI 全绿才合入（W0 纪律） |
| R3 | module 合并引发 import 环 | 中 | gui 只依赖 engine 下游；P1 第一步先做依赖方向审查测试 |
| R4 | macOS universal lipo/公证问题 | 中 | 无公证（拍板）；lipo 失败回退双架构分离产物 |
| R5 | 签名私钥丢失/泄露 | 低 | 用户侧 shared 目录；**提醒用户离线备份**；泄露走 key rotation（规则钥匙机制已有 key_id，天然支持） |
| R6 | SmartScreen/未签名告警劝退用户 | 高 | README 置顶信任教学（minisign 验证命令一行）；发布说明带指纹 |
| R7 | CI 产物与 selfupdate 布局漂移 | 中 | 产物命名 golden + rc1→rc2 真升级演练（P3 退出标准） |

## 6. 退出标准（W4p2 = M3 = v1.0）

1. `v1.0.0` tag → release.yml 全链路绿，三平台产物 + checksums + minisign 签名齐；
2. 用户全新 Windows 机器验收清单通过（§7）；
3. 真机更新演练：v1.0.0→v1.0.1（apply→boot→confirmed）+ rollback 演练各一次通过；
4. CI 全绿（含 `-tags gui` 矩阵扩展）；
5. README/CHANGELOG 发布态完成，公测可推。

## 7. 用户参与点（不可替代，提前预告）

**Windows 真机验收清单**（P5，约 30 分钟）：
1. 全新环境（或卸载重装）下载 setup.exe 安装（过 SmartScreen）；
2. `ghydra on` / GUI 一键接管 → clone + push + Release 下载三场景；
3. 托盘常驻：关窗隐藏/双击唤起/单实例二次启动唤醒；
4. 开机自启开关往返；
5. `ghydra update check` + GUI 更新流到 v1.0.1 演练 + rollback；
6. `ghydra diag` 导出检查无隐私；
7. 卸载：系统代理还原 + 无残留进程 + 用户数据保留提示。

**发布时刻**：推 `v1.0.0` tag 需开魔法（沙箱 push 通道）。

## 8. 待用户拍板（开工前）

| # | 决策 | 推荐 | 备选 |
|---|---|---|---|
| 1 | release 签名流程 | **CI 出包到 draft → 本地/沙箱签名 → 上传**（私钥不出用户侧，CI 零 secret） | CI secret 存私钥全自动（便利但扩大泄露面） |
| 2 | Linux GUI | **v1.0 不做**（CLI-only，M4+ 池） | 挤进 v1.0（+GTK CGO 矩阵，估 +2 Phase 工作量） |
| 3 | NSIS 自启默认 | **不预置**（首启 GUI 引导开） | 安装时可选勾选 |
| 4 | v1.0.0 发布节奏 | **rc1 演练（P3）→ 真机验收 → v1.0.0** | 直接 v1.0.0（省一轮，风险高） |

## 9. 预估

P1（2-3 轮 CI）→ P2（1-2 轮）→ P3（3-4 轮）→ P4（2-3 轮）→ P5（真机 1 轮 + 发布 1 轮）。
按 W4p1 节奏（单日 13 轮 CI 收敛）预估 **2-3 个工作 session 到 v1.0 tag**。
