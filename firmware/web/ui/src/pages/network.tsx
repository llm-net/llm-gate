// 网络/域名/代理页：一页两个标签页，页头切换，各占一条路由（切换标签就是站内导航，
// 老书签直达原内容，导航轨上高亮同一项；各标签页只读自己要用的读数）。
//
//   网络与域名（/network）  这台设备**怎么被连上**——网卡 IPv4、内网域名与公网接入
//                          （外网映射 / Cloudflare Tunnel 二选一）。
//   出站代理（/egress）     它**怎么连出去**——SOCKS5 / Clash · Mihomo，按五类流量
//                          各自选直连或经代理。
//
// 换新（数据升级、固件升级）在「设备更新」页，别往这里加。
//
// 多网卡按「修改会不会把自己踢下线」分两条路：
//
// - **已连接网卡**走服务端三段状态机——提交后约 2 秒生效（先改运行时，不写配置文件），
//   管理员须在确认窗口内点「确认保留」才落盘，逾期设备自动回滚到原配置。警告分档：
//   改「当前入口」（服务端按请求经由的本地地址标注，`entry`）才用「本页将失联」的
//   强警告；改其他在线网卡不断本页，弱化提示；入口未知（回环/反代进来）保守按强警告。
// - **未连接网卡**（未插线/未关联）走离线直写——服务端把新配置直接写进候选连接
//   profile（`persisted`），不动运行时、无确认窗口，接入时生效。
//
// 横幅倒计时是**纯客户端计时器**：本页遵守零轮询约定，绝不发请求刷状态，刷新由人按。
//
// 待确认横幅的倒计时在管理台原实现里靠 `node.isConnected` 自检和整页 reload 清理。
// **React 里必须改成 effect cleanup**：StrictMode 双挂载会跑两遍 effect，不清理就是
// 双倒计时 + 内存泄漏。

import { CircleHelp, Pencil } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";

import { HelpTip } from "@/components/help-tip";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { SegTabs } from "@/features/access/access";
import { CloudflarePanel } from "@/features/network/cloudflare-panel";
import { EgressSection } from "@/features/network/egress-panel";
import { LanDomainSection } from "@/features/network/lan-domain-panel";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { t, tx } from "@/lib/i18n";
import { navigate } from "@/lib/router";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate, SectionHead } from "./page-shell";

// ---- 网络配置 ----

function typeLabelOf(kind: string): string {
  return kind === "ethernet" ? t("有线网口") : kind === "wifi" ? t("无线网卡") : kind;
}

function methodLabel(method: string | undefined): string {
  if (method === "manual") return t("静态 IP");
  if (method === "auto") return t("自动获取（DHCP）");
  return method ?? "—";
}

const dash = (v: string | undefined): string => (v === undefined || v === "" ? "—" : v);
const listOrDash = (v: string[] | undefined): string => (v === undefined || v.length === 0 ? "—" : v.join(t("、")));

function summarizeIPv4(c: api.NetIPv4): string {
  if (c.method === "auto") return t("自动获取（DHCP）");
  const parts: string[] = [];
  if (c.addresses !== undefined && c.addresses.length > 0) parts.push(c.addresses.join("+"));
  if (c.gateway !== undefined && c.gateway !== "") parts.push(t("网关 {gateway}", { gateway: c.gateway }));
  return parts.length === 0 ? t("（空）") : parts.join(t("，"));
}

/** IPv4 输入的宽松预校验（服务端为准）：四段点分数字且每段 ≤255。 */
function looksLikeIPv4(s: string): boolean {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s);
  if (m === null) return false;
  return m.slice(1).every((part) => Number(part) <= 255);
}

function fmtCountdown(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

function newAddressOf(p: api.NetPending): string | null {
  const cidr = p.new.addresses?.[0];
  if (cidr === undefined) return null;
  const ip = cidr.split("/")[0];
  return ip === undefined || ip === "" ? null : ip;
}

function StateBadge({ iface }: { iface: api.NetInterface }) {
  if (iface.connected)
    return (
      <Badge variant="outline" className="text-signal-ok">
        {t("已连接")}
      </Badge>
    );
  if (iface.state === "unavailable")
    return (
      <Badge variant="secondary" title={t("网线未接入或硬件未就绪")}>
        {t("未插线")}
      </Badge>
    );
  return (
    <Badge variant="secondary" title={iface.state}>
      {t("未连接")}
    </Badge>
  );
}

function IfaceCard({
  iface,
  pendingBlocks,
  onEdit,
}: {
  iface: api.NetInterface;
  pendingBlocks: boolean;
  onEdit: () => void;
}) {
  // 读数以运行时实际生效值为准；未连接时退回配置文件（候选 profile）视角。
  const live = iface.runtime ?? iface.connection?.ipv4;
  const rows: [string, React.ReactNode][] = [
    [t("连接"), iface.connection !== undefined ? <code className="font-mono">{iface.connection.id}</code> : <span className="text-muted-foreground">—</span>],
    [t("获取方式"), methodLabel(iface.connection?.ipv4.method)],
    [t("IP 地址"), <span className="font-mono">{listOrDash(live?.addresses)}</span>],
    [t("网关"), <span className="font-mono">{dash(live?.gateway)}</span>],
    ["DNS", <span className="font-mono">{listOrDash(live?.dns)}</span>],
    ["MAC", <span className="font-mono">{dash(iface.mac)}</span>],
  ];
  return (
    <Card
      className={cn(
        "gap-2 border-l-4 p-4",
        iface.connected ? "border-l-signal-ok" : "border-l-muted-foreground/40 opacity-70",
      )}
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="font-medium">{typeLabelOf(iface.type)}</span>
        <code className="text-muted-foreground font-mono text-xs">{iface.device}</code>
        <span className="ml-auto flex items-center gap-1.5">
          {iface.entry === true ? (
            <Badge variant="outline" title={t("当前管理台请求经由这块网卡进来")}>
              {t("当前入口")}
            </Badge>
          ) : null}
          {iface.default_route === true ? (
            <Badge variant="outline" title={t("持有默认路由：多卡在线时设备的对外流量走这块网卡")}>
              {t("默认出口")}
            </Badge>
          ) : null}
          <StateBadge iface={iface} />
        </span>
      </div>
      <dl className="grid grid-cols-[4.5rem_1fr] gap-y-0.5 text-xs">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
      <div className="mt-auto flex flex-wrap items-center gap-2 pt-1">
        {iface.connection !== undefined ? (
          <>
            <Button
              size="xs"
              variant="outline"
              disabled={pendingBlocks}
              title={pendingBlocks ? t("已有一项网络变更进行中，请先确认或等待回滚") : undefined}
              onClick={onEdit}
            >
              {t("修改网卡配置")}
            </Button>
            {!iface.connected ? (
              <span className="text-muted-foreground text-xs">{t("未连接：修改直接写入配置文件，接入时生效")}</span>
            ) : null}
          </>
        ) : (
          <span className="text-muted-foreground text-xs">{t("没有可修改的连接配置，接入网络后再配置")}</span>
        )}
      </div>
    </Card>
  );
}

// 待确认横幅：**纯客户端倒计时，不发任何请求**。计时器用 effect cleanup 收，
// 不靠节点自检。
function PendingBanner({ network, reload }: { network: api.NetworkStatus; reload: () => void }) {
  const p = network.pending;
  const [now, setNow] = useState(() => Date.now());
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  if (p === undefined) return null;
  const appliesAt = Date.parse(p.applies_at);
  // scheduled 阶段还没有回滚截止时刻，用「生效时刻 + 确认窗口」先估一个；应用成功后
  // 服务端下发准值（刷新页面即校准）。
  const deadline =
    p.confirm_deadline !== undefined
      ? Date.parse(p.confirm_deadline)
      : appliesAt + network.confirm_window_seconds * 1000;
  const newIP = newAddressOf(p);
  // 变更的不是当前入口网卡时，本页不会失联，「去新地址重登」的指引换成一句宽心话。
  const entry = (network.interfaces ?? []).find((i) => i.entry === true);
  const entryElsewhere = entry !== undefined && entry.device !== p.device ? entry.device : null;

  return (
    <Card className="border-signal-alert gap-2 border-l-4 p-4">
      <div className="flex flex-wrap items-center gap-2">
        <b>
          {p.phase === "scheduled"
            ? t("{device} 的新网络配置即将生效（提交后约 {seconds} 秒）", {
                device: p.device,
                seconds: network.apply_delay_seconds,
              })
            : t("{device} 的新网络配置已生效，等待确认", { device: p.device })}
        </b>
        <Button
          className="ml-auto"
          size="sm"
          disabled={busy}
          onClick={() => {
            setBusy(true);
            api.confirmNetwork().then(
              () => {
                toast(t("新网络配置已确认保留"));
                reload();
              },
              (err: unknown) => {
                setBusy(false);
                toast.error(api.errorMessage(err));
              },
            );
          }}
        >
          {t("确认保留")}
        </Button>
      </div>
      <p className="text-sm">
        {tx("变更：<m>{from}</m> → <m>{to}</m>", {
          m: (s) => <span className="font-mono">{s}</span>,
          from: summarizeIPv4(p.old),
          to: summarizeIPv4(p.new),
        })}
      </p>
      <p className="text-sm">
        {tx(
          "距自动回滚还有 <b>{countdown}</b>。未在此前点击「确认保留」，设备将自动恢复原配置；确认前新配置未写入存档，断电重启同样回到原配置。",
          { b: (s) => <b className="font-mono">{s}</b>, countdown: fmtCountdown(deadline - now) },
        )}
      </p>
      {entryElsewhere !== null ? (
        <p className="text-sm">
          {t("本次变更的不是当前入口网卡（入口在 {entry}），本页不受影响，可直接点「确认保留」。", { entry: entryElsewhere })}
        </p>
      ) : p.new.method === "auto" ? (
        <p className="text-sm">{t("新地址由 DHCP 分配：请在路由器/DHCP 服务器上查询本设备的新地址，再访问其管理台完成确认。")}</p>
      ) : newIP !== null && newIP !== window.location.hostname ? (
        <p className="text-sm">
          {tx("若本页已失去连接，请从新地址进入并重新登录后确认：<a>{url}</a>", {
            a: (s) => (
              <a className="underline" href={`http://${newIP}/ui/network`}>
                {s}
              </a>
            ),
            url: `http://${newIP}/ui/network`,
          })}
        </p>
      ) : null}
    </Card>
  );
}

// 修改网卡配置。已连接网卡提交后**约 2 秒生效**，改的是当前入口时本页通常就此失联——
// 文案必须把这件事说在前面；改其他在线网卡或未连接网卡时按实际风险弱化，别一律惊吓。
function EditNetworkDialog({
  target,
  network,
  onClose,
  onDone,
}: {
  target: api.NetInterface | null;
  network: api.NetworkStatus;
  onClose: () => void;
  onDone: () => void;
}) {
  const cur = target?.connection?.ipv4;
  const [method, setMethod] = useState<"auto" | "manual">(cur?.method === "manual" ? "manual" : "auto");
  const [address, setAddress] = useState(() => (cur?.addresses?.[0] ?? "").split("/")[0] ?? "");
  const [prefix, setPrefix] = useState(() => {
    const parts = (cur?.addresses?.[0] ?? "").split("/");
    return parts[1] ?? "24";
  });
  const [gateway, setGateway] = useState(cur?.gateway ?? "");
  const [dns, setDns] = useState((cur?.dns ?? []).join(","));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (target === null) return <Dialog open={false} onOpenChange={() => undefined} />;
  const iface = target;
  // 风险分档：未连接 → 离线直写；连接中改非入口卡 → 不断本页；改入口卡或入口未知
  //（回环/反代进来）→ 保守按「会失联」处理。
  const entry = (network.interfaces ?? []).find((i) => i.entry === true);
  const offline = !iface.connected;
  const entryElsewhere =
    !offline && entry !== undefined && entry.device !== iface.device ? entry.device : null;

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const change: api.NetworkChange = { device: iface.device, method };
    if (method === "manual") {
      if (!looksLikeIPv4(address.trim())) {
        setError(t("IP 地址填得不对：请填四段点分十进制，如 192.168.1.20"));
        return;
      }
      const p = Number(prefix.trim());
      if (!Number.isInteger(p) || p < 1 || p > 32) {
        setError(t("前缀长度须为 1–32 的整数（常见是 24）"));
        return;
      }
      if (gateway.trim() !== "" && !looksLikeIPv4(gateway.trim())) {
        setError(t("网关填得不对：请填四段点分十进制，或留空"));
        return;
      }
      const dnsList = dns
        .split(/[,，\s]+/)
        .map((s) => s.trim())
        .filter((s) => s !== "");
      if (dnsList.some((d) => !looksLikeIPv4(d))) {
        setError(t("DNS 填得不对：请填一个或多个 IPv4 地址，用逗号分隔"));
        return;
      }
      change.address = address.trim();
      change.prefix = p;
      if (gateway.trim() !== "") change.gateway = gateway.trim();
      if (dnsList.length > 0) change.dns = dnsList;
    }
    setError(null);
    setBusy(true);
    api.updateNetwork(change).then(
      (res) => {
        setBusy(false);
        onClose();
        if (res.persisted !== undefined) {
          toast(
            t("已写入连接「{id}」的配置文件，{device} 接入时生效", {
              id: res.persisted.connection_id,
              device: res.persisted.device,
            }),
          );
        } else {
          toast(t("网络变更已提交，请按横幅提示确认保留"));
        }
        onDone();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("修改网卡配置 — {device}", { device: iface.device })}</DialogTitle>
          </DialogHeader>
          {offline ? (
            <p className="text-muted-foreground text-xs">
              {t("该网卡当前未连接：提交将直接写入连接「{id}」的配置文件，下次接入时生效；不影响现有网络，无需确认。", {
                id: iface.connection?.id ?? "",
              })}
            </p>
          ) : entryElsewhere !== null ? (
            <p className="text-muted-foreground text-xs">
              {t(
                "当前入口在 {entry}，本次修改不影响本页连接。新配置约 {delay} 秒后生效，仍须在 {minutes} 分钟的确认窗口内点「确认保留」——逾期自动回滚到原配置。",
                {
                  entry: entryElsewhere,
                  delay: network.apply_delay_seconds,
                  minutes: Math.round(network.confirm_window_seconds / 60),
                },
              )}
            </p>
          ) : (
            <p className="text-signal-alert text-xs">
              {t(
                "提交后约 {delay} 秒生效，当前入口很可能立即失联。请到新地址重新登录，并在 {minutes} 分钟的确认窗口内点「确认保留」——逾期设备自动回滚到原配置。",
                {
                  delay: network.apply_delay_seconds,
                  minutes: Math.round(network.confirm_window_seconds / 60),
                },
              )}
            </p>
          )}
          <div className="flex flex-col gap-1.5">
            <Label>{t("获取方式")}</Label>
            <Select value={method} onValueChange={(v) => setMethod(v === "manual" ? "manual" : "auto")}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="auto">{t("自动获取（DHCP）")}</SelectItem>
                <SelectItem value="manual">{t("静态 IP")}</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {method === "manual" ? (
            <>
              <div className="flex gap-2">
                <div className="flex flex-1 flex-col gap-1.5">
                  <Label htmlFor="net-ip">{t("IP 地址")}</Label>
                  <Input
                    id="net-ip"
                    value={address}
                    autoComplete="off"
                    spellCheck={false}
                    placeholder="192.168.1.20"
                    onChange={(e) => setAddress(e.target.value)}
                  />
                </div>
                <div className="flex w-24 flex-col gap-1.5">
                  <Label htmlFor="net-prefix">{t("前缀")}</Label>
                  <Input
                    id="net-prefix"
                    value={prefix}
                    autoComplete="off"
                    spellCheck={false}
                    onChange={(e) => setPrefix(e.target.value)}
                  />
                </div>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="net-gw">{t("网关")}</Label>
                <Input
                  id="net-gw"
                  value={gateway}
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="192.168.1.1"
                  onChange={(e) => setGateway(e.target.value)}
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="net-dns">DNS</Label>
                <Input
                  id="net-dns"
                  value={dns}
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="223.5.5.5, 8.8.8.8"
                  onChange={(e) => setDns(e.target.value)}
                />
                <p className="text-muted-foreground text-xs">{t("多个用逗号分隔；留空则不设。")}</p>
              </div>
            </>
          ) : null}
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy}>
              {t("提交变更")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 公网接入：外网映射 / Cloudflare Tunnel 二选一 ----

const EXTERNAL_MODES: api.ExternalMode[] = ["none", "manual", "cloudflare"];

const MODE_LABEL: Record<api.ExternalMode, string> = {
  none: t("不开放公网"),
  manual: t("外网映射"),
  cloudflare: "Cloudflare Tunnel",
};

function looksLikeHTTPURL(s: string): boolean {
  try {
    const u = new URL(s);
    return (
      (u.protocol === "http:" || u.protocol === "https:") &&
      u.hostname !== "" &&
      u.username === "" &&
      u.password === "" &&
      u.search === "" &&
      u.hash === ""
    );
  } catch {
    return false;
  }
}

// 三种方式的解释全部收在这张对话框里，页面本身只留选择器。
function ExternalHelpDialog({
  open,
  onOpenChange,
  port,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  port: number | undefined;
}) {
  const httpPort =
    port === undefined || port === 0 ? t("设备的明文服务端口") : t("设备的明文服务端口 {port}", { port });
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("公网接入说明")}</DialogTitle>
          <DialogDescription asChild>
            <div className="space-y-3 leading-relaxed">
              <p>
                {tx("<m>{mode}</m>：只在内网使用，「API调用」和「开发工具接入」不给出公网地址。", {
                  m: (s) => <span className="text-foreground font-medium">{s}</span>,
                  mode: MODE_LABEL.none,
                })}
              </p>
              <p>
                {tx(
                  "<m>{mode}</m>：登记你在自己网络边界（反向代理、端口映射、DDNS 等）配置并验证可用的地址；设备只记录展示，不建立或探测连接。",
                  { m: (s) => <span className="text-foreground font-medium">{s}</span>, mode: MODE_LABEL.manual },
                )}
                {t("普通 API 可转发到{port}；Claude Code 只接受系统信任的 HTTPS 地址，反向代理可在边缘终止 TLS 后以 HTTP 回源设备。", {
                  port: httpPort,
                })}
              </p>
              <p>
                {tx(
                  "<m>{mode}</m>：设备经 cloudflared 主动连出到你自己的 Cloudflare 账号与域名，无需公网 IP 或端口映射；Cloudflare 在边缘终止 TLS。",
                  { m: (s) => <span className="text-foreground font-medium">{s}</span>, mode: MODE_LABEL.cloudflare },
                )}
              </p>
              <p>{t("两种方式的设置分别保留，切换不需要重填；Tunnel 启用中不能切走。")}</p>
            </div>
          </DialogDescription>
        </DialogHeader>
        <DialogFooter showCloseButton />
      </DialogContent>
    </Dialog>
  );
}

// ModeCard 是二选一的开关：下拉选定后先二次确认再保存方式（两套设置分别保留）。
// Tunnel 启用中服务端会拒绝切走，这里直接锁住选择器。
function ModeCard({
  mode,
  locked,
  busy,
  port,
  onPick,
}: {
  mode: api.ExternalMode;
  locked: boolean;
  busy: boolean;
  port: number | undefined;
  onPick: (mode: api.ExternalMode) => void;
}) {
  const [helpOpen, setHelpOpen] = useState(false);
  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-3">
        <Label htmlFor="external-mode">{t("接入方式")}</Label>
        <Select value={mode} onValueChange={(v) => onPick(v as api.ExternalMode)} disabled={busy || locked}>
          <SelectTrigger id="external-mode" size="sm" className="w-48">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {EXTERNAL_MODES.map((m) => (
              <SelectItem key={m} value={m}>
                {MODE_LABEL[m]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {locked ? <span className="text-muted-foreground text-xs">{t("Tunnel 启用中，停用后才能切换")}</span> : null}
        <Button type="button" variant="ghost" size="xs" className="ml-auto" onClick={() => setHelpOpen(true)}>
          <CircleHelp />
          {t("说明")}
        </Button>
      </div>
      <ExternalHelpDialog open={helpOpen} onOpenChange={setHelpOpen} port={port} />
    </Card>
  );
}

// ExternalPanel 是外网映射的地址卡片：平时只读展示，点「编辑」才出输入框，保存 / 取消收口。
// `draft` 表示方式还没落盘（刚选了外网映射但没登记过地址）：卡片直接进编辑态，保存那一笔
// 把方式和地址一起写进去，取消则回到原方式。
function ExternalPanel({
  current,
  draft,
  onCancelDraft,
  onSaved,
}: {
  current: string;
  draft: boolean;
  onCancelDraft: () => void;
  onSaved: () => void;
}) {
  const [editing, setEditing] = useState(draft);
  const [url, setURL] = useState(current);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function cancel(): void {
    setURL(current);
    setError(null);
    setEditing(false);
    if (draft) onCancelDraft();
  }

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const value = url.trim();
    if (value === "") {
      setError(t("地址不能为空；不再使用请把接入方式切到「不开放公网」"));
      return;
    }
    if (!looksLikeHTTPURL(value)) {
      setError(t("请填写不带账号、查询参数或片段的完整 http:// 或 https:// 基址"));
      return;
    }
    setError(null);
    setBusy(true);
    api.setExternal("manual", value).then(
      () => {
        setBusy(false);
        toast(t("外网映射地址已保存"));
        onSaved();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  const shown = editing ? url.trim() : current;
  const plainHTTP = shown.toLowerCase().startsWith("http://");
  return (
    <Card className="gap-3 p-5">
      <div className="flex items-center gap-2">
        <Badge variant={current === "" ? "secondary" : "outline"} className={current === "" ? "" : "text-signal-ok"}>
          {current === "" ? t("未配置") : t("已配置")}
        </Badge>
        {editing ? null : (
          <Button type="button" variant="outline" size="xs" className="ml-auto" onClick={() => setEditing(true)}>
            <Pencil />
            {t("编辑")}
          </Button>
        )}
      </div>
      {editing ? (
        <form className="flex flex-col gap-3" onSubmit={submit}>
          <div className="flex flex-col gap-1.5">
            <div className="flex items-center gap-1.5">
              <Label htmlFor="external-url">{t("外部地址")}</Label>
              <HelpTip label={t("外部地址填写说明")}>
                {t("填你已经通过反向代理、端口映射或 DDNS 配置并验证可用的完整基址，如 https://device.example.com。设备只记录并展示，不建立或探测映射。")}
              </HelpTip>
            </div>
            <Input
              id="external-url"
              value={url}
              autoFocus
              autoComplete="off"
              spellCheck={false}
              placeholder="https://device.example.com"
              onChange={(e) => setURL(e.target.value)}
            />
          </div>
          {plainHTTP ? (
            <p className="text-signal-alert text-xs">{t("HTTP 地址只用于普通 API；Claude Code 需要系统信任的 HTTPS 地址。")}</p>
          ) : null}
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <div className="flex gap-2">
            <Button type="submit" size="sm" disabled={busy}>
              {t("保存")}
            </Button>
            <Button type="button" size="sm" variant="outline" disabled={busy} onClick={cancel}>
              {t("取消")}
            </Button>
          </div>
        </form>
      ) : (
        <div className="flex flex-col gap-1.5">
          <span className="text-muted-foreground text-xs">{t("外部地址")}</span>
          <code className="text-sm break-all">{current}</code>
          {plainHTTP ? (
            <p className="text-signal-alert text-xs">{t("HTTP 地址只用于普通 API；Claude Code 需要系统信任的 HTTPS 地址。")}</p>
          ) : null}
        </div>
      )}
    </Card>
  );
}

function switchBody(target: api.ExternalMode, external: api.ExternalAccess): React.ReactNode {
  switch (target) {
    case "none":
      return (
        <p>
          {t("「API调用」和「开发工具接入」将不再给出公网地址。")}
          {external.manual_url === "" ? "" : t("已登记的外网映射地址会保留。")}
        </p>
      );
    case "manual":
      return external.manual_url === "" ? (
        <p>{t("还没有登记过外部地址：确认后填写并保存地址，方式才会生效。")}</p>
      ) : (
        <p>{t("将公布已登记的地址 {url}。", { url: external.manual_url })}</p>
      );
    default:
      return <p>{t("切换后先在「第三方组件」页安装 cloudflared，再在下方面板完成 token 与域名设置并启用，公网地址才会生效。")}</p>;
  }
}

// PublicAccessSection 把方式选择与两张配置卡片收在一起：方式切换先二次确认；选了
// 外网映射却没登记过地址时方式不能先落盘（服务端要求地址非空），先以草稿态展示地址
// 卡片，保存地址那一笔连方式一起写入。
function PublicAccessSection({
  external,
  tunnel,
  port,
  reload,
}: {
  external: api.ExternalAccess;
  tunnel: api.CloudflareStatus | null;
  port: number | undefined;
  reload: () => void;
}) {
  const confirm = useConfirm();
  const [busy, setBusy] = useState(false);
  const [draftManual, setDraftManual] = useState(false);
  const shownMode: api.ExternalMode = draftManual ? "manual" : external.mode;

  async function pick(mode: api.ExternalMode): Promise<void> {
    if (mode === shownMode) return;
    // 草稿态切回原方式：什么都没保存过，直接撤掉草稿。
    if (mode === external.mode) {
      setDraftManual(false);
      return;
    }
    const ok = await confirm({
      title: t("切换到「{mode}」？", { mode: MODE_LABEL[mode] }),
      body: switchBody(mode, external),
      confirmText: t("切换"),
    });
    if (!ok) return;
    if (mode === "manual" && external.manual_url === "") {
      setDraftManual(true);
      return;
    }
    setDraftManual(false);
    setBusy(true);
    try {
      await api.setExternal(mode, external.manual_url);
      toast(t("公网接入已切换到「{mode}」", { mode: MODE_LABEL[mode] }));
      reload();
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <ModeCard mode={shownMode} locked={external.cloudflare_enabled} busy={busy} port={port} onPick={(m) => void pick(m)} />
      {shownMode === "manual" ? (
        <ExternalPanel
          key={`${external.manual_url}-${draftManual ? "draft" : "saved"}`}
          current={external.manual_url}
          draft={draftManual}
          onCancelDraft={() => setDraftManual(false)}
          onSaved={() => {
            setDraftManual(false);
            reload();
          }}
        />
      ) : null}
      {shownMode === "cloudflare" ? (
        tunnel === null ? (
          <Card className="text-muted-foreground p-5 text-sm">{t("Cloudflare Tunnel 读数暂时不可用，请刷新重试。")}</Card>
        ) : (
          <CloudflarePanel status={tunnel} reload={reload} />
        )
      ) : null}
    </>
  );
}

// ---- 页面：一页两个标签页，页头切换 ----

type NetworkTab = "network" | "egress";

// 页头两个标签页共用：标题与简介不随标签变，切换标签就是站内导航。
function NetworkHeader({
  tab,
  refreshing,
  onRefresh,
}: {
  tab: NetworkTab;
  refreshing: boolean;
  onRefresh: () => void;
}): React.ReactElement {
  return (
    <PageHeader
      title={t("网络/域名/代理")}
      note={t("配置设备网卡、内网域名、公网接入与出站代理。")}
      actions={
        <SegTabs
          size="sm"
          items={[
            { key: "network", label: t("网络与域名") },
            { key: "egress", label: t("出站代理") },
          ]}
          active={tab}
          onSelect={(k) => navigate(k === "egress" ? "/egress" : "/network")}
        />
      }
      refreshing={refreshing}
      onRefresh={onRefresh}
    />
  );
}

export function NetworkPage(): React.ReactElement {
  const [editIface, setEditIface] = useState<api.NetInterface | null>(null);
  const res = useResource(
    () =>
      Promise.all([
        api.getNetwork(),
        api.getEndpoints(),
        api.getExternal(),
        // Tunnel 读数失败（管理器未接入、引擎聚合异常）不拖垮整页：分区自己说明不可用。
        api.getCloudflareTunnel().then((r) => r.tunnel).catch((): api.CloudflareStatus | null => null),
        // 内网域名读数同理：管理器未接入时分区自己说明不可用。
        api.getLanDomain().then((r) => r.lan_domain).catch((): api.LanDomainStatus | null => null),
      ]).then(([network, access, external, tunnel, lanDomain]) => ({
        network: network.network,
        endpoints: access.endpoints,
        external: external.external,
        tunnel,
        lanDomain,
      })),
    [],
  );

  // 全宽页：网卡卡片按可用宽度自动排列（auto-fill），两种域名路径各自成段。
  return (
    <PageContainer wide>
      <NetworkHeader tab="network" refreshing={res.loading} onRefresh={res.reload} />
      <ResourceGate resource={res}>
        {(data) => {
          const network = data.network;
          const ifaces = network.interfaces ?? [];
          return (
            <>
              {network.pending !== undefined ? (
                <PendingBanner network={network} reload={res.reload} />
              ) : network.last !== undefined && network.last.event !== "confirmed" ? (
                // 回滚/失败必须留痕：管理员回到旧地址时，这里解释「刚才发生了什么」。
                <p className="text-signal-alert text-xs">{network.last.detail}</p>
              ) : null}
              <section className="flex min-w-0 flex-col gap-3">
                <SectionHead
                  title={t("网卡配置")}
                  desc={t(
                    "这台设备的网卡与 IPv4 地址。已连接网卡的修改有确认窗口保护，逾期自动回滚；未连接网卡的修改直接写入连接配置文件，接入时生效。",
                  )}
                />
                {!network.supported ? (
                  <Card className="text-muted-foreground p-5 text-sm">
                    {network.reason ?? t("当前运行环境不支持网络配置；部署到设备上即可查看和修改设备 IP。")}
                  </Card>
                ) : ifaces.length === 0 ? (
                  <Card className="text-muted-foreground p-5 text-sm">{t("没有发现物理网卡。")}</Card>
                ) : (
                  <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,20rem),1fr))] gap-3">
                    {ifaces.map((i) => (
                      <IfaceCard
                        key={i.device}
                        iface={i}
                        pendingBlocks={network.pending !== undefined}
                        onEdit={() => setEditIface(i)}
                      />
                    ))}
                  </div>
                )}
              </section>

              <section className="flex min-w-0 flex-col gap-3">
                <SectionHead
                  title={t("内网域名")}
                  desc={t("经 LLM Gate官网为这台设备申领固定内网域名并自动签发 HTTPS 证书；域名解析到内网 IP，只在局域网内可达。")}
                />
                {data.lanDomain === null ? (
                  <Card className="text-muted-foreground p-5 text-sm">{t("内网域名读数暂时不可用，请刷新重试。")}</Card>
                ) : (
                  <LanDomainSection status={data.lanDomain} reload={res.reload} />
                )}
              </section>

              <section className="flex min-w-0 flex-col gap-3">
                <SectionHead
                  title={t("公网接入")}
                  desc={t("外网映射与 Cloudflare Tunnel 二选一；生效地址会出现在「API调用」和「开发工具接入」页。")}
                />
                <PublicAccessSection
                  external={data.external}
                  tunnel={data.tunnel}
                  port={data.endpoints.http_port}
                  reload={res.reload}
                />
              </section>

              <EditNetworkDialog
                key={`net-${editIface?.device ?? "none"}`}
                target={editIface}
                network={network}
                onClose={() => setEditIface(null)}
                onDone={res.reload}
              />
            </>
          );
        }}
      </ResourceGate>
    </PageContainer>
  );
}

export function EgressPage(): React.ReactElement {
  const res = useResource(
    () =>
      Promise.all([
        // 出站代理读数失败（管理器未接入）不拖垮整页：正文自己说明不可用。
        api.getEgress().then((r) => r.egress).catch((): api.EgressStatus | null => null),
        // 内置内核读数同理（只在代理方式选它时展示）。
        api.getProxyCore().then((r) => r.proxy_core).catch((): api.ProxyCoreStatus | null => null),
      ]).then(([egress, proxyCore]) => ({ egress, proxyCore })),
    [],
  );

  return (
    <PageContainer wide>
      <NetworkHeader tab="egress" refreshing={res.loading} onRefresh={res.reload} />
      <ResourceGate resource={res}>
        {(data) => (
          <section className="flex min-w-0 flex-col gap-3">
            <SectionHead
              title={t("出站代理")}
              desc={t("让 LLM Gate 的指定外连经 SOCKS5、已有的 Clash / Mihomo 或设备内置内核（按 Clash 订阅）出去：各类流量分别选直连或经代理，选了代理的流量在代理不可用时直接失败、不会自动直连。")}
            />
            {data.egress === null ? (
              <Card className="text-muted-foreground p-5 text-sm">{t("出站代理读数暂时不可用，请刷新重试。")}</Card>
            ) : (
              <EgressSection status={data.egress} proxyCore={data.proxyCore} reload={res.reload} />
            )}
          </section>
        )}
      </ResourceGate>
    </PageContainer>
  );
}
