// sseStale.test.ts —— F12：断线必须作废陈旧 status 帧（僵尸横幅根因）。
//
// 实证（2026-09-15 GUI 第二轮）：CLI off 杀 serve 后，SSE error 只翻
// connected 标志、不清 status——加速页残留「已接管」「接管系统代理 ✓」。
// 契约：error → connected=false 且 status=null（重连后新帧自然恢复）。

import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { useEvents } from "../api/sse";
import type { ApiStatus } from "../types";

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  listeners = new Map<string, Array<(e: unknown) => void>>();
  url = "";

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, fn: (e: unknown) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) || []), fn]);
  }

  emit(type: string, payload?: unknown) {
    for (const fn of this.listeners.get(type) || []) {
      fn(payload ?? { data: "{}" });
    }
  }

  close() {}
}

describe("useEvents 断线语义（F12）", () => {
  beforeEach(() => {
    FakeEventSource.instances = [];
    (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
    // node 环境无 localStorage（client.ts daemonBase/apiToken 读取）
    const store = new Map<string, string>();
    (globalThis as { localStorage?: unknown }).localStorage = {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => store.set(k, v),
      removeItem: (k: string) => store.delete(k),
    };
  });
  afterEach(() => {
    delete (globalThis as { EventSource?: unknown }).EventSource;
    delete (globalThis as { localStorage?: unknown }).localStorage;
  });

  it("error 事件清空陈旧 status（僵尸「已接管」横幅根因）", () => {
    const { state } = useEvents();
    const es = FakeEventSource.instances[0];

    // 已连接 + 接管中的 status 帧
    es.emit("open");
    es.emit("status", { data: JSON.stringify({ takeover: { on: true } } as Partial<ApiStatus>) });
    expect(state().connected).toBe(true);
    expect(state().status?.takeover?.on).toBe(true);

    // daemon 被 off 杀死：SSE error → 连接断 + 陈旧帧作废
    es.emit("error");
    expect(state().connected).toBe(false);
    expect(state().status).toBeNull();
  });

  it("重连后新 status 帧恢复", () => {
    const { state } = useEvents();
    const es = FakeEventSource.instances[0];
    es.emit("error");
    es.emit("open");
    es.emit("status", { data: JSON.stringify({ takeover: { on: false } } as Partial<ApiStatus>) });
    expect(state().connected).toBe(true);
    expect(state().status?.takeover?.on).toBe(false);
  });
});
