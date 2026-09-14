// updateState.test.ts —— 更新卡片状态机表驱动（P4c L1）。
import { describe, expect, it } from "vitest";
import { updateCardState, type UpdateCardInput } from "../lib/updateState";
import type { UpdateInfo, UpdateStatus } from "../types";

const base: UpdateCardInput = {
  supported: true,
  connected: true,
  currentVersion: "1.0.0",
  info: null,
  status: null,
  checking: false,
  versionAtApply: null,
};

const updInfo = (over: Partial<UpdateInfo> = {}): UpdateInfo => ({
  current: "1.0.0",
  has_update: true,
  latest: "1.0.1",
  checked_at: "2026-09-14T00:00:00Z",
  cached: false,
  ...over,
});

const updStatus = (over: Partial<UpdateStatus> = {}): UpdateStatus => ({
  state: "idle",
  current: "1.0.0",
  updated_at: "2026-09-14T00:00:00Z",
  ...over,
});

describe("updateCardState", () => {
  it("unsupported 优先于一切", () => {
    expect(updateCardState({ ...base, supported: false }).kind).toBe("unsupported");
    expect(
      updateCardState({ ...base, supported: false, status: updStatus({ state: "failed" }) }).kind,
    ).toBe("unsupported");
  });

  it("安装跟踪：断连 = restarting（不依赖 pending_boot 帧——两段式 <1s 退出）", () => {
    const st = updateCardState({ ...base, versionAtApply: "1.0.0", connected: false });
    expect(st.kind).toBe("restarting");
  });

  it("安装跟踪：重连且版本变化 = done（needReenable：更新后加速默认关）", () => {
    const st = updateCardState({
      ...base,
      versionAtApply: "1.0.0",
      currentVersion: "1.0.1",
    });
    expect(st).toMatchObject({ kind: "done", version: "1.0.1", needReenable: true });
  });

  it("安装跟踪：重连但版本未变（回滚场景）不误报 done，继续看状态机", () => {
    const st = updateCardState({
      ...base,
      versionAtApply: "1.0.0",
      status: updStatus({ state: "failed", error: "swap 回滚" }),
    });
    expect(st).toMatchObject({ kind: "error", message: "swap 回滚" });
  });

  it.each(["checking", "downloading", "verifying", "swapping", "pending_boot"] as const)(
    "apply 态 %s → applying（进度透传）",
    (s) => {
      const st = updateCardState({
        ...base,
        status: updStatus({ state: s, progress_pct: s === "downloading" ? 42.5 : 0 }),
      });
      expect(st).toMatchObject({ kind: "applying", state: s });
    },
  );

  it("downloading 进度百分比透传", () => {
    const st = updateCardState({ ...base, status: updStatus({ state: "downloading", progress_pct: 42.5 }) });
    expect(st).toMatchObject({ kind: "applying", pct: 42.5 });
  });

  it("failed → error（SSE 帧）", () => {
    const st = updateCardState({ ...base, status: updStatus({ state: "failed", error: "checksum mismatch" }) });
    expect(st).toMatchObject({ kind: "error", message: "checksum mismatch" });
  });

  it("checking（本地请求在途）", () => {
    expect(updateCardState({ ...base, checking: true }).kind).toBe("checking");
  });

  it("check 结果：可更新 → available；已最新 → uptodate；错误 → error", () => {
    expect(updateCardState({ ...base, info: updInfo() })).toMatchObject({
      kind: "available",
      info: { latest: "1.0.1" },
    });
    expect(
      updateCardState({ ...base, info: updInfo({ has_update: false, latest: undefined }) }).kind,
    ).toBe("uptodate");
    expect(updateCardState({ ...base, info: updInfo({ error: "rate limited" }) })).toMatchObject({
      kind: "error",
      message: "rate limited",
    });
  });

  it("空输入 = idle", () => {
    expect(updateCardState(base).kind).toBe("idle");
  });

  it("优先级：断连 restarting 压过 apply 进度帧", () => {
    const st = updateCardState({
      ...base,
      versionAtApply: "1.0.0",
      connected: false,
      status: updStatus({ state: "downloading", progress_pct: 10 }),
    });
    expect(st.kind).toBe("restarting");
  });
});
