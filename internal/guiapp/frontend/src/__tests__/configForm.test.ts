// configForm.test.ts —— 运行参数表单 diff（P4c L1；p4a 后端契约）。
import { describe, expect, it } from "vitest";
import { diffPatch, restartHint, valuesOf } from "../lib/configForm";
import type { ApiConfig, ConfigSetResp } from "../types";

const cfg = (over: Partial<ApiConfig> = {}): ApiConfig => ({
  listen: "127.0.0.1:9801",
  scheduler: true,
  cdn: "",
  doctor_every_s: 3600,
  doctor_repo: "xueweijian/ghydra",
  managed: false,
  rules_url: "",
  rules_interval_s: 21600,
  ...over,
});

describe("diffPatch", () => {
  it("无变化 = 空 patch（后端指针语义：缺省 = 不改）", () => {
    expect(diffPatch(cfg(), valuesOf(cfg()))).toEqual({});
  });

  it("cdn 变化单独提交（热更字段）", () => {
    const v = { ...valuesOf(cfg()), cdn: "https://gh-proxy.com/" };
    expect(diffPatch(cfg(), v)).toEqual({ cdn: "https://gh-proxy.com/" });
  });

  it("持久化字段分别进入 patch", () => {
    const v = { ...valuesOf(cfg()), listen: "127.0.0.1:9802", rulesUrl: "https://mirror/current.json" };
    expect(diffPatch(cfg(), v)).toEqual({
      listen: "127.0.0.1:9802",
      rules_url: "https://mirror/current.json",
    });
  });

  it("混合变更只带变化项", () => {
    const v = { ...valuesOf(cfg()), cdn: "https://x/", doctorEveryS: 7200 };
    expect(diffPatch(cfg(), v)).toEqual({ cdn: "https://x/", doctor_every_s: 7200 });
  });
});

describe("restartHint", () => {
  it("无重启项 = null", () => {
    const resp: ConfigSetResp = { ...cfg() };
    expect(restartHint(resp)).toBeNull();
  });

  it("字段名翻译成人话（cdn 不需要重启不在名单语义内）", () => {
    const resp: ConfigSetResp = { ...cfg(), requires_restart: ["listen", "rules_url"] };
    expect(restartHint(resp)).toContain("监听端口");
    expect(restartHint(resp)).toContain("规则源");
  });

  it("未知字段原样显示", () => {
    const resp: ConfigSetResp = { ...cfg(), requires_restart: ["future_field"] };
    expect(restartHint(resp)).toContain("future_field");
  });
});
