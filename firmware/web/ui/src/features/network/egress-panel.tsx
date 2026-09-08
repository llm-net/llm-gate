// 出站代理面板（网络/域名/代理页「出站代理」标签页，docs-dev/firmware-egress-proxy.md）。
//
// 设备的全部互联网出口按五类流量各自选「直连 / 经代理」；代理端点只有一个，两种方式
// （SOCKS5 代理 / Clash · Mihomo）都只经标准 SOCKS5 CONNECT 通信，区别只在填写引导：
// Clash / Mihomo 填它的 socks-port 或 mixed-port。选了代理的流量失败关闭，不回落直连。
//
// 用户名与口令只活在本组件的输入框内存里：任何响应都不回显（服务端只回「是否已设置」），
// 省略即保留、空串即清除。零轮询：读数只在进页/刷新/动作后重取一次。

import { CircleHelp, Loader2Icon, Pencil, PlugZap } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
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
import { MihomoPanel } from "@/features/network/mihomo-panel";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t, tx } from "@/lib/i18n";

// Radix Select 不接受空串 value：「不使用代理」在选择器里用 none 表示。
type ProviderPick = "none" | "socks5" | "clash" | "mihomo";

const PROVIDER_PICKS: ProviderPick[] = ["none", "socks5", "clash", "mihomo"];

function pickOf(p: api.EgressProvider): ProviderPick {
  return p === "" ? "none" : p;
}

function providerOf(p: ProviderPick): api.EgressProvider {
  return p === "none" ? "" : p;
}

const PROVIDER_LABEL: Record<ProviderPick, string> = {
  none: t("不使用代理"),
  socks5: t("SOCKS5 代理"),
  clash: t("Clash / Mihomo（连接已有的）"),
  mihomo: t("Clash 订阅（设备内置内核）"),
};

const SCOPE_DESC: Record<api.EgressScope, string> = {
  model_api: t("模型平台 API、余额与来源测试，以及 Codex / Grok / Claude Code / Cursor 订阅后端"),
  agent_auth: t("OpenAI / xAI 的 OAuth 登录、换码与刷新"),
  official_site: t("数据升级、固件索引与下载、推荐应用、官网账号关联与域名证书"),
  cli_artifacts: t("五种开发工具官方安装物的白名单透传"),
  component_artifacts: t("cloudflared / Mihomo 等可选组件的官方制品下载（首次安装本机代理内核时应保持直连）"),
  proxy_subscription: t("内置内核的 Clash 订阅拉取（首次拉取时内核还没起来，通常保持直连）"),
};

const STAGE_LABEL: Record<string, string> = {
  proxy_tcp: t("连接代理"),
  socks5: t("SOCKS5 握手"),
  tls: t("目标 TLS"),
  https: t("HTTPS 请求"),
};

function looksLikeHostPort(s: string): boolean {
  if (s === "" || /[/?#@\s]/.test(s) || s.includes("://")) return false;
  const m = /^(.+):(\d{1,5})$/.exec(s);
  if (m === null) return false;
  const port = Number(m[2]);
  return port >= 1 && port <= 65535;
}

// ---- 说明 ----

function EgressHelpDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("出站代理说明")}</DialogTitle>
          <DialogDescription asChild>
            <div className="space-y-3 leading-relaxed">
              <p>{t("代理只影响 LLM Gate 自己的指定外连（下方五类流量），不改变设备操作系统或你电脑的全局网络。")}</p>
              <p>
                {tx("<m>{mode}</m>：填写任意 SOCKS5 服务器的 host:port；需要认证时填用户名与口令。", {
                  m: (s) => <span className="text-foreground font-medium">{s}</span>,
                  mode: PROVIDER_LABEL.socks5,
                })}
              </p>
              <p>
                {tx(
                  "<m>{mode}</m>：填写 Clash / Mihomo 的 socks-port 或 mixed-port（常见 7890 / 7891）。内核运行在这台设备上填 127.0.0.1；运行在局域网另一台电脑上时，需在那边开启 allow-lan 并填它的内网 IP。设备只经标准 SOCKS5 与它通信，不读取、不解析 Clash 配置或订阅。",
                  { m: (s) => <span className="text-foreground font-medium">{s}</span>, mode: PROVIDER_LABEL.clash },
                )}
              </p>
              <p>
                {tx(
                  "<m>{mode}</m>：把机场给的 Clash 订阅地址填给设备，设备从 Mihomo 官方 release 安装内核（GPL-3.0 第三方程序）、只取订阅里的节点、在本机 127.0.0.1 上起一个 SOCKS 端口并把出站代理指向它。不用另一台电脑跑 Clash。",
                  { m: (s) => <span className="text-foreground font-medium">{s}</span>, mode: PROVIDER_LABEL.mihomo },
                )}
              </p>
              <p>{t("目标域名交给代理端解析（socks5h 语义），设备本地 DNS 只解析代理端点自己的名字；目标 TLS 证书仍由设备按目标域名校验，经代理只允许 https:// 目标。")}</p>
              <p className="text-signal-alert">{t("选了经代理的流量在代理不可用时会直接失败，不会自动改走直连——这样提示词、凭据与访问元数据不会绕过你选定的出口。")}</p>
              <p className="text-signal-alert">{t("SOCKS5 本身不加密，用户名/口令以明文承载；远程代理只应放在受信网络、SSH/VPN 隧道或其他已加密的承载之内。")}</p>
              <p>{t("修改保存后新请求即按新配置选路，正在进行的请求按原路径完成；不需要重启设备。")}</p>
            </div>
          </DialogDescription>
        </DialogHeader>
        <DialogFooter showCloseButton />
      </DialogContent>
    </Dialog>
  );
}

// ---- 代理端点 ----

// EndpointForm 是代理端点的编辑表单：draft 表示方式还没落盘（刚选了代理方式但没登记过
// 地址），保存那一笔把方式和地址一起写进去，取消则回到原方式。
function EndpointForm({
  status,
  provider,
  draft,
  onCancel,
  onSaved,
}: {
  status: api.EgressStatus;
  provider: api.EgressProvider;
  draft: boolean;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const [address, setAddress] = useState(status.address);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [clearAuth, setClearAuth] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const hasAuth = status.username_set || status.password_set;
  const isClash = provider === "clash";

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const addr = address.trim();
    if (!looksLikeHostPort(addr)) {
      setError(t("代理地址请填 host:port，例如 127.0.0.1:7891 或 [::1]:7890；不要带协议、路径或账号"));
      return;
    }
    const patch: api.EgressPatch = { address: addr };
    if (draft || provider !== status.provider) patch.provider = provider;
    if (clearAuth) {
      patch.username = "";
      patch.password = "";
    } else if (username !== "" || password !== "") {
      if (username === "" || password === "") {
        setError(t("用户名与口令必须同时填写；不需要认证请都留空"));
        return;
      }
      patch.username = username;
      patch.password = password;
    }
    setError(null);
    setBusy(true);
    api.updateEgress(patch).then(
      () => {
        setBusy(false);
        toast(t("代理端点已保存"));
        onSaved();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <form className="flex flex-col gap-3" onSubmit={submit}>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="egress-address">{isClash ? t("Clash / Mihomo 的 SOCKS 端口地址") : t("SOCKS5 地址")}</Label>
        <Input
          id="egress-address"
          value={address}
          autoFocus
          autoComplete="off"
          spellCheck={false}
          placeholder={isClash ? "127.0.0.1:7891" : "192.168.1.10:1080"}
          onChange={(e) => setAddress(e.target.value)}
        />
        <p className="text-muted-foreground text-xs">
          {isClash
            ? t("填 socks-port 或 mixed-port；内核在本机填 127.0.0.1，在另一台电脑上填它的内网 IP（那边需开启 allow-lan）。")
            : t("host:port 形式；代理为域名时该名字经设备本地 DNS 解析，目标域名则交给代理解析。")}
        </p>
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="egress-user">{t("用户名")}</Label>
          <Input
            id="egress-user"
            value={username}
            autoComplete="off"
            spellCheck={false}
            disabled={clearAuth}
            placeholder={hasAuth ? t("留空表示不修改") : t("无认证可留空")}
            onChange={(e) => setUsername(e.target.value)}
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="egress-pass">{t("口令")}</Label>
          <Input
            id="egress-pass"
            type="password"
            value={password}
            autoComplete="new-password"
            disabled={clearAuth}
            placeholder={hasAuth ? t("留空表示不修改") : t("无认证可留空")}
            onChange={(e) => setPassword(e.target.value)}
          />
        </div>
      </div>
      {hasAuth ? (
        <label className="flex items-center gap-2 text-sm">
          <Checkbox checked={clearAuth} onCheckedChange={(c) => setClearAuth(c === true)} />
          {t("清除已保存的用户名与口令（改为无认证）")}
        </label>
      ) : null}
      <p className="text-muted-foreground text-xs">
        {t("用户名与口令用设备密钥封存，不可读回，只能覆盖；SOCKS5 认证以明文承载，远程代理请置于受信网络或加密隧道内。")}
      </p>
      {error === null ? null : (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={busy}>
          {t("保存")}
        </Button>
        <Button type="button" size="sm" variant="outline" disabled={busy} onClick={onCancel}>
          {t("取消")}
        </Button>
      </div>
    </form>
  );
}

function TestResultView({ res }: { res: api.EgressTestResult }) {
  return (
    <div className="flex flex-col gap-1.5 text-xs">
      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="outline" className={res.ok ? "text-signal-ok" : "text-signal-alert"}>
          {res.ok ? t("测试通过") : t("测试未通过")}
        </Badge>
        <span className="text-muted-foreground">
          {t("{time} · 目标 {target}", { time: fmtTime(res.at), target: res.target })}
        </span>
      </div>
      <ul className="flex flex-wrap gap-x-4 gap-y-1">
        {res.stages.map((st) => (
          <li key={st.name} className="flex items-center gap-1.5">
            <span className={cn("size-2 rounded-full", st.ok ? "bg-signal-ok" : "bg-signal-alert")} aria-hidden="true" />
            <span>{STAGE_LABEL[st.name] ?? st.name}</span>
            <span className="text-muted-foreground tabular-nums">{st.latency_ms}ms</span>
            {st.detail !== undefined && st.detail !== "" ? <span className="text-muted-foreground">{st.detail}</span> : null}
          </li>
        ))}
      </ul>
      {res.message !== undefined && res.message !== "" ? (
        <p className={res.ok ? "text-muted-foreground" : "text-signal-alert"}>{res.message}</p>
      ) : null}
    </div>
  );
}

function EndpointCard({ status, proxyCore, reload }: { status: api.EgressStatus; proxyCore: api.ProxyCoreStatus | null; reload: () => void }) {
  const confirm = useConfirm();
  const [helpOpen, setHelpOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(false);
  // draftProvider：刚选了代理方式但还没登记地址，先以草稿态展示表单。
  const [draftProvider, setDraftProvider] = useState<api.EgressProvider | null>(null);
  const shownProvider: api.EgressProvider = draftProvider ?? status.provider;

  async function pick(p: ProviderPick): Promise<void> {
    const next = providerOf(p);
    if (next === shownProvider) return;
    if (next === status.provider) {
      setDraftProvider(null);
      return;
    }
    if (next === "") {
      const proxied = status.scopes.filter((s) => s.route === "proxy").map((s) => s.label);
      const ok = await confirm({
        title: t("不再使用代理？"),
        body: (
          <>
            <p>{t("将清除代理地址与认证信息，全部流量改为直连。")}</p>
            {proxied.length > 0 ? <p>{t("当前经代理的流量：{scopes}。", { scopes: proxied.join(t("、")) })}</p> : null}
            {status.overrides.some((o) => o.egress_mode === "proxy") ? (
              <p className="text-signal-alert">{t("仍有账号显式设为经代理，需先在「模型接入」页改回；否则本次清除会被拒绝。")}</p>
            ) : null}
          </>
        ),
        confirmText: t("清除代理"),
        danger: true,
      });
      if (!ok) return;
      setBusy(true);
      try {
        const routes: Partial<Record<api.EgressScope, api.EgressRoute>> = {};
        for (const s of status.scopes) routes[s.id] = "direct";
        await api.updateEgress({ provider: "", routes });
        toast(t("已清除代理，全部流量直连"));
        setDraftProvider(null);
        reload();
      } catch (err: unknown) {
        toast.error(api.errorMessage(err));
      } finally {
        setBusy(false);
      }
      return;
    }
    if (next === "mihomo" || status.provider === "mihomo") {
      // 内置内核的地址由固件管理：切到它只写方式；从它切走由服务端在内核启用中拒绝。
      if (next === "mihomo") {
        const ok = await confirm({
          title: t("切换到「{mode}」？", { mode: PROVIDER_LABEL.mihomo }),
          body: (
            <>
              <p>{t("设备将自己安装并运行 Mihomo 内核、按你的 Clash 订阅出站。已填写的 SOCKS5 地址与认证会被清除。")}</p>
              <p className="text-muted-foreground text-xs">{t("在下方安装内核、填写订阅并启用后，出站代理才算配置完成。")}</p>
            </>
          ),
          confirmText: t("切换"),
        });
        if (!ok) return;
      }
      setBusy(true);
      try {
        await api.updateEgress({ provider: next });
        toast(t("代理方式已切换到「{mode}」", { mode: PROVIDER_LABEL[pickOf(next)] }));
        setDraftProvider(null);
        if (next !== "mihomo" && !status.configured) {
          setDraftProvider(next);
          setEditing(true);
        }
        reload();
      } catch (err: unknown) {
        toast.error(api.errorMessage(err));
      } finally {
        setBusy(false);
      }
      return;
    }
    if (!status.configured) {
      setDraftProvider(next);
      setEditing(true);
      return;
    }
    // 已配置：只换方式，地址与认证照旧。
    setBusy(true);
    try {
      await api.updateEgress({ provider: next });
      toast(t("代理方式已切换到「{mode}」", { mode: PROVIDER_LABEL[pickOf(next)] }));
      reload();
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  async function runTest(): Promise<void> {
    setBusy(true);
    try {
      const res = await api.testEgress();
      const last = res.egress.last_test;
      if (last?.ok === true) toast(t("代理连通性测试通过"));
      else toast.error(last?.message ?? t("代理连通性测试未通过"));
      reload();
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  const showForm = editing || draftProvider !== null;
  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-3">
        <Label htmlFor="egress-provider">{t("代理方式")}</Label>
        <Select value={pickOf(shownProvider)} onValueChange={(v) => void pick(v as ProviderPick)} disabled={busy}>
          <SelectTrigger id="egress-provider" size="sm" className="w-48">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PROVIDER_PICKS.map((p) => (
              <SelectItem key={p} value={p}>
                {PROVIDER_LABEL[p]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {status.configured ? (
          status.available ? (
            <Badge variant="outline" className="text-signal-ok">
              {t("已配置")}
            </Badge>
          ) : (
            <Badge variant="outline" className="text-signal-alert" title={status.unavailable_reason}>
              {t("凭据不可用")}
            </Badge>
          )
        ) : (
          <Badge variant="secondary">{t("未配置")}</Badge>
        )}
        <Button type="button" variant="ghost" size="xs" className="ml-auto" onClick={() => setHelpOpen(true)}>
          <CircleHelp />
          {t("说明")}
        </Button>
      </div>
      {status.configured && !status.available && status.unavailable_reason !== undefined ? (
        <p className="text-signal-alert text-xs">{status.unavailable_reason}</p>
      ) : null}
      {shownProvider === "mihomo" ? (
        proxyCore === null ? (
          <p className="text-muted-foreground text-xs">{t("内置内核读数暂时不可用，请刷新重试。")}</p>
        ) : (
          <MihomoPanel status={proxyCore} reload={reload} />
        )
      ) : shownProvider !== "" ? (
        showForm ? (
          <EndpointForm
            key={`${shownProvider}-${status.address}`}
            status={status}
            provider={shownProvider}
            draft={draftProvider !== null}
            onCancel={() => {
              setEditing(false);
              setDraftProvider(null);
            }}
            onSaved={() => {
              setEditing(false);
              setDraftProvider(null);
              reload();
            }}
          />
        ) : (
          <div className="flex flex-col gap-2">
            <dl className="grid grid-cols-[5rem_1fr] gap-y-1 text-xs">
              <dt className="text-muted-foreground">{t("代理地址")}</dt>
              <dd>
                <code className="font-mono break-all">{status.address}</code>
              </dd>
              <dt className="text-muted-foreground">{t("认证")}</dt>
              <dd>{status.username_set ? t("用户名/口令（已封存）") : t("无")}</dd>
            </dl>
            <div className="flex flex-wrap gap-2">
              <Button type="button" variant="outline" size="xs" disabled={busy} onClick={() => setEditing(true)}>
                <Pencil />
                {t("编辑")}
              </Button>
              <Button type="button" variant="outline" size="xs" disabled={busy || !status.available} onClick={() => void runTest()}>
                {busy ? <Loader2Icon className="animate-spin" /> : <PlugZap />}
                {t("测试连通性")}
              </Button>
            </div>
            {status.last_test !== undefined ? (
              <TestResultView res={status.last_test} />
            ) : (
              <p className="text-muted-foreground text-xs">
                {t("测试对固定的 HTTPS 目标做「连接代理 → SOCKS5 握手 → 目标 TLS → HTTPS 请求」四层诊断，不发送提示词或任何上游凭据。")}
              </p>
            )}
          </div>
        )
      ) : (
        <p className="text-muted-foreground text-xs">{t("当前全部流量直连。选择一种代理方式后填写端点，再在下方为需要的流量勾选经代理。")}</p>
      )}
      <EgressHelpDialog open={helpOpen} onOpenChange={setHelpOpen} />
    </Card>
  );
}

// ---- 出站范围 ----

function ScopesCard({ status, reload }: { status: api.EgressStatus; reload: () => void }) {
  const [routes, setRoutes] = useState<Record<api.EgressScope, api.EgressRoute>>(() => {
    const init = {} as Record<api.EgressScope, api.EgressRoute>;
    for (const s of status.scopes) init[s.id] = s.route;
    return init;
  });
  const [busy, setBusy] = useState(false);
  const dirty = status.scopes.some((s) => routes[s.id] !== s.route);

  function save(): void {
    const patch: Partial<Record<api.EgressScope, api.EgressRoute>> = {};
    for (const s of status.scopes) if (routes[s.id] !== s.route) patch[s.id] = routes[s.id];
    setBusy(true);
    api.updateEgress({ routes: patch }).then(
      () => {
        setBusy(false);
        toast(t("出站范围已保存，新请求即按新配置选路"));
        reload();
      },
      (err: unknown) => {
        setBusy(false);
        toast.error(api.errorMessage(err));
      },
    );
  }

  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-col gap-1">
        <span className="font-medium">{t("出站范围")}</span>
        <p className="text-muted-foreground text-xs">
          {status.configured
            ? t("为每类流量选择直连或经代理。选了经代理的流量在代理不可用时直接失败，不会自动直连。")
            : t("先配置代理端点，才能把流量设为经代理。")}
        </p>
      </div>
      <ul className="divide-y">
        {status.scopes.map((s) => {
          const err = status.last_errors?.[s.id];
          return (
            <li key={s.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-2">
              <div className="min-w-0 flex-1">
                <div className="text-sm font-medium">{s.label}</div>
                <div className="text-muted-foreground text-xs">{SCOPE_DESC[s.id]}</div>
                {err !== undefined && s.route === "proxy" ? (
                  <div className="text-signal-alert text-xs">{t("最近失败：{message}（{time}）", { message: err.message, time: fmtTime(err.at) })}</div>
                ) : null}
              </div>
              <Select
                value={routes[s.id]}
                onValueChange={(v) => setRoutes({ ...routes, [s.id]: v as api.EgressRoute })}
                disabled={busy || !status.configured}
              >
                <SelectTrigger size="sm" className="w-28" aria-label={t("{scope} 的出口", { scope: s.label })}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="direct">{t("直连")}</SelectItem>
                  <SelectItem value="proxy">{t("经代理")}</SelectItem>
                </SelectContent>
              </Select>
            </li>
          );
        })}
      </ul>
      <div className="flex items-center gap-2">
        <Button type="button" size="sm" disabled={busy || !dirty} onClick={save}>
          {t("保存出站范围")}
        </Button>
        {dirty ? <span className="text-muted-foreground text-xs">{t("有未保存的修改")}</span> : null}
      </div>
    </Card>
  );
}

// ---- 账号覆盖 ----

function OverridesCard({ status }: { status: api.EgressStatus }) {
  if (status.overrides.length === 0) return null;
  const modeLabel = (m: api.EgressMode): string => (m === "proxy" ? t("经代理") : m === "direct" ? t("直连") : t("跟随设备设置"));
  return (
    <Card className="gap-2 p-5">
      <span className="font-medium">{t("按账号覆盖")}</span>
      <p className="text-muted-foreground text-xs">
        {t("以下上游账号在「模型接入」页单独指定了出站方式，不跟随「模型与订阅接口」的设置。同一模型挂了不同出口的来源时，切换来源的重试可能经不同出口。")}
      </p>
      <ul className="flex flex-wrap gap-2 text-xs">
        {status.overrides.map((o) => (
          <li key={o.upstream_id} className="flex items-center gap-1.5 rounded-md border px-2 py-1">
            <span className={cn(o.disabled && "text-muted-foreground line-through")}>{o.name}</span>
            <Badge variant="outline" className={o.egress_mode === "proxy" ? "text-signal-ok" : ""}>
              {modeLabel(o.egress_mode)}
            </Badge>
          </li>
        ))}
      </ul>
    </Card>
  );
}

// ---- 分区 ----

export function EgressSection({
  status,
  proxyCore,
  reload,
}: {
  status: api.EgressStatus;
  proxyCore: api.ProxyCoreStatus | null;
  reload: () => void;
}): React.ReactElement {
  return (
    <>
      <EndpointCard status={status} proxyCore={proxyCore} reload={reload} />
      <ScopesCard key={status.scopes.map((s) => s.route).join(",") + (status.configured ? "1" : "0")} status={status} reload={reload} />
      <OverridesCard status={status} />
    </>
  );
}
