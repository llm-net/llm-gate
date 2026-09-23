// 三个第三方组件的规格（名字、一句话说明、上游、许可证、清单路径、读数与 API、被谁使用）。
// 新增组件只加一条并挂进 COMPONENT_SPECS：「组件管理」页的列表、详情与功能页的先决条件卡
// 都只认这张表，谁都不用改。

import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

import type { ComponentSpec } from "./component-card";

export const CLOUDFLARED_SPEC: ComponentSpec = {
  id: "cloudflared",
  name: "cloudflared",
  tagline: t("Cloudflare Tunnel 的 connector，供「公网接入」使用。"),
  upstream: "Cloudflare",
  purpose: t("Cloudflare Tunnel 的 connector：设备主动连出到你的 Cloudflare 账号，把公网请求经专用 origin socket 回源到设备。"),
  sourceNote: t(
    "设备只安装签名清单指定的 Cloudflare 官方 release 精确制品，逐一核对长度、SHA-256、架构与自述版本。官方地址不可达时，从「官方 release」链接下载同一文件后上传；离线时可把官网的 stable.json 与 stable.json.sig 原文粘贴导入。",
  ),
  licenseNote: t("cloudflared 由 Cloudflare 以 Apache-2.0 许可发布；官网只提供签名清单，不镜像可执行文件。"),
  manifestPath: "/updates/components/cloudflared/stable.json",
  versionLabel: (v) => v,
  process: "connector",
  usedBy: { label: t("公网接入 · Cloudflare Tunnel"), route: "/network" },
  keepsOnRemove: t("本机 Tunnel 设置与已密封的 token"),
  status: () => api.getCloudflareTunnel().then((r) => ({ comp: r.tunnel.component, inUse: r.tunnel.enabled })),
  api: {
    check: api.cloudflaredCheck,
    importManifest: api.cloudflaredImportManifest,
    download: api.cloudflaredDownload,
    upload: api.cloudflaredUpload,
    install: api.cloudflaredInstall,
    rollback: api.cloudflaredRollback,
    discard: api.cloudflaredDiscard,
    remove: api.cloudflaredRemove,
  },
};

export const MIHOMO_SPEC: ComponentSpec = {
  id: "mihomo",
  name: t("Mihomo 内核"),
  tagline: t("Clash 订阅的代理内核，供「出站代理」的设备内置内核方式使用。"),
  upstream: "Mihomo",
  purpose: t("出站代理「Clash 订阅（设备内置内核）」方式的代理内核：按订阅节点生成配置，只绑定本机 127.0.0.1 的 SOCKS 端口。"),
  sourceNote: t(
    "Mihomo 是 MetaCubeX 以 GPL-3.0 发布的第三方代理内核。设备只安装签名清单指定的官方 release 精确制品：下载 gzip 包后核对长度与 SHA-256，解压后再核对长度、SHA-256、架构与自述版本。官方地址不可达时，从「官方 release」链接下载同一文件后上传；离线时可粘贴官网的 stable.json 与 stable.json.sig 导入。",
  ),
  licenseNote: t("Mihomo 由 MetaCubeX 以 GPL-3.0 许可发布；LLM Gate 不含其代码，只经标准 SOCKS5 与它通信。官网只提供签名清单，不镜像可执行文件。"),
  manifestPath: "/updates/components/mihomo/stable.json",
  versionLabel: (v) => `v${v}`,
  process: t("内核"),
  usedBy: { label: t("出站代理 · Clash 订阅（设备内置内核）"), route: "/egress" },
  keepsOnRemove: t("订阅与节点选择"),
  status: () => api.getProxyCore().then((r) => ({ comp: r.proxy_core.component, inUse: r.proxy_core.enabled })),
  api: {
    check: api.mihomoCheck,
    importManifest: api.mihomoImportManifest,
    download: api.mihomoDownload,
    upload: api.mihomoUpload,
    install: api.mihomoInstall,
    rollback: api.mihomoRollback,
    discard: api.mihomoDiscard,
    remove: api.mihomoRemove,
  },
};

export const CODEX_APP_SERVER_SPEC: ComponentSpec = {
  id: "codex-app-server",
  name: "Codex App Server",
  tagline: t("OpenAI Codex 的 App Server 整包，供「主机/SoC」页的 Agent远控使用。"),
  upstream: "OpenAI",
  purpose: t("OpenAI Codex 的 App Server 整包：随 Codex CLI 同版本发布，安装在设备的 A/B 槽位里；本页只管安装、升级、回退与卸载，不启动常驻进程。「主机/SoC」页的 Agent远控以它为引擎，API 密钥与模型在新建对话时选择。"),
  sourceNote: t(
    "Codex App Server 是 OpenAI 以 Apache-2.0 发布的第三方组件。设备只安装签名清单指定的官方 release 整包（codex-app-server-package）：下载 tar.gz 后核对长度与 SHA-256，按官方布局解包（入口与工具执行宿主、内置资源一并到位），再核对入口的长度、SHA-256、架构与自述版本。官方地址不可达时，从「官方 release」链接下载同一文件后上传；离线时可粘贴官网的 stable.json 与 stable.json.sig 导入。",
  ),
  licenseNote: t("Codex App Server 由 OpenAI 以 Apache-2.0 许可发布；LLM Gate 不含其代码。官网只提供签名清单，不镜像可执行文件。"),
  manifestPath: "/updates/components/codex-app-server/stable.json",
  versionLabel: (v) => `v${v}`,
  keepsOnRemove: t("已接受的签名清单"),
  // 没有对应功能：组件层读数之外没有「功能启用中」这一位，卸载也就没有守卫。
  status: () => api.getCodexAppServer().then((r) => ({ comp: r.component, inUse: false })),
  api: {
    check: api.codexAppServerCheck,
    importManifest: api.codexAppServerImportManifest,
    download: api.codexAppServerDownload,
    upload: api.codexAppServerUpload,
    install: api.codexAppServerInstall,
    rollback: api.codexAppServerRollback,
    discard: api.codexAppServerDiscard,
    remove: api.codexAppServerRemove,
  },
};

// 「组件管理」页的列表顺序即这里的顺序。
export const COMPONENT_SPECS: ComponentSpec[] = [CLOUDFLARED_SPEC, MIHOMO_SPEC, CODEX_APP_SERVER_SPEC];
