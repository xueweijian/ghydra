// Status.tsx —— 状态页（W1 完整页）：SSE 实时驱动。
import { Show, For, createMemo } from "solid-js";
import type { EventsState } from "../api/sse";
import type { ConnEvent } from "../types";

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

  return (
    <div class="page">
      <Show when={st()} fallback={<p class="muted">等待 daemon 事件流…（检查连接与 token）</p>}>
        <div class="cards">
          <div class="card">
            <h3>监听</h3>
            <p class="mono">{st()!.listen}</p>
            <p class="muted">调度器 {st()!.scheduler ? "开" : "关"} · 运行 {Math.round(st()!.uptime_s)}s</p>
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
          </div>
          <div class="card">
            <h3>通道 B</h3>
            <p class="big">{st()!.channel.b_ok === null || st()!.channel.b_ok === undefined ? "未知" : st()!.channel.b_ok ? "正常" : "异常"}</p>
            <Show when={st()!.cdn}>
              <p class="muted mono">{st()!.cdn}</p>
            </Show>
          </div>
        </div>

        <Show when={ev().doctor}>
          <h3>最近体检</h3>
          <p class="mono">
            [{ev().doctor!.run_id}] verdict={ev().doctor!.verdict} 通道 {ev().doctor!.proxy_ok}/
            {ev().doctor!.proxy_total} · 直连 {ev().doctor!.direct_ok}/{ev().doctor!.proxy_total}
            <Show when={ev().doctor!.cdn_ok !== null && ev().doctor!.cdn_ok !== undefined}>
              {" "}
              · B {ev().doctor!.cdn_ok}/{ev().doctor!.proxy_total}
            </Show>
            <Show when={ev().doctor!.error}> · <span class="err">{ev().doctor!.error}</span></Show>
          </p>
        </Show>

        <h3>IP 池</h3>
        <For each={Object.entries(st()!.pools || {})}>
          {([host, pool]) => (
            <div class="card">
              <b>{host}</b> <span class="muted">粘性 {pool.sticky || "—"}</span>
              <table>
                <thead>
                  <tr>
                    <th>地址</th><th>状态</th><th>评分</th><th>RTT</th><th>失败率</th><th>样本</th>
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
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
            </div>
          )}
        </For>

        <h3>连接流（SSE 实时）</h3>
        <Show when={ev().conns.length === 0}>
          <p class="muted">暂无连接事件</p>
        </Show>
        <table>
          <tbody>
            <For each={ev().conns.slice(0, 30)}>
              {(c: ConnEvent) => (
                <tr>
                  <td class="mono">{new Date(c.ts).toLocaleTimeString()}</td>
                  <td class="mono">{c.host}</td>
                  <td>{c.accel ? "加速" : "直连"}</td>
                  <td style={{ color: c.ok ? "#22c55e" : "#ef4444" }}>{c.ok ? "OK" : c.err || "失败"}</td>
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
