# GHydra（九头鸟）

> 一头直连，一头 CDN —— 断一头，活一头。
> 中国开发者的 GitHub 全链路加速器：网页 / 登录 / clone / push / Release 下载 / SSH 六场景，周可用率 ≥ 99.5%。

**状态：M0（技术验证期）** —— 当前仓库包含 ClientHello 解析/改写 PoC 与四级自举链原型。

## M0 组成

| 模块 | 说明 |
|---|---|
| `engine/sni` | 手写 TLS ClientHello 解析 + SNI 改写（不解密、零第三方依赖、可审计） |
| `engine/bootstrap` | 四级自举链：meta API → DoH → last-good 缓存 → 冻结种子直连 |
| `engine/cmd/ghydra` | CLI：`bench`（自举链报告）/ `poc`（本地 SNI 转发器） |

## 用法

```bash
# 四级自举链探测（真机验收用 --json 贴回报告）
go run ./engine/cmd/ghydra bench --json

# 本地 SNI 转发器（curl --resolve 配合验证）
go run ./engine/cmd/ghydra poc --listen 127.0.0.1:8443
curl --resolve github.com:8443:127.0.0.1 https://github.com:8443 -kI
```

## 设计文档

- [docs/GHydra-PRD.md](docs/GHydra-PRD.md) — 产品需求 + 技术路线
- [docs/GHydra-TechReference.md](docs/GHydra-TechReference.md) — 技术参考地图
- [docs/GHydra-DevWorkflow.md](docs/GHydra-DevWorkflow.md) — 工程约定（CI 即开发环境 / 测试优先 / 阶段串行）

## 开发

CI 即开发环境：push 触发 lint（gofmt/vet）→ 三平台单测 → 基准记录 → 四目标交叉编译 + artifact。见 [.github/workflows/ci.yml](.github/workflows/ci.yml)。

## License

MIT
