// Mitm.tsx —— MITM 页占位（W1 桩消费；引擎 W2 上线，页面 W4 完整）。
import { createResource, Show } from "solid-js";
import { mitmStatus } from "../api/client";

export default function Mitm() {
  const [st] = createResource(mitmStatus);
  return (
    <div class="page">
      <h2>增强模式（MITM）</h2>
      <Show when={st()} fallback={<p class="muted">读取中…</p>}>
        <div class="card">
          <p>
            引擎状态：<b>{st()!.available ? "可用" : "未上线"}</b>
            <Show when={st()!.reason}>
              <span class="muted">（{st()!.reason}）</span>
            </Show>
          </p>
          <p class="muted">
            该模式将提供：浏览器流量 B 通道兜底、googleapis 资源替换。默认关闭，开启需
            二次确认并安装本机 CA——安全边界见 M3-Plan D6。
          </p>
        </div>
      </Show>
    </div>
  );
}
