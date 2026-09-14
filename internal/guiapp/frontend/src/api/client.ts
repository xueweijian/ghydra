// client.ts —— daemon 通信层（M3-W1）。
// 架构约定：GUI 只是 daemon（ghydra serve）的薄客户端，经 127.0.0.1
// HTTP 说 /api/*；写保护 = 本机 token（X-GHydra-Token）。
// token 来源：localStorage（浏览器兜底模式用户粘贴；壳模式 W4 改为
// 进程读文件注入后写入同一 key，消费端无感）。

export const DAEMON_BASE_KEY = "ghydra.daemonBase";
export const TOKEN_KEY = "ghydra.token";
export const DEFAULT_BASE = "http://127.0.0.1:9801";

export function daemonBase(): string {
  const saved = localStorage.getItem(DAEMON_BASE_KEY);
  if (saved) return saved;
  // 浏览器兜底模式：面板由 serve 托管 → daemon 天然同源
  if (typeof location !== "undefined" && location.protocol.startsWith("http")) {
    return location.origin;
  }
  return DEFAULT_BASE; // 壳模式（wails:// 等）：默认本机 daemon 端口
}

export function setDaemonBase(v: string) {
  localStorage.setItem(DAEMON_BASE_KEY, v.replace(/\/+$/, ""));
}

export function apiToken(): string {
  return localStorage.getItem(TOKEN_KEY) || "";
}

export function setApiToken(v: string) {
  localStorage.setItem(TOKEN_KEY, v.trim());
}

/** 401 时分发：App 层弹出 token 引导横幅。 */
export function notifyUnauthorized() {
  window.dispatchEvent(new CustomEvent("ghydra.unauthorized"));
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, msg: string) {
    super(msg);
    this.status = status;
  }
}

export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const r = await fetch(`${daemonBase()}${path}`, {
    ...init,
    headers: {
      Accept: "application/json",
      "X-GHydra-Token": apiToken(),
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      ...(init?.headers || {}),
    },
  });
  if (r.status === 401) notifyUnauthorized();
  const body = await r.json().catch(() => ({}));
  if (!r.ok) {
    throw new ApiError(r.status, (body as { error?: string }).error || `HTTP ${r.status}`);
  }
  return body as T;
}

// ---- 便捷封装 ----

export const getStatus = () => apiFetch<import("../types").ApiStatus>("/api/status");
export const getConfig = () => apiFetch<import("../types").ApiConfig>("/api/config");
export const setConfig = (patch: import("../types").ConfigPatch) =>
  apiFetch<import("../types").ConfigSetResp>("/api/config", {
    method: "POST",
    body: JSON.stringify(patch),
  });
export const setCDN = (cdn: string) =>
  apiFetch<import("../types").ApiConfig>("/api/config", {
    method: "POST",
    body: JSON.stringify({ cdn }),
  });

// P4/D7 自更新三件套（updateRunner nil = 503，卡片按不支持降级）。
export const updateCheck = () => apiFetch<import("../types").UpdateInfo>("/api/update/check");
export const updateApply = () =>
  apiFetch<import("../types").UpdateApplyResp>("/api/update/apply", { method: "POST" });
export const updateStatus = () => apiFetch<import("../types").UpdateStatus>("/api/update/status");
export const systemOn = (mode: "pac" | "proxy" = "pac") =>
  apiFetch<{ ok: boolean; mode: string }>("/api/on", { method: "POST", body: JSON.stringify({ mode }) });
export const systemOff = (shutdown = false) =>
  apiFetch<{ ok: boolean; shutdown: boolean }>("/api/off", { method: "POST", body: JSON.stringify({ shutdown }) });
export const doctorRun = () =>
  apiFetch<{ run_id: string; started: boolean }>("/api/doctor/run", { method: "POST", body: "{}" });
export const doctorSummary = (hours = 24) =>
  apiFetch<import("../types").DoctorSummaryResp>(`/api/doctor/summary?hours=${hours}`);
export const getProgress = () => apiFetch<import("../types").GetProgressResp>("/api/get/progress");
export const mitmStatus = () => apiFetch<import("../types").MitmStatus>("/api/mitm/status");
export const getRules = () => apiFetch<import("../types").RulesSnapshot>("/api/rules");
export const refreshRules = () =>
  apiFetch<{ refresh_id: string }>("/api/rules/refresh", { method: "POST" });

export function gitOp(action: "enable" | "disable" | "status", body?: object) {
  return apiFetch<Record<string, unknown>>(`/api/git/${action}`, {
    method: action === "status" ? "GET" : "POST",
    body: action === "status" ? undefined : JSON.stringify(body || {}),
  });
}

export const gitStatus = () => apiFetch<import("../types").GitStatusInfo>("/api/git/status");
export const sshStatus = () => apiFetch<import("../types").SSHStatusInfo>("/api/ssh/status");

export function sshOp(action: "enable" | "disable" | "status", body?: object) {
  return apiFetch<Record<string, unknown>>(`/api/ssh/${action}`, {
    method: action === "status" ? "GET" : "POST",
    body: action === "status" ? undefined : JSON.stringify(body || {}),
  });
}

export function startDownload(url: string, dst?: string) {
  return apiFetch<import("../types").GetStartResp>("/api/get/start", {
    method: "POST",
    body: JSON.stringify({ url, dst }),
  });
}
