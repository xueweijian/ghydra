// Downloads.tsx —— 下载页（W3d 收尾）：错误分类 + 空态教学（D6 浏览器
// 下载缺口的缓解面）+ recent 表（channel 徽章/rate/耗时）。
import { createSignal, Show, For, onMount } from "solid-js";
import { startDownload, getProgress } from "../api/client";
import type { ApiError } from "../api/client";
import type { EventsState } from "../api/sse";
import type { GetTask } from "../types";

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1073741824) return `${(n / 1048576).toFixed(1)} MB`;
  return `${(n / 1073741824).toFixed(2)} GB`;
}

function errKind(e: unknown): string {
  const ae = e as ApiError;
  if (ae && ae.status) {
    if (ae.status === 401) return "token";
    if (ae.status === 400 || ae.status === 404) return "url";
    if (ae.status >= 500) return "daemon";
  }
  return "network";
}

function durOf(t: GetTask): string {
  if (!t.ended_at) return "—";
  const ms = Date.parse(t.ended_at) - Date.parse(t.started_at);
  return ms > 0 ? `${(ms / 1000).toFixed(1)}s` : "—";
}

export default function Downloads(props: { events: () => EventsState }) {
  const [url, setUrl] = createSignal("");
  const [recent, setRecent] = createSignal<GetTask[]>([]);
  const [err, setErr] = createSignal("");
  const [kind, setKind] = createSignal("");

  const refresh = () => getProgress().then((p) => setRecent(p.recent)).catch(() => {});
  onMount(refresh);

  const start = async () => {
    setErr("");
    setKind("");
    try {
      await startDownload(url());
      setUrl("");
    } catch (e) {
      setKind(errKind(e));
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  const active = () => (props.events().get && !props.events().get!.done ? props.events().get : null);
  const pct = (t: GetTask) =>
    t.total_bytes > 0 ? Math.min(100, Math.round((t.got_bytes / t.total_bytes) * 100)) : null;

  const hasAny = () => active() || recent().length > 0;

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
        <p class="err">
          {kind() === "token" && "API token 无效——设置页更新 token。"}
          {kind() === "url" && "链接不合法或不受支持（目前支持 github Release/archive 链接）。"}
          {kind() === "daemon" && "daemon 处理失败："}
          {kind() === "network" && "无法连接 daemon（检查设置页地址）："}
          {kind() !== "daemon" && kind() !== "network" ? "" : ` ${err()}`}
        </p>
      </Show>

      <Show when={active()}>
        {(t) => (
          <div class="card">
            <b>{t().task_id}</b>{" "}
            <span class={"badge " + (t().channel === "B" ? "b-b" : "b-ok")}>通道 {t().channel}</span>
            <div class="bar">
              <div class="bar-fill" style={{ width: `${pct(t()) ?? 0}%` }} />
            </div>
            <p class="muted">
              {fmtBytes(t().got_bytes)}/{t().total_bytes > 0 ? fmtBytes(t().total_bytes) : "?"} ·{" "}
              {(t().rate_bps / 1048576).toFixed(2)} MB/s
            </p>
          </div>
        )}
      </Show>

      <Show when={!hasAny()}>
        <div class="card">
          <h3>浏览器下载 GitHub 慢/失败？</h3>
          <p class="muted">
            在 Release 页面右键复制下载链接，粘贴到上方——GHydra 自动选 A（直连 IP 择优）或
            B（CDN 镜像）通道，支持断点续传与完整性校验。
          </p>
          <p class="muted">
            终端等价命令：<code>ghydra get &lt;url&gt; [-o 输出文件]</code>
          </p>
        </div>
      </Show>

      <h3>最近任务</h3>
      <Show when={recent().length > 0} fallback={<p class="muted">暂无历史</p>}>
        <table>
          <thead>
            <tr>
              <th>文件</th><th>通道</th><th>大小</th><th>速度</th><th>耗时</th><th>状态</th>
            </tr>
          </thead>
          <tbody>
            <For each={recent()}>
              {(t) => (
                <tr>
                  <td class="mono" title={t.url}>{t.dst.split("/").pop() || t.url}</td>
                  <td>
                    <span class={"badge " + (t.channel === "B" ? "b-b" : "b-ok")}>{t.channel}</span>
                  </td>
                  <td>{fmtBytes(t.total_bytes || t.got_bytes)}</td>
                  <td>{t.rate_bps > 0 ? `${(t.rate_bps / 1048576).toFixed(2)} MB/s` : "—"}</td>
                  <td>{durOf(t)}</td>
                  <td style={{ color: t.error ? "#ef4444" : t.done ? "#22c55e" : "#94a3b8" }}>
                    {t.error ? t.error : t.done ? "完成" : "进行中"}
                  </td>
                </tr>
              )}
            </For>
          </tbody>
        </table>
      </Show>
    </div>
  );
}
