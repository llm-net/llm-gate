// 组件管理页：设备上按需安装的第三方程序（cloudflared、Mihomo 内核、Codex App Server）
// 集中在这里安装、升级、回退与卸载；功能页（公网接入、出站代理）只展示组件状态并链接过来。
//
// 版面撑满内容区的左右两栏（PageContainer 的 wide）：左栏是组件列表，一行说清装没装、装的
// 哪版、能不能更新、是不是正被功能使用；右栏是选中那个组件的详情与全部管理动作。组件不多，
// 但每个的读数与动作都长，铺成一列要反复上下找——选中式两栏让页面高度只由一个组件决定。
//
// 读数按 `ComponentSpec.status` 一次并发取齐：cloudflared 与 Mihomo 取自功能读数，既带组件层
// 读数也带「功能是否启用」——卸载必须先停功能，这一位是卸载按钮能不能按的判据；Codex App
// Server 没有对应功能，只取组件读数，卸载没有守卫。任一读数失败（管理器未接入、引擎聚合
// 异常）只让那一个组件在列表里标明并在右栏说明，不拖垮整页。零轮询。

import { useState } from "react";

import { Card } from "@/components/ui/card";
import { ComponentDetail, useRun } from "@/features/components/component-card";
import { ComponentList, type ComponentEntry } from "@/features/components/component-list";
import { COMPONENT_SPECS } from "@/features/components/specs";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

export function ComponentsPage(): React.ReactElement {
  const res = useResource(
    () =>
      Promise.all(
        COMPONENT_SPECS.map((spec) =>
          spec
            .status()
            .then((reading): ComponentEntry => ({ spec, reading }))
            .catch((): ComponentEntry => ({ spec, reading: null })),
        ),
      ),
    [],
  );
  const { busy, run } = useRun(res.reload);
  const [selected, setSelected] = useState<string>(COMPONENT_SPECS[0]?.id ?? "");

  return (
    <PageContainer wide>
      <PageHeader
        title={t("组件管理")}
        note={t("安装、升级、回退与卸载设备上按需运行的第三方程序；功能页只使用已安装的组件。")}
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {(entries) => {
          // 选中项恒落在列表里：规格表是编译期常量，取不到只可能是空表。
          const current = entries.find((e) => e.spec.id === selected) ?? entries[0];
          if (current === undefined) return null;
          return (
            <div className="grid min-w-0 grid-cols-1 items-start gap-4 lg:grid-cols-[22rem_minmax(0,1fr)] xl:grid-cols-[26rem_minmax(0,1fr)]">
              <ComponentList entries={entries} selected={current.spec.id} onSelect={setSelected} />
              <div className="flex min-w-0 flex-col gap-4">
                {current.reading === null ? (
                  <Card className="text-muted-foreground p-5 text-sm">
                    {t("{name} 组件读数暂时不可用，请刷新重试。", { name: current.spec.name })}
                  </Card>
                ) : (
                  <ComponentDetail spec={current.spec} comp={current.reading.comp} inUse={current.reading.inUse} busy={busy} run={run} />
                )}
                <Card className="text-muted-foreground p-5 text-xs leading-relaxed">
                  {t(
                    "组件不随固件发布，也不在设备上编译：设备只安装 LLM Gate官网签名清单指定的上游官方 release 精确制品，下载或上传后核对大小、SHA-256、架构与自述版本，再由升级引擎装进 A/B 槽位，可随时回退到上一版本。官网只签发清单，不镜像可执行文件。卸载前先到对应功能页停用。",
                  )}
                </Card>
              </div>
            </div>
          );
        }}
      </ResourceGate>
    </PageContainer>
  );
}
