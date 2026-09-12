# GHydra（九头鸟）

> 一头直连，一头 CDN —— 断一头，活一头。
> 中国开发者的 GitHub 全链路加速器：网页 / 登录 / clone / push / Release 下载 / SSH 六场景，周可用率 ≥ 99.5%。

**状态：M1 W4（直连通道 dogfooding 准备）** —— CONNECT 代理、IP 调度器、PAC/系统代理接管、doctor 六场景探针已落地；Windows 真机验收与 7 天数据收集是当前退出项。

## 当前组成

| 模块 | 说明 |
|---|---|
| `engine/sni` | 手写 TLS ClientHello 解析 + SNI 改写（不解密、零第三方依赖、可审计） |
| `engine/bootstrap` | 四级自举链：meta API → DoH → last-good 缓存 → 冻结种子直连 |
| `engine/cmd/ghydra` | CLI：`on/off/serve/status/doctor/bench` + PoC/压测 |
| `engine/probe` | W4 六场景探针、阶段计时、根因分类、direct 对照 |
| `engine/sysproxy` | Windows 注册表/WinINet、macOS networksetup、Linux gsettings |

## 用法

```bash
# 后台接管（Windows 主力平台推荐 PAC 模式）
ghydra on
# 退出并恢复接管前代理
ghydra off

# 六场景报告：direct 对照 + 本地 GHydra 通道，结果落 SQLite
ghydra doctor --mode both --json
# 7 天汇总
ghydra doctor --report --since 168h

# 自举链 / 六 GitHub 域名存活
 ghydra bench --mode bootstrap --json
ghydra bench --mode proxy --proxy http://127.0.0.1:9801 --json

# 前台服务调试
ghydra serve --listen 127.0.0.1:9801
```

## 设计文档

- [docs/GHydra-PRD.md](docs/GHydra-PRD.md) — 产品需求 + 技术路线
- [docs/GHydra-TechReference.md](docs/GHydra-TechReference.md) — 技术参考地图
- [docs/GHydra-DevWorkflow.md](docs/GHydra-DevWorkflow.md) — 工程约定（CI 即开发环境 / 测试优先 / 阶段串行）

## 开发

CI 即开发环境：push 触发 lint（gofmt/vet）→ 三平台单测 → 基准记录 → 四目标交叉编译 + artifact。见 [.github/workflows/ci.yml](.github/workflows/ci.yml)。

## License

MIT
