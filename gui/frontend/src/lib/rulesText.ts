// rulesText.ts —— 规则页纯函数层（W3b）：后端分类 → 用户可读语言。
// 值域锁定 engine/rules/refresher.go 七分类常量；改后端必须同步这里
// （vitest rulesText.test.ts 全量映射断言）。

/** 拉取结果七分类 → 人话。拒绝类 = 防御生效（W2 威胁模型显性化）。 */
export function refreshResultText(lastResult: string): string {
  switch (lastResult) {
    case "":
      return "从未拉取";
    case "ok":
      return "已更新";
    case "fetch_err":
      return "拉取失败（网络，保留现行规则）";
    case "sig_rejected":
      return "验签失败，拒绝应用（疑似内容篡改）";
    case "rollback":
      return "版本回滚，拒绝应用（疑似重放攻击）";
    case "fast_forward":
      return "版本快进，拒绝应用（疑似锁定攻击）";
    case "schema_rejected":
      return "内容不合规，拒绝应用";
    case "size_exceeded":
      return "数据超限，拒绝应用";
    default:
      return lastResult; // 未知分类原样透出，不吞
  }
}

/** 信任来源徽章。 */
export function sourceText(source: string): string {
  switch (source) {
    case "embedded":
      return "内嵌兜底";
    case "disk":
      return "本地签名版";
    case "remote":
      return "远程最新";
    default:
      return source;
  }
}

/** 本地 stale 展示判定（只影响徽章颜色——信任判定在引擎，A6 不失效）。 */
export function isStale(expiresAt: string): boolean {
  if (!expiresAt) return false;
  const t = Date.parse(expiresAt);
  if (Number.isNaN(t)) return false;
  return t < Date.now();
}
