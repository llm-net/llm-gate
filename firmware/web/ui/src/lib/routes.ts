// 路由表。路由值是 /ui/ 下的真路径（`/ui/keys`）——服务端对 /ui/ 未知子路径
// 回退 index.html，深链刷新不 404，所以不需要 hash。
//
// **没有角色守卫**：设备只有一个管理员，登录进来就都看得见。守卫只剩一件事——
// 没会话就去登录页，有会话就别停在登录页。
//
// 无会话页只有两张：`/login`（管理员登录）与 `/connect`（「接入方法」：拿到一把
// API 密钥的人贴入 Key 自助查看调用地址与开发工具接入，features/access/connect-view.tsx）。
// `/connect` 不看会话——它凭 Key 打数据面，有没有登着都能开，守卫对它不做任何事。
//
// 三组路由作用域不同，加路由时别搞混：
// - `/model-routing/*` 教「怎么把客户端或开发工具连到这台设备」；
// - `/upstreams/*` 与 `/agent-accounts` 是「模型接入」菜单组的四页——API按量计费、
//   API订阅套餐、API私有部署、开发工具订阅——各占一条路由，侧栏各是一项。
//
// 设备自身的两页按「常改的」与「偶尔做一次的」分开，别再合成一页：
// - `/network` 是这台设备怎么被连上（网卡 IPv4 + 内网域名 + 公网接入），
//   `/egress` 是它怎么连出去（出站代理）——与 `/network` 同页不同标签；
// - `/updates` 是把设备上的东西换新（数据升级 + 固件升级）；
// - `/components` 是设备上按需运行的第三方程序（cloudflared、Mihomo 内核）的安装 / 升级 /
//   回退 / 卸载。功能页只展示组件状态并链接过来，卸载前先在功能页停用。

/** 登录后可达的全部路由。`/` 不是路由：它归一化到落地页（见 LEGACY_PATH）。 */
export const BUSINESS_ROUTES = [
  "/model-routing/api",
  "/model-routing/dev-tools",
  "/keys",
  "/upstreams/usage",
  "/upstreams/plan",
  "/upstreams/private",
  "/agent-accounts",
  "/usage",
  "/status",
  "/network",
  "/egress",
  "/updates",
  "/components",
] as const;

export type BusinessRoute = (typeof BUSINESS_ROUTES)[number];
/** 无会话页：`/login` 管理员登录，`/connect` 凭 API 密钥自助查看接入方法。 */
export const PUBLIC_ROUTES = ["/login", "/connect"] as const;
export type PublicRoute = (typeof PUBLIC_ROUTES)[number];
export type Route = PublicRoute | BusinessRoute;

const BUSINESS: readonly string[] = BUSINESS_ROUTES;
const PUBLIC: readonly string[] = PUBLIC_ROUTES;

/** 登录后的落地页：先交付最通用的 API 调用方式。 */
export const LANDING: BusinessRoute = "/model-routing/api";

// 管理台改名前的旧 hash：归一化到新路由，老书签不落到答非所问的页面上。
// `/admin/` 那张跳转页（internal/admin/ui.go）读 location.hash 之后查的就是
// 这张表——fragment 到不了服务端，302 换不出 hash 里那一段。
export const LEGACY_HASH: Readonly<Record<string, BusinessRoute>> = {
  "#/assistant": "/model-routing/api", // 使用助手 → API调用
  "#/agents": "/model-routing/dev-tools", // Agents → 开发工具接入
  "#/models": "/agent-accounts", // 模型页并入模型接入菜单组，缺省先看开发工具订阅
  "#/users": "/keys", // 用户页退场 → API密钥（原本挂在用户下的东西只剩它）
  "#/profile": "/network", // 个人信息退场 → 改密进了顶栏；设备本体的落点是网络/域名/代理
};

// 界面自己改过的旧路径归一化到当前页面；设备设置拆成网络/域名/代理与设备更新，
// `/users`、`/profile`、`/my-usage` 随用户概念退场。
//
// 归一化去**语义最接近**的那一页，而不是一律甩回落地页：老书签落在一个答非
// 所问的页面上，比多跳一次更让人困惑。
const LEGACY_PATH: Readonly<Record<string, BusinessRoute>> = {
  "/": "/model-routing/api", // /ui/ 的根：直接落到落地页
  "/upstreams": "/upstreams/usage", // API密钥接入拆成按量 / 套餐 / 私有部署三页，旧书签落到按量
  "/use-models": "/model-routing/api",
  "/use-api": "/model-routing/api",
  "/use-agent": "/model-routing/dev-tools",
  "/settings": "/network", // 设备设置拆成两页，网络/域名/代理是它的第一分区
  "/users": "/keys", // 用户页没了，它下面真正在用的东西是密钥
  "/profile": "/network", // 个人资料没了，改密现在在顶栏
  "/my-usage": "/usage", // 个人账并进设备唯一的那本账
};

export function isBusinessRoute(route: Route): route is BusinessRoute {
  return !PUBLIC.includes(route);
}

/** parseRoute 把 /ui/ 内的路径收窄成已知路由；不认识的回 null（由调用方送去落地页）。 */
export function parseRoute(path: string): Route | null {
  if (PUBLIC.includes(path)) return path as PublicRoute;
  if (BUSINESS.includes(path)) return path as BusinessRoute;
  return LEGACY_PATH[path] ?? null;
}

/**
 * resolveRoute 是守卫链，三步顺序不能改：
 *
 * 1. 认识就用它，不认识归一化到落地页；
 * 2. 没会话就去登录页，有会话就别停在登录页（`/connect` 两种状态都放行）；
 * 3. 调用方按返回值回写地址栏（真实路径与界面必须一致）。
 */
export function resolveRoute(path: string, authed: boolean): Route {
  const landing: Route = authed ? LANDING : "/login";
  let route = parseRoute(path) ?? landing;
  if (!authed && isBusinessRoute(route)) {
    route = "/login";
  } else if (authed && route === "/login") {
    route = landing;
  }
  return route;
}
