// Rules.tsx —— 规则页（W3b）：W2 信任链数据的主消费阵地。
// 数据：GET /api/rules（快照全景）+ POST /api/rules/refresh（手动触发，
// 异步单飞）。SSE status 帧的 rules 摘要变化时 refetch（1s diff 驱动）。
import { createResource, createSignal, Show, For, onCleanup } from "solid-js";
import { getRules, refreshRules } from "../api/client";
import type { EventsState } from "../api/sse";
import { refreshResultText, sourceText, isStale } from "../lib/rulesText";

function fmtTime(s: string): string {
  if (!s) return "—";
  const t = Date.parse(s);
  if (Number.isNaN(t)) return s;
  return new Date(t).toLocaleString();
}

export default function Rules(props: { events: () => EventsState }) {
  const [rules, { refetch }] = createResource(getRules);
  const [busy, setBusy] = createSignal(false);
  const [note, setNote] = createSignal("");
  let pollTimer: ReturnType<typeof setInterval> | undefined;
  onCleanup(() => pollTimer && clearInterval(pollTimer));

  // SSE rules 摘要（version/source）变化 → 全景 refetch
  const summary = () => props.events().status?.rules;
  let lastKey = "";
  const unpark = setInterval(() => {
    const s = summary();
    const key = s ? `${s.version}|${s.source}` : "";
    if (key && key !== lastKey) {
      lastKey = key;
      if (!rules.loading) refetch();
    }
  }, 1000);
  onCleanup(() => clearInterval(unpark));

  const manualRefresh = async () => {
    setBusy(true);
    setNote("已触发刷新…");
    try {
      await refreshRules();
      // 异步触发：轮询直到 running:false（最多 ~30s）
      let tries = 0;
      pollTimer && clearInterval(pollTimer);
      pollTimer = setInterval(async () => {
        tries++;
        try {
          const r = await getRules();
          if (!r.refresh.running || tries >= 15) {
            clearInterval(pollTimer!);
            pollTimer = undefined;
            setBusy(false);
            setNote(refreshResultText(r.refresh.last_result));
            refetch();
          }
        } catch {
          /* 轮询失败继续等 */
        }
      }, 2000);
    } catch (e) {
      setBusy(false);
      const msg = e instanceof Error ? e.message : String(e);
      setNote(msg.includes("already in flight") ? "刷新进行中，请稍候" : `刷新失败：${msg}`);
    }
  };

  return (
    <div class="page">
      <h2>规则（签名信任链）</h2>
      <Show when={rules()} fallback={<p class="muted">加载中…（daemon 未连接时检查 token）</p>}>
        {(r) => (
          <>
            <div class="cards">
              <div class="card">
                <h3>当前版本</h3>
                <p class="big">
                  v{r().version}{" "}
                  <span class={"badge " + (r().source === "embedded" ? "b-warn" : "b-ok")}>
                    {sourceText(r().source)}
                  </span>{" "}
                  <Show when={isStale(r().expires_at) || r().stale}>
                    <span class="badge b-warn">已过期（仍生效）</span>
                  </Show>
                </p>
                <p class="muted">
                  生成 {fmtTime(r().generated_at)} · 有效期至 {fmtTime(r().expires_at)}
                </p>
                <div class="row">
                  <button disabled={busy()} onClick={manualRefresh}>
                    {busy() ? "刷新中…" : "手动刷新"}
                  </button>
                  <Show when={note()}>
                    <span class="muted">{note()}</span>
                  </Show>
                </div>
              </div>
              <div class="card">
                <h3>自动更新</h3>
                <Show
                  when={r().refresh.running}
                  fallback={
                    <p class="big">{refreshResultText(r().refresh.last_result)}</p>
                  }
                >
                  <p class="big">拉取中…</p>
                </Show>
                <p class="muted">
                  上次 {fmtTime(r().refresh.last_at)} · 下次 {fmtTime(r().refresh.next_at)}
                </p>
                <p class="muted">周期 6h；失败退避 1h→4h→24h（冻结期间旧规则继续服务）</p>
              </div>
            </div>

            <h3>加速域名（{r().domains.length}）</h3>
            <table>
              <tbody>
                <For each={r().domains}>
                  {(d) => (
                    <tr>
                      <td class="mono">{d}</td>
                      <td class="muted">{d.startsWith("*.") ? "子域通配" : "精确匹配"}</td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>

            <h3>B 通道端点（{r().cdn_endpoints.length}）</h3>
            <For each={r().cdn_endpoints}>
              {(c) => (
                <p class="mono" style={{ margin: "2px 0" }}>
                  {c}
                </p>
              )}
            </For>

            <details>
              <summary class="muted">种子 IP 表（{Object.keys(r().seed_ips).length} 域）</summary>
              <For each={Object.entries(r().seed_ips)}>
                {([host, ips]: [string, string[]]) => (
                  <p class="mono" style={{ margin: "4px 0" }}>
                    {host}: {ips.length} 个
                  </p>
                )}
              </For>
            </details>
          </>
        )}
      </Show>
    </div>
  );
}
