// Settings.tsx —— 设置页（W3d 收尾）：连接配置 + 测试连接 + 只读运行参数 + 关于。
import { createSignal, onMount, Show } from "solid-js";
import {
  daemonBase, setDaemonBase, apiToken, setApiToken, setCDN, getConfig, getStatus,
} from "../api/client";
import { useEvents } from "../api/sse";

export default function Settings() {
  const [base, setBase] = createSignal(daemonBase());
  const [tok, setTok] = createSignal(apiToken());
  const [cdn, setCdn] = createSignal("");
  const [msg, setMsg] = createSignal("");
  const [test, setTest] = createSignal("");

  const [doctorEvery, setDoctorEvery] = createSignal(0);
  const [doctorRepo, setDoctorRepo] = createSignal("");
  const events = useEvents();

  onMount(async () => {
    try {
      const c = await getConfig();
      setCdn(c.cdn);
      setDoctorEvery(c.doctor_every_s);
      setDoctorRepo(c.doctor_repo);
    } catch {
      /* 未连接时不阻塞设置编辑 */
    }
  });

  const save = async () => {
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

  const testConn = async () => {
    setTest("测试中…");
    const t0 = performance.now();
    try {
      const s = await getStatus();
      const ms = Math.round(performance.now() - t0);
      setTest(`✓ 已连接 v${s.version} · ${ms}ms · 调度器${s.scheduler ? "开" : "关"}`);
    } catch (e) {
      setTest(`✗ ${e instanceof Error ? e.message : String(e)}`);
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
        <button onClick={testConn}>测试连接</button>
        <Show when={test()}>
            <span class={test().startsWith("✓") ? "muted" : "err"}>{test()}</span>
        </Show>
      </div>
      <Show when={msg()}>
        <p class="mono">{msg()}</p>
      </Show>

      <h3>运行参数（只读）</h3>
      <div class="card">
        <p class="mono">
          体检周期 {doctorEvery() > 0 ? `${Math.round(doctorEvery() / 60)} 分钟` : "—"} · 体检仓库{" "}
          {doctorRepo() || "—"}
        </p>
        <p class="muted">启动参数（serve 旗标），修改需重启 daemon：--doctor-every / --doctor-repo / --scheduler</p>
      </div>

      <h3>关于</h3>
      <div class="card">
        <p>
          GHydra{" "}
          <Show when={events.state().status} fallback={<span class="muted">（未连接）</span>}>
            <span class="mono">v{events.state().status!.version}</span>
          </Show>{" "}
          · <a href="https://github.com/xueweijian/ghydra" target="_blank" rel="noreferrer">GitHub</a>
        </p>
        <p class="muted">双通道 GitHub 加速器 · 规则签名信任链 · MIT</p>
      </div>
    </div>
  );
}
