// 模型用量读数页。回答一个问题——「这段时间在模型上花了多少钱」，金额为主、token 与耗时为辅。
//
// **一页、一条路由、一个端点**，一次请求取齐：`/usage` → GET /admin/v1/usage，
// 设备汇总 + 五个维度分解 + 全量明细环。设备只有一个管理员，账也就只有一本
// （用户概念退场前这里还分「用量」与「个人用量」两页）。
//
// 本页**没有**预算进度块：额度是密钥自己的属性，跟着「API密钥」页的列表一起看
// ——为一份额度在这里多发一个请求会破掉零轮询纪律，同一份事实也不该有两处画法。
//
// **零轮询**（与设备状态页同一口径）：进入页面一次、手动刷新一次、切区间一次，各一个
// 请求；服务端不推、客户端不轮。
//
// 两处渲染纪律，反着写就会当场骗人：
//   - **带 task_id 的明细行是「视频任务清算」不是失败请求**：清算样本的
//     status/attempts 恒为 0（它不是一次调用，而是任务完成后补记的一笔账），按状态码
//     渲染会把它们全画成错误。
//   - **「未定价」看服务端给的 priced，不看金额**：金额 0 的原因太多了——只被
//     count_tokens 打过、显式配 0、请求全被拒。反过来，改价前留下的一笔消费也会让真
//     未定价的模型金额大于 0。

import { useState } from "react";
import { ChevronRight } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { SegTabs } from "@/features/access/access";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

// 跨刷新记住上次选的区间（模块级，会话内有效）。
let usageRange: api.UsageRange = "today";
let usageKeyID: number | null = null;

const rangeOptions: [api.UsageRange, string][] = [
  ["today", t("今日")],
  ["month", t("本月")],
  ["7d", t("近 7 天")],
  ["30d", t("近 30 天")],
];

function rangeLabel(r: api.UsageRange): string {
  return rangeOptions.find(([k]) => k === r)?.[1] ?? r;
}

const CHART_W = 600;
const CHART_H = 120;
const dimRowLimit = 8;
const dayMs = 86_400_000;

// ---- 格式化 ----

function fmtInt(n: number): string {
  return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

function fmtDuration(ms: number): string {
  if (ms <= 0) return "—";
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60_000)}m${String(Math.round((ms % 60_000) / 1000)).padStart(2, "0")}s`;
}

/** 区间端点：到分钟即可（区间边界不是仪表读数）。 */
function fmtStamp(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const p = (n: number): string => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** 明细行的时刻：同一天的居多，到秒，日期只在跨天时补。 */
function fmtEventClock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  const p = (n: number): string => String(n).padStart(2, "0");
  const hms = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  const now = new Date();
  const sameDay =
    d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  return sameDay ? hms : `${p(d.getMonth() + 1)}-${p(d.getDate())} ${hms}`;
}

// 计费量是三种而不是一种：文本按 token，MiniMax H3 视频按秒，方舟 Seedream 按出图
// 张数。表里只印 token 的话，每一行视频消费都显示「0」——金额看得见、金额背后的量
// 看不见。单位跟着值走、零值不印（三项全零才是「—」）。
interface QuantityLike {
  total_tokens: number;
  video_seconds: number;
  image_count: number;
}

function fmtQuantity(q: QuantityLike): string {
  const parts: string[] = [];
  if (q.total_tokens > 0) parts.push(`${fmtInt(q.total_tokens)} Token`);
  if (q.video_seconds > 0) parts.push(t("{n} 秒", { n: fmtInt(q.video_seconds), count: q.video_seconds }));
  if (q.image_count > 0) parts.push(t("{n} 张", { n: fmtInt(q.image_count), count: q.image_count }));
  return parts.length === 0 ? "—" : parts.join(" · ");
}

function nonTokenSuffix(q: QuantityLike): string {
  const out: string[] = [];
  if (q.video_seconds > 0) out.push(t("视频 {n} 秒", { n: fmtInt(q.video_seconds), count: q.video_seconds }));
  if (q.image_count > 0) out.push(t("图片 {n} 张", { n: fmtInt(q.image_count), count: q.image_count }));
  return out.length === 0 ? "" : ` · ${out.join(" · ")}`;
}

/** 空取值的渲染：空串照样进表（否则各维度合计与总计对不上），统一显示成「—」。 */
function Dash({ title }: { title: string }) {
  return (
    <span className="text-muted-foreground" title={title}>
      —
    </span>
  );
}

// ---- 数字卡 ----

function StatTile({ label, value, sub }: { label: string; value: string; sub: string }) {
  return (
    <Card className="gap-1 p-4">
      <span className="text-muted-foreground text-xs">{label}</span>
      <b className="font-mono text-lg tabular-nums">{value}</b>
      <span className="text-muted-foreground text-xs">{sub}</span>
    </Card>
  );
}

function StatTiles({ total }: { total: api.UsageSummary }) {
  const forwarded = Math.max(0, total.requests - total.rejected_requests);
  const avgMs = total.requests > 0 ? Math.round(total.duration_ms_sum / total.requests) : 0;
  const abnormal = total.errors + total.rejected_requests;
  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-6">
      <StatTile
        label={t("消费金额")}
        value={api.fmtMoney(total.cost_micro)}
        sub={
          total.unavailable_requests > 0
            ? t("其中 {n} 笔未取得完整用量，未计费", { n: fmtInt(total.unavailable_requests) })
            : total.estimated_requests > 0
            ? t("其中 {n} 笔含估算", { n: fmtInt(total.estimated_requests), count: total.estimated_requests })
            : t("全部按厂商用量计")
        }
      />
      <StatTile
        label={t("请求数")}
        value={fmtInt(total.requests)}
        sub={t("转发 {forwarded} · 被拒 {rejected}", {
          forwarded: fmtInt(forwarded),
          rejected: fmtInt(total.rejected_requests),
        })}
      />
      <StatTile
        label={t("异常请求")}
        value={fmtInt(abnormal)}
        sub={t("上游或协议错误 {errors} · 限额拒绝 {rejected}", {
          errors: fmtInt(total.errors),
          rejected: fmtInt(total.rejected_requests),
        })}
      />
      <StatTile
        label={t("Token 合计")}
        value={fmtInt(total.total_tokens)}
        sub={t("输入 {prompt} · 输出 {completion}", {
          prompt: fmtInt(total.prompt_tokens),
          completion: fmtInt(total.completion_tokens),
        })}
      />
      <StatTile
        label={t("缓存 / 媒体")}
        value={`${fmtInt(total.cache_read_tokens)} Token`}
        sub={t("缓存写入 {n}", { n: fmtInt(total.cache_write_tokens) }) + nonTokenSuffix(total)}
      />
      <StatTile
        label={t("平均耗时")}
        value={avgMs === 0 ? "—" : fmtDuration(avgMs)}
        sub={t("合计 {duration}", { duration: fmtDuration(total.duration_ms_sum) })}
      />
    </div>
  );
}

// ---- 消费趋势（手绘 SVG，无图表库） ----

function zeroSummary(): api.UsageSummary {
  return {
    cost_micro: 0,
    requests: 0,
    errors: 0,
    rejected_requests: 0,
    estimated_requests: 0,
    unavailable_requests: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    cache_read_tokens: 0,
    cache_write_tokens: 0,
    total_tokens: 0,
    video_seconds: 0,
    image_count: 0,
    duration_ms_sum: 0,
  };
}

/** tzOffsetMs 取 RFC 3339 串尾的时区偏移（无偏移或 Z 即 UTC）。 */
function tzOffsetMs(rfc: string): number {
  const m = /([+-])(\d{2}):(\d{2})$/.exec(rfc);
  if (m === null) return 0;
  return (m[1] === "-" ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3])) * 60_000;
}

function ymdUTC(ms: number): string {
  const d = new Date(ms);
  const p = (n: number): string => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())}`;
}

function dayKeysBetween(from: string, to: string): string[] {
  const a = Date.parse(from);
  const b = Date.parse(to);
  if (Number.isNaN(a) || Number.isNaN(b) || b < a) return [];
  const off = tzOffsetMs(from);
  const start = Math.floor((a + off) / dayMs);
  const end = Math.floor((b + off) / dayMs);
  if (end - start > 400) return []; // 防御：区间关键词最长 30 天，越界即数据异常
  const out: string[] = [];
  for (let d = start; d <= end; d++) out.push(ymdUTC(d * dayMs));
  return out;
}

// fillDays 把「只含有数据的那些天」补成完整的日期轴。日期边界取响应里 from/to
// **自带的时区偏移**（设备本地时区）而不是浏览器的——管理员的浏览器未必与设备同区，
// 按浏览器算会整体错开一天，而 day 串是服务端按设备本地日期给的。
function fillDays(rep: api.UsageReport): api.UsageDayPoint[] {
  const byDay = new Map<string, api.UsageDayPoint>();
  for (const d of rep.days) byDay.set(d.day, d);
  for (const k of dayKeysBetween(rep.from, rep.to)) {
    if (!byDay.has(k)) byDay.set(k, { ...zeroSummary(), day: k });
  }
  return [...byDay.values()].sort((a, b) => (a.day < b.day ? -1 : a.day > b.day ? 1 : 0));
}

// 按日金额柱状图。日粒度的账是离散的桶而不是连续曲线，所以**画柱不画线**；没有消费
// 的那天是一根零高的柱（服务端只回有数据的天，补零是展示层的职责），断档因此自然成缝。
function TrendPanel({ rep }: { rep: api.UsageReport }) {
  const days = fillDays(rep);
  const [hover, setHover] = useState<number | null>(null);

  if (days.length < 2) {
    return (
      <Card className="gap-3 p-5">
        <h2 className="font-semibold">{t("消费趋势")}</h2>
        <p className="text-muted-foreground text-xs">
          {t("当前区间不足两天，没有跨日趋势可画——切到「本月」「近 7 天」或「近 30 天」即可看到。")}
        </p>
      </Card>
    );
  }

  const maxCost = days.reduce((m, d) => Math.max(m, d.cost_micro), 0);
  const yMax = maxCost > 0 ? maxCost : 1;
  const slot = CHART_W / days.length;
  const barW = Math.max(1.5, Math.min(28, slot * 0.62));
  const idx = hover ?? days.length - 1;
  const shown = days[idx];
  const first = days[0];
  const last = days[days.length - 1];

  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-baseline gap-3">
        <h2 className="font-semibold">{t("消费趋势")}</h2>
        {shown === undefined ? null : (
          <span className="ml-auto flex items-baseline gap-1.5">
            <b className="font-mono text-xs tabular-nums">{api.fmtMoney(shown.cost_micro)}</b>
            <span className="text-muted-foreground text-[10px]">
              {t("{day} · {n} 请求", { day: shown.day, n: fmtInt(shown.requests), count: shown.requests })}
            </span>
          </span>
        )}
      </div>
      <div
        className="relative"
        onPointerMove={(ev) => {
          const rect = ev.currentTarget.getBoundingClientRect();
          if (rect.width <= 0) return;
          const i = Math.min(
            days.length - 1,
            Math.max(0, Math.floor(((ev.clientX - rect.left) / rect.width) * days.length)),
          );
          setHover(i);
        }}
        onPointerLeave={() => setHover(null)}
      >
        <svg
          viewBox={`0 0 ${CHART_W} ${CHART_H}`}
          preserveAspectRatio="none"
          className="bg-muted/30 h-[120px] w-full rounded-md"
        >
          {[0, 0.5, 1].map((frac) => (
            <line
              key={frac}
              x1="0"
              y1={CHART_H * frac}
              x2={CHART_W}
              y2={CHART_H * frac}
              className="stroke-border"
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
            />
          ))}
          {days.map((d, i) => {
            // 零高的柱不画（画出来是一条贴地的线，看着像「有一点点消费」）。
            const h = d.cost_micro <= 0 ? 0 : Math.max(1.5, (d.cost_micro / yMax) * CHART_H);
            if (h === 0) return null;
            return (
              <rect
                key={d.day}
                x={slot * i + (slot - barW) / 2}
                y={CHART_H - h}
                width={barW}
                height={h}
                className={i === idx ? "fill-primary" : "fill-primary/55"}
              />
            );
          })}
        </svg>
        <span className="text-muted-foreground absolute top-0.5 left-1 text-[10px]">{api.fmtMoney(yMax)}</span>
        <span className="text-muted-foreground absolute bottom-0.5 left-1 text-[10px]">¥0.00</span>
      </div>
      <div className="text-muted-foreground flex justify-between text-[10px]">
        <span>{first?.day ?? ""}</span>
        <span>{last?.day ?? ""}</span>
      </div>
      {maxCost === 0 ? (
        <p className="text-muted-foreground text-xs">{t("本区间还没有产生金额：未定价的模型照常转发但记 0 元。")}</p>
      ) : null}
    </Card>
  );
}

// ---- 维度分解 ----

type DimCell = (r: api.UsageDimRow) => React.ReactNode;

function keyLabel(label: string | undefined): string {
  return label === undefined || label === "" ? t("无标签") : label;
}

function KeyIdentity({ label, display }: { label: string | undefined; display: string }): React.ReactElement {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <span className={label === undefined || label === "" ? "text-muted-foreground text-xs" : "font-medium"}>
        {keyLabel(label)}
      </span>
      {display === "" ? (
        <Dash title={t("这一行没有记到 Key 展示串，用量仍按它的 id 单独归集")} />
      ) : (
        <code className="text-muted-foreground font-mono text-xs">{display}</code>
      )}
    </div>
  );
}

function UsageScopeBar({
  keys,
  selectedID,
  onSelect,
}: {
  keys: api.UsageKeyRef[];
  selectedID: number | null;
  onSelect: (id: number | null) => void;
}): React.ReactElement {
  const selected = selectedID === null ? undefined : keys.find((k) => k.id === selectedID);
  return (
    <Card className="gap-3 p-4 sm:flex-row sm:items-center">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <h2 className="text-sm font-semibold">{t("统计对象")}</h2>
          <Badge variant={selectedID === null ? "secondary" : "outline"}>
            {selectedID === null ? t("设备总览") : t("单 Key 详情")}
          </Badge>
        </div>
        <p className="text-muted-foreground mt-1 text-xs">
          {selectedID === null
            ? t("当前汇总全部 API 密钥；可从下方列表进入单 Key 详情，或在右侧直接选择。")
            : t("顶部读数、消费趋势、维度分布和最近请求均只统计当前这把 Key。")}
        </p>
      </div>
      <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:items-center">
        <Select
          value={selectedID === null ? "all" : String(selectedID)}
          onValueChange={(value) => onSelect(value === "all" ? null : Number(value))}
        >
          <SelectTrigger className="w-full sm:w-[25rem]" aria-label={t("选择统计对象")}>
            <SelectValue>
              {selectedID === null ? (
                <span>{t("全部 API 密钥")}</span>
              ) : (
                <span className="flex min-w-0 items-center gap-2">
                  <span className="truncate">{keyLabel(selected?.label)}</span>
                  <code className="text-muted-foreground truncate font-mono text-xs">
                    {selected?.display ?? `Key #${selectedID}`}
                  </code>
                </span>
              )}
            </SelectValue>
          </SelectTrigger>
          <SelectContent align="end">
            <SelectItem value="all">{t("全部 API 密钥")}</SelectItem>
            {keys.map((k) => (
              <SelectItem key={k.id} value={String(k.id)}>
                <span className="flex min-w-0 flex-col items-start">
                  <span className="max-w-72 truncate">{keyLabel(k.label)}</span>
                  <code className="text-muted-foreground max-w-72 truncate font-mono text-xs">
                    {k.display === "" ? `Key #${k.id}` : k.display}
                  </code>
                </span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {selectedID === null ? null : (
          <Button type="button" size="sm" variant="outline" onClick={() => onSelect(null)}>
            {t("返回总览")}
          </Button>
        )}
      </div>
    </Card>
  );
}

function KeyUsagePanel({
  rows,
  onSelect,
}: {
  rows: api.UsageDimRow[];
  onSelect: (id: number) => void;
}): React.ReactElement {
  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-baseline gap-2">
        <h2 className="font-semibold">{t("按 API密钥")}</h2>
        <span className="text-muted-foreground text-xs">
          {t("标签用于识别调用方；点击“查看详情”可展开该 Key 的完整统计。")}
        </span>
      </div>
      <Table className="min-w-[72rem] table-fixed">
        <TableHeader>
          <TableRow>
            <TableHead className="w-[18rem]">{t("标签 / Key")}</TableHead>
            <TableHead className="w-32">{t("金额")}</TableHead>
            <TableHead className="w-24">{t("请求")}</TableHead>
            <TableHead className="w-24">{t("错误")}</TableHead>
            <TableHead className="w-24">{t("拒绝")}</TableHead>
            <TableHead className="w-[20rem]">{t("用量")}</TableHead>
            <TableHead className="w-28">{t("平均耗时")}</TableHead>
            <TableHead className="w-28 text-right">{t("操作")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.length === 0 ? (
            <TableRow>
              <TableCell colSpan={8} className="text-muted-foreground h-24 text-center">
                {t("本区间没有 API 密钥产生用量。")}
              </TableCell>
            </TableRow>
          ) : (
            rows.map((r, i) => {
              const avgMs = r.requests > 0 ? Math.round(r.duration_ms_sum / r.requests) : 0;
              return (
                <TableRow key={`${r.id ?? 0}-${r.key}-${i}`}>
                  <TableCell>
                    <KeyIdentity label={r.label} display={r.key} />
                  </TableCell>
                  <TableCell className="font-mono tabular-nums">{api.fmtMoney(r.cost_micro)}</TableCell>
                  <TableCell className="font-mono tabular-nums">{fmtInt(r.requests)}</TableCell>
                  <TableCell className="font-mono tabular-nums">{fmtInt(r.errors)}</TableCell>
                  <TableCell className="font-mono tabular-nums">{fmtInt(r.rejected_requests)}</TableCell>
                  <TableCell className="font-mono text-xs tabular-nums">{fmtQuantity(r)}</TableCell>
                  <TableCell className="font-mono tabular-nums">{avgMs === 0 ? "—" : fmtDuration(avgMs)}</TableCell>
                  <TableCell className="text-right">
                    {r.id === undefined || r.id <= 0 ? null : (
                      <Button
                        type="button"
                        size="sm"
                        variant="ghost"
                        aria-label={t("查看 {label} 的用量详情", { label: keyLabel(r.label) })}
                        onClick={() => onSelect(r.id as number)}
                      >
                        {t("查看详情")}
                        <ChevronRight />
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              );
            })
          )}
        </TableBody>
      </Table>
    </Card>
  );
}

// 请求数带异常注脚：一行「1 次、¥0.00、0 Token」看不出发生过什么，而它多半正是
// 一次失败或一次被拒——注脚让人在维度表里就读得出，不必先去翻明细环。错误与拒绝
// **分开写**：混成一个数会把「密钥超限」读成「上游故障」（同 StatTiles 的口径）。
function requestsCell(q: { requests: number; errors: number; rejected_requests: number }): React.ReactNode {
  const notes: string[] = [];
  if (q.errors > 0) notes.push(t("{n} 错误", { n: fmtInt(q.errors), count: q.errors }));
  if (q.rejected_requests > 0) {
    notes.push(t("{n} 被拒", { n: fmtInt(q.rejected_requests), count: q.rejected_requests }));
  }
  return (
    <>
      {fmtInt(q.requests)}
      {notes.length > 0 ? (
        <span
          className="text-muted-foreground ml-1.5 text-xs"
          title={t("错误 = 客户端看到的最终状态 ≥400；被拒 = 预算或限流在网关这一侧拦下，没有发往上游。")}
        >
          {notes.join(" · ")}
        </span>
      ) : null}
    </>
  );
}

// 上游维度的空取值不是「没记到名字」，而是一件确定的事：这笔请求没有绑上任何
// 上游。给它一个能读的名字——一个孤零零的「—」只会让人以为是坏数据，而它恰恰是
// 最该看懂的那一行（设备上没连订阅、没选出来源，或者被限额拦下）。
function upstreamCell(r: api.UsageDimRow): React.ReactNode {
  if (r.key !== "") return r.key;
  return (
    <span
      className="text-muted-foreground"
      title={t("这笔请求没有发往任何上游：设备上没有可用的订阅或上游账号、选路没选出来源，或者被预算/限流拒绝。")}
    >
      {t("未转发到上游")}
    </span>
  );
}

// 取值需要译名的维度（入口 / 种类）：空取值仍走 Dash，别让译名函数把空串译成一个
// 看不出是空的字符。
function mappedCell(r: api.UsageDimRow, label: (s: string) => string, emptyTitle: string): React.ReactNode {
  return r.key === "" ? <Dash title={emptyTitle} /> : label(r.key);
}

// 模型名 + 「未定价」徽章。判据是服务端给的 `priced`（读模型目录得来的真值），
// **不是从金额反推**——反推的两个方向都在收口走查里真实复现过：已定价模型在只有
// count_tokens 流量的区间里金额为 0（被误标未定价），未定价模型因为窗口内还留着一笔
// 改价前的消费而金额大于 0（漏标）。priced 缺省 = 目录里没有这个名字，不下判断。
function modelCell(r: api.UsageDimRow): React.ReactNode {
  const how = t("到「模型接入」页的「模型」列表给它录入目录价即可开始计费。");
  return (
    <>
      {r.key === "" ? <Dash title={t("这一行没有记到模型名")} /> : r.key}
      {r.priced === false ? (
        <Badge
          variant="outline"
          className="text-signal-alert ml-1.5"
          title={t("该模型尚未录入目录价：照常转发，但每次调用的金额都记 0 元。{how}", { how })}
        >
          {t("未定价")}
        </Badge>
      ) : null}
    </>
  );
}

// 一个维度的分解表：金额降序（服务端已排好），只列前若干行以免一屏塞不下——余下的
// 并成一行「其余 N 项」，合计因此始终对得上。
function DimPanel({
  title,
  rows,
  head,
  cell,
}: {
  title: string;
  rows: api.UsageDimRow[];
  head: string;
  cell: DimCell;
}) {
  if (rows.length === 0) {
    return (
      <Card className="gap-3 p-5">
        <h2 className="font-semibold">{title}</h2>
        <p className="text-muted-foreground text-xs">{t("本区间没有数据。")}</p>
      </Card>
    );
  }
  const shown = rows.slice(0, dimRowLimit);
  const rest = rows.slice(dimRowLimit);
  const sum = rest.reduce(
    (a, r) => ({
      cost_micro: a.cost_micro + r.cost_micro,
      requests: a.requests + r.requests,
      errors: a.errors + r.errors,
      rejected_requests: a.rejected_requests + r.rejected_requests,
      total_tokens: a.total_tokens + r.total_tokens,
      video_seconds: a.video_seconds + r.video_seconds,
      image_count: a.image_count + r.image_count,
    }),
    { cost_micro: 0, requests: 0, errors: 0, rejected_requests: 0, total_tokens: 0, video_seconds: 0, image_count: 0 },
  );
  return (
    <Card className="gap-3 p-5">
      <h2 className="font-semibold">{title}</h2>
      <div className="w-full overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{head}</TableHead>
              <TableHead>{t("金额")}</TableHead>
              <TableHead>{t("请求")}</TableHead>
              <TableHead>{t("用量")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((r, i) => (
              <TableRow key={`${r.key}-${i}`}>
                <TableCell>{cell(r)}</TableCell>
                <TableCell className="font-mono tabular-nums">{api.fmtMoney(r.cost_micro)}</TableCell>
                <TableCell className="font-mono tabular-nums">{requestsCell(r)}</TableCell>
                <TableCell className="font-mono tabular-nums">{fmtQuantity(r)}</TableCell>
              </TableRow>
            ))}
            {rest.length > 0 ? (
              <TableRow className="text-muted-foreground">
                <TableCell>{t("其余 {n} 项", { n: rest.length })}</TableCell>
                <TableCell className="font-mono tabular-nums">{api.fmtMoney(sum.cost_micro)}</TableCell>
                <TableCell className="font-mono tabular-nums">{requestsCell(sum)}</TableCell>
                <TableCell className="font-mono tabular-nums">{fmtQuantity(sum)}</TableCell>
              </TableRow>
            ) : null}
          </TableBody>
        </Table>
      </div>
    </Card>
  );
}

// ---- 最近请求 ----

// 把准入拒绝的档位译成人话。未知档位原样透出，别编。
function rejectText(reason: string | undefined): string {
  switch (reason) {
    case undefined:
    case "":
      return t("超出限额");
    case "rpm":
      return t("超出该 Key 的每分钟请求上限");
    case "key_budget_day":
      return t("超出该 Key 的日预算");
    case "key_budget_week":
      return t("超出该 Key 的周预算");
    case "key_budget_month":
      return t("超出该 Key 的月预算");
    default:
      return reason;
  }
}

// 结果列。**三类行三种画法**——清算行的 status 恒为 0，按状态码渲染会把它们全画成
// 错误，别退回去。
function ResultCell({ ev, settle }: { ev: api.UsageEvent; settle: boolean }) {
  if (settle) {
    return (
      <Badge variant="secondary" title={t("视频任务完成后补记的金额，不是一次新的调用")}>
        {t("任务清算")}
      </Badge>
    );
  }
  if (ev.rejected === true) {
    return (
      <>
        <Badge variant="outline" className="text-signal-alert">
          {t("已拒绝")}
        </Badge>
        <div className="text-muted-foreground text-xs">{rejectText(ev.reject_reason)}</div>
      </>
    );
  }
  // status 0 = 客户端断开或根本没走到写响应那一步，别把它印成一个「0」当状态码。
  const bad = ev.status === 0 || ev.status >= 400;
  return (
    <>
      <Badge variant="outline" className={bad ? "text-signal-alert" : "text-signal-ok"}>
        {ev.status === 0 ? t("无响应") : String(ev.status)}
      </Badge>
      {ev.attempts > 1 ? (
        <div className="text-muted-foreground text-xs">{t("{n} 条来源尝试", { n: ev.attempts })}</div>
      ) : null}
    </>
  );
}

// 明细环。**只存在于内存**（请求级明细不落盘——每请求一行 INSERT 是 SD 卡写放大的
// 最大头），设备重启即清空。
//
// 它**不随上方的区间过滤**：环里恒是本次运行以来最近的那些条，选「近 30 天」也不会
// 多出昨天的行。这一点必须在界面上说出来，否则明细与汇总对不上时，看的人只会以为
// 账算错了。
function RecentPanel({ events, keys }: { events: api.UsageEvent[]; keys: api.UsageKeyRef[] }) {
  const keyByID = new Map(keys.map((k) => [k.id, k]));
  return (
    <Card className="gap-3 p-5">
      <div className="flex items-center gap-3">
        <h2 className="font-semibold">{t("最近请求")}</h2>
        <span className="text-muted-foreground ml-auto text-xs">
          {t("{n} 条 · 不随上方区间过滤", { n: events.length })}
        </span>
      </div>
      <div className="w-full overflow-x-auto">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("时间")}</TableHead>
              <TableHead>Key</TableHead>
              <TableHead>{t("模型")}</TableHead>
              <TableHead>{t("协议面")}</TableHead>
              <TableHead>{t("结果")}</TableHead>
              <TableHead>{t("用量")}</TableHead>
              <TableHead>{t("金额")}</TableHead>
              <TableHead>{t("耗时")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.length === 0 ? (
              <TableRow>
                <TableCell colSpan={8} className="text-muted-foreground">
                  {t("本次运行以来还没有请求记录。")}
                </TableCell>
              </TableRow>
            ) : (
              events.map((ev, i) => {
                const settle = ev.task_id !== undefined && ev.task_id !== "";
                const key = keyByID.get(ev.key_id);
                return (
                  <TableRow key={i} className={ev.rejected === true ? "opacity-60" : undefined}>
                    <TableCell className="text-muted-foreground font-mono text-xs">{fmtEventClock(ev.at)}</TableCell>
                    <TableCell>
                      <KeyIdentity label={key?.label} display={ev.key_display || key?.display || ""} />
                    </TableCell>
                    <TableCell>
                      {ev.model_name === "" ? <Dash title={t("没有记到模型名")} /> : ev.model_name}
                      <div className="text-muted-foreground text-xs">
                        {api.usageKindLabel(ev.kind)}
                        {ev.upstream_name === "" ? null : ` · ${ev.upstream_name}`}
                      </div>
                    </TableCell>
                    <TableCell>{api.entryLabel(ev.entry)}</TableCell>
                    <TableCell>
                      <ResultCell ev={ev} settle={settle} />
                    </TableCell>
                    <TableCell className="font-mono text-xs tabular-nums">
                      {/* 清算行也印量：视频任务的秒数/张数就是它那笔金额的依据。 */}
                      {ev.usage_unavailable ? t("用量不完整") : fmtQuantity(ev)}
                      {(ev.entry === "cursor_agent" || ev.entry === "responses_agents") && !ev.usage_unavailable ? <div className="mt-1 text-muted-foreground">
                        {t("输入 {input} · 输出 {output} · 缓存读 {read} · 缓存写 {write}", { input: fmtInt(ev.prompt_tokens), output: fmtInt(ev.completion_tokens), read: fmtInt(ev.cache_read_tokens), write: fmtInt(ev.cache_write_tokens) })}
                      </div> : null}
                      {ev.usage_unavailable ? <Badge variant="outline" className="ml-1" title={t("未取得完整的实际用量，本次不扣费")}>{t("未计费")}</Badge> : null}
                      {ev.estimated === true ? (
                        <Badge variant="secondary" className="ml-1" title={t("本笔用量含估算成分")}>
                          {t("含估算")}
                        </Badge>
                      ) : null}
                    </TableCell>
                    <TableCell className="font-mono tabular-nums">{`¥${api.fmtYuan(ev.cost_micro, 4)}`}</TableCell>
                    <TableCell className="text-muted-foreground font-mono tabular-nums">
                      {settle ? "—" : fmtDuration(ev.duration_ms)}
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>
      <p className="text-muted-foreground text-xs">
        {t("明细只保留最近若干条且只在内存里，设备重启后清空，也不随上方所选区间变化——落盘的是按小时聚合的账本，上方各项汇总都来自它。")}
        <br />
        {t(
          "「含估算」表示这笔用量有估算成分（客户端关掉了 usage 回传、流被中断，或厂商没给出用量）；「任务清算」是视频任务完成后补记的金额，提交时的那次请求已单独计过一笔。",
        )}
      </p>
    </Card>
  );
}

// ---- 页面 ----

export function UsagePage(): React.ReactElement {
  const [range, setRange] = useState<api.UsageRange>(usageRange);
  const [selectedKeyID, setSelectedKeyID] = useState<number | null>(usageKeyID);

  function selectKey(id: number | null): void {
    usageKeyID = id;
    setSelectedKeyID(id);
    document.querySelector('[data-slot="page-container"]')?.scrollIntoView({ block: "start" });
  }

  const res = useResource(
    () =>
      api.getUsage(range, selectedKeyID ?? undefined).then(
        (rep): { rep: api.UsageReport | null } => ({ rep }),
        (err: unknown) => {
          // 503 usage_unavailable = 计量未装配（没有 Meter 就没有数可读）。这不是故障
          // 也不是权限问题，给一句人话而不是把整页塌成一行红字。
          if (err instanceof api.ApiError && err.status === 503) return { rep: null };
          throw err;
        },
      ),
    [range, selectedKeyID],
  );

  return (
    <PageContainer wide>
      <PageHeader
        title={t("模型用量")}
        note={t("查看设备总览或单把 API 密钥的模型调用、用量、异常与消费金额。")}
        actions={
          <SegTabs
            size="sm"
            items={rangeOptions.map(([k, label]) => ({ key: k, label }))}
            active={range}
            onSelect={(k) => {
              const r = k as api.UsageRange;
              usageRange = r;
              setRange(r);
            }}
          />
        }
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {({ rep }) => {
          if (rep === null) {
            return (
              <Card className="text-muted-foreground p-6 text-sm">
                {t("用量计量当前未启用，本页暂无数据。计量随 gatewayd 一同启动，重启设备后即可看到读数。")}
              </Card>
            );
          }
          const selectedKey =
            selectedKeyID === null ? undefined : rep.keys.find((candidate) => candidate.id === selectedKeyID);
          const scopeMatches = (rep.key_id ?? null) === selectedKeyID && rep.range === range;
          return (
            <>
              <p className="text-muted-foreground text-xs">
                {!scopeMatches ? (
                  t("正在读取{range}的{scope}…", {
                    range: rangeLabel(range),
                    scope: selectedKeyID === null ? t("设备总览") : t("单 Key 统计"),
                  })
                ) : (
                  <>
                    {selectedKeyID === null
                      ? t("当前显示这台设备的全部消费；每把密钥的额度与预算已用额在「API密钥」页。")
                      : t("当前显示 {label}（{display}）的消费；额度与预算已用额在「API密钥」页。", {
                          label: keyLabel(selectedKey?.label),
                          display: selectedKey?.display ?? `Key #${selectedKeyID}`,
                        })}
                    <br />
                    {t("统计区间：{range}（{from} — {to}，按设备本地时区裁）", {
                      range: rangeLabel(rep.range),
                      from: fmtStamp(rep.from),
                      to: fmtStamp(rep.to),
                    })}
                  </>
                )}
              </p>
              <UsageScopeBar keys={rep.keys} selectedID={selectedKeyID} onSelect={selectKey} />
              {!scopeMatches ? (
                <Card className="text-muted-foreground p-6 text-sm">{t("正在读取所选统计对象…")}</Card>
              ) : (
                <>
                  <StatTiles total={rep.total} />
                  <TrendPanel rep={rep} />
                  {selectedKeyID === null ? <KeyUsagePanel rows={rep.by_key} onSelect={selectKey} /> : null}
                  <div className="grid gap-4 2xl:grid-cols-2">
                    <DimPanel title={t("按模型")} rows={rep.by_model} head={t("模型")} cell={modelCell} />
                    <DimPanel title={t("按上游账号")} rows={rep.by_upstream} head={t("上游")} cell={upstreamCell} />
                    <DimPanel
                      title={t("按协议面")}
                      rows={rep.by_entry}
                      head={t("协议面")}
                      cell={(r) => mappedCell(r, api.entryLabel, t("没有记到协议面"))}
                    />
                    <DimPanel
                      title={t("按种类")}
                      rows={rep.by_kind}
                      head={t("种类")}
                      cell={(r) => mappedCell(r, api.usageKindLabel, t("没有记到种类"))}
                    />
                  </div>
                  <RecentPanel events={rep.recent} keys={rep.keys} />
                </>
              )}
            </>
          );
        }}
      </ResourceGate>
    </PageContainer>
  );
}
