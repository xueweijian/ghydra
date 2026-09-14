# GHydra v1.0.0 Windows 真机 dogfooding 验收报告

- 测试日期: 2026-09-14 20:49–20:58 (本地时间)（第一轮，v2rayN 挂机未接管）/ 21:24–21:30（第二轮，干净环境，v2rayN 等代理软件已全部退出）
- 执行方式: 真机 CLI 自动化验收（ZCode 会话，工作目录 D:\gh）
- 最终状态确认: ✅ ghydra off 已执行 / 注册表已还原 / 无残留进程（两轮均确认）
- 第二轮补测数据见文末「第二轮（干净环境）复测」章节

---

## 1. 系统信息

| 项目 | 值 |
|---|---|
| Windows 版本 | Windows 10 Home China（产品名字符串）/ NT 10.0.26200.0 |
| 架构 | 64-bit (amd64) |
| 代理/VPN 软件 | v2rayN.exe 在运行（PID 14360，且在 HKCU Run 自启动中）；Proxifier 也在自启动项 |
| 系统代理状态 | ProxyEnable=0（关闭）。直连 github.com 被 TCP 阻断，判断 v2rayN 未开启 TUN/系统接管，测试流量为真实直连 |

## 2. 下载与校验

- 实际使用 URL: **GitHub 直连全部成功**（checksums.txt / setup.exe / gui.zip 三个文件）
- **镜像 https://gh-proxy.com 被 Cloudflare 拦截**，返回 HTTP 拦截页 "Sorry, you have been blocked"（Ray ID: a3af817f7e4d4679），未能通过镜像下载
- SHA256 校验（certutil -hashfile，对比 checksums.txt）:

| 文件 | 计算值 | checksums.txt | 结果 |
|---|---|---|---|
| ghydra-windows-amd64-setup.exe | c7b603685b9b4c7cb46cb51c00a94967ba38967c1dd9b919c9e92b0ddcf9dd52 | c7b603...d52 | ✅ 一致 |
| ghydra-windows-amd64-gui.zip | c6ae937165425c199f23e294e07ed5cf3eb1302f31531c3b8a8ed5a5a6dc24ba | c6ae93...4ba | ✅ 一致 |

## 3. 安装

- 命令: `ghydra-windows-amd64-setup.exe /S` → 退出码 0
- 安装目录: `C:\Program Files\GHydra\ghydra.exe`
- `ghydra version` 输出:
  ```
  ghydra version 1.0.0
  检查更新: ghydra update check（--pre 含预发布）
  ```
- ⚠️ **问题: 安装器未把 GHydra 写入 PATH**（Machine PATH 与 User/HKCU Environment PATH 均无 GHydra 条目）。新开终端直接执行 `ghydra` 会找不到命令，只能用全路径

## 4. doctor 完整输出

```
2026/09/14 20:52:22.150496 [doctor] direct 六场景探测开始（timeout=12s）
2026/09/14 20:52:34.195075 [doctor] direct 完成：五场景 1/5；含 SSH 观测 2/6，12002ms
2026/09/14 20:52:34.195608 [doctor] proxy 六场景探测开始（timeout=12s）
2026/09/14 20:52:34.199960 [doctor] proxy 完成：五场景 0/5；含 SSH 观测 0/6，4ms
doctor direct 五场景 1/5（20.0%），含 SSH 观测 2/6，总耗时 12002ms
SCENARIO   CHECK              OK     REACH            STATUS  CLASS             TTFB_MS  RATE_BPS ERROR
web        web                FAIL   yes              200     timeout             206.3      2109 context deadline exceeded
login      login              FAIL   no               0       timeout               0.0         0 Get "https://github.com/login": context deadline exceeded
clone      clone-dry-run      OK     yes              200     ok                  445.6         0
push       push-dry-run       FAIL   no               0       timeout               0.0         0 Get "https://github.com/xueweijian/ghydra.git/info/refs?service=git-receive-pack": context deadline exceeded
release    release-ttfb-rate  FAIL   no               0       timeout               0.0         0 Get "https://github.com/cli/cli/releases/latest": context deadline exceeded
ssh        ssh-22             OK     yes              0       ok                    0.0         0
ssh        ssh-443            OK     yes              0       ok                    0.0         0
doctor proxy  五场景 0/5（0.0%），含 SSH 观测 0/6，总耗时 4ms
SCENARIO   CHECK              OK     REACH            STATUS  CLASS             TTFB_MS  RATE_BPS ERROR
web        web                FAIL   no               0       proxy_unreachable       0.0         0 Get "https://github.com/": proxyconnect tcp: dial tcp 127.0.0.1:9801: connectex: No connection could be made because the target machine actively refused it.
login      login              FAIL   no               0       proxy_unreachable       0.0         0 Get "https://github.com/login": proxyconnect tcp: dial tcp 127.0.0.1:9801: connectex: No connection could be made because the target machine actively refused it.
clone      clone-dry-run      FAIL   no               0       proxy_unreachable       0.0         0 Get "https://github.com/xueweijian/ghydra.git/info/refs?service=git-upload-pack": proxyconnect tcp: dial tcp 127.0.0.1:9801: connectex: No connection could be made because the target machine actively refused it.
push       push-dry-run       FAIL   no               0       proxy_unreachable       0.0         0 Get "https://github.com/xueweijian/ghydra.git/info/refs?service=git-receive-pack": proxyconnect tcp: dial tcp 127.0.0.1:9801: connectex: No connection could be made because the target machine actively refused it.
release    release-ttfb-rate  FAIL   no               0       proxy_unreachable       0.0         0 Get "https://github.com/cli/cli/releases/latest": proxyconnect tcp: dial tcp 127.0.0.1:9801: connectex: No connection could be made because the target machine actively refused it.
ssh        ssh-22             FAIL   no               0       proxy_unreachable       0.0         0 proxy dial 127.0.0.1:9801: dial tcp 127.0.0.1:9801: connectex: No connection could be made because the target machine actively refused it.
ssh        ssh-443            FAIL   no               0       proxy_unreachable       0.0         0 proxy dial 127.0.0.1:9801: dial tcp 127.0.0.1:9801: connectex: No connection could be made because the target machine actively refused it.
```
- 退出码 0
- 现象记录: proxy 组全部 `proxy_unreachable`（127.0.0.1:9801 拒绝连接）——doctor 执行时 serve 未在运行，doctor 不会自行拉起代理通道

## 5. bench --json 完整输出

### 5.1 默认模式（bootstrap）
`ghydra bench --json`（默认 `-mode bootstrap`），**仅 311ms 即返回，只含 level 1 "meta" 自举解析，无后续吞吐步骤**（与"跑 1-2 分钟"的预期不符，原样记录）:
```json
{
  "steps": [
    {
      "level": 1,
      "name": "meta",
      "ok": true,
      "elapsed_ms": 287.884,
      "ips": ["192.30.252.1","192.30.252.2","192.30.252.3","192.30.252.4","192.30.255.255","185.199.108.1","185.199.108.2","185.199.108.3","185.199.108.4","185.199.111.255","140.82.112.1","140.82.112.2","140.82.112.3","140.82.112.4","140.82.127.255","143.55.64.1","143.55.64.2","143.55.64.3","143.55.64.4","143.55.79.255"]
    }
  ],
  "ips": ["192.30.252.1","192.30.252.2","192.30.252.3","192.30.252.4","192.30.255.255","185.199.108.1","185.199.108.2","185.199.108.3","185.199.108.4","185.199.111.255","140.82.112.1","140.82.112.2","140.82.112.3","140.82.112.4","140.82.127.255","143.55.64.1","143.55.64.2","143.55.64.3","143.55.64.4","143.55.79.255"],
  "source": "meta",
  "domains": ["*.github.com","*.github.dev","*.github.io","*.githubassets.com","*.githubusercontent.com"],
  "elapsed_ms": 311.582
}
```

`bench --help` 显示存在 `-mode bootstrap|direct|proxy` 三模式，默认 bootstrap。

### 5.2 `-mode direct`（六域名存活报告，补测）
```json
{
  "checks": [
    {"scenario":"github-domains","name":"github","mode":"direct","target":"https://github.com/","ok":false,"reachable":false,"class":"tcp_block","error":"Get \"https://github.com/\": dial tcp 20.205.243.166:443: connectex: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond.","duration_ms":21064.674,"dns_ms":11.2392,"connect_ms":21048.9587},
    {"scenario":"github-domains","name":"api","mode":"direct","target":"https://api.github.com/","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":352.692,"dns_ms":4.946,"connect_ms":168.2969,"tls_ms":112.0739,"ttfb_ms":349.0124,"bytes":2262,"rate_bps":614607.1079230518},
    {"scenario":"github-domains","name":"codeload","mode":"direct","target":"https://codeload.github.com/xueweijian/ghydra/zip/refs/heads/main","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":1310.947,"dns_ms":7.8962,"connect_ms":62.8285,"tls_ms":58.2822,"ttfb_ms":817.2876,"bytes":65536,"rate_bps":132755.41835803085},
    {"scenario":"github-domains","name":"avatars","mode":"direct","target":"https://avatars.githubusercontent.com/u/9919?s=64","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":247.854,"dns_ms":7.1438,"connect_ms":74.5707,"tls_ms":83.1614,"ttfb_ms":247.3413,"bytes":1600,"rate_bps":3117692.907248636},
    {"scenario":"github-domains","name":"objects","mode":"direct","target":"https://objects.githubusercontent.com/","ok":true,"reachable":true,"status":404,"class":"ok","duration_ms":241.853,"dns_ms":12.628,"connect_ms":77.592,"tls_ms":78.041,"ttfb_ms":241.8537,"bytes":425},
    {"scenario":"github-domains","name":"raw","mode":"direct","target":"https://raw.githubusercontent.com/xueweijian/ghydra/main/README.md","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":454.361,"dns_ms":23.3387,"connect_ms":80.1023,"tls_ms":74.4436,"ttfb_ms":449.8476,"bytes":3964,"rate_bps":878254.1265093607}
  ],
  "mode": "direct"
}
```
- 退出码 0

### 5.3 status（serve 产生流量前后对比）
- on 之后、流量之前: `暂无 last_good 记录（serve 运行并产生流量后生成）`
- 经代理产生真实流量（curl 访问 github.com + git ls-remote）之后:
  ```
  DOMAIN                                   IP                        SCORE    RTT_MS  UPDATED
  github.com                               140.82.112.4:443          0.164       548  09-14 20:54
  raw.githubusercontent.com                185.199.108.133:443       0.042       139  09-14 20:54
  ```
- 代理通道真实流量验证: `curl -x http://127.0.0.1:9801 https://github.com` → **HTTP 200，TTFB 1.16s，总耗时 10.07s**（该域名直连为 tcp_block）；`git -c http.proxy=http://127.0.0.1:9801 ls-remote` 成功列出 main / ci-failure-log 分支

## 6. on/off 注册表三态对比

`HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

| 阶段 | ProxyEnable | ProxyServer | AutoConfigURL |
|---|---|---|---|
| ① 原始（on 之前） | 0 | 127.0.0.1:10808（历史残留，未启用） | （空） |
| ② `ghydra on` 后 | 0 | 127.0.0.1:10808（未被改动） | **http://127.0.0.1:9801/pac** |
| ③ `ghydra off` 后 | 0 | 127.0.0.1:10808 | （空） |

- `ghydra on` 输出: `GHydra 已接管（pac，端口 9801，pid 19448）。GitHub 流量经本地代理加速；ghydra off 恢复。`
- `ghydra off` 输出: `GHydra 已退出，系统代理已恢复。`
- **结论: PAC 方式接管；off 后三值与原始完全一致，还原完整 ✅**
- 注: `ghydra status` 在接管态下不显示连接数（只有 last_good 表），连接数见第 7 步 API 的 `conns: 3`

## 7. GUI 变体

- **zip 结构与任务描述不一致**: `ghydra-windows-amd64-gui.zip` 解压后只有一个文件 `ghydra.exe`（无 `gui/` 子目录，无 `ghydra-gui.exe`）
- 该变体通过子命令启动面板: `ghydra gui`（`--help` 标注 "桌面面板（-tags gui 构建才内置）"）
- 启动后进程: `ghydra` × 2（PID 8284、20516，应为面板 + serve 守护）
- API 探活（token 取自 `%USERPROFILE%\.ghydra\api-token`，32 字符）:
  `curl.exe -s -H "X-GHydra-Token: <token>" http://127.0.0.1:9801/api/status` 返回:
```json
{"api_version":1,"version":"1.0.0","listen":"127.0.0.1:9801","scheduler":true,"conns":3,"uptime_s":22.6138654,
 "pools":{
   "github.com":{"sticky":"140.82.112.4:443","ips":[
     {"addr":"140.82.112.4:443","state":"Active","score":0.3,"rtt_ms":1585.342,"fail_rate":0,"samples":2,"cooldown_count":0},
     {"addr":"140.82.112.3:443","state":"Active","score":0.3,"rtt_ms":1614.643,"fail_rate":0,"samples":1,"cooldown_count":0},
     {"addr":"185.199.108.4:443","state":"Cooldown","score":0.8,"rtt_ms":5000,"fail_rate":1,"samples":1,"cooldown_count":1},
     "...（其余 17 个 IP 均 Cooldown/score 0.8/fail_rate 1，略）"
   ]},
   "raw.githubusercontent.com":{"sticky":"","ips":[{"addr":"185.199.108.133:443","state":"New","score":0.5,"rtt_ms":0,"fail_rate":0,"samples":0,"cooldown_count":0}]}},
 "channel":{"state":"closed","fail_rate":0,"samples":1,"backoff_mult":0,"b_suppressed":0},
 "rules":{"version":10,"source":"disk","stale":false},
 "takeover":{"on":false,"since":""},
 "update":{"state":"idle","current":"1.0.0","updated_at":"2026-09-14T12:56:00Z"}}
```
- 含 version/takeover/scheduler 字段 ✅（另有 api_version/listen/conns/uptime_s/pools/channel/rules/update）
- 注: GUI 模式下 `takeover.on=false`（面板拉起了 serve 但未做系统代理接管，符合"未手动开启接管"的预期）

## 8. 自启动现状（第 5 步）

`HKCU\...\Run` 现有条目: OneDrive、Tabbit Browser、v2rayN、Proxifier、Edge、uTools。
**无 GHydra 条目**（安装器不预置自启，GUI 设置中也未开过，符合预期）。

## 9. 报错原文 / 异常行为清单（原样记录，未做原因推断）

1. gh-proxy.com 镜像返回 Cloudflare 拦截页: `Sorry, you have been blocked`（Ray ID a3af817f7e4d4679），镜像不可用
2. **安装器未写 PATH**: Machine PATH、HKCU\Environment PATH 均无 GHydra 条目；新终端 `ghydra` 不可直接调用
3. `ghydra bench --json`（默认 bootstrap 模式）仅 311ms 返回、只含 meta 自举步骤，与任务预期"跑 1-2 分钟"不符；需显式 `-mode direct` 才有六域名报告
4. doctor 的 proxy 组在 serve 未运行时全部 `proxy_unreachable`（dial 127.0.0.1:9801 被拒）
5. GUI zip 内无 `gui/` 目录、无 `ghydra-gui.exe`，仅单文件 `ghydra.exe`，GUI 通过 `ghydra gui` 子命令启动
6. bench direct: `https://github.com/` 主站直连 `tcp_block`（connect 阶段 21s 超时，IP 20.205.243.166）
7. doctor direct 的 web 场景: HTTP 200 已到达但分类为 `timeout`（RATE_BPS 2109，`context deadline exceeded`）
8. `ghydra status` 本身不输出"连接数"，连接数需从 API `conns` 字段获取
9. Git Bash 下直接执行下载的 exe 报 `Permission denied`，需经 PowerShell `Start-Process` 启动（环境现象，记录备查）

## 10. 结束态确认

- ✅ `ghydra off` 已执行（输出 "GHydra 已退出，系统代理已恢复"）
- ✅ 注册表三值与原始态完全一致（ProxyEnable=0 / ProxyServer=127.0.0.1:10808 / AutoConfigURL 空）
- ✅ 无残留 ghydra 进程（测试用 GUI 进程已停止）
- ✅ 9801 端口无监听（仅 TCP TIME_WAIT 自然消亡）
- ✅ 未删除 `%USERPROFILE%\.ghydra`，未改动 GitHub 仓库，无其他系统级更改

---

# 第二轮（干净环境）复测 21:24–21:30

前置：用户已手动退出 v2rayN 及其他代理软件。复测前扫描确认：无 clash/v2ray/shadowsocks/sing-box/trojan/surge/proxifier/hysteria/wireguard/openvpn/antigravity 等进程；注册表基线 ProxyEnable=0 / ProxyServer=127.0.0.1:10808 / AutoConfigURL 空；9801 端口空闲。

## doctor（干净环境，退出码 0）

```
2026/09/14 21:24:35.182086 [doctor] direct 六场景探测开始（timeout=12s）
2026/09/14 21:24:36.470290 [doctor] direct 完成：五场景 5/5；含 SSH 观测 6/6，1273ms
2026/09/14 21:24:36.474304 [doctor] proxy 完成：五场景 0/5；含 SSH 观测 0/6，4ms
doctor direct 五场景 5/5（100.0%），含 SSH 观测 6/6，总耗时 1273ms
SCENARIO   CHECK              OK     REACH            STATUS  CLASS   TTFB_MS  RATE_BPS ERROR
web        web                OK     yes              200     ok       274.9   4899627
login      login              OK     yes              200     ok       507.2   1064509
clone      clone-dry-run      OK     yes              200     ok       470.0   1504252
push       push-dry-run       OK     yes              401     ok       697.2     26191
release    release-ttfb-rate  OK     yes              200     ok      1220.1   4294972
ssh        ssh-22             OK     yes              0       ok         0.0         0
ssh        ssh-443            OK     yes              0       ok         0.0         0
doctor proxy  五场景 0/5（0.0%），含 SSH 观测 0/6，总耗时 4ms
（proxy 组 7 行与第一轮完全相同：全部 proxy_unreachable，dial tcp 127.0.0.1:9801 actively refused）
```

## bench（干净环境）

- `bench --json`（默认 bootstrap）：行为与第一轮一致，仅 level 1 meta 自举（529ms），无后续步骤
- `bench -mode direct --json`（完整输出，退出码 0）：

```json
{
  "checks": [
    {"scenario":"github-domains","name":"github","mode":"direct","target":"https://github.com/","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":449.854,"dns_ms":11.7084,"connect_ms":46.3329,"tls_ms":90.0312,"ttfb_ms":246.9687,"bytes":524288,"rate_bps":2584154.5},
    {"scenario":"github-domains","name":"api","mode":"direct","target":"https://api.github.com/","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":166.317,"dns_ms":0.5538,"connect_ms":50.0223,"tls_ms":58.0638,"ttfb_ms":165.7992,"bytes":2262,"rate_bps":4360902.3},
    {"scenario":"github-domains","name":"codeload","mode":"direct","target":"https://codeload.github.com/xueweijian/ghydra/zip/refs/heads/main","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":711.361,"dns_ms":8.7509,"connect_ms":54.646,"tls_ms":133.6442,"ttfb_ms":487.9951,"bytes":65536,"rate_bps":293401.3},
    {"scenario":"github-domains","name":"avatars","mode":"direct","target":"https://avatars.githubusercontent.com/u/9919?s=64","ok":true,"reachable":true,"status":200,"class":"ok","duration_ms":258.848,"dns_ms":9.6561,"connect_ms":78.7885,"tls_ms":89.8673,"ttfb_ms":255.1243,"bytes":1600,"rate_bps":429576.3},
    {"scenario":"github-domains","name":"objects","mode":"direct","target":"https://objects.githubusercontent.com/","ok":true,"reachable":true,"status":404,"class":"ok","duration_ms":262.372,"dns_ms":16.2022,"connect_ms":80.853,"tls_ms":85.7881,"ttfb_ms":260.9959,"bytes":424,"rate_bps":308050.0},
    {"scenario":"github-domains","name":"raw","mode":"direct","target":"https://raw.githubusercontent.com/xueweijian/ghydra/main/README.md","ok":false,"reachable":false,"class":"tls_reset","error":"Get \"https://raw.githubusercontent.com/xueweijian/ghydra/main/README.md\": read tcp 192.168.100.5:56028->185.199.109.133:443: wsarecv: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond.","duration_ms":19494.868,"dns_ms":10.5635,"connect_ms":109.878,"tls_ms":112.0987}
  ],
  "mode": "direct"
}
```

## on/off + 真实流量（干净环境）

- `ghydra on` 输出：`GHydra 已接管（pac，端口 9801，pid 8204）。`；注册表：ProxyEnable=0 / ProxyServer=127.0.0.1:10808（未动）/ AutoConfigURL=http://127.0.0.1:9801/pac
- 流量测试（这轮明显不稳）：
  1. `curl -x http://127.0.0.1:9801 https://github.com` 首次：**30s 超时 HTTP 000**（curl exit 28）
  2. 重试 1：**HTTP 200**，总耗时 21.01s，TTFB 2.78s
  3. 重试 2：HTTP 200 但 **30s 时传输中断**（已收 483801 bytes，curl exit 28）
  4. `git -c http.proxy=... ls-remote`：**成功**（main / ci-failure-log）
  5. `raw .../README.md` 经代理：**30s 超时 HTTP 000**
- on 后 `ghydra status`：
  ```
  DOMAIN                       IP                        SCORE    RTT_MS  UPDATED
  github.com                   20.205.243.166:443        0.045       149  09-14 21:25
  raw.githubusercontent.com    185.199.108.133:443       0.042       139  09-14 20:54
  ```
- `ghydra off` 输出：`GHydra 已退出，系统代理已恢复。`；注册表还原为 ProxyEnable=0 / ProxyServer=127.0.0.1:10808 / AutoConfigURL 空 ✅；ghydra 进程 0；9801 无监听 ✅

## 两轮对比摘要

| 项目 | 第一轮（v2rayN 挂机未接管） | 第二轮（干净环境） |
|---|---|---|
| doctor direct | 1/5，12002ms | **5/5，1273ms** |
| github.com 直连 | tcp_block（21s 超时） | **OK，2.58 MB/s** |
| raw 直连 | OK（.108.133） | **tls_reset（.109.133，19.5s）** |
| 代理通道 github.com | HTTP 200，总 10.07s | 首次 30s 超时→重试 200/21s→再试传输中断 |
| raw 经代理 | （未单测，通道整体可用） | 30s 超时 |
| git ls-remote 经代理 | 成功 | 成功 |
| 注册表还原 | ✅ 完整 | ✅ 完整 |

- 客观记录：两轮直连结果差异极大（github.com 从阻断→全通，raw 从通→tls_reset，且 raw 两次解析到不同 Fastly IP .108.133/.109.133），说明本机出口网络对 GitHub 的连通性在分钟级波动；两轮代理通道均可用但第二round慢且抖动。第二轮 doctor direct 5/5 说明当时直连本身已全通，代理加速价值需在网络恶化窗口对比。

---

# 第三轮复测（环境同第二轮）21:35–21:38

前置确认：代理类进程数 0；注册表基线 ProxyEnable=0 / ProxyServer=127.0.0.1:10808 / AutoConfigURL 空。

## doctor（退出码 0）
```
doctor direct 五场景 5/5（100.0%），含 SSH 观测 6/6，总耗时 1773ms
web        web                OK   200  ok    622.3  1392625
login      login              OK   200  ok   1011.7  1625459
clone      clone-dry-run      OK   200  ok    849.8        0
push       push-dry-run       OK   401  ok    839.8    71883
release    release-ttfb-rate  OK   200  ok   1658.6  2117135
ssh        ssh-22 / ssh-443   OK
doctor proxy 组：与第一、二轮完全相同，全部 proxy_unreachable（serve 未运行）
```

## bench
- bootstrap：与一、二轮一致，仅 meta（341ms）
- `-mode direct`：**6/6 全 OK**（退出码 0）：
  - github: 200, 647ms, rate 1,339,181 B/s（connect 51.6ms, tls 135.1ms, ttfb 255.5ms, bytes 524288）
  - api: 200, 240ms, 3,353,595 B/s
  - codeload: 200, 1432ms, 119,401 B/s
  - avatars: 200, 257ms, 2,853,067 B/s
  - objects: 404, 361ms
  - raw: **200, 404ms, 3,634,030 B/s**（第二轮的 tls_reset 为瞬时波动，本轮恢复）

## on/off + 真实流量
- on: `GHydra 已接管（pac，端口 9801，pid 5576）`；注册表 PAC 生效，ProxyEnable/ProxyServer 未被碰
- github.com 经代理 3 次：
  1. **30s 超时 HTTP 000**（curl exit 28，与第二轮首请求现象一致）
  2. **HTTP 200，2.14s，TTFB 0.23s**，完整下载 577,009 bytes
  3. **HTTP 200，1.48s，TTFB 0.17s**，577,010 bytes
- git ls-remote 经代理：成功（main / ci-failure-log）
- raw 经代理：**HTTP 200，0.59s**，3964 bytes（第二轮该项超时，本轮正常）
- status 池：github.com → 20.205.243.166:443（0.035 / 117ms）；raw → 185.199.108.133:443（0.024 / 81ms）
- off: `GHydra 已退出，系统代理已恢复`；注册表还原 ✅、0 进程、9801 无监听 ✅

## 三轮对比总表

| 项目 | 第一轮 20:49（v2rayN 挂机未接管） | 第二轮 21:24（干净环境） | 第三轮 21:35（干净环境） |
|---|---|---|---|
| doctor direct | 1/5，12002ms | 5/5，1273ms | **5/5，1773ms** |
| github.com 直连 | tcp_block | OK 2.58 MB/s | OK 1.34 MB/s |
| raw 直连 | OK（.108.133） | tls_reset（.109.133） | OK 3.63 MB/s |
| bench direct 总评 | 5/6（github 挂） | 5/6（raw 挂） | **6/6** |
| 经代理 github.com 首请求 | 200 但慢（总 10.07s） | **30s 超时** | **30s 超时** |
| 经代理 github.com 后续 | — | 200/21s；200 但 30s 中断 | **200/2.14s；200/1.48s（稳定快）** |
| git ls-remote 经代理 | 成功 | 成功 | 成功 |
| raw 经代理 | — | 30s 超时 | **200 / 0.59s** |
| 注册表还原 / 无残留 | ✅ / ✅ | ✅ / ✅ | ✅ / ✅ |

### 三轮定论
1. 直连 GitHub 的连通性分钟级波动（第一轮 github.com 被墙、第二轮 raw 被重置、第三轮全通），与是否运行 v2rayN 无关——v2rayN 挂机时并未接管流量，三轮差异是网络环境本身波动。
2. **可复现的产品问题：经 ghydra 代理的首个请求在冷启动时 30s 超时**（第二、三轮均复现），首连成功后后续请求稳定快速（第三轮 1.5~2.1s 完整拉取 577KB 主页、raw 0.59s）。疑似冷启动阶段上游探测/调度建连耗时超过客户端超时，建议开发关注 serve 冷启动建连路径。
3. doctor proxy 组在 serve 未运行时全部 proxy_unreachable（三轮一致，行为稳定）。
4. bench 默认 bootstrap 模式仅 meta 自举（三轮一致），完整报告需 `-mode direct`。
5. 安装器不写 PATH、gh-proxy.com 镜像被 Cloudflare 拦截、gui zip 无独立 ghydra-gui.exe——三点维持第一轮结论。

---

# 补充：gh-proxy.com 可用性追踪（21:42）

澄清：gh-proxy.com 仅在第 0 步下载阶段（20:49）试过一次，三轮 doctor/bench/on-off 复测均未涉及。应用户要求追加复测：

| 时间 | 现象 |
|---|---|
| 20:49 | **Cloudflare 边缘拦截**："Sorry, you have been blocked"（Ray ID a3af817f7e4d4679），HTTP 拦截页 |
| 21:42 下载端点复测 | 已能过 Cloudflare，但应用层返回 **403 Forbidden**（站点自制中文页"禁止访问"，0.12s，1957 bytes）；带浏览器 UA 结果相同 |
| 21:42 首页对照 | **HTTP 200 正常**（"GitHub加速下载代理"，90KB）；favicon 200 |

结论：站点本身在线，但 URL 前缀代理下载端点（`gh-proxy.com/https://github.com/...`）对本机持续拒绝，且一小时内拦截方式从 CF 边缘变为应用层 403。更换 UA 无效，疑似 IP 维度限制或下载端点策略收紧。同期 GitHub 直连正常，不影响 GHydra 分发（但镜像作为官方推荐下载渠道之一不可用，建议开发评估备用镜像）。

---

# 第四轮复测（环境同二/三轮）21:41–21:43

前置确认：代理类进程数 0；注册表基线 ProxyEnable=0 / ProxyServer=127.0.0.1:10808 / AutoConfigURL 空。

## doctor（退出码 0）
```
doctor direct 五场景 5/5（100.0%），含 SSH 观测 6/6，总耗时 1237ms
web 200 OK (203.5ms, 3.53MB/s) | login 200 OK (460.8ms) | clone 200 OK (438.8ms)
push 401 OK (506.7ms) | release 200 OK (1184.8ms, 4.34MB/s) | ssh-22/443 OK
proxy 组：与前三轮完全一致（serve 未运行，全部 proxy_unreachable）
```

## bench
- bootstrap：仅 meta（669ms），四轮行为一致
- `-mode direct`：**6/6 全 OK**（退出码 0）：
  - github: 200, 1371.9ms, 686,866 B/s（tls 479.7ms 偏慢, ttfb 608.5ms）
  - api: 200, 165.4ms, 5,540,044 B/s
  - codeload: 200, 495.4ms, 666,259 B/s
  - avatars: 200, 269.8ms, 2,940,096 B/s
  - objects: 404, 230.7ms
  - raw: 200, 1447.8ms, **9,114,739 B/s**（connect 1103.7ms 偏慢，但下载速率四轮最高）

## on/off + 真实流量
- on: `GHydra 已接管（pac，端口 9801，pid 5972）`；注册表 PAC 生效，ProxyEnable/ProxyServer 未被碰
- github.com 经代理 3 次：
  1. **HTTP 200，4.35s**（TTFB 0.25s，完整 577,009 bytes）——**本轮无冷启动超时**
  2. HTTP 200，**0.43s**（TTFB 0.17s）——四轮最快单次
  3. HTTP 200，4.95s（TTFB 0.28s，传输段波动）
- git ls-remote 经代理：成功（main / ci-failure-log）
- raw 经代理：HTTP 200，1.46s，3964 bytes
- status 池（两条记录均为第三轮 21:36 持久化，本轮 serve 直接复用，RTT 值未变）：
  ```
  github.com             20.205.243.166:443   0.035  117ms  21:36
  raw.githubusercontent.com 185.199.108.133:443  0.024   81ms  21:36
  ```
- off: `GHydra 已退出，系统代理已恢复`；注册表还原 ✅、0 进程、9801 无监听 ✅

## gh-proxy.com（本轮纳入固定测试项）
- 下载端点 `gh-proxy.com/https://github.com/.../checksums.txt`：**HTTP 403**（1957 bytes，与 21:42 复测相同的站点自制 403 页）
- 首页 `gh-proxy.com/`：**HTTP 200**（0.14s）

## 四轮对比总表

| 项目 | 一轮 20:49（v2rayN挂机） | 二轮 21:24（干净） | 三轮 21:35（干净） | 四轮 21:41（干净） |
|---|---|---|---|---|
| doctor direct | 1/5，12.0s | 5/5，1.3s | 5/5，1.8s | 5/5，1.2s |
| bench direct | 5/6（github挂） | 5/6（raw挂） | 6/6 | 6/6 |
| github.com 直连 | tcp_block | OK 2.58MB/s | OK 1.34MB/s | OK 0.69MB/s |
| raw 直连 | OK | tls_reset | OK 3.63MB/s | OK 9.11MB/s |
| 经代理 github.com 首请求 | 200 慢(10.1s) | **30s 超时** | **30s 超时** | 200 / 4.3s |
| 经代理 github.com 后续 | — | 21s / 30s中断 | 2.1s / 1.5s（快） | **0.43s** / 4.9s |
| git ls-remote 经代理 | 成功 | 成功 | 成功 | 成功 |
| raw 经代理 | — | 30s 超时 | 0.59s | 1.46s |
| gh-proxy 下载端点 | CF 边缘拦截 | （未测） | （未测） | **403 应用层拒绝** |
| gh-proxy 首页 | （未测） | （未测） | （未测） | 200 |
| 注册表还原 / 无残留 | ✅/✅ | ✅/✅ | ✅/✅ | ✅/✅ |

### 四轮定论（可贴给开发 AI）
1. **ghydra 核心链路四轮稳定**：安装/接管/off 还原四轮零失误；git 操作经代理四轮全部成功；直连正常时 doctor 5/5、bench 6/6 为常态。
2. **首请求冷启动问题四轮中两轮复现（30s 超时）、一轮慢（10s）、一轮正常（4.3s）**：表现不稳定，warm 后单次最快 0.43s，冷启动是当前最明显的体验短板。
3. **直连 GitHub 分钟级波动是环境常态**（一轮主站被墙、二轮 raw 被重置、三/四轮全通），与代理软件无关；这正说明 ghydra 调度器（persist last_good、CDN 择优）有真实价值。
4. **gh-proxy.com 镜像对本机持续不可用**：20:49 CF 边缘拦截 → 21:42/21:43 应用层 403（首页正常），一小时内拦截方式变化，建议开发准备备用镜像。
5. 其他既录问题不变：安装器不写 PATH；bench 默认 bootstrap 仅 meta；gui zip 无独立 ghydra-gui.exe（`ghydra gui` 启动）；doctor proxy 组在 serve 未运行时全部 proxy_unreachable。

---

# 补充：双击 ghydra.exe "闪退"排查 + GUI 资源缺失 bug（22:20–22:25）

## 双击闪退（非 bug）
两个变体（Program Files 安装版 / gui zip 版）无参数运行的行为一致：打印用法帮助后立即退出（退出码 2，实测复现）。双击 = 无参数运行，黑窗口打印后 <0.1s 关闭，属控制台程序正常行为。使用方式：终端内运行子命令；桌面面板需运行 gui zip 变体的 `ghydra gui` 子命令。
已在 D:\gh 创建可双击的启动器「启动GHydra面板.bat」（纯 ASCII，已验证可拉起面板）。

## 新发现 bug：GUI 前端资源未嵌入，窗口内容疑似空白
`ghydra gui` 可正常拉起（Wails v3.0.0-beta.20 / WebView2 152.0.4191.66，窗口标题 "GHydra" 出现，supervisor 拉起 serve 子进程），但日志持续报错：
```
ERR [AssetFileServerFS] Unable to handle request url=/ err=no `index.html` could be found in your Assets fs.FS
ERR [AssetFileServerFS] Unable to handle request url=/favicon.ico err=no `index.html` could be found in your Assets fs.FS
```
即 Windows gui zip 构建产物中未嵌入前端 index.html，WebView2 加载不到页面，窗口大概率显示空白。两次启动（22:22、22:24）均复现。建议检查 gui 构建的 embed 步骤/发布流水线是否漏打包前端产物。

构建信息（日志原文）：Compiler=go1.25.14，-tags=gui，CGO_ENABLED=0，vcs.revision=1a2b89222a37a42014ee71b8ed150b7b7e067866，vcs.time=2026-09-14T11:59:01Z。

测试后现场已清理：ghydra 进程 0，注册表基线未变，9801 无监听。

---

# 补充：「GUI 空白是否因下载包错误」排查（22:28–22:45）

用户质疑 GUI 空白可能是下载包不对。排查结论：**包完全正确，是发布构建本身的缺陷**。

## 证据链
1. v1.0.0 全部资产经 GitHub API 核对，Windows 仅 3 个包：setup.exe (6,977,381 B)、普通版 zip (5,296,237 B)、gui zip (6,958,887 B)。已下载的 gui zip 字节数与 API 一致，SHA256 `c6ae9371...dc24ba` 与 checksums.txt 一致。
2. 补下另外两个包核对（本次经 GitHub 直连抢在窗口期完成，期间直连再次经历 恢复→504→恢复 的波动）：
   - ghydra-windows-amd64.zip：5,296,237 B，SHA256 `7d42f844...812f5a` ✅ 与 checksums.txt 一致
   - ghydra-darwin-arm64-gui.zip：6,305,850 B，SHA256 `b78e0c43...3aad94` ✅ 与 checksums.txt 一致
3. 三个包内容对比（unzip -l）：
   ```
   windows普通版:  ghydra.exe  12,539,392 B（单文件）
   windows gui版:  ghydra.exe  17,203,200 B（单文件，比普通版大 4.66MB = Wails 运行时）
   darwin gui版:   ghydra      15,603,442 B（单文件）
   ```
   三个包都只含单个二进制，无任何独立前端资源文件 → 设计上前端应 go:embed 进二进制。
4. 运行时报错 `no index.html could be found in your Assets fs.FS` 表明嵌入的资产 FS 里没有 index.html——典型原因是打包时未先构建前端（frontend dist 为空即被 embed）。

**结论：gui zip / setup.exe 下载无误、校验通过；Windows 与 macOS 的 gui 构建存在同一缺陷（前端未嵌入，窗口必然空白），需修复发布流水线（先构建前端产物再 wails build）。**

## 附带发现
1. `ghydra get` 下载器实测（22:34，网络恶化窗口）：A 通道 HTTP 504（TTFB 11.7s）、B 通道（CDN）**HTTP 403**（TTFB 2.5s），0 字节失败——B 通道默认 CDN 疑似 gh-proxy 类镜像（与 21:42 起 gh-proxy.com 对本机 403 的现象吻合）；--json 输出结构清晰（segments 含每通道 ttfb/why_out），可观测性好。
2. **生命周期缺口**：由 GUI supervisor 拉起的 serve 子进程（22:25:15 启动）在 `ghydra off` 报告"已退出"后仍存活（pid 18468），需手动 Stop-Process。off 未覆盖 GUI 场景拉起的守护。
3. 22:28–22:40 期间再次观测到 GitHub 直连 恢复(200)→504 网关错误→恢复(200) 的分钟级波动；DNS 解析正常（20.205.243.166 为正规 GitHub IP），本地无 TUN/拦截进程，504 为链路中间层瞬时行为。
4. 环境备注：本机 PATH 中存在 `D:\Program Files\ProxyBridge` 与 `D:\Antigravity IDE\bin`（未运行）；curl 下载 504 响应体为 92 字节最小 HTML，非 GitHub 官方错误页。

## 现场确认
ghydra off 已执行；残留 serve 子进程已手动清除（0 进程）；注册表 ProxyEnable=0 / ProxyServer=127.0.0.1:10808 / AutoConfigURL 空；9801 无监听。
