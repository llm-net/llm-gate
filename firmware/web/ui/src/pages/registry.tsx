// 路由 → 页面。守卫已经把 route 收窄到 BusinessRoute，所以这里是一张全覆盖的表，
// 没有兜底分支——加路由时 TypeScript 会逼着在这里补上对应页面。
//
// 迁移期未落地的页面先给 Pending 占位。每迁完一页换掉一行。

import { t } from "@/lib/i18n";
import type { BusinessRoute } from "@/lib/routes";

import { AgentHostsPage } from "./agent-hosts";
import { ComponentsPage } from "./components";
import { CredentialsPage } from "./credentials";
import { HostAgentPage } from "./host-agent";
import { HostModelPage } from "./host-model";
import { HostToolsPage } from "./host-tools";
import { KeysPage } from "./keys";
import { EgressPage, NetworkPage } from "./network";
import { Pending } from "./pending";
import { StatusPage } from "./status";
import { UpdatesPage } from "./updates";
import { AgentAccountsPage, ApiAccountsPage, PrivateDeploymentPage } from "./upstreams";
import { ModelRoutingPage } from "./use-models";
import { UsagePage } from "./usage";
import { WorkspacePage } from "./workspace";
import { WorkspacesPage } from "./workspaces";
import { APIDebugPage } from "./api-debug";
import { MediaPage } from "./media";

type PageRoute = BusinessRoute;

const TITLES: Record<PageRoute, string> = {
  "/model-routing/api": t("API调用"),
  "/model-routing/dev-tools": t("开发工具接入"),
  "/api-debug": t("API调测"),
  "/media": t("媒体生成"),
  "/keys": t("API密钥"),
  "/upstreams/usage": t("模型接入 · API按量计费"),
  "/upstreams/plan": t("模型接入 · API订阅套餐"),
  "/upstreams/private": t("模型接入 · API私有部署"),
  "/agent-accounts": t("模型接入 · 开发工具订阅"),
  "/agent-hosts": t("智能体 · 主机/SoC"),
  "/host-agent": t("智能体 · Agent远控"),
  "/host-tools": t("智能体 · 工具配置"),
  "/host-model": t("智能体 · 模型服务"),
  "/credentials": t("智能体 · 凭证管理"),
  "/workspaces": t("智能体 · 工作空间管理"),
  "/workspace": t("智能体 · 工作空间管理"),
  "/usage": t("模型用量"),
  "/status": t("设备状态"),
  "/network": t("网络/域名/代理"),
  "/egress": t("网络/域名/代理 · 出站代理"),
  "/updates": t("设备更新"),
  "/components": t("组件管理"),
};

export function Page({ route }: { route: PageRoute }): React.ReactElement {
  switch (route) {
    case "/keys":
      return <KeysPage />;
    case "/model-routing/api":
      return <ModelRoutingPage mode="api" />;
    case "/model-routing/dev-tools":
      return <ModelRoutingPage mode="dev-tools" />;
    case "/api-debug":
      return <APIDebugPage />;
    case "/media":
      return <MediaPage />;
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
    case "/agent-hosts":
      return <AgentHostsPage />;
    case "/host-agent":
      return <HostAgentPage />;
    case "/host-tools":
      return <HostToolsPage />;
    case "/host-model":
      return <HostModelPage />;
    case "/credentials":
      return <CredentialsPage />;
    case "/workspaces":
      return <WorkspacesPage />;
    case "/workspace":
      return <WorkspacePage />;
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
