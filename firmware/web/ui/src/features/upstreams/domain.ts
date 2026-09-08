// 「模型接入」菜单组各页的归属判据。模型列（API按量计费 / API订阅套餐）与订阅模型列
// （开发工具订阅）各取一边，两边必须同口径，所以只在这里写一份；服务端注记侧是同一条判据。

import type * as api from "@/lib/api";

// 订阅文本计价行：目录数据把这个名字算作某份开发工具订阅的模型，且没挂任何上游。一旦挂了
// 上游，它就同时真被 API 上游承载，回 API 那两页按常规渲染、照旧可编辑——所以条件
// 里有 sources 空。
export function agentPricingModel(m: api.Model): boolean {
  return m.kind === "text" && m.agent !== "" && m.sources.length === 0;
}

// 持上游 Key 的账号分两页：账号建号时从平台目录快照的 billing_mode 是唯一判据——
// `subscription` 归「API订阅套餐」，`usage` 与无计费标记的通用兼容适配 / mock（`none`）
// 归「API按量计费」。平台选单（UpstreamPlatform）与账号（Upstream）的 billing_mode
// 同域，新建账号对话框按同一函数裁剪本页可选的平台。
export type ApiBillingPage = "usage" | "plan";

export function apiBillingPage(mode: api.BillingMode | "none"): ApiBillingPage {
  return mode === "subscription" ? "plan" : "usage";
}
