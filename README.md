# GHydra（九头鸟）

> 一头直连，一头 CDN —— 断一头，活一头。
> 中国开发者的 GitHub 全链路加速器：网页 / 登录 / clone / push / Release 下载 / SSH 六场景，周可用率 ≥ 99.5%。

**状态：M2-W4（双通道降级 + Git/SSH 集成，v0.5 beta 准备）** —— 通道 A（直连择优）/通道 B（CDN 反代，含可一键自部署的 Worker 模板）、吞吐启发下载切道、doctor 驱动的通道级熔断、insteadOf/ssh:443 集成、黑盒故障演练已落地；Windows 真机现场验收是 v0.5 之后的持续项。

## 当前组成

| 模块 | 说明 |
|---|---|
| `engine/bootstrap` | 四级自举链：meta API → DoH → last-good 缓存 → 冻结种子直连 |
| `engine/sched` | IP 池五态调度：EWMA 评分、粘性、按需探测、DoH 枯竭补充 |
| `engine/proxy` | CONNECT 隧道内核 + 明文 HTTP 路径 B 通道改写 |
| `engine/channel` | 通道级三态熔断（closed/open/half-open），doctor 判定驱动 |
| `engine/get` | 下载器：TTFB/前 1MB 启发、Range 断点续传切道、一致性校验 |
| `engine/gitcfg` | insteadOf/pushInsteadOf 成对集成：fetch 走 CDN、push 直连，快照恢复 |
| `engine/sshcfg` | ssh config 443 managed 块：first-match 语义安全写入 + 字节级还原 |
| `engine/probe` | 六场景探针、阶段计时、根因分类、direct/CDN 三列对照 |
| `engine/store` | SQLite（纯 Go）：last_good、doctor_log、托管快照 |
| `engine/sysproxy` | Windows 注册表/WinINet、macOS networksetup、Linux gsettings |
| `worker/` | Cloudflare Worker 模板（gh-proxy 协议、白名单、流式、缓存、TOKEN） |

## 用法

```bash
# 后台接管（Windows 主力平台推荐 PAC 模式）
ghydra on
# 退出并恢复接管前代理（含 git/ssh 托管键还原）
ghydra off

# 六场景报告：direct 对照 + 本地 GHydra 通道，结果落 SQLite
ghydra doctor --mode both --json
# 7 天汇总
ghydra doctor --report --since 168h

# Release 下载：A 择优起步，慢/断自动 Range 续传切 B
ghydra get <release-asset-url> -o out.bin --cdn "https://<你的worker>/t/<TOKEN>/"

# Git 集成：clone/fetch 经 CDN、push 保持直连（写前快照，disable 精确还原）
ghydra git enable --cdn "https://<你的worker>/t/<TOKEN>/"
ghydra git status
ghydra git disable

# SSH：22 断而 ssh.github.com:443 通时，写入 managed 块
ghydra ssh enable
ghydra ssh disable

# 故障演练（黑盒：真实 serve 进程 + 网络层故障注入，30s 内切道断言）
bash scripts/drill.sh

# 前台服务调试
ghydra serve --listen 127.0.0.1:9801 --cdn "https://<你的worker>/"
```

### 自部署 Worker（通道 B）

`worker/` 是可直接 `wrangler deploy` 的 Cloudflare Worker 模板：域名白名单、流式透传、SHA 归档缓存、`TOKEN` 鉴权（防配额被蹭）。免费档实测见 [docs/GHydra-W3-Freetier-Report.md](docs/GHydra-W3-Freetier-Report.md)——860MB 流式透传 CPU P99 2.8ms（限额 10ms），个人重度使用配额消耗 ≈0.05%。部署说明见 [worker/README.md](worker/README.md)。

## 设计文档

- [docs/GHydra-PRD.md](docs/GHydra-PRD.md) — 产品需求 + 技术路线
- [docs/GHydra-TechReference.md](docs/GHydra-TechReference.md) — 技术参考地图
- [docs/GHydra-DevWorkflow.md](docs/GHydra-DevWorkflow.md) — 工程约定（CI 即开发环境 / 测试优先 / 阶段串行）

## 开发

CI 即开发环境：push 触发 lint（gofmt/vet）→ 三平台单测 → 基准记录 → 四目标交叉编译 + artifact。见 [.github/workflows/ci.yml](.github/workflows/ci.yml)。

## License

MIT
