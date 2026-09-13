# GHydra M3-W2 设计 · F6 规则热更新（rules.json + minisign 签名链）

> 状态：待拍板（2026-09-13 起草）。映射：M3-Plan §W2 / D4 / PRD F6 上半 / TechReference M9。
> 前置阅读已完成：minisign 文件格式规范（jedisct1/minisign 官方）、TUF 规范的攻击分类（rollback / fast-forward / indefinite freeze / endless data / mix-and-match）。
> 产出本文件并拍板后开工，测试计划（§7）在写任何实现代码之前冻结。

## 0. 范围

**做**：
1. `rules.json` v1 schema + 严格解析器（`engine/rules/schema.go`）
2. minisign 兼容验签器（零依赖手写，`engine/rules/minisign.go`，与官方工具互验）
3. 版本防线：回滚拒 / 快进 DoS 拒 / 冻结告警（`engine/rules/version.go`）
4. 原子落盘 + 崩溃安全加载（`engine/rules/atomic.go`）
5. 拉取管线（复用 `engine/get.Downloader`，走自身 A/B 双通道）
6. `RulesProvider` 三级信任地板：内嵌 → 磁盘 → 远程（`engine/rules/provider.go`）
7. serve 装配：规则热生效（Matcher/PAC/CONNECT 分流重建）+ `GET /api/rules` + `POST /api/rules/refresh` + SSE status 帧携带规则状态 + golden 双端
8. 签名工具 `scripts/sign-rules` + 仓库规则真源 `rules/current.json`
9. fakesite `--mode rules` 毒化矩阵 + CI 化（`ci-rules.yml` 或并入现有 ci）
10. 真机双源拉取实验（L3）

**不做（防膨胀）**：
- 公钥 rotation 签名协议——字段预留（`keys[]`），不解析不启用（v2.0 议）
- TUF 全家桶（角色分离 / 门限签名 / timestamp+snapshot 双角色）——单维护人过重，取其攻击分类与防线思想
- 程序自更新（D5 → W4）
- GUI 规则页（W3 只读展示）
- 众包规则回流（F7，v2.0）

## 1. 威胁模型

**攻击者能力假设**：完全控制网络路径（GFW / 中间人 / BGP / CDN 节点被劫持 / 规则源仓库被黑）、可重放任何历史合法文件；**不能**控制用户本机进程与离线私钥。

对照 TUF 攻击分类逐条设防（差异声明见后）：

| # | 攻击 | GHydra 对策 |
|---|---|---|
| A1 | 伪造规则（无钥签名） | ed25519 验签，公钥编译期冻结（信任锚 = 二进制） |
| A2 | 篡改内容字节 | detached 签名覆盖文件全部字节；版本号嵌在文件内 ⇒ 同时被签 |
| A3 | **回滚投毒**（重放历史合法版本：含已沦陷域名/已死 IP 的旧版） | 版本单调：仅接受 `version > 本地已见最大值`；已见最大值持久化（SQLite `rules_state`） |
| A4 | **快进 DoS**（把版本抬到天文数字，令此后一切真更新都被判回滚拒 = 永久锁死） | 版本硬上限 `1_000_000`：超出即拒 + doctor 记录「fast-forward 异常」（TUF 明文列出的攻击，多数自研更新系统漏防） |
| A5 | **无尽数据**（喂无限流耗尽磁盘/内存） | 拉取全程内存缓冲 + 1 MiB 尺寸上限，超限即断 |
| A6 | **无限冻结**（一直喂旧版，用户无感知新规则存在） | `expires_at` 软过期：过期 → `RulesStale` 告警进 status/SSE/doctor（可见性），**不失效不拒绝** |
| A7 | mix-and-match（新签名拼旧内容 / 签名换内容） | 签名绑定内容字节；磁盘上的 json+minisig 成对重验（加载时验，不只拉取时验——防磁盘篡改） |
| A8 | **语义投毒**（签名合法但内容恶意：把 banking.com 塞进加速清单 / 内网 IP 塞进种子表） | schema 硬校验：域名 ≤64 条且形态白名单（`(\*\.)?[a-z0-9.-]+`）、禁 loopback/内网 IP、cdn 必须 `https://` 且 ≤4 条、seed ≤8 域 ×≤32 IP |
| A9 | 降级攻击（未来 schema v2 文件喂给 v1 老客户端） | `schema_version` 未知即拒（老客户端不吃新格式，宁可冻结） |

**与 TUF 的信任模型差异（明示，不是偷懒）**：TUF 防「过期信任」——元数据到期即失效，因为它的场景是软件分发（过期 = 可能已沦陷）。GHydra 规则的信任语义是「**旧规则好过没规则**」——过期域名清单最多让加速质量退化，冻结兜底永远可用，因此过期只做可观测（A6）不做失效。角色分离/门限签名服务于多维护者仓库，单人项目引入只增加私钥管理面，rotation 预留字段即可。

## 2. 文件格式

### 2.1 `rules.json` v1

```json
{
  "schema_version": 1,
  "version": 42,
  "generated_at": "2026-09-13T12:00:00Z",
  "expires_at": "2026-10-28T12:00:00Z",
  "domains": ["github.com", "*.github.com", "…"],
  "cdn_endpoints": ["https://gh.1ciyuan.cn", "https://gh-proxy.com"],
  "seed_ips": { "github.com": ["140.82.112.3", "…"] }
}
```

- 解析：`json.Decoder` + `DisallowUnknownFields`（未知字段拒收，防语义漂移）；全部 A8 上限校验；RFC3339 UTC（与 golden 时间戳纪律一致）
- `expires_at ≤ generated_at + 45d`（生成端纪律，签名工具强制）
- 内容真源 = 本仓库 `rules/current.json`（PR 审计轨迹 + git blame 可查每次规则变更）

### 2.2 签名：minisign 兼容（detached `.minisig`）

四行标准格式（官方规范，非 prehash 变体 `Ed`——规则文件仅 KB 级无需 prehash，免 blake2b 依赖）：

```
untrusted comment: ghydra rules
base64( "Ed" ‖ key_id[8] ‖ signature[64] )
trusted comment: ghydra-rules v42 sha256=<hex>
base64( global_signature[64] )    ← ed25519( signature ‖ trusted_comment )
```

- **互操作性是硬要求**：任何用户可用官方 `minisign -Vm rules/current.json -P <公钥>` 独立验证我们的规则——信任不依赖 GHydra 自身代码（README 提供一行命令）
- 零依赖手写编解码 ~120 行（标准库 `crypto/ed25519`），不引 `aead.dev/minisign`（依赖白名单洁净，格式可审计——与手写 ClientHello 同一工程哲学）
- `trusted_comment` 被全局签名覆盖 ⇒ 版本号 + sha256 指纹不可被改写，可作人眼核对面
- 公钥冻结：`engine/rules/keys.go`（base64 常量 + key_id）；`key_id` 用于快速匹配与人眼核对

### 2.3 三级信任地板

| 级 | 内容 | 何时用 |
|---|---|---|
| L0 | `engine/rules/embedded.json`（go:embed，编译期冻结，v1） | 永远可用的地板；拔网线冷启动即此（PRD F6 验收：断网可启动可工作） |
| L1 | `~/.ghydra/rules/current.json` + `.minisig` 成对 | 启动加载，**加载时重验签**（A7：防磁盘篡改）；验签失败回退 L0 |
| L2 | 远程拉取成功 | 原子落盘升级 L1，内存热生效 |

## 3. 版本判定（version.go）

`seen_max = max(内嵌版本, 磁盘版本, 历史拉到过的最大版本)`，持久化于 SQLite `rules_state(key TEXT PRIMARY KEY, value TEXT)`（复用 store 异步写基建；幂等建表与现有模式一致）。

| new.version 判定 | 结果 |
|---|---|
| `> 1_000_000` | 拒 + 记 fast-forward 异常（A4） |
| `≤ seen_max` | 拒 rollback（A3） |
| `> seen_max` 且验签/ schema 全过 | 接受：落盘 + seen_max 更新 + 热生效 |
| 验签失败 / schema 拒 | 拒（拒绝原因入 store，doctor 可见） |

首装语义：`seen_max` 初始 = 内嵌版本 ⇒ 首次远程拉取天然要求 `> 内嵌版`，无特例分支。

## 4. 拉取管线（refresher.go）

- **复用 `engine/get.Downloader`**——A 择优直连 / B CDN 续传切道原样生效：**更新链路自己先吃自己的狗粮**（D4 原文要求）
- 端点（双源同内容，均在本包白名单域内）：
  - A 源：`https://raw.githubusercontent.com/xueweijian/ghydra/main/rules/current.json`（+ `.minisig`）
  - B 源：自建 worker 代理同 raw 路径（`https://gh.1ciyuan.cn/https://raw.…`，复用现有 B 通道协议）
- 调度：启动后 30s 首拉 → 每 6h；`POST /api/rules/refresh` 手动（进行中 409）；doctor 手动触发附带刷新
- 失败退避 1h → 4h → 24h 封顶（不重试风暴；退避原因入 store）
- 流程：拉 json+sig（内存，各 1 MiB 上限）→ 尺寸 → 验签 → 版本 → schema → **原子落盘**
- 原子落盘：`current.json.tmp` 写 + fsync → rename → fsync 目录；sig 同理先落。**两文件非原子对**的中途崩溃窗口：重启加载要求成对验签，缺一/不配即回退 L0——崩溃安全，无需日志
- 全程指标入 store（attempt/success/reject 分类计数），`/api/rules` 与 doctor 可见

## 5. 热生效与 API

**`RulesProvider`**（`engine/rules/provider.go`）：`atomic.Pointer[Snapshot]` 无锁读；`Snapshot{Matcher(预构建), Domains, CDNEndpoints, Seeds, Version, Source: embedded|disk|remote, Stale bool, LastRefresh}`。

消费方改造（现状两处硬编码 `rules.New(rules.DefaultDomains)`）：
- `cmd/ghydra/main.go:212`（serve：PAC 生成 + CONNECT 分流）→ 读 provider 快照
- `cmd/ghydra/get.go:92`（get 判定加速域）→ 同
- PAC 端点、分流器在规则更新时**重建**（Matcher 只读并发安全，整指针换——读侧零锁）

**CDN 端点优先级链（写死，防语义含糊）**：
`用户显式 --cdn / POST /api/config 手动设置` > `远程规则 cdn_endpoints[0]` > `内嵌默认`。手动设置永不被规则覆盖（用户意图至上），清除手动值后回落规则值。

**API（golden 双端同步更新）**：
| 端点 | 行为 |
|---|---|
| `GET /api/rules` | 快照 + 刷新状态（next_refresh / last_result / backoff） |
| `POST /api/rules/refresh` | 异步单飞，409 if running |
| `ApiStatus` 增 `rules` 字段（version/source/stale） | SSE status 帧 diff 自动携带 |

## 6. 签名工具与发布链路

**`scripts/sign-rules`**（独立小 main，`go run ./scripts/sign-rules -key ~/.ghydra-seckey/minisign.key -in rules/current.json`）：
读入 → schema 校验 → `version` 必须 > 上一签版本（本地拒绝手滑倒退）→ 产 `.minisig` → 控制台输出 key_id/sha256 指纹。

**推荐流程：本地离线签 + CI 只验不签**（拍板点 #1）：
1. 规则变更走 PR 修改 `rules/current.json`
2. 维护者本地 `sign-rules` 生成 `.minisig`，**签名文件一并提交**（raw 端点要求两文件同在）
3. CI 新增 rules job：`minisign 验签 + schema 校验 + 版本较 main 上版单调`——**私钥永不进 CI**，CI secret 面为零，且防手滑提交未签/倒退版本
4. 备选档（不推荐）：CI secret 存私钥自动签——便利但泄露面 = 规则签名权；且机器 commit 回写 main 的流程复杂度不值

**密钥管理**：
- 私钥生成一次性（`minisign -G` 或 `scripts/genkey`），存维护者离线介质，**永不进 repo/CI**
- 公钥冻结 `engine/rules/keys.go`；rotation：schema 预留 `keys[]` 字段（v1 不解析），v2 若需轮换走 TUF root 轮换式双签过渡

## 7. 测试计划（先于实现冻结）

**L1 单元（先写测试，后写实现）**：
1. **minisign 互操作向量**：`testdata/` 放官方 minisign 工具真实生成的密钥/签名 fixture（保证不是自说自话）；边界：坏 base64 / 短 sig / 错 key_id / 坏 global_signature / untrusted comment 篡改不影响验签（规范如此）/ trusted comment 篡改必拒
2. schema：全字段 happy；未知字段拒；A8 全矩阵拒（第 65 域名 / 第 5 个 cdn / 非 https cdn / 私网 IP `10.0.0.1` / loopback 域名 / `schema_version:2`）；`expires_at > generated_at+45d` 拒
3. 版本判定：§3 矩阵表驱动（含首装、快进、回滚、等值）
4. 原子落盘：tmp 残留不影响后续加载；rename 后立即可读；**加载时验签失败回退 L0**（A7）
5. Provider 三级降级：L2 拒 → L1 在；L1 坏 → L0 在；L0 永在（编译期）

**L2 集成（CI 化，fakesite `--mode rules`）**：
6. 验签失败拒（内容改一字节）
7. 回滚拒（v43 后喂 v42 重放）
8. 快进拒（合法签名 + `version: 999_999_999`）
9. 无尽数据拒（>1 MiB 流）
10. 断网回退（源死 → 冻结规则继续服务：PAC 正常返回、分流正常、`source=embedded|disk` 且 doctor 可见）
11. 热生效端到端（v42 → v43：新域名进 PAC 文本 + CONNECT 分流命中 + SSE status 帧出现新 version）
12. 崩溃安全（拉取落盘中途 kill serve → 重启规则完好或干净回退，无半状态）
13. golden：`/api/rules` schema 双端（Go byte 比对 ↔ vitest 类型断言）
14. CI rules job：验 repo 内 `rules/current.json` 签名 + 版本单调（自身发布纪律 CI 化）

**L3 真机（用户实验，验收物 `docs/evidence/M3-W2/`）**：
15. 双源拉取：真机 A 源（raw 直连择优）与 B 源（worker 代理）各验一次完整链路
16. 6h 自动刷新日志走查 + doctor 显示规则源与版本
17. 拔网线冷启动（PRD F6 验收原文：断网可启动可工作）

**L4 故障注入（毒化演练）**：
18. §1 攻击矩阵 A1–A9 逐条注入，全部走拒绝路径且 doctor 归因正确——**这是本设计的退出考试**

## 8. 实施顺序（TDD）

1. minisign fixture 生成（一次性：沙箱获取官方 minisign 二进制产测试向量，或用可信第三方实现产向量后逐字节核对格式；fixture 提交进 repo）
2. `schema.go` / `minisign.go` / `version.go` / `atomic.go`——纯逻辑，每文件先测后码
3. `scripts/sign-rules` + `rules/current.json` v1 真源（内容 = 现 DefaultDomains + gh.1ciyuan.cn + seeds.json 种子数据首版）+ CI rules job
4. `provider.go` + serve 装配（main.go/get.go 消费点改造 + PAC/分流热重建）+ `/api/rules` + golden 双端
5. fakesite `--mode rules` 毒化矩阵 + CI 集成 job
6. L3 真机实验（需用户配合网络窗口）→ 文档回填 TechReference §6

## 9. 风险与开放问题

| 风险 | 缓解 |
|---|---|
| 沙箱获取官方 minisign 二进制（fixture 互操作） | apk 无 minisign 包；备选：GitHub release 下载官方静态二进制到 /tmp 一次性使用（几 MB，不违大文件禁令）；再备选：手写向量按官方规范逐字节构造 + 文档记录规范出处 |
| A 源 raw.githubusercontent.com 真机可达性 | 它本在加速白名单内（A 择优 + B worker 代理双路）；W2 L3 实验专门验证「更新链路吃到自身加速红利」 |
| 两文件（json+sig）拉取时源端不一致（repo 更新窗口） | 分别拉取后成对验签，不一致即拒（下轮 6h 再试）；不存在半接受态 |
| SQLite 新表迁移 | `CREATE TABLE IF NOT EXISTS rules_state` 幂等，与现有模式一致 |
| 版本上限 1e6 | 每天一版可用 2700 年；真触顶 = 迁 schema v2 事件 |
| daemon 重启频繁场景 6h 周期永远到不了 | 启动后 30s 首拉已覆盖；`LastRefresh` 超过 24h → doctor 提示 |

## 10. 拍板点（3 项）

| # | 议题 | 选项与建议 |
|---|---|---|
| 1 | 签名流程 | **建议：本地离线签 + CI 只验不签**（零 secret 面，流程最简）；备选 CI secret 自动签（便利，泄露面=规则签名权） |
| 2 | 自动拉取周期 | **建议 6h**（规则变更低频，6h 足够新鲜且零打扰）；备选 24h |
| 3 | `cdn_endpoints` 与手动 `--cdn` 的优先级链 | **建议：手动设置 > 规则值 > 内嵌默认**（用户意图永不被远程规则覆盖） |
