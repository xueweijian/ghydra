# GHydra M2-W4 实施设计 · Git 集成 + 演练 + v0.5

> 状态：设计中（2026-09-13 起草）。前置：W1–W3 全部交付（含免费档实测 + worker token）。
> M2-Plan 映射：§2 W4 行 + D6 安全边界；PRD 映射：F5（Git/SSH 深度集成 P0）、验收「全新机器三件事零手工配置」。

## 0. 本周交付面

| 任务 | 交付 | 验收 |
|---|---|---|
| insteadOf 自动化（快照/恢复/冲突检测） | `engine/gitcfg` + cmd `ghydra git` | 本地假 git 仓库 L2 全流程 + CI 真 git 集成测试 |
| ssh config 443 写入 | `engine/sshcfg` + cmd `ghydra ssh` | dry-run 断言 + 字节级恢复 |
| 故障演练脚本 | `scripts/drill.sh` + CI job | 黑盒进程级注入矩阵绿 |
| v0.5 beta tag + release | tag `v0.5.0-beta.1` + release job | CI 全绿 + 4 平台 artifact 附着 |

## 1. Git 集成设计（D6 具体化）

### 1.1 核心语义：insteadOf + pushInsteadOf 成对写入

M2-Plan D6 原文「push 默认仍走 A」的落地机制——git 的两个 rewrite 键配合：

```ini
# fetch/clone 走 B（CDN 前缀）
[url "https://gh.example.com/t/<TOKEN>/https://github.com/"]
    insteadOf = https://github.com/
# push 回直连（pushInsteadOf 覆盖 insteadOf 的 push 语义，git 官方文档明确）
[url "https://github.com/"]
    pushInsteadOf = https://gh.example.com/t/<TOKEN>/https://github.com/
```

- **作用域一律 `--global`**（~/.gitconfig），永不 --system；不碰仓库级 config。
- **`--push-via-b`**（可选）：不写 pushInsteadOf → push 也走 B（全阻断网络用户）。
- **`--ssh-rewrite`**（可选，默认关）：`insteadOf = git@github.com:` → SSH remote 的 fetch 也走 HTTPS-B。
- 写键前**检测用户已有 insteadOf**：任何 url.* 键的 value 或 base 涉及 github.com → 报冲突拒绝（`--force` 覆盖，冲突项打日志）；我们自己的键（base 含 ghydra 管理标记）→ 幂等重写不算冲突。

### 1.2 执行器：git CLI，不手写 INI 解析

**设计铁律：所有 gitconfig 读写经 `git config` 子命令**（`--global --get-regexp/--add/--unset-all`），不自己解析 INI——Windows .gitconfig 行尾、include、多级 key 兼容性是深坑，git 自己最懂。前提 = 目标用户必装 git（合理假设，README 声明）。

### 1.3 快照与恢复

- 快照内容（JSON，存 store `git_snapshot_json`，复用 sysproxy 快照三函数模式）：
  `{written_keys: [{key, value}...], prior: [{key, value}...], git_version, ts}`
- `ghydra git enable`：先读全部 `url.*` 相关键 → 写入 → 快照落盘。
- `ghydra git disable` / `ghydra off` 连带：unset-all 我们写的键 → 精确恢复 prior 值 → 删快照。恢复失败**不删快照**（与 sysproxy 同规）。
- 冲突快照模式：损坏/不完整快照 → 清除不阻塞（同 daemon 对账逻辑）。

### 1.4 UX

```
ghydra git enable [--cdn URL] [--push-via-b] [--ssh-rewrite] [--force] [--dry-run]
ghydra git status        # 当前键、冲突、生效演示（git config --get-regexp url.*）
ghydra git disable
```
`ghydra on --with-git`：系统代理接管后连带 git enable（默认不连带，不惊吓）。

## 2. SSH 设计

- 依据 doctor 既有探针：:22 死 + ssh.github.com:443 活 → 才允许 enable（`ghydra ssh enable` 内部先跑探针，或 `--assume-ok` 跳过）。
- **managed 块文本级插入**（ssh 无 CLI 写手）：
  ```
  # >>> ghydra managed (ssh-over-443) >>>
  Host github.com
      HostName ssh.github.com
      Port 443
      User git
  # <<< ghydra managed <<<
  ```
- ssh_config 是**first-match-wins**：块必须插文件**顶部**才能覆盖既有 Host github.com。
  - 文件不存在 / 无 github.com 冲突段 → 插顶部（安全路径）。
  - 已有 `Host github.com`（或含 github.com 的通配 Host 段）→ **拒绝**，提供 `--alias` 模式写 `Host github.com-b` 段（不修改任何已有段，文件尾部追加）并打印 remote 改写指引。
- 恢复：快照 = 整文件字节拷贝（存 data 目录），disable 时还原； markers 块单独可识别，二次 enable 幂等。

## 3. 故障演练脚本（退出标准①的黑盒进程级证明）

W1 的 fault 注入是 Go 测试内；drill.sh 把它升级为**真实二进制 + 真实进程链**：

- 本地起 fake 源站（Python http.server 变体：403 模式 / RST 模式 / 黑洞模式 / 健康模式，端口切换）。
- `ghydra serve --scheduler on --db tmp.db` 起真实进程 → 注入源头 403 → 断言 ≤30s 内新请求走 B（fake B 落点）→ 停注入 → 断言 doctor 驱动回 A。
- CI：新 job `drill`（ubuntu-latest，`go build` 后跑脚本），push 触发，与现有矩阵同门禁。
- 明确非目标：不做公网演练（那是用户真机 7 天 dogfooding 的事）。

## 4. v0.5 发布

- `on: push: tags: ['v*']` 触发 release job：4 平台 artifact（已有 build matrix 产物）+ release notes（含 worker 一键部署、TOKEN/前缀说明、git/ssh 集成指引、已知限制）。
- 发布前置核查单：CI 全绿 ×3 天 + README quickstart 覆盖六场景 + Free-tier 报告链接 + Windows 真机验收清单标注「现场项」。
- P1 顺延项（不阻塞）：credential helper PAT 引导、git lfs 向导（PRD F5 P1 → M3 一并做）。

## 5. 测试计划

| 层 | 对象 | 关键用例 |
|---|---|---|
| L1 | gitcfg 解析/快照 JSON | 成对键生成、冲突分类、幂等 |
| L2 (本地+CI) | 真 git、假 HOME | enable→clone 实际落到假 CDN base；disable→prior 精确还原；冲突拒绝 |
| L2 | sshcfg | 空文件/冲突文件/alias 模式/字节级还原 |
| CI drill | serve 进程 | 403→≤30s 切 B→恢复回 A |
| L2 (真网, 用户窗口) | `ghydra git enable` + 真实 clone/fetch/push | 经 gh.1ciyuan.cn 完整交付 |

## 6. 风险

| # | 风险 | 对策 |
|---|---|---|
| W4-R1 | insteadOf 污染用户 git（M2-R3 细化） | git CLI 执行器 + 快照恢复 + 冲突检测 + dry-run 默认提示 |
| W4-R2 | ssh config first-match 语义反直觉 | 顶部插入策略 + 冲突拒绝 + marker 块可识别 |
| W4-R3 | push 走直连在 403 事件中失败 | `--push-via-b` 逃生档 + status 输出明确提示当前 push 路径 |
| W4-R4 | drill.sh 平台差异（windows 无 bash） | drill 仅 ubuntu CI job；windows 演练归现场验收项 |
