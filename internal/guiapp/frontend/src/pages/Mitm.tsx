// Mitm.tsx —— MITM 页占位（v2.0+ 候选：2026-09-13 拍板移出 v1.0，见 M3-Plan D6；
// API 桩与 golden 契约保留，页面降级为计划说明）。
import { createResource, Show } from "solid-js";
import { mitmStatus } from "../api/client";

export default function Mitm() {
  const [st] = createResource(mitmStatus);
  return (
    <div class="page">
      <h2>增强模式（MITM）</h2>
      <div class="card">
        <p>
          <b>v2.0 计划中</b>
          <span class="muted">（2026-09-13 拍板移出 v1.0，接口桩已预留）</span>
        </p>
        <p class="muted">
          未来将提供：浏览器流量 B 通道兜底、googleapis 资源替换。当前版本请用{" "}
          <code>ghydra get</code> 命令下载 Release——已内置 A/B 通道自动切换与断点续传。
        </p>
      </div>
      <Show when={st()} fallback={null}>
        <p class="muted">引擎状态：{st()!.available ? "可用" : "未上线"}</p>
      </Show>
    </div>
  );
}
