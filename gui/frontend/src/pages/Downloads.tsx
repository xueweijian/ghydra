// Downloads.tsx —— 下载页骨架（W1：start + SSE 进度 + 历史列表）。
import { createSignal, Show, For } from "solid-js";
import { startDownload, getProgress } from "../api/client";
import type { EventsState } from "../api/sse";
import { onMount } from "solid-js";
import type { GetTask } from "../types";

export default function Downloads(props: { events: () => EventsState }) {
  const [url, setUrl] = createSignal("");
  const [recent, setRecent] = createSignal<GetTask[]>([]);
  const [err, setErr] = createSignal("");

  const refresh = () => getProgress().then((p) => setRecent(p.recent)).catch(() => {});
  onMount(refresh);

  const start = async () => {
    setErr("");
    try {
      await startDownload(url());
      setUrl("");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  const active = () => props.events().get && !props.events().get!.done ? props.events().get : null;
  const pct = (t: GetTask) =>
    t.total_bytes > 0 ? Math.min(100, Math.round((t.got_bytes / t.total_bytes) * 100)) : null;

  return (
    <div class="page">
      <h2>新建下载（A/B 择路 + 断点续传）</h2>
      <div class="row">
        <input
          type="text"
          placeholder="https://github.com/owner/repo/releases/download/…"
          value={url()}
          onInput={(e) => setUrl(e.currentTarget.value)}
          style={{ flex: "1" }}
        />
        <button onClick={start}>开始</button>
      </div>
      <Show when={err()}>
        <p class="err">{err()}</p>
      </Show>

      <Show when={active()}>
        {(t) => (
          <div class="card">
            <b>{t().task_id}</b> <span class="mono">{t().channel}</span>
            <div class="bar">
              <div class="bar-fill" style={{ width: `${pct(t()) ?? 0}%` }} />
            </div>
            <p class="muted">
              {t().got_bytes}/{t().total_bytes > 0 ? t().total_bytes : "?"} 字节 ·{" "}
              {(t().rate_bps / 1048576).toFixed(2)} MB/s
            </p>
          </div>
        )}
      </Show>

      <h3>历史</h3>
      <For each={recent()}>
        {(t) => (
          <p class="mono">
            [{t.task_id}] {t.done ? (t.error ? `✗ ${t.error}` : "✓") : "…"} {t.url} → {t.dst}（{t.channel || "-"}）
          </p>
        )}
      </For>
    </div>
  );
}
