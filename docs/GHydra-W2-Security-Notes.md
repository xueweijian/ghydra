# GHydra W2 核心技术档案：信任链 / 毒化矩阵 / 拉取管线

> **用途**：M3-W2（F6 规则热更）的核心技术实现与算法决策档案——代码注释讲不了的全局推理、实测数据、攻击→防线→测试的映射关系都在这里。
> **读者**：未来贡献者、安全审计者、以及做规则 v2 schema / 密钥轮换 / MITM(v2.0) 时的自己。
> **配套**：坑见 `GHydra-Pitfalls.md` §6/§9；设计概要见 `GHydra-M3-W2-Design.md`；本档案只记"技术为什么与怎么做"。

---

## 1. minisign 信任链（engine/rules/minisign.go）

### 1.1 为什么零依赖手写

- 依赖白名单原则（供应链审计）：minisign 只有 Go 第三方实现（aead.dev/minisign 已引入作 **crosscheck 互操作**，不进运行时）
- 运行时验签核心 ~120 行：ed25519（标准库）+ blake2b-512（x/crypto，唯一新增）
- **信任不依赖自身代码**：`official minisign -Vm rules/current.json -P <公钥>` 可独立验证——第三方可绕开 GHydra 全栈审计签名链

### 1.2 签名格式（实测逆向，官方 spec 页面过时）

minisign 0.11 四行结构：

```
untrusted comment: <任意文本，不参与验签>
<base64: algo[2] || key_id[8] || sig[64]>        ← "Ed"=直接签名 / "ED"=blake2b-512 prehash
trusted comment: ghydra-rules v11 sha256=<hex>   ← 被 global signature 覆盖（可承载版本+指纹）
<base64: ed25519(sig || trusted_comment)>        ← global signature（双段签名链）
```

**私钥 -W（无密码）格式实测 158B**（spec 页面写 142B/独立 checksum 是错的）：

```
偏移      字段           实测
0..1      algo "Ed"      （私钥算法标识与签名算法无关）
2..3      kdf "B2"
4..5      ad "B2"
6..37     salt[32]
38..45    ops[8]         （-W 模式全零）
46..53    mem[8]         （-W 模式全零）
54..61    key_id[8]      ← blob 内嵌
62..93    seed[32]       ← ed25519 私钥 = expand seed
94..125   pk[32]         ← ⚠️ 公钥段在 94..126，不在尾部（spec 说在尾部）
126..157  cksum[32]      ← -W 模式全零，不校验（完整性由验签闭环保证）
```

- seed 展开：`ed25519.NewKeyFromSeed(seed)` —— pk 段可重算，仅作一致性参考
- 解析在 `sign-rules/main.go parseSecretKeyFile`；运行时不需要私钥

### 1.3 关键陷阱

- **公钥注释行的 key_id（展示名，如 5C89977CFADDD713）≠ blob 内嵌 key_id**（签名匹配用的，13d7ddfa…）。sign-rules 打印与运行时匹配一律用 blob 值
- GHydra 真源用官方 minisign 默认 "ED"（prehash）签发；测试辅助（testsig_test.go）用 "Ed"（直接签）——两算法验签路径都实现并有官方向量覆盖（minisign_test.go / aead.dev 交叉验证）
- trusted comment 篡改必拒（global sig 覆盖它）；untrusted comment 篡改不影响（规范如此）

## 2. 威胁模型 → 防线 → 测试映射（TUF A1–A9）

| 攻击 | 手法 | 防线（代码位置） | 测试（证据） |
|---|---|---|---|
| A1 伪造内容 | 篡改 json 字节 | VerifyMinisign 验签（provider.Apply ①） | smoke_w2p3 场景 2（fakesite a1 现场改字节）+ httptest R8 |
| A2 替换签名 | 换攻击者签名 | key_id + 公钥冻结（keys.go 编译期） | minisign_test 官方向量 |
| A3 回滚 | 重放旧合法签名版 | DecideVersion：candidate ≤ seenMax 拒 | smoke_w2p3 场景 3（v9 向量）+ **场景 7 跨重启** |
| A4 快进 DoS | version 抬到 999999999 锁死未来更新 | 双防线：schema cap 1e6（转写 ErrFastForward 保持归因）+ DecideVersion | smoke_w2p3 场景 4（vff 向量，-unsafe 签出） |
| A5 无尽数据 | 无限流/慢速流耗资源 | 三层：Fetch 包装 readLimited（ctx done + 1MiB/64KiB）→ fetchOne len 复核 → schema MaxFileBytes | smoke_w2p3 场景 5（1MiB 块洪泛）+ 慢速流教训（见 §4） |
| A6 冻结 | 源永不更新 | 「旧规则好过没规则」：过期仅 StaleNow 可观测**不失效**；失败退避 1h→4h→24h | smoke_w2p3 场景 6（断网续服务 + PAC 仍出） |
| A7 磁盘篡改 | 改/删本地规则对 | 启动加载时重验签（不比网络更可信）→ 回退 L0 embedded | provider_test 篡改回退 + **seen_max 双保险**（§3） |
| A8 语义投毒 | 合法签名 + 恶意字段 | ParseRules 严格 schema（DisallowUnknownFields + 重复键检测 + 全上限：64 域/4 cdn https/8×32 seed 公网/45d validity） | schema_test 全矩阵 + smoke 场景（签不出的版本 sign-rules 拒签=设计特性） |
| A9 混合与降级 | 跨源错配/降级替换 | 单源成套拉取（json+sig 同源，A 失败整组走 B）；cdn 优先级链手动>规则[0]>内嵌 | refresher_test R5 双源编排 |

**归因纪律**：每个拒绝路径返回 sentinel（ErrRollback/ErrFastForward/ErrSig*/ErrSchema），doctor 必须看到攻击类型而非笼统 schema violation——schema cap 与版本判定重叠时归因给可观测的那条（ErrVersionCap → ErrFastForward 转写）。

## 3. seen_max 双保险（A3 × A7 组合攻击窗口）

单独的 A3/A7 防线都有，**组合起来有洞**：

```
攻击链：disk 曾到 v11 → 攻击者毁盘（A7）→ 重启回退 L0（embedded v1）
        → 内存 seen_max 回落 v1 → 重放"v2..v11 任意旧合法签名版"（原版场景拒、损坏后放行）
```

堵法：**seen_max 独立于规则文件持久化**（SQLite rules_state 单行表），启动装配取 `max(embedded, disk, 表值)` 喂 Provider。

- `Provider.SetSeenMax` 只增不减（回退 = 亲手拆 A3 防线）
- 表的写路径**同步**（刷新 ≤6h 低频，换「写后立读」一致性；极端崩溃丢表由 disk 版本兜底——仍取 max）
- 适配器全行 UPSERT 会覆盖 seen_max 列 → 写前读现值回填（Pitfalls §1）
- 端到端证据：smoke_w2p3 场景 7（拉 v11 → kill → 重启 → 网络重放 v9 → rollback 拒）；Provider 层语义 TestProviderSetSeenMax

## 4. 拉取管线防线（engine/rules/refresher.go）

```
Trigger(手动/30s 首拉/6h 周期)
  → 单飞闸门（ErrBusy；手动不受退避拦——用户意图至上）
  → 源编排 A→B（单源成套 json+sig；全失败归因主源 A 的类别）
  → fetchOne：readLimited（ctx 超时 30s + limit 1MiB/64KiB 双防线）
  → Apply：验签 → schema → 版本 → 原子落盘 → 热替换（phase 1/2 零重复）
  → finish：结果七分类（ok/fetch_err/sig/rollback/ff/schema/size）→ StateStore 持久化
```

- **慢速流攻击**（实测教训）：`io.ReadAll` 无 ctx 感知——4KB/s 流绕过 fetchWait 挂 256s。readLimited 在读循环 select ctx.Done（A5 慢速变体防线）；洪泛变体由 limit 断（fakesite a5 用 1MiB 块 ×64 造）
- **退避两个语义分开**：`nextDelay`（周期 tick 用，= 退避剩余或 Interval）≠ `backoffRemaining`（首拉推迟用，纯退避）——首拉 30s 不能被 Interval 抬走，退避恢复又必须推迟首拉
- 退避跨重启：BackoffUntil 持久化，「重启即重置」不能被远程触发器利用成重试风暴
- 调度器种子池热重建走 **OnApply 推模型**（Apply 成功路径 fire 一次；拉模型有 6h 生效窗口）

## 5. 毒化矩阵方法论（测试基础设施）

### 5.1 分层（测试金字塔，不把黑盒当万能）

| 层 | 载体 | 覆盖 | 成本 |
|---|---|---|---|
| L1 单元 | fake fetch/fake clock 表驱动 | 调度/退避/单飞/分类/双源/尺寸（R1–R7） | 毫秒 |
| L2 集成 | httptest 真 HTTP + 内存 StateStore | 毒化语义六变体 + 重启防重放（R8/R9） | 秒 |
| 黑盒 | 真 serve + fakesite --mode rules | 7 场景（含 PAC 探针域热生效、跨重启） | CI api-smoke job |
| L3 真机 | 用户网络窗口 | 双源拉取 + 拔网冷启动 | 人工排期 |

### 5.2 向量管理

- **真私钥签名进 repo 是合法发布形态**（签名 = 公开数据）；私钥永不进 repo/CI
- 向量集 `rules/testdata/vectors/{v9,v11,vff}`：v11 带 PAC 探针域 `v11probe.ghydra-test.example`（IANA 保留 TLD，不真解析）断言热生效；v9 = A3；vff = A4（**必须 `-unsafe` 签**——schema 防线拒签毒化版本是设计特性）
- **45 天 validity 上限** → 向量会过期：重签时 generated=当天、expires=+30~45d；过期后归因漂移（rollback → schema_rejected）——CI 延后跑挂了先查这个
- `.gitattributes`：`rules/** -text` 锁字节（CRLF = 验签必挂）
- 向量与 serve 的信任锚一致（keys.go 冻结公钥）——黑盒才能走真验签链；L2 用测试密钥注入是另一层，不混淆

### 5.3 fakesite --mode rules

- URL 相对路径映射 rules-dir（`-rules-dir $VECS/v11` + `/current.json`，URL 不再带变体名）
- `-poison a1`：响应时改 json 第 3 字节（签名脱钩）；`-poison a5`：1MiB 块 ×64 无 sleep 洪泛（读端限长断开 → broken pipe 自然退出）
- 断言模式：POST /api/rules/refresh（异步）→ 轮询 /api/rules 的 `refresh.last_result` 到目标分类 + version 冻结断言

### 5.4 smoke 编写纪律（CI 实证教训）

- **改装配后必须回归既有 smoke 全集**（e1b991f 修的就是旧 w1 断言过时：503 桩 → 200 真触发、refresh 零值 → next_at 有值）
- 起跑 `rm -rf $D` + BIN/D 目录分离——本地连跑残留（rules_state seen_max / 场景 16 落盘规则）CI 从未暴露
- 进程管理 `start_serve` echo `$!` 精确 pid（ps aux 不可靠）；busy 文案 grep 别带引号前缀（`rules: refresh already in flight`）

## 6. 规则发布与轮换（运维手册）

发布新版本：改 `rules/current.json`（版本 +1，generated=当天，expires ≤+45d）→ `go run ./scripts/sign-rules -key <dev.key> -in rules/current.json` → json+minisig 一并提交 → CI rules job 验签+单调 → raw 端点自动生效（serve 30s 首拉/6h 周期消费）。

密钥轮换：schema 预留 keys[]（v1 不解析）；v2 走 TUF root 轮换式双签过渡——单公钥冻结在 keys.go 的现状下，轮换 = 发版（embedded L0 更新随二进制）。
