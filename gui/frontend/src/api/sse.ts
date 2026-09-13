// sse.ts —— /api/events 订阅（M3-W1）。
// EventSource 不支持自定义头 → token 走 query。断线重连由浏览器原生
// 提供（重放 query token）。

import { createSignal } from "solid-js";
import { daemonBase, apiToken, notifyUnauthorized } from "./client";
import type { ApiStatus, ConnEvent, DoctorFrame, GetTask, HelloFrame } from "./types";

export interface EventsState {
  hello: HelloFrame | null;
  status: ApiStatus | null;
  conns: ConnEvent[]; // 最近 200 条（新→旧）
  doctor: DoctorFrame | null; // 最近一次
  get: GetTask | null; // 最近一帧（进行中或终态）
  connected: boolean;
}

export function useEvents() {
  const [state, setState] = createSignal<EventsState>({
    hello: null,
    status: null,
    conns: [],
    doctor: null,
    get: null,
    connected: false,
  });

  let es: EventSource | null = null;
  const connect = () => {
    if (es) es.close();
    es = new EventSource(`${daemonBase()}/api/events?token=${encodeURIComponent(apiToken())}`);
    es.addEventListener("open", () => setState((s) => ({ ...s, connected: true })));
    es.addEventListener("error", () => setState((s) => ({ ...s, connected: false })));
    es.addEventListener("hello", (e) => {
      setState((s) => ({ ...s, hello: JSON.parse((e as MessageEvent).data), connected: true }));
    });
    es.addEventListener("status", (e) => {
      setState((s) => ({ ...s, status: JSON.parse((e as MessageEvent).data) }));
    });
    es.addEventListener("conn", (e) => {
      const f = JSON.parse((e as MessageEvent).data) as { events: ConnEvent[] };
      setState((s) => ({ ...s, conns: [...f.events].reverse().concat(s.conns).slice(0, 200) }));
    });
    es.addEventListener("doctor", (e) => {
      setState((s) => ({ ...s, doctor: JSON.parse((e as MessageEvent).data) }));
    });
    es.addEventListener("get", (e) => {
      setState((s) => ({ ...s, get: JSON.parse((e as MessageEvent).data) }));
    });
    es.addEventListener("401", () => notifyUnauthorized());
  };
  connect();

  const close = () => es?.close();
  return { state, close, reconnect: connect };
}
