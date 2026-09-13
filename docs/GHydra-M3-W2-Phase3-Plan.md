# M3-W2 Phase 3 方案：refresher 拉取管线 + 毒化矩阵（2026-09-13）

前置：phase 1（签名链/schema/版本/原子写）+ phase 2（Provider 三级地板/装配//api/rules）已合入。
本文档冻结 phase 3 设计与测试计划；测试先于实现（§8）。

## 1. 目标与退出标准

- 规则从「手动落盘」升级为「自动拉取 + 全自动热生效」，全程吃自身 A/B 狗粮
- A1–A9 毒化矩阵在**真 refresher 链路**上逐条退出考试（拒绝路径 + 冻结续服务）
- 退出标准：①L1/L2 全绿 ②CI 黑盒（serve+fakesite rules 模式）绿 ③L3 真机双源 + 拔网冷启动过 ④seen_max 跨重启防重放演示

## 2. 架构与分层

```
engine/rules/refresher.go   ← 新增（状态机+调度+退避+编排）
  ├── Fetch func(ctx, url) ([]byte, error)     ← 注入；生产=get 通道 A/B，测试=httptest
  ├── StateStore interface                      ← 注入；生产=store.Store(rules_state 表)，测试=内存
  └── 调 Provider.Apply + Provider.SetSeenMax   ← 桥接：seen_max 双保险（§5）
```

- **rules 包不 import store**（保持分层可测）；StateStore 接口 4 方法：`SeenMax() / SetSeenMax(v) / SaveRefreshState(Row) / LoadRefreshState() (Row, bool)`
- 装配（main.go）：启动序列 = Provider 加载 → 读 store seen_max 喂 Provider（取 max(disk 版本, 表值)）→ 起 refresher → api.RulesRefresh 接 refresher.Trigger（503→202 生效）

## 3. 拉取编排（一次 refresh）

1. **单飞闸门**：进行中再触发 → `ErrBusy`（API 409、周期 tick 跳过、手动 trigger 同语义）
2. **源序 A→B**（拍板 #1）：A 源 raw 直连（择优+熔断，复用 get.Fetcher）；A 失败（dial/TLS/HTTP≠200/超时 30s）→ B 源 worker 代理兜底。两源同内容，成功即止
3. 双文件：`current.json` + `current.json.minisig`，**各自 1 MiB 硬上限**（读超即断——A5 无尽数据）
4. 内存中调 `Provider.Apply(json, sig)`：验签→schema→版本→原子落盘→热替换（phase 2 已全实现，refresher 零重复）
5. 结果分类落 StateStore：`ok / fetch_err / sig_rejected / rollback / fast_forward / schema_rejected / size_exceeded`（doctor 与 /api/rules 可观测）
6. **调度**：启动 30s 首拉 → 每 6h；失败退避 1h→4h→24h 封顶，成功清零；退避期内**手动 trigger 放行**（用户意图至上）；退避状态持久化跨重启（拍板 #2）

## 4. rules_state 表（store 包新增）

```sql
CREATE TABLE IF NOT EXISTS rules_state (
  id INTEGER PRIMARY KEY CHECK (id=1),
  seen_max INTEGER NOT NULL DEFAULT 0,
  last_attempt_at TEXT, last_ok_at TEXT,
  last_result TEXT, last_detail TEXT,
  backoff_until TEXT, consecutive_failures INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0, successes INTEGER NOT NULL DEFAULT 0,
  rejects INTEGER NOT NULL DEFAULT 0
);
```

单行表；Store 加 4 个异步方法（与现有 writeLoop 同模式）。

## 5. seen_max 双保险（关键安全语义）

磁盘版本**不是**可靠的 seen_max 下界：A7 场景（磁盘规则被删/篡改）→ 重启回退 L0 → 内存 seen_max 回落到 embedded 版 → **攻击者若同时控制网络，可重放"embedded 版之上的任意旧合法签名版"**（如 disk 曾到 v15，推 v12 的旧合法签名——原版场景拒、损坏后放行）。
故 seen_max 必须独立于规则文件持久化在 SQLite。取值 `max(embedded, disk, state表)` 三者最大。
拒绝场景同样推进计数但不推进 seen_max（防攻击者用毒化版本顶住 seen_max——等值/更小才拒，与推进无交集）。

## 6. 热生效消费方收尾（phase 2 留尾）

| 消费方 | phase 2 现状 | phase 3 动作 |
|---|---|---|
| PAC / CONNECT 分流 | 每请求现取快照 | 已零改动 ✓ |
| cdn 端点回落链 | 手动>规则[0]>内嵌 | 规则更新后 cdnFn 重算（Apply 成功回调） |
| **调度器种子池** | 启动快照固定 | **热重建**（拍板 #3：Apply 回调 → 装配层 SwapSeeds vs 调度器轮询） |
| /api/rules/refresh | 503 桩 | 接 refresher.Trigger → 202/409 |

## 7. 毒化矩阵与 fakesite --mode rules

**分层**（测试金字塔，避免黑盒堆叠）：
- **L2 语义层（Go 集成，每次 CI）**：httptest 注入 Fetch——A1 伪造/A3 回滚/A4 快进/A5 无限流/A8 语义投毒/A6 冻结，断言 Apply 拒绝路径分类正确 + 快照冻结（版本/Source 不变）+ 续服务
- **CI 黑盒（api-smoke 扩展）**：真 serve + fakesite rules 模式端到端——伺服预置测试向量（测试密钥预签），断言 refresh 202 → 轮询 /api/rules 版本变化 → PAC 文本含新域 → SSE status 帧新版本
- fakesite `--mode rules -rules-dir <dir> -poison <a1|a3|a4|a5|a8|freeze|ok>`：静态伺服向量文件；毒化变体 = 伺服时现改（A1 换 sig 字节 / A5 chunked 无限流）

测试向量 `engine/rules/testdata/rules-matrix/`：`v10-ok`（=当前真源）、`v11-ok`、`a1-forged`、`a3-rollback(v9)`、`a4-fastforward(v999999999)`、`a8-schema-poison`、`a6-frozen`（合法签名的旧 generated/expires）——全部用**测试私钥**预签（newTestProvider 同款，keyID 可注入；CI 再用真私钥向量独立验 repo 内真源，两层不混淆）。

## 8. 测试计划（先冻结，后实现）

**L1 单元（表驱动 + fake clock + fake fetch）**：
- R1 调度时序：30s 首拉 / 6h 周期 / 手动立即；Stop 后不再拉
- R2 退避状态机：1h→4h→24h 封顶；成功清零；退避期手动放行；**退避跨重启**（重启后 backoff_until 仍约束周期 tick）
- R3 单飞：进行中 trigger → ErrBusy；完成后可再触发
- R4 结果分类：七类各写一行 StateStore（last_result/detail/计数）；ok 才进 successes，拒绝类进 rejects
- R5 双源编排：A 成功不碰 B；A 各失败形态（dial/TLS/404/超时）→ B 兜底；A+B 全死 → fetch_err
- R6 尺寸防线：json >1MiB / sig >1MiB → size_exceeded，连接关闭不死等
- R7 store rules_state：单行约束、往返、异步写不阻塞调用方

**L2 集成（httptest 真 HTTP）**：
- R8 毒化矩阵六变体：拒绝分类正确 + Provider 冻结（Version/Source 不变）+ PAC/分流续服务
- R9 正常热更 v10→v11：快照 Source=remote + seen_max 推进 + **重启后 seen_max 保留**（R 系列安全核心：重启+网络重放 v10 → rollback 拒）
- R10 崩溃安全：Apply 落盘阶段注入失败 → 重启加载或干净回退，无半状态（phase 1 已有单测，此处走 refresher 全链路复验）
- R11 golden：Refresh 状态值填实（schema 沿用 phase 2，字段不动则 golden 不动；确认无漂移）

**CI 黑盒 + L3 真机**：
- R12 api-smoke 扩展：serve + fakesite rules → 202 → 版本/PAC/SSE 三点断言（毒化变体选 a3 一条代表进 CI，全量毒化走 L2）
- R13 L3 真机（用户）：A/B 双源各一次完整拉取（关魔法跑 A 直连 + 开魔法跑 B 对照）、拔网线冷启动、doctor 显示规则源/版本

## 9. 实施顺序（TDD，每步本地绿再进）

1. store rules_state 表 + 4 方法（R7）
2. refresher 状态机骨架：clock/fetch 双注入 + 调度/退避/单飞（R1–R3）
3. 拉取编排 + 尺寸防线 + 结果分类（R4–R6）
4. 毒化矩阵 L2 + 重启防重放（R8–R10）
5. 装配：启动序列 + RulesRefresh 接真 + cdn 回落链 + 调度器热重建（拍板 #3 定机制）
6. 测试向量生成脚本（scripts/rules-matrix-gen，测试密钥预签 7 变体）
7. fakesite --mode rules + api-smoke 扩展（R12）
8. 文档（W2 收官报告）+ push CI + L3 真机排期（R13）

## 10. 拍板点

1. **源序**：A 直连优先、失败 B worker 兜底（推荐——A 可用性检验自身就是遥测；B 是 fallback）？还是 B 优先（自家 worker 最稳，A 是 fallback）？
2. **退避跨重启**：backoff_until 持久化（推荐——防"重启即重置"的重试风暴被远程触发器利用）？还是重启重新计时？
3. **调度器热重建机制**：Provider Apply 成功回调（推模型，即时但引入回调链）vs 调度器低频轮询快照版本（拉模型，简单但有 6h 内不生效窗口）？推荐推模型（种子池本就低频变化，回调只在 Apply 成功路径 fire 一次）
4. **6h 周期与 30s 首拉的 debug 缩放**：serve 加 `--rules-interval` 旗标（L3 真机验证方便，默认 6h）？推荐加（一个旗标零成本，真机实验必需）
