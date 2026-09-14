// Status.tsx —— 状态仪表盘（W3c 深化）：SSE 实时驱动 + doctor 24h 表 +
// 熔断详情 + 连接流过滤。约定（W3-Design §7-R4）：帧有的用帧。
import { Show, For, createMemo, createSignal, onMount } from "solid-js";
import type { EventsState } from "../api/sse";
import type { ConnEvent, DoctorSummaryRow } from "../types";
import { doctorSummary, doctorRun } from "../api/client";

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1073741824) return `${(n / 1048576).toFixed(1)} MB`;
  return `${(n / 1073741824).toFixed(2)} GB`;
}

const stateColor: Record<string, string> = {
  Closed: "#22c55e",
  Open: "#ef4444",
  HalfOpen: "#f59e0b",
};

export default function Status(props: { events: () => EventsState }) {
  const ev = props.events;
  const st = createMemo(() => ev().status);

  const [rows, setRows] = createSignal<DoctorSummaryRow[]>([]);
  const [docBusy, setDocBusy] = createSignal(false);
  const [host, setHost] = createSignal("");
  const [onlyAccel, setOnlyAccel] = createSignal(false);

  const loadSummary = () => doctorSummary(24).then((r) => setRows(r.runs)).catch(() => {});
  onMount(loadSummary);
  // doctor SSE 帧到达（新体检完成）→ 刷新 24h 表
  let lastRun = "";
  const t = setInterval(() => {
    const d = ev().doctor;
    if (d && d.run_id !== lastRun) {
      lastRun = d.run_id;
      loadSummary();
    }
  }, 1000);

  const runDoctor = async () => {
    setDocBusy(true);
    try {
      await doctorRun();
    } catch {
      /* busy/未装配静默——SSE 帧到达即更新 */
    } finally {
      setDocBusy(false);
    }
  };

  const connsFiltered = createMemo(() => {
    let list = ev().conns;
    if (host()) {
      const h = host().toLowerCase();
      list = list.filter((c) => c.host.toLowerCase().includes(h));
    }
    if (onlyAccel()) list = list.filter((c) => c.accel);
    return list;
  });

  const openUntil = () => {
    const ou = st()?.channel.open_until;
    if (!ou) return "";
    const ms = Date.parse(ou) - Date.now();
    return ms > 0 ? `恢复探针 ~${Math.ceil(ms / 1000)}s` : "即将重试";
  };

  return (
    <div class="page">
      <Show when={st()} fallback={<p class="muted">等待 daemon 事件流…（检查连接与 token）</p>}>
        <div class="cards">
          <div class="card">
            <h3>监听</h3>
            <p class="mono">{st()!.listen}</p>
            <p class="muted">
              v{st()!.version === "" ? "?" : st()!.version} · 调度器 {st()!.scheduler ? "开" : "关"} · 运行{" "}
              {Math.round(st()!.uptime_s)}s
            </p>
          </div>
          <div class="card">
            <h3>连接</h3>
            <p class="big">{st()!.conns}</p>
            <p class="muted">当前并发隧道</p>
          </div>
          <div class="card">
            <h3>通道 A</h3>
            <p class="big" style={{ color: stateColor[st()!.channel.state] || "#e2e8f0" }}>
              {st()!.channel.state}
            </p>
            <Show when={st()!.channel.trip_why}>
              <p class="muted">熔断原因：{st()!.channel.trip_why}</p>
            </Show>
            <Show when={st()!.channel.state === "Open"}>
              <p class="muted">
                退避 ×{st()!.channel.backoff_mult} · {openUntil()}
              </p>
            </Show>
          </div>
          <div class="card">
            <h3>通道 B</h3>
            <p class="big">
              {st()!.channel.b_ok === null || st()!.channel.b_ok === undefined
                ? "未知"
                : st()!.channel.b_ok
                  ? "正常"
                  : "异常"}
            </p>
            <Show when={st()!.channel.b_suppressed > 0}>
              <p class="muted">
                B 抑制 {st()!.channel.b_suppressed} 次（{st()!.channel.b_supp_why || "—"}）
              </p>
            </Show>
            <Show when={st()!.cdn}>
              <p class="muted mono">{st()!.cdn}</p>
            </Show>
          </div>
        </div>

        <h3>最近体检</h3>
        <div class="row">
          <button disabled={docBusy()} onClick={runDoctor}>
            {docBusy() ? "体检中…" : "立即体检"}
          </button>
          <Show when={ev().doctor}>
            <span class="muted">
              [{ev().doctor!.run_id}] verdict={ev().doctor!.verdict} · 通道 {ev().doctor!.proxy_ok}/
              {ev().doctor!.proxy_total} · 直连 {ev().doctor!.direct_ok}/{ev().doctor!.proxy_total}
            </span>
          </Show>
        </div>
        <Show when={rows().length > 0} fallback={<p class="muted">24h 内暂无体检记录</p>}>
          <table>
            <thead>
              <tr>
                <th>模式</th><th>场景</th><th>通过</th><th>可达</th><th>TTFB</th><th>耗时</th><th>最近</th>
              </tr>
            </thead>
            <tbody>
              <For each={rows()}>
                {(r) => (
                  <tr>
                    <td>{r.mode}</td>
                    <td>{r.scenario}</td>
                    <td>
                      {r.passed}/{r.checks}
                    </td>
                    <td>{r.reachable}</td>
                    <td>{r.avg_ttfb_ms.toFixed(0)}ms</td>
                    <td>{r.avg_duration_ms.toFixed(0)}ms</td>
                    <td class="muted">{new Date(r.last_at).toLocaleTimeString()}</td>
                  </tr>
                )}
              </For>
            </tbody>
          </table>
        </Show>

        <h3>IP 池</h3>
        <Show
          when={Object.keys(st()!.pools || {}).length > 0}
          fallback={
            <p class="muted">
              调度器未运行或暂无池数据（serve --scheduler=false 时直连模式，无 IP 择优）
            </p>
          }
        >
          <For each={Object.entries(st()!.pools || {})}>
            {([host, pool]) => (
              <div class="card">
                <b>{host}</b> <span class="muted">粘性 {pool.sticky || "—"}</span>
                <table>
                  <thead>
                    <tr>
                      <th>地址</th><th>状态</th><th>评分</th><th>RTT</th><th>失败率</th><th>样本</th><th>熔断</th>
                    </tr>
                  </thead>
                  <tbody>
                    <For each={pool.ips}>
                      {(ip) => (
                        <tr>
                          <td class="mono">{ip.addr}</td>
                          <td>{ip.state}</td>
                          <td>{ip.score.toFixed(3)}</td>
                          <td>{ip.rtt_ms.toFixed(0)}ms</td>
                          <td>{(ip.fail_rate * 100).toFixed(0)}%</td>
                          <td>{ip.samples}</td>
                          <td>{ip.cooldown_count}</td>
                        </tr>
                      )}
                    </For>
                  </tbody>
                </table>
              </div>
            )}
          </For>
        </Show>

        <h3>连接流（SSE 实时）</h3>
        <div class="row">
          <input
            type="text"
            placeholder="按 host 过滤…"
            value={host()}
            onInput={(e) => setHost(e.currentTarget.value)}
            style={{ width: "220px" }}
          />
          <label style={{ display: "flex", gap: "4px", "align-items": "center", margin: 0 }}>
            <input type="checkbox" checked={onlyAccel()} onChange={(e) => setOnlyAccel(e.currentTarget.checked)} />
            仅加速
          </label>
        </div>
        <Show when={connsFiltered().length === 0}>
          <p class="muted">{ev().conns.length === 0 ? "暂无连接事件" : "无匹配连接"}</p>
        </Show>
        <table>
          <tbody>
            <For each={connsFiltered().slice(0, 30)}>
              {(c: ConnEvent) => (
                <tr>
                  <td class="mono">{new Date(c.ts).toLocaleTimeString()}</td>
                  <td class="mono">{c.host}</td>
                  <td>{c.accel ? "加速" : "直连"}</td>
                  <td style={{ color: c.ok ? "#22c55e" : "#ef4444" }}>{c.ok ? "OK" : c.err || "失败"}</td>
                  <td>{c.dial_ms.toFixed(0)}ms</td>
                  <td>↓{fmtBytes(c.rx)}</td>
                  <td>↑{fmtBytes(c.tx)}</td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </Show>
    </div>
  );
}
