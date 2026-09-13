# GHydra M3 实施方案 · 产品化与安全（GUI + 更新体系 + 分发）

> 状态：设计中（2026-09-13 起草，待拍板点 §5 定稿）。
> 前置：M2 全部交付（main=8dea1c0，v0.5.0-beta.1 已发布，四平台 artifact）。
> PRD 映射：§M3「产品化与安全」；退出标准：全新 Windows 机器双击安装→一键加速→clone+push+Release 三件事零配置完成。

## 0. 背景与硬约束

- **体积红线已取消（2026-09-13 用户拍板）**：安装包不设上限，实测记录进发布说明供参考（CLI 当前 10–11MB）。这解除了 upx/激进裁剪等 hack，但**不改变壳选型**——Wails 拍板（2026-09-12）的核心理由是纯 Go 单一技术栈 + CI 免装 Rust + GUI 与引擎解耦，体积只是附带论据。
- **Wails 版本现状（2026-09-13 API 核实）**：v3 = beta.20（2026-09-10，日均一版的节奏逼近 3.0），**仍未转正** → DevWorkflow §6 决策规则第 2 条生效：**锁 v2.13+**。规则 #1（正式版或 RC 稳定 ≥2 个月）在 M3 窗口内大概率赶不上，v1.0 按 v2.13 出。
- **v2 的已知短板**：无原生托盘（wailsapp/wails#1010，同进程跑 systray "basically impossible"，macOS 主线程冲突）——托盘是 spike 三件套之一，这正是 W0 要终局的问题（D2）。
- **M2 交付盘点（M3 的地基）**：serve/on/off/get/status/doctor/git/ssh 全 CLI 面；channel Router（三态熔断 + doctor 驱动恢复）；get 下载器（A/B 择路 + Range 续传 + 段轨迹）；gitcfg/sshcfg 快照恢复；TS Worker 模板（免费档实测全绿）；fakesite + drill 黑盒演练 CI 化。

## 1. 设计决策（D1–D6）

### D1 · 进程模型：hybrid 单二进制，GUI 只是 daemon 的薄客户端

- `ghydra`（默认命令）= GUI 入口：拉起/连接常驻 daemon，自身不含任何加速逻辑。
- `ghydra serve` = headless（服务器/CI 场景，现役）。
- 开机自启自启的是 GUI 入口；加速可用性**永不依赖 GUI 存活**（daemon 独立进程，GUI 崩 = 重开窗口）。
- 单二进制的理由是省第二份 Go runtime + 安装/更新路径单一，非性能非性积（体积红线已取消）。

### D2 · 壳与托盘：三路线 spike 终局（W0）

| 路线 | 构成 | 已知风险 |
|---|---|---|
| **A1（首选）** | Wails v2.13 窗口（无托盘）+ **托盘独立进程**（daemon 内可选 systray 模块，getlantern/systray 系纯 Go） | 两个事件循环分属两进程，规避 #1010；macOS 需验证 detached 进程的 NSStatusItem 可行性 |
| A2 | Wails v2.13 + energye/systray 同进程 | macOS 已知冲突；Windows 报告可行 |
| B | Wails v3 beta.20 原生托盘 | 破坏规则字面（beta 不用）；churn 大（日更）。仅当 spike 期满足规则 #1 才转正 |
| **C（兜底）** | syncthing 零壳：无 Wails，serve 托管 Web UI + systray + 浏览器面板 | 体验降级但零壳风险 |

- spike 顺序 **A1 → A2 → C**，判定矩阵 = 三件套（托盘常驻 / 开机自启 / 单实例唤醒）× Windows/macOS。
- **单实例与唤醒**：TCP 端口 bind（serve 已有）+ GUI 层锁文件；唤醒 = Windows 命名事件/窗口消息，unix domain socket。
- **开机自启：自研 ~100 行**（win Run 键 / macOS LaunchAgent plist / Linux XDG autostart）——三平台注册表/配置写入在 sysproxy、gitcfg、sshcfg 里已是熟路，不引库。

### D3 · 控制 API（engine/api）：REST + SSE，本机安全模型

- `127.0.0.1:9801/api/*`（serve mux 扩展）：`GET /status`（通道/熔断/sched 池/doctor 摘要）、`POST /on|/off`、`POST /doctor/run`、`GET|POST /config`、`POST /git|/ssh/{enable,disable}`、`GET /get/progress`。
- `GET /api/events`（SSE）：通道切换、熔断、探针完成——Event 体系现成。
- **安全模型（syncthing 式）**：绑 127.0.0.1 + Host 头校验（防 DNS rebinding）+ **写操作必须带本机 token**（daemon 启动生成，serve.json 0600，GUI 读取）。只读接口免 token 便于诊断。
- serve 直接托管前端 dist → C 兜底模式与 Wails 壳**零成本共用同一前端**。

### D4 · 规则热更新（F6 上半）

- `rules.json` v1：域名清单（PAC 生成源）、B 通道默认端点、冻结种子 IP 表、Worker 白名单。
- **ed25519 签名 + 版本号单调递增**（防回滚投毒）+ 原子替换 + 编译期内嵌冻结版兜底（断网用）。
- 签名工具 `scripts/sign-rules`；私钥离线生成、CI 用 secret、公钥冻结进二进制；预留 key rotation 字段。
- 拉取**走自身双通道**（A 直连 raw / B 自有 worker 前缀）——更新链路自己先受益。

### D5 · 程序自更新（F6 下半）

- 版本检查：GitHub Releases API 走自身通道（worker path 形态）。
- 下载**复用 `ghydra get` 下载器**（A/B 择路、Range 断点、ETag 一致性）→ minisign 风格 ed25519 验签。
- 安装：**双目录**（current/staged）+ 启动器交换 + 失败自动回滚；Windows 运行中 exe 不可覆写 → rename 交换技巧。
- daemon 重启由 GUI/launcher 编排；on/off 快照对账（ensureReconcile）已保证切换中途崩溃可恢复。

### D6 · 范围裁剪（拍板点，见 §5）

- **F4 MITM 建议整体移出 v1.0**（P1→P2，M4 再评估）：v1.0 的品牌就是「不解密」；工作量 = 整个 FastGithub 核心；零配置体验不需要它。
- **浏览器场景（M2-D1 遗留）**：维持现状 + GUI 体检报告明确归因（"直连不可用时浏览器无 B 兜底，全行业现状"）；浏览器扩展不进 M3。
- 分发渠道无签名的 SmartScreen 提示是体验项 → 拍板证书。

## 2. 五周分解（节奏不绑日历周，模块完成即推进）

### W0 · 壳 spike（终局周）
| 任务 | 交付 | 测试 |
|---|---|---|
| wails v2.13 + SolidJS 最小壳，三平台 CI 出包（ubuntu apt libgtk-3-dev/libwebkit2gtk，win 自带 WebView2，mac xcode） | CI GUI job 模板 | 三平台 build 绿 |
| 三件套矩阵：A1/A2/C × win/mac | spike 仓库/分支 | 托盘常驻/自启/单实例唤醒逐项勾验 |
| 自启自研三平台 + 单实例锁与唤醒 | `engine/autostart`、`engine/singleinstance` | 平台单测 + 手动清单 |
| 体积实测（参考数据，无红线） | 记录进 spike 报告 | — |
| **产出：`docs/GHydra-M3-Spike.md` + 壳终局拍板 + CI 模板合入** | | |

### W1 · 控制 API + 前端骨架
| 任务 | 交付 | 测试 |
|---|---|---|
| engine/api：REST 全套 + SSE + token + Host 校验 | `engine/api/` | httptest 全覆盖 + rebinding/token 攻击面用例 |
| serve 托管前端 dist；SolidJS 骨架（布局/路由/types） | `gui/` | types 与 API schema 一致性测试 |
| Wails 壳与浏览器模式跑同一 dist | 双模式冒烟 | — |

### W2 · GUI 功能面
| 任务 | 交付 | 测试 |
|---|---|---|
| 状态页：doctor 双列可视化 + 通道/熔断实时（SSE） | gui | API 级 E2E |
| 一键加速 on/off + git/ssh 开关 + 失败回滚提示 | gui | API 级 E2E |
| get 下载进度页（段轨迹/速率/通道可视化，Segments 现成） | gui | API 级 E2E |
| 设置页（worker 端点/TOKEN/探测参数）+ 空态错误态引导 | gui | 手动清单 |

### W3 · F6 规则热更新 + 自更新
| 任务 | 交付 | 测试 |
|---|---|---|
| rules.json v1 + 签名工具 + 拉取管线（自身通道）+ 原子替换 + 冻结兜底 | `engine/rules` 扩展 | 假规则源（fakesite 扩展）：验签失败拒/断网回退/版本回滚拒，全 CI 化 |
| 自更新：检查/下载/验签/双目录交换/回滚 + 重启编排 | `engine/selfupdate` + launcher | CI 升级演练：旧版→新版→回滚，daemon 状态对账 |
| 密钥管理：私钥离线、公钥冻结、rotation 字段 | scripts + 文档 | — |

### W4 · 打包分发 + 安全收尾 + v1.0
| 任务 | 交付 | 测试 |
|---|---|---|
| Windows NSIS 安装器（开始菜单/可选自启/卸载清理：注册表+gitcfg+sshcfg+快照残留）、macOS DMG、Linux tar | `packaging/` | CI 出包 + 安装冒烟 |
| winget manifest + Homebrew tap（tap 仓库）+ scoop | 分发渠道 | manifest 校验 |
| 诊断包一键导出脱敏（F8） | cmd `ghydra diag` | 脱敏断言 |
| **全新 Windows 机器验收**：双击→一键→clone+push+Release 零配置 | 用户真机 | 退出标准② |
| v1.0.0 tag + 公测发布 | Release | 全绿后 |

## 3. 风险增量

| # | 风险 | 对策 |
|---|---|---|
| M3-R1 | v2 托盘同进程冲突（macOS 已知死） | A1 独立进程架构规避；spike 首验；C 兜底 |
| M3-R2 | CI 跑 wails 构建（linux webkit2gtk cgo / win webview2） | W0 产出 CI 模板并验证，失败即知 |
| M3-R3 | v3 日更逼近 3.0 的诱惑 | 锁 v2.13 精确版本；转正后按规则 #1 在 M3 内一次性迁移（独立迁移周，不混功能） |
| M3-R4 | 自更新半态破坏（更新中崩溃/断电） | 双目录 + 回滚 + ensureReconcile 对账；演练 CI 化 |
| M3-R5 | localhost API 攻击面（rebinding/本机恶意进程） | 127.0.0.1 + Host 校验 + 写操作 token + 只读限流 |
| M3-R6 | 规则签名私钥泄露 | 离线保管 + 版本单调 + rotation 字段 + 公钥冻结进二进制 |
| M3-R7 | 范围蔓延（MITM/浏览器扩展） | D6 裁剪拍板先行，v1.0 范围锁死 |
| M3-R8 | 无签名 SmartScreen 拦小白 | 拍板证书；v1.0 可先无签名 + README 信任教学 |

## 4. 验收口径

- 退出①（spike）：三件套矩阵报告 + 壳终局拍板 + GUI CI 模板三平台绿。
- 退出②（GUI）：API 测试全绿 + **用户真机**全新 Windows 双击→一键加速→clone+push+Release 零配置。
- 退出③（F6）：规则热更新（假源/断网/回滚）与自更新（升级/回滚）全部 CI 化。
- 退出④（发布）：v1.0.0 四平台安装包 + 分发渠道 manifest 就绪 + 公测发布。

## 5. 拍板点（定稿前需用户确认）

1. **F4 MITM 移出 v1.0**（P1→P2，M4 再评估）？——建议：是。
2. **代码签名证书**：v1.0 无签名 + README 教学（建议），还是先买 OV（~¥700–2000/年）？
3. **托盘架构**：spike 按 A1→A2→C 顺序，有预置偏好吗？
4. **v3 处理**：M3 窗口内维持 v2.13（v3 转正后按规则 #1 迁移）——确认？
5. **自启自研**（~100 行）vs 引库——建议自研。
