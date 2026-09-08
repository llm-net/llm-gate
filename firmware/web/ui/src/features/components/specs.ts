// 两个第三方组件的规格（名字、上游、许可证、清单路径、API、被谁使用）。
// 新增组件只加一条，卡片与先决条件卡不用改。

import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

import type { ComponentSpec } from "./component-card";

export const CLOUDFLARED_SPEC: ComponentSpec = {
  id: "cloudflared",
  name: "cloudflared",
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
