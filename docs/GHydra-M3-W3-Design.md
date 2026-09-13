# GHydra M3-W3 设计 · GUI 功能面（消费端开发）

> 状态：设计稿（待用户过目后开工）。
> 前置：W2 已收官（e1b991f CI 全绿）——rules API、golden 双端契约、SSE 五帧全部就绪。
> 定位：**前端纯消费端**——不新增加速逻辑，只把后端已有能力变成可用的界面。
> PRD 映射：M3-Plan §2 W3 表；MITM 页保持 v2.0 占位（D6）。

## 1. 范围与非目标

**范围**：6 页功能化（状态仪表盘/加速/规则/下载/设置 + MITM 占位不动）、后端最小增量（接管态查询 + 版本字段）、纯逻辑层抽取与测试。

**非目标**：
- 不引入 jsdom/@testing-library（重依赖、Solid 组件测试收益低——页面逻辑抽纯函数层测）；
- 不做视觉设计稿级 UI（现有深色卡片风 34 行 CSS 增量演进，不推翻）；
- 不动 MITM 页与桩契约（D6 拍板）；
- 不做自更新 UI（W4）；
- config 热更仍仅 cdn——doctor_every_s/doctor_repo 是启动旗标，设置页**只读展示**，不做假开关。

## 2. 消费矩阵（页面 ↔ 后端就绪面）

| 页面 | REST | SSE 帧 | 现状 → W3 |
|---|---|---|---|
| 状态（仪表盘） | status、doctor/summary | status/conn/doctor | 骨架→深化：doctor 24h 表、熔断详情、连接流过滤 |
| 加速 | on/off、git/*、ssh/* | status（含新 takeover） | JSON dump→状态化大开关 |
| **规则（新页）** | rules、rules/refresh | status（rules 摘要） | 无→整页新写（W2 数据消费主阵地） |
| 下载 | get/start、get/progress | get | 骨架→错误态分类+空态教学 |
| 增强 | mitm/status | — | 占位合规，不动 |
| 设置 | config（GET+POST cdn） | — | 补连接测试/只读卡/关于卡 |

## 3. 后端增量（唯一改动面，~60 行）

### 3.1 接管态查询（Boost 页核心缺口）

现状：on/off 是命令式，**前端无法得知"当前是否接管中"**——Boost 页只能盲按。

方案：`ApiStatus` 增加 `takeover` 字段（status 单一真相模式的自然延伸，SSE 1s diff 帧自动推送变化）：

```json
"takeover": {"on": true, "since": "2026-09-13T12:00:00Z"}
```

- `on` = sysproxy_snapshot 表存在行（与 ensureReconcile 崩溃对账同源判定，零新语义）；
- `since` = taken_at 列（store 新增 `LoadTakeoverState() (ok bool, takenAt time.Time, err error)`，单查询）；
- 不带 mode：快照 setting_json 存的是**原值**（恢复凭证），mode 信息本就不该存进去；UI 需要时以本地记忆兜底，v1 不做。
- 崩溃对账竞态：ensureReconcile 在子命令入口触发，serve 常驻时快照残留可能滞后——`takeover.on=true` 但系统代理已被手工还原。可接受：UI 显示"已接管"+ off 按钮即可恢复，语义无害。

### 3.2 版本字段

`ApiStatus` 增加 `version string`（`var Version = "dev"`，CI release 打 tag 时 ldflags 注入——W4 自更新的地基，现在占位）。

### 3.3 golden 双端同步（改形状的显式成本）

Go 侧 golden status.json + types.ts ApiStatus + vitest 断言 + smoke_w1 断言（status 帧带 `"takeover":{"on":false` / `"version":"`）四处同步——golden 流程已成型，照 W2 phase 2 走。

## 4. 页面规格

### 4.1 规则页（新，W2 数据主消费阵地）

```
┌ 版本卡 ────────────────────────────┐
│ v10 · [disk] · 2026-09-13 生成      │
│ 有效期至 2026-10-28（45d 内绿/过期黄 stale 徽章）│
│ [手动刷新] ← POST refresh；running 时 spinner     │
├ 刷新卡 ────────────────────────────┤
│ 上次结果：ok（七分类人话文案）· 上次时间 · 下次调度 │
├ 域名清单表（domain 列 + 匹配说明）  │
├ B 通道端点列表（cdn_endpoints）     │
└ 种子 IP 概览（seed_ips 域名分组计数，默认折叠）│
```

- 七分类文案映射（`lib/rulesText.ts` 纯函数）：ok→"已更新"/fetch_err→"拉取失败（网络）"/sig→"验签失败（拒绝应用）"/rollback→"版本回滚攻击（拒绝）"/ff→"版本快进（拒绝）"/schema→"内容不合规（拒绝）"/size→"超限（拒绝）"——**把 W2 的防御语义变成用户可读语言**；
- 手动刷新：POST → busy 409 提示"刷新进行中"；成功后轮询 GET /api/rules 直到 `running:false`（或 2s×15 次超时提示稍后自动）；
- stale 徽章消费 `expires_at` vs now（本地时钟，只做展示判定不做信任判定——信任判定在引擎）。

### 4.2 状态页（仪表盘深化）

- **doctor 24h 表**：`GET /api/doctor/summary?hours=24` onMount + doctor SSE 帧到达后刷新——mode×scenario 行（checks/passed/reachable/avg_ttfb_ms）；[立即体检] 按钮（POST doctor/run，busy 处理）；
- 通道 A 卡补：backoff_mult、open_until（熔断打开时显示恢复倒计时）、b_suppressed（B 抑制计数+原因）；
- IP 池空态：scheduler off 时显示引导文案而非空表；
- 连接流：host 过滤输入框 + 加速/直连过滤——纯前端 filter，不动数据层。

### 4.3 加速页（状态化重写）

```
┌ 大开关卡 ─────────────────────────┐
│ ● 已接管（自 12:00）    [恢复直连]  │
│ 或 ○ 未接管  [一键加速 PAC] [系统代理模式] │
├ 失败提示区：applyTakeover 错误 →   │
│ "接管失败已回滚，系统代理未改动"    │
├ Git insteadOf 卡：enabled 徽章 +   │
│ cdn 显示 + [启用][停用]            │
└ SSH 443 卡：同构                    │
```

- 接管态由 `status.takeover` SSE 驱动（不本地记状态）；
- on 失败（ErrBusy 之外的错误）：展示后端 error 文案 + 回滚语义说明（apicore 保证失败全回滚）；
- git/ssh：进页拉 status 显示当前态，操作后重新拉取；两个卡禁用态 = daemon 未装配（503）时灰化+提示。

### 4.4 下载页

- 错误分类：401→token 引导横幅（复用全局事件）；400/404→URL 问题；5xx→daemon 问题；
- **空态教学（D6 缓解面）**：无任务时显示引导卡——"浏览器下载 GitHub Release 不走加速？复制链接到本页，或终端 `ghydra get <url>`"；
- recent 表：channel 徽章（A 绿/B 蓝）、rate、耗时列；进行中条目 SSE 驱动（已有）。

### 4.5 设置页

- 现有 daemon/token/cdn 保留；加 [测试连接]（GET status → 显示版本+延迟）；
- 只读卡：调度器/doctor 周期/doctor 仓库（来自 config，标注"启动参数，修改需重启"）；
- 关于卡：版本（status.version）+ 仓库链接。

## 5. 测试计划

| 层 | 对象 | 工具 |
|---|---|---|
| 纯函数单测 | lib/rulesText（七分类）、lib/format（字节/时长/百分比）、stale 判定、连接流 filter 逻辑 | vitest（node 环境，零新依赖） |
| 契约 | takeover/version 字段双向锁定 | golden_test.go（Go）+ golden.test.ts（vitest） |
| 黑盒 | serve 托管 dist + status 帧含新字段 + /api/rules 页数据链 | smoke_w1 扩 2 断言 |
| 手动清单 | 真机三件事（clone/push/Release）+ 各页交互 | W4 Windows 真机验收合并 |

**纪律**（Pitfalls §9 教训）：后端字段改动后**回归既有 smoke 全集**（w1+w2p3 连跑）。

## 6. 实施顺序（模块完成即推进，每 phase 本地绿即推 CI）

1. **W3a 后端增量**：takeover/version 字段 + store.LoadTakeoverState + golden 双端 + smoke 扩断言（半天量）
2. **W3b 规则页**：lib/rulesText + Rules.tsx + nav（最厚一页，W2 收益显性化）
3. **W3c 加速页状态化 + 状态页深化**（doctor 表/熔断详情/过滤）
4. **W3d 下载/设置收尾** + 全局空态错误态 pass
5. 收官：vitest 全绿 + go test 全绿 + smoke 连跑 + CI

## 7. 风险

| # | 风险 | 对策 |
|---|---|---|
| W3-R1 | status.json golden 迁移漏改一处（四处同步） | golden 双端机制本身兜底——漏改必红 |
| W3-R2 | 接管态与真实系统状态竞态（手工改系统代理后 UI 滞后） | on=true + off 可恢复，语义无害（§3.1 已论证）；真同步=轮询 sysproxy.Current()，v2 再议 |
| W3-R3 | SSE status 帧 1s diff 粒度下 takeover 变化延迟 | 可接受（秒级）；on/off 响应本身是同步确认 |
| W3-R4 | 前端状态散落（各页自拉 vs SSE） | 约定：**帧有的用帧**（status/conn/doctor/get），帧没有的才 REST 拉取（rules 详情/doctor summary/git/ssh status/config） |
