// 「使用指南」菜单组的两张页面：API调用与开发工具接入（路由仍是 /model-routing/*）。
//
// 两页共用同一套接入地址选择：
//
//   内网 IP / 内网域名 / 外网映射
//     ├─ API调用         三个文本协议面的客户端配置与示例
//     └─ 开发工具接入    作为 gate 首次安装脚本的一项生成选项
//
// 开发工具页先安装 gate，再讲五种工具的用法；Codex CLI 与 Codex App 分成两张帮助标签。
// 接入地址与 API 密钥都只归第一段的安装脚本生成器，不是下方标签的前置步骤。组件持有选中的
// 接入路径、帮助标签页与已解封的 API密钥明文缓存；切换帮助或地址时 React 会保留
// 这份组件状态，避免白做一次明文解封。**明文只活在 KeyFill 那张手写 Map 里**，
// 不进任何持久化状态（§15.1）。
//
// **零轮询**：API调用只取接入读数；开发工具接入把接入读数与 Key 列表并发取齐。
// 接入读数失败降级成「只知道当前地址」，Key 列表失败就当没有 Key（命令保留占位符）。
// 401 照旧抛给 api 层的统一回调。
//
// 版面**撑满内容区**（PageContainer 的 wide）：两张页面正文都是双栏，栏里摆的是
// 地址、命令和整段可复制的示例代码，钉在 max-w-5xl 上只会把长命令挤成横滚条。

import { useState } from "react";

import {
  accessTargets,
  defaultTarget,
  TargetTabs,
  type TargetKind,
} from "@/features/access/access";
import { ApiGuide } from "@/features/access/api-guide";
import {
  DevToolGuide,
  type DevToolHelpTab,
  type KeyFill,
} from "@/features/access/devtool-guide";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

export type ModelRoutingMode = "api" | "dev-tools";

function readAccessSnapshot(): Promise<api.AccessSnapshot> {
  return api.getEndpoints().catch((err: unknown) => {
    if (err instanceof api.ApiError && err.status === 401) throw err;
    // 降级快照：读不到接入读数时页面照常画壳子。版本给空串——页面不显示
    // 版本，空串也让读版本的地方（顶栏）知道这不是一份真读数。
    return {
      endpoints: { addresses: [] },
      models: [],
      firmware_version: "",
      hardware_model: "",
    } as api.AccessSnapshot;
  });
}

export function ModelRoutingPage({ mode }: { mode: ModelRoutingMode }): React.ReactElement {
  const res = useResource(
    () => {
      const snap = readAccessSnapshot();
      if (mode === "api") return snap.then((value) => ({ snap: value, keys: [] as api.ApiKey[] }));
      return Promise.all([snap, api.listKeys().catch(() => ({ keys: [] as api.ApiKey[] }))]).then(
        ([value, all]) => ({ snap: value, keys: all.keys }),
      );
    },
    [mode],
  );
  const [kind, setKind] = useState<TargetKind | null>(null);
  const [activeToolTab, setActiveToolTab] = useState<DevToolHelpTab | null>(null);
  const [fill, setFill] = useState<KeyFill>({ selectedID: 0, plain: new Map() });
  const apiMode = mode === "api";

  return (
    <PageContainer wide>
      <PageHeader
        title={apiMode ? t("API调用") : t("开发工具接入")}
        note={
          apiMode
            ? t("先选从哪个地址连到这台设备，再复制对应的调用地址和示例。")
            : t("先安装 gate 工具，再查看各开发工具的独立接入方法。")
        }
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {(d) => {
          const targets = accessTargets(d.snap.endpoints);
          // 没选过、或选过的那条已不在表里时落到缺省路径（内网 IP 优先）。
          const target = targets.find((tg) => tg.kind === kind) ?? defaultTarget(targets);
          const keys = d.keys.filter((k) => !k.disabled);
          return (
            <>
              {apiMode ? (
                <>
                  <div className="flex flex-col gap-2">
                    <div className="flex flex-wrap items-center gap-3">
                      <span className="text-sm font-medium">{t("接入地址")}</span>
                      <TargetTabs targets={targets} active={target} onSelect={(next) => setKind(next.kind)} />
                    </div>
                    <p className="text-muted-foreground text-xs">{target.note}</p>
                  </div>
                  <ApiGuide target={target} models={d.snap.models} />
                </>
              ) : (
                <DevToolGuide
                  targets={targets}
                  target={target}
                  onTargetSelect={(next) => setKind(next.kind)}
                  snap={d.snap}
                  keys={keys}
                  activeTab={activeToolTab}
                  setActiveTab={setActiveToolTab}
                  fill={fill}
                  setFill={setFill}
                />
              )}
            </>
          );
        }}
      </ResourceGate>
    </PageContainer>
  );
}
