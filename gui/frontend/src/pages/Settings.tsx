// Settings.tsx —— 设置页（W1：daemon 地址 / token / cdn）。
import { createSignal, onMount, Show } from "solid-js";
import {
  daemonBase, setDaemonBase, apiToken, setApiToken, setCDN, getConfig,
} from "../api/client";

export default function Settings() {
  const [base, setBase] = createSignal(daemonBase());
  const [tok, setTok] = createSignal(apiToken());
  const [cdn, setCdn] = createSignal("");
  const [msg, setMsg] = createSignal("");

  onMount(async () => {
    try {
      const c = await getConfig();
      setCdn(c.cdn);
    } catch {
      /* 未连接时不阻塞设置编辑 */
    }
  });

  const save = async () => {
    setDaemonBase(base());
    setApiToken(tok());
    setMsg("连接已保存。");
    if (cdn() !== "") {
      try {
        await setCDN(cdn());
        setMsg(`已保存；B 通道 CDN = ${cdn()}`);
      } catch (e) {
        setMsg(`CDN 保存失败：${e instanceof Error ? e.message : String(e)}`);
      }
    }
  };

  return (
    <div class="page">
      <h2>设置</h2>
      <label>Daemon 地址</label>
      <input type="text" value={base()} onInput={(e) => setBase(e.currentTarget.value)} />
      <label>API Token（终端运行 ghydra token 获取）</label>
      <input type="password" value={tok()} onInput={(e) => setTok(e.currentTarget.value)} />
      <label>B 通道 CDN 前缀（可留空）</label>
      <input type="text" value={cdn()} onInput={(e) => setCdn(e.currentTarget.value)} />
      <div class="row">
        <button onClick={save}>保存</button>
      </div>
      <Show when={msg()}>
        <p class="mono">{msg()}</p>
      </Show>
    </div>
  );
}
