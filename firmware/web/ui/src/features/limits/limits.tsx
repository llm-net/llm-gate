// 限额录入件：API密钥页的预算 / RPM 输入与读数。照搬自管理台 views/limits.ts。
//
// 三态是这一层的全部难点，服务端 internal/admin/limits.go 与它一一对应：
//   不传该字段 = 本次不改
//   传 null    = 清除限额（回到不限）
//   传数字     = 设成这个值——**0 是合法额度**（零额度，一律拒绝），
//                与 null（不限）泾渭分明，别在任何一层把 0 当成「没设置」。
// 表单里「空输入框」就是 null；想要零额度得明明白白填一个 0。
//
// 金额一律元录入、整数微元传输（换算在 lib/api/money.ts，禁止浮点）。

import { Fragment } from "react";

import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";

/** 一次限额读入：ok 时 value 为 null 表示「不限」。 */
export type LimitResult = { ok: true; value: number | null } | { ok: false; msg: string };

/** readBudget 读金额输入框（元）。空 = 不限。label 只用于错误文案。 */
export function readBudget(text: string, label: string): LimitResult {
  const s = text.trim();
  if (s === "") return { ok: true, value: null };
  const micro = api.parseYuan(s);
  if (micro === null) {
    return {
      ok: false,
      msg: t(
        "「{label}」填得不对：请填 0 或正数，最多 {maxDecimals} 位小数，且不超过 {max} 元（留空表示不限；0 表示零额度，一律拒绝）",
        { label, maxDecimals: api.YuanInputMaxDecimals, max: api.fmtYuan(api.MaxPricingMicro) },
      ),
    };
  }
  return { ok: true, value: micro };
}

export function readRPM(text: string, label: string): LimitResult {
  const s = text.trim();
  if (s === "") return { ok: true, value: null };
  if (!/^\d+$/.test(s)) {
    return { ok: false, msg: t("「{label}」须为非负整数（留空表示不限；0 表示一律拒绝）", { label }) };
  }
  const n = Number(s);
  if (!Number.isSafeInteger(n)) return { ok: false, msg: t("「{label}」的数值过大", { label }) };
  return { ok: true, value: n };
}

/** 限额输入框的回填值：null（不限）就是空串。 */
export function budgetValue(micro: number | null): string {
  return micro === null ? "" : api.yuanText(micro);
}

export function rpmValue(n: number | null): string {
  return n === null ? "" : String(n);
}

export function LimitInput(props: React.ComponentProps<typeof Input>): React.ReactElement {
  return <Input placeholder={t("留空 = 不限")} autoComplete="off" spellCheck={false} {...props} />;
}

/** budgetText 金额读数：null = 不限，0 = 零额度（说人话，别显示成 ¥0.00 让人以为没设）。 */
export function budgetText(micro: number | null): string {
  if (micro === null) return t("不限");
  if (micro === 0) return t("零额度");
  return `¥${api.fmtYuan(micro)}`;
}

export function rpmText(n: number | null): string {
  if (n === null) return t("不限");
  if (n === 0) return t("零额度");
  return t("{n} 次/分", { n });
}

/** LimitsCell 表格里的一格限额读数：日 / 周 / 月（/ RPM）。全不限时收成一个「不限」。 */
export function LimitsCell({
  day,
  week,
  month,
  rpm,
}: {
  day: number | null;
  week: number | null;
  month: number | null;
  rpm?: number | null;
}): React.ReactElement {
  const hasBudget = day !== null || week !== null || month !== null || (rpm !== null && rpm !== undefined);
  if (!hasBudget) return <span className="text-muted-foreground">{t("不限")}</span>;
  return (
    <div className="text-xs">
      <div>
        <span className="text-muted-foreground">{t("日")} </span>
        {budgetText(day)}
      </div>
      <div>
        <span className="text-muted-foreground">{t("周")} </span>
        {budgetText(week)}
      </div>
      <div>
        <span className="text-muted-foreground">{t("月")} </span>
        {budgetText(month)}
      </div>
      {rpm === null || rpm === undefined ? null : (
        <div>
          <span className="text-muted-foreground">{t("速率")} </span>
          {rpmText(rpm)}
        </div>
      )}
    </div>
  );
}

/** 启用/禁用状态徽章（对齐 dom.ts 的 badge）。 */
export function StatusBadge({ disabled }: { disabled: boolean }): React.ReactElement {
  return (
    <Badge variant={disabled ? "secondary" : "outline"} className={cn(!disabled && "text-signal-ok")}>
      {disabled ? t("已禁用") : t("正常")}
    </Badge>
  );
}

/** 两页共用的说明：预算只挡新请求，不掐断在途的流与已提交的任务。 */
export const budgetNote = t(
  "预算按设备本地时区的自然日 / 自然周（周一起算）/ 自然月计算；预算用完后，有按量额度就继续使用并从中扣减，额度也用完才拒绝新请求（429）。已在进行的流式响应与已提交的视频任务不受影响，照常完成并入账。留空 = 不限，填 0 = 零额度。",
);

/** 仪表上的一格窗口：已消费额 + 预算（null = 不限，0 = 零额度）。 */
export interface BudgetWindow {
  label: string;
  /** 恒为实数——「计量未装配」由调用方改用 LimitsCell 表达，别在这里用 null 兼职，
   *  那会让「没花钱」和「没接线」共用一种画法。 */
  spentMicro: number;
  limitMicro: number | null;
}

/**
 * 三窗仪表（密钥页的「消费 / 预算」列）：今日 / 本周 / 本月各一条微型进度条，
 * 读数写「已消费 / 预算」。三条的标签、条、读数各占一列，刻度因此左右对齐
 * ——一眼看得出哪一档先见底。
 *
 * 严重度：常态、≥80% 琥珀、打满转红，判据是**整数比较**（已用 × 5 ≥ 额度 × 4），
 * 不引浮点。不限额的窗口不画槽、只留一条虚线基准——那一档没有刻度可读，画成
 * 空槽会被读成「用了 0%」，暗示有个看不见的上限。
 *
 * 全不限且无消费时收成一个「不限」：那种行没有仪表可读，画三条空刻度只是噪音
 * （同 LimitsCell 的收拢规则）。
 */
export function BudgetGauge({ windows }: { windows: BudgetWindow[] }): React.ReactElement {
  const live = windows.some((w) => w.limitMicro !== null || w.spentMicro > 0);
  if (!live) return <span className="text-muted-foreground">{t("不限")}</span>;
  return (
    <div className="grid grid-cols-[2.5rem_5rem_1fr] items-center gap-x-2 gap-y-1 text-xs">
      {windows.map((w) => {
        const limit = w.limitMicro;
        const over = limit !== null && w.spentMicro >= limit;
        const near = !over && limit !== null && limit > 0 && w.spentMicro * 5 >= limit * 4;
        const pct = limit === null ? 0 : limit > 0 ? Math.min(100, (w.spentMicro / limit) * 100) : 100;
        return (
          <Fragment key={w.label}>
            <span className="text-muted-foreground">{w.label}</span>
            {limit === null ? (
              // 不限额：一条虚线基准，无槽无填充。
              <span className="border-muted-foreground/40 block border-t border-dashed" />
            ) : (
              <span className="bg-muted block h-1.5 overflow-hidden rounded-full">
                <i
                  className={cn(
                    "block h-full rounded-full",
                    over ? "bg-signal-danger" : near ? "bg-signal-alert" : "bg-primary",
                  )}
                  style={{ width: `${pct.toFixed(1)}%` }}
                />
              </span>
            )}
            <span className={cn("tabular-nums", over ? "text-signal-danger" : near ? "text-signal-alert" : "")}>
              {/* 已消费在前、主色；预算在后、灰色——这一格的主语是「花了多少」，
                  额度只是参照。严重度只染前半截。 */}
              {api.fmtMoney(w.spentMicro)}
              <span className="text-muted-foreground"> / {budgetText(limit)}</span>
            </span>
          </Fragment>
        );
      })}
    </div>
  );
}
