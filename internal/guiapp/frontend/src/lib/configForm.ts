// configForm.ts —— Settings 运行参数表单纯逻辑（P4c）。
//
// diffPatch：只提交变化的字段（指针语义：null/缺省 = 不改，后端契约）。
// restartHint：requires_restart（p4a 后端：rules_url/rules_interval_s/
// listen/doctor_every_s；cdn 热更不在内）翻译成人话提示。

import type { ApiConfig, ConfigPatch, ConfigSetResp } from "../types";

export interface ConfigFormValues {
  cdn: string;
  rulesUrl: string;
  rulesIntervalS: number;
  listen: string;
  doctorEveryS: number;
}

export function valuesOf(c: ApiConfig): ConfigFormValues {
  return {
    cdn: c.cdn,
    rulesUrl: c.rules_url,
    rulesIntervalS: c.rules_interval_s,
    listen: c.listen,
    doctorEveryS: c.doctor_every_s,
  };
}

export function diffPatch(cur: ApiConfig, v: ConfigFormValues): ConfigPatch {
  const p: ConfigPatch = {};
  if (v.cdn !== cur.cdn) p.cdn = v.cdn;
  if (v.rulesUrl !== cur.rules_url) p.rules_url = v.rulesUrl;
  if (v.rulesIntervalS !== cur.rules_interval_s) p.rules_interval_s = v.rulesIntervalS;
  if (v.listen !== cur.listen) p.listen = v.listen;
  if (v.doctorEveryS !== cur.doctor_every_s) p.doctor_every_s = v.doctorEveryS;
  return p;
}

const LABELS: Record<string, string> = {
  rules_url: "规则源",
  rules_interval_s: "规则周期",
  listen: "监听端口",
  doctor_every_s: "体检周期",
};

export function restartHint(resp: ConfigSetResp): string | null {
  const keys = resp.requires_restart || [];
  if (keys.length === 0) return null;
  const names = keys.map((k) => LABELS[k] || k).join("、");
  return `${names} 需重启 daemon 生效（更新或 ghydra on/off 重启时自动应用）`;
}
