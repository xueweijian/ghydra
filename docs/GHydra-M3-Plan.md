# GHydra M3 实施方案 · 产品化与安全（GUI + MITM + 更新体系 + 分发）

> 状态：**已定稿**（2026-09-13 起草，同日五项拍板落定，见 §5）。
> 前置：M2 全部交付（main=99caaf3 前为 8dea1c0，v0.5.0-beta.1 已发布，四平台 artifact）。
> PRD 映射：§M3「产品化与安全」；退出标准：全新 Windows 机器双击安装→一键加速→clone+push+Release 三件事零配置完成。

## 0. 背景与硬约束

- **体积红线已取消（2026-09-13 用户拍板）**：安装包不设上限，实测记录进发布说明供参考（CLI 当前 10–11MB）。不改变壳选型——Wails 拍板的核心理由是纯 Go 单一技术栈 + CI 免装 Rust + GUI 与引擎解耦。
- **Wails 版本现状（2026-09-13 API 核实）**：v3 = beta.20（2026-09-10，日均一版逼近 3.0），官方口径 "API is stable"。**2026-09-13 用户拍板：只用 v3 做壳**（覆盖 DevWorkflow §6 原「beta 不用」规则）——v2 路线作废（无原生托盘 #1010）。纪律：锁死 beta.20，升级须 CI 三平台全绿，3.0 正式版后迁移。
- **v2 已知短板**：无原生托盘（wailsapp/wails#1010，同进程 "basically impossible"）——W0 spike 终局（拍板：spike 顺序 A1→A2→C）。
- **M2 交付盘点（M3 的地基）**：serve/on/off/get/status/doctor/git/ssh 全 CLI 面；channel Router（三态熔断 + doctor 驱动恢复）；get 下载器（A/B 择路 + Range 续传 + 段轨迹）；gitcfg/sshcfg 快照恢复；TS Worker 模板；fakesite + drill 黑盒演练 CI 化。
- **MITM 回归 v1.0（2026-09-13 拍板，D6）**：这是本方案相对初稿的最大增量——它同时补上 M2-D1 的架构空洞（浏览器 CONNECT 流量原本 A 专属、无 B 兜底；MITM 后 URL 可见，浏览器流量可切 CDN）。

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
- **MITM 端点**：`GET /mitm/status`（CA 指纹/信任态/拦截域计数）、`POST /mitm/enable`（**携带 UI 二次确认回执**才生效）、`POST /mitm/disable`、`POST /mitm/uninstall-cert`。
- **安全模型（syncthing 式）**：绑 127.0.0.1 + Host 头校验（防 DNS rebinding）+ **写操作必须带本机 token**（daemon 启动生成，serve.json 0600，GUI 读取）。只读接口免 token。
- serve 直接托管前端 dist → C 兜底与 Wails 壳共用同一前端。

### D4 · 规则热更新（F6 上半）

- `rules.json` v1：域名清单（PAC 生成源）、B 通道默认端点、冻结种子 IP 表、Worker 白名单、**MITM 域名单**（与白名单同源，仅 github 系）。
- **ed25519 签名 + 版本号单调递增**（防回滚投毒）+ 原子替换 + 编译期内嵌冻结版兜底。
- 签名工具 `scripts/sign-rules`；私钥离线、CI 用 secret、公钥冻结进二进制；预留 key rotation 字段。
- 拉取**走自身双通道**——更新链路自己先受益。

### D5 · 程序自更新（F6 下半）

- 版本检查：GitHub Releases API 走自身通道；下载**复用 `ghydra get`**（A/B 择路、Range 断点、ETag 一致性）→ minisign 风格验签。
- 安装：**双目录**（current/staged）+ 启动器交换 + 失败自动回滚；Windows 运行中 exe 不可覆写 → rename 交换。
- daemon 重启由 GUI/launcher 编排；on/off 快照对账保证切换中途崩溃可恢复。

### D6 · F4 MITM 进 v1.0（拍板：不移出）——范围与安全边界

**功能面（P1，默认关）**：
1. **CA 体系**：安装时生成 per-install 随机 CA（私钥平台密钥保护存储，永不出设备、不随配置同步）；一键安装信任（win 证书库/mac keychain/linux NSS）+ **一键卸载并校验系统存储无残留**。
2. **动态签发**：按 host 唯一 leaf 证书，内存 LRU 缓存。
3. **拦截边界**：仅白名单 github 系域拦截终结 TLS；**白名单外 CONNECT 原样透传**（现有路径，行为零变化）。默认关；开启需 UI 二次确认（PRD F4/F8）。
4. **HTTP/2**：下游 ALPN h2（浏览器↔代理），上游 h1.1（dev-sidecar 模式，避免 h2↔h2 长尾）。
5. **浏览器 B 兜底**（MITM 的存在理由）：A 源头级故障（403/全段死）时，白名单域浏览器流量改写 B 前缀——M2-D1 表中「浏览器 HTTPS = A 专属」行在 MITM 开启时变为 A/B 可切。
6. **googleapis 资源替换**：HTML/CSS 内 googleapis 域静态资源改写国内可达镜像（规则进 rules.json 可热更）。
7. CSP nonce 注入：stretch（我们不注入脚本，仅当替换触发 CSP 拦截才需要）。

**安全/合规边界**：
- 合规声明随 F4 落地同步修订（PRD §4）："默认不解密；可选增强模式经用户显式二次确认后，仅对 github 白名单域本地终结 TLS"——用户自装 CA 的本地代理与 dev-sidecar/FastGithub 同模型。
- doctor 归因扩展：拦截率/改写率/回退次数入体检报告；MITM 开启时体检报告显著标注。
- 关闭 MITM = CA 卸载提示（可保留证书但停止拦截，用户选择）。

### D7 · 范围锁定（拍板记录）

- 浏览器扩展：**不进 M3**（MITM 已兜浏览器 B 场景，扩展是独立产品面，M4+ 评估）。
- 代码签名：**v1.0 无签名 + README 信任教学**（拍板）；公测反响后决策是否购 OV。
- v3：**M3 窗口内维持 v2.13**（拍板）。
- 自启：**自研**（拍板）。

## 2. 七周分解（节奏不绑日历周，模块完成即推进；W2/W3 MITM 是最大增量）

### W0 · 壳 spike（终局周）
| 任务 | 交付 | 测试 |
|---|---|---|
| wails v3（beta.20 锁定）+ SolidJS 最小壳，三平台 CI 出包（ubuntu apt libgtk-3-dev/libwebkit2gtk，win 自带 WebView2，mac xcode） | CI GUI job 模板 | 三平台 build 绿 |
| 三件套矩阵：A1/A2/C × win/mac | spike 分支 | 托盘常驻/自启/单实例唤醒逐项勾验 |
| 自启自研三平台 + 单实例锁与唤醒 | `engine/autostart`、`engine/singleinstance` | 平台单测 + 手动清单 |
| **产出：`docs/GHydra-M3-Spike.md` + 壳终局 + CI 模板合入** | | |

### W1 · 控制 API + 前端骨架
| 任务 | 交付 | 测试 |
|---|---|---|
| engine/api：REST 全套 + SSE + token + Host 校验 + MITM 端点 | `engine/api/` | httptest 全覆盖 + rebinding/token 攻击面用例 |
| serve 托管 dist；SolidJS 骨架（布局/路由/types） | `gui/` | types 与 API schema 一致性测试 |
| Wails 壳与浏览器模式跑同一 dist | 双模式冒烟 | — |

### W2 · MITM 地基（CA + 拦截边界）
| 任务 | 交付 | 测试 |
|---|---|---|
| CA 生成/信任安装/一键卸载（三平台）+ 私钥保护存储 | `engine/mitm/ca` | CI 集成（win 证书库 round-trip）+ 残留断言 |
| 按 host 动态 leaf 签发 + LRU | `engine/mitm/cert` | 证书链校验 + LRU 单测 |
| CONNECT 拦截开关：默认关 + 二次确认 + 白名单外透传 | `engine/proxy` 扩展 | **白名单外行为零变化**回归断言 + 默认关断言 |
| doctor 归因扩展（拦截/改写计数） | probe/store | 汇总查询 |

### W3 · MITM 深化（h2 + 改写 + 浏览器 B 兜底）
| 任务 | 交付 | 测试 |
|---|---|---|
| 下游 ALPN h2、上游 h1.1 | `engine/mitm` | 浏览器模拟（h2 client）E2E |
| 白名单域 URL 可见 → B 前缀改写（A 源头故障时，复用 channel 决策器） | channel/mitm 接线 | 故障注入：403 → 浏览器流量 ≤30s 切 B（**M2 退出标准①的浏览器版**） |
| googleapis 资源替换（规则热更） | `engine/mitm/rewrite` | 替换表单测 + 规则驱动断言 |
| （stretch）CSP nonce | — | — |

### W4 · GUI 功能面
| 任务 | 交付 | 测试 |
|---|---|---|
| 状态页：doctor 双列可视化 + 通道/熔断实时（SSE） | gui | API 级 E2E |
| 一键加速 on/off + git/ssh 开关 + 失败回滚提示 | gui | API 级 E2E |
| MITM 页：开启二次确认流程 + CA 指纹展示 + 一键卸载 + 拦截统计 | gui | E2E |
| get 下载进度页 + 设置页 + 空态错误态引导 | gui | API 级 E2E + 手动清单 |

### W5 · F6 规则热更新 + 自更新
| 任务 | 交付 | 测试 |
|---|---|---|
| rules.json v1 + 签名工具 + 拉取管线（自身通道）+ 原子替换 + 冻结兜底 | `engine/rules` 扩展 | 假规则源（fakesite 扩展）：验签失败拒/断网回退/版本回滚拒，全 CI 化 |
| 自更新：检查/下载/验签/双目录交换/回滚 + 重启编排 | `engine/selfupdate` + launcher | CI 升级演练：旧版→新版→回滚，daemon 状态对账 |
| 密钥管理：私钥离线、公钥冻结、rotation 字段 | scripts + 文档 | — |

### W6 · 打包分发 + 安全收尾 + v1.0
| 任务 | 交付 | 测试 |
|---|---|---|
| Windows NSIS 安装器（开始菜单/可选自启/卸载清理：注册表+gitcfg+sshcfg+CA+快照残留）、macOS DMG、Linux tar | `packaging/` | CI 出包 + 安装冒烟 |
| winget manifest + Homebrew tap + scoop（无签名，README 信任教学） | 分发渠道 | manifest 校验 |
| 诊断包一键导出脱敏（F8） | cmd `ghydra diag` | 脱敏断言 |
| **全新 Windows 机器验收**：双击→一键→clone+push+Release 零配置 | 用户真机 | 退出标准② |
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
| M3-R7 | **CA 私钥泄露 = 用户信任被滥用** | per-install 随机生成（不预置不同步）、平台密钥保护存储、卸载彻底、文档声明信任边界 |
| M3-R8 | MITM 误伤非白名单域/合规观感 | 白名单外透传的回归断言进 CI；默认关 + 二次确认 + 体检报告显著标注；合规措辞随 W2 修订 |
| M3-R9 | h2 兼容性长尾 | 下游 h2 上游 h1.1（dev-sidecar 已验证的模式）；问题站点 doctor 一键豁免（回到透传） |
| M3-R10 | 无签名 SmartScreen 拦小白 | 拍板：v1.0 无签名 + README 教学；数据好再买 OV |

## 4. 验收口径

- 退出①（spike）：三件套矩阵报告 + 壳终局拍板 + GUI CI 模板三平台绿。
- 退出②（MITM）：CI 化——默认关断言、白名单外透传回归、CA 安装→拦截→改写→卸载无残留 round-trip、**浏览器流量 403 注入 ≤30s 切 B**；真机二次确认流程走查。
- 退出③（GUI）：API 测试全绿 + 用户真机全新 Windows 双击→一键加速→clone+push+Release 零配置。
- 退出④（F6）：规则热更新（假源/断网/回滚）与自更新（升级/回滚）全部 CI 化。
- 退出⑤（发布）：v1.0.0 四平台安装包 + 分发渠道 manifest 就绪 + 公测发布。

## 5. 拍板记录（2026-09-13 全部落定）

| # | 议题 | 结论 |
|---|---|---|
| 1 | F4 MITM 移出 v1.0？ | **不移出**（用户拍板，否决我的建议）——进 v1.0，范围与安全边界见 D6 |
| 2 | 代码签名 | v1.0 无签名 + README 信任教学 |
| 3 | 托盘架构 | spike 按 A1→A2→C |
| 4 | Wails 版本 | **只用 v3**（2026-09-13 用户拍板，覆盖原规则 #2）：beta.20 锁定，升级须 CI 三平台全绿，3.0 正式版后迁移 |
| 5 | 开机自启 | 自研 ~100 行 |
