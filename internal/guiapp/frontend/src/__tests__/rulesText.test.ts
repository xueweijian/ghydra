// rulesText.test.ts —— 七分类人话文案 + stale 判定（W3b）。
// 值域与 engine/rules/refresher.go 常量一一对应。
import { describe, it, expect } from "vitest";
import { refreshResultText, sourceText, isStale } from "../lib/rulesText";

describe("refreshResultText 七分类（refresher.go 常量）", () => {
  it("空 = 从未拉取", () => {
    expect(refreshResultText("")).toBe("从未拉取");
  });
  it("ok = 已更新", () => {
    expect(refreshResultText("ok")).toBe("已更新");
  });
  it("fetch_err = 拉取失败（网络类，非拒绝）", () => {
    expect(refreshResultText("fetch_err")).toContain("拉取失败");
  });
  it("拒绝类带攻击语义（用户可读的防御语言）", () => {
    expect(refreshResultText("sig_rejected")).toContain("验签失败");
    expect(refreshResultText("sig_rejected")).toContain("拒绝");
    expect(refreshResultText("rollback")).toContain("回滚");
    expect(refreshResultText("fast_forward")).toContain("快进");
    expect(refreshResultText("schema_rejected")).toContain("不合规");
    expect(refreshResultText("size_exceeded")).toContain("超限");
  });
  it("未知值兜底原样展示", () => {
    expect(refreshResultText("weird")).toBe("weird");
  });
});

describe("sourceText 来源徽章", () => {
  it("三来源人话", () => {
    expect(sourceText("embedded")).toBe("内嵌兜底");
    expect(sourceText("disk")).toBe("本地签名版");
    expect(sourceText("remote")).toBe("远程最新");
  });
  it("未知原样", () => {
    expect(sourceText("x")).toBe("x");
  });
});

describe("isStale 本地展示判定（只影响徽章色，不影响信任）", () => {
  it("过期时间为空不算 stale", () => {
    expect(isStale("")).toBe(false);
  });
  it("已过期 = stale（A6：可观测不失效）", () => {
    expect(isStale("2020-01-01T00:00:00Z")).toBe(true);
  });
  it("远期不过期", () => {
    expect(isStale("2099-01-01T00:00:00Z")).toBe(false);
  });
  it("非法时间串不抛异常", () => {
    expect(isStale("not-a-time")).toBe(false);
  });
});
