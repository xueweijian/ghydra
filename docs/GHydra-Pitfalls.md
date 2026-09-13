# GHydra 工程坑合集（Pitfalls）

> 维护规则：**踩到新坑随手追加**，每条 = 症状 → 根因 → 解法。
> 方法论（测试优先/阶段串行/CI即开发环境）见 DevWorkflow.md，本文只记"坑"。
> 条目前 ×N = 历史踩中次数，×2 以上的条目红线级——动手前先查本文。

## 1. Go 语言 / 标准库

- **`io.ReadAll` 无 ctx 感知** ×1（W2P3，fakesite a5 慢速流 4KB/s 绕过 fetchWait 挂 256s）。
  网络体读取一律用 readLimited（select ctx.Done + limit 双防线）；`http.Response.Body` 的读阶段**不在** `http.Client` 的 ctx 管辖内。
- **SQLite 全行 UPSERT 覆盖单列** ×1：`rules_state` 适配器 SaveRefreshState 全行写会把 `seen_max` 列清零。
  解法：写前先读现值写回；或拆「全行写」与「单列推进」两个语句（SetRulesSeenMax）。
- **Go flag 包 bool flag 带值吞位置参数** ×2（W2 get / W4 serve）：`--scheduler on` 把 `on` 当位置参数，**静默吞掉其后全部 flag**。
  解法：统一 `reorderFlags(args, "scheduler", "managed")` 防御；新 bool flag 记得登记进白名单。
- **Windows 计时器粒度 ~15ms** ×4：本地 dial 断言别写 `>0`；计时敏感测试在 windows 矩阵挂先怀疑粒度再怀疑代码。
- **t.Setenv("HOME") 在 Windows 无效** ×1：`os.UserHomeDir()` 读 USERPROFILE——两个都要设。
- **`os.UserHomeDir()` 在 HOME 空时报错**（PRoot 常态，见 §3）。

## 2. 依赖 / 构建

- **GOPROXY 必须换 goproxy.cn**：proxy.golang.org 被墙。
- **x/sys / sqlite 等版本门槛看 go.mod 的 go 指令**：v0.48 要 go≥1.26、modernc sqlite v1.58 要 go≥1.25——先查本地 go version 再 get。
- **.gitignore 裸 `ghydra` 模式匹配了 cmd 目录** ×1 → 前缀 `/ghydra` 锚定根目录；新增二进制名同理（fakesite/sign-rules 已追加）。
- **.lastsigned（版本单调状态）是本地文件**：不进 repo；测试向量目录里误提交会锁死后续重签。
- **go:embed 不能跨目录引用上级** ×1（spikecheck 图标副本）。

## 3. PRoot 沙箱（本机开发环境）

- **HOME 可能为空** ×N：go build 报 "GOCACHE is not defined"。
  脚本第一行 `export HOME="${HOME:-/root}"`；CI 不受影响。
- **本地 -race 不可用**：Android 内核 39-bit VMA，TSAN 需 48-bit。race 全靠 CI Linux 矩阵。
- **ps aux 不可靠**（PRoot 转译）×2：进程管理一律 `start_xxx` 函数 `echo $!` 精确记录 pid。
- **git 测试 TMPDIR 指 /tmp** ×1：/data 上 loose objects 会 PRoot 残留（can't stat，删不掉）。
- **/tmp 下目录残尸删不掉** ×2（npm/git）：换全新目录名即可，别死磕 rm。
- **大文件禁令**：手机存储 ~14GB 余量，Android SDK/大模型权重/大 npm 缓存禁止下载；大任务走 GitHub Actions CI。
- **ICMP 被阻断**：ping 挂死，用 curl/wget 测连通。
- **BusyBox 工具集**：find 无 `-include`、无 globstar/brace expansion/bash arrays；grep -r 不接 --include（用 -r + --include 的 BusyBox 变体或换 find | xargs）。
- **pkill -f 会匹配自身命令行自杀** ×1：用 `[s]ched.test` 正则技巧或精确 pid。

## 4. CI（GitHub Actions）

- **windows 矩阵 run: 默认 pwsh** ×4（第 4 次是 661607a docs-only 假失败）：bash 语法第一行 CommandNotFound 崩，测试从未执行。
  铁律：**windows 矩阵里的 run: 一律显式 `shell: bash`**。
- **build job steps=[] = runner provisioning 失败**，非代码问题；同 commit 三平台只有 linux-amd64 挂即可判定，空 commit 重跑证伪。
- **flaky 判定流程**：纯 docs/tsx commit 挂测试 → diff 确认代码未变 → 空 commit 重触发。Windows 计时器粒度已 4 次（3 真坑 1 flaky）。
- **CI 失败日志考古**：失败时 runner 把 /tmp/test.out push 回 `ci-failure-log` 分支（permissions: contents: write），**匿名 git fetch 可读**——绕过 API 日志 admin 权限墙；fetch 完毕 rm 分支。
- **等待 CI 用 ~4 分钟短轮询**，不要 8 分钟长睡（用户拍板）。
- **macOS dyld 拒绝 Go≤1.23 race 二进制**（LC_UUID，golang/go#68678）：CI go-version ≥1.24。
- **签名/eol 供应链**：`*.minisig`、`engine/rules/testdata/**`、`rules/**` 必须 `.gitattributes -text`——CRLF 化 = 字节改变 = 验签必挂（global-sig 挂而主签名过的特征可诊断）。
- **Windows 读写并发 transient 冲突** ×1：读侧 os.ReadFile 撞 sharing violation、写侧 rename 三种 violation（SHARING/LOCK/ACCESS_DENIED），双侧指数退避重试（readfile_windows.go / rename_windows.go）；posix 直通。
- **windows FileListener 不支持任意 fd** ×1：listener 平台拆分（unix 自建 socket / windows net.Listen 回退）。
- **workflow 触发统一 push**，不依赖 dispatch（私有仓库 0-jobs 注册怪癖）。
- **integration tag 的文件本地默认不编译** ×1：unused import 溜进 CI——相关 job 加 `go vet -tags=integration`。

## 5. 网络（GFW / 协作模式）

- **push 代码/查 CI = 用户开魔法；真机网络验收 = 必须关魔法**（开着测的数据无效）。
- **被打断的 push 可能已成功**：重试前先 `git fetch` 确认。
- **api.github.com 301 劫持**：`--resolve api.github.com:443:140.82.112.6` IP 直连立即可用（GFW 选择性干扰，非魔法）；git push 通道正常。
- **GFW 阻断 github.com 443 时**：push 需用户开魔法；直连 SSL unexpected eof / TLS timeout 重试即可。
- **SNI 透明改写被密码学证伪**（TLS1.3 transcript 绑定 / TLS1.2 Finished 校验）：任何中间盒改写 ClientHello 必死——勿再投入（PRD F3 已改"原样转发"，`poc --rewrite-sni` 仅留作协议实验）。

## 6. 规则签名链（minisign）

- **开发私钥位置：`/var/minis/shared/ghydra-keys/ghydra-dev.key`**（公钥 fingerprint 5C89977CFADDD713 = keys.go 冻结值）。别再找 `~/.ghydra-seckey`（不存在）。workspace 不可靠存储——密钥类一律 shared。
- **minisign 0.11 -W 私钥布局 = 158B**，spec 页面过时（写 142B/独立 checksum）：Ed(2)+kdf(2)+B2(2)+salt32+ops8+mem8+key_id8+seed32+pk32+cksum32(-W 全零)；**pk 段在 94..126 不在尾部**；cksum 全零不校验（完整性由验签闭环保证）。
- **minisign CLI**：Alpine 0.11 不接受尾部裸文件名（必须 `-m`）；默认产 "ED" prehashed（blake2b-512，故引入 x/crypto/blake2b）。
- **公钥注释行 key_id（展示名）≠ blob 真实 key_id**（签名匹配用的）——sign-rules 打印必须用 blob 值。
- **sign-rules 三态**：默认 schema+版本单调全检；`-force` 跳单调（轮换/重置）；`-unsafe` 跳 schema（**仅测试向量**——运行时必拒）。schema 防线拒签毒化版本是设计特性，a4 快进向量必须 -unsafe 签出。
- **测试向量 validity ≤ 1080h（45d）** ×1：v9/v11 的 generated/expires 必须 45 天内——**CI 在未来某天后跑会过期**，归因会漂移（rollback → schema_rejected）。重签时 generated 用当天、expires 用 +30~45d。
- **vff 向量 trusted comment version=0**（unsafe 跳过解析的副作用）：签名本体有效，运行时验签过 → schema cap 拒 → fast_forward 归因 ✓。

## 7. smoke / 黑盒脚本

- **脚本 rm -rf $D 别把 build 输出一起删** ×1：BIN（/tmp/xxx-bin）与 D（/tmp/xxx 数据）目录分离。
- **HOME 复用残留持久化状态** ×1：smoke 场景间共享 HOME → rules_state 的 seen_max 残留 → 第二轮 rollback（意外实证防线工作）。起跑 `rm -rf "$D"` 或每场景独立 HOME。
- **serve 端口被占静默迁移 +1** ×1（W4 drill）：脚本轮询打到别人进程——`/status` listen 字段自检。
- **fakesite rules 模式路径映射**：URL 相对路径直接映射 rules-dir 下（`-rules-dir $VECS/v11` + `/current.json`，**URL 不再带变体名**）。
- **refresh 是异步的**：POST 后轮询 `/api/rules` 的 `refresh.last_result` 到终态（10s 超时），别 sleep 硬等。

## 8. 存储 / 工程流程

- **workspace 不是可靠存储** ×1（GHydra 源码曾在 /var/minis/workspace 消失）：重要工作副本必须确认已 push；密钥/密钥类资产另存 `/var/minis/shared/`。
- **diff 测试台应第一轮就建**（diar 教训，通用）：秒级定位 vs 小时级猜；浮点正确性包含编译器行为（FMA 收缩）。
- **file_edit old_string 逐字核对** ×2（"优先实现序"vs"优先序"）：cat -A 查不可见差异；长文改动用 python 脚本批量替换更稳。
- **golden 双端契约**：改 schema 必须显式改 golden（Go byte 比对 ↔ vitest 类型断言）；时间戳形态锁 RFC3339 UTC。

## 9. 测试向量与密钥资产管理（W2P3 新增）

- 真私钥签名向量进 repo 是**合法发布形态**（签名=公开数据）；`-unsafe` 签出的 vff 单独放，文件头注释标明。
- 新增毒化变体 checklist：①变体 json 用 python 从 current.json 改字段 ②sign-rules 签名（schema 拒的走 -unsafe）③`.gitattributes` 覆盖确认 ④smoke 加场景 ⑤归因断言写具体错误类。
- serve 侧测试注入点：`--rules-url`（A 源覆盖，信任锚不变）+ `--rules-interval`（调试缩短周期）；fakesite `-poison`（a1 内容/a5 无限流）。
