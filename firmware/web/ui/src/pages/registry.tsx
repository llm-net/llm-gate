// 路由 → 页面。守卫已经把 route 收窄到 BusinessRoute，所以这里是一张全覆盖的表，
// 没有兜底分支——加路由时 TypeScript 会逼着在这里补上对应页面。
//
// 迁移期未落地的页面先给 Pending 占位。每迁完一页换掉一行。

import { t } from "@/lib/i18n";
import type { BusinessRoute } from "@/lib/routes";

import { ComponentsPage } from "./components";
import { KeysPage } from "./keys";
import { EgressPage, NetworkPage } from "./network";
import { Pending } from "./pending";
import { StatusPage } from "./status";
import { UpdatesPage } from "./updates";
import { AgentAccountsPage, ApiAccountsPage, PrivateDeploymentPage } from "./upstreams";
import { ModelRoutingPage } from "./use-models";
import { UsagePage } from "./usage";

type PageRoute = BusinessRoute;

const TITLES: Record<PageRoute, string> = {
  "/model-routing/api": t("API调用"),
  "/model-routing/dev-tools": t("开发工具接入"),
  "/keys": t("API密钥"),
  "/upstreams/usage": t("模型接入 · API按量计费"),
  "/upstreams/plan": t("模型接入 · API订阅套餐"),
  "/upstreams/private": t("模型接入 · API私有部署"),
  "/agent-accounts": t("模型接入 · 开发工具订阅"),
  "/usage": t("模型用量"),
  "/status": t("设备状态"),
  "/network": t("网络/域名/代理"),
  "/egress": t("网络/域名/代理 · 出站代理"),
  "/updates": t("设备更新"),
  "/components": t("第三方组件"),
};

export function Page({ route }: { route: PageRoute }): React.ReactElement {
  switch (route) {
    case "/keys":
      return <KeysPage />;
    case "/model-routing/api":
      return <ModelRoutingPage mode="api" />;
    case "/model-routing/dev-tools":
      return <ModelRoutingPage mode="dev-tools" />;
    case "/status":
      return <StatusPage />;
    case "/usage":
      return <UsagePage />;
    case "/upstreams/usage":
      return <ApiAccountsPage billing="usage" />;
    case "/upstreams/plan":
      return <ApiAccountsPage billing="plan" />;
    case "/upstreams/private":
      return <PrivateDeploymentPage />;
    case "/agent-accounts":
      return <AgentAccountsPage />;
    case "/network":
      return <NetworkPage />;
    case "/egress":
      return <EgressPage />;
    case "/updates":
      return <UpdatesPage />;
    case "/components":
      return <ComponentsPage />;
    default:
      return <Pending route={route} title={TITLES[route]} />;
  }
}

export function pageTitle(route: PageRoute): string {
  return TITLES[route];
}
