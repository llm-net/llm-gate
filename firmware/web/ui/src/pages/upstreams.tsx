// 「模型接入」菜单组的四页。四种接入各占一页、各一条路由，侧栏各是一项：
//
//   API按量计费    /upstreams/usage    持上游 Key、按 token / 按次计费的平台账号，也收
//                                     无计费标记的通用兼容适配与 mock
//   API订阅套餐    /upstreams/plan     持上游 Key 的已付费套餐账号（ark_plan、qwen_plan、opencode_go…）
//   API私有部署    /upstreams/private  预留：用户自己私有化部署的模型服务
//   开发工具订阅   /agent-accounts     四种开发工具订阅铺位（agent_accounts 表）
//
// 四种接入在设备上统一成同一组输出——标准文本协议面、按厂商区分的视频/图像官方协议面、
// 各开发工具订阅独立的协议通道——那一侧由「使用指南」两页负责讲；本文件只管输入侧。
//
// 前两页共用同一套正文（账号列 + 模型列，features/upstreams/），只按账号的 billing_mode
// 分页（domain.ts 的 apiBillingPage）：账号列只列本页的账号，新建账号对话框只列本页那一组
// 平台；模型列只列在本页账号上挂了来源的模型——同一个模型既挂套餐又挂按量兜底时两页
// 都有它（卡片上照样列全它的来源），一条来源都没挂的模型归按量页。「添加上游」对话框
// 仍列全部账号：给套餐模型挂一条按量兜底正是从套餐页发起的常规动作。
//
// **整页滚动**（PageContainer 的常规流），两列的列头钉在同一网格行上（各列组件把列头
// 与正文渲染成网格的两个直接子节点），卡片顶因此对齐；API 页按正文可用宽度切换单双列。
//
// 页头在取数闸外**常驻渲染**；标题、简介与刷新入口都使用管理页共用标题栏，不随正文
// 加载态消失。零轮询：进页一次并发取两份读数（账号 + 模型目录），刷新按钮再取一次。

import { AgentAccountsColumn } from "@/features/upstreams/agent-accounts";
import { AgentModelsColumn } from "@/features/upstreams/agent-models";
import { ApiAccountsColumn } from "@/features/upstreams/api-accounts";
import { agentPricingModel, apiBillingPage, type ApiBillingPage } from "@/features/upstreams/domain";
import { ModelsColumn } from "@/features/upstreams/models-column";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { AddModelsDialog, useAddModels } from "./add-models";
import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

// 两列网格：弹性列一律 minmax(0,1fr) 而不是裸 1fr——裸 1fr 的下限是内容的 min-content，
// 模型卡里那张上游小表一宽就把整列撑出视口；下限归零后表在卡内横滚，列不动。
const twoColumns =
  "grid grid-cols-[minmax(0,1fr)] gap-x-4 gap-y-3 lg:grid-cols-[25rem_minmax(0,1fr)] xl:grid-cols-[27rem_minmax(0,1fr)]";

const API_PAGE_COPY: Record<ApiBillingPage, { title: string; note: string }> = {
  usage: {
    title: t("API按量计费"),
    note: t("管理按量计费的平台账号，为模型配置上游来源与目录价。"),
  },
  plan: {
    title: t("API订阅套餐"),
    note: t("持上游平台 Key 的已付费套餐账号；挂给模型时默认优先、先用满套餐，再落到按量账号兜底。"),
  },
};

export function ApiAccountsPage({ billing }: { billing: ApiBillingPage }): React.ReactElement {
  const addModels = useAddModels();
  const res = useResource(async () => {
    const [{ upstreams }, { models }] = await Promise.all([api.listUpstreams(), api.listModels()]);
    return { upstreams, models };
  }, [billing]);
  const copy = API_PAGE_COPY[billing];

  return (
    <PageContainer wide compact>
      <PageHeader compact title={copy.title} note={copy.note} refreshing={res.loading} onRefresh={res.reload} />
      <ResourceGate resource={res}>
        {(d) => {
          const upstreams = d.upstreams.filter((u) => apiBillingPage(u.billing_mode) === billing);
          const ids = new Set(upstreams.map((u) => u.id));
          const models = d.models.filter(
            (m) =>
              !agentPricingModel(m) &&
              (m.sources.some((s) => ids.has(s.upstream_id)) || (billing === "usage" && m.sources.length === 0)),
          );
          return (
            <div className="@container/upstreams">
              <div className="grid min-w-0 grid-cols-1 gap-x-3 gap-y-2 @3xl/upstreams:grid-cols-[18rem_minmax(0,1fr)] @5xl/upstreams:grid-cols-[20rem_minmax(0,1fr)]">
                <ApiAccountsColumn
                  billing={billing}
                  upstreams={upstreams}
                  models={d.models}
                  reload={res.reload}
                  onAddModels={addModels.open}
                />
                <ModelsColumn all={models} upstreams={d.upstreams} reload={res.reload} />
              </div>
            </div>
          );
        }}
      </ResourceGate>
      <AddModelsDialog state={addModels} onDone={res.reload} />
    </PageContainer>
  );
}

// 预留页：只有说明，没有账号或模型。私有部署的语义（计费、调度优先级、健康探测）还没定，
// 在定下来之前不借「通用兼容适配」的壳子伪装成已经支持——但要把眼下可走的路写清楚。
export function PrivateDeploymentPage(): React.ReactElement {
  return (
    <PageContainer wide>
      <PageHeader title={t("API私有部署")} note={t("接入用户自己私有化部署的模型服务。")} />
      <div className="text-muted-foreground flex flex-col gap-2 rounded-xl border border-dashed px-5 py-10 text-center text-sm leading-relaxed">
        <p>{t("这一页预留给私有化部署的模型服务，当前尚未开放。")}</p>
        <p>{t("已经部署好的兼容服务，眼下可以到「API按量计费」页新建账号，选「通用兼容适配」平台填入服务地址与 Key 接入。")}</p>
      </div>
    </PageContainer>
  );
}

export function AgentAccountsPage(): React.ReactElement {
  const res = useResource(async () => {
    const [{ accounts }, { models }, cursorPrices] = await Promise.all([
      api.listAgentAccounts(),
      api.listModels(),
      api.getCursorPricing(),
    ]);
    return { accounts, models, cursorPrices };
  }, []);

  return (
    <PageContainer wide>
      <PageHeader
        title={t("开发工具订阅")}
        note={t("接入 Codex、Grok Build、Claude Code 与 Cursor 订阅；凭据只封存在设备里，成员用自己的 API密钥经设备使用。")}
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {(d) => (
          <div className={twoColumns}>
            <AgentAccountsColumn accounts={d.accounts} reload={res.reload} />
            <AgentModelsColumn all={d.models} accounts={d.accounts} cursorPrices={d.cursorPrices} />
          </div>
        )}
      </ResourceGate>
    </PageContainer>
  );
}
