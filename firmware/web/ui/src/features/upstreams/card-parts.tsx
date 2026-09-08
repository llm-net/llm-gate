// 模型接入页共用件：账号与模型搜索、状态灯徽章、订阅卡片事实带。

import { Search, X } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";

/** 账号与模型列表的本地过滤；不发请求，不保存搜索内容。 */
export function CatalogSearch({
  value,
  onChange,
  label,
}: {
  value: string;
  onChange: (value: string) => void;
  label: string;
}): React.ReactElement {
  return (
    <div className="relative mt-auto">
      <Search className="text-muted-foreground pointer-events-none absolute top-1/2 left-2 size-3.5 -translate-y-1/2" aria-hidden="true" />
      <Input
        type="search"
        value={value}
        onChange={(ev) => onChange(ev.target.value)}
        placeholder={label}
        aria-label={label}
        className="bg-card h-7 pr-8 pl-7 text-xs shadow-none md:text-xs [&::-webkit-search-cancel-button]:appearance-none"
      />
      {value === "" ? null : (
        <Button
          type="button"
          size="icon-xs"
          variant="ghost"
          className="absolute top-1/2 right-1 -translate-y-1/2"
          aria-label={t("清除搜索")}
          onClick={() => onChange("")}
        >
          <X aria-hidden="true" />
        </Button>
      )}
    </div>
  );
}

/** 状态灯徽章：一粒色点 + 那句话本身。颜色从不单独承担语义，灯旁恒有文字。 */
export function Pill({
  dot,
  className,
  children,
}: {
  dot: string;
  className?: string;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <Badge variant="outline" className={cn("gap-1.5", className)}>
      <span aria-hidden="true" className={cn("size-1.5 shrink-0 rounded-full", dot)} />
      {children}
    </Badge>
  );
}

export interface Fact {
  label: string;
  value: React.ReactNode;
}

/**
 * 卡片事实带：sm 起横排成格（标签在上、读数在下，格间竖线）；再窄就一格一行、标签左
 * 读数右——三格并排在手机上会把时间戳截成半截。读数一律单行截断，全文挂在读数自己的
 * title 上。
 */
export function FactStrip({ facts, dim = false }: { facts: Fact[]; dim?: boolean }): React.ReactElement {
  return (
    <dl
      className={cn(
        "bg-muted/30 grid divide-y border-t sm:divide-x sm:divide-y-0",
        facts.length === 3 ? "sm:grid-cols-3" : "sm:grid-cols-2",
        dim && "opacity-60",
      )}
    >
      {facts.map((f) => (
        <div key={f.label} className="flex min-w-0 items-baseline justify-between gap-3 px-3 py-2 sm:block sm:py-2.5">
          <dt className="text-muted-foreground shrink-0 text-[11px] leading-4">{f.label}</dt>
          <dd className="min-w-0 truncate text-xs leading-5">{f.value}</dd>
        </div>
      ))}
    </dl>
  );
}
