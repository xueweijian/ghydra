import { createSignal, onCleanup, onMount } from "solid-js";
import { fetchStatus, daemonBase, setDaemonBase, type ServeStatus } from "./api";

export default function App() {
  const [status, setStatus] = createSignal<ServeStatus | null>(null);
  const [err, setErr] = createSignal<string | null>(null);
  const [base, setBase] = createSignal(daemonBase());

  let timer: number | undefined;

  const poll = async () => {
    try {
      const s = await fetchStatus();
      setStatus(s);
      setErr(null);
    } catch (e) {
      setStatus(null);
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  onMount(() => {
    poll();
    timer = setInterval(poll, 2000);
  });
  onCleanup(() => clearInterval(timer));

  const applyBase = (v: string) => {
    setDaemonBase(v);
    setBase(daemonBase());
    poll();
  };

  return (
    <div style={{ "max-width": "860px", margin: "0 auto", padding: "24px" }}>
      <header
        style={{
          display: "flex",
          "align-items": "center",
          gap: "12px",
          "margin-bottom": "20px",
        }}
      >
        <div
          style={{
            width: "36px",
            height: "36px",
            "border-radius": "9px",
            background: "#10b981",
            display: "grid",
            "place-items": "center",
            "font-weight": "800",
            color: "#0f172a",
          }}
        >
          九
        </div>
        <div>
          <h1 style={{ margin: 0, "font-size": "20px" }}>GHydra</h1>
          <div style={{ "font-size": "12px", color: "#64748b" }}>
            GitHub 全链路加速器 · GUI spike（Wails v3）
          </div>
        </div>
      </header>

      <input
        value={base()}
        onChange={(e) => applyBase(e.currentTarget.value)}
        placeholder="daemon 地址"
        title="daemon 地址（回车生效）"
        style={{
          width: "100%",
          "box-sizing": "border-box",
          padding: "9px 12px",
          "border-radius": "8px",
          border: "1px solid #334155",
          background: "#1e293b",
          color: "#e2e8f0",
          "margin-bottom": "16px",
        }}
      />

      {err() && (
        <div
          style={{
            padding: "14px 16px",
            "border-radius": "8px",
            background: "#451a03",
            border: "1px solid #b45309",
            "margin-bottom": "16px",
          }}
        >
          <b>daemon 未连接</b>（{err()}）
          <div style={{ "font-size": "13px", color: "#fcd34d", "margin-top": "4px" }}>
            请先运行 <code>ghydra on</code> 或 <code>ghydra serve</code>。
          </div>
        </div>
      )}

      {status() && (
        <div
          style={{
            padding: "16px",
            "border-radius": "8px",
            background: "#1e293b",
            border: "1px solid #334155",
          }}
        >
          <div style={{ display: "flex", gap: "20px", "flex-wrap": "wrap" }}>
            <Stat label="监听" value={status()!.listen} />
            <Stat label="活跃连接" value={String(status()!.conns)} />
            <Stat
              label="运行时长"
              value={`${Math.floor(status()!.uptime_s / 60)}m ${Math.floor(status()!.uptime_s % 60)}s`}
            />
            <Stat label="调度器" value={status()!.scheduler ? "开" : "关"} />
            {status()!.cdn && <Stat label="CDN" value={status()!.cdn!} />}
          </div>
          <div style={{ "margin-top": "12px", "font-size": "12px", color: "#64748b" }}>
            W2-W4 将在此呈现：doctor 双列体检 / 通道熔断实时 / get 下载轨迹 / MITM 状态页。
          </div>
        </div>
      )}
    </div>
  );
}

function Stat(props: { label: string; value: string }) {
  return (
    <div>
      <div style={{ "font-size": "11px", color: "#64748b", "text-transform": "uppercase" }}>
        {props.label}
      </div>
      <div style={{ "font-size": "15px", "font-weight": "600" }}>{props.value}</div>
    </div>
  );
}
