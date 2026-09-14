// golden.test.ts —— 契约锁定（TS 侧，W1-Design §8）。
// 这些 JSON 由 Go 侧 golden 测试生成（engine/api/testdata/golden/）。
// 类型不匹配 = 编译失败；字段漂移 = 断言失败。改任何一端必须显式
// 同步，review 可见。

import { describe, it, expect } from "vitest";
import type {
  ApiStatus,
  ApiConfig,
  ConfigSetResp,
  RulesSnapshot,
  DoctorSummaryResp,
  DoctorRunResp,
  GetProgressResp,
  GetStartResp,
  MitmStatus,
  ConnFrame,
  DoctorFrame,
  GetTask,
  HelloFrame,
} from "../types";

import goldenStatus from "../../../../../engine/api/testdata/golden/status.json";
import goldenConfig from "../../../../../engine/api/testdata/golden/config.json";
import goldenDoctorSummary from "../../../../../engine/api/testdata/golden/doctor_summary.json";
import goldenDoctorRun from "../../../../../engine/api/testdata/golden/doctor_run.json";
import goldenGetProgress from "../../../../../engine/api/testdata/golden/get_progress.json";
import goldenGetStart from "../../../../../engine/api/testdata/golden/get_start.json";
import goldenMitm from "../../../../../engine/api/testdata/golden/mitm_status.json";
import goldenConn from "../../../../../engine/api/testdata/golden/sse_conn.json";
import goldenDoctorFrame from "../../../../../engine/api/testdata/golden/sse_doctor.json";
import goldenGetFrame from "../../../../../engine/api/testdata/golden/sse_get.json";
import goldenHello from "../../../../../engine/api/testdata/golden/sse_hello.json";
import goldenRules from "../../../../../engine/api/testdata/golden/rules_snapshot.json";

describe("golden ↔ types.ts 契约锁定", () => {
  it("status.json 满足 ApiStatus", () => {
    const s: ApiStatus = goldenStatus as ApiStatus;
    expect(s.api_version).toBe(1);
    expect(s.version).toBe("0.5.0-beta.1");
    expect(s.listen).toContain("127.0.0.1");
    expect(s.channel.state).toBe("Closed");
    expect(s.rules.version).toBe(10);
    expect(s.rules.source).toBe("disk");
    expect(s.rules.stale).toBe(false);
    expect(s.pools!["github.com"].ips[0].state).toBe("Active");
    // W3a：接管态厚形态锁定（false 形态由 smoke 真 serve 断言）
    expect(s.takeover.on).toBe(true);
    expect(s.takeover.since).toBe("2026-09-13T11:30:00Z");
  });

  it("rules_snapshot.json 满足 RulesSnapshot", () => {
    const r: RulesSnapshot = goldenRules as RulesSnapshot;
    expect(r.version).toBe(10);
    expect(r.source).toBe("disk");
    expect(r.domains.length).toBeGreaterThan(0);
    expect(r.seed_ips["github.com"].length).toBeGreaterThan(0);
    expect(r.refresh.last_result).toBe("ok");
  });

  it("config.json 满足 ApiConfig", () => {
    const c: ApiConfig = goldenConfig as ApiConfig;
    expect(typeof c.cdn).toBe("string");
    expect(c.doctor_every_s).toBeGreaterThan(0);
    // P4/D6：持久化字段进契约
    expect(typeof c.rules_url).toBe("string");
    expect(c.rules_interval_s).toBeGreaterThan(0);
  });

  it("ConfigSetResp.requires_restart 类型成立", () => {
    const r: ConfigSetResp = { ...goldenConfig, requires_restart: ["listen"] } as ConfigSetResp;
    expect(r.requires_restart).toContain("listen");
  });

  it("doctor_summary.json 满足 DoctorSummaryResp", () => {
    const d: DoctorSummaryResp = goldenDoctorSummary as DoctorSummaryResp;
    expect(d.runs.length).toBeGreaterThan(0);
    expect(d.runs[0].scenario).toBe("clone");
  });

  it("doctor_run.json 满足 DoctorRunResp", () => {
    const d: DoctorRunResp = goldenDoctorRun as DoctorRunResp;
    expect(d.started).toBe(true);
  });

  it("get_progress.json 满足 GetProgressResp", () => {
    const g: GetProgressResp = goldenGetProgress as GetProgressResp;
    expect(g.active).toBeNull();
    expect(g.recent[0].done).toBe(true);
  });

  it("get_start.json 满足 GetStartResp", () => {
    const g: GetStartResp = goldenGetStart as GetStartResp;
    expect(g.task_id).toContain("get-");
  });

  it("mitm_status.json 满足 MitmStatus（W1 桩）", () => {
    const m: MitmStatus = goldenMitm as MitmStatus;
    expect(m.available).toBe(false);
  });

  it("sse_conn.json 满足 ConnFrame", () => {
    const f: ConnFrame = goldenConn as ConnFrame;
    expect(f.events.length).toBeGreaterThan(0);
    expect(typeof f.events[0].accel).toBe("boolean");
  });

  it("sse_doctor.json 满足 DoctorFrame", () => {
    const f: DoctorFrame = goldenDoctorFrame as DoctorFrame;
    expect(f.proxy_total).toBe(5);
  });

  it("sse_get.json 满足 GetTask", () => {
    const t: GetTask = goldenGetFrame as GetTask;
    expect(t.total_bytes).toBeGreaterThan(0);
  });

  it("sse_hello.json 满足 HelloFrame", () => {
    const h: HelloFrame = goldenHello as HelloFrame;
    expect(h.proto).toBe(1);
  });
});
