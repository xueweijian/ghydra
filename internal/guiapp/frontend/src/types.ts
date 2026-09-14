// types.ts —— /api/* 契约（与 engine/api/testdata/golden/*.json 逐一
// 对齐；__tests__/golden.test.ts 在 CI 双端锁定，改形状必须同步改）。

export interface IPEntry {
  addr: string;
  state: string;
  score: number;
  rtt_ms: number;
  fail_rate: number;
  samples: number;
  cooldown_count: number;
}

export interface PoolSnapshot {
  sticky: string;
  ips: IPEntry[];
}

export interface ChannelSnapshot {
  state: string;
  override?: string;
  fail_rate: number;
  samples: number;
  trip_at?: string;
  trip_why?: string;
  open_until?: string;
  backoff_mult: number;
  b_ok?: boolean | null;
  b_at?: string;
  b_suppressed: number;
  b_supp_why?: string;
}

export interface ApiStatus {
  api_version: number;
  version: string;
  listen: string;
  scheduler: boolean;
  conns: number;
  uptime_s: number;
  pools?: Record<string, PoolSnapshot>;
  channel: ChannelSnapshot;
  cdn?: string;
  rules: RulesStatus;
  takeover: TakeoverState;
}

// W3c：git/ssh 集成状态（GET /api/git|ssh/status）。
export interface GitKeyInfo {
  name: string;
  value: string;
  managed: boolean;
}

export interface GitStatusInfo {
  keys: GitKeyInfo[];
  snapshot: boolean; // 存在可还原快照
}

export interface SSHStatusInfo {
  enabled: boolean;
  alias: boolean;
  config_path: string;
}

// W3a：系统代理接管态（Boost 页大开关数据源；SSE status 帧同形状）。
export interface TakeoverState {
  on: boolean;
  since: string; // RFC3339 UTC，on=false 时空
}

// M3-W2 规则热更新（信任地板快照）
export interface RulesStatus {
  version: number;
  source: "embedded" | "disk" | "remote";
  stale: boolean;
}

export interface RulesSnapshot {
  version: number;
  source: "embedded" | "disk" | "remote";
  stale: boolean;
  generated_at: string;
  expires_at: string;
  domains: string[];
  cdn_endpoints: string[];
  seed_ips: Record<string, string[]>;
  refresh: RulesRefreshState;
}

export interface RulesRefreshState {
  last_result: string;
  last_at: string;
  next_at: string;
  running: boolean;
}

export interface ApiConfig {
  listen: string;
  scheduler: boolean;
  cdn: string;
  doctor_every_s: number;
  doctor_repo: string;
  managed: boolean;
  gui_dist?: string;
}

export interface DoctorSummaryRow {
  mode: string;
  scenario: string;
  checks: number;
  passed: number;
  reachable: number;
  avg_duration_ms: number;
  avg_ttfb_ms: number;
  first_at: string;
  last_at: string;
}

export interface DoctorSummaryResp {
  runs: DoctorSummaryRow[];
}

export interface DoctorRunResp {
  run_id: string;
  started: boolean;
}

export interface GetTask {
  task_id: string;
  url: string;
  dst: string;
  got_bytes: number;
  total_bytes: number;
  rate_bps: number;
  channel: string;
  done: boolean;
  error?: string;
  started_at: string;
  ended_at?: string;
}

export interface GetStartResp {
  task_id: string;
  dst: string;
}

export interface GetProgressResp {
  active: GetTask | null;
  recent: GetTask[];
}

export interface MitmStatus {
  available: boolean;
  enabled: boolean;
  reason?: string;
}

// ---- SSE 帧 ----

export interface HelloFrame {
  proto: number;
  heartbeat_s: number;
  api_version: number;
}

export interface ConnEvent {
  host: string;
  target: string;
  accel: boolean;
  ok: boolean;
  dial_ms: number;
  rx: number;
  tx: number;
  dur_ms: number;
  err?: string;
  ts: string;
}

export interface ConnFrame {
  events: ConnEvent[];
}

export interface DoctorFrame {
  run_id: string;
  verdict: string;
  proxy_ok: number;
  proxy_total: number;
  direct_ok: number;
  cdn_ok?: number | null;
  error?: string;
  finished_at: string;
}
