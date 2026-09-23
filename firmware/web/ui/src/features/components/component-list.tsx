// 组件列表（「组件管理」页左栏）：一行一个组件，点哪个右栏就是哪个的详情与管理动作。
//
// 这一栏比普通导航列宽，因为它要把「装没装」当场说清：状态徽标、已装版本与槽位、
// 可更新到哪一版、有没有就绪制品、使用它的功能是不是启用中——不点开也看得见。
// 读数取不到的组件照样列出来并标明，选中后由右栏说明怎么恢复。
//
// 纯展示 + 选中回调，不发请求、不轮询：读数由页面一次并发取齐后传进来。

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";

import { componentBadge, toneClass, type ComponentReading, type ComponentSpec } from "./component-card";

export interface ComponentEntry {
  spec: ComponentSpec;
  /** 组件层读数；取不到为 null（那一个组件降级，不影响别的）。 */
  reading: ComponentReading | null;
}

export function ComponentList({
  entries,
  selected,
  onSelect,
}: {
  entries: ComponentEntry[];
  selected: string;
  onSelect: (id: string) => void;
}): React.ReactElement {
  return (
    <Card className="gap-0 p-0">
      <header className="flex items-center justify-between gap-2 border-b px-4 py-2.5">
        <h2 className="text-sm font-semibold">{t("组件")}</h2>
        <span className="text-muted-foreground text-xs">{t("共 {n} 个", { n: entries.length })}</span>
      </header>
      <ul className="divide-y" aria-label={t("组件列表")}>
        {entries.map(({ spec, reading }) => (
          <li key={spec.id}>
            <button
              type="button"
              aria-current={spec.id === selected ? "true" : undefined}
              onClick={() => onSelect(spec.id)}
              className={cn(
                "flex w-full cursor-pointer flex-col gap-1 px-4 py-3 text-left transition-colors outline-none",
                "focus-visible:ring-ring/50 focus-visible:ring-[3px] focus-visible:-outline-offset-2",
                spec.id === selected ? "bg-accent/60" : "hover:bg-accent/30",
              )}
            >
              <div className="flex flex-wrap items-center gap-2">
                <span className="min-w-0 truncate font-medium">{spec.name}</span>
                <ComponentStateBadge reading={reading} />
              </div>
              <p className="text-muted-foreground text-xs leading-5">{spec.tagline}</p>
              <ComponentInstallLine spec={spec} reading={reading} />
            </button>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function ComponentStateBadge({ reading }: { reading: ComponentReading | null }): React.ReactElement {
  if (reading === null) {
    return (
      <Badge variant="outline" className="text-signal-alert">
        {t("读数不可用")}
      </Badge>
    );
  }
  const badge = componentBadge(reading.comp);
  return (
    <Badge variant={badge.tone === "off" ? "secondary" : "outline"} className={toneClass(badge.tone)}>
      {badge.label}
    </Badge>
  );
}

// 安装情况那一行：装了就报版本与槽位，没装就明说，后面再跟上「可更新 / 制品就绪 /
// 使用中」几个短标记——这几项决定管理员进这一页要不要动手。
function ComponentInstallLine({ spec, reading }: { spec: ComponentSpec; reading: ComponentReading | null }): React.ReactElement {
  if (reading === null) {
    return <p className="text-muted-foreground text-xs">{t("刷新后重试")}</p>;
  }
  const comp = reading.comp;
  const latest = comp.advisory?.latest ?? null;
  const staged = comp.staged ?? null;
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
      {comp.installed ? (
        <span className="text-muted-foreground">
          <code className="text-foreground font-mono">{spec.versionLabel(comp.version ?? "")}</code>
          <span className="ml-1.5">slot {comp.slot}</span>
        </span>
      ) : (
        <span className="text-muted-foreground">{t("未安装")}</span>
      )}
      {latest === null ? null : (
        <span className="text-signal-alert">
          {comp.installed
            ? t("可更新到 {version}", { version: spec.versionLabel(latest.version) })
            : t("可安装 {version}", { version: spec.versionLabel(latest.version) })}
        </span>
      )}
      {staged === null ? null : <span className="text-muted-foreground">{t("制品就绪 {version}", { version: spec.versionLabel(staged.version) })}</span>}
      {reading.inUse ? <span className="text-signal-ok">{t("使用中")}</span> : null}
    </div>
  );
}
