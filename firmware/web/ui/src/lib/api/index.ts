// /admin/v1 API 客户端：与 internal/admin 的端点一一对应。整体照搬自管理台
// web/admin/src/api.ts（那棵树是手写 DOM、这棵是 React，但这一层不碰 DOM，
// 所以是搬不是重写）。
//
// 分三个文件而不是按「类型/函数」切：本文件按业务域组织，每域的类型、端点与
// 标签函数写在一起、注释也是按域写的，拆开会让注释和它解释的东西分家。真正
// 独立出去的只有两样——传输层 client.ts 与金额换算 money.ts，它们与业务域无关。

import { t } from "@/lib/i18n";

import { keyRequest, request, requestBinary } from "./client";

export { ApiError, errorMessage, setSessionExpiredHandler } from "./client";
export * from "./money";

// ApiKey 对应服务端 keyJSON；永不含明文与摘要——明文只走创建响应与复制端点
// （revealKey）两条通道。
export interface ApiKey {
  id: number;
  label: string;
  display_prefix: string;
  display_last4: string;
  disabled: boolean;
  // 库里封存有明文、可自助复制；false = 签发于旧版本固件，明文物理上
  // 不可恢复，界面不给复制按钮，出路是重签一把。
  plaintext_available: boolean;
  // 密钥级限额（整数微元 / 次每分钟，null = 不限，0 = 零额度）。
  budget_day_micro: number | null;
  budget_week_micro: number | null;
  budget_month_micro: number | null;
  rpm_limit: number | null;
  // 预算耗尽后继续使用的一次性按量额度即时剩余（整数微元）。
  metered_allowance_micro: number;
  created_at: string;
  last_used_at?: string; // 缺省 = 从未使用
  spend?: KeySpend; // 列表与 PATCH 响应填充（计量未装配时整块缺席）
}

// KeySpend 是这把密钥**当前**自然日 / 自然周 / 自然月的已消费额（整数微元）。
// 与上面三条预算成对读：一个是花了多少，一个是能花多少。
//
// **整块字段缺席 = 计量未装配**，不是「没花钱」：那种情况下界面只显示额度，
// 绝不把缺席当 0 画进仪表（服务端 keySpendJSON 的注释是同一条口径）。
export interface KeySpend {
  day_micro: number;
  week_micro: number;
  month_micro: number;
}

// UpstreamType 是内置端点表认识的上游类型（建后不可改）。qwen_plan 是阿里云
// 百炼的通义千问 Token Plan 订阅套餐（2026-08-09，文本三个协议面）；opencode_go 是
// OpenCode Go 包月套餐（同一把 sk- Key，文本三个协议面同一端点根）。openai_compat 是
// 通用 OpenAI 适配：无内置端点、地址自填，以 Chat 上游承载 Chat 与 Responses 协议面，
// 排在特化平台之后作兜底。
export type UpstreamType =
  | "deepseek"
  | "ark"
  | "ark_plan"
  | "qwen_plan"
  | "opencode_go"
  | "minimax"
  | "openai_compat"
  | "anthropic_compat"
  | "mock";

// Protocol 是协议面标识，与来源的 protocols 字段同域。文本三个协议面之外是
// 按「厂商 × 模态」各一个的厂商协议面：协议按「所属模型的 kind」圈定（服务端
// kindProtocols），一个 AIGC 模型的全部来源必须同一协议面。与文本三个协议面
// 不同，**厂商协议面就是客户端契约**——客户端得按那家厂商的官方文档构造请求，
// 路径是 `/<厂商段>` + 厂商站点根之后的原样那一段（minimax_video →
// /minimax/v2/…，ark_video / ark_image → /ark/api/v3/…；厂商段见 ProtocolFace）。
// 方舟按量与套餐是同一协议面的两种上游。
export type Protocol =
  | "openai_chat"
  | "openai_responses"
  | "anthropic_messages"
  | "ark_video"
  | "ark_image"
  | "minimax_video";

// ProtocolFace 是厂商协议面的路径首段（服务端 config.ProtocolFace*）：设备只有一个
// 主机名，厂商面按 URL 第一段分开，首段之后就是厂商官方路径。
export type ProtocolFace = "ark" | "minimax";

// protocolFaceOf 是协议面 → 厂商段的唯一映射；文本三面没有厂商段。
export function protocolFaceOf(p: Protocol): ProtocolFace | null {
  switch (p) {
    case "ark_video":
    case "ark_image":
      return "ark";
    case "minimax_video":
      return "minimax";
    default:
      return null;
  }
}

// vendorSurfaceSubmitPaths 是各厂商协议面对客户端开放的**提交**路径（与服务端
// server.go 的路由表同表；查询 / 取消等后续路径在「可用模型」卡的教程里逐条写）。
// 文本三面走 TextProtocolSurfaces。
export function vendorSurfaceSubmitPaths(p: Protocol): string[] {
  switch (p) {
    case "minimax_video":
      return ["POST /minimax/v2/video_generation", "POST /minimax/v2/h3_context_ir"];
    case "ark_video":
      return ["POST /ark/api/v3/contents/generations/tasks"];
    case "ark_image":
      return ["POST /ark/api/v3/images/generations"];
    default:
      return [];
  }
}

// ModelKind 是模型种类（建后不可改，改 kind 等于换模型）：它决定模型走哪组
// 入口——text 走三个对话协议面，video 走异步任务面，image 走同步出图入口。
export type ModelKind = "text" | "video" | "image";

// MiniMax 的国内/国际双站点（服务端 upstream.MinimaxSiteCN/Intl 同值）：
// minimax 上游的 base_url 只在这两个值里选，空串 = 国内缺省。两个站点的
// 账号与 Key 互相独立、余额不互通。
export const MinimaxSiteCN = "https://api.minimaxi.com";
export const MinimaxSiteIntl = "https://api.minimax.io";

// Upstream 对应服务端 upstreamJSON。凭证永不回显：只有末 4 位；
// api_key_last4 为空表示「无凭证的 mock」或「密文解不开，需重新录入」。
// balance_supported 表示该类型支持平台余额查询（服务端是唯一权威，界面只按
// 它显隐入口，不自维护类型表）。
export interface Upstream {
  id: number;
  name: string;
  type: UpstreamType;
  catalog_id: string;
  platform_label: string;
  billing_mode: BillingMode | "none";
  api_key_last4: string;
  base_url: string;
  disabled: boolean;
  // egress_mode 是该账号的出站方式覆盖：inherit 跟随「网络/域名/代理」页「模型与订阅接口」
  // 的出口，direct / proxy 强制（见 EgressStatus）。
  egress_mode: EgressMode;
  balance_supported: boolean;
  created_at: string;
  updated_at: string;
}

// ModelSource 对应服务端 sourceJSON：一条「模型 → 上游」的来源。
// upstream_model_id 空串表示与模型名相同；protocols 是这条来源运行时能服务
// 的入口（服务端按内置端点表算好，UI 不再自行推导）。
export interface ModelSource {
  id: number;
  model_id: number;
  upstream_id: number;
  upstream_name: string;
  upstream_type: UpstreamType;
  upstream_catalog_id: string;
  upstream_platform_label: string;
  upstream_billing_mode: BillingMode | "none";
  upstream_disabled: boolean;
  // upstream_egress_mode 是该来源所在账号的出站方式覆盖；同一模型的来源出口不一致时
  // 界面提示「切换来源的重试可能经不同出口」。
  upstream_egress_mode: EgressMode;
  upstream_model_id: string;
  priority: number;
  disabled: boolean;
  protocols: Protocol[];
}

// Pricing 是模型目录价：「价格字段名 → 整数微元」的扁平表（服务端
// usage.Pricing 同形）。null / 空对象 = 未定价——照常转发、金额记 0、界面挂
// 「未定价」警示徽章；显式写 0 的字段是"定价为免费"，与未定价是两回事。
export type Pricing = Record<string, number>;

// Model 对应服务端 modelJSON：name 就是客户端可见的原始模型名。
// entry_openai / entry_responses / entry_anthropic 是协议面开关（仅 text 有语义，默认全开，
// 至少留一个；AIGC 模型恒为 true 且界面不渲染开关）——数据面对关掉的入口
// 直接 404，契约见 docs/firmware-gateway.md。
export interface Model {
  id: number;
  name: string;
  kind: ModelKind;
  // family 是 AIGC 模型声明的协议面（协议标识，如 ark_video；0014，建后不可改；text 恒空串）。
  // 空串的 video 行 = 存量未声明（迁移回填不到的无来源行），由首条上游懒钉。
  family: "" | Protocol;
  // protocol_face 是 family 所属厂商的路径首段（服务端派生、只读）；text 与未声明行为空串。
  protocol_face: "" | ProtocolFace;
  // agent 非空 = 官方价文件把这个名字标为该订阅的主力文本模型，本行因此是
  // 订阅流量的计价行（数据面按名取价记名义金额）。服务端逐次按已存文件注记、
  // 仅 text 行会带；无上游的这类行归「开发工具订阅」页（features/upstreams/domain.ts
  // 的 agentPricingModel），挂了上游照旧按 API 模型渲染。
  agent: "" | AgentProvider;
  disabled: boolean;
  entry_openai: boolean;
  entry_responses: boolean;
  entry_anthropic: boolean;
  pricing: Pricing | null;
  sources: ModelSource[];
}

// PricingField 是一个价格字段的录入规格。name 与服务端 usage.FieldsFor(kind)
// 的字段名逐字一致（服务端拒绝未知字段，写错即 400）；unit 是这个价格的计价
// 单位——录入一律是「元 / 该单位」，落到线上换算成「微元 / 该单位」。
export interface PricingField {
  name: string;
  group: string; // 表单分节：按厂商计费形态分组
  label: string;
  unit: string;
  hint?: string;
}

// 价格字段名（与服务端 internal/usage/pricing.go 的常量同值）。字段名带厂商
// 前缀是有意的：video 种类下两家的价签字段共用一份 kind 词汇表（服务端
// usage.FieldsFor 按 kind 圈定），前缀使两家价签互不覆盖——模型自身恒单一
// 协议面（0014 起建时声明）。
// 按 token 计价的单位。它是目录价里的**缺省单位**：读数处（PricingItems）只在单位不是
// 它时才写出来，「秒 / 张」这类必须写、「百万 token」不写——录价表单仍逐项标明单位。
export const UnitTokens = t("百万 token");

// pricingFieldsByKind 是各 kind 的价格字段集与排列顺序，镜像服务端的
// usage.FieldsFor(kind)——那边是唯一裁决方，这里加一档价必须同步。
const pricingFieldsByKind: Record<ModelKind, PricingField[]> = {
  text: [
    { name: "in", group: t("文本对话"), label: t("输入价"), unit: UnitTokens },
    { name: "out", group: t("文本对话"), label: t("输出价"), unit: UnitTokens },
    {
      name: "cache_read",
      group: t("文本对话"),
      label: t("缓存命中价"),
      unit: UnitTokens,
      hint: t("留空表示按输入价计"),
    },
    { name: "cache_write", group: t("文本对话"), label: t("缓存写入价"), unit: UnitTokens, hint: t("留空表示按输入价计") },
  ],
  video: [
    { name: "ark_video_token", group: t("火山方舟 Seedance（视频）"), label: t("无参考视频"), unit: UnitTokens },
    { name: "ark_video_token_ref", group: t("火山方舟 Seedance（视频）"), label: t("含参考视频"), unit: UnitTokens },
    { name: "minimax_video_sec_768p", group: t("MiniMax H3（视频）"), label: t("768P 秒价"), unit: t("秒") },
    {
      name: "minimax_video_sec_2k",
      group: t("MiniMax H3（视频）"),
      label: t("2K 秒价"),
      unit: t("秒"),
      hint: t("只配一档时未知分辨率按已配的最高档计"),
    },
    {
      name: "minimax_video_image_extra",
      group: t("MiniMax H3（视频）"),
      label: t("超额参考图"),
      unit: t("张"),
      hint: t("只对超出 5 张免费额度的部分计费"),
    },
    { name: "minimax_context_ir_in", group: t("MiniMax H3（提示词增强）"), label: t("输入价"), unit: UnitTokens },
    { name: "minimax_context_ir_out", group: t("MiniMax H3（提示词增强）"), label: t("输出价"), unit: UnitTokens },
  ],
  image: [
    { name: "ark_image_each", group: t("火山方舟 Seedream（图像）"), label: t("按张计价"), unit: t("张") },
    {
      name: "ark_image_token",
      group: t("火山方舟 Seedream（图像）"),
      label: t("按 token 计价"),
      unit: UnitTokens,
      hint: t("与张价是相加关系，按上游价签只配其一"),
    },
  ],
};

// pricingFieldsFor 返回某 kind 的价格字段规格（副本，调用方改不动内部表）。
export function pricingFieldsFor(kind: ModelKind): PricingField[] {
  return pricingFieldsByKind[kind].map((f) => ({ ...f }));
}

// PricingPairs 是「要么都填、要么都不填」的字段组，与服务端 pricingPairs 同表。
// 只填一半是静默错账（缺的那一档会被按 0 元或按邻档计，而模型仍显示为已定价），
// 服务端因此直接 400——表单必须自己先拦下来，别把这条校验暴露成一句莫名其妙
// 的报错。表外的字段是**有意可选**的（cache_read 缺省按 in、两档秒价互为回退、
// 超额图片附加费、图片张价/token 价二选一），别顺手补进来。
export const PricingPairs: [string, string][] = [
  ["in", "out"],
  ["ark_video_token", "ark_video_token_ref"],
  ["minimax_context_ir_in", "minimax_context_ir_out"],
];

// 与 internal/auth 对齐的输入约束（服务端仍会校验，这里只做提示与预拦截）。
export const PasswordMinLen = 8;
export const PasswordMaxLen = 128;
export const LabelMaxLen = 128;

// 与 internal/admin 的目录校验对齐（catalogNameMaxRunes / apiKey*Runes /
// defaultSourcePriority）。
export const CatalogNameMaxLen = 128;
export const UpstreamKeyMinLen = 8;
export const UpstreamKeyMaxLen = 512;
export const DefaultSourcePriority = 100;

// 计费模式的默认优先级（数小者先）：订阅是已付费套餐先用满 → 100；
// 用量按 token 计费作溢出兜底 → 200。这是挂来源时的默认值，可手动改；
// 数值相同时服务端还有订阅先行的裁决位（store.billingRankSQL）。
export const SubscriptionSourcePriority = 100;
export const UsageSourcePriority = 200;

// ---- 数据升级（仅 admin）----

// OfficialPricingStatus 是数据升级两份文件里价格那份的状态读数。
//
// supported 只回答本进程是否接入官网客户端；从未更新过时版本字段缺席。
export interface OfficialPricingStatus {
  supported: boolean;
  url?: string;
  synced_at?: string;
  version?: number;
  updated_at?: string;
  entries?: number;
}

// PlatformModelsStatus 是同一次同步的第二份：「哪个平台有哪些模型」的选单
// （2026-08-14）。与价格那份有一处**根本不同**——设备恒有固件内嵌的基线，
// 所以 supported=false 只说「此刻更新不了」，不说「没得选」；origin 是当前生效
// 的那份，builtin = 内嵌基线，synced = 同步来的更新版本（比版本号，不比时间）。
// supported 与已存读数的解耦口径同上。
export interface PlatformModelsStatus {
  supported: boolean;
  url?: string;
  origin: "builtin" | "synced";
  version: number;
  updated_at?: string;
  builtin_version: number;
  platforms: number;
  models: number;
  synced_at?: string;
}

// CatalogAutoStatus 是每小时数据升级的读数。enabled = 已接入官网客户端。
//
// 两点别读错：
//   interval_seconds 是自动检查周期。
//   两个时刻都是**内存态，进程重启即空**。所以面板上的「最后更新」必须取两份
//     文件已落库的 synced_at 的最大值（那是持久事实），last_updated_at 只作
//     本次运行期内「刚刚真的取到新文件」的补充读数。
export interface CatalogAutoStatus {
  enabled: boolean;
  interval_seconds: number;
  last_checked_at?: string;
  last_updated_at?: string;
}

export interface DataStatus {
  official_pricing: OfficialPricingStatus;
  platform_models: PlatformModelsStatus;
  automatic: CatalogAutoStatus;
}

export function getDataStatus(): Promise<DataStatus> {
  return request("GET", "/admin/v1/system/data");
}

// ---- 固件升级（仅 admin，docs/firmware-update.md）----

// FirmwareRelease 是官网发布目录的一行。
export interface FirmwareRelease {
  version: string;
  channel: string;
  artifactUrl: string;
  artifactSha256: string;
  sizeBytes: number;
  releaseNotes: string;
  minVersion: string;
  publishedAt?: string;
}

// FirmwareAdvisory 是最近一次升级情报：latest 是现在就能装的那个（可能是
// 垫脚石中间版本），newest 是官网索引里最新的那个；source 当前恒为 website。
export interface FirmwareAdvisory {
  latest?: FirmwareRelease | null;
  newest?: FirmwareRelease | null;
  upgrade_available: boolean;
  checked_at: string;
  source: string;
}

export interface FirmwareStaged {
  version: string;
  sha256: string;
  size_bytes: number;
  source: string; // website | upload
  staged_at: string;
}

export interface FirmwareResult {
  kind: string; // install | rollback
  from_version: string;
  to_version: string;
  outcome: string; // success | rolled_back | failed
  reason?: string;
  finished_at: string;
}

// FirmwareEngine 是升级引擎（llmgate-updated）读数；available=false = socket
// 不可达（未部署引擎的老板子 / 开发机），此时安装与回退不可用、其余照常。
export interface FirmwareEngine {
  available: boolean;
  busy?: boolean;
  phase?: string;
  installed_version?: string;
  prev_version?: string;
  last_result?: FirmwareResult | null;
}

export interface FirmwareStatus {
  // 固件唯一的版本名称（形如 26082217-2d81）。产品里没有第二个版本号。
  current_version: string;
  advisory?: FirmwareAdvisory | null;
  downloading: boolean;
  download_error?: string;
  staged?: FirmwareStaged | null;
  engine: FirmwareEngine;
}

export function getFirmwareStatus(): Promise<FirmwareStatus> {
  return request("GET", "/admin/v1/system/firmware");
}

export function firmwareCheck(): Promise<FirmwareStatus> {
  return request("POST", "/admin/v1/system/firmware/check");
}

// firmwareDownload 服务端同步陪等最多 60s：小包一次返回就绪，大包/慢链路
// 返回「下载中」快照，结果落在状态里、刷新可见（零轮询）。
export function firmwareDownload(): Promise<FirmwareStatus> {
  return request("POST", "/admin/v1/system/firmware/download");
}

export function firmwareInstall(): Promise<FirmwareStatus> {
  return request("POST", "/admin/v1/system/firmware/install");
}

export function firmwareRollback(): Promise<FirmwareStatus> {
  return request("POST", "/admin/v1/system/firmware/rollback");
}

export function firmwareDiscardStaged(): Promise<FirmwareStatus> {
  return request("DELETE", "/admin/v1/system/firmware/staged");
}

// firmwareUpload 上传固件包：全管理台唯一的非 JSON 变更请求——裸二进制体
// （application/octet-stream），CSRF 头照旧。走 client.ts 的 requestBinary，
// 与 request 同一套错误解码与 401 回调。
export function firmwareUpload(file: File): Promise<FirmwareStatus> {
  return requestBinary<FirmwareStatus>("/admin/v1/system/firmware/upload", file);
}

// ---- 会话 ----
//
// 设备只有一个登录口令（出厂 llm-gate），登录因此没有用户名这一项。
// login 是唯一的无会话管理 API。

export function login(password: string): Promise<void> {
  return request("POST", "/admin/v1/login", { password });
}

export function logout(): Promise<void> {
  return request("POST", "/admin/v1/logout");
}

// session 是 boot 探针：有会话 204，没有则 401（客户端据此送去登录页）。
// 它刻意不回 body——设备没有「当前用户」这种东西可答。
export function session(): Promise<void> {
  return request("GET", "/admin/v1/session");
}

// firmwareVersion 取登录页规格铭牌的两项公开事实：固件唯一版本名称与服务端
// 按 boardinfo 识别的本机型号。**免会话**，因为登录页在会话之前。登录后的
// 顶栏不走这条——两项都搭 getEndpoints 那趟车回来，仍只发一个请求。
export function firmwareVersion(): Promise<{ version: string; hardware_model: string }> {
  return request("GET", "/admin/v1/version");
}

// changePassword 改登录口令（验旧口令）：成功后服务端清空全部旧会话并为当前
// 浏览器经 Set-Cookie 轮换新会话，无需重新登录。
export function changePassword(oldPassword: string, newPassword: string): Promise<void> {
  return request("POST", "/admin/v1/password", {
    old_password: oldPassword,
    new_password: newPassword,
  });
}

// ---- 接入读数（使用API页用） ----

// AccessAddress 是设备的一个对外 IPv4 地址；primary 表示本页这条连接就落在
// 它上面，对当前浏览器一定可达（服务端已把它排在最前）。
export interface AccessAddress {
  host: string;
  interface: string;
  primary: boolean;
}

// DeviceEndpoints 是「怎么连到这台设备」的地址读数。浏览器只知道自己走的
// 那一条——从域名打开管理台的看不到内网 IP，设备有两块网卡时也只看得到一块，
// 端口更是只对当前这条路径成立，所以端口也由设备给。
export interface DeviceEndpoints {
  addresses: AccessAddress[];
  // http_port 是设备的明文监听端口；内网 IP 直连恒走它。缺省（0/undefined）
  // 表示服务端没能给出，界面退回浏览器当前端口。
  http_port?: number;
  // 当前生效的公网接入基址：外网映射登记的地址，或 Cloudflare Tunnel 启用后的
  // https://<hostname>（两者二选一，见 ExternalAccess）。设备只公布地址；外网
  // 映射的链路由管理员维护，Tunnel 由设备自己连出。
  external_url?: string;
  // lan_domain_url 是经 LLM Gate官网申领的内网域名 HTTPS 地址（LanDomainStatus）：
  // 证书覆盖域名且 HTTPS 监听在跑时才出现。它解析到设备的内网 IP，只在内网可达。
  lan_domain_url?: string;
}

// ServableModel 是客户端现在就能调用的一个模型：名字直接填进请求的 model
// 字段。protocols 为空数组说明它列得出但三个协议面都不可用（来源的上游类型
// 不服务任何入口），界面照实标出来。
export interface ServableModel {
  name: string;
  protocols: Protocol[];
}

// AIGCModel 是一个视频/图像模型的最小读数。模型固定一个厂商协议面（源头厂商
// 官方接口），api 字段就是它的协议标识（如 minimax_video）、protocol_face 是厂商段
// （如 minimax）——它们是客户端契约的一部分（客户端要按该厂商官方文档构造请求、
// 打「接入地址 + /<厂商段>」），所以对 member 可见；使用API页据此渲染正确的官方
// 路径与示例。available 为假 = 名字列得出但来源全挂在不服务该种类协议面的上游上，
// 提交一律 404。
export interface AIGCModel {
  name: string;
  kind: "video" | "image";
  api?: string;
  protocol_face?: ProtocolFace;
  available: boolean;
}

// AgentsAccess 是「这台设备现在能不能当某种 agent 后端用」的读数（迭代 11；
// 2026-08-12 起每个 provider 一项）。available=false 时「使用Agent」页对应分节
// 不渲染启动命令，只说该找谁开通——账号的名称、账号标识、状态与凭据都不在
// 这里，那些是管理员视角（「模型接入 → 开发工具订阅」页，仅 admin）。
export interface AgentsAccess {
  /** 这一项说的是哪一种 agent（codex | grok | claude | cursor）；四项恒在场。 */
  provider: AgentProvider;
  /** 存在一份已连接、启用且可用的订阅（需重新登录的那种算不可用）。 */
  available: boolean;
  /** 管理员设的默认模型，写进对应 CLI 的配置；空 = 未设或当前不可用。 */
  default_model: string;
}

export interface APIModelCount {
  text: number;
  aigc: number;
}

// AccessSnapshot 是使用API页要的全部读数，一次请求取齐。
export interface AccessSnapshot {
  endpoints: DeviceEndpoints;
  models: ServableModel[];
  // aigc_models 是视频/图像模型清单；没有这类模型时字段缺省。
  aigc_models?: AIGCModel[];
  // 管理顶栏按可用来源的计费模式分别去重；同一模型可在两组各计一次。
  // Key 接入页复用的快照与降级快照不含这项管理读数。
  api_model_counts?: Record<BillingMode, APIModelCount>;
  // agents 是各订阅代理的可用性数组（服务端恒在场、恒含四种 provider；
  // 可选只为容纳降级快照——读不到读数时整段缺省）。
  agents?: AgentsAccess[];
  // firmware_version 是固件唯一的版本名称（形如 26082217-2d81）。它搭这趟车
  // 是为了守住外壳顶栏「挂载时只发一个请求」那条纪律（topbar.tsx 开头）。
  firmware_version: string;
  // hardware_model 是服务端按本机 boardinfo 型号档案识别出的技术型号代号。
  // 空串表示平台无法识别，界面不显示该格。
  hardware_model: string;
  // 本地管理显示名；Key 接入页和公开铭牌不含此项。
  device_name?: string;
}

// AgentProvider 是受支持的 agent 订阅类型。用户可见文案里 codex 说
// 「Codex」、grok 说「Grok Build」、claude 说「Claude Code」、cursor 说「Cursor」。
export type AgentProvider = "codex" | "grok" | "claude" | "cursor";
// OAuthAgentProvider 使用通用登录/导入端点；Claude 使用专用手动授权码端点。
export type OAuthAgentProvider = Exclude<AgentProvider, "claude" | "cursor">;
export type DevToolName = AgentProvider | "opencode";

// devToolLabels 是用户可见名字的完整查表：新 provider 忘了登记会被类型检查
// 拦下，而不是悄悄顶成别家的名字。
const devToolLabels: Record<DevToolName, string> = {
  codex: "Codex",
  grok: "Grok Build",
  claude: "Claude Code",
  cursor: "Cursor",
  opencode: "OpenCode",
};

// agentProviderLabel 是用户可见的 provider 名。入参保持宽类型（Model.agent
// 这类服务端串）；查不到的值原样透出，不顶成别家的名字。
export function agentProviderLabel(provider: string): string {
  return devToolLabels[provider as DevToolName] ?? provider;
}

export function devToolLabel(tool: DevToolName): string {
  return devToolLabels[tool];
}

// getEndpoints 取接入读数：页面加载读一次（无轮询），服务端只做一次网卡
// 枚举、一次外网映射点查与必要的模型、订阅读取，不给设备添负担。
export function getEndpoints(): Promise<AccessSnapshot> {
  return request("GET", "/admin/v1/endpoints");
}

export function setDeviceName(name: string): Promise<{ name: string }> {
  return request("PUT", "/admin/v1/system/device-name", { name });
}

// ---- 凭 API 密钥自证的只读读数（「接入方法」页 /ui/connect，无会话） ----
//
// 两条端点都在数据面、都只认 Bearer Key（keyRequest）：
// - /gate-helper/v1/endpoints 是 getEndpoints 那份读数的按 Key 版本——地址与铭牌
//   同一份，模型清单按这把 Key 的「可用模型」范围裁剪，**没有 agents**；
// - /gate-helper/v1/config 是 gate 每次启动前读的同一份开发工具策略：四种订阅对这把
//   Key 可不可用、每个工具能看见哪些模型。页面据它拼出 AgentsAccess 交给
//   DevToolGuide，并把各工具的可见模型列给使用者看。

export interface KeyAccessSnapshot {
  endpoints: DeviceEndpoints;
  models: ServableModel[];
  aigc_models?: AIGCModel[];
  firmware_version: string;
  hardware_model: string;
}

export interface DevToolPolicySubscription {
  provider: AgentProvider;
  /** 管理员为这把 Key 勾了该订阅。 */
  configured: boolean;
  /** 勾了、且设备上该订阅已连接可用。 */
  available: boolean;
  default_model: string;
}

export interface DevToolPolicyModel {
  name: string;
  source: "subscription" | "catalog";
}

export interface DevToolPolicyTool {
  default_model: string;
  models: DevToolPolicyModel[];
}

export interface DevToolPolicy {
  schema_version: number;
  revision: number;
  subscriptions: DevToolPolicySubscription[];
  tools: Partial<Record<DevToolName, DevToolPolicyTool>>;
}

export function keyEndpoints(key: string): Promise<KeyAccessSnapshot> {
  return keyRequest("/gate-helper/v1/endpoints", key);
}

export function keyDevToolConfig(key: string): Promise<DevToolPolicy> {
  return keyRequest("/gate-helper/v1/config", key);
}

// ---- 公网接入方式（网络/域名/代理页，仅 admin） ----

// ExternalMode 是公网接入方式三态：none 不开放、manual 外网映射、cloudflare
// Cloudflare Tunnel。两种设置分别保留，切换时不要求重填；Tunnel 启用中不能切走
// （服务端 409 cloudflare_tunnel_active）。
export type ExternalMode = "none" | "manual" | "cloudflare";

export interface ExternalAccess {
  mode: ExternalMode;
  manual_url: string;
  // external_url 是按 mode 派生的当前生效基址；none 或 Tunnel 未启用时缺席。
  external_url?: string;
  cloudflare_enabled: boolean;
}

export function getExternal(): Promise<{ external: ExternalAccess }> {
  return request("GET", "/admin/v1/system/external");
}

// setExternal 选择方式并保存外网映射地址（manual 模式必须非空；其余模式下也会
// 保存，空串即清除）。
export function setExternal(mode: ExternalMode, manualURL: string): Promise<{ external: ExternalAccess }> {
  return request("PUT", "/admin/v1/system/external", { mode, manual_url: manualURL });
}

// ---- 出站代理（网络/域名/代理页，仅 admin；docs-dev/firmware-egress-proxy.md） ----

// EgressProvider：""（不使用代理）| socks5 | clash（连接已有 Clash / Mihomo 的 SOCKS 端口）|
// mihomo（Clash 订阅：设备内置内核，地址由固件管理，见 ProxyCoreStatus）。前两种都只经标准
// SOCKS5 CONNECT 通信，区别只在界面引导。
export type EgressProvider = "" | "socks5" | "clash" | "mihomo";
// EgressScope 是六类互联网出口；每类各自选 direct / proxy，缺省全部 direct。
export type EgressScope = "model_api" | "agent_auth" | "official_site" | "cli_artifacts" | "component_artifacts" | "proxy_subscription";
export type EgressRoute = "direct" | "proxy";
// EgressMode 是单个上游账号对 model_api 出口的覆盖。
export type EgressMode = "inherit" | "direct" | "proxy";

export interface EgressTestStage {
  name: "proxy_tcp" | "socks5" | "tls" | "https" | string;
  ok: boolean;
  latency_ms: number;
  detail?: string;
}

export interface EgressTestResult {
  ok: boolean;
  at: string;
  target: string;
  stages: EgressTestStage[];
  category?: string;
  message?: string;
}

export interface EgressPassiveError {
  category: string;
  message: string;
  at: string;
}

export interface EgressStatus {
  provider: EgressProvider;
  address: string;
  username_set: boolean;
  password_set: boolean;
  // configured：已声明代理端点；available：凭据可用（解封成功）。不可用时选了代理的流量失败关闭。
  configured: boolean;
  available: boolean;
  unavailable_reason?: string;
  routes: Record<EgressScope, EgressRoute>;
  last_test?: EgressTestResult;
  last_errors?: Partial<Record<EgressScope, EgressPassiveError>>;
  providers: { id: EgressProvider; label: string }[];
  scopes: { id: EgressScope; label: string; route: EgressRoute }[];
  overrides: { upstream_id: number; name: string; egress_mode: EgressMode; disabled: boolean }[];
}

// EgressPatch：字段全部可选；username / password 省略即保留、空串即清除；provider 传 ""
// 清除整个端点（仍有分类或账号经代理时服务端拒绝）。routes 只覆盖给出的分类。
export interface EgressPatch {
  provider?: EgressProvider;
  address?: string;
  username?: string;
  password?: string;
  routes?: Partial<Record<EgressScope, EgressRoute>>;
}

type EgressReply = { egress: EgressStatus };

export function getEgress(): Promise<EgressReply> {
  return request("GET", "/admin/v1/system/egress");
}

export function updateEgress(patch: EgressPatch): Promise<EgressReply> {
  return request("PATCH", "/admin/v1/system/egress", patch);
}

// testEgress 对固定 HTTPS 目标做四层诊断；结果随读数的 last_test 返回。
export function testEgress(): Promise<EgressReply> {
  return request("POST", "/admin/v1/system/egress/test", {});
}

// ---- Clash 订阅 / 设备内置内核（出站代理分区，仅 admin；docs-dev/firmware-egress-proxy.md §8） ----

// ProxyCoreComponent 是 Mihomo 组件层读数：与 cloudflared 同一形状（ComponentStatus），
// 清单条目多两个 gzip 字段（ComponentRelease.unpacked*）。
export type ProxyCoreComponent = ComponentStatus;

// ProxyCoreCore 是内核 unit 读数。state：stopped / starting / running / failed / unknown。
export interface ProxyCoreCore {
  engine_available: boolean;
  state: string;
  unit_state?: string;
  restarts: number;
  ready: boolean;
}

export interface ProxyCoreUserInfo {
  upload: number;
  download: number;
  total: number;
  expire: number;
}

// ProxyCoreSubscription 是订阅读数：只有站点主机名、节点数与用量，不含地址与节点凭据。
export interface ProxyCoreSubscription {
  set: boolean;
  host?: string;
  fetched_at?: string;
  node_count: number;
  dropped: number;
  user_info?: ProxyCoreUserInfo | null;
  last_error?: string;
}

// ProxyCoreNode 的 latency_ms 是最近一轮测试直连节点服务器的 TCP 连接耗时（毫秒，成功至少 1）；
// latency_failed 表示那轮没连上。两者都缺席 = 还没测过。
export interface ProxyCoreNode {
  name: string;
  type: string;
  latency_ms?: number;
  latency_failed?: boolean;
}

export interface ProxyCoreStatus {
  enabled: boolean;
  license_accepted: boolean;
  port: number;
  // address 是内核起来后出站代理指向的本机地址（127.0.0.1:<port>）。
  address: string;
  platform: string;
  component: ProxyCoreComponent;
  core: ProxyCoreCore;
  subscription: ProxyCoreSubscription;
  nodes: ProxyCoreNode[];
  // selected 是选定节点名；空 = 自动选择。
  selected: string;
  // latency_tested_at 是最近一轮节点延迟测试的时刻（没测过则缺席）。
  latency_tested_at?: string;
  last_error?: string;
}

type ProxyCoreReply = { proxy_core: ProxyCoreStatus };

export function getProxyCore(): Promise<ProxyCoreReply> {
  return request("GET", "/admin/v1/system/proxy-core");
}

// setProxyCoreSubscription 保存订阅地址（服务端密封）并立即拉取；地址只在这次请求体里出现。
export function setProxyCoreSubscription(url: string): Promise<ProxyCoreReply> {
  return request("PUT", "/admin/v1/system/proxy-core/subscription", { url });
}

export function refreshProxyCoreSubscription(): Promise<ProxyCoreReply> {
  return request("POST", "/admin/v1/system/proxy-core/subscription/refresh", {});
}

export function clearProxyCoreSubscription(): Promise<ProxyCoreReply> {
  return request("DELETE", "/admin/v1/system/proxy-core/subscription");
}

export function selectProxyCoreNode(name: string): Promise<ProxyCoreReply> {
  return request("PUT", "/admin/v1/system/proxy-core/node", { name });
}

// testProxyCoreLatency 让设备直连各节点服务器测一轮 TCP 连接延迟；结果随读数的 nodes 返回。
export function testProxyCoreLatency(): Promise<ProxyCoreReply> {
  return request("POST", "/admin/v1/system/proxy-core/latency", {});
}

export function enableProxyCore(acceptLicense: boolean): Promise<ProxyCoreReply> {
  return request("POST", "/admin/v1/system/proxy-core/enable", { accept_license: acceptLicense });
}

export function disableProxyCore(): Promise<ProxyCoreReply> {
  return request("POST", "/admin/v1/system/proxy-core/disable", {});
}

type MihomoComponentReply = { component: ProxyCoreComponent };

export function mihomoCheck(): Promise<MihomoComponentReply> {
  return request("POST", "/admin/v1/system/components/mihomo/check", {});
}

export function mihomoImportManifest(index: string, signature: string): Promise<MihomoComponentReply> {
  return request("POST", "/admin/v1/system/components/mihomo/manifest", { index, signature });
}

export function mihomoDownload(): Promise<MihomoComponentReply> {
  return request("POST", "/admin/v1/system/components/mihomo/download", {});
}

export function mihomoUpload(file: File): Promise<MihomoComponentReply> {
  return requestBinary<MihomoComponentReply>("/admin/v1/system/components/mihomo/upload", file);
}

export function mihomoInstall(): Promise<MihomoComponentReply> {
  return request("POST", "/admin/v1/system/components/mihomo/install", {});
}

export function mihomoRollback(): Promise<MihomoComponentReply> {
  return request("POST", "/admin/v1/system/components/mihomo/rollback", {});
}

export function mihomoDiscard(): Promise<MihomoComponentReply> {
  return request("DELETE", "/admin/v1/system/components/mihomo/staged");
}

// mihomoRemove 卸载 Mihomo 内核组件（整个 A/B 槽位目录）；内核启用中服务端答 409
// component_in_use，订阅与节点选择保留。只限 LAN。
export function mihomoRemove(): Promise<MihomoComponentReply> {
  return request("DELETE", "/admin/v1/system/components/mihomo");
}

// ---- 内网域名（网络/域名/代理页，仅 admin；docs-dev/lan-domain.md） ----

// LanDomainProvider：""（不使用）| official_site（LLM Gate官网申领 <label>.llm.net）|
// own_domain（管理员自己的域名：A 记录自己指到设备内网 IP，_acme-challenge CNAME 委托给官网
// 做 DNS-01，证书仍由官网向 CA 申请）。两种都先关联官网账号。
export type LanDomainProvider = "" | "official_site" | "own_domain";

export interface LanDomainProviderOption {
  id: LanDomainProvider;
  label: string;
  available: boolean;
}

// LanDomainPending 是进行中的官网账号关联：管理员把 user_code 拿到官网 /link/ 页确认。
export interface LanDomainPending {
  user_code: string;
  verification_url: string;
  verification_url_complete: string;
  expires_at: string;
  status: "pending" | "approved" | "denied" | "expired" | "failed";
  error?: string;
}

export interface LanDomainCert {
  not_before: string;
  not_after: string;
  issuer: string;
  sans: string[];
  expiring_soon: boolean;
  covers_hostname: boolean;
}

// LanDNSRecord 是自有域名持有人要在自己的 DNS 服务商设置的一条记录。
export interface LanDNSRecord {
  type: "A" | "CNAME" | string;
  name: string;
  value: string;
}

export type LanDNSCheckStatus = "ok" | "mismatch" | "missing" | "error";

// LanDNSCheck 是官网用公共解析器核对自有域名三条记录的结果。ready：签发的前置条件（委托 CNAME
// 就位且 CAA 不拦）都满足；A 记录只影响能不能访问，不影响签发。
export interface LanDNSCheck {
  hostname: string;
  acme_delegate: string;
  target_ip: string;
  a: { status: LanDNSCheckStatus; addresses: string[] };
  challenge: { status: LanDNSCheckStatus; target?: string };
  caa: { status: "ok" | "blocked" | "none" | "error"; found_at?: string; records: string[]; permitted: string[] };
  ready: boolean;
  checked_at: string;
}

// LanIssueStage：submitting（设备生成密钥/CSR 并提交订单）→ pending（官网向 CA 建订单）→
// authorizing（写 DNS 验证记录）→ challenging（等公共解析器看到记录）→ validating（CA 验证中）→
// finalizing（CA 签发中）→ installing（设备装证书、起 HTTPS）。
export type LanIssueStage = "submitting" | "pending" | "authorizing" | "challenging" | "validating" | "finalizing" | "installing";

export interface LanDomainStatus {
  provider: LanDomainProvider;
  providers: LanDomainProviderOption[];
  site_configured: boolean;
  suffix: string;
  link: { linked: boolean; account?: string; pending?: LanDomainPending };
  claimed: boolean;
  // kind：managed（官网申领）/ custom（自有域名）；未选提供方式时缺席。
  kind?: "managed" | "custom";
  label?: string;
  hostname?: string;
  target_ip?: string;
  // acme_delegate / dns_records 只在自有域名登记后出现。
  acme_delegate?: string;
  dns_records?: LanDNSRecord[];
  // url 是证书就绪且 HTTPS 监听在跑时的访问地址。
  url?: string;
  issuing: boolean;
  // issue_stage 是进行中签发的阶段（LanIssueStage）；不在签发时缺席。
  issue_stage?: LanIssueStage;
  cert?: LanDomainCert;
  last_ca?: string;
  last_issued_at?: string;
  last_error?: string;
  last_error_at?: string;
  https_listen: string;
  https_active: boolean;
  https_addr?: string;
  addresses: AccessAddress[];
}

type LanDomainReply = { lan_domain: LanDomainStatus };

export function getLanDomain(): Promise<LanDomainReply> {
  return request("GET", "/admin/v1/system/lan-domain");
}

export function updateLanDomain(patch: { provider?: LanDomainProvider; https_listen?: string }): Promise<LanDomainReply> {
  return request("PUT", "/admin/v1/system/lan-domain", patch);
}

export function lanDomainLinkStart(): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/link/start", {});
}

// lanDomainLinkWait 由服务端同步陪等一轮（≤25 秒）官网确认；关联对话框开着时反复调，
// 直到 pending 进入终态。
export function lanDomainLinkWait(): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/link/wait", {});
}

export function lanDomainLinkCancel(): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/link/cancel", {});
}

export function lanDomainUnlink(): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/unlink", {});
}

// lanDomainClaim 申领域名并随即签发证书；服务端陪等 ≤60 秒，超时回 issuing=true 的快照。
export function lanDomainClaim(label: string, targetIP: string): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/claim", { label, target_ip: targetIP });
}

// lanDomainRegister 登记自有域名（不签发：先由管理员按读数里的 dns_records 设好 DNS）。
export function lanDomainRegister(hostname: string, targetIP: string): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/register", { hostname, target_ip: targetIP });
}

// lanDomainDNSCheck 请官网核对自有域名的公开 DNS 记录；只读。
export function lanDomainDNSCheck(): Promise<LanDomainReply & { dns_check: LanDNSCheck }> {
  return request("POST", "/admin/v1/system/lan-domain/dns-check", {});
}

// lanDomainIssue 发起签发/续期；服务端陪等到阶段离开 submitting 或结束（失败 502 issue_failed）。
export function lanDomainIssue(): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/certificate", {});
}

// lanDomainIssueWait 陪等进行中的签发：since 是界面已看到的阶段，阶段变了或签发结束即返回
// （失败 502 issue_failed）；没有签发在进行时立即回快照。签发中反复调直到 issuing=false。
export function lanDomainIssueWait(since: LanIssueStage | ""): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/certificate/wait", { since });
}

export function lanDomainSetTarget(targetIP: string): Promise<LanDomainReply> {
  return request("PUT", "/admin/v1/system/lan-domain/target", { target_ip: targetIP });
}

export function lanDomainRelease(): Promise<LanDomainReply> {
  return request("POST", "/admin/v1/system/lan-domain/release", {});
}

// ---- Cloudflare Tunnel（仅 admin，docs-dev/firmware-cloudflare-tunnel.md） ----

export type CloudflareExposure = "api_only" | "api_and_admin";

export interface CloudflareConfig {
  hostname: string;
  exposure: CloudflareExposure;
  auto_update: boolean;
}

// ComponentRelease 是签名组件清单里的一个精确版本条目。
export interface ComponentRelease {
  version: string;
  platform: string;
  artifactUrl: string;
  artifactSha256: string;
  sizeBytes: number;
  // gzip 打包的组件（Mihomo）另带解压后长度/摘要与许可证链接。
  packaging?: string;
  unpackedSha256?: string;
  unpackedSizeBytes?: number;
  licenseUrl?: string;
  license: string;
  sourceUrl?: string;
  minComponentManager: number;
  allowInstall: boolean;
  allowUpdate: boolean;
  blocked: boolean;
  note?: string;
  publishedAt?: string;
}

// ComponentAdvisory 是按本机平台与已装版本筛出的安装建议：latest 是现在就能装的，
// newest 是清单里最新的（可能因阻断或许可不可装）。
export interface ComponentAdvisory {
  revision: number;
  source: string; // website | upload | stored
  checked_at?: string;
  latest?: ComponentRelease | null;
  newest?: ComponentRelease | null;
  installed_blocked: boolean;
  needs_firmware?: boolean;
}

export interface ComponentStaged {
  version: string;
  sha256: string;
  size_bytes: number;
  source: string; // website | upload
  staged_at: string;
}

// ComponentStatus 是第三方组件（cloudflared、Mihomo 内核）共用的组件层读数，「第三方组件」页
// 与各功能页的先决条件卡都只认它。state：engine_unavailable / downloading / blocked / staged /
// not_installed / update_available / installed。
export interface ComponentStatus {
  engine_available: boolean;
  state: string;
  installed: boolean;
  version?: string;
  slot?: string;
  previous_version?: string;
  staged?: ComponentStaged | null;
  advisory?: ComponentAdvisory | null;
  downloading: boolean;
  download_error?: string;
  website_enabled: boolean;
}

export type CloudflareComponent = ComponentStatus;

// CloudflareConnector 是 connector 层读数。state：stopped / connecting /
// connected / degraded / unknown。
export interface CloudflareConnector {
  engine_available: boolean;
  state: string;
  unit_state?: string;
  restarts: number;
  ready_connections: number;
}

// CloudflareOrigin 是 origin 层读数。state：not_listening / listening / admin_gated。
export interface CloudflareOrigin {
  state: string;
  listening: boolean;
  admin_gate_ok: boolean;
  hostname?: string;
  exposure?: string;
  socket_exists: boolean;
}

export interface CloudflareTestCheck {
  layer: "component" | "connector" | "origin" | "public" | string;
  ok: boolean;
  detail: string;
}

export interface CloudflareTestReport {
  at: string;
  public: boolean;
  ok: boolean;
  checks: CloudflareTestCheck[];
}

// CloudflareStatus 是四层状态 + 配置（不含 token）。credential.state：unset /
// sealed / unreadable。service_url 是要逐字填进 Cloudflare Published application
// 的 Service URL。
export interface CloudflareStatus {
  config: CloudflareConfig;
  enabled: boolean;
  external_url?: string;
  socket: string;
  platform: string;
  credential: { state: "unset" | "sealed" | "unreadable" | string };
  component: CloudflareComponent;
  connector: CloudflareConnector;
  origin: CloudflareOrigin;
  last_test?: CloudflareTestReport | null;
  last_error?: string;
  mode: ExternalMode;
  service_url: string;
}

export function getCloudflareTunnel(): Promise<{ tunnel: CloudflareStatus }> {
  return request("GET", "/admin/v1/system/cloudflare-tunnel");
}

// CloudflarePatch 三态：字段缺席 = 不改；token 缺席 = 保留已密封的那把，
// clear_token 才销毁。token 只走这一次请求，任何响应都不回显。
export interface CloudflarePatch {
  hostname?: string;
  exposure?: CloudflareExposure;
  auto_update?: boolean;
  token?: string;
  clear_token?: boolean;
}

export function updateCloudflareTunnel(patch: CloudflarePatch): Promise<{ tunnel: CloudflareStatus }> {
  return request("PUT", "/admin/v1/system/cloudflare-tunnel", patch);
}

// enableCloudflareTunnel 要带两道确认：接受 Cloudflare 的第三方数据路径与组件
// 许可证；exposure 为 api_and_admin 时再确认把管理面暴露到公网。
export function enableCloudflareTunnel(
  acceptThirdParty: boolean,
  confirmAdminExposure: boolean,
): Promise<{ tunnel: CloudflareStatus }> {
  return request("POST", "/admin/v1/system/cloudflare-tunnel/enable", {
    accept_third_party: acceptThirdParty,
    confirm_admin_exposure: confirmAdminExposure,
  });
}

export function disableCloudflareTunnel(keepToken: boolean): Promise<{ tunnel: CloudflareStatus }> {
  return request("POST", "/admin/v1/system/cloudflare-tunnel/disable", { keep_token: keepToken });
}

// testCloudflareTunnel 本地分层自检；publicProbe 为真时设备再对 https://<hostname>/healthz
// 探一次。服务端不设长等待，但公网探测可能要十几秒。
export function testCloudflareTunnel(
  publicProbe: boolean,
): Promise<{ report: CloudflareTestReport; tunnel: CloudflareStatus }> {
  return request("POST", "/admin/v1/system/cloudflare-tunnel/test", { public: publicProbe });
}

export function deleteCloudflareTunnel(removeComponent: boolean): Promise<{ tunnel: CloudflareStatus }> {
  return request("POST", "/admin/v1/system/cloudflare-tunnel/delete", { remove_component: removeComponent });
}

export function getCloudflaredComponent(): Promise<{ component: CloudflareComponent }> {
  return request("GET", "/admin/v1/system/components/cloudflared");
}

export function cloudflaredCheck(): Promise<{ component: CloudflareComponent }> {
  return request("POST", "/admin/v1/system/components/cloudflared/check");
}

// cloudflaredImportManifest 离线导入官网发布的签名清单（stable.json 与 stable.json.sig
// 的原文），走与在线检查同一套验签与防回退。
export function cloudflaredImportManifest(
  index: string,
  signature: string,
): Promise<{ component: CloudflareComponent }> {
  return request("POST", "/admin/v1/system/components/cloudflared/manifest", { index, signature });
}

// cloudflaredDownload 服务端同步陪等最多 60s；大包/慢链路返回「拉取中」快照，
// 结果落在状态里、刷新可见（零轮询）。**不能加请求超时**。
export function cloudflaredDownload(): Promise<{ component: CloudflareComponent }> {
  return request("POST", "/admin/v1/system/components/cloudflared/download");
}

// cloudflaredUpload 上传管理员在电脑上从官方 release 下载的制品：裸二进制体，
// 摘要必须命中清单条目。
export function cloudflaredUpload(file: File): Promise<{ component: CloudflareComponent }> {
  return requestBinary<{ component: CloudflareComponent }>("/admin/v1/system/components/cloudflared/upload", file);
}

export function cloudflaredInstall(): Promise<{ component: CloudflareComponent }> {
  return request("POST", "/admin/v1/system/components/cloudflared/install");
}

export function cloudflaredRollback(): Promise<{ component: CloudflareComponent }> {
  return request("POST", "/admin/v1/system/components/cloudflared/rollback");
}

export function cloudflaredDiscard(): Promise<{ component: CloudflareComponent }> {
  return request("DELETE", "/admin/v1/system/components/cloudflared/staged");
}

// cloudflaredRemove 卸载 cloudflared 组件（整个 A/B 槽位目录）；Tunnel 启用中服务端答 409
// component_in_use，本机 Tunnel 设置与密封 token 保留。只限 LAN。
export function cloudflaredRemove(): Promise<{ component: CloudflareComponent }> {
  return request("DELETE", "/admin/v1/system/components/cloudflared");
}

// ---- API密钥 ----

export function listKeys(): Promise<{ keys: ApiKey[] }> {
  return request("GET", "/admin/v1/keys");
}

// createKey 签发一把 Key。响应带 plaintext 供当场复制；此后随时可经 revealKey
// 重新取回（明文封存入库）。
export function createKey(label: string): Promise<{ key: ApiKey; plaintext: string }> {
  return request("POST", "/admin/v1/keys", { label });
}

// revealKey 解封 Key 明文（自助复制）。签发于旧版本固件的 Key 无封存明文，
// 409 plaintext_unavailable（列表的 plaintext_available 已预告，正常路径不会
// 走到）。
export function revealKey(id: number): Promise<{ plaintext: string }> {
  return request("POST", `/admin/v1/keys/${id}/plaintext`);
}

// KeyPatch 只列可写字段。label 空串 = 清除标签；限额四字段是**三态**：
// 不传 = 本次不改，null = 清除限额（不限），数字 = 设成这个值（0 是合法额度，
// 一律拒绝，与 null 泾渭分明）。服务端 internal/admin/limits.go 是同一套语义。
export interface KeyPatch {
  label?: string;
  disabled?: boolean;
  budget_day_micro?: number | null;
  budget_week_micro?: number | null;
  budget_month_micro?: number | null;
  rpm_limit?: number | null;
}

export function updateKey(id: number, patch: KeyPatch): Promise<{ key: ApiKey }> {
  return request("PATCH", `/admin/v1/keys/${id}`, patch);
}

export function setKeyDisabled(id: number, disabled: boolean): Promise<{ key: ApiKey }> {
  return updateKey(id, { disabled });
}

// adjustKeyMeteredAllowance 增量调整按量额度：正数增加、负数扣减。不能用 PATCH
// 设置绝对值，否则会覆盖同一时刻计量器正在落库的消费扣减。
export function adjustKeyMeteredAllowance(id: number, deltaMicro: number): Promise<{ key: ApiKey }> {
  return request("POST", `/admin/v1/keys/${id}/metered-allowance`, { delta_micro: deltaMicro });
}

// deleteKey 删除一把 Key（不可撤销：封存明文随行删除，删了只能重签一把）。
export function deleteKey(id: number): Promise<void> {
  return request("DELETE", `/admin/v1/keys/${id}`);
}

export interface DevToolSubscriptionChoices {
  codex: boolean;
  grok: boolean;
  claude: boolean;
  cursor: boolean;
}

export interface DevToolSubscriptionStatus {
  provider: AgentProvider;
  configured: boolean;
  available: boolean;
  default_model: string;
}

// DevToolConfig 是「可用订阅」读数：端点只管开发工具策略里的四个订阅开关。
// 同一份策略里的目录模型选择（开发工具可见）由 api-models 端点连同调用范围
// 一起整份替换。
export interface DevToolConfig {
  revision: number;
  subscriptions: DevToolSubscriptionChoices;
  subscription_status: DevToolSubscriptionStatus[];
}

export function getKeyDevTools(id: number): Promise<DevToolConfig> {
  return request("GET", `/admin/v1/keys/${id}/dev-tools`);
}

export function putKeyDevTools(
  id: number,
  subscriptions: DevToolSubscriptionChoices,
): Promise<DevToolConfig> {
  return request("PUT", `/admin/v1/keys/${id}/dev-tools`, { subscriptions });
}

// ---- 可用模型（单把 Key 的调用范围 + 开发工具可见集合） ----

// APIModelOption 是一个候选模型。候选恒为 API密钥接入 承载的模型——订阅接入
// 的模型没有可路由的上游来源，本来就调不通 API 入口，不进这份清单。
// servable 是此刻能不能调通（模型启用、且有启用来源挂在启用的上游上），只作
// 展示：选择是策略，不因为一时不可用就不许勾。
export interface APIModelOption {
  id: number;
  name: string;
  kind: ModelKind;
  platforms: string[];
  selected: boolean;
  // selectable=false 的行只因旧选择还挂在清单里（模型已改由订阅接入承载），
  // 不能新勾为可用。
  selectable: boolean;
  servable: boolean;
  reason?: string;
  // dev_tools 是该模型可投影到的开发工具子集；空数组 = 不是开发工具候选。
  dev_tools: Array<"codex" | "opencode" | "claude">;
  dev_tool_selected: boolean;
  // dev_tool_reason 是已勾选却失去兼容性时的机器原因（服务端枚举，界面翻译）。
  dev_tool_reason?: string;
}

// KeyAPIModels 是「可用模型」读数，一份响应回答两个叠加的问题：这把 Key 能调
// 哪些模型（restricted + model_ids，restricted=false 是缺省的不限制；true 时只
// 能调 model_ids 里的那些，空集合 = 一个都不能调），以及其中哪些出现在开发工具
// 里（dev_tool_model_ids，即 Codex / Claude Code / OpenCode 的目录模型投影）。
// 收窄时服务端强制 dev_tool_model_ids ⊆ model_ids。
export interface KeyAPIModels {
  revision: number;
  restricted: boolean;
  model_ids: number[];
  dev_tool_model_ids: number[];
  models: APIModelOption[];
}

export function getKeyAPIModels(id: number): Promise<KeyAPIModels> {
  return request("GET", `/admin/v1/keys/${id}/api-models`);
}

export function putKeyAPIModels(
  id: number,
  restricted: boolean,
  modelIDs: number[],
  devToolModelIDs: number[],
): Promise<KeyAPIModels> {
  return request("PUT", `/admin/v1/keys/${id}/api-models`, {
    restricted,
    model_ids: modelIDs,
    dev_tool_model_ids: devToolModelIDs,
  });
}

// ---- 模型接入 · API按量计费 / API订阅套餐（持上游 Key 的账号） ----

export function listUpstreams(): Promise<{ upstreams: Upstream[] }> {
  return request("GET", "/admin/v1/upstreams");
}

// UpstreamPlatform 是当前生效数据目录里的可录入平台。id 是平台身份，type 是
// 固件稳定适配器；新平台可以通过数据升级新增 id 并复用兼容适配器。
export interface UpstreamPlatform {
  id: string;
  type: UpstreamType;
  vendor: string;
  base_url?: string;
  custom_base_url: boolean;
  billing_mode: BillingMode | "none";
  suggested_name: string;
  entries: string;
  protocols: Protocol[];
  ability: string;
}

export function listUpstreamPlatforms(): Promise<{ platforms: UpstreamPlatform[] }> {
  return request("GET", "/admin/v1/upstream-platforms");
}

// createUpstream 建上游。类型联动（服务端同口径）：mock 必须给 base_url 且
// 不需要凭证；deepseek/ark/ark_plan/qwen_plan/opencode_go 必须给 api_key 且不得带 base_url；
// minimax 必须给 api_key，base_url 是站点选择、只能留空（国内缺省）或取
// MinimaxSiteCN / MinimaxSiteIntl 之一；openai_compat 必须同时给 api_key
// 与 base_url（通用适配，端点根自填）。
export function createUpstream(
  name: string,
  type: UpstreamType,
  apiKey: string,
  baseURL: string,
  catalogID: string = type,
): Promise<{ upstream: Upstream }> {
  const body: Record<string, unknown> = { name, type, catalog_id: catalogID };
  if (apiKey !== "") body["api_key"] = apiKey;
  if (baseURL !== "") body["base_url"] = baseURL;
  return request("POST", "/admin/v1/upstreams", body);
}

// UpstreamPatch 只列可写字段（type 不可改，故不在其中）。服务端拒绝未知
// 字段，调用方按需逐个赋值，不要回传 GET 来的整个对象。
export interface UpstreamPatch {
  name?: string;
  // base_url：mock 任意合法地址；minimax 双站点白名单内换；openai_compat
  // 改为不同值时必须同请求携带 api_key（服务端 400 base_url_requires_key，
  // 封存的旧 Key 不能被发往新地址）；其余类型的历史行只允许清空（""）。
  base_url?: string;
  // api_key 缺省 = 不修改（没有清空凭证的路径）。
  api_key?: string;
  disabled?: boolean;
  // egress_mode：proxy 要求设备已配置代理端点（服务端 400 proxy_unconfigured）。
  egress_mode?: EgressMode;
}

export function updateUpstream(id: number, patch: UpstreamPatch): Promise<{ upstream: Upstream }> {
  return request("PATCH", `/admin/v1/upstreams/${id}`, patch);
}

// deleteUpstream 删上游；仍被模型来源引用时服务端 409 upstream_in_use。
export function deleteUpstream(id: number): Promise<void> {
  return request("DELETE", `/admin/v1/upstreams/${id}`);
}

// UpstreamBalanceAmount 是一种币种的余额读数：金额保持平台返回的十进制
// 字符串原样（设备不换算——这是厂商侧账户的读数，不是设备内的账）。
// granted/topped_up 平台没有对应口径时为空串，界面按空隐藏。
export interface UpstreamBalanceAmount {
  currency: string;
  total: string;
  granted: string;
  topped_up: string;
}

// UpstreamBalance 对应服务端 balanceReport。ok=false 时 message 是失败摘要；
// status 0 表示未收到 HTTP 响应（连接失败/超时）。
export interface UpstreamBalance {
  upstream_id: number;
  name: string;
  type: UpstreamType;
  ok: boolean;
  status: number;
  latency_ms: number;
  message: string;
  available: boolean;
  balances: UpstreamBalanceAmount[];
}

// queryUpstreamBalance 向上游平台的余额 API 发一次只读查询（特化平台能力，
// balance_supported 为 true 的上游才有此入口；其余类型服务端回 400
// balance_not_supported）。金额只在本响应里出现，不入库、不进日志与审计。
export function queryUpstreamBalance(id: number): Promise<UpstreamBalance> {
  return request("POST", `/admin/v1/upstreams/${id}/balance`);
}

// ---- API密钥接入 → 添加模型（2026-08-14） ----

// PlatformModel 是「添加模型」清单里的一行（来自平台模型信息文件：固件内嵌
// 基线或同步来的更新版本）。added = 已挂在这个账号上（置灰勾选）；
// in_catalog = 目录里已有同名行但没挂本账号（加它 = 给既有模型多挂一条上游）；
// blocked 非空 = 加不了，原因见 addModelBlockedLabel。
export interface PlatformModel {
  name: string;
  kind: ModelKind;
  /** AIGC 的协议面；文本恒空。由账号所属平台定，不由操作者选。 */
  family?: Protocol;
  upstream_model_id?: string;
  note?: string;
  added?: boolean;
  in_catalog?: boolean;
  blocked?: string;
}

// UpstreamKind 是这个账号能承载的一种模型种类：自定义录入时的种类选项，
// family 是该种类下这家平台走的协议面（文本恒空）。
export interface UpstreamKind {
  kind: ModelKind;
  family?: Protocol;
}

// UpstreamModelsCatalog 是清单本身的出处读数。listed=false 表示这家平台的官方
// 型号表还没收录——不是错误，对话框只提供「自定义」。
export interface UpstreamModelsCatalog {
  origin: "builtin" | "synced";
  version: number;
  updated_at?: string;
  listed: boolean;
  vendor?: string;
  note?: string;
  source?: string;
  checked_at?: string;
}

// UpstreamModels 是 GET 与 POST 共用的响应（POST 回添加后的最新清单，弹框
// 原地重绘，不必再发一次 GET）。created / added 是本次 POST 的产出，GET 恒 0。
export interface UpstreamModels {
  upstream_id: number;
  upstream_name: string;
  upstream_type: UpstreamType;
  /** 本次添加会用的来源优先级（服务端按计费模式取 100/200）。 */
  priority: number;
  kinds: UpstreamKind[];
  catalog: UpstreamModelsCatalog;
  models: PlatformModel[];
  created: number;
  added: number;
}

// AddUpstreamModel 是一条待添加的模型。family 不必给——服务端按这条账号服务
// 的协议面定（给了就必须相等）；upstream_model_id 留空 = 与模型名相同。
export interface AddUpstreamModel {
  name: string;
  kind: ModelKind;
  family?: Protocol;
  upstream_model_id?: string;
}

export function listUpstreamModels(upstreamID: number): Promise<UpstreamModels> {
  return request("GET", `/admin/v1/upstreams/${upstreamID}/models`);
}

// addUpstreamModels 幂等添加：缺的模型行自动建、已有同名同种类的行只多挂一条
// 上游、已挂过的原样跳过。名字**不限于清单**（自定义是产品要求：厂商上新而
// 清单还没更新时照样录得进去），裁决者是服务端与手工建模同一条的校验。
export function addUpstreamModels(upstreamID: number, models: AddUpstreamModel[]): Promise<UpstreamModels> {
  return request("POST", `/admin/v1/upstreams/${upstreamID}/models`, { models });
}

// addModelBlockedLabel 把「这条为什么加不了」译成 zh-CN。未知取值原样回显：
// 服务端加了新原因而界面还没跟上时，宁可显示机读值也别说成别的意思。
export function addModelBlockedLabel(reason: string): string {
  switch (reason) {
    case "kind_mismatch":
      return t("同名模型是另一种种类");
    case "family_mismatch":
      return t("同名模型走另一个协议面");
    case "agent_managed":
      return t("同名模型由 Agent 订阅承载");
    default:
      return reason;
  }
}

// ---- Agents 订阅账号（iteration-11，仅 admin） ----

// AgentStatus 与服务端 store 的三值同域：active 可用 / disabled 管理员停用 /
// auth_expired 刷新被上游确定性拒绝（界面显「需重新登录」徽章）。
export type AgentStatus = "active" | "disabled" | "auth_expired";

// AgentAccount 是一份已连接的订阅账号（管理视图）。**没有凭据字段**：
// auth.json 与它的密文只在盒子里，任何响应都不带（服务端契约，§15.1）。
export interface AgentAccount {
  id: number;
  /** codex | grok | claude | cursor（见 AgentProvider）。 */
  provider: AgentProvider;
  label: string;
  /** 上游侧账号标识（明文值，非密钥物料），用来认「连的是哪个账号」。 */
  account_id: string;
  /** 默认或可见模型；Cursor 透明代理恒为空。 */
  default_model: string;
  status: AgentStatus;
  /** null = 从未由本盒刷新过（刚连上就是这个状态）。 */
  last_refresh_at: string | null;
  created_at: string;
  updated_at: string;
  quota?: AgentQuota;
  credential_kind?: "oauth" | "setup_token" | "setup_token+oauth";
  setup_token_configured?: boolean;
  quota_oauth_configured?: boolean;
  quota_oauth_expired?: boolean;
}

export interface AgentQuota {
  windows: { id: string; label: string; period: string; used_bps: number | null; window_seconds?: number; resets_at: string | null }[];
  source: string;
  status: string;
  error_code?: string;
  last_attempt_at: string | null;
  last_success_at: string | null;
  next_sync_at: string | null;
  stale: boolean;
}

export function syncAgentQuota(id: number): Promise<{ quota: AgentQuota }> {
  return request("POST", `/admin/v1/agent-accounts/${id}/quota/sync`, {});
}

export function listAgentAccounts(): Promise<{ accounts: AgentAccount[] }> {
  return request("GET", "/admin/v1/agent-accounts");
}

// AgentLoginStart 是发起登录的应答，形态随 provider 分两种（provider 字段说
// 明这是哪一种，前端据此渲染）：
//   - codex（授权码+PKCE）：authorize_url + redirect_uri（那个打不开的回调地址）；
//   - grok（设备码流）：user_code + verification_uri(_complete)。
export interface AgentLoginStart {
  provider: OAuthAgentProvider;
  expires_in: number;
  // codex 侧
  authorize_url?: string;
  redirect_uri?: string;
  // grok 侧
  user_code?: string;
  verification_uri?: string;
  verification_uri_complete?: string;
}

export function startAgentLogin(provider: OAuthAgentProvider): Promise<AgentLoginStart> {
  return request("POST", "/admin/v1/agent-accounts/login/start", { provider });
}

// completeAgentLogin 完成一次登录。codex 传管理员从地址栏整条复制回来的回调
// 地址；grok 不传码（盒子打一次令牌端点问「批准了没」），callbackURL 传空串。
// 换码/问询、密封落库都在这一次 POST 里同步走完（没有轮询，没有读态 GET）。
// grok 还没批准时服务端回 409 agent_login_pending——调用方据此提示「稍后再点」，
// 会话仍在。
export function completeAgentLogin(
  provider: OAuthAgentProvider,
  callbackURL: string,
  label: string,
  defaultModel: string,
): Promise<{ account: AgentAccount }> {
  return request("POST", "/admin/v1/agent-accounts/login/callback", {
    provider,
    callback_url: callbackURL,
    label,
    default_model: defaultModel,
  });
}

// importAgentAuth 是「粘贴 auth.json」兜底入口：在别处 codex/grok login 之后把
// ~/.codex/auth.json 或 ~/.grok/auth.json 整份贴进来，盒子密封留存、自己刷新。
export function importAgentAuth(
  provider: OAuthAgentProvider,
  authJSON: string,
  label: string,
  defaultModel: string,
): Promise<{ account: AgentAccount }> {
  return request("POST", "/admin/v1/agent-accounts/import", {
    provider,
    auth_json: authJSON,
    label,
    default_model: defaultModel,
  });
}

export interface ClaudeLoginStart {
  provider: "claude";
  authorize_url: string;
  expires_in: number;
}

export function startClaudeLogin(): Promise<ClaudeLoginStart> {
  return request("POST", "/admin/v1/agent-accounts/claude/login/start", {});
}

export function completeClaudeLogin(code: string, label: string, defaultModel: string): Promise<{ account: AgentAccount }> {
  return request("POST", "/admin/v1/agent-accounts/claude/login/callback", { code, label, default_model: defaultModel });
}

// Static setup-token remains available without automatic refresh.
// 该值只进这一次管理请求；响应没有凭据字段。
export function connectClaudeSetupToken(
  setupToken: string,
  label: string,
  defaultModel: string,
): Promise<{ account: AgentAccount }> {
  return request("POST", "/admin/v1/agent-accounts/claude/setup-token", {
    setup_token: setupToken,
    label,
    default_model: defaultModel,
  });
}

// connectCursorAPIKey 封存管理员在 cursor.com/dashboard 签发的 Cursor API Key。
// 连接离线完成（设备只校验形态、不联网验证，同 setup-token 纪律）；该值只进
// 这一次管理请求，响应没有凭据字段。
export function connectCursorAPIKey(
  apiKey: string,
  label: string,
): Promise<{ account: AgentAccount }> {
  return request("POST", "/admin/v1/agent-accounts/cursor/api-key", {
    api_key: apiKey,
    label,
  });
}

// AgentAccountPatch 三态同 KeyPatch：字段缺席 = 不改，null 或空串 = 清空，
// 有值 = 设置。disabled 走同一条 PATCH（服务端拒绝把 auth_expired 直接启用）。
export interface AgentAccountPatch {
  label?: string | null;
  default_model?: string | null;
  disabled?: boolean;
}

export function updateAgentAccount(id: number, patch: AgentAccountPatch): Promise<{ account: AgentAccount }> {
  return request("PATCH", `/admin/v1/agent-accounts/${id}`, patch);
}

export function deleteAgentAccount(id: number): Promise<void> {
  return request("DELETE", `/admin/v1/agent-accounts/${id}`);
}

// refreshAgentAccount 是「自检」：让盒子强制换一代令牌，回状态不回令牌。
// 成功时若该行原为 auth_expired 会被带回 active。
export function refreshAgentAccount(id: number): Promise<{ account: AgentAccount }> {
  return request("POST", `/admin/v1/agent-accounts/${id}/refresh`);
}

export interface CursorPricingView {
  models: { name: string; pricing: Pricing | null }[];
}
export function getCursorPricing(): Promise<CursorPricingView> {
  return request("GET", "/admin/v1/cursor-pricing");
}

// Agent 订阅模型没有管理端点：一份订阅带哪些模型由模型目录数据的 agents 段
// 定义，设备连上订阅即把它们建进模型目录（服务端收敛器），管理台只在
// 「模型接入 → 开发工具订阅」右列**读出来显示**，不提供任何增删改。

// ---- 模型目录与来源（iteration-5） ----

export function listModels(): Promise<{ models: Model[] }> {
  return request("GET", "/admin/v1/models");
}

// PricingSyncApplied 是一条被填上官方价的模型。created 是历史字段：2026-08-15
// 起同步本身不再建任何行（Agent 订阅模型改由目录 agents 段与在场订阅收敛出来，
// 见 AgentModelSyncResult），所以它恒缺席。
export interface PricingSyncApplied {
  model: string;
  kind: ModelKind;
  vendor?: string;
  pricing: Pricing;
  note?: string;
  created?: boolean;
}

// PricingSyncSkipReason 是服务端给的机读跳过原因（文案在 pricingSkipLabel）。
export type PricingSyncSkipReason =
  | "already_priced"
  | "not_in_file"
  | "kind_mismatch"
  | "no_price_in_file"
  | "invalid_pricing";

export interface PricingSyncSkipped {
  model: string;
  kind: ModelKind;
  reason: PricingSyncSkipReason | string;
  detail?: string;
}

// AgentModelSyncResult 是同步收尾那次 Agent 订阅模型收敛的账（2026-08-15）：
// 按新目录建了几行、补了几行（补名义价或恢复启用）、回收了几行（厂商下架的
// 型号）。三个数都是 0 = 订阅模型没变化。
export interface AgentModelSyncResult {
  created: number;
  updated: number;
  removed: number;
}

// CatalogSyncResult 是一次「立即更新」的结果：价格那份逐条报（applied /
// skipped），平台模型那份只报状态，外加一次 Agent 订阅模型收敛的账。
// platform_error 非空 = 那一份没取成，**不是整次失败**——它有固件内嵌的基线可用，
// 价格照常已经生效。
export interface CatalogSyncResult {
  source: OfficialPricingStatus;
  applied: PricingSyncApplied[];
  skipped: PricingSyncSkipped[];
  platform_models: PlatformModelsStatus;
  platform_error?: string;
  agent_models: AgentModelSyncResult;
}

// syncCatalog 一次取 LLM Gate官网的两份公开静态文件：价格那份
// **只填充未定价的模型**——管理员录过的价一律不动；平台模型那份存下来作
// 「API密钥接入 → 添加模型」的选单，并在收尾时按它的 agents 段收敛一次 Agent 订阅
// 模型（服务端口径见 internal/admin/catalogsync.go 与 agentmodels.go）。
//
// 手动这条是**无条件 GET**（人点按钮就是要确认官网此刻发布的内容），自动那条带
// If-None-Match；两条共用同一把单飞锁，撞上正在跑的自动更新答 409
// catalog_sync_busy。
export function syncCatalog(): Promise<CatalogSyncResult> {
  return request("POST", "/admin/v1/system/data/update");
}

// pricingSkipLabel 把跳过原因译成 zh-CN。未知取值原样回显：服务端加了新原因
// 而界面还没跟上时，宁可显示一个机读值，也别把那一条说成别的意思。
export function pricingSkipLabel(reason: string): string {
  switch (reason) {
    case "already_priced":
      return t("已有目录价，未改动");
    case "not_in_file":
      return t("官方价格文件里没有这个模型");
    case "kind_mismatch":
      return t("官方价格文件里同名条目是另一种模型种类");
    case "no_price_in_file":
      return t("官方价格文件里这一条没有定价");
    case "invalid_pricing":
      return t("官方价格文件里这一条的价目不合本种类的形态");
    default:
      return reason;
  }
}

// createModel 建模型。kind 与 family 建后都不可改（服务端 PATCH 对改动回
// kind_immutable / family_immutable），换种类/换协议面只能删了重建。entries 仅
// 文本模型可带（服务端对 AIGC 带开关回 400 entries_text_only），缺省双开；
// family 仅 AIGC 模型可带（text 带值回 400 family_text_only）——video 界面上
// 必选（服务端可缺省 = 未声明、由首条上游懒钉，那是旧调用方的兼容口径），
// image 恒 ark_image（缺省服务端自动补）。
export function createModel(
  name: string,
  kind: ModelKind,
  entries?: { entry_openai: boolean; entry_responses?: boolean; entry_anthropic: boolean },
  family?: Protocol,
): Promise<{ model: Model }> {
  return request("POST", "/admin/v1/models", { name, kind, family, ...entries });
}

// ModelPatch 只列可写字段：改名即改客户端可见名。pricing 三态与限额同规：
// 不传 = 不改，null = 清除定价（回到未定价），对象 = 整表替换（服务端按 kind
// 校验字段集并拒绝未知字段，故必须整表给全，不能只带改动的那一档）。
// 协议面开关仅文本模型可带；全部关闭时服务端 400 entries_required。
export interface ModelPatch {
  name?: string;
  disabled?: boolean;
  pricing?: Pricing | null;
  entry_openai?: boolean;
  entry_responses?: boolean;
  entry_anthropic?: boolean;
}

export function updateModel(id: number, patch: ModelPatch): Promise<{ model: Model }> {
  return request("PATCH", `/admin/v1/models/${id}`, patch);
}

// deleteModel 删模型，其全部来源随外键级联删除。
export function deleteModel(id: number): Promise<void> {
  return request("DELETE", `/admin/v1/models/${id}`);
}

// createModelSource 给模型挂来源。upstreamModelID 留空 = 与模型名相同；
// 同一 (模型, 上游) 只能有一条来源（重复挂服务端 409 source_exists）。
export function createModelSource(
  modelID: number,
  upstreamID: number,
  upstreamModelID: string,
  priority: number,
): Promise<{ source: ModelSource }> {
  return request("POST", `/admin/v1/models/${modelID}/sources`, {
    upstream_id: upstreamID,
    upstream_model_id: upstreamModelID,
    priority,
  });
}

// SourcePatch 只列可写字段（上游归属不可改——换上游即删了重挂）。
export interface SourcePatch {
  upstream_model_id?: string;
  priority?: number;
  disabled?: boolean;
}

export function updateModelSource(id: number, patch: SourcePatch): Promise<{ source: ModelSource }> {
  return request("PATCH", `/admin/v1/sources/${id}`, patch);
}

export function deleteModelSource(id: number): Promise<void> {
  return request("DELETE", `/admin/v1/sources/${id}`);
}

// SourceTestResult 是一次入口协议探测的结果。status 0 表示未收到 HTTP 响应
// （连接失败/超时）；message 为失败摘要（上游错误 message 或传输错误归类），
// 成功时为空。
export interface SourceTestResult {
  protocol: Protocol;
  ok: boolean;
  status: number;
  latency_ms: number;
  message: string;
}

// SourceTestReport 对应测试端点响应：服务端对该来源能服务的每个入口协议
// 各发一次最小探测请求（与 protocols 徽标同序）；results 为空表示该来源
// 当前不服务任何入口。upstream_model_id 已解析（留空的来源回填模型名）。
export interface SourceTestReport {
  source_id: number;
  model_id: number;
  model: string;
  upstream: string;
  upstream_model_id: string;
  results: SourceTestResult[];
}

// testModelSource 触发一条来源的连通性测试。这会真实调用上游（max_tokens=1
// 的最小请求），产生极小量计费；凭证密文解不开时服务端 409
// upstream_key_unreadable，指引到「模型接入」页重新录入 Key。
export function testModelSource(id: number): Promise<SourceTestReport> {
  return request("POST", `/admin/v1/sources/${id}/test`);
}

// ---- 设备状态（仅 admin） ----

// SystemStatus 对应服务端 sysinfo.Snapshot：除 sampled_at/window_ms 外的节
// 均为「平台读得到才有」（开发机 darwin 没有 /proc 时整节缺省），界面按节
// 省略渲染，不显示编造的零。
export interface CpuCore {
  cpu: number;
  percent: number;
}

// CpuCluster 是一个 cpufreq 簇：big.LITTLE 的大核/小核各一簇，服务端已按
// 大→小排序并起好用户可见名（大核/小核/CPU…）。
export interface CpuCluster {
  label: string;
  core_model?: string; // 如 Cortex-A76；识别不出则缺省
  percent: number;
  cur_freq_mhz?: number;
  max_freq_mhz?: number;
  cores: CpuCore[];
}

export interface CpuStatus {
  overall_percent: number;
  clusters: CpuCluster[];
}

export interface MemoryStatus {
  total_bytes: number;
  used_bytes: number;
  available_bytes: number;
  buff_cache_bytes: number;
  swap_total_bytes: number;
  swap_used_bytes: number;
}

export interface NetIface {
  name: string;
  up: boolean;
  rx_bytes_per_sec: number;
  tx_bytes_per_sec: number;
  rx_total_bytes: number;
  tx_total_bytes: number;
}

export interface TempZone {
  label: string;
  celsius: number;
  /** 机身表面那一路；label 按语言本地化，排除它只能认这个标记。 */
  surface?: boolean;
}

export interface DiskStatus {
  mount: string;
  total_bytes: number;
  used_bytes: number;
  avail_bytes: number;
}

export interface SystemStatus {
  sampled_at: string;
  window_ms: number;
  uptime_seconds?: number;
  load_avg?: number[]; // 1/5/15 分钟
  cpu?: CpuStatus;
  memory?: MemoryStatus;
  network?: NetIface[];
  temperatures?: TempZone[];
  disk?: DiskStatus;
}

// getSystemStatus 取一份设备状态快照。服务端按需采样并带 2s 结果缓存：
// 页面永不轮询，刷新由人按（资源纪律见 internal/sysinfo）。
// firmwareVersion 来自 buildinfo；hardwareModel 来自服务端的本机 boardinfo
// 型号档案。设备状态页的整机读数条使用这两项。
export function getSystemStatus(): Promise<{
  system: SystemStatus;
  firmwareVersion: string;
  hardwareModel: string;
}> {
  return request("GET", "/admin/v1/system");
}

// HistoryMetric 是一段窗口内某读数的均值与峰值（分钟层两者相等）。
export interface HistoryMetric {
  avg: number;
  max: number;
}

export interface LabeledHistoryMetric extends HistoryMetric {
  label: string;
  /** 温度区里机身表面那一路为 true（与 TempZone.surface 同源）。 */
  surface?: boolean;
}

export interface NetHistoryMetric {
  name: string;
  rx_avg: number;
  rx_max: number;
  tx_avg: number;
  tx_max: number;
}

// HistoryPoint 是历史序列一个点；各节读不到即缺省（与快照同口径）。
// t 为归档窗口起点。
export interface HistoryPoint {
  t: string;
  cpu?: HistoryMetric;
  clusters?: LabeledHistoryMetric[];
  memory_percent?: HistoryMetric;
  swap_percent?: HistoryMetric;
  net?: NetHistoryMetric[];
  temps?: LabeledHistoryMetric[];
  disk_percent?: HistoryMetric;
}

// HistoryRange："6h" 是内存分钟层；"7d" 是磁盘归档层——名字是习惯叫法，
// 实际跨度以 retention_days 为准。
export type HistoryRange = "6h" | "7d";

export interface SystemHistory {
  enabled: boolean; // false = history_days: 0，历史记录整体关闭
  range: HistoryRange;
  interval_seconds: number;
  retention_days: number;
  points: HistoryPoint[];
}

// getSystemHistory 取历史序列。服务端只在此刻读归档文件，平时只有每
// 10 分钟一行的追加——不因打开页面产生额外磁盘压力。
export function getSystemHistory(range: HistoryRange): Promise<{ history: SystemHistory }> {
  return request("GET", `/admin/v1/system/history?range=${range}`);
}

// ---- 网络/域名/代理——网络配置（仅 admin） ----

// NetIPv4 一份 IPv4 配置：连接（配置文件）视角带 method；运行时视角无。
export interface NetIPv4 {
  method?: string; // manual | auto | 其他 NM 值原样
  addresses?: string[]; // CIDR，如 192.168.50.101/24
  gateway?: string;
  dns?: string[];
}

// NetConnection 修改将写入的 NM 连接（配置文件视角）：已连接网卡是活动连接，
// 未连接网卡是离线直写的候选 profile。
export interface NetConnection {
  id: string;
  uuid: string;
  ipv4: NetIPv4;
}

// NetInterface 一块物理网卡（服务端只下发 wifi/ethernet）。
export interface NetInterface {
  device: string;
  type: string; // wifi | ethernet
  mac?: string;
  state: string; // NM 状态词原样（connected/unavailable/…）
  connected: boolean;
  default_route?: boolean; // 运行时持有 0.0.0.0/0（多卡在线时的默认出口）
  entry?: boolean; // 本次请求经由的网卡（当前入口）；回环/反代进来时全员缺省
  connection?: NetConnection; // 无可修改连接（未连接且无候选 profile）时缺省
  runtime?: NetIPv4; // 已连接时的实际生效值
}

// NetPending 一次进行中的变更。phase=scheduled 是提交后尚未动网的约 2 秒；
// awaiting_confirm 表示新配置已在运行时生效，等确认或到期回滚。
export interface NetPending {
  device: string;
  connection_id: string;
  phase: "scheduled" | "awaiting_confirm";
  old: NetIPv4;
  new: NetIPv4;
  applies_at: string;
  confirm_deadline?: string; // 应用成功后才有
}

// NetLastResult 上一轮变更的终局（服务端内存态，重启即失）：回滚与失败
// 必须留痕，否则管理员只会看到"配置无事发生"。
export interface NetLastResult {
  at: string;
  event: "confirmed" | "rolled_back" | "apply_failed" | "revert_failed";
  ok: boolean;
  device: string;
  detail: string;
}

export interface NetworkStatus {
  supported: boolean;
  reason?: string; // supported=false 的说明（开发环境无 NetworkManager）
  interfaces?: NetInterface[];
  pending?: NetPending;
  last?: NetLastResult;
  confirm_window_seconds: number;
  apply_delay_seconds: number;
}

// getNetwork 读网络配置状态。服务端按需跑几条 nmcli（毫秒级），无轮询。
export function getNetwork(): Promise<{ network: NetworkStatus }> {
  return request("GET", "/admin/v1/system/network");
}

// NetworkChange 修改入参：method=auto（DHCP）时静态字段必须留空。
export interface NetworkChange {
  device: string;
  method: "manual" | "auto";
  address?: string;
  prefix?: number;
  gateway?: string;
  dns?: string[];
}

// NetPersisted 未连接网卡的离线直写结果：已写入连接配置文件，接入时生效。
export interface NetPersisted {
  device: string;
  connection_id: string;
  old: NetIPv4;
  new: NetIPv4;
}

// updateNetwork 提交修改，二选一返回。已连接网卡（pending）：约 2 秒后生效
// （先改运行时，不写配置文件），改当前入口的 IP 本页会失联，须到新地址登录
// 并在确认窗口内 confirmNetwork，否则自动回滚。未连接网卡（persisted）：直接
// 写入连接配置文件，接入时生效，无确认窗口。
export function updateNetwork(
  change: NetworkChange,
): Promise<{ pending?: NetPending; persisted?: NetPersisted }> {
  return request("PUT", "/admin/v1/system/network", change);
}

// confirmNetwork 确认保留：把已生效的新配置写入连接配置文件（持久化）。
export function confirmNetwork(): Promise<{ ok: boolean }> {
  return request("POST", "/admin/v1/system/network/confirm");
}

// ---- 用量读数（iteration-9） ----

// UsageRange 是读数区间关键词，窗口按**设备本地时区**裁（服务端 usage.Range*
// 同值）。服务端对不认识的关键词回 400 而不是静默换成缺省区间。
export type UsageRange = "today" | "month" | "7d" | "30d";

// UsageSummary 是一组行的合计。金额为主读数，token / 秒 / 张为辅——**不是每种
// 消费都按 token 计价**：H3 视频按秒（外加超出免费额度的参考图张数）、Seedream
// 按出图张数，那些行的 token 恒为 0，量在 video_seconds / image_count 里。
export interface UsageSummary {
  cost_micro: number;
  requests: number;
  errors: number;
  rejected_requests: number;
  estimated_requests: number;
  unavailable_requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  total_tokens: number;
  video_seconds: number;
  image_count: number;
  duration_ms_sum: number;
}

// UsageDimRow 是某个维度取值的合计。id 只在用户/密钥两个维度有意义；key 是
// 展示名快照，**可能为空串**（准入被拒的行没有上游名、未挂来源的模型没有
// 上游）——空取值照样进表，否则各维度合计与总计对不上，界面按「—」渲染。
export interface UsageDimRow extends UsageSummary {
  id?: number;
  key: string;
  // label 只在按 API 密钥维度出现，取该 Key 当前的管理标签；空标签缺省。
  label?: string;
  // priced 只在**按模型**这一个维度出现，且只对目录里真的有这一行的名字给值：
  // false = 该模型尚未录目录价（挂「未定价」徽章），缺省 = 无从判断（客户端
  // 随口给的模型名、被并进哨兵的行、以及其余五个维度）。
  // 服务端读目录得来的真值——**别再从「有请求却 0 元」反推**，两个方向都会错。
  priced?: boolean;
}

// UsageKeyRef 是用量页「全部 / 单 Key」统计对象的选择项。display 只有脱敏
// 展示串；当前已不存在的历史 Key 没有 label。
export interface UsageKeyRef {
  id: number;
  display: string;
  label?: string;
}

// UsageDayPoint 是按日序列的一个点（day 为设备本地日期 YYYY-MM-DD）。序列
// **只含有数据的那些天**，补零是展示层的事。
export interface UsageDayPoint extends UsageSummary {
  day: string;
}

// UsageEvent 是明细环里的一条。两类行混在一起，渲染前先分清：
//   - 普通请求行：status 是 HTTP 状态码，attempts ≥ 1；
//   - **视频任务清算行**（带 task_id）：status/attempts 恒为 0，它不是一次
//     调用而是一笔补记的账（提交那一刻的请求早已记过），按状态码渲染会把
//     它们全画成错误。
export interface UsageEvent {
  at: string;
  key_id: number;
  key_display: string;
  model_name: string;
  upstream_name: string;
  entry: string;
  kind: string;
  status: number;
  attempts: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  video_seconds: number;
  image_count: number;
  cost_micro: number;
  duration_ms: number;
  estimated?: boolean;
  usage_unavailable?: boolean;
  cache_read_tokens: number;
  cache_write_tokens: number;
  rejected?: boolean;
  reject_reason?: string;
  task_id?: string;
}

// UsageReport 是用量端点的响应形（服务端 Report 内嵌平铺 + range 回显）。
export interface UsageReport {
  range: UsageRange;
  key_id?: number;
  from: string;
  to: string;
  total: UsageSummary;
  by_key: UsageDimRow[];
  by_model: UsageDimRow[];
  by_upstream: UsageDimRow[];
  by_entry: UsageDimRow[];
  by_kind: UsageDimRow[];
  keys: UsageKeyRef[];
  days: UsageDayPoint[];
  recent: UsageEvent[];
}

// getUsage 取用量报表：不传 keyID 是设备总览，传入后总计、五个维度、按日
// 序列与明细环都只保留该 Key；keys 选择项始终完整。计量未装配时服务端
// 503 usage_unavailable。
export function getUsage(range: UsageRange, keyID?: number): Promise<UsageReport> {
  const key = keyID === undefined ? "" : `&key_id=${keyID}`;
  return request("GET", `/admin/v1/usage?range=${range}${key}`);
}

// entryLabel 是消费入口的展示名，与模型列表的入口徽标同一套词（chat / messages /
// Claude Code / video / images）。空串 = 服务端没记到入口（如准入在选路前就拒了），按「—」渲染。
export function entryLabel(entry: string): string {
  switch (entry) {
    case "chat":
      return protocolLabel("openai_chat");
    case "responses":
      return protocolLabel("openai_responses");
    case "responses_agents":
    case "claude_code":
    case "cursor_agent":
    case "imagine_image":
    case "imagine_video":
    case "subscription":
      return t("订阅流量");
    case "messages":
      return protocolLabel("anthropic_messages");
    case "video":
      return t("视频");
    case "image":
      return t("图像");
    default:
      return entry === "" ? "—" : entry;
  }
}

// usageKindLabel 是账本里 kind 维度的展示名（值域同 ModelKind，但账本行是
// 历史快照，容得下没见过的值——原样透出而不是编一个）。
export function usageKindLabel(kind: string): string {
  switch (kind) {
    case "text":
    case "video":
    case "image":
      return kindLabel(kind);
    default:
      return kind === "" ? "—" : kind;
  }
}

// BillingMode 是上游类型自带的计费模式：subscription（订阅，已付费套餐）
// 与 usage（用量，按 token 计费）。它影响调度优先级——订阅通道先用满、
// 用量兜底（默认优先级 100/200，同值时服务端按订阅先行裁决）。
export type BillingMode = "subscription" | "usage";

export function billingModeLabel(mode: BillingMode): string {
  return mode === "subscription" ? t("订阅") : t("用量");
}

// 文本模型只有这三个协议面；名称、顺序、路径与开关字段共用同一份定义。
export const TextProtocolSurfaces = [
  { protocol: "openai_chat", field: "entry_openai", path: "/v1/chat/completions" },
  { protocol: "openai_responses", field: "entry_responses", path: "/v1/responses" },
  { protocol: "anthropic_messages", field: "entry_anthropic", path: "/v1/messages" },
] as const;

// protocolLabel 是协议面的唯一展示名称定义。文本三面按协议本名；厂商协议面是
// 「厂商 模态」（火山方舟 视频、火山方舟 图像、MiniMax 视频），全部都叫协议面，
// 不再有「兼容」「接口族」这类第二套说法。
export function protocolLabel(p: Protocol): string {
  switch (p) {
    case "openai_chat":
      return t("OpenAI Chat");
    case "openai_responses":
      return t("OpenAI Responses");
    case "anthropic_messages":
      return t("Anthropic Messages");
    case "minimax_video":
      return t("MiniMax 视频");
    case "ark_video":
      return t("火山方舟 视频");
    case "ark_image":
      return t("火山方舟 图像");
  }
}

// protocolFaceLabel 是厂商段的展示名（厂商名本身）。
export function protocolFaceLabel(face: ProtocolFace): string {
  switch (face) {
    case "ark":
      return t("火山方舟");
    case "minimax":
      return "MiniMax";
  }
}

// kindLabel 是模型种类的展示名（模型列表徽章与使用API页共用）。
export function kindLabel(kind: ModelKind): string {
  switch (kind) {
    case "text":
      return t("文本");
    case "video":
      return t("视频");
    case "image":
      return t("图像");
  }
}
