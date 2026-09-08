// 订阅模型列：「开发工具订阅」页的右列，一张按订阅分组的目录价表。
//
// 模型和价格由目录数据定义，连接订阅后自动显示。共享目录行由服务端收敛器维护，
// Cursor 直接读取平台目录。整列只读，不提供添加、修改或删除价格的动作。
//
// 分组顺序与左列订阅卡同一份 PROVIDERS；组行右端是那份订阅此刻的状态灯（与左列同一个
// 件）：型号能不能被成员调到，先看订阅本身通不通。默认模型 / 对成员可见的模型在行上
// 打标，读数与左列卡片同源（account.default_model）。Cursor 的计价清单单独从
// 平台目录读取并放进同一张表，可用模型仍由 cursor-agent 经订阅面自行发现。
//
// 状态徽章只标「需要操作者处理的状态」：已禁用、未定价。正常态不标。

import { Fragment } from "react";

import { AgentProviderIcon } from "@/components/brand-icon";
import { Badge } from "@/components/ui/badge";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";

import { PROVIDERS, StatusPill, visibleModelSemantics } from "./agent-status";
import { agentPricingModel } from "./domain";

type PricingModel = Pick<api.Model, "name" | "pricing" | "disabled">;

interface Group {
  provider: api.AgentProvider;
  account: api.AgentAccount | undefined;
  models: PricingModel[];
}

const headCell = "text-muted-foreground h-9 text-xs font-medium";

function ModelLine({
  m,
  provider,
  account,
  fields,
}: {
  m: PricingModel;
  provider: api.AgentProvider;
  account: api.AgentAccount | undefined;
  fields: api.PricingField[];
}): React.ReactElement {
  const pricing = m.pricing ?? {};
  const priced = fields.some((f) => pricing[f.name] !== undefined);
  // 与左列卡片上的「默认模型 / 可见模型」同一份读数；名字对得上才打标。
  const marked = provider !== "cursor" && account !== undefined && account.default_model !== "" && account.default_model === m.name;
  return (
    <TableRow className={cn(m.disabled && "opacity-60")}>
      <TableCell className="pl-4">
        <div className="flex flex-wrap items-center gap-2">
          <code className="font-mono text-[13px] leading-5" title={m.name}>
            {m.name}
          </code>
          {marked ? (
            <Badge
              variant="secondary"
              title={
                visibleModelSemantics(provider)
                  ? t("对成员可见的模型：成员的 Claude Code 只看得到它")
                  : t("写进成员 CLI 配置的默认模型")
              }
            >
              {visibleModelSemantics(provider) ? t("成员可见") : t("默认")}
            </Badge>
          ) : null}
          {m.disabled ? <Badge variant="secondary">{t("已禁用")}</Badge> : null}
          {priced ? null : (
            // 未定价的模型照常转发，但每一次调用都记 0 元，账上看不出这台设备在替谁花钱
            // ——这是需要操作者处理的状态，用警示色而非熄灭色。
            <Badge
              variant="outline"
              className="text-signal-alert"
              title={t(
                "尚未取到官方目录价：调用照常，但每次的名义金额都记 0 元，用量页上看不到它的花费——设备会自动执行数据升级，也可到「设备更新 → 数据升级」点「立即更新」取一次",
              )}
            >
              {t("未定价")}
            </Badge>
          )}
        </div>
      </TableCell>
      {fields.map((f, i) => {
        const v = pricing[f.name];
        return (
          <TableCell
            key={f.name}
            className={cn("text-right font-mono text-xs tabular-nums", i === fields.length - 1 && "pr-4")}
          >
            {v === undefined ? <span className="text-muted-foreground">—</span> : api.fmtMoney(v)}
          </TableCell>
        );
      })}
    </TableRow>
  );
}

export function AgentModelsColumn({
  all,
  accounts,
  cursorPrices,
}: {
  all: api.Model[];
  accounts: api.AgentAccount[];
  cursorPrices: api.CursorPricingView;
}): React.ReactElement {
  const models = all.filter(agentPricingModel);
  // 目录只收文本模型，价目使用输入、输出与缓存读写四档；列头文案与录价表单同一份字段规格。
  const fields = api.pricingFieldsFor("text");
  const groups: Group[] = PROVIDERS.map((p) => ({
    provider: p,
    account: accounts.find((a) => a.provider === p),
    models: p === "cursor"
      ? cursorPrices.models.map((m) => ({ ...m, disabled: false }))
      : models.filter((m) => m.agent === p),
  })).filter((g) => g.models.length > 0);
  const modelCount = groups.reduce((count, group) => count + group.models.length, 0);

  return (
    <>
      <div className="flex flex-col gap-1 max-lg:mt-3 lg:col-start-2 lg:row-start-1">
        <div className="flex min-h-8 flex-wrap items-center gap-2">
          <h2 className="text-base font-semibold">{t("模型")}</h2>
          <Badge variant="secondary" title={t("共 {n} 个模型", { n: modelCount })}>
            {modelCount}
          </Badge>
        </div>
        <p className="text-muted-foreground text-xs">
          {t("订阅文本按目录价格记名义金额，模型和价格自动同步且只读；图片/视频无按量费。")}
        </p>
      </div>
      <div className="lg:col-start-2 lg:row-start-2">
        {groups.length === 0 ? (
          <p className="text-muted-foreground rounded-xl border border-dashed px-5 py-8 text-center text-sm leading-relaxed">
            {t(
              "暂无订阅目录模型。在左列连上 Codex、Grok Build、Claude Code 或 Cursor 订阅后，目录中的文本模型价格会自动出现在这里。",
            )}
          </p>
        ) : (
          <div className="bg-card overflow-hidden rounded-xl border shadow-xs">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className={cn(headCell, "pl-4")}>{t("模型")}</TableHead>
                  {fields.map((f, i) => (
                    <TableHead
                      key={f.name}
                      className={cn(headCell, "w-24 text-right", i === fields.length - 1 && "pr-4")}
                    >
                      {f.label}
                    </TableHead>
                  ))}
                </TableRow>
              </TableHeader>
              <TableBody>
                {groups.map((g) => (
                  <Fragment key={g.provider}>
                    <TableRow className="bg-muted/40 hover:bg-muted/40">
                      <TableCell colSpan={fields.length + 1} className="px-4 py-2">
                        <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1">
                          <AgentProviderIcon provider={g.provider} size="tab" />
                          <span className="text-sm font-semibold">{api.agentProviderLabel(g.provider)}</span>
                          <span className="text-muted-foreground text-xs">
                            {t("{n} 个模型", { n: g.models.length })}
                          </span>
                          {g.account === undefined ? null : (
                            <span className="ml-auto">
                              <StatusPill a={g.account} />
                            </span>
                          )}
                        </div>
                      </TableCell>
                    </TableRow>
                    {g.models.map((m) => (
                      <ModelLine key={m.name} m={m} provider={g.provider} account={g.account} fields={fields} />
                    ))}
                  </Fragment>
                ))}
              </TableBody>
            </Table>
            <p className="text-muted-foreground border-t px-4 py-2 text-[11px] leading-4">
              {t("目录价单位：元 / 百万 token；未列缓存命中价的模型按输入价计。")}
            </p>
          </div>
        )}
      </div>
    </>
  );
}
