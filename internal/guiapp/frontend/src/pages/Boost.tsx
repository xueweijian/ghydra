// Boost.tsx —— 一键加速（W3c 状态化重写）：接管态 SSE 驱动大开关 +
// git/ssh 集成卡。on 失败展示回滚语义（apicore 保证失败全回滚）。
import { createSignal, Show, createResource, For } from "solid-js";
import { systemOn, systemOff, gitOp, sshOp, gitStatus, sshStatus } from "../api/client";
import type { ApiError } from "../api/client";
import type { EventsState } from "../api/sse";
import type { GitStatusInfo, SSHStatusInfo } from "../types";

function fmtSince(s: string): string {
  const t = Date.parse(s);
  if (Number.isNaN(t)) return s;
  return new Date(t).toLocaleTimeString();
}

function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

export default function Boost(props: { events: () => EventsState }) {
  const [busy, setBusy] = createSignal(false);
  const [msg, setMsg] = createSignal("");
  const [err, setErr] = createSignal("");

  const takeover = () => props.events().status?.takeover;
  const connected = () => props.events().connected;

  const [git, gitActions] = createResource<GitStatusInfo>(() => gitStatus());
  const [ssh, sshActions] = createResource<SSHStatusInfo>(() => sshStatus());

  const run = async (label: string, fn: () => Promise<unknown>, after?: () => void) => {
    setBusy(true);
    setErr("");
    setMsg(`${label}…`);
    try {
      await fn();
      setMsg(`${label} ✓`);
      after?.();
    } catch (e) {
      const ae = e as ApiError;
      setMsg("");
      if (ae && ae.status === 503) {
        setErr(`${label} ✗ daemon 未装配该能力（${errText(e)}）`);
      } else if (ae && ae.status === 409) {
        setErr(`${label} ✗ ${errText(e)}`);
      } else {
        setErr(`${label} ✗ ${errText(e)}${label.includes("接管") ? "——已回滚，系统代理未改动" : ""}`);
      }
    } finally {
      setBusy(false);
    }
  };

  const gitOn = async () =>
    run("git enable", () => {
      const cdn = localStorage.getItem("ghydra.cdn") || "";
      return gitOp("enable", cdn ? { cdn } : {});
    }, gitActions.refetch);
  const gitOff = async () => run("git disable", () => gitOp("disable"), gitActions.refetch);
  const sshOn = async () => run("ssh enable", () => sshOp("enable", { assume_ok: true }), sshActions.refetch);
  const sshOff = async () => run("ssh disable", () => sshOp("disable"), sshActions.refetch);

  const gitManaged = () => (git()?.keys || []).some((k) => k.managed);

  return (
    <div class="page">
      <h2>系统代理</h2>
      <Show
        when={takeover()}
        fallback={
          <p class="muted">
            {connected() ? "等待 daemon 状态…" : "daemon 未连接（检查地址与 token，设置页可配）"}
          </p>
        }
      >
        {(t) => (
          <div class="card">
            <Show
              when={t().on}
              fallback={
                <>
                  <p class="big switch">○ 未接管</p>
                  <p class="muted">浏览器/Git 流量走系统默认网络</p>
                  <div class="row">
                    <button disabled={busy()} onClick={() => run("接管系统代理", () => systemOn("pac"))}>
                      一键加速（PAC）
                    </button>
                    <button disabled={busy()} onClick={() => run("接管系统代理", () => systemOn("proxy"))}>
                      全局代理模式
                    </button>
                  </div>
                </>
              }
            >
              <p class="big switch" style={{ color: "#22c55e" }}>
                ● 已接管
              </p>
              <p class="muted">自 {fmtSince(t().since)} · github 系域名经 GHydra 加速</p>
              <div class="row">
                <button disabled={busy()} onClick={() => run("恢复直连", () => systemOff(false))}>
                  恢复直连（off）
                </button>
              </div>
            </Show>
          </div>
        )}
      </Show>
      <Show when={msg()}>
        <p class="mono result">{msg()}</p>
      </Show>
      <Show when={err()}>
        <p class="err">{err()}</p>
      </Show>

      <h2>Git insteadOf（clone/fetch 走 B 通道）</h2>
      <div class="card">
        <Show when={git()} fallback={<p class="muted">读取中…</p>}>
          {(g) => (
            <>
              <p>
                <span class={"badge " + (gitManaged() ? "b-ok" : "b-off")}>
                  {gitManaged() ? "已启用" : "未启用"}
                </span>{" "}
                <Show when={g().snapshot}>
                  <span class="badge b-b">快照可还原</span>
                </Show>
              </p>
              <Show when={g().keys.length > 0}>
                <table>
                  <tbody>
                    <For each={g().keys}>
                      {(k) => (
                        <tr>
                          <td class="mono">{k.name}</td>
                          <td class="mono muted">{k.value}</td>
                          <Show when={k.managed}>
                            <td class="badge b-ok">ghydra</td>
                          </Show>
                        </tr>
                      )}
                    </For>
                  </tbody>
                </table>
              </Show>
              <div class="row">
                <button disabled={busy()} onClick={gitOn}>
                  启用
                </button>
                <button disabled={busy()} onClick={gitOff}>
                  停用（还原）
                </button>
              </div>
              <p class="muted">B 通道 CDN 取设置页配置；push 默认保持直连</p>
            </>
          )}
        </Show>
      </div>

      <h2>SSH 443（绕过 22 端口封锁）</h2>
      <div class="card">
        <Show when={ssh()} fallback={<p class="muted">读取中…</p>}>
          {(s) => (
            <>
              <p>
                <span class={"badge " + (s().enabled ? "b-ok" : "b-off")}>
                  {s().enabled ? "已启用" : "未启用"}
                </span>{" "}
                <Show when={s().alias}>
                  <span class="badge b-b">alias 模式</span>
                </Show>
              </p>
              <p class="muted mono">{s().config_path}</p>
              <div class="row">
                <button disabled={busy()} onClick={sshOn}>
                  启用
                </button>
                <button disabled={busy()} onClick={sshOff}>
                  停用（还原）
                </button>
              </div>
            </>
          )}
        </Show>
      </div>
    </div>
  );
}
