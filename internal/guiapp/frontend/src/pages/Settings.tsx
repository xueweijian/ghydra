// Settings.tsx —— 设置页（P4c）：连接配置 + 运行参数（可编辑，p4a 后端）
// + 自更新卡片（D7/D8）+ 关于。
//
// 更新卡片由纯函数 updateCardState 驱动（__tests__/updateState.test.ts
// 表驱动锁定状态机）；SSE status.update 帧 1s diff 自动喂入。两段式语义：
// apply 成功后 daemon <1s 内退出——「重启中」按「曾点安装+断连」判定。
import { createSignal, onMount, Show } from "solid-js";
import {
  daemonBase, setDaemonBase, apiToken, setApiToken, setConfig, getConfig, getStatus,
  updateCheck, updateApply, updateStatus,
} from "../api/client";
import { useEvents } from "../api/sse";
import { diffPatch, restartHint, valuesOf, type ConfigFormValues } from "../lib/configForm";
import { updateCardState } from "../lib/updateState";

const APPLY_TEXT: Record<string, string> = {
  checking: "正在检查…",
  downloading: "正在下载",
  verifying: "正在验证签名",
  swapping: "正在交换二进制",
  pending_boot: "等待重启生效",
};

export default function Settings() {
  const [base, setBase] = createSignal(daemonBase());
  const [tok, setTok] = createSignal(apiToken());
  const [msg, setMsg] = createSignal("");
  const [test, setTest] = createSignal("");
  const events = useEvents();

  // ---- 运行参数表单（p4a：POST /api/config 指针语义 + requires_restart）----
  const [form, setForm] = createSignal<ConfigFormValues | null>(null);
  const [cfgMsg, setCfgMsg] = createSignal("");
  const [cfgErr, setCfgErr] = createSignal("");
  const [doctorRepo, setDoctorRepo] = createSignal(""); // 只读展示（无 patch 字段）
  const cur = () => events.state().status; // 表单基准 = 运行态 config

  onMount(async () => {
    try {
      const c = await getConfig();
      setForm(valuesOf(c));
      setDoctorRepo(c.doctor_repo);
    } catch {
      /* 未连接时不阻塞设置编辑 */
    }
  });

  const saveConn = async () => {
    // W3 存量修复：原实现从未持久化地址/token（仅提示）。落盘 + 重连。
    setDaemonBase(base());
    setApiToken(tok());
    events.reconnect();
    setMsg("连接已保存。");
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

  const saveCfg = async () => {
    const f = form();
    if (!f) return;
    setCfgErr("");
    setCfgMsg("");
    const patch = diffPatch(
      {
        listen: f.listen, scheduler: true, cdn: f.cdn, doctor_every_s: f.doctorEveryS,
        doctor_repo: doctorRepo() || "", managed: false,
        rules_url: f.rulesUrl, rules_interval_s: f.rulesIntervalS,
      },
      f,
    );
    if (Object.keys(patch).length === 0) {
      setCfgMsg("无修改。");
      return;
    }
    try {
      const resp = await setConfig(patch);
      setForm(valuesOf(resp));
      const hint = restartHint(resp);
      setCfgMsg(hint ? `已保存；${hint}` : "已保存（立即生效）。");
    } catch (e) {
      setCfgErr(e instanceof Error ? e.message : String(e));
    }
  };

  // ---- 自更新卡片（D7）----
  const [info, setInfo] = createSignal<Awaited<ReturnType<typeof updateCheck>> | null>(null);
  const [checking, setChecking] = createSignal(false);
  const [versionAtApply, setVersionAtApply] = createSignal<string | null>(null);
  const [applyErr, setApplyErr] = createSignal("");
  // updateRunner 未装配（CLI 裸构建等）= 503 → 卡片降级「不支持」。
  const [supported, setSupported] = createSignal(true);
  onMount(async () => {
    try {
      await updateStatus();
    } catch (e) {
      if (e instanceof Error && e.message.includes("503")) setSupported(false);
    }
  });

  const doCheck = async () => {
    setChecking(true);
    setApplyErr("");
    try {
      setInfo(await updateCheck());
    } catch (e) {
      setApplyErr(e instanceof Error ? e.message : String(e));
    } finally {
      setChecking(false);
    }
  };

  const doApply = async () => {
    setApplyErr("");
    const v = cur()?.version;
    if (!v) return;
    setVersionAtApply(v);
    try {
      await updateApply();
    } catch (e) {
      setApplyErr(e instanceof Error ? e.message : String(e));
      setVersionAtApply(null);
    }
  };

  const inShell = () => !(typeof location !== "undefined" && location.protocol.startsWith("http"));
  const card = () =>
    updateCardState({
      supported: supported(),
      connected: events.state().connected,
      currentVersion: cur()?.version || "",
      info: info(),
      status: cur()?.update || null,
      checking: checking(),
      versionAtApply: versionAtApply(),
    });

  const onDoneAck = () => {
    setVersionAtApply(null);
    setInfo(null);
  };

  return (
    <div class="page">
      <h2>设置</h2>
      <label>Daemon 地址</label>
      <input type="text" value={base()} onInput={(e) => setBase(e.currentTarget.value)} />
      <label>API Token（终端运行 ghydra token 获取）</label>
      <input type="password" value={tok()} onInput={(e) => setTok(e.currentTarget.value)} />
      <div class="row">
        <button onClick={saveConn}>保存</button>
        <button onClick={testConn}>测试连接</button>
        <Show when={test()}>
            <span class={test().startsWith("✓") ? "muted" : "err"}>{test()}</span>
        </Show>
      </div>
      <Show when={msg()}>
        <p class="mono">{msg()}</p>
      </Show>

      <h3>运行参数</h3>
      <Show when={form()} fallback={<p class="muted">未连接，无法读取运行参数。</p>}>
        <div class="card">
          <label>监听地址（127.0.0.1:端口；重启生效）</label>
          <input type="text" value={form()!.listen}
            onInput={(e) => setForm({ ...form()!, listen: e.currentTarget.value })} />
          <label>规则源 URL（留空 = 官方源；重启生效）</label>
          <input type="text" value={form()!.rulesUrl}
            onInput={(e) => setForm({ ...form()!, rulesUrl: e.currentTarget.value })} />
          <div class="row">
            <label>规则周期（分钟）</label>
            <input type="number" min="1" style={{ width: "6em" }} value={form()!.rulesIntervalS / 60}
              onInput={(e) => setForm({ ...form()!, rulesIntervalS: Math.max(1, Number(e.currentTarget.value) || 1) * 60 })} />
            <label>体检周期（分钟，0=关）</label>
            <input type="number" min="0" style={{ width: "6em" }} value={form()!.doctorEveryS / 60}
              onInput={(e) => setForm({ ...form()!, doctorEveryS: Math.max(0, Number(e.currentTarget.value) || 0) * 60 })} />
          </div>
          <div class="row">
            <button onClick={saveCfg}>保存运行参数</button>
            <Show when={cfgMsg()}><span class="muted">{cfgMsg()}</span></Show>
            <Show when={cfgErr()}><span class="err">{cfgErr()}</span></Show>
          </div>
        </div>
      </Show>

      <h3>自更新</h3>
      <div class="card">
        <Show when={events.state().connected} fallback={<p class="muted">daemon 未连接。</p>}>
          <Show when={card().kind !== "unsupported"} fallback={<p class="muted">此构建不支持自更新。</p>}>
            {(() => {
              const c = card();
              switch (c.kind) {
                case "idle":
                  return (
                    <div class="row">
                      <span class="mono">当前 v{cur()?.version}</span>
                      <button onClick={doCheck}>检查更新</button>
                    </div>
                  );
                case "checking":
                  return <p>正在检查更新…</p>;
                case "uptodate":
                  return <p>已是最新 v{c.version}。</p>;
                case "available":
                  return (
                    <div>
                      <p class="mono">v{cur()?.version} → v{c.info.latest}</p>
                      <Show when={c.info.notes}><p class="muted">{c.info.notes}</p></Show>
                      <div class="row">
                        <button onClick={doApply}>下载并安装</button>
                        <Show when={c.info.html_url}>
                          <a href={c.info.html_url} target="_blank" rel="noreferrer">更新说明</a>
                        </Show>
                      </div>
                    </div>
                  );
                case "applying":
                  return (
                    <div>
                      <p>{APPLY_TEXT[c.state] || c.state}…</p>
                      <div style={{ background: "#ddd", "border-radius": "4px", height: "8px" }}>
                        <div style={{ background: "#36c", height: "8px", width: `${Math.round(c.pct)}%`, "border-radius": "4px" }} />
                      </div>
                    </div>
                  );
                case "restarting":
                  return inShell()
                    ? <p>更新已就绪，daemon 正在重启…</p>
                    : <p>更新已安装，daemon 已退出。请在终端运行 <code>ghydra on</code> 重新启动。</p>;
                case "done":
                  return (
                    <div>
                      <p>✓ 已更新到 v{c.version}</p>
                      <p class="muted">
                        {inShell() ? "系统代理已恢复直连，可在「加速」页重新开启。" : "可运行 ghydra on 重新开启加速。"}
                      </p>
                      <div class="row"><button onClick={onDoneAck}>知道了</button></div>
                    </div>
                  );
                case "error":
                  return (
                    <div>
                      <p class="err">更新失败：{c.message}</p>
                      <div class="row"><button onClick={onDoneAck}>返回</button></div>
                    </div>
                  );
                default:
                  return null;
              }
            })()}
            <Show when={applyErr()}><p class="err">{applyErr()}</p></Show>
          </Show>
        </Show>
      </div>

      <h3>关于</h3>
      <div class="card">
        <p>
          GHydra{" "}
          <Show when={cur()} fallback={<span class="muted">（未连接）</span>}>
            <span class="mono">v{cur()!.version}</span>
          </Show>{" "}
          · <a href="https://github.com/xueweijian/ghydra" target="_blank" rel="noreferrer">GitHub</a>
        </p>
        <p class="muted">双通道 GitHub 加速器 · 规则签名信任链 · MIT</p>
      </div>
    </div>
  );
}
