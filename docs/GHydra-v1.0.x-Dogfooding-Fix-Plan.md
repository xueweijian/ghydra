# GHydra v1.0.x Dogfooding 修复方案（设计冻结稿）

- 日期：2026-09-14
- 输入：v1.0.0 Windows 真机四轮验收（Codex 2026-09-14 20:49–21:43）+ 沙箱双网络 gh-proxy 探测
- 状态：**方案定稿，未动码**。纪律：每阶段先冻结测试计划，TDD 实现，CI 三平台绿 + Windows 复测才收官。
- 命名：F# 为修复项编号，进 commit 前缀。

---

## 0. 四轮数据 → 问题的映射

| 实证 | 定性 | 编号 |
|---|---|---|
| 首请求 30s 超时 ×2 轮 / 10s ×1 / 4.3s ×1；warm 后 0.43–2.1s 稳定 | 冷启动建连吃穿客户端预算 | F3 |
| 传输中断后调度器 score/fail_rate 无变化（0.045→0.045） | 数据面质量不回灌 | F4 |
| gh-proxy 下载端点双网络 403（CF 拦→应用层拦，一小时内切换）；objects 直连 4/4 OK | 默认镜像死，资产下载无降级链 | F2 |
| 安装器不写 PATH（Machine/HKCU 均无） | 分发阻断级 UX 缺陷 | F1 |
| web 场景 200/2.1KB/s 记 FAIL；bench 默认 bootstrap 太弱；status 无 conns；raw 域池单 IP；bench 不记 dst_ip；last_good 复用无衰减 | 可观测性/分级缺口 | F5–F10 |

不变量（不修）：on/off 注册表逐字段还原 4/4、零残留、SHA256 链、自更新链、doctor proxy_unreachable 语义。

---

## F1（P1）NSIS 写入 PATH

**方案**：EnVar 插件（v0.3.4 unicode）+ 双注册。

1. 安装：`EnVar::SetHKLM` + `EnVar::AddPath "$INSTDIR"`（装 Program Files 已是管理员上下文）；插件 DLL vendor 进仓库 `installer/plugins/x86-unicode/`，CI 免下载。
2. 同时注册 `HKLM\...\App Paths\ghydra.exe`（Win+R / ShellExecute 可达，零 PATH 污染风险）。
3. 卸载：`EnVar::DeleteValue` 精确删自身条目（绝不动 PATH 其余内容）+ 删 App Paths 键。
4. EnVar 自带 WM_SETTINGCHANGE 广播 → 新开终端立即可用，免注销。

**为什么不用** `${EnvVar}` NSIS 宏：PATH 超长时宏的字符串拼接会截断（注册表直写绕开）；为什么不用 HKCU：机器级安装语义应为全用户可见。

**测试计划**：
- L3 新增 CI job（`windows-latest`）：下载 setup.exe artifact → `/S` 静默装 → PowerShell 读 `HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment` 断言含 GHydra → 全新进程 `ghydra version` → 卸载 `/S` → 断言 PATH 条目消失。
- 回归：既有 NSIS 四红线（不预置自启/失败弹窗/off --wait 前置/不碰 .ghydra）断言不动。

**验收**：真机新开终端直接 `ghydra version`；卸载后 PATH 干净。

---

## F2（P1~P2）资产下载降级链：直连为主，镜像为多候选探活

**核心洞察**：minisign 签名链让「镜像选择」降级为**纯可用性问题**——镜像投毒在验签面前无效（A1 已防）。所以镜像可以激进轮换，不需要信任。

**下载链（get.Downloader 改造）**：
1. **主路 = 直连**：release 资产 302 落到 `objects.githubusercontent.com` / `release-assets.githubusercontent.com`，四轮直连全通。Downloader 对 `.githubusercontent.com` 域走 A 通道 IP 池（复用调度器），不走 `-cdn` 前缀。
2. **镜像 = 有序候选列表**（非单点）：
   - `-cdn` 旗标演进为 `-mirrors=a,b,c`（保持 `-cdn` 兼容=单元素列表）；
   - 默认列表来自 **rules.json 新字段 `mirrors: []`**（v11 schema，签名热更——镜像死了发规则就换，不用发版）；
   - 优先级 = 手动 > rules > 内嵌兜底列表（内嵌只放已验证 2+ 个）。
3. **探活选择**：对每个候选先拉 `checksums.txt`（<1KB，Range 请求）——成功且内容合法者胜出，全量资产下载才走它；失败候选记 SQLite `mirror_health`（成功率/最后成功时间），下次跳过（半开复活 1h）。
4. **API 侧**：`api.github.com` 直连失败时同样按候选顺序试镜像 API 端点（gh-proxy 系镜像均支持 API 透传）。

**B 通道战略面（M2 前的过渡）**：gh-proxy.com 双网络死、机制还会漂移 → 内嵌默认列表不再以它为唯一；README/文档措辞从「推荐镜像下载」改为「镜像为直连失败时的自动降级」。自建 CF Worker 模板（原 M2 课题）提前进 v1.1 的调研位。

**测试计划**：
- L1：mirror 链纯逻辑（候选排序/健康缓存/半开复活/`-cdn` 兼容解析）单测。
- L2：fakesite 起三个假镜像（好/403/慢速）断言选优与跳过；rules v11 携带 mirrors 列表热生效。
- schema：rules v11 = v10 + `mirrors`（上限 4 个、https only、每项 URL 规范化校验）；v10 客户端忽略该字段（向后兼容，验签按各自版本）。

**验收**：gh-proxy 403 环境下 `ghydra update` 仍成功（直连主路）；拔掉直连（hosts 黑洞）时镜像自动接手；/api/status 可见 mirror 链各候选健康度。

---

## F3（P2）冷启动预热 + 上游拨号超时封顶

**机制定论**（四轮证据）：首请求独自承担「sticky 拨号 → 失败 → 轮换」串行全成本；GFW tarbit 时单次 connect 21s 级（bench tcp_block 实证），一次失败即吃穿 curl 30s。第四轮 sticky 已验证（持久化复用）则首请求 4.3s 无超时 → **问题不在调度策略，在验证成本被压到了首个用户请求上**。

**三层修复**：

1. **拨号超时封顶**：上游候选 `net.Dialer{Timeout: 5s}`（现为系统默认 ~21s+）。任何单候选最多烧 5s。
2. **启动/接管异步预热**：serve 启动与 `ghydra on` 时，对每个活跃域池异步（不阻塞端口就绪）跑：
   - sticky 存在 → 先单探 sticky（5s 帽）；失败立即并行预筛 top-5 候选；
   - sticky 不存在（新域）→ 并行预筛全部候选（20 个 ×5s 并行 ≈ 5s 墙钟，替代现行串行 ~10s）。
   - 预热完成前到达的 CONNECT 请求不等预热（见 3）。
3. **客户端 CONNECT 竞速拨号**：无已验证 sticky 时，不串行试单个候选——并行竞速拨 2–3 个候选（happy-eyeballs 式），先成功者服务该连接，其余取消。客户端预算内最多两轮竞速（≤10s），仍无出路则快速回 502（浏览器/curl 秒级可见错误并重试成功），**绝不静默挂 30s**。

**度量（进 /api/status + 日志）**：每域记录 `cold_first_ms`（冷启动首请求延迟）与 `warm_p50_ms`，字段化验证修复效果。

**测试计划**：
- L1：竞速拨号器纯逻辑（2 候选 1 死 1 慢 → 选慢者成功；全死 → 快速失败）。
- L2：fake 上游（tarbit 型：accept 后不发 syn-ack…用 hang 连接模拟）断言首请求 ≤ 预算、无 21s 级挂起；预热路径断言 on 后 ≤2s 出现已验证 sticky。
- 回归红线：loadtest 千并发不退化（竞速拨号不得放大连接风暴——仅在无 sticky 时竞速，且并发去重同域同时只一组竞速，singleflight）。

**验收**：真机 ≥10 次冷启动采样，首请求 P95 < 8s，30s 超时零出现；`/api/status` 冷/热延迟字段可查。

---

## F4（P2）数据面回灌：吞吐感知计分

**根因**：relay 建立（CONNECT 200）后字节流质量对调度器不可见；只有控制面事件（握手死 `rx==0&&tx>0`）计分。GFW「连得上传不动」型 QoS 全盲。

**方案**：

1. **停滞检测（stall）**：relay 循环内维护 (t₀,b₀)；若 Δt ≥ 4s 且 Δbytes < 4KB → 判 stall → 主动断开 + `ReportFail(ip, stall)`。stall = 「完全不动」，不是「慢」——避免误伤 GFW 限速但可用的流（第二轮 16KB/s 那种是活流）。
2. **非对称关闭归因**：记录哪侧先关（upstream RST/EOF 先于 client → 记 `upstream_drop`；时长 >10s 且传输 >64KB 的 upstream_drop 才计罚，防小连接噪声）。
3. **吞吐 EWMA 分量**：连接自然结束且 bytes ≥ 64KB 时计算速率，归一化进 score：`score = 0.25·rtt + 0.35·fail + 0.15·jitter + 0.25·slow_penalty`（slow_penalty = clamp(0..1)，<100KB/s→1，>1MB/s→0）。小响应不计（ls-remote 类噪声）。
4. **stall 即时去粘**：stall 事件直接释放 sticky（下请求重新择路），不等 60s 窗口自然过期。
5. 惩罚随成功传输按既有 EWMA α 衰减。

**测试计划**：
- L1：stall 判定纯逻辑（4s/4KB 边界、活流不误判、非对称关闭归因）。
- L2：fake 上游三型（传一半 RST / 停滞 / 慢速可用）断言计分走向与 sticky 释放；小响应不计吞吐分量。
- 冒烟：smoke 断言「stall 后 60s 内 /api/status 可见 fail_rate 上升」。

**验收**：复现第二轮场景（中断后）score 显著上升、sticky 换 IP；正常慢速下载（>100KB/s 持续）不计罚。

**风险与对策**：误判合法慢流 → stall 只看「零字节窗口」；计罚过敏感 → 惩罚系数进常量区可调，先灰度观测 /api/status 数据再收紧。

---

## F5（P3）doctor「可达但慢」独立分级

- 新分类 `slow`：2xx 且 (rate < 100KB/s 或 TTFB > 2s) → WARN 不 FAIL；表头 OK 列改 OK/WARN/FAIL。
- 退出码：仅硬 FAIL 影响非零；WARN 不影响（可用率口径只算 FAIL）。
- 测试：L1 分类边界（100KB/s±、TTFB 2s±）；golden doctor_run.json 更新。

## F6（P3）bench 默认模式改 direct

- 默认 `-mode direct`（六域名报告即用户价值）；bootstrap 降为显式旗标（自举自检用）。
- smoke_w1 断言与 README 同步更新。

## F7（P3）status 输出 conns

- `ghydra status` 顶部加 `当前连接: N`（数据源=API conns 计数器，双入口同源）。

## F8（P3）githubusercontent 域池加厚

- 现状：raw 域池仅 1 IP（DoH 单答案），Fastly 边缘 .108–.111 轮动下单点必挂（第二轮实证 .109 死）。
- 方案：`*.githubusercontent.com` 池种子 = meta 全 4 段边缘 IP（185.199.108/109/110/111.133）×{443}，且**同后缀域共享池**（raw/avatars/objects/gist 解析同边缘集——rules 已有通配匹配，池键按通配组聚合）。
- 测试：L2 断言 raw 请求在 .109 死时自动轮换 .108（hosts 指向假边缘模拟）。

## F9（P3）bench/doctor 记录 dst_ip

- 自定义 DialContext 捕获实际 RemoteAddr → check JSON 加 `dst_ip` 字段、表格加列。
- 价值：第二轮「直连 2.5MB/s 用的哪个 IP」从此可答；代理路径 vs 直连路径可归因对比（配合 F3/F4 验证）。

## F10（P3）last_good 复用新鲜度衰减

- 加载持久化记录时按 age 衰减：`score += min(0.3, hours×0.02)`；age > 24h → 强制重验（并入 F3 预热）。
- 防长期陈旧记录在 IP 质量漂移后仍被信任。

---

## 发布与排期（建议）

| 版本 | 内容 | 理由 |
|---|---|---|
| **v1.0.1** | F1 PATH + F6 bench 默认 + F7 conns + F9 dst_ip | 低风险速赢；F9 是 F3/F4 的观测前提 |
| **v1.0.2** | F3 冷启动（拨号帽+预热+竞速）+ F5 doctor 分级 | 同属「首请求体验」一揽子 |
| **v1.0.3** | F4 数据面回灌 | 调度器内核改动，单独一版留观测期 |
| **v1.1.0** | F2 镜像链 + rules v11 + F8 池加厚 + F10 衰减 | 含签名规则发版与 schema bump，小版本号 |

每版走既有发布链（tag → release.yml → sign-release → 面板自更新），**修复的交付载体本身就是自更新功能——每次发版都在给 v1.0 的链路续真机验证样本**。

## 复测协议（固化）

四轮验收脚本固化为 Codex 可重复执行的清单，每版发后必跑：
1. `doctor` + `bench -mode direct`（现默认即 direct）
2. `ghydra on` → 冷启动首请求 ×5（记 cold_first_ms）→ warm ×3 → `off`
3. gh-proxy 及各镜像候选探活一行
4. 注册表三态 + 进程/端口残留
5. 数据回贴 → 与历史轮次对比表

## 7 天可用率口径（预登记，防事后争议）

- 分母 = doctor 每 check 每 snapshot；分子失败 = 硬 FAIL（F5 后 WARN 不计）。
- 双列：ghydra 开启列 vs 直连对照列（GitHub 自身故障不计 ghydra 失分，M1 拍板 #3 沿用）。
- 目标：周可用率 ≥ 99.5%（PRD 北极星）；cold_first P95 < 8s（F3 验收）。
