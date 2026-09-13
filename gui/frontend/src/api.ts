// daemon 通信层（M3-W0 spike 最小面）。
// 架构约定：GUI 只经 127.0.0.1 HTTP 与 daemon 说话，W3-W1 升级为
// /api/* + SSE + 本机 token；当前先用 serve 已有的 /status。
export const DAEMON_BASE_KEY = "ghydra.daemonBase";
export const DEFAULT_BASE = "http://127.0.0.1:9801";

export function daemonBase(): string {
  return localStorage.getItem(DAEMON_BASE_KEY) || DEFAULT_BASE;
}

export function setDaemonBase(v: string) {
  localStorage.setItem(DAEMON_BASE_KEY, v.replace(/\/+$/, ""));
}

export interface ServeStatus {
  listen: string;
  scheduler: boolean;
  conns: number;
  uptime_s: number;
  channel?: Record<string, unknown>;
  cdn?: string;
}

export async function fetchStatus(
  signal?: AbortSignal,
): Promise<ServeStatus> {
  const r = await fetch(`${daemonBase()}/status`, {
    signal,
    headers: { Accept: "application/json" },
  });
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}
