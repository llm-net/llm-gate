// 组件先决条件卡：功能页（公网接入的 Cloudflare Tunnel 面板、出站代理的内置内核面板）里
// 只展示「组件装没装、装的哪版、有没有更新」，安装 / 升级 / 回退 / 卸载一律去「第三方组件」页。

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import type { ComponentStatus } from "@/lib/api";
import { t } from "@/lib/i18n";
import { Link } from "@/lib/router";

import { componentBadge, toneClass, type ComponentSpec } from "./component-card";

export function ComponentPrereqCard({ spec, comp }: { spec: ComponentSpec; comp: ComponentStatus }): React.ReactElement {
  const badge = componentBadge(comp);
  const latest = comp.advisory?.latest ?? null;
  return (
    <Card className="gap-2 p-5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{t("{name} 组件", { name: spec.name })}</span>
        <Badge variant={badge.tone === "off" ? "secondary" : "outline"} className={toneClass(badge.tone)}>
          {badge.label}
        </Badge>
        {comp.installed ? <code className="text-muted-foreground font-mono text-xs">{spec.versionLabel(comp.version ?? "")}</code> : null}
        {latest !== null ? (
          <Badge variant="outline" className="text-signal-alert">
            {comp.installed ? t("可更新到 {version}", { version: spec.versionLabel(latest.version) }) : t("可安装 {version}", { version: spec.versionLabel(latest.version) })}
          </Badge>
        ) : null}
        <Link to="/components" className="ml-auto text-xs underline">
          {comp.installed ? t("去「第三方组件」页升级或卸载") : t("去「第三方组件」页安装")}
        </Link>
      </div>
      <p className="text-muted-foreground text-xs">
        {comp.installed
          ? t("组件由「第三方组件」页统一管理；这里只读。")
          : t("这项功能需要先安装该组件；组件由「第三方组件」页统一管理。")}
      </p>
    </Card>
  );
}
