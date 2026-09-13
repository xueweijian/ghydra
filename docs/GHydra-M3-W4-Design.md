# GHydra M3-W4 设计 · 自更新 + 打包分发 + v1.0

> 状态：**W4p1 已实现并 CI 全绿（ce652dc）**；W4p2（打包/分发/diag/GUI 转正）待开工。
> 实现笔记（算法/协议/不变量/实证理由）见 `GHydra-M3-W4-Notes.md`；踩坑见 `GHydra-Pitfalls.md` §10。
> 前置：W3 已收官（d3af2b6 ci + ci-gui 双绿）——GUI 六页功能化、takeover 状态、version 字段占位就绪。
> 定位：v1.0 收官周。核心命题 = **"永不自毁"的自更新**（PRD L141：更新失败不破坏旧版本）+ 三平台出包 + 公测发布。
> 铁律重申（用户 2026-09-13）：**测试无比重要——本文档 §7 测试计划先于实现冻结，L1 测试先写后实现。**

## 1. 范围与非目标

**范围**：`engine/selfupdate`（检查/下载/验签/交换/回滚/重启编排）、GUI 壳产品化 + hybrid 合体、三平台打包（NSIS/DMG/tar）、分发渠道（Release + winget）、`ghydra diag` 脱敏导出、v1.0.0 发布编排。

**非目标**：
- 代码签名/notarization 不做（M3 拍板 #2：v1.0 无签名 + README 信任教学）；
- scoop 分发、Homebrew tap 视拍板 #4（默认延后）；
- 增量更新（delta patch）不做——二进制 ~11MB，全量下载可接受；
- 更新通道灰度/百分比放量不做（v2.0 众包遥测后再议）；
- GUI 内更新动画不追求（复用 get 任务进度模型，够用即可）。

## 2. 自更新核心设计

### 2.1 威胁模型（延续 TUF 记法，U=Update）

| # | 攻击 | 防线 |
|---|---|---|
| U1 | 内容投毒（资产被篡改/损坏） | 双防线：checksums.txt **SHA256** 逐资产核对 + checksums.txt 自身 **minisign 验签**（复用 rules 链 ed25519 实现） |
| U2 | 回滚攻击（喂旧版二进制，利用已修漏洞） | semver 单调：仅接受 `> current`；显式降级走 `--allow-downgrade` 交互确认（新版变砖的逃生门） |
| U3 | 源替换（Releases API 响应指向恶意产物） | repo 属主硬编码 `xueweijian/ghydra` + 资产 URL 域白名单（github.com/objects.githubusercontent.com/release-assets）+ U1 验签兜底 |
| U4 | 尺寸欺骗/无限流 | Content-Length 必须 == Release API 报的 size，且 ≤ 100MiB 硬顶 |
| U5 | 中断/半写 | tmp+fsync+rename（复用 rules/atomic.go）+ 启动时残留清扫 |
| U6 | 更新后新版启动失败（兼容 bug） | **启动自检 + 自动回滚状态机**（§2.4，"永不自毁"承诺的核心） |
| U7 | Windows 文件锁 | 运行中 exe 可 rename 不可删 → 交换序列专门设计（§2.3）+ rename_windows.go 退避经验复用 |
| U8 | 无签名 Release 误更新 | **无 minisig 的 Release 视为不可信，拒绝更新**——忘签名 = 没人更新到它（防呆由协议保证，不靠人记性） |

### 2.2 版本布局与交换模型

安装目录 = exe 所在目录（用户级安装，拍板 #1）。更新产物 = **平台 archive**（zip/tar，内含 1..N 个文件），统一模型逐文件交换，平台只是文件数不同：

```
<GHydra>/ghydra(.exe)        当前版本（daemon+CLI）
<GHydra>/ghydra-gui(.exe)    GUI hybrid（mac/linux；win 单 exe 无此文件，拍板 #5）
~/.ghydra/update.json        更新状态机（下述）
```

交换序列（swap，单文件为例）：
1. 下载 archive → 验签（checksums minisig）→ sha256 核对 → 解包校验（每文件 sha256 == checksums）到 `update-staging/`；
2. `mv ghydra.exe ghydra.old.exe`（Windows 下运行中进程的同卷 rename 合法）；
3. `mv update-staging/ghydra.exe ghydra.exe`；
4. 写 `update.json{pending: target, confirmed: false, boot_attempts: 0}`；
5. 重启编排（§2.5）。

崩溃在任意步 → 下次启动 `ghydra` 按状态机清扫（§2.4），旧版永远可跑。

### 2.3 启动自检 + 自动回滚状态机（U6 防线）

`update.json` 状态机（engine/selfupdate 纯逻辑，表驱动测试）：

```
无文件                      → 正常启动
{pending, confirmed:false}  → 自检模式启动：
  自检 = ①ghydra version 输出 == pending（拉子进程，10s 超时）
        ②若更新前 daemon 在跑（update.json 记录 daemon_was_running）：
          spawn 新 daemon → /api/status 200 且 version==pending（15s）
  通过 → confirmed:true，清 staging，成功日志 + doctor 事件
  失败 → boot_attempts++ ≤2 重试；>2 → 自动回滚：
    mv ghydra.exe ghydra.bad.exe / mv ghydra.old.exe ghydra.exe
    记 bad_version → 以后 check 遇到该版本直接跳过并提示
    若 daemon_was_running → spawn 旧 daemon → 状态对账
{confirmed:true}            → 正常启动（顺带清扫 .old/.bad/staging 残留，保留最近一个 .old）
```

- 自检逻辑内嵌在 `ghydra` 每次启动入口（子命令路由之前，开销 = 读一个 json）；
- `ghydra update rollback` 手动回滚同路径（用户主动降级场景）。

### 2.4 下载与检查链路

```
ghydra update [--pre] [--allow-downgrade]
  1. check: GET api.github.com/repos/xueweijian/ghydra/releases/latest
     （走 A 通道 bootstrap.ResolveHost per-host DoH + IP 直连——W2 真网验证过该路径）
     过滤：draft=false；prerelease 默认跳过（--pre 才装，拍板 #3）
     解析：目标平台资产 + checksums.txt + checksums.txt.minisig，无 minisig → U8 拒
  2. compare: semver（手写 ~60 行，三段+prerelease 优先级，表驱动；不引 golang.org/x/mod）
  3. download: 复用 get.Downloader（A 起步/吞吐判 B/续传/一致性防护）——
     Release 资产下载本就是 get 的主战场（M2），更新链路吃自家狗粮
  4. verify: minisign 验签 checksums → sha256 逐文件 → 尺寸上限
  5. swap + 重启编排（§2.2/2.5）
```

**密钥**：新 ed25519 钥匙对（与 rules 钥匙分离——规则信任与二进制信任职责分离，泄露互不殃及）。私钥离线（shared/ghydra-keys/ghydra-release.key，同 W2 纪律），公钥冻结 `engine/selfupdate/keys.go`；签名工具 `scripts/sign-release`（复用 sign-rules 的 minisign 签名实现，抽公共代码）。CI 只验不签。

**发布流程**：tag push → CI 出四平台产物 + checksums.txt（Release 无 minisig 不可更新态）→ 维护者本地 `scripts/sign-release` → `gh release upload checksums.txt.minisig` → 更新通道生效。

### 2.5 重启编排

- **CLI 场景**（无 daemon）：swap 后打印"已更新到 vX，运行 ghydra status 验证"。无自动重启负担。
- **daemon 场景**：swap 前 daemon 在跑（serve.json + 端口探活，与 ensureReconcile 同源判定）→ swap 后 spawn 新 daemon（复用 W3 spawn 隔离：unix setsid / win DETACHED）→ 等待探活 + version==target（15s）→ 端口沿用（serve.json 不动，新 daemon 读同一状态）。失败 → 回滚 swap + 拉起旧 daemon + 非零退出。
- **GUI 场景**（W4p2）：设置页"检查更新"→ `POST /api/update/check`（返回 latest/notes/current）→ "立即更新" → `POST /api/update/apply`（异步任务，复用 get 任务注册表 + SSE 节流模式）→ 完成后壳提示重启（壳进程自杀 + spawn 新壳，Wails 单实例锁保证不双开）。

## 3. GUI 壳产品化 + hybrid 合体

W0 spike 已验三件套（SystemTray/AutostartManager/SingleInstance 全 v3 原生）。W4p2 转正：

1. **合体**：gui/ 壳代码改为可导入包（`gui/app.go` 导出 `Run()`），主二进制加 `ghydra gui` 子命令（build tag `gui` 隔离——纯 CLI 构建不拉 wails 依赖，linux CLI 保持静态链接不被 gtk 污染）；
2. **产物形态**：win = 单 exe（wails v3 win 纯 Go CGO=0，CLI+GUI 合一）；mac/linux = 双文件（`ghydra` CLI + `ghydra-gui` GUI，后者 cgo 链接 webkit）。archive 统一模型已覆盖（§2.2）；
3. **daemon 生命周期绑定**：壳启动 → 探测 daemon → 未跑则引导一键接管（on 幂等）；关闭窗口 = 隐藏到托盘（spike 已验）；托盘菜单：显示面板/接管状态/退出（退出 = 只退壳，daemon 不动——加速不依赖 GUI 存活，D1 铁律）；
4. **GUI 更新流**（§2.5）+ 设置页"关于"卡显示版本 + 检查更新按钮。

## 4. 打包与分发

### 4.1 三平台出包（CI release job 升级）

| 平台 | 格式 | 工具 | 要点 |
|---|---|---|---|
| win | NSIS 安装器 + zip | CI runner 自带 NSIS | 装 `%LOCALAPPDATA%\Programs\GHydra`（拍板 #1，用户级免 UAC → 自更新免提权）；开始菜单快捷方式；可选自启（HKCU Run） |
| mac | DMG | hdiutil | GHydra.app bundle（Info.plist/icon/壳二进制）；无 notarization，README Gatekeeper 教学 |
| linux | tar.gz | tar | 双二进制 + README + systemd user service 示例文件 |

**卸载清理序列（NSIS，安全优先级从高到低）**：
① sysproxy 快照若在 → **先还原系统代理再卸任何文件**（否则用户网络残废——最高优先级）；② gitcfg/sshcfg insteadOf 还原（询问）；③ ~/.ghydra 数据（询问保留/删除）；④ 注册表 Run 键 + Uninstall 键 + 快捷方式。CI windows runner 真装真卸断言（§7 L3-16）。

### 4.2 分发渠道

- **GitHub Release 主渠道**；README 信任教学：sha256 + minisign 独立验证命令（`ghydra verify <file>` 子命令顺手提供 + minisign -Vm 官方工具双路径）；
- **大陆可达性**：首装的鸡生蛋问题——README 提供 gh-proxy 类公共前缀镜像下载指引 + **验签链保证镜像不可信也不致害**（这正是 minisign 设计的实证卖点）；
- **winget**：v1.0.0 发布后 PR microsoft/winget-pkgs（人工审核数天，不阻塞发布）；
- **Homebrew tap / scoop**：拍板 #4（默认延后到公测后按需）。

## 5. `ghydra diag` 脱敏导出（F8）

`ghydra diag [-o ghydra-diag.zip]`：收集版本/平台、daemon 日志尾段、doctor 24h 汇总（双列）、调度池聚合统计（状态计数，**不含原始 IP**）、规则版本、gitcfg/sshcfg 状态（不含密钥路径外内容）。

**脱敏器 = 输出前强制门**（不是后处理约定，是代码路径必经）：
- token 全文（api-token 内容绝不进包）；
- IP 打码（用户内网 IP 全码；github 公网 IP 保留——公开数据，诊断价值高）；
- HOME/用户名路径 → `~`；主机名 → `<host>`；
- 写 zip 前跑**敏感正则断言器**：注入探针（测试里已知 token/IP/路径进 → 断言 zip 内全文零命中才允许落盘）。测试锁定，新增敏感类 = 新增用例。

## 6. CI 结构增量

- `ci.yml` 新增 **selfupdate-smoke** job：三平台矩阵跑 smoke_w4 升级演练（真编译 vN/vN+1 两版二进制→升级→回滚）；
- `ci.yml` **packaging** job：win NSIS 出包 + runner 真装真卸断言；mac DMG + tar 出包结构断言；
- release job 升级：archive 化产物 + checksums.txt 生成 + （无 minisig 上传逻辑——维护者手动，U8 防呆）；
- `ci-gui.yml`：spikecheck 冒烟保留 + 壳产品构建（gui tag）进发布矩阵。

## 7. 测试计划（先于实现冻结）

**L1 单元（先写测试后写实现）**：
1. semver 解析+比较表驱动：正常三段/prerelease 优先级（`1.0.0-beta < 1.0.0`）/双位数/垃圾输入报错/build metadata 忽略/等值；
2. Releases API JSON 解析（真实响应 fixture）：找平台资产/prerelease 过滤/draft 过滤/缺 minisig 拒（U8）/缺资产报错；
3. checksums.txt 解析+核对：坏行/缺行/多行/大小写归一；
4. minisign 验签新钥匙 fixture + 官方 minisign 工具互操作向量（复用 W2 测试框架）；
5. swap 操作序列纯逻辑：文件存在性矩阵（有 old/无 old/有 staging 残留/bad 残留）→ 期望操作序列；
6. update.json 状态机：pending→confirmed / boot_attempts 递增 / >2 回滚 / bad_version 记录与跳过 / daemon_was_running 分支；
7. diag 脱敏：token/IP/路径/主机名逐类注入断言零泄漏 + 混合夹逼 + 敏感正则断言器自身的行为（命中即拒写）；
8. 资产域白名单（U3）：github.com/objects.githubusercontent.com/release-assets.githubusercontent.com 放行，其余拒。

**L2 集成（CI 化，fake Release server = httptest 起 Releases API + 资产服务）**：
9. happy path：check→download→verify→swap→自检确认全绿；
10. U1 篡改双防线：资产改一字节 → sha256 拒；checksums 改一字节 → minisig 拒；
11. U2 回滚拒（喂旧版 → "无更新"）；`--allow-downgrade` 显式降级成功；
12. U4 尺寸不符拒（Content-Length ≠ API size）；
13. 中断恢复：swap 序列各步注入崩溃 → 重启清扫 → 旧版完好可跑；
14. daemon 对账：更新前 daemon 在跑 → 更新后新 daemon 起来 + version==target + 端口不变；
15. 自动回滚：新版 = 故意坏二进制（exit 1 脚本）→ 自检失败×3 → 自动回滚 → 旧版接管 → bad_version 生效（同版本再 check 跳过）。

**L3 黑盒 smoke_w4.sh（CI 三平台矩阵）**：
16. 真二进制升级演练：go build vN（ldflags 注入版本）→ 起 serve → update → 新 version 确认 → rollback 演练 → daemon 状态对账；
17. NSIS 真装真卸（win CI）：安装 → 文件/注册表/快捷方式断言 → **接管状态下卸载 → 断言系统代理已还原** → 残留清单断言；
18. DMG/tar 结构断言（bundle 布局/双二进制/可执行位）；
19. diag 端到端：造脏数据（真 token 文件+假 IP 日志）→ diag → 解 zip → 敏感探针零命中。

**L4 真机（用户，v1.0 验收 = M3 退出标准④）**：
20. 全新 Windows 机器：双击 NSIS → 一键接管 → clone/push/Release 下载零配置 → 更新演练（装 beta1 → 升 beta2 → 验证 → 回滚）→ 卸载干净；
21. mac：DMG 安装 + Gatekeeper 教学 walkthrough + 壳三件套（托盘/自启/单实例）真机勾验（W0 遗留清单）。

## 8. 拍板点（5 个）

| # | 问题 | 建议 |
|---|---|---|
| 1 | win 安装位置：`%LOCALAPPDATA%\Programs\GHydra`（用户级免 UAC，自更新免提权）vs Program Files（标准需 UAC） | **用户级**（dev-sidecar/VSCode 用户安装同款；自更新体验决定性更好） |
| 2 | 新版启动失败：自动回滚 vs 仅手动 `ghydra update rollback` | **自动回滚**（"永不自毁"承诺核心；成本 = 状态机 ~150 行 + 测试，值得） |
| 3 | prerelease：默认跳过 vs 默认装 | **默认跳过，`--pre` 显式开启**（v1.0.0-beta 阶段正好用 prerelease 演练升级链） |
| 4 | Homebrew tap v1.0 同发 vs 延后 | **延后**（需新 repo + PAT secret + 维护面；公测有 mac 用户呼声再做） |
| 5 | GUI 更新流（壳内检查+更新+重启）v1.0 进 vs 延后 | **进，放 p2 末尾**（CLI 更新先行独立交付，GUI 流砍线不影响主链） |

## 9. 实施顺序（4 phase，测试先行）

| Phase | 内容 | 测试门 |
|---|---|---|
| W4p1 | selfupdate 核心：semver/Release 解析/checksums+minisig 链/swap/状态机/自动回滚/daemon 重启编排 + `ghydra update` CLI | L1 全部（1-8 先写）+ L2 9-15 + smoke_w4 场景 16 |
| W4p2 | hybrid 合体（gui tag + `ghydra gui`）+ 壳转正（daemon 生命周期/托盘/自启/单实例产品化）+ /api/update/* + GUI 更新流 | 壳冒烟升级 + golden 双端（update 帧契约）+ API 测试 |
| W4p3 | packaging：archive 统一产物 + NSIS/DMG/tar + sign-release + CI release/packaging job | L3 17-18 + CI 出包断言 |
| W4p4 | `ghydra diag` 脱敏 + winget manifest + v1.0.0 发布编排（tag→出包→签名→上传→公告）+ 用户真机验收 | L1-7 + L3-19 + L4 20-21 |

每 phase：设计不变则直接 TDD；退出标准 = 本 phase 测试门全绿 + CI 三平台绿 + 真机走查（涉渲染/安装类）。
