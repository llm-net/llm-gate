// 展示层小工具。照搬自管理台 dom.ts 里与 DOM 无关的那几个（fmtTime 是手写的，
// 没引日期库，这边也不引）。

import type { ApiKey } from "./api";

/** fmtTime 把 RFC 3339 时间串格式化为本地「YYYY-MM-DD HH:mm」。 */
export function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const p = (n: number): string => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** keyDisplay 是 Key 在界面上的短读数（前缀…后四位），明文从不参与展示。 */
export function keyDisplay(k: ApiKey): string {
  return `${k.display_prefix}…${k.display_last4}`;
}

/**
 * keyMask 把一把 Key 明文压成与 keyDisplay 同款的「前缀…末 4 位」（`sk_…ab12`）——
 * 「接入方法」页拿使用者贴入的 Key 这样回显。前缀取到第一个下划线为止（`sk_`，
 * 导入的密钥可能是别的前缀），没有下划线就取前 3 个字符；太短的串只给省略号，
 * 不回显任何一段。
 */
export function keyMask(plain: string): string {
  const trimmed = plain.trim();
  if (trimmed.length < 12) return "…";
  const underscore = trimmed.indexOf("_");
  const prefix = underscore > 0 && underscore < 8 ? trimmed.slice(0, underscore + 1) : trimmed.slice(0, 3);
  return `${prefix}…${trimmed.slice(-4)}`;
}
