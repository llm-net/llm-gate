// 设备状态页（仅 admin）：处理器（分大小核）/内存/网络/温度/存储 的仪表读数面板，
// 外加一条与登录卡规格铭牌同源的整机读数条。
//
// **资源纪律**：进入页面只发一次 GET /admin/v1/system，**绝不轮询**；「刷新读数」
// 由人按，服务端还有 2s 结果缓存兜底，连点也只采样一次。
//
// 视觉语义：仪表条的填充色承载严重度——常态、≥80% 琥珀（需留意）、≥92% 红（接近
// 打满）；数字一律穿墨色不穿状态色（色觉无关可读）。逐核小柱不做变色：单核打满是
// 正常工作形态，严重度只在「整簇/整机容量」层面才有意义。
//
// 走势图是**手绘 SVG，零图表库**：一格一指标的小型记录仪阵列，均值实线 + 峰值细线
// （仅归档层）+ 面积淡染。文字全部在 HTML 层（viewBox 被拉伸，文字进去会变形）。
// **采样断档（设备关机）在图上留缝，不做跨缝连线**——这条是换图表库就会丢的东西。

import { useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { SegTabs } from "@/features/access/access";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { t, tx } from "@/lib/i18n";
import {
  clampPct,
  fmtBytes,
  fmtClock,
  fmtPct,
  fmtRate,
  fmtUptime,
  fmtWindow,
  ratio,
} from "@/lib/units";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

// 跨刷新记住上次选的范围（模块级，会话内有效）。
let historyRange: api.HistoryRange = "6h";

const CHART_W = 600;
const CHART_H = 100;

// ---- 仪表 ----

// 一行仪表：可选标签 + 度量条 + 右侧读数。填充色按严重度切换。
function MeterRow({ label, pct, valText }: { label?: string; pct: number; valText: string }) {
  const p = clampPct(pct);
  return (
    <div className="flex items-center gap-2 text-xs">
      {label === undefined ? null : <span className="text-muted-foreground w-12 shrink-0">{label}</span>}
      <span className="bg-muted block h-1.5 flex-1 overflow-hidden rounded-full">
        <i
          className={cn(
            "block h-full rounded-full",
            p >= 92 ? "bg-signal-danger" : p >= 80 ? "bg-signal-alert" : "bg-primary",
          )}
          style={{ width: `${p}%` }}
        />
      </span>
      <span className="w-14 shrink-0 text-right font-mono tabular-nums">{valText}</span>
    </div>
  );
}

function Panel({ title, aside, children }: { title: string; aside?: React.ReactNode; children: React.ReactNode }) {
  return (
    <Card className="gap-3 p-5">
      <div className="flex items-center gap-3">
        <h2 className="font-semibold">{title}</h2>
        {aside === undefined ? null : <div className="text-muted-foreground ml-auto text-xs">{aside}</div>}
      </div>
      {children}
    </Card>
  );
}

// ---- 处理器 ----

function ClusterBlock({ cluster }: { cluster: api.CpuCluster }) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-baseline gap-2 text-xs">
        <b>{cluster.label}</b>
        <span className="text-muted-foreground">
          {`×${cluster.cores.length}${cluster.core_model !== undefined ? ` · ${cluster.core_model}` : ""}`}
        </span>
        {cluster.cur_freq_mhz !== undefined && cluster.max_freq_mhz !== undefined ? (
          <span className="text-muted-foreground ml-auto font-mono" title={t("当前 / 最高频率")}>
            {`${cluster.cur_freq_mhz} / ${cluster.max_freq_mhz} MHz`}
          </span>
        ) : null}
      </div>
      <MeterRow pct={cluster.percent} valText={fmtPct(cluster.percent)} />
      {/* 逐核小柱不变色：单核打满是正常工作形态。 */}
      <div className="flex h-6 items-end gap-1">
        {cluster.cores.map((c) => (
          <span
            key={c.cpu}
            title={`CPU${c.cpu} · ${fmtPct(c.percent)}`}
            className="bg-muted flex h-full w-2 items-end overflow-hidden rounded-sm"
          >
            <i className="bg-primary/70 block w-full" style={{ height: `${clampPct(c.percent)}%` }} />
          </span>
        ))}
      </div>
    </div>
  );
}

// 可选属性显式带 undefined：本仓 exactOptionalPropertyTypes: true，
// 「没传这个属性」与「传了 undefined」是两回事，透传上游可选字段时要写成联合类型。
function CpuPanel({ cpu, loadAvg }: { cpu: api.CpuStatus; loadAvg?: number[] | undefined }) {
  const coreCount = cpu.clusters.reduce((n, c) => n + c.cores.length, 0);
  return (
    <Panel title={t("处理器")} aside={<span className="font-mono">{t("{n} 核", { n: coreCount })}</span>}>
      <MeterRow label={t("总占用")} pct={cpu.overall_percent} valText={fmtPct(cpu.overall_percent)} />
      {cpu.clusters.map((c) => (
        <ClusterBlock key={c.label} cluster={c} />
      ))}
      {loadAvg !== undefined && loadAvg.length === 3 ? (
        <p className="text-muted-foreground text-xs">
          {tx("负载 {load}（1 / 5 / 15 分钟）", {
            load: <span className="font-mono">{loadAvg.map((v) => v.toFixed(2)).join(" / ")}</span>,
          })}
        </p>
      ) : null}
    </Panel>
  );
}

// ---- 内存 / 存储 ----

function MemoryPanel({ mem }: { mem: api.MemoryStatus }) {
  const pct = ratio(mem.used_bytes, mem.total_bytes);
  const swapPct = ratio(mem.swap_used_bytes, mem.swap_total_bytes);
  return (
    <Panel title={t("内存")}>
      <MeterRow label={t("已用")} pct={pct} valText={fmtPct(pct)} />
      <p className="text-muted-foreground text-xs">
        <span className="font-mono">{`${fmtBytes(mem.used_bytes)} / ${fmtBytes(mem.total_bytes)}`}</span>
        {" · "}
        {t("可用 {avail} · 缓冲/缓存 {cache}", {
          avail: fmtBytes(mem.available_bytes),
          cache: fmtBytes(mem.buff_cache_bytes),
        })}
      </p>
      {mem.swap_total_bytes > 0 ? (
        <>
          <MeterRow label={t("交换区")} pct={swapPct} valText={fmtPct(swapPct)} />
          <p className="text-muted-foreground font-mono text-xs">
            {`${fmtBytes(mem.swap_used_bytes)} / ${fmtBytes(mem.swap_total_bytes)}`}
          </p>
        </>
      ) : null}
    </Panel>
  );
}

function DiskPanel({ disk }: { disk: api.DiskStatus }) {
  const pct = ratio(disk.used_bytes, disk.total_bytes);
  return (
    <Panel title={t("存储")} aside={<span className="font-mono" title={t("挂载点")}>{disk.mount}</span>}>
      <MeterRow label={t("已用")} pct={pct} valText={fmtPct(pct)} />
      <p className="text-muted-foreground text-xs">
        <span className="font-mono">{`${fmtBytes(disk.used_bytes)} / ${fmtBytes(disk.total_bytes)}`}</span>
        {" · "}
        {t("可写入 {avail}", { avail: fmtBytes(disk.avail_bytes) })}
      </p>
    </Panel>
  );
}

// ---- 网络 ----

// 每块物理网卡一行：连接状态沿用 LED 徽章语义（灰 = 未插线，是「未点亮」不是
// 故障），速率为采样窗内的均速。
function NetworkPanel({ ifaces }: { ifaces: api.NetIface[] }) {
  return (
    <Panel title={t("网络")}>
      {ifaces.map((n) => (
        <div key={n.name} className="flex flex-col gap-0.5">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <code className="font-mono">{n.name}</code>
            <Badge variant={n.up ? "outline" : "secondary"} className={cn(n.up && "text-signal-ok")}>
              {n.up ? t("已连接") : t("未连接")}
            </Badge>
            <span className="text-muted-foreground ml-auto flex gap-3 font-mono" title={t("采样窗内的平均速率")}>
              <span>{`↓ ${fmtRate(n.rx_bytes_per_sec)}`}</span>
              <span>{`↑ ${fmtRate(n.tx_bytes_per_sec)}`}</span>
            </span>
          </div>
          <p className="text-muted-foreground text-xs">
            {t("累计接收 {recv} · 发送 {sent}", {
              recv: fmtBytes(n.rx_total_bytes),
              sent: fmtBytes(n.tx_total_bytes),
            })}
          </p>
        </div>
      ))}
    </Panel>
  );
}

// ---- 温度 ----

// 数字恒为墨色；超温只点一颗 LED 圆点（琥珀 ≥70℃ 需留意、红 ≥85℃ 过热），
// 常温不点灯——绿点阵列是噪音。
function TempDot({ celsius }: { celsius: number }) {
  if (celsius >= 85) return <i className="bg-signal-danger inline-block size-1.5 rounded-full" title={t("过热")} />;
  if (celsius >= 70) return <i className="bg-signal-alert inline-block size-1.5 rounded-full" title={t("温度偏高")} />;
  return null;
}

function TemperaturePanel({ temps }: { temps: api.TempZone[] }) {
  return (
    <Panel title={t("温度")}>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        {temps.map((z) => (
          <div key={z.label} className="flex flex-col gap-0.5">
            <span className="text-muted-foreground flex items-center gap-1 text-xs">
              <TempDot celsius={z.celsius} />
              {z.label}
            </span>
            <b className="font-mono tabular-nums">
              {z.celsius.toFixed(1)}
              <span className="text-muted-foreground text-xs font-normal"> ℃</span>
            </b>
          </div>
        ))}
      </div>
    </Panel>
  );
}

// ---- 历史趋势：手绘 SVG ----

interface SeriesPoint {
  t: number; // 毫秒时间戳
  avg: number;
  max: number;
}

type ChartUnit = "%" | "℃" | "rate";

interface TileSpec {
  title: string;
  unit: ChartUnit;
  series: SeriesPoint[];
}

function fmtChartVal(v: number, unit: ChartUnit): string {
  switch (unit) {
    case "%":
      return `${v.toFixed(1)}%`;
    case "℃":
      return `${v.toFixed(1)} ℃`;
    case "rate":
      return fmtRate(v);
  }
}

function fmtChartAxis(v: number, unit: ChartUnit): string {
  switch (unit) {
    case "%":
      return `${Math.round(v)}%`;
    case "℃":
      return `${Math.round(v)}°`;
    case "rate":
      return fmtBytes(v);
  }
}

function fmtChartTime(ms: number, rng: api.HistoryRange): string {
  const d = new Date(ms);
  const p = (n: number): string => String(n).padStart(2, "0");
  const hm = `${p(d.getHours())}:${p(d.getMinutes())}`;
  return rng === "7d" ? `${d.getMonth() + 1}-${d.getDate()} ${hm}` : hm;
}

function extract(
  points: api.HistoryPoint[],
  pick: (p: api.HistoryPoint) => api.HistoryMetric | undefined,
): SeriesPoint[] {
  const out: SeriesPoint[] = [];
  for (const p of points) {
    const m = pick(p);
    if (m === undefined) continue;
    const ms = Date.parse(p.t);
    if (Number.isNaN(ms)) continue;
    out.push({ t: ms, avg: m.avg, max: m.max });
  }
  return out;
}

function clusterLabels(points: api.HistoryPoint[]): string[] {
  const seen = new Set<string>();
  const order: string[] = [];
  for (const p of points) {
    for (const c of p.clusters ?? []) {
      if (!seen.has(c.label)) {
        seen.add(c.label);
        order.push(c.label);
      }
    }
  }
  return order;
}

// chipTemp 每点取硅片各区（机身表面除外）的最高温。
function chipTemp(p: api.HistoryPoint): api.HistoryMetric | undefined {
  let avg = -Infinity;
  let max = -Infinity;
  for (const z of p.temps ?? []) {
    // 机身表面靠服务端给的语言无关标记排除，不比对文案（label 已按语言本地化）。
    if (z.surface === true) continue;
    if (z.avg > avg) avg = z.avg;
    if (z.max > max) max = z.max;
  }
  return avg === -Infinity ? undefined : { avg, max };
}

// netSum 全部物理网卡合计。
function netSum(p: api.HistoryPoint, dir: "rx" | "tx"): api.HistoryMetric | undefined {
  const list = p.net ?? [];
  if (list.length === 0) return undefined;
  let avg = 0;
  let max = 0;
  for (const n of list) {
    avg += dir === "rx" ? n.rx_avg : n.tx_avg;
    max += dir === "rx" ? n.rx_max : n.tx_max;
  }
  return { avg, max };
}

// 一格指标小图：标题 + 数字读数（悬停变游标读数）+ SVG 走势 + 首末时刻标尺。
function ChartTile({
  spec,
  intervalMs,
  showMax,
  rng,
}: {
  spec: TileSpec;
  intervalMs: number;
  showMax: boolean;
  rng: api.HistoryRange;
}) {
  const [hover, setHover] = useState<SeriesPoint | null>(null);
  const s = spec.series;
  const first = s[0];
  const last = s[s.length - 1];
  if (s.length < 2 || first === undefined || last === undefined) {
    return (
      <div className="flex flex-col gap-1">
        <span className="text-xs font-medium">{spec.title}</span>
        <div className="text-muted-foreground bg-muted/30 flex h-[100px] items-center justify-center rounded-md text-xs">
          {t("数据积累中…")}
        </div>
      </div>
    );
  }

  // 值域：百分比锁 0–100（跨图可比），温度贴数据留 2° 呼吸，速率从 0 起。
  let yMin = 0;
  let yMax = 100;
  if (spec.unit === "℃") {
    yMin = Math.floor(Math.min(...s.map((p) => p.avg))) - 2;
    yMax = Math.ceil(Math.max(...s.map((p) => p.max))) + 2;
  } else if (spec.unit === "rate") {
    yMax = Math.max(1024, Math.max(...s.map((p) => p.max)) * 1.05);
  }
  const spanT = Math.max(1, last.t - first.t);
  const x = (ms: number): number => ((ms - first.t) / spanT) * CHART_W;
  const y = (v: number): number =>
    CHART_H - ((Math.min(yMax, Math.max(yMin, v)) - yMin) / (yMax - yMin)) * CHART_H;

  // **断档成缝**：间隔超过采样周期 2.5 倍即断开，不跨缝连线。
  const segments: SeriesPoint[][] = [];
  let cur: SeriesPoint[] = [];
  let prevT: number | null = null;
  for (const p of s) {
    if (prevT !== null && p.t - prevT > intervalMs * 2.5 && cur.length > 0) {
      segments.push(cur);
      cur = [];
    }
    cur.push(p);
    prevT = p.t;
  }
  if (cur.length > 0) segments.push(cur);

  const hasMaxLine = showMax && s.some((p) => p.max - p.avg > 0.05);
  const shown = hover ?? last;
  const readout =
    hasMaxLine && shown.max - shown.avg > 0.05
      ? t("{avg} · 峰 {max}", {
          avg: fmtChartVal(shown.avg, spec.unit),
          max: fmtChartVal(shown.max, spec.unit),
        })
      : fmtChartVal(shown.avg, spec.unit);

  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-baseline gap-2">
        <span className="text-xs font-medium">{spec.title}</span>
        <span className="ml-auto flex items-baseline gap-1.5">
          <b className="font-mono text-xs tabular-nums">{readout}</b>
          <span className="text-muted-foreground text-[10px]">{fmtChartTime(shown.t, rng)}</span>
        </span>
      </div>
      <div
        className="relative"
        onPointerMove={(ev) => {
          const rect = ev.currentTarget.getBoundingClientRect();
          if (rect.width <= 0) return;
          const at = first.t + ((ev.clientX - rect.left) / rect.width) * spanT;
          let nearest = last;
          let best = Infinity;
          for (const p of s) {
            const d = Math.abs(p.t - at);
            if (d < best) {
              best = d;
              nearest = p;
            }
          }
          setHover(nearest);
        }}
        onPointerLeave={() => setHover(null)}
      >
        <svg
          viewBox={`0 0 ${CHART_W} ${CHART_H}`}
          preserveAspectRatio="none"
          className="bg-muted/30 h-[100px] w-full rounded-md"
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
          {segments.map((seg, i) => {
            const segFirst = seg[0];
            const segLast = seg[seg.length - 1];
            if (segFirst === undefined || segLast === undefined) return null;
            if (seg.length === 1) {
              // 孤点（前后都是断档）画一颗小圆，线画不出来。
              return (
                <circle
                  key={i}
                  cx={x(segFirst.t)}
                  cy={y(segFirst.avg)}
                  r="2.5"
                  className="fill-primary"
                />
              );
            }
            const avgPts = seg.map((p) => `${x(p.t).toFixed(2)},${y(p.avg).toFixed(2)}`);
            return (
              <g key={i}>
                <path
                  d={`M ${x(segFirst.t).toFixed(2)},${CHART_H} L ${avgPts.join(" L ")} L ${x(segLast.t).toFixed(2)},${CHART_H} Z`}
                  className="fill-primary/8"
                />
                {hasMaxLine ? (
                  <path
                    d={`M ${seg.map((p) => `${x(p.t).toFixed(2)},${y(p.max).toFixed(2)}`).join(" L ")}`}
                    className="stroke-primary/40"
                    fill="none"
                    strokeWidth={1}
                    vectorEffect="non-scaling-stroke"
                  />
                ) : null}
                <path
                  d={`M ${avgPts.join(" L ")}`}
                  className="stroke-primary"
                  fill="none"
                  strokeWidth={1.5}
                  vectorEffect="non-scaling-stroke"
                />
              </g>
            );
          })}
          {hover === null ? null : (
            <line
              x1={x(hover.t)}
              y1="0"
              x2={x(hover.t)}
              y2={CHART_H}
              className="stroke-foreground/40"
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
            />
          )}
        </svg>
        {/* 文字全在 HTML 层：viewBox 被拉伸，文字进 SVG 会变形。 */}
        <span className="text-muted-foreground absolute top-0.5 left-1 text-[10px]">
          {fmtChartAxis(yMax, spec.unit)}
        </span>
        <span className="text-muted-foreground absolute bottom-0.5 left-1 text-[10px]">
          {fmtChartAxis(yMin, spec.unit)}
        </span>
      </div>
      <div className="text-muted-foreground flex justify-between text-[10px]">
        <span>{fmtChartTime(first.t, rng)}</span>
        <span>{fmtChartTime(last.t, rng)}</span>
      </div>
    </div>
  );
}

function ChartGrid({ hist }: { hist: api.SystemHistory }) {
  const pts = hist.points;
  const tiles: TileSpec[] = [{ title: t("CPU 总占用"), unit: "%", series: extract(pts, (p) => p.cpu) }];
  const labels = clusterLabels(pts);
  if (labels.length >= 2) {
    for (const label of labels) {
      tiles.push({
        title: t("{label}占用", { label }),
        unit: "%",
        series: extract(pts, (p) => (p.clusters ?? []).find((c) => c.label === label)),
      });
    }
  }
  tiles.push({ title: t("内存占用"), unit: "%", series: extract(pts, (p) => p.memory_percent) });
  tiles.push({ title: t("芯片温度（最高）"), unit: "℃", series: extract(pts, chipTemp) });
  tiles.push({ title: t("网络接收"), unit: "rate", series: extract(pts, (p) => netSum(p, "rx")) });
  tiles.push({ title: t("网络发送"), unit: "rate", series: extract(pts, (p) => netSum(p, "tx")) });
  tiles.push({ title: t("存储占用"), unit: "%", series: extract(pts, (p) => p.disk_percent) });

  const present = tiles.filter((tile) => tile.series.length > 0);
  if (present.length === 0) {
    return (
      <p className="text-muted-foreground text-xs">
        {t("还没有可绘制的数据：采样每分钟进行一次，稍后回来即可看到走势。")}
      </p>
    );
  }
  const showMax = hist.range === "7d";
  return (
    <div className="flex flex-col gap-3">
      {/* 归档层图例：均值/峰值两键（单指标双影线，不是两个系列，图例只此一处）。 */}
      {showMax ? (
        <p className="text-muted-foreground flex items-center gap-3 text-xs">
          <span className="flex items-center gap-1.5">
            <i className="bg-primary inline-block h-0.5 w-4" />
            {t("均值")}
          </span>
          <span className="flex items-center gap-1.5">
            <i className="bg-primary/40 inline-block h-0.5 w-4" />
            {t("峰值（10 分钟窗口内）")}
          </span>
        </p>
      ) : null}
      <div className="grid gap-x-6 gap-y-4 sm:grid-cols-2 xl:grid-cols-3">
        {present.map((tile) => (
          <ChartTile
            key={tile.title}
            spec={tile}
            intervalMs={hist.interval_seconds * 1000}
            showMax={showMax}
            rng={hist.range}
          />
        ))}
      </div>
    </div>
  );
}

// 范围切换只重取历史序列并原位重绘，不动整页。
function HistoryPanel({ initial }: { initial: api.SystemHistory | null }) {
  const [range, setRange] = useState<api.HistoryRange>(historyRange);
  const [hist, setHist] = useState<api.SystemHistory | null>(initial);
  const [loading, setLoading] = useState(false);

  function switchRange(r: api.HistoryRange): void {
    if (r === range) return;
    historyRange = r;
    setRange(r);
    setLoading(true);
    api.getSystemHistory(r).then(
      (res) => {
        setLoading(false);
        setHist(res.history);
      },
      () => {
        setLoading(false);
        setHist(null);
      },
    );
  }

  const retention =
    initial !== null && initial.retention_days > 0 ? t("近 {n} 天", { n: initial.retention_days }) : t("近 7 天");
  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="font-semibold">{t("历史趋势")}</h2>
        <SegTabs
          items={[
            { key: "6h", label: t("近 6 小时") },
            { key: "7d", label: retention },
          ]}
          active={range}
          onSelect={(k) => switchRange(k === "7d" ? "7d" : "6h")}
        />
        {hist !== null && hist.enabled ? (
          <span className="text-muted-foreground ml-auto text-xs">
            {t("每分钟采样 · 每 10 分钟归档 · 保留 {n} 天", { n: hist.retention_days })}
          </span>
        ) : null}
      </div>
      {loading ? (
        <p className="text-muted-foreground text-xs">{t("加载中…")}</p>
      ) : hist === null ? (
        <p className="text-destructive text-xs">{t("历史数据加载失败，请刷新页面重试。")}</p>
      ) : !hist.enabled ? (
        <p className="text-muted-foreground text-xs">
          {t("历史记录已关闭（配置 history_days: 0）。打开后每分钟采样、每 10 分钟归档一行。")}
        </p>
      ) : (
        <ChartGrid hist={hist} />
      )}
    </Card>
  );
}

// ---- 整机读数条 ----

// 设备型号来自服务端的本机 boardinfo 型号档案；固件版本来自 buildinfo。
// 两者都没有前端常量兜底，读不到时宁可不显示。
function StatusBar({
  sys,
  fwVersion,
  hardwareModel,
}: {
  sys: api.SystemStatus;
  fwVersion: string;
  hardwareModel: string;
}) {
  const item = (k: string, v: React.ReactNode) => (
    <span key={k} className="flex items-baseline gap-1.5">
      <span className="text-muted-foreground text-xs">{k}</span>
      <span className="font-mono text-xs">{v}</span>
    </span>
  );
  return (
    <Card className="flex-row flex-wrap items-baseline gap-x-6 gap-y-1 p-4">
      {hardwareModel === "" ? null : item(t("设备型号"), hardwareModel)}
      {fwVersion === "" ? null : item(t("固件版本"), fwVersion)}
      {sys.uptime_seconds !== undefined && sys.uptime_seconds > 0
        ? item(t("已运行"), fmtUptime(sys.uptime_seconds))
        : null}
      {item(
        t("采样于"),
        t("{at} · 窗口 {window}", { at: fmtClock(sys.sampled_at), window: fmtWindow(sys.window_ms) }),
      )}
    </Card>
  );
}

export function StatusPage(): React.ReactElement {
  // 即时快照与历史序列并行取；历史失败不拖垮整页，面板内给错误文案。
  const res = useResource(
    () =>
      Promise.all([
        api.getSystemStatus(),
        api
          .getSystemHistory(historyRange)
          .then((r): api.SystemHistory | null => r.history)
          .catch(() => null),
      ]).then(([snap, history]) => ({ snap, history })),
    [],
  );

  return (
    <PageContainer>
      <PageHeader
        title={t("设备状态")}
        note={t("查看设备资源、运行状态与历史趋势。")}
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {({ snap, history }) => {
          const sys = snap.system;
          const panels: React.ReactNode[] = [];
          if (sys.cpu !== undefined) panels.push(<CpuPanel key="cpu" cpu={sys.cpu} loadAvg={sys.load_avg} />);
          if (sys.memory !== undefined) panels.push(<MemoryPanel key="mem" mem={sys.memory} />);
          if (sys.network !== undefined && sys.network.length > 0)
            panels.push(<NetworkPanel key="net" ifaces={sys.network} />);
          if (sys.temperatures !== undefined && sys.temperatures.length > 0)
            panels.push(<TemperaturePanel key="temp" temps={sys.temperatures} />);
          if (sys.disk !== undefined) panels.push(<DiskPanel key="disk" disk={sys.disk} />);
          return (
            <>
              <StatusBar
                sys={sys}
                fwVersion={snap.firmwareVersion}
                hardwareModel={snap.hardwareModel}
              />
              {panels.length === 0 ? (
                <Card className="text-muted-foreground p-6 text-sm">
                  {t("当前运行平台不提供系统读数（开发环境无 /proc）；部署到设备上即可看到完整仪表。")}
                </Card>
              ) : (
                <div className="grid gap-4 lg:grid-cols-2">{panels}</div>
              )}
              <HistoryPanel initial={history} />
            </>
          );
        }}
      </ResourceGate>
    </PageContainer>
  );
}
