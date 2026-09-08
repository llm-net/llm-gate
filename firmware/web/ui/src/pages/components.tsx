// 第三方组件页：设备上按需安装的第三方程序（cloudflared、Mihomo 内核）集中在这里
// 安装、升级、回退与卸载；功能页（公网接入、出站代理）只展示组件状态并链接过来。
//
// 读数直接取两项功能各自的状态端点：它们既带组件层读数，也带「功能是否启用」——卸载
// 必须先停功能，这一位是卸载按钮能不能按的判据。任一读数失败（管理器未接入、引擎聚合
// 异常）只让那一张卡说明不可用，不拖垮整页。零轮询。

import { Card } from "@/components/ui/card";
import { ComponentCard, useRun } from "@/features/components/component-card";
import { CLOUDFLARED_SPEC, MIHOMO_SPEC } from "@/features/components/specs";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate, SectionHead } from "./page-shell";

interface Slot {
  comp: api.ComponentStatus;
  inUse: boolean;
}

export function ComponentsPage(): React.ReactElement {
  const res = useResource(
    () =>
      Promise.all([
        api
          .getCloudflareTunnel()
          .then((r): Slot => ({ comp: r.tunnel.component, inUse: r.tunnel.enabled }))
          .catch((): Slot | null => null),
        api
          .getProxyCore()
          .then((r): Slot => ({ comp: r.proxy_core.component, inUse: r.proxy_core.enabled }))
          .catch((): Slot | null => null),
      ]).then(([cloudflared, mihomo]) => ({ cloudflared, mihomo })),
    [],
  );
  const { busy, run } = useRun(res.reload);

  return (
    <PageContainer>
      <PageHeader
        title={t("第三方组件")}
        note={t("安装、升级、回退与卸载设备上按需运行的第三方程序；功能页只使用已安装的组件。")}
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <Card className="text-muted-foreground p-5 text-xs leading-relaxed">
        {t(
          "组件不随固件发布，也不在设备上编译：设备只安装 LLM Gate官网签名清单指定的上游官方 release 精确制品，下载或上传后核对大小、SHA-256、架构与自述版本，再由升级引擎装进 A/B 槽位，可随时回退到上一版本。官网只签发清单，不镜像可执行文件。卸载前先到对应功能页停用。",
        )}
      </Card>
      <ResourceGate resource={res}>
        {(d) => (
          <>
            <SectionHead title="cloudflared" desc={t("Cloudflare Tunnel 的 connector，供「网络/域名/代理 → 公网接入」使用。")} />
            {d.cloudflared === null ? (
              <Card className="text-muted-foreground p-5 text-sm">{t("cloudflared 组件读数暂时不可用，请刷新重试。")}</Card>
            ) : (
              <ComponentCard spec={CLOUDFLARED_SPEC} comp={d.cloudflared.comp} inUse={d.cloudflared.inUse} busy={busy} run={run} />
            )}

            <SectionHead title={t("Mihomo 内核")} desc={t("Clash 订阅的代理内核，供「网络/域名/代理 → 出站代理」的「Clash 订阅（设备内置内核）」方式使用。")} />
            {d.mihomo === null ? (
              <Card className="text-muted-foreground p-5 text-sm">{t("Mihomo 内核组件读数暂时不可用，请刷新重试。")}</Card>
            ) : (
              <ComponentCard spec={MIHOMO_SPEC} comp={d.mihomo.comp} inUse={d.mihomo.inUse} busy={busy} run={run} />
            )}
          </>
        )}
      </ResourceGate>
    </PageContainer>
  );
}
