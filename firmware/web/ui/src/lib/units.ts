// 读数格式化。照搬自管理台 views/status.ts 的同名函数，两张图表页共用。

import { t } from "@/lib/i18n";

export function clampPct(p: number): number {
  return Number.isFinite(p) ? Math.min(100, Math.max(0, p)) : 0;
}

export function ratio(used: number, total: number): number {
  return total > 0 ? (used / total) * 100 : 0;
}

export function fmtPct(p: number): string {
  return `${clampPct(p).toFixed(1)}%`;
}

/** fmtBytes 二进制单位（与内核口径一致），三位有效数字上下。 */
export function fmtBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n < 1024) return `${Math.round(n)} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let u = -1;
  do {
    v /= 1024;
    u++;
  } while (v >= 1024 && u < units.length - 1);
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[u] ?? "TiB"}`;
}

export function fmtRate(n: number): string {
  return `${fmtBytes(n)}/s`;
}

export function fmtUptime(secs: number): string {
  if (secs < 60) return t("{secs} 秒", { secs });
  const m = Math.floor(secs / 60) % 60;
  const h = Math.floor(secs / 3600) % 24;
  const d = Math.floor(secs / 86400);
  if (d > 0) return t("{d} 天 {h} 小时", { d, h });
  if (h > 0) return t("{h} 小时 {m} 分", { h, m });
  return t("{m} 分钟", { m });
}

/** fmtClock 采样时刻到秒（fmtTime 只到分，仪表读数需要更细）。 */
export function fmtClock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const p = (n: number): string => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

export function fmtWindow(ms: number): string {
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)} s` : `${ms} ms`;
}
