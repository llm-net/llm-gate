// Clash 订阅（设备内置内核）面板（出站代理标签页，代理方式选「Clash 订阅（设备内置内核）」时显示）。
//
// 三张卡：
//   1. Mihomo 内核组件先决条件卡（features/components/prereq-card.tsx）——只展示装没装、哪版、
//      有没有更新；安装 / 升级 / 回退 / 卸载去「第三方组件」页。
//   2. Clash 订阅——填机场给的 https 订阅地址（只活在输入框内存，任何响应都不回显）；固件只取
//      其中的节点列表，剔除不支持的协议；显示节点数、用量与到期。
//   3. 内核——选节点（自动选择 / 指定）、启用 / 停用、运行状态；启用后出站代理自动指向
//      127.0.0.1 的 SOCKS 端口，再在「出站范围」里给需要的流量选经代理。
//
// 零轮询：读数只在进页/刷新/动作后重取一次。

import { Loader2Icon } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { HelpTip } from "@/components/help-tip";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ComponentPrereqCard } from "@/features/components/prereq-card";
import { MIHOMO_SPEC } from "@/features/components/specs";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t, tx } from "@/lib/i18n";

type Run = (name: string, fn: () => Promise<unknown>, after?: () => void) => void;

function bytesLabel(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GiB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  return `${Math.round(n / 1024)} KiB`;
}

function coreBadge(c: api.ProxyCoreCore, enabled: boolean): { label: string; tone: "ok" | "alert" | "off" } {
  if (!c.engine_available) return { label: t("引擎不可达"), tone: "alert" };
  switch (c.state) {
    case "running":
      return { label: t("运行中"), tone: "ok" };
    case "starting":
      return { label: t("启动中"), tone: "off" };
    case "failed":
      return { label: t("已失败"), tone: "alert" };
    case "stopped":
      return { label: enabled ? t("已停止（应在运行）") : t("已停止"), tone: enabled ? "alert" : "off" };
    default:
      return { label: t("未知"), tone: "off" };
  }
}

function toneClass(tone: "ok" | "alert" | "off"): string {
  return tone === "ok" ? "text-signal-ok" : tone === "alert" ? "text-signal-alert" : "";
}

// ---- 订阅卡 ----

function looksLikeHTTPSURL(s: string): boolean {
  try {
    const u = new URL(s);
    return u.protocol === "https:" && u.hostname !== "" && u.username === "" && u.password === "";
  } catch {
    return false;
  }
}

function SubscriptionCard({ status, busy, run }: { status: api.ProxyCoreStatus; busy: string | null; run: Run }) {
  const confirm = useConfirm();
  const sub = status.subscription;
  const [editing, setEditing] = useState(!sub.set);
  const [url, setURL] = useState("");
  const [error, setError] = useState<string | null>(null);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const value = url.trim();
    if (!looksLikeHTTPSURL(value)) {
      setError(t("请填写 https:// 开头、不带账号的完整订阅地址（机场后台「Clash 订阅」给的那条）"));
      return;
    }
    setError(null);
    run(
      "subscription",
      () =>
        api.setProxyCoreSubscription(value).then((r) => {
          toast(t("订阅已保存并更新：{n} 个节点", { n: r.proxy_core.subscription.node_count }));
          return r;
        }),
      () => {
        setURL("");
        setEditing(false);
      },
    );
  }

  const ui = sub.user_info;
  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{t("Clash 订阅")}</span>
        {sub.set ? (
          <Badge variant="outline" className={sub.last_error === undefined || sub.last_error === "" ? "text-signal-ok" : "text-signal-alert"}>
            {sub.last_error === undefined || sub.last_error === "" ? t("已设置") : t("更新失败")}
          </Badge>
        ) : (
          <Badge variant="secondary">{t("未设置")}</Badge>
        )}
        <HelpTip label={t("订阅说明")}>
          {t("设备按 Clash 内核的方式拉取这条地址，只取其中的节点列表（proxies），机场下发的规则、DNS 与 TUN 设置一概不用；节点凭据只交给本机内核，订阅地址用设备密钥封存，任何响应都不回显。不支持的协议会被剔除并计数。")}
        </HelpTip>
      </div>
      {sub.set && !editing ? (
        <dl className="grid grid-cols-[6rem_1fr] gap-y-1 text-xs">
          <dt className="text-muted-foreground">{t("订阅站点")}</dt>
          <dd>
            <code className="font-mono">{sub.host}</code>
          </dd>
          <dt className="text-muted-foreground">{t("节点")}</dt>
          <dd>
            {t("{n} 个可用", { n: sub.node_count })}
            {sub.dropped > 0 ? <span className="text-muted-foreground ml-2">{t("（剔除 {n} 个不支持的条目）", { n: sub.dropped })}</span> : null}
          </dd>
          <dt className="text-muted-foreground">{t("更新于")}</dt>
          <dd>{sub.fetched_at === undefined || sub.fetched_at === "" ? "—" : fmtTime(sub.fetched_at)}</dd>
          {ui !== undefined && ui !== null ? (
            <>
              <dt className="text-muted-foreground">{t("用量")}</dt>
              <dd>
                {ui.total > 0
                  ? t("已用 {used} / {total}", { used: bytesLabel(ui.upload + ui.download), total: bytesLabel(ui.total) })
                  : t("已用 {used}", { used: bytesLabel(ui.upload + ui.download) })}
                {ui.expire > 0 ? <span className="text-muted-foreground ml-2">{t("到期 {time}", { time: fmtTime(new Date(ui.expire * 1000).toISOString()) })}</span> : null}
              </dd>
            </>
          ) : null}
        </dl>
      ) : null}
      {sub.last_error !== undefined && sub.last_error !== "" ? <p className="text-signal-alert text-xs">{sub.last_error}</p> : null}
      {editing ? (
        <form className="flex flex-col gap-2" onSubmit={submit}>
          <Label htmlFor="mh-sub">{t("订阅地址")}</Label>
          <Input
            id="mh-sub"
            type="url"
            value={url}
            autoFocus
            autoComplete="off"
            spellCheck={false}
            placeholder="https://example.com/subscribe/…/clash/"
            onChange={(e) => setURL(e.target.value)}
          />
          <p className="text-muted-foreground text-xs">{sub.set ? t("留空取消；填入新地址会替换并立即更新。") : t("保存后立即拉取并解析节点。")}</p>
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <div className="flex gap-2">
            <Button type="submit" size="sm" disabled={busy !== null}>
              {busy === "subscription" ? t("更新中…") : t("保存并更新")}
            </Button>
            {sub.set ? (
              <Button type="button" size="sm" variant="outline" disabled={busy !== null} onClick={() => setEditing(false)}>
                {t("取消")}
              </Button>
            ) : null}
          </div>
        </form>
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => run("refresh", () => api.refreshProxyCoreSubscription().then((r) => {
            toast(t("订阅已更新：{n} 个节点", { n: r.proxy_core.subscription.node_count }));
            return r;
          }))}>
            {busy === "refresh" ? <Loader2Icon className="animate-spin" /> : null}
            {t("立即更新订阅")}
          </Button>
          <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => setEditing(true)}>
            {t("更换订阅地址")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null || status.enabled}
            title={status.enabled ? t("内核启用中，先停用") : undefined}
            onClick={() => {
              void confirm({
                title: t("清除订阅？"),
                body: t("删除订阅地址与已保存的节点列表。"),
                confirmText: t("清除"),
                danger: true,
              }).then((ok) => {
                if (!ok) return;
                run("clear", () => api.clearProxyCoreSubscription(), () => setEditing(true));
              });
            }}
          >
            {t("清除订阅")}
          </Button>
        </div>
      )}
    </Card>
  );
}

// ---- 内核卡 ----

const AUTO = "__auto__";

// latencyLabel 画节点行末的延迟读数：直连节点服务器的 TCP 连接耗时（没测过不画）。
function latencyLabel(n: api.ProxyCoreNode): React.ReactElement | null {
  if (n.latency_failed === true) return <span className="text-signal-alert ml-1 text-xs">{t("连不上")}</span>;
  if (n.latency_ms === undefined || n.latency_ms <= 0) return null;
  const cls = n.latency_ms <= 300 ? "text-signal-ok" : n.latency_ms <= 800 ? "text-muted-foreground" : "text-signal-alert";
  return <span className={cn("ml-1 text-xs tabular-nums", cls)}>{n.latency_ms} ms</span>;
}

function CoreCard({ status, busy, run }: { status: api.ProxyCoreStatus; busy: string | null; run: Run }) {
  const confirm = useConfirm();
  const [accept, setAccept] = useState(status.license_accepted);
  const badge = coreBadge(status.core, status.enabled);
  const canEnable = status.component.installed && status.subscription.node_count > 0 && status.component.engine_available;
  const blocker = !status.component.installed
    ? t("先到「第三方组件」页安装 Mihomo 内核")
    : status.subscription.node_count === 0
      ? t("先设置订阅并取得节点")
      : !status.component.engine_available
        ? t("升级引擎不可达")
        : null;

  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{t("内核")}</span>
        <Badge variant={status.enabled ? "outline" : "secondary"} className={status.enabled ? "text-signal-ok" : ""}>
          {status.enabled ? t("已启用") : t("未启用")}
        </Badge>
        <Badge variant={badge.tone === "off" ? "secondary" : "outline"} className={toneClass(badge.tone)}>
          {badge.label}
        </Badge>
        {status.enabled ? (
          <code className="text-muted-foreground font-mono text-xs">socks5://{status.address}</code>
        ) : null}
      </div>
      {status.last_error !== undefined && status.last_error !== "" ? <p className="text-signal-alert text-xs">{status.last_error}</p> : null}
      <div className="flex flex-wrap items-center gap-3">
        <Label htmlFor="mh-node">{t("节点")}</Label>
        <Select
          value={status.selected === "" ? AUTO : status.selected}
          disabled={busy !== null || status.nodes.length === 0}
          onValueChange={(v) => run("node", () => api.selectProxyCoreNode(v === AUTO ? "" : v))}
        >
          <SelectTrigger id="mh-node" size="sm" className="w-72">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value={AUTO}>{t("自动选择（按延迟）")}</SelectItem>
            {status.nodes.map((n) => (
              <SelectItem key={n.name} value={n.name}>
                {n.name}
                <span className="text-muted-foreground ml-1 text-xs">{n.type}</span>
                {latencyLabel(n)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <span className="text-muted-foreground text-xs">{t("{n} 个节点", { n: status.nodes.length })}</span>
        <Button
          size="sm"
          variant="outline"
          disabled={busy !== null || status.nodes.length === 0}
          onClick={() =>
            run("latency", () =>
              api.testProxyCoreLatency().then((r) => {
                const reachable = r.proxy_core.nodes.filter((n) => (n.latency_ms ?? 0) > 0).length;
                toast(t("延迟已更新：{reachable}/{total} 个节点可达", { reachable, total: r.proxy_core.nodes.length }));
                return r;
              }),
            )
          }
        >
          {busy === "latency" ? <Loader2Icon className="animate-spin" /> : null}
          {t("测延迟")}
        </Button>
        <HelpTip label={t("延迟说明")}>
          {t("延迟是设备直接与各节点服务器建立 TCP 连接的耗时（含域名解析），内核没启用也能测，供选节点参考；「自动选择」由内核运行时按完整代理链路自行测速决定，两个数值口径不同。")}
        </HelpTip>
        {status.latency_tested_at === undefined ? null : (
          <span className="text-muted-foreground text-xs">{t("测于 {time}", { time: fmtTime(status.latency_tested_at) })}</span>
        )}
      </div>
      {status.enabled ? (
        <div className="flex flex-wrap gap-2">
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null}
            onClick={() => {
              void confirm({
                title: t("停用内置内核"),
                body: (
                  <>
                    <p>{t("停止内核；「出站范围」里经代理的流量全部改回直连。订阅与节点保留。")}</p>
                  </>
                ),
                confirmText: t("停用"),
              }).then((ok) => {
                if (!ok) return;
                run("disable", () =>
                  api.disableProxyCore().then((r) => {
                    toast(t("内置内核已停用"));
                    return r;
                  }),
                );
              });
            }}
          >
            {busy === "disable" ? t("停用中…") : t("停用")}
          </Button>
        </div>
      ) : (
        <div className="flex flex-col gap-2">
          {status.license_accepted ? null : (
            <label className="flex items-start gap-2 text-xs">
              <Checkbox className="mt-0.5" checked={accept} onCheckedChange={(c) => setAccept(c === true)} />
              <span>
                {tx(
                  "我知道 Mihomo 是 MetaCubeX 以 <a>GPL-3.0</a> 发布的第三方程序，LLM Gate 只经 SOCKS5 与它通信；经代理的流量会经过订阅提供的节点。",
                  {
                    a: (s) => (
                      <a className="underline" href="https://github.com/MetaCubeX/mihomo/blob/v1.19.30/LICENSE" target="_blank" rel="noreferrer">
                        {s}
                      </a>
                    ),
                  },
                )}
              </span>
            </label>
          )}
          <div className="flex flex-wrap items-center gap-2">
            <Button
              size="sm"
              disabled={busy !== null || !canEnable || (!status.license_accepted && !accept)}
              title={blocker ?? undefined}
              onClick={() =>
                run("enable", () =>
                  api.enableProxyCore(accept).then((r) => {
                    toast(t("内置内核已启动，出站代理已指向 {address}", { address: r.proxy_core.address }));
                    return r;
                  }),
                )
              }
            >
              {busy === "enable" ? t("启动中…") : t("启用内核")}
            </Button>
            {blocker === null ? null : <span className="text-muted-foreground text-xs">{blocker}</span>}
          </div>
          <p className="text-muted-foreground text-xs">{t("启用后在下方「出站范围」里为需要的流量选「经代理」；内核只绑定本机 127.0.0.1，不对局域网开放。")}</p>
        </div>
      )}
    </Card>
  );
}

// ---- 面板 ----

export function MihomoPanel({ status, reload }: { status: api.ProxyCoreStatus; reload: () => void }): React.ReactElement {
  const [busy, setBusy] = useState<string | null>(null);
  const run: Run = (name, fn, after) => {
    setBusy(name);
    fn().then(
      () => {
        setBusy(null);
        after?.();
        reload();
      },
      (err: unknown) => {
        setBusy(null);
        toast.error(api.errorMessage(err));
        reload();
      },
    );
  };
  return (
    <div className={cn("flex flex-col gap-3", busy !== null && "opacity-90")}>
      <ComponentPrereqCard spec={MIHOMO_SPEC} comp={status.component} />
      <SubscriptionCard key={status.subscription.host ?? ""} status={status} busy={busy} run={run} />
      <CoreCard status={status} busy={busy} run={run} />
    </div>
  );
}
