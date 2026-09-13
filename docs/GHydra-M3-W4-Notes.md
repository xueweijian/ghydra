# GHydra M3-W4 实现笔记 · 自更新与（后续）打包分发

> 状态：W4p1（selfupdate 核心）已交付，`ce652dc` CI 18/18 全绿。
> 配套：设计（做什么、为什么）见 `GHydra-M3-W4-Design.md`；本文记**怎么做的**——
> 算法、协议、不变量、以及每个决策背后的实证理由（供 W4p2/未来维护者直接抄）。
> 避坑清单见 `GHydra-Pitfalls.md` §10。

## 1. 交付地图

| 层 | 文件 | 职责 |
|---|---|---|
| 纯逻辑 | `engine/selfupdate/semver.go` | 版本解析/比较（唯一真源） |
| | `engine/selfupdate/release.go` | Release JSON + 资产选择 + U2/U3/U4/U8 门槛 |
| | `engine/selfupdate/checksums.go` | sha256sum 清单解析核对 + minisign 验签（复用 rules 实现） |
| | `engine/selfupdate/state.go` | 状态机（pending/confirmed/boot/back） |
| 文件系统 | `engine/selfupdate/swap.go` | 交换序列 + 崩溃恢复 + 回滚 |
| | `engine/selfupdate/archive.go` | zip/tar.gz 解包（仅白名单常规文件，zip-slip 全拒） |
| 编排 | `engine/selfupdate/update.go` | Check / ApplyPlan / BootHook / Rollback |
| 基建 | `engine/internal/fsx` | 原子写 + Windows 锁退避 rename/read |
| | `engine/internal/minisign` | `-W` 私钥解析 + ED 签名（签名侧） |
| 装配 | `engine/cmd/ghydra/selfupdateglue.go` | A/B 通道注入、daemon 控制、CLI 入口 |
| 工具 | `scripts/releasesrv` | 测试/演练用 fake Release server（攻击模式开关） |
| 黑盒 | `scripts/smoke_w4.sh` | 四场景升级演练（CI 三平台矩阵） |

## 2. 信任链（U1–U8 威胁模型）

```
        assets 字节  ──sha256──┐
                              ├─→ 比对来自 ↓ 的清单
checksums.txt(下载) ─minisign─┘
        ↑
   用**编译期冻结**的公钥验签（与 rules 钥匙分离：规则信任 ≠ 二进制信任）
```

| 攻击 | 防线 | 实现位置 |
|---|---|---|
| U1 内容投毒 | sha256 + minisign 双防线 | `VerifyArchive` / `VerifyChecksumsSignature` |
| U2 版本回滚 | semver 单调（`Compare <= 0` 即 ErrNoUpdate） | `SelectAsset` |
| U2' 降级攻击 | 仅 `--allow-downgrade` 逃生门可绕过 | `SelectOpts.AllowDowngrade` |
| U3 源替换（资产 URL 换外域） | https + 精确 host 白名单 + 无端口/无 userinfo | `AssetDomainAllowed` |
| U4 无限流/超大资产 | `MaxAssetSize`（100 MiB）+ Release 元数据尺寸核对 | `SelectAsset` |
| U7 冻结（停止更新欺骗） | 过期只告警不失效（W2 规则同策略） | 设计 §2.1 |
| U8 忘签名 | **无 `checksums.txt.minisig` 一律拒** | `SelectAsset` → `ErrNoMinisig` |
| 荒谬版本号 | 段 >6 位拒（防快进 DoS 的版本抬升） | `ParseVersion` |
| 解包攻击 | zip-slip / symlink / 设备文件 / 嵌套路径全拒；仅白名单文件 | `archive.go` |

**关键不变量**：任何失败路径都不得触碰现有安装——验签、尺寸、域检查全在下载/解包**之前**或**之后但未交换**，
交换序列是唯一修改安装目录的代码路径。

**验证过的地方**（不是自说自话）：`checksums.txt.minisig` fixture 由官方 `minisign 0.11 -S` 生成、
官方 `minisign -Vm` 独立验签通过；`engine/internal/minisign` 的签名由官方工具反向验证（本地必跑，CI 跳过）。

## 3. 交换算法（"永不自毁"的物理实现）

### 3.1 布局

```
<install-dir>/
  ghydra(.exe)          当前版本
  ghydra(.exe).old      上一版 = 回滚凭证
  ghydra(.exe).bad      回滚时留档的坏版（诊断用，下次启动清扫）
  update-staging/       已验签解包的新文件
```

Windows 关键事实：**运行中的 exe 可 rename 不可 delete**（同卷）。整个算法建立在这一条上。

### 3.2 交换序列（对称三拍）

```
每文件：
  [若 .old 残留 → remove]        ← 上一轮留下的（第二次更新场景）
  现版 → 现版.old                ← rename：旧版并未消失，只是改名
  staging/name → name            ← rename：新版就位
```

崩溃恢复（每次进程启动 `RecoverFromCrash`，幂等）：

```
1. 清 update-staging/（半解包的残骸一律不可信）
2. 清 *.bad
3. 若 name 缺失而 name.old 在场  → 说明崩在"旧版已改名、新版未就位"中间态
   → name.old 恢复回 name（宁可停在旧版，不可停在无程序状态）
```

### 3.3 回滚

```
name → name.bad      （坏版留档，不删——诊断证据）
name.old → name      （旧版复位）
```

前置**全量校验**：任一文件缺 `.old` 直接拒（`ErrNoOldToRollback`），不产生半回滚。

### 3.4 不变量

- 任意时刻：`name` 要么是完整旧版、要么是完整新版，**从不存在半个可执行体**；
- `.old` 是回滚的唯一凭证，只有"交换成功且自检通过"才允许被下一轮覆盖；
- 崩溃点穷举后可恢复（L1 覆盖：崩在 rename 前/中/后三态）。

## 4. 状态机（`~/.ghydra/update.json`）

```
                    ┌──────────────── 无文件 ────────────────┐
                    │                                        │
  ApplyPlan: 交换前写 ─→ {pending: V, confirmed: false}       │
                    │                                        │
  下次任意启动（BootHook）                                    │
        │                                                    │
        ├─ 自检通过 ─→ Confirm() ─→ {confirmed: true} ─→ 正常启动
        │
        └─ 自检失败 ─→ BootAttempts++ 
                         ├─ ≤2 次：留 pending 等下次（可能是一次性环境问题）
                         └─ 第 3 次：自动回滚 + BadVersion=V
                                    → {pending: "", bad: V}   ← 该版本以后跳过
```

字段语义：

| 字段 | 含义 | 谁写 |
|---|---|---|
| `pending_version` | 已交换、待自检确认的版本 | ApplyPlan |
| `confirmed` | 自检已通过 | BootHook / ApplyPlan |
| `boot_attempts` | 累计失败次数（阈值 >2 触发回滚） | BootHook |
| `bad_version` | 曾启动失败被回滚的版本（后续 check 跳过） | rollbackSync |
| `daemon_was_running` | 是否需重启 daemon 对账 | ApplyPlan |

**原子性**：`tmp + fsync + rename`（`fsx.WriteFileAtomic`）；崩溃只留 `.tmp`，不会读到半 JSON。

**双路径实现**：同一状态机两处调用——`ApplyPlan` 内联（更新当场验证）与 `BootHook`（重启后兜底），
共享 `saveStateMerge`（读-改-写，避免覆盖 BootAttempts / BadVersion 等旁路字段）。

## 5. 自检与 daemon 对账协议

### 5.1 自检（`ghydra version`）

```
子进程 = <install-dir>/ghydra version
  超时：默认 10s，生产 30s（Defender 扫新 exe 可超 10s）
  环境：附加 GHYDRA_SELF_CHECK_CHILD=1  ← 递归护栏
  期望：输出含 "ghydra version <pending_version>"（用**文件内**版本，不是 CLI 参数）
```

**递归护栏是必须的**：自检跑 `version` → 该进程也进 BootHook → 又见 pending 未确认 → 又自检 → 套娃到超时被杀
（冒烟首跑实证 `signal: killed`）。三重防线：`version` 子命令跳过 BootHook + 子进程 env 标记 + 状态机本身的幂等。

### 5.2 daemon 版本对账

```
1. Stop 旧 daemon（等端口下放行；5s 优雅超时则 SIGKILL / taskkill /F 升级）
2. spawn 新 daemon（**用交换前捕获的 exe 路径**，见 §6.1）
3. 轮询 GET /api/status（X-GHydra-Token）直到 version == 目标
   - 每轮重读 serve.json 取**当前端口**（见 §6.2）
   - 成功/失败的观测都进心跳日志（10s 一条：port/last/want/err）
```

对账失败 → 走 `rollbackSync`（与自检失败同路径），并把 daemon 拉回旧版。

## 6. 三个必须预捕获/显式传递的东西（每个都对应一个真 bug）

### 6.1 exe 路径：`os.Executable()` 在交换后会漂移

```
Linux  os.Executable() → readlink("/proc/self/exe")
                             ↑ 交换把运行中的 exe rename 成 .old 后，这根链接指向 .old
macOS  proc_pidpath 同源风险；Windows PEB 路径不受影响

症状：apply 成功、交换成功，但重启的 daemon 是**旧版二进制** → 对账 60s 一直读到旧 version
```

**解法**：构造 `Updater`（交换之前）捕获 `selfPath`，`daemonControlProd` 全程用它；
`os.Executable()` 仅作非自更新路径（boot hook）兜底。回归测试 3 个（含 rename 语义复现）。

### 6.2 端口：运行态真相归 daemon

端口冲突时 serve 会迁移 `+1..+8`（M1-W3 设计）。**请求值 ≠ 绑定值**，而更新器/脚本只能按绑定值对账。

**解法**：托管 serve 绑定成功后**自己写 `serve.json`**（实际 `ln.Addr()` 端口）；对账每轮重读；
非托管（前台调试）不写，避免覆盖 `ghydra on` 建立的运行态。

### 6.3 用户主目录：Windows 上 HOME ≠ 用户目录

`os.UserHomeDir()` 在 Windows 读 `USERPROFILE`，忽略 `HOME`（Go 官方行为）。
只设 `HOME` 时状态文件会落到**真实用户 profile**。测试/脚本两个变量都要设；
跨平台路径还要 `pwd -W` 原生化（Git-bash 的 `/d/a/...` 原生 Go 进程打不开）。

## 7. 测试矩阵（测试计划先于实现冻结，逐条对应）

| 层级 | 数量 | 内容 |
|---|---|---|
| L1 单元（先写后实现） | 24 | semver 表驱动（含前导零/超长/多段）；Release 解析 + 选择门槛（U2/U3/U4/U8 + bad_version + prerelease）；checksums 解析（重复名/缺文件名/坏 hex）+ 官方工具互操作 fixture；swap 三态 + 崩溃恢复 + 回滚；状态机 roundtrip/损坏 JSON/原子性/自检门/×3 回滚 |
| L2 集成 | 12 | fake Release server（TLS httptest）+ fakebin 真子进程自检 + fake daemon：happy path、双防线篡改、回滚拒 + 显式降级、尺寸不符、崩溃恢复三态、自动回滚（内联 + BootHook 两路径）、daemon 对账 |
| L3 黑盒（CI 三平台） | 4 场景 | A 正常链（凭证+状态）/ B 回滚 + bad_version / C 投毒拒 + 零破坏 / D daemon 对账（换 pid + 版本跳变） |
| 回归锁 | 3 | 交换后 exe 路径必须用预捕获值（含与 `os.Executable()` 的语义差异复现） |

**L3 的关键设计**：真编译 v1.0.0/v1.0.1 两版二进制（ldflags 注入测试公钥），
`releasesrv` 扮演 GitHub Release（含 `-tamper-asset` / `-tamper-checksums` / `-wrong-size` 攻击开关），
四场景**拆独立 CI step**——失败步骤名即定位（见 Pitfalls §10.1）。

## 8. 构建期注入（为什么测试公钥要绕 main 包）

```
go build -ldflags "-X main.Version=1.0.1 -X main.pubKeyOverrideFile=/abs/path/test.pub"
                                  ↑ 稳定可用              ↑ 经 init() 转交 selfupdate 包
```

实证：本工具链（go1.23.9）**全路径形如 `-X pkg/path.Var=...` 静默失效**（不报错、不生效），
`-X main.Var=...` 可靠。故 `selfupdate.SetPublicKeyOverride(path)` 由 cmd 层 `init()` 调用中转。

信任语义不受影响：这是**构建期**注入（改的是二进制本身），不是运行时开关；
生产构建该变量为空 → 用 `keys.go` 里的冻结公钥。
冒烟用 `testdata/test.pub`（只签测试向量，库外无价值）；release 正式钥匙 `F0716070E4E793E7`
私钥存 `/var/minis/shared/ghydra-keys/ghydra-release.key`（离线，与 rules 钥匙分离）。

## 9. 运行时可观测点（排障先看这些）

| 观测点 | 位置 | 说明 |
|---|---|---|
| 对账首见/心跳 | `[selfupdate] 对账中：port=… last=… want=… err=…` | 10s 一条；`err` 是请求级失败原因 |
| 对账端口变更 | `[selfupdate] 对账端口 = N（serve.json）` | 端口迁移时会看到跳变 |
| 起 daemon | `[selfupdate] 起 daemon: exe=… port=…` | **验证 exe 是否为新版路径** |
| 托管 serve 日志 | `~/.ghydra/serve.log` | spawnServe 显式落盘（原先丢 /dev/null） |
| 运行态 | `~/.ghydra/serve.json` | pid/port，**daemon 自写**，端口真相 |
| 状态 | `~/.ghydra/update.json` | pending/confirmed/bad |
| 自检/回滚 | `[selfupdate] vX 自检通过` / `回滚 V: 原因` | 归因链完整 |

## 10. W4p2 待办（含已识别的风险）

1. **`ghydra gui` 转正**：build tag 隔离 wails（`//go:build gui`），CLI 构建不沾 gtk/webkit——
   否则 Linux 用户装 CLI 也要拖 GUI 依赖树。
2. **`engine/diag`**：脱敏导出（token/路径/IP/主机名），强制正则断言器做门（命中即拒写 zip）。
3. **packaging**：NSIS（`%LOCALAPPDATA%\Programs\GHydra`，用户级免 UAC）、DMG、tar；
   **卸载必须先还原系统代理再删文件**（否则用户网络残废）；CI 真装真卸断言。
4. **release.yml**：ldflags 注入 `main.Version`（tag）+ 产物名与更新器 `SelectAsset` 前缀对齐
   （`ghydra-<goos>-<goarch>[.zip|.tar.gz]`）+ checksums.txt 签名（离线签 or CI secret，设计 §5 待拍板）。
5. **flags 持久化**：`--cdn` / `--rules-url` 等目前是启动旗标，GUI 设置页需要落盘配置。
6. **GUI 更新流**：p2 末尾，砍线不伤 CLI 主链（拍板 #10）。
7. **风险**：NSIS 静默更新与运行中 daemon 的交互（先停 daemon 再换文件）；
   macOS 公证缺位下的 Gatekeeper 引导文案；首次安装无 `.old` 时回滚路径（已拒，符合预期）。
