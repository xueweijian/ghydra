# GHydra M1 W2 设计 · IP 调度器 + SQLite

> **用途**：W2（M1-Plan §4 第 4 周）开工前的细化设计。前置阅读已完成（FastGithub fork `creazyboyone/FastGithub` + dev-sidecar master），结论在第 1 节。
> **状态**：草案（2026-09-12），待拍板后编码。
> **交付后状态**：`ghydra serve` 从"隧道正确"升级为"真正加速"——命中域名走择优 IP。

---

## 0. 两句话总结

真实流量就是最好的探测器（dev-sidecar v2.2 的核心洞见）：连接成败都变成评分输入，专用探测只做补充。状态机管生死（可用性），EWMA 管排序（速度），粘性管体验（会话稳定），熔断优先于一切。

## 1. 参考项目消化结论（前置阅读交付）

### 1.1 dev-sidecar SpeedTester（packages/mitmproxy/src/lib/speed/）

**机制**：per-hostname 实例，`alive`（已验证）+ `backupList`（候选）双列表；TCP connect 5s 超时测速按耗时排序。

**核心洞见——按需探测（v2.2 新增）**，正是 GHydra 要的：
- `alive` 为空时**轮转分配** backupList 中未失败 IP 给并发请求（`_probeIndex++ % fresh.length`，并发自动分散）
- `_probing` 标志防重复探测同一 IP；第一轮取 fresh，全忙则第二轮允许复用
- **真实连接当探测**：成功 → `status=success` + push 进 alive；失败 → `status=failed`。省掉专用探测且测的是真实路径
- 节能：1h 无访问停定时器；`tryTestCount % 10` 渐进升级（9 次增量重测 → 第 10 次全量重 DNS）
- 失败但曾有成绩的 IP 保留历史 time（只有从未成功过的才永久 failed）

**没有的**（GHydra 增强）：EWMA、状态机、粘性、熔断时间窗、持久化（纯内存）。

### 1.2 FastGithub DomainResolve（fork creazyboyone）

**机制**：定时全量测速（TCP connect 5s）按 Elapsed 排序取 **top 3**；`fastSort` = WhenAny 竞速（所有 IP 并行 connect，2s 窗口内第一个成功的排最前，随即取消其余）。

**借鉴点**：
- TCP connect-only 测速（不做 TLS 握手探测——贵）
- **本机网络故障识别**：`SocketError.NetworkDown/Unreachable` → 时延缓存用 1min 短过期（快速重试）而非归罪 IP。进 GHydra 的失败分类
- 双层缓存过期（域名-IP 关系 10min / 时延结果 5min）
- 持久化极简：只存 endpoint 列表（json），不存测速结果

**没有的**：状态机/EWMA/粘性/熔断——它本质是"定时全量 ping + 排序"，比 GHydra 设计粗糙，验证了 GHydra 自研路线的必要性。

### 1.3 对 GHydra 设计的修正/确认

| 项 | 结论 |
|---|---|
| 探测策略 | **被动为主**（真实流量成败即探测，dev-sidecar 式）+ **主动为辅**（TCP connect，仅候选入池/池枯竭/后台刷新时）。确认，不改为 TLS 揢测——TLS-RST 型坏 IP 由被动失败熔断 + Quarantine 兜底 |
| TCP-only 探测的盲区 | TCP 通但 TLS 死的 IP 会短暂入池 → 首次真实连接失败 → Cooldown 7s → 再失败 Quarantine 5min。自愈路径闭环，可接受 |
| 池规模 | FastGithub 只留 top3；GHydra 保留全量候选（供切换）但 **Active 排序池取 top 5**（score 最优的 5 个） |
| 节能 | 抄 dev-sidecar：1h 无流量的域名停后台刷新 |

## 2. 范围与不变量

**In**：sched 包（EWMA/状态机/粘性/按需探测）+ store 包（SQLite）+ serve 接入调度器。
**Out**：通道 B/降级（M2）、吞吐启发（M2）、PAC/sysproxy（W3）、doctor（W4）。

不变量：
- 依赖增量：`modernc.org/sqlite` 一个（纯 Go 无 CGO，CI 三平台免配置）——依赖白名单登记
- proxy 包不改接口——调度器通过 `UpstreamSelector` 接入（W1 留的口正好）
- 失败安全：调度器任何内部 panic 不得杀 serve（per-domain recover）

## 3. 核心设计

### 3.1 数据流

```
bootstrap（meta/DoH/冻结种子）
    │ AddCandidates(domain, ips)          ┌────────────────────────┐
    ▼                                     │ per-domain state       │
CONNECT github.com:443 ──► Pick(host) ──► │ New → Probing → Active │
    │                                     │        ↓      ↓       │
    ├─ 粘性 IP 未过期 → 直接返回            │    Cooldown(7s)       │
    ├─ Active 池非空 → 返回 score 最优      │        ↓ 3 次         │
    ├─ 池有 New/Probing → 轮转分配（本连接  │    Quarantine(5min)   │
    │   当试金石，连上即入 Active）         └────────────────────────┘
    └─ 池枯竭 → 降级 W1 行为（直连域名，DNS 兜底）
    
连接结束 ──Event(dial_ms, 读写成败)──► Report(ip, rtt, ok)
    ├─ ok: EWMA(RTT/失败率/抖动) 更新 + 续粘性
    └─ fail: 失败计数 → Active→Cooldown（立即，粘性同时失效）
             Cooldown 期满重入轮转；连续 3 轮失败 → Quarantine
```

### 3.2 状态机（`engine/sched/state.go`）

| from | event | to | 副作用 |
|---|---|---|---|
| New | 被轮转分配/主动探测 | Probing | 标记 probing 防重复分配 |
| Probing | Report(ok) | Active | 记录 rtt 样本 |
| Probing | Report(fail) | Cooldown | 7s 退避 |
| Active | Report(fail) | Cooldown | **粘性立即失效**（熔断优先，R3） |
| Active | 后台刷新成功 | Active | EWMA 更新 |
| Cooldown | 7s 到期 | New（重入轮转） | cooldown 计数 +1 |
| Cooldown | 连续 3 轮 | Quarantine | 5min 隔离 |
| Quarantine | 5min 到期 / 池枯竭 | New | 计数清零（给重生的机会） |

参数全部可配（Config 结构体），默认：`Cooldown 7s / Quarantine 5min / 粘性 60s / 探测超时 5s / Active 池 5`。

### 3.3 评分（`engine/sched/ewma.go`，PRD 公式不动）

```
score = 0.3·norm(RTT_EWMA) + 0.5·FailureRate_EWMA + 0.2·norm(Jitter_EWMA)
norm(RTT) = clamp(ms/1000, 0, 1)        // 150ms → 0.15
norm(Jitter) = clamp(stddev/mean, 0, 1) // 变异系数
```

- EWMA α=0.3（新样本占三成，约 7 个样本收敛 92%）
- 冷启动（无样本）score=0.5 中庸值——不奖励不惩罚，靠轮转快速积累
- 权重含义即"可用性主导，速度微调"（失败率一动权重 0.5，RTT 差 150ms 才 0.045）——符合加速器价值观，测试锁定此行为
- 失败率 EWMA 特殊处理：失败样本 rtt 记为超时值（探测超时 5s），既惩罚可用性也抬高 RTT

### 3.4 粘性（`engine/sched/sticky.go`）

- 同域 60s 内 Pick 返回同一 Active IP（会话稳定，TLS session resumption 友好）
- 例外：该 IP 熔断 → 立即换（熔断优先于粘性，测试锁定）
- 粘性是 per-domain 单值（不是 per-client-src），CONNECT 代理场景客户端身份不可得

### 3.5 按需探测轮转（`engine/sched/probe.go`）

抄 dev-sidecar 骨架 + GHydra 状态机语义：
- 候选来源：bootstrap 喂数（启动/定时）+ DoH 兜底解析（池枯竭时同步触发，带 7s 预算）
- 轮转索引 per-domain，优先 New（未探测），其次到期 Cooldown
- `_probing` 标志防重复；全忙时允许复用（总比降级 DNS 好）
- 主动 TCP 探测仅两场景：①启动时 last-good 恢复验证 ②后台定时刷新（活跃域名，30s 间隔）

### 3.6 存储（`engine/store/`，modernc.org/sqlite）

MVP 两表：

```sql
CREATE TABLE IF NOT EXISTS last_good (
  domain TEXT PRIMARY KEY, ip TEXT NOT NULL,
  score REAL, rtt_ms REAL, updated_at INTEGER  -- unix ms
);
CREATE TABLE IF NOT EXISTS probe_log (          -- W4 SLA/doctor 的粮
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  domain TEXT, ip TEXT, ok INTEGER, rtt_ms REAL, source TEXT,  -- passive|active|bootstrap
  ts INTEGER
);
```

- 写入时机：Report 时异步批量（channel + 单写 goroutine，掉电最多丢窗口内数据，不阻塞热路径）
- last_good 更新条件：score 优于现存 或 现存 IP 已 Quarantine
- 启动恢复：last_good 入池为 Active（经一次 TCP 验证后放行）
- `WAL` 模式；库文件 `~/.ghydra/ghydra.db`（Windows 兼容路径用 os.UserHomeDir）

### 3.7 serve 集成

```go
sel := sched.NewSelector(scheduler, m)  // 实现 proxy.UpstreamSelector
// 命中规则 → scheduler.Pick(domain)；未命中 → ("", false) 放行
// 池枯竭 → Pick 内部降级返回 domain:443（W1 行为兜底）
```

`--scheduler=on|off` 开关（off = W1 直连行为，留作 A/B 对照与故障逃生）。`status` 子命令输出池快照（W4 doctor 前的临时观察口）。

## 4. 测试计划（映射 DevWorkflow）

| 层 | 对象 | 断言 |
|---|---|---|
| L1 | EWMA | 收敛性（α 权重）、冷启动 0.5、失败率惩罚单调、抖动计算 |
| L1 | 状态机 | §3.2 全转换表驱动（含非法转换拒绝） |
| L1 | 粘性 | 窗口内同 IP；熔断立即换；窗口过期重选 |
| L1 | 轮转 | 并发 N 拿 N 个不同 New IP；全忙时复用 |
| L2 | 端到端切换 | 假 IP（127.0.0.1:1）注入 → 后续连接换 IP，总切换时间 <7s |
| L2 | bootstrap 喂数 | fixture 候选流入池 → 轮转分配 → 成功入 Active |
| L2 | SQLite | 写入/重启恢复/last_good 更新条件；断写不阻塞 Pick |
| L2 | serve 集成 | --scheduler=on 时 Event.Target 是 IP 而非域名 |
| L3 | 真机（沙箱，关魔法） | serve 挂 10 分钟真实流量 → status 池快照合理（Active 有 IP、RTT 真实） |

## 5. 需拍板项

1. **探测分层"被动为主+主动为辅"**（§1.3）——我认为是本次设计最重要的一个决定，影响整个数据流形态。
2. **SQLite MVP 只做 last_good + probe_log 两表**，池快照不做（重启靠 last_good 恢复 + bootstrap 重喂数）。池状态不值钱，候选源便宜。
3. **参数默认值**（§3.2 末）：Cooldown 7s / Quarantine 5min / 粘性 60s / 探测超时 5s / Active top5 / EWMA α=0.3 / 后台刷新 30s。全部可配，默认值我按 PRD + dev-sidecar 实证参数定的。
4. **`--scheduler` 默认 on**（off 留作对照/逃生）。
