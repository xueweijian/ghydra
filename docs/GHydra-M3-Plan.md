# GHydra M3 实施方案 · 产品化与安全（GUI + 更新体系 + 分发）

> 状态：**已定稿**（2026-09-13 起草，同日五项拍板落定 + MITM 移出二次拍板，见 §5/D6）。
> 前置：M2 全部交付（main=99caaf3 前为 8dea1c0，v0.5.0-beta.1 已发布，四平台 artifact）。
> PRD 映射：§M3「产品化与安全」；退出标准：全新 Windows 机器双击安装→一键加速→clone+push+Release 三件事零配置完成。

## 0. 背景与硬约束

- **体积红线已取消（2026-09-13 用户拍板）**：安装包不设上限，实测记录进发布说明供参考（CLI 当前 10–11MB）。不改变壳选型——Wails 拍板的核心理由是纯 Go 单一技术栈 + CI 免装 Rust + GUI 与引擎解耦。
- **Wails 版本现状（2026-09-13 API 核实）**：v3 = beta.20（2026-09-10，日均一版逼近 3.0），官方口径 "API is stable"。**2026-09-13 用户拍板：只用 v3 做壳**（覆盖 DevWorkflow §6 原「beta 不用」规则）——v2 路线作废（无原生托盘 #1010）。纪律：锁死 beta.20，升级须 CI 三平台全绿，3.0 正式版后迁移。
- **v2 已知短板**：无原生托盘（wailsapp/wails#1010，同进程 "basically impossible"）——W0 spike 终局（拍板：spike 顺序 A1→A2→C）。
- **M2 交付盘点（M3 的地基）**：serve/on/off/get/status/doctor/git/ssh 全 CLI 面；channel Router（三态熔断 + doctor 驱动恢复）；get 下载器（A/B 择路 + Range 续传 + 段轨迹）；gitcfg/sshcfg 快照恢复；TS Worker 模板；fakesite + drill 黑盒演练 CI 化。
- **MITM 移出 v1.0（2026-09-13 二次拍板，D6）**：同日两拍——初拍进 v1.0（否决"移出"建议），W1 收官后复盘改判**移出**。直接原因：W2 设计展开后发现工作量（CA 体系三平台信任库 + 动态签发 + h2 下游 + 改写器 + 合规面）远超预期，而它解决的唯一真问题（浏览器 CONNECT 流量无 B 兜底）有更便宜的替代（`ghydra get` CLI 已有完整 A/B 切换）。MITM 与核心加速链路（通道A、insteadOf、get 下载器）**零耦合**，属单一场景可选增强。详见 D6。

## 1. 设计决策（D1–D7）

### D1 · 进程模型：hybrid 单二进制，GUI 只是 daemon 的薄客户端

- `ghydra`（默认命令）= GUI 入口：拉起/连接常驻 daemon，自身不含任何加速逻辑。
- `ghydra serve` = headless（服务器/CI 场景，现役）。
- 开机自启自启的是 GUI 入口；加速可用性**永不依赖 GUI 存活**（daemon 独立进程，GUI 崩 = 重开窗口）。
- 单二进制理由是省第二份 Go runtime + 安装/更新路径单一，非体积（红线已取消）。

### D2 · 壳与托盘：Wails v3（2026-09-13 用户拍板：只用 v3）

- **v3 = beta.20 锁定**（2026-09-13 拍板，覆盖 DevWorkflow §6 原决策规则 #2）。v2 路线（v2.13/v2.15）作废——v2 无原生托盘（#1010）。
- **三件套全部 v3 内建（W0 已核实 beta.20 API）**：
  - 托盘 = `SystemTray`（进程内，AttachWindow 点击唤起/隐藏窗口、SetMenu 菜单、SetTooltip）；
  - 单实例 = `Options.SingleInstance`（UniqueID + OnSecondInstanceLaunch 回调 → 首实例把面板带到前台）；
  - 自启 = `AutostartManager`（win Run 键 / macOS SMAppService 或 LaunchAgent / XDG autostart——与原自研方案同机制，**自研方案作废**，拍板 #5 随 v3 决策被原生实现取代）。
- spike 判定矩阵 = 三件套 × Windows/macOS（Linux 尽力）；CI 机检部分 = linux xvfb 冒烟（栈启动/窗口/托盘创建/autostart 往返，`gui/spikecheck`）；交互行为（可见性/点击语义）留真机清单。
- v3 平台级翻车的逃生门 = syncthing 零壳（C）不变。

### D3 · 控制 API（engine/api）：REST + SSE，本机安全模型

- `127.0.0.1:9801/api/*`（serve mux 扩展）：`GET /status`、`POST /on|/off`、`POST /doctor/run`、`GET|POST /config`、`POST /git|/ssh/{enable,disable}`、`GET /get/progress`、SSE `/api/events`。
- ~~**MITM 端点**~~：**已随 D6 二次拍板降级**——W1 已按原设计实现 `/api/mitm/*` 桩（status/enable/disable/uninstall-cert）并锁进 golden 契约，**桩保留、v2.0+ 启用**（删除需双端改 golden，收益为零）。
- **安全模型（syncthing 式）**：绑 127.0.0.1 + Host 头校验（防 DNS rebinding）+ **写操作必须带本机 token**（daemon 启动生成，serve.json 0600，GUI 读取）。只读接口免 token。
- serve 直接托管前端 dist → C 兜底与 Wails 壳共用同一前端。

### D4 · 规则热更新（F6 上半）

- `rules.json` v1：域名清单（PAC 生成源）、B 通道默认端点、冻结种子 IP 表、Worker 白名单。（~~MITM 域名单~~ 随 D6 移出，v2.0+ 再议）
- **ed25519 签名 + 版本号单调递增**（防回滚投毒）+ 原子替换 + 编译期内嵌冻结版兜底。
- 签名工具 `scripts/sign-rules`；私钥离线、CI 用 secret、公钥冻结进二进制；预留 key rotation 字段。
- 拉取**走自身双通道**——更新链路自己先受益。

### D5 · 程序自更新（F6 下半）

- 版本检查：GitHub Releases API 走自身通道；下载**复用 `ghydra get`**（A/B 择路、Range 断点、ETag 一致性）→ minisign 风格验签。
- 安装：**双目录**（current/staged）+ 启动器交换 + 失败自动回滚；Windows 运行中 exe 不可覆写 → rename 交换。
- daemon 重启由 GUI/launcher 编排；on/off 快照对账保证切换中途崩溃可恢复。

### D6 · F4 MITM 移出 v1.0（2026-09-13 二次拍板）——降级为 v2.0+ 候选

**拍板轨迹**：初拍进 v1.0（否决"移出"建议）→ W1 收官、W2 设计展开时复盘改判**移出**。改判理由：

1. **工作量与收益严重失衡**：完整功能面 = CA 体系（三平台信任库安装/卸载/残留校验）+ per-install 随机 CA + 动态 leaf 签发（LRU）+ 下游 h2 + URL 改写器 + 合规声明与二次确认 UI——远超初拍时"几千行"的量级估计；而它解决的唯一真问题（浏览器 CONNECT 流量无 B 兜底）已有更便宜的替代（`ghydra get` CLI 已交付完整 A/B 择路 + 断点续传 + 一致性校验）。
2. **与核心链路零耦合**：通道A、insteadOf、get 下载器、控制 API、GUI 壳均不依赖它——砍除不触碰任何已交付功能。
3. **全项目最大的信任/安全面**：解密用户 TLS 与产品"默认不解密"卖点相抵触；配套防御（默认关+二次确认+一键卸载+白名单外透传回归断言+doctor 标注）本身又是一大块工作量。

**处置**：
- **代码桩保留**：W1 已交付的 `/api/mitm/*` 桩端点、golden 契约（Go 侧 + vitest 11 用例）、前端 `Mitm.tsx` 骨架页全部保留（占位态，页面标注 v2.0 计划）——v2.0+ 启用时接口/契约/页面零改动，删除反而要双端改 golden，纯负收益。
- **PRD F4 降级**：v2.0+ 候选（P3）；浏览器 B 兜底缺口回归**已知限制**记入风险表，v1.0 缓解 = GUI 空态 + README 引导 `ghydra get`。
- **恢复触发器**：真实用户反馈"必须在浏览器下载"达到可感知量级；或 B 通道生态出现透明 CONNECT 型代理（无需 URL 可见）时重估。
- 原风险 M3-R7（CA 私钥）/M3-R8（误伤非白名单域）/M3-R9（h2 长尾）随之转入 v2.0+ 风险池。

**v2.0+ 启用时的规格输入**（初拍 D6 设计全文备查，git 历史本版本之前）：
per-install 随机 CA（平台密钥保护、不同步）+ 三平台一键信任/卸载无残留校验；按 host 唯一 leaf + 内存 LRU；仅白名单 github 系域终结 TLS、白名单外原样透传（回归断言进 CI）；下游 ALPN h2 / 上游 h1.1（dev-sidecar 模式）；A 源头故障时白名单域浏览器流量改写 B 前缀（复用 channel 决策器）；googleapis 资源替换规则热更；CSP nonce 仅 stretch。

### D7 · 范围锁定（拍板记录）

- 浏览器扩展：**不进 M3**（独立产品面，M4+ 评估）。注：原理由"MITM 已兜浏览器 B 场景"随 D6 改判失效——浏览器 B 兜底缺口现为已知限制（见 D6 处置），v1.0 缓解 = 引导 `ghydra get`。
- 代码签名：**v1.0 无签名 + README 信任教学**（拍板）；公测反响后决策是否购 OV。
- v3：**M3 窗口内维持 v2.13**（拍板）。
- 自启：**自研**（拍板）。

## 2. 五周分解（节奏不绑日历周，模块完成即推进；W0/W1 已完成，MITM 已移出见 D6）

### W0 · 壳 spike（✅ 已完成：c37283f CI 全绿 + 9c1a78e 报告，v3 beta.20 拍板）
| 任务 | 交付 | 测试 |
|---|---|---|
| wails v3（beta.20 锁定）+ SolidJS 最小壳，三平台 CI 出包（ubuntu apt libgtk-3-dev/libwebkit2gtk，win 自带 WebView2，mac xcode） | CI GUI job 模板 | 三平台 build 绿 |
| 三件套矩阵：A1/A2/C × win/mac | spike 分支 | 托盘常驻/自启/单实例唤醒逐项勾验 |
| 自启自研三平台 + 单实例锁与唤醒 | `engine/autostart`、`engine/singleinstance` | 平台单测 + 手动清单 |
| **产出：`docs/GHydra-M3-Spike.md` + 壳终局 + CI 模板合入** | | |

### W1 · 控制 API + 前端骨架（✅ 已完成：4f0a29f + 962c0e6，CI 首跑全绿含 api-smoke 12 断言 + golden 双端锁定）
| 任务 | 交付 | 测试 |
|---|---|---|
| engine/api：REST 全套 + SSE + token + Host 校验 + MITM 端点 | `engine/api/` | httptest 全覆盖 + rebinding/token 攻击面用例 |
| serve 托管 dist；SolidJS 骨架（布局/路由/types） | `gui/` | types 与 API schema 一致性测试 |
| Wails 壳与浏览器模式跑同一 dist | 双模式冒烟 | — |

### W2 · F6 规则热更（rules.json + 签名管线）
| 任务 | 交付 | 测试 |
|---|---|---|
| rules.json v1：域名清单（PAC 生成源）、B 通道默认端点、冻结种子 IP 表、Worker 白名单 | `engine/rules` 扩展 | schema 单测 |
| ed25519 签名 + 版本号单调递增（防回滚投毒）+ 原子替换 + 编译期内嵌冻结版兜底 | `engine/rules` + `scripts/sign-rules` | 假规则源（fakesite 扩展）：验签失败拒/断网回退/版本回滚拒，全 CI 化 |
| 拉取走自身双通道（更新链路自己先受益） | 下载器接线 | 拉取路径断言 |
| 密钥管理：私钥离线、CI 用 secret、公钥冻结进二进制、rotation 字段 | scripts + 文档 | — |

### W3 · GUI 功能面
| 任务 | 交付 | 测试 |
|---|---|---|
| 状态页：doctor 双列可视化 + 通道/熔断实时（SSE） | gui | API 级 E2E |
| 一键加速 on/off + git/ssh 开关 + 失败回滚提示 | gui | API 级 E2E |
| get 下载进度页 + 设置页 + 空态错误态引导（**浏览器下载场景引导 `ghydra get`**——D6 已知限制的缓解面） | gui | API 级 E2E + 手动清单 |
| Mitm 页保持 v2.0 占位（桩契约已锁，不删路由） | gui | golden 回归 |

### W4 · 自更新 + 打包分发 + v1.0

**W4p1（✅ 已完成：ce652dc CI 18/18 全绿；实现笔记 `GHydra-M3-W4-Notes.md`）**
| 任务 | 交付 | 测试 |
|---|---|---|
| 自更新：检查（Releases API 走自身通道）/下载（复用 `ghydra get`）/验签/双目录交换/回滚 + 重启编排 | `engine/selfupdate` + `engine/internal/{fsx,minisign}` + CLI `ghydra version/update` | L1 24 + L2 12 + L3 黑盒四场景 × 三平台 CI ✓ |
| 信任链：sha256 + checksums.txt minisign 双防线（U1-U8）/ 版本单调 / 域白名单 / 尺寸顶 / 无签名拒 | `release.go`/`checksums.go`/`semver.go` | 官方 minisign 工具互操作 fixture ✓ |
| 交换 + 崩溃恢复 + 启动自检 ×3 自动回滚 + daemon 版本对账 | `swap.go`/`state.go`/`update.go` | 崩溃三态 + 两路径自动回滚 + 真二进制升级演练 ✓ |
| 测试基建：fake Release server（攻击模式开关）/ fakebin 真子进程 / 冒烟四场景拆独立 CI step | `scripts/releasesrv`、`scripts/smoke_w4.sh` | CI 三平台矩阵 ✓ |

**W4p2（待开工）**
| 任务 | 交付 | 测试 |
|---|---|---|
| `ghydra gui` 子命令转正（build tag 隔离 wails，CLI 构建不沾 gtk） | cmd + gui 模块 | 三平台 CI 出包 |
| Windows NSIS 安装器（开始菜单/可选自启/卸载清理：注册表+gitcfg+sshcfg+快照残留）、macOS DMG、Linux tar | `packaging/` | CI 出包 + 安装/卸载冒烟（卸载先还原系统代理） |
| winget manifest + scoop（Homebrew tap 延后，拍板 #9） | 分发渠道 | manifest 校验 |
| 诊断包一键导出脱敏（F8） | cmd `ghydra diag` | 脱敏断言（命中即拒写） |
| release.yml：ldflags 版本注入 + 产物名对齐 SelectAsset + checksums 签名 | CI | tag 出包演练 |
| flags 持久化 + GUI 更新流（拍板 #10） | config + GUI | API 级 E2E |
| **全新 Windows 机器验收**：双击→一键→clone+push+Release 零配置 | 用户真机 | 退出标准④ |
| v1.0.0 tag + 公测发布 | Release | 全绿后 |

## 3. 风险增量

| # | 风险 | 对策 |
|---|---|---|
| M3-R1 | v3 beta 平台级翻车（托盘/单实例在真机不工作） | 逃生门不变：syncthing 零壳（C）；spike 真机清单首验 |
| M3-R2 | CI 跑 wails 构建（linux webkit2gtk cgo / win webview2） | W0 产出 CI 模板并验证，失败即知 |
| M3-R3 | v3 日更 churn / beta→beta API 破坏 | 锁死 beta.20 精确版本；升级必须 CI 三平台全绿才合入；3.0 正式版后一次性迁移 |
| M3-R4 | 自更新半态破坏（更新中崩溃/断电） | 双目录 + 回滚 + ensureReconcile 对账；演练 CI 化 |
| M3-R5 | localhost API 攻击面（rebinding/本机恶意进程） | 127.0.0.1 + Host 校验 + 写操作 token + 只读限流 |
| M3-R6 | 规则签名私钥泄露 | 离线保管 + 版本单调 + rotation + 公钥冻结进二进制 |
| M3-R7 | **浏览器 B 兜底缺口**（MITM 移出的已知代价，D6）：A 源头级故障时浏览器点下载无兜底 | v1.0 缓解 = GUI 空态 + README 引导 `ghydra get`；恢复触发器见 D6；原 CA 私钥/误伤非白名单域/h2 长尾三风险随功能转入 v2.0+ 风险池 |
| M3-R8 | 无签名 SmartScreen 拦小白 | 拍板：v1.0 无签名 + README 教学；数据好再买 OV |

## 4. 验收口径

- 退出①（spike）：✅ 已达成（W0：三件套矩阵报告 + 壳终局拍板 + GUI CI 模板三平台绿）。
- 退出②（控制 API）：✅ 已达成（W1：REST+SSE+token+Host 校验，api-smoke 12 断言 CI 化，golden 双端锁定）。
- 退出③（F6 规则热更）：假规则源（验签失败拒/断网回退/版本回滚拒）全部 CI 化。
- 退出④（GUI + 发布）：API 测试全绿 + 用户真机全新 Windows 双击→一键加速→clone+push+Release 零配置 + 自更新（升级/回滚）CI 化 + v1.0.0 四平台安装包 + 分发渠道 manifest 就绪 + 公测发布。

## 5. 拍板记录

| # | 议题 | 结论 |
|---|---|---|
| 1 | F4 MITM 移出 v1.0？ | 初拍（2026-09-13 上午）：不移出。**二次拍板（2026-09-13 W1 收官后，用户主导复盘）：移出 v1.0，降级 v2.0+ 候选**——W2 设计展开后发现工作量远超预期（CA 三平台信任库+动态签发+h2+改写+合规面），收益限于浏览器 B 兜底单一场景，且与核心链路零耦合；替代 = 引导 `ghydra get`；代码桩保留。详见 D6 |
| 2 | 代码签名 | v1.0 无签名 + README 信任教学 |
| 3 | 托盘架构 | spike 按 A1→A2→C（W0 已终局：v3 原生三件套） |
| 4 | Wails 版本 | **只用 v3**（2026-09-13 用户拍板，覆盖原规则 #2）：beta.20 锁定，升级须 CI 三平台全绿，3.0 正式版后迁移 |
| 5 | 开机自启 | 初拍自研 ~100 行；随 v3 拍板被 AutostartManager 原生实现取代（D2） |
| 6 | win 安装位置（W4） | `%LOCALAPPDATA%\Programs\GHydra` 用户级——免 UAC，自更新免提权（2026-09-13 用户同意建议） |
| 7 | 新版启动失败（W4） | 自动回滚 + bad_version 跳过（"永不自毁"核心，2026-09-13 用户同意建议） |
| 8 | prerelease（W4） | 默认跳过，`--pre` 显式开启（beta 阶段正好演练升级链，2026-09-13 用户同意建议） |
| 9 | Homebrew tap（W4） | v1.0 延后，公测有 mac 用户呼声再做（2026-09-13 用户同意建议） |
| 10 | GUI 更新流（W4） | 进 v1.0，放 W4p2 末尾，砍线不伤 CLI 主链（2026-09-13 用户同意建议） |

W4 release 签名钥匙（与 rules 钥匙分离）：公钥冻结 `engine/selfupdate/keys.go`，私钥离线 `shared/ghydra-keys/ghydra-release.key`（key_id F0716070E4E793E7）。 |
