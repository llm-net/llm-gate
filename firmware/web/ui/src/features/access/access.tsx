// 接入地址的共享模型：「连到这台设备」的三条路径与它们的 tab，外加复制用的示例
// 代码块。
//
// 为什么要单独一个模块：「使用指南」的两张页面（API调用 / 开发工具接入）都要回答
// 同一个问题——客户端该往哪个地址发请求。各抄一份地址推导，只会在下一次改端口
// 口径时漏掉一边。
//
// 三条路径互不替代，一次只讲一条。tab 顺序即下表顺序：
//   内网 IP   每块网卡的 IPv4 直连，恒走明文端口，永远可用
//   内网域名  客户在自己局域网里给这台设备配的域名；设备不申领也不解析，
//             只能照浏览器此刻用的主机名如实报告
//   公网地址  外网映射（管理员在自己的网络边界配置，设备只记录不建立）或
//             Cloudflare Tunnel（设备主动连出到用户自己的 Cloudflare 账号），二选一
//
// 每条 tab 挂一盏状态灯：绿=这条已开通（有可用地址），灰=未开通（熄灭态，不是
// 故障）。三条 tab 恒在场，灯就是那句「哪几条对我成立」的一眼答案。
//
// **内网 IP 与公网地址的协议、端口由设备给，不取浏览器当前值**。内网域名是
// 唯一的例外——那条路径存不存在、该怎么写，只有浏览器知道（理由见
// lanDomainTarget）。

import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { copyText } from "@/lib/clipboard";
import { t } from "@/lib/i18n";

export type TargetKind = "lan_ip" | "lan_domain" | "external";

/** 一条可用基址：tag 是同一 tab 内多行时的行前标签，hint 说明它什么时候管用。 */
export interface AccessLine {
  tag: string;
  base: string;
  hint: string;
}

/**
 * 一条接入路径（一个 tab）。lines 为空表示这条路径当前不可用，此时渲染 empty
 * 里的说明而不是把 tab 藏起来——三条路径都摆出来，用户才知道还有哪条可以开。
 */
export interface AccessTarget {
  kind: TargetKind;
  label: string;
  note: string;
  lines: AccessLine[];
  empty: string;
}

/** 恒为三条，顺序即 tab 顺序：内网 IP、内网域名、公网地址。 */
export type AccessTargets = [AccessTarget, AccessTarget, AccessTarget];

// portSuffix 端口后缀：与该协议的默认端口相同（或服务端没给）就不写出来。
function portSuffix(port: number | undefined, dflt: number): string {
  return port === undefined || port === 0 || port === dflt ? "" : `:${port}`;
}

// browserSuffix 是浏览器当前端口的后缀，仅在服务端没给出监听端口时兜底。
function browserSuffix(): string {
  return window.location.port === "" ? "" : `:${window.location.port}`;
}

// plainBase 明文基址：走设备自己的明文监听端口。服务端没给端口时退回浏览器当前
// 的协议与端口——那至少是一套此刻确实连得通的值。
function plainBase(host: string, ep: api.DeviceEndpoints): string {
  const port = ep.http_port;
  if (port === undefined || port === 0) return `${window.location.protocol}//${host}${browserSuffix()}`;
  return `http://${host}${portSuffix(port, 80)}`;
}

/** hostOf 取基址的主机名；服务端已经校验过，异常值只作不匹配处理。 */
function hostOf(base: string): string {
  try {
    return new URL(base).hostname;
  } catch {
    return "";
  }
}

function originIsExternal(ep: api.DeviceEndpoints): boolean {
  const url = ep.external_url ?? "";
  return url !== "" && hostOf(url) === window.location.hostname;
}

// isIPHost 判断一个主机名是不是 IP 字面量（点分 IPv4，或方括号包着的 IPv6）。
// 域名不可能只由数字与点组成（顶级域不许全数字），这条判据够用且不引依赖。
function isIPHost(host: string): boolean {
  return host.startsWith("[") || /^[0-9.]+$/.test(host);
}

// isLoopbackHost：在设备自己身上打开的地址对别的客户端没有意义，不能当成一条
// 可以抄走的内网域名给出去。
function isLoopbackHost(host: string): boolean {
  return host === "localhost" || host.endsWith(".localhost");
}

function lanIPTarget(ep: api.DeviceEndpoints): AccessTarget {
  const lines: AccessLine[] = ep.addresses.map((a) => ({
    tag: t("IP 直连"),
    base: plainBase(a.host, ep),
    hint: t("网卡 {iface}，同一局域网内即可连", { iface: a.interface }),
  }));
  return {
    kind: "lan_ip",
    label: t("内网 IP"),
    note: t("在同一局域网内按 IP 直连，不依赖域名、证书或任何外部服务，永远可用。"),
    lines,
    empty: t("没能读到设备的内网地址，请直接用你浏览器地址栏里的地址。"),
  };
}

// 内网域名：客户在自己局域网里给这台设备配的域名。
//
// **设备无从知道有没有这么一条**——DNS 记录在客户的局域网里，设备既不申领、也不
// 解析、更没有 mDNS 主机名可报（产品方明确不要那一套）。所以这条路径不问设备，
// 只认一件能确证的事实：浏览器此刻用的主机名。它既不是 IP 字面量、不是回环名，
// 那它就是一条真的走得通的内网域名，照它给出可抄的地址；认不出就如实说没有，
// 绝不编一个。
//
// 例外是内网域名（ep.lan_domain_url，官网申领的或管理员自有的）：那条是设备自己申领、自己
// 持有证书的，设备确知它存在，直接列出。
function lanDomainTarget(ep: api.DeviceEndpoints): AccessTarget {
  const here = window.location.hostname;
  const issued = ep.lan_domain_url ?? "";
  const issuedHost = issued === "" ? "" : new URL(issued).hostname;
  const usable = !isIPHost(here) && !isLoopbackHost(here) && !originIsExternal(ep) && here !== issuedHost;
  const lines: AccessLine[] = [];
  if (issued !== "") {
    lines.push({ tag: t("官网签发"), base: issued, hint: t("在「网络/域名/代理」页申领的内网域名，证书由 LLM Gate官网签发") });
  }
  if (usable) lines.push({ tag: t("当前地址"), base: window.location.origin, hint: t("你正用它打开本页") });
  return {
    kind: "lan_domain",
    label: t("内网域名"),
    note: t(
      "局域网里为这台设备配的域名（经 LLM Gate官网申领，或 DNS 记录 / 客户端 hosts 指到它的内网 IP）。域名也是给设备装 HTTPS 证书的前提——证书只覆盖域名，IP 上没有能通过校验的证书。",
    ),
    lines,
    empty: t(
      "还没有内网域名。管理员可在「网络/域名/代理」页经 LLM Gate官网申领一个并自动签发证书；或在局域网 DNS（或客户端 hosts）里加一条指向内网 IP 的记录，再用那个域名打开本页，这条就会给出可抄的地址。",
    ),
  };
}

function externalTarget(ep: api.DeviceEndpoints): AccessTarget {
  const url = ep.external_url ?? "";
  return {
    kind: "external",
    label: t("公网地址"),
    note: t(
      "管理员在「网络/域名/代理」页选定的公网接入方式：外网映射（自己的反向代理、端口映射、DDNS 等，设备只记录地址）或 Cloudflare Tunnel（设备经 cloudflared 连出到你自己的 Cloudflare 账号，Cloudflare 在边缘终止 TLS）。",
    ),
    lines:
      url === ""
        ? []
        : [{ tag: t("公网"), base: url, hint: t("从设备外访问；外网映射的链路由管理员维护，Tunnel 由设备自己连出") }],
    empty: t("还没有开放公网接入。请先到「网络/域名/代理」页选择外网映射或 Cloudflare Tunnel 并完成配置。"),
  };
}

export function accessTargets(ep: api.DeviceEndpoints): AccessTargets {
  return [lanIPTarget(ep), lanDomainTarget(ep), externalTarget(ep)];
}

/**
 * defaultTarget 缺省选内网 IP：它不依赖局域网 DNS 或公网接入，设备能如实给出
 * 当前读数。内网域名与公网地址只在用户主动切换时使用。
 */
export function defaultTarget(targets: AccessTargets): AccessTarget {
  const open = (kind: TargetKind): AccessTarget | undefined =>
    targets.find((item) => item.kind === kind && item.lines.length > 0);
  return open("lan_ip") ?? targets.find((item) => item.lines.length > 0) ?? targets[0];
}

// ---- tab 组 ----

/**
 * 一个 tab：key 是身份（选中态按它比），label 是显示名，lit 是可选的状态灯——
 * true 绿灯（已开通）、false 灰灯（未开通），不给这个字段就不挂灯；count 是可选
 * 的计数，挂在标签右侧。
 *
 * count 是**读数**不是徽标：它必须来自本次进页对该标签页自己那份列表的真实读取
 * ——包括当前没打开的那条 tab——拿不到就不给数字（留 undefined），绝不显示缓存
 * 值或估算值。
 */
export interface SegItem {
  key: string;
  label: string;
  icon?: React.ReactNode;
  lit?: boolean;
  count?: number;
}

/**
 * 「一次只选一条」的 tab 组。点当前这条不回调。
 *
 * `w-fit`：分段控件按内容宽度排，绝不铺满。缺了它，摆在 `flex-col` 里的那几处
 * （页面直接摞下来的 tab 行）会被交叉轴拉伸成一条通栏。
 */
export function SegTabs({
  items,
  active,
  onSelect,
  size = "default",
}: {
  items: SegItem[];
  active: string;
  onSelect: (key: string) => void;
  size?: "default" | "sm";
}): React.ReactElement {
  return (
    <div
      data-slot="seg-tabs"
      className={cn(
        "bg-muted inline-flex w-fit items-center gap-1 rounded-md",
        size === "sm" ? "h-8 p-0.5" : "p-1",
      )}
    >
      {items.map((it) => (
        <button
          key={it.key}
          type="button"
          onClick={() => {
            if (it.key !== active) onSelect(it.key);
          }}
          className={cn(
            "inline-flex items-center gap-1.5 rounded-sm px-3 py-1 text-sm transition-colors",
            it.key === active ? "bg-background shadow-sm" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {it.icon}
          {it.lit === undefined ? null : (
            <span
              title={it.lit ? t("已开通") : t("未开通")}
              className={cn("inline-block size-1.5 rounded-full", it.lit ? "bg-signal-ok" : "bg-muted-foreground/40")}
            />
          )}
          {it.label}
          {it.count === undefined ? null : (
            <span className="text-muted-foreground text-xs tabular-nums">{it.count}</span>
          )}
        </button>
      ))}
    </div>
  );
}

/**
 * 三条接入路径的 tab 组。灯亮与否就是「这条有没有可用地址」——与 tab 点开后是
 * 给读数还是给 empty 说明同一个判据，不许各算各的。
 *
 * **消费方另有要求时（Claude Code 只收 HTTPS）由它自己在正文里说清楚，别去改灯**：
 * 这排 tab 挂在页面顶上、两个标签页共用，灯说的是设备的地址事实，与你在里层挑了
 * 哪个客户端无关；跟着里层的选择明灭，只会让人以为地址本身没了。
 */
export function TargetTabs({
  targets,
  active,
  onSelect,
}: {
  targets: AccessTargets;
  active: AccessTarget;
  onSelect: (t: AccessTarget) => void;
}): React.ReactElement {
  return (
    <SegTabs
      items={targets.map((item) => ({ key: item.kind, label: item.label, lit: item.lines.length > 0 }))}
      active={active.kind}
      onSelect={(key) => {
        const next = targets.find((item) => item.kind === key);
        if (next !== undefined) onSelect(next);
      }}
    />
  );
}

// ---- 示例代码块 ----

/** 一段可复制的示例/命令。 */
export function Sample({ title, text }: { title: string; text: string }): React.ReactElement {
  return (
    <div className="overflow-hidden rounded-md border">
      <div className="bg-muted/50 flex items-center gap-2 border-b px-3 py-1.5">
        <b className="text-xs font-medium">{title}</b>
        <Button
          size="xs"
          variant="outline"
          className="ml-auto"
          onClick={() => {
            void copyText(text).then((ok) => {
              if (ok) toast(t("已复制到剪贴板"));
              else toast.error(t("复制失败，请手动选中复制"));
            });
          }}
        >
          {t("复制")}
        </Button>
      </div>
      {/* 横向滚动：长命令不折行，窄屏在块内横滚是既有约定。 */}
      <pre className="overflow-x-auto p-3 text-xs leading-relaxed">{text}</pre>
    </div>
  );
}
