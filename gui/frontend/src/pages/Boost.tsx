// Boost.tsx —— 一键加速骨架（W1：按钮 + API 结果展示；W4 补交互细节）。
import { createSignal, Show } from "solid-js";
import { systemOn, systemOff, gitOp, sshOp } from "../api/client";

export default function Boost() {
  const [msg, setMsg] = createSignal("");
  const [busy, setBusy] = createSignal(false);

  const run = async (label: string, fn: () => Promise<unknown>) => {
    setBusy(true);
    setMsg(`${label}…`);
    try {
      const r = await fn();
      setMsg(`${label} ✓ ${JSON.stringify(r)}`);
    } catch (e) {
      setMsg(`${label} ✗ ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="page">
      <h2>加速开关</h2>
      <div class="row">
        <button disabled={busy()} onClick={() => run("接管系统代理", () => systemOn("pac"))}>
          一键加速（on）
        </button>
        <button disabled={busy()} onClick={() => run("恢复直连", () => systemOff(false))}>
          恢复（off）
        </button>
      </div>
      <h2>Git insteadOf</h2>
      <div class="row">
        <button disabled={busy()} onClick={() => run("git enable", () => gitOp("enable", { cdn: promptCDN() }))}>
          git enable
        </button>
        <button disabled={busy()} onClick={() => run("git disable", () => gitOp("disable"))}>
          git disable
        </button>
        <button disabled={busy()} onClick={() => run("git status", () => gitOp("status"))}>
          git status
        </button>
      </div>
      <h2>SSH 443</h2>
      <div class="row">
        <button disabled={busy()} onClick={() => run("ssh enable", () => sshOp("enable", { assume_ok: true }))}>
          ssh enable
        </button>
        <button disabled={busy()} onClick={() => run("ssh disable", () => sshOp("disable"))}>
          ssh disable
        </button>
        <button disabled={busy()} onClick={() => run("ssh status", () => sshOp("status"))}>
          ssh status
        </button>
      </div>
      <Show when={msg()}>
        <p class="mono result">{msg()}</p>
      </Show>
    </div>
  );
}

// W1 骨架：CDN 取全局设置；W4 设置页贯通后删此函数
function promptCDN(): string {
  return localStorage.getItem("ghydra.cdn") || "";
}
