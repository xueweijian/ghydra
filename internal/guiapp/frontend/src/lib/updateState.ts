// updateState.ts —— Settings 更新卡片状态机（P4c，纯函数）。
//
// 关键语义（p4b 两段式实证）：daemon apply 成功后 pending_boot <1s 内
// 优雅退出——前端大概率看不到 pending_boot 帧。卡片「重启中」按
// 「曾点安装 + SSE 断连」判定，不依赖最后一帧；重连后版本变化 = 完成。
// 更新后系统代理默认恢复直连（用户拍板：不自动重开）——done 态带提示。

import type { UpdateInfo, UpdateStatus } from "../types";

export interface UpdateCardInput {
  supported: boolean; // /api/update/status 非 503（runner 已装配）
  connected: boolean; // SSE 连接态
  currentVersion: string; // SSE status.version
  info: UpdateInfo | null; // 最近一次 check 结果
  status: UpdateStatus | null; // SSE status.update
  checking: boolean; // 本地 check 请求在途
  versionAtApply: string | null; // 点「下载并安装」时的 daemon 版本
}

export type UpdateCardState =
  | { kind: "unsupported" }
  | { kind: "idle" }
  | { kind: "checking" }
  | { kind: "uptodate"; version: string }
  | { kind: "available"; info: UpdateInfo }
  | { kind: "applying"; state: string; pct: number }
  | { kind: "restarting" }
  | { kind: "done"; version: string; needReenable: boolean }
  | { kind: "error"; message: string };

const APPLY_STATES = new Set(["checking", "downloading", "verifying", "swapping", "pending_boot"]);

export function updateCardState(inp: UpdateCardInput): UpdateCardState {
  if (!inp.supported) return { kind: "unsupported" };

  // 安装流程跟踪优先于一切（versionAtApply 由组件在点安装时记录）
  if (inp.versionAtApply !== null) {
    if (!inp.connected) return { kind: "restarting" };
    if (inp.currentVersion !== inp.versionAtApply) {
      // 重连且版本已变 = 新版生效。加速默认已关（更新退出 hook 恢复直连）。
      return { kind: "done", version: inp.currentVersion, needReenable: true };
    }
  }

  const st = inp.status;
  if (st && APPLY_STATES.has(st.state)) {
    return { kind: "applying", state: st.state, pct: st.progress_pct ?? 0 };
  }
  if (st && st.state === "failed") {
    return { kind: "error", message: st.error || "更新失败" };
  }

  if (inp.checking) return { kind: "checking" };

  const info = inp.info;
  if (info) {
    if (info.error) return { kind: "error", message: info.error };
    if (info.has_update && info.latest) return { kind: "available", info };
    return { kind: "uptodate", version: info.current };
  }
  return { kind: "idle" };
}
