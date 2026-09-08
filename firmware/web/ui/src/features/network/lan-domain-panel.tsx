// 内网域名面板（网络/域名/代理页「内网域名」分区）。
//
// 提供方式二选一：LLM Gate官网 / 自有域名。两种都先关联官网账号（证书由官网以该账号名义向
// CA 申请，设备只交 CSR）：
//   1. 关联官网账号——设备向官网申请一枚关联码，管理员在官网 /link/ 页（没有账号可现场
//      用邮箱验证码注册）确认；对话框开着时反复请求服务端陪等（每轮 ≤25 秒），直到终态。
//   2a. 官网方式：申领 <前缀>.llm.net 并签发——填前缀、选解析到哪块网卡的 IP；官网写 DNS、
//       按配额选 CA 用 DNS-01 签发。
//   2b. 自有域名方式：登记自己的域名——官网不写它的解析，面板列出管理员要在自己 DNS 服务商
//       设的两条记录（A 指到设备内网 IP、_acme-challenge CNAME 委托到官网），「检查 DNS 设置」
//       由官网用公共解析器逐条核对，就绪后「签发证书」；续期全自动。
//
// 零轮询：除两段由人发起的陪等——关联对话框、签发进行中（服务端阶段一变就回一帧，界面显示
// 阶段再陪等下一帧，直到终态）——读数只在进页/刷新/动作后重取一次。
// 令牌、私钥不经过界面；读数里只有域名、IP、委托名、证书公开字段与最近一次错误。

import { CircleHelp, Copy, ExternalLink, Loader2Icon, Pencil, RefreshCw, SearchCheck } from "lucide-react";
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
import * as api from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t, tx } from "@/lib/i18n";

// ---- 小工具 ----

type Tone = "ok" | "alert" | "off";

function LampBadge({ name, label, tone, title }: { name: string; label: string; tone: Tone; title?: string | undefined }): React.ReactElement {
  return (
    <span className="flex items-center gap-1.5 text-xs" title={title}>
      <span className="text-muted-foreground">{name}</span>
      <Badge variant={tone === "off" ? "secondary" : "outline"} className={cn(tone === "ok" && "text-signal-ok", tone === "alert" && "text-signal-alert")}>
        {label}
      </Badge>
    </span>
  );
}

function caLabel(ca: string | undefined): string {
  if (ca === "google") return "Google Trust Services";
  if (ca === "letsencrypt") return "Let's Encrypt";
  return ca ?? "";
}

function looksLikeLabel(s: string): boolean {
  return /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(s) && s.length >= 5 && s.length <= 32 && !s.includes("--");
}

// looksLikeHostname 是自有域名的界面预检（服务端与官网为准）：至少两级，各级字母数字连字符。
function looksLikeHostname(s: string): boolean {
  if (s.length < 4 || s.length > 253 || s.includes("*")) return false;
  const labels = s.split(".");
  if (labels.length < 2) return false;
  return labels.every((l) => /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(l) && l.length <= 63);
}

function CopyButton({ value, done }: { value: string; done: string }): React.ReactElement {
  return (
    <Button
      type="button"
      variant="ghost"
      size="xs"
      aria-label={t("复制")}
      onClick={() => {
        void copyText(value).then((ok) => {
          toast(ok ? done : t("复制失败，请手动选择后复制"));
        });
      }}
    >
      <Copy />
    </Button>
  );
}

// ---- 说明 ----

function LanDomainHelpDialog({ open, onOpenChange, suffix }: { open: boolean; onOpenChange: (open: boolean) => void; suffix: string }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("内网域名说明")}</DialogTitle>
          <DialogDescription asChild>
            <div className="space-y-3 leading-relaxed">
              <p>
                {tx("<m>LLM Gate官网</m>：把设备关联到你的官网账号后，可申领一个 <c>{suffix}</c> 下的域名。官网把它解析到设备的内网 IP，并通过 Google Trust Services 或 Let's Encrypt（按配额自动选择）签发受信 HTTPS 证书。", {
                  m: (s) => <span className="text-foreground font-medium">{s}</span>,
                  c: (s) => <code className="font-mono">{s}</code>,
                  suffix: `${t("<前缀>")}.${suffix}`,
                })}
              </p>
              <p>
                {tx("<m>自有域名</m>：用你自己的域名（例如 <c>box.example.com</c>）。你在域名的 DNS 服务商把 A 记录指到设备的内网 IP，再加一条 <c>_acme-challenge</c> 的 CNAME 记录把证书验证委托给官网；官网据此向 Google Trust Services 申请证书并自动续期。官网不会改动你域名的其他记录。", {
                  m: (s) => <span className="text-foreground font-medium">{s}</span>,
                  c: (s) => <code className="font-mono">{s}</code>,
                })}
              </p>
              <p>{t("两种方式都需要先关联官网账号：证书以该账号的名义申请。证书私钥只在设备上生成和保存，官网只接收证书签名请求（CSR）；官网账号不因此获得设备的管理权限、模型密钥或用量。")}</p>
              <p>{t("域名解析到的是内网地址，只在同一局域网内可达；设备在证书到期前 30 天自动续期（官网申领的域名还会每半天核对一次解析地址）。")}</p>
              <p>{t("纯 IP 访问始终可用：官网不可达、证书失败只影响域名 HTTPS 入口。")}</p>
            </div>
          </DialogDescription>
        </DialogHeader>
        <DialogFooter showCloseButton />
      </DialogContent>
    </Dialog>
  );
}

// ---- 提供方式 ----

function providerLabel(id: api.LanDomainProvider, fallback: string): string {
  if (id === "official_site") return t("LLM Gate官网");
  if (id === "own_domain") return t("自有域名");
  return fallback;
}

function ProviderCard({
  status,
  busy,
  onPick,
}: {
  status: api.LanDomainStatus;
  busy: boolean;
  onPick: (provider: api.LanDomainProvider) => void;
}) {
  const [helpOpen, setHelpOpen] = useState(false);
  const options: { id: api.LanDomainProvider; label: string; available: boolean }[] = [
    { id: "", label: t("不使用"), available: true },
    ...status.providers.map((p) => ({ id: p.id, label: providerLabel(p.id, p.label), available: p.available })),
  ];
  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-3">
        <Label htmlFor="lan-domain-provider">{t("提供方式")}</Label>
        <Select value={status.provider} onValueChange={(v) => onPick(v as api.LanDomainProvider)} disabled={busy}>
          <SelectTrigger id="lan-domain-provider" size="sm" className="w-56">
            <SelectValue placeholder={t("不使用")} />
          </SelectTrigger>
          <SelectContent>
            {options.map((o) => (
              <SelectItem key={o.id || "none"} value={o.id} disabled={!o.available}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {!status.site_configured ? <span className="text-muted-foreground text-xs">{t("本进程未接入 LLM Gate官网")}</span> : null}
        <Button type="button" variant="ghost" size="xs" className="ml-auto" onClick={() => setHelpOpen(true)}>
          <CircleHelp />
          {t("说明")}
        </Button>
      </div>
      <LanDomainHelpDialog open={helpOpen} onOpenChange={setHelpOpen} suffix={status.suffix} />
    </Card>
  );
}

// ---- 第一步：关联官网账号 ----

// LinkDialog 发起关联并反复陪等直到终态；关掉对话框即取消本地会话（官网侧关联码自然过期）。
function LinkDialog({ open, onOpenChange, onLinked }: { open: boolean; onOpenChange: (open: boolean) => void; onLinked: () => void }) {
  const [pending, setPending] = useState<api.LanDomainPending | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);

  useEffect(() => {
    if (!open) return;
    setPending(null);
    setError(null);
    setStarting(true);
    let cancelled = false;
    (async () => {
      try {
        const started = await api.lanDomainLinkStart();
        if (cancelled) return;
        setStarting(false);
        setPending(started.lan_domain.link.pending ?? null);
        // 陪等循环：每轮由服务端最多等 25 秒；终态即停。
        for (;;) {
          if (cancelled) return;
          const res = await api.lanDomainLinkWait();
          if (cancelled) return;
          const p = res.lan_domain.link.pending ?? null;
          setPending(p);
          if (p === null || p.status !== "pending") {
            if (p?.status === "approved") {
              toast(t("已关联 LLM Gate官网账号"));
              onLinked();
            }
            return;
          }
        }
      } catch (err: unknown) {
        if (cancelled) return;
        setStarting(false);
        setError(api.errorMessage(err));
      }
    })();
    return () => {
      cancelled = true;
      void api.lanDomainLinkCancel().catch(() => undefined);
    };
    // onLinked 只在批准时调用一次，不作为重启依据。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const done = pending !== null && pending.status !== "pending";
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("关联 LLM Gate官网账号")}</DialogTitle>
          <DialogDescription asChild>
            <div className="space-y-4">
              {error !== null ? (
                <p role="alert" className="text-destructive text-sm">
                  {error}
                </p>
              ) : starting || pending === null ? (
                <p className="text-sm">{t("正在向 LLM Gate官网申请关联码…")}</p>
              ) : (
                <>
                  <p className="text-sm">
                    {t("在手机或电脑浏览器打开下面的地址，登录官网账号（没有账号可用邮箱验证码现场注册），核对设备信息后点「关联到我的账号」。")}
                  </p>
                  <div className="flex flex-col items-center gap-2 rounded-md border p-4">
                    <span className="text-muted-foreground text-xs">{t("关联码")}</span>
                    <code className="font-mono text-2xl font-semibold tracking-[0.2em]">{pending.user_code}</code>
                    <a
                      className="text-signal-ok flex items-center gap-1 text-sm underline"
                      href={pending.verification_url_complete}
                      target="_blank"
                      rel="noreferrer"
                    >
                      {pending.verification_url_complete}
                      <ExternalLink className="size-3.5" />
                    </a>
                    <Button
                      type="button"
                      size="xs"
                      variant="outline"
                      onClick={() => {
                        void copyText(pending.verification_url_complete).then((ok) => {
                          toast(ok ? t("已复制关联地址") : t("复制失败，请手动选择后复制"));
                        });
                      }}
                    >
                      <Copy />
                      {t("复制地址")}
                    </Button>
                  </div>
                  {pending.status === "pending" ? (
                    <p className="text-muted-foreground text-xs">
                      {t("等待官网确认… 关联码 {time} 前有效；本对话框会自动继续。", { time: fmtTime(pending.expires_at) })}
                    </p>
                  ) : pending.status === "approved" ? (
                    <p className="text-signal-ok text-sm">{t("已关联，可以继续下一步了。")}</p>
                  ) : pending.status === "denied" ? (
                    <p className="text-signal-alert text-sm">{t("官网账号拒绝了这次关联。")}</p>
                  ) : pending.status === "expired" ? (
                    <p className="text-signal-alert text-sm">{t("关联码已过期，请重新发起。")}</p>
                  ) : (
                    <p className="text-signal-alert text-sm">{pending.error ?? t("关联失败，请重试。")}</p>
                  )}
                </>
              )}
            </div>
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button type="button" variant={done ? "default" : "outline"} onClick={() => onOpenChange(false)}>
            {done ? t("完成") : t("取消")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function LinkCard({ status, reload }: { status: api.LanDomainStatus; reload: () => void }) {
  const confirm = useConfirm();
  const [linkOpen, setLinkOpen] = useState(false);
  const [busy, setBusy] = useState(false);

  async function unlink(): Promise<void> {
    const ok = await confirm({
      title: t("解除官网账号关联？"),
      body: (
        <p>
          {!status.claimed
            ? t("解除后需要重新关联才能申领或登记域名。")
            : status.kind === "custom"
              ? t("已登记的 {hostname} 会随之取消：官网不再为它签发或续签证书，本机证书删除、HTTPS 入口失效；你自己 DNS 里的记录不受影响。", { hostname: status.hostname ?? "" })
              : t("已申领的 {hostname} 会随之释放：解析记录删除、本机证书删除、HTTPS 入口失效。", { hostname: status.hostname ?? "" })}
        </p>
      ),
      confirmText: t("解除关联"),
      danger: true,
    });
    if (!ok) return;
    setBusy(true);
    try {
      await api.lanDomainUnlink();
      toast(t("已解除官网账号关联"));
      reload();
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{t("第一步：关联 LLM Gate官网账号")}</span>
        <LampBadge
          name={t("账号")}
          label={status.link.linked ? status.link.account || t("已关联") : t("未关联")}
          tone={status.link.linked ? "ok" : "off"}
        />
        {status.link.linked ? (
          <Button type="button" variant="outline" size="xs" className="ml-auto" disabled={busy || status.issuing} onClick={() => void unlink()}>
            {t("解除关联")}
          </Button>
        ) : (
          <Button type="button" size="xs" className="ml-auto" disabled={!status.site_configured} onClick={() => setLinkOpen(true)}>
            {t("关联官网账号")}
          </Button>
        )}
      </div>
      <p className="text-muted-foreground text-xs">
        {status.provider === "own_domain"
          ? t("证书由官网以这个账号的名义向证书签发方申请；官网账号只用于申请证书，不获得设备管理权限；随时可在官网账号设置或这里解除。")
          : t("官网账号只用于申领域名与签发证书，不获得设备管理权限；随时可在官网账号设置或这里解除。")}
      </p>
      <LinkDialog
        open={linkOpen}
        onOpenChange={setLinkOpen}
        onLinked={() => {
          reload();
        }}
      />
    </Card>
  );
}

// ---- 共用：证书灯、解析地址与监听地址编辑 ----

function certLamp(status: api.LanDomainStatus): { label: string; tone: Tone; title?: string } {
  if (status.issuing) return { label: t("签发中"), tone: "alert" };
  const c = status.cert;
  if (c === undefined) return { label: t("未签发"), tone: "off" };
  if (!c.covers_hostname) return { label: t("证书与域名不符"), tone: "alert", title: t("证书覆盖的是别的域名，请重新签发") };
  if (c.expiring_soon) return { label: t("到期 {time}，待续期", { time: fmtTime(c.not_after) }), tone: "alert" };
  return { label: t("有效至 {time}", { time: fmtTime(c.not_after) }), tone: "ok" };
}

function AddressSelect({ id, value, onChange, addresses }: { id?: string; value: string; onChange: (v: string) => void; addresses: api.AccessAddress[] }) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger id={id} size="sm" className="w-64">
        <SelectValue placeholder={t("选择内网 IP")} />
      </SelectTrigger>
      <SelectContent>
        {addresses.map((a) => (
          <SelectItem key={a.host} value={a.host}>
            {a.host}（{a.interface}）
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}

// TargetEditor 是「解析到」那一行：显示当前地址，可改。官网方式改的是官网 A 记录；自有域名
// 方式改的只是设备期望的地址（面板上 A 记录的示例值），管理员要自己同步改 DNS。
function TargetEditor({ status, busy, onSave }: { status: api.LanDomainStatus; busy: boolean; onSave: (ip: string) => void }) {
  const [editing, setEditing] = useState(false);
  const [ip, setIP] = useState(status.target_ip ?? "");
  if (editing) {
    return (
      <>
        <AddressSelect value={ip} onChange={setIP} addresses={status.addresses} />
        <Button
          type="button"
          size="xs"
          disabled={busy}
          onClick={() => {
            setEditing(false);
            onSave(ip);
          }}
        >
          {t("保存")}
        </Button>
        <Button type="button" size="xs" variant="outline" onClick={() => setEditing(false)}>
          {t("取消")}
        </Button>
      </>
    );
  }
  return (
    <>
      <code className="font-mono">{status.target_ip}</code>
      <Button type="button" variant="ghost" size="xs" disabled={busy} onClick={() => setEditing(true)}>
        <Pencil />
        {t("修改")}
      </Button>
    </>
  );
}

function ListenEditor({ status, busy, onSave }: { status: api.LanDomainStatus; busy: boolean; onSave: (listen: string) => void }) {
  const [editing, setEditing] = useState(false);
  const [listen, setListen] = useState(status.https_listen);
  if (editing) {
    return (
      <>
        <Input value={listen} className="h-8 w-40 font-mono" autoComplete="off" spellCheck={false} onChange={(e) => setListen(e.target.value)} />
        <Button
          type="button"
          size="xs"
          disabled={busy}
          onClick={() => {
            setEditing(false);
            onSave(listen);
          }}
        >
          {t("保存")}
        </Button>
        <Button type="button" size="xs" variant="outline" onClick={() => setEditing(false)}>
          {t("取消")}
        </Button>
      </>
    );
  }
  return (
    <>
      <code className="font-mono">{status.https_listen}</code>
      <Button type="button" variant="ghost" size="xs" disabled={busy} onClick={() => setEditing(true)}>
        <Pencil />
        {t("修改")}
      </Button>
      <HelpTip label={t("HTTPS 监听说明")}>
        {t("证书就绪后设备在这个地址上提供 HTTPS，缺省 :443（全部网卡）。与客户自管 TLS 监听端口冲突时会启动失败，改成别的端口即可。")}
      </HelpTip>
    </>
  );
}

function LastError({ status }: { status: api.LanDomainStatus }): React.ReactElement | null {
  if (status.last_error === undefined || status.last_error === "") return null;
  return (
    <p className="text-signal-alert text-xs">
      {status.last_error}
      {status.last_error_at ? <span className="text-muted-foreground">（{fmtTime(status.last_error_at)}）</span> : null}
    </p>
  );
}

// ---- 签发进度 ----

function stageText(stage: api.LanIssueStage | undefined): string {
  switch (stage) {
    case "submitting":
      return t("生成密钥与证书请求，向官网提交订单…");
    case "pending":
      return t("官网正在向证书签发方创建订单…");
    case "authorizing":
      return t("正在写入 DNS 验证记录…");
    case "challenging":
      return t("等待 DNS 验证记录在公共解析器生效（这一步最慢，通常十几秒到一两分钟）…");
    case "validating":
      return t("证书签发方正在验证域名…");
    case "finalizing":
      return t("证书签发方正在签发证书…");
    case "installing":
      return t("校验并安装证书，启动 HTTPS 监听…");
    default:
      return t("签发中…");
  }
}

const STAGE_ORDER: api.LanIssueStage[] = ["submitting", "pending", "authorizing", "challenging", "validating", "finalizing", "installing"];

// IssueProgress 是签发进行中的那一块：转圈 + 当前阶段 + 七格进度条。
function IssueProgress({ stage }: { stage: api.LanIssueStage | undefined }): React.ReactElement {
  const index = stage === undefined ? -1 : STAGE_ORDER.indexOf(stage);
  return (
    <div role="status" aria-live="polite" className="flex flex-col gap-2 rounded-md border p-3 text-xs">
      <div className="flex items-center gap-2">
        <Loader2Icon className="text-signal-ok size-4 animate-spin motion-reduce:animate-none" aria-hidden="true" />
        <span>{stageText(stage)}</span>
      </div>
      <div className="flex gap-1" aria-hidden="true">
        {STAGE_ORDER.map((s, i) => (
          <span key={s} className={cn("h-1 flex-1 rounded-full", i <= index ? "bg-signal-ok" : "bg-muted")} />
        ))}
      </div>
      <p className="text-muted-foreground">{t("证书由官网向证书签发方申请，全程通常 30–90 秒；关掉本页也不会中断，回来刷新即可看到结果。")}</p>
    </div>
  );
}

// useIssueWatch 在签发进行中反复陪等服务端（阶段一变就回一帧），终态时 toast 并重取读数。
// 返回当前阶段（陪等帧比页面读数新）。只在 status.issuing 为真时工作；卸载即停。
function useIssueWatch(status: api.LanDomainStatus, reload: () => void): api.LanIssueStage | undefined {
  const [stage, setStage] = useState<api.LanIssueStage | undefined>(status.issue_stage);
  useEffect(() => {
    setStage(status.issue_stage);
    if (!status.issuing) return;
    let cancelled = false;
    (async () => {
      let since: api.LanIssueStage | "" = status.issue_stage ?? "";
      for (;;) {
        let res: api.LanDomainStatus;
        try {
          res = (await api.lanDomainIssueWait(since)).lan_domain;
        } catch (err: unknown) {
          if (cancelled) return;
          toast.error(api.errorMessage(err));
          reload();
          return;
        }
        if (cancelled) return;
        if (!res.issuing) {
          if (res.last_error) toast.error(res.last_error);
          else toast(t("证书已签发"));
          reload();
          return;
        }
        since = res.issue_stage ?? "";
        setStage(res.issue_stage);
      }
    })();
    return () => {
      cancelled = true;
    };
    // reload 是稳定回调；只在签发状态/阶段变化时重启陪等。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status.issuing, status.issue_stage]);
  return stage;
}

// IssueButton 是「签发证书 / 立即续期」：点下去先发起（服务端陪等到阶段离开 submitting 或结束），
// 之后由 useIssueWatch 接手显示进度；进行中带转圈。
function IssueButton({ status, busy, onStart, primary }: { status: api.LanDomainStatus; busy: boolean; onStart: () => void; primary: boolean }): React.ReactElement {
  const running = busy || status.issuing;
  return (
    <Button type="button" size="sm" variant={primary && !running ? "default" : "outline"} disabled={running} onClick={onStart}>
      {running ? <Loader2Icon className="animate-spin motion-reduce:animate-none" aria-hidden="true" /> : null}
      {running ? t("签发中…") : status.cert === undefined ? t("签发证书") : t("立即续期")}
    </Button>
  );
}

// useAction 把「跑一个动作 → toast → 重取」收成一处；busy 记当前在跑的动作名。
function useAction(reload: () => void): [string | null, (key: string, action: () => Promise<unknown>, done: string) => Promise<void>] {
  const [busy, setBusy] = useState<string | null>(null);
  async function run(key: string, action: () => Promise<unknown>, done: string): Promise<void> {
    setBusy(key);
    try {
      await action();
      if (done !== "") toast(done);
      reload();
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      setBusy(null);
    }
  }
  return [busy, run];
}

// ---- 第二步（官网方式）：申领 <前缀>.llm.net ----

function ClaimForm({ status, onDone }: { status: api.LanDomainStatus; onDone: () => void }) {
  const primary = status.addresses.find((a) => a.primary) ?? status.addresses[0];
  const [label, setLabel] = useState("");
  const [ip, setIP] = useState(primary?.host ?? "");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const value = label.trim().toLowerCase();
    if (!looksLikeLabel(value)) {
      setError(t("域名前缀须为 5–32 位小写字母、数字或连字符，不能以连字符开头结尾"));
      return;
    }
    if (ip === "") {
      setError(t("请选择解析到哪一个内网 IP"));
      return;
    }
    setError(null);
    setBusy(true);
    api.lanDomainClaim(value, ip).then(
      (res) => {
        setBusy(false);
        toast(res.lan_domain.issuing ? t("域名已申领，正在签发证书…") : t("域名已申领并签发证书"));
        onDone();
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
        <div className="flex items-center gap-1.5">
          <Label htmlFor="lan-domain-label">{t("域名前缀")}</Label>
          <HelpTip label={t("域名前缀说明")}>
            {t("5–32 位小写字母、数字或连字符。保留名称（www、api、admin 等）与已被占用的前缀会被官网拒绝。")}
          </HelpTip>
        </div>
        <div className="flex items-center gap-2">
          <Input
            id="lan-domain-label"
            value={label}
            autoComplete="off"
            spellCheck={false}
            placeholder="studio"
            className="max-w-56"
            onChange={(e) => setLabel(e.target.value)}
          />
          <code className="text-muted-foreground font-mono text-sm">.{status.suffix}</code>
        </div>
      </div>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="lan-domain-ip">{t("解析到")}</Label>
        <AddressSelect id="lan-domain-ip" value={ip} onChange={setIP} addresses={status.addresses} />
        <p className="text-muted-foreground text-xs">{t("域名会解析到这个内网地址；设备每半天核对一次，IP 变化时自动更新。")}</p>
      </div>
      {error === null ? null : (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      <div>
        <Button type="submit" size="sm" disabled={busy}>
          {busy ? t("申领并签发中（最长约一分钟）…") : t("申领并签发证书")}
        </Button>
      </div>
    </form>
  );
}

// startIssue 发起签发：服务端在阶段离开 submitting 或签发结束时返回；没结束就交给 useIssueWatch
// 显示进度（toast 留到终态），结束了才直接报结果。
async function startIssue(reload: () => void): Promise<void> {
  const res = await api.lanDomainIssue();
  if (!res.lan_domain.issuing) toast(t("证书已签发"));
  reload();
}

function DomainCard({ status, reload }: { status: api.LanDomainStatus; reload: () => void }) {
  const confirm = useConfirm();
  const [busy, run] = useAction(reload);
  const stage = useIssueWatch(status, reload);

  async function release(): Promise<void> {
    const ok = await confirm({
      title: t("释放 {hostname}？", { hostname: status.hostname ?? "" }),
      body: <p>{t("域名将立即停止解析并释放前缀（此后可能被其他设备申领）；本机证书与私钥一并删除，HTTPS 入口随即失效。")}</p>,
      confirmText: t("释放"),
      danger: true,
    });
    if (!ok) return;
    await run("release", () => api.lanDomainRelease(), t("域名已释放"));
  }

  if (!status.claimed) {
    return (
      <Card className="gap-3 p-5">
        <span className="font-medium">{t("第二步：申领域名并签发证书")}</span>
        <LastError status={status} />
        <ClaimForm status={status} onDone={reload} />
      </Card>
    );
  }

  const lamp = certLamp(status);
  return (
    <Card className="gap-3 p-5">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <span className="font-medium">{t("第二步：域名与证书")}</span>
        <LampBadge name={t("证书")} label={lamp.label} tone={lamp.tone} title={lamp.title} />
        <LampBadge
          name="HTTPS"
          label={status.https_active ? t("监听 {addr}", { addr: status.https_addr ?? status.https_listen }) : t("未监听")}
          tone={status.https_active ? "ok" : "off"}
        />
        <Button type="button" variant="ghost" size="xs" className="ml-auto" onClick={reload}>
          <RefreshCw />
          {t("刷新")}
        </Button>
      </div>
      <dl className="grid grid-cols-[5rem_1fr] gap-y-1 text-sm">
        <dt className="text-muted-foreground">{t("域名")}</dt>
        <dd>
          {status.url !== undefined ? (
            <a className="font-mono underline" href={status.url} target="_blank" rel="noreferrer">
              {status.url}
            </a>
          ) : (
            <code className="font-mono">{status.hostname}</code>
          )}
        </dd>
        <dt className="text-muted-foreground">{t("解析到")}</dt>
        <dd className="flex flex-wrap items-center gap-2">
          <TargetEditor status={status} busy={busy !== null} onSave={(ip) => void run("target", () => api.lanDomainSetTarget(ip), t("解析地址已更新"))} />
        </dd>
        {status.cert !== undefined ? (
          <>
            <dt className="text-muted-foreground">{t("签发方")}</dt>
            <dd>
              {status.cert.issuer}
              {status.last_ca ? <span className="text-muted-foreground">（{caLabel(status.last_ca)}）</span> : null}
            </dd>
          </>
        ) : null}
        <dt className="text-muted-foreground">{t("HTTPS 监听")}</dt>
        <dd className="flex flex-wrap items-center gap-2">
          <ListenEditor status={status} busy={busy !== null} onSave={(listen) => void run("listen", () => api.updateLanDomain({ https_listen: listen }), t("HTTPS 监听地址已更新"))} />
        </dd>
      </dl>
      {status.issuing ? <IssueProgress stage={stage} /> : <LastError status={status} />}
      <div className="flex flex-wrap gap-2">
        <IssueButton status={status} busy={busy !== null} primary={false} onStart={() => void run("issue", () => startIssue(reload), "")} />
        <Button type="button" size="sm" variant="outline" disabled={busy !== null || status.issuing} onClick={() => void release()}>
          {t("释放域名")}
        </Button>
      </div>
    </Card>
  );
}

// ---- 第二步（自有域名方式）：登记域名、设 DNS、检查、签发 ----

function RegisterForm({ status, onDone }: { status: api.LanDomainStatus; onDone: () => void }) {
  const primary = status.addresses.find((a) => a.primary) ?? status.addresses[0];
  const [hostname, setHostname] = useState("");
  const [ip, setIP] = useState(primary?.host ?? "");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const value = hostname.trim().toLowerCase().replace(/\.$/, "");
    if (!looksLikeHostname(value)) {
      setError(t("请填写完整域名（至少两级，例如 box.example.com），只能包含小写字母、数字与连字符"));
      return;
    }
    if (ip === "") {
      setError(t("请选择解析到哪一个内网 IP"));
      return;
    }
    setError(null);
    setBusy(true);
    api.lanDomainRegister(value, ip).then(
      () => {
        setBusy(false);
        toast(t("域名已登记，请按下面列出的记录设置 DNS"));
        onDone();
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
        <div className="flex items-center gap-1.5">
          <Label htmlFor="lan-domain-hostname">{t("域名")}</Label>
          <HelpTip label={t("自有域名说明")}>
            {t("你自己持有、能在 DNS 服务商添加记录的域名或子域名。登记后面板会列出要添加的记录；官网不会改动你域名的任何记录。")}
          </HelpTip>
        </div>
        <Input
          id="lan-domain-hostname"
          value={hostname}
          autoComplete="off"
          spellCheck={false}
          placeholder="box.example.com"
          className="max-w-80 font-mono"
          onChange={(e) => setHostname(e.target.value)}
        />
      </div>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="lan-domain-own-ip">{t("解析到")}</Label>
        <AddressSelect id="lan-domain-own-ip" value={ip} onChange={setIP} addresses={status.addresses} />
        <p className="text-muted-foreground text-xs">{t("你需要把域名的 A 记录指到这个内网地址；设备不会替你改 DNS，IP 变化时请自行更新记录。")}</p>
      </div>
      {error === null ? null : (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      <div>
        <Button type="submit" size="sm" disabled={busy}>
          {busy ? t("登记中…") : t("登记域名")}
        </Button>
      </div>
    </form>
  );
}

function checkLamp(status: api.LanDNSCheckStatus): Tone {
  if (status === "ok") return "ok";
  if (status === "error") return "off";
  return "alert";
}

function DNSCheckResult({ check }: { check: api.LanDNSCheck }): React.ReactElement {
  const aText =
    check.a.status === "ok"
      ? t("已指向 {ip}", { ip: check.target_ip })
      : check.a.status === "mismatch"
        ? t("当前解析到 {actual}，期望 {ip}", { actual: check.a.addresses.join(", "), ip: check.target_ip })
        : check.a.status === "missing"
          ? t("公共 DNS 里还没有这条记录")
          : t("暂时无法查询");
  const cnameText =
    check.challenge.status === "ok"
      ? t("已委托到 {target}", { target: check.challenge.target ?? check.acme_delegate })
      : check.challenge.status === "mismatch"
        ? t("当前指向 {actual}，应为 {target}", { actual: check.challenge.target ?? "", target: check.acme_delegate })
        : check.challenge.status === "missing"
          ? t("公共 DNS 里还没有这条记录")
          : t("暂时无法查询");
  const caaText =
    check.caa.status === "none"
      ? t("未设置 CAA，不限制签发方")
      : check.caa.status === "ok"
        ? t("{name} 的 CAA 允许签发", { name: check.caa.found_at ?? "" })
        : check.caa.status === "blocked"
          ? t("{name} 的 CAA 不允许当前签发方，请添加 CAA 0 issue \"pki.goog\"", { name: check.caa.found_at ?? "" })
          : t("暂时无法查询");
  const caaTone: Tone = check.caa.status === "blocked" ? "alert" : check.caa.status === "error" ? "off" : "ok";
  return (
    <div className="flex flex-col gap-1.5 rounded-md border p-3 text-xs">
      <LampBadge name={t("A 记录")} label={aText} tone={checkLamp(check.a.status)} />
      <LampBadge name={t("证书验证委托")} label={cnameText} tone={checkLamp(check.challenge.status)} />
      <LampBadge name="CAA" label={caaText} tone={caaTone} />
      <p className="text-muted-foreground">
        {check.ready ? t("证书签发的前置条件已满足；A 记录只影响访问，不影响签发。") : t("证书验证委托就位（并且 CAA 不拦）后才能签发。DNS 记录通常几分钟内生效，改完可再次检查。")}
        {check.checked_at ? <span>（{t("检查时间 {time}", { time: fmtTime(check.checked_at) })}）</span> : null}
      </p>
    </div>
  );
}

function OwnDomainCard({ status, reload }: { status: api.LanDomainStatus; reload: () => void }) {
  const confirm = useConfirm();
  const [busy, run] = useAction(reload);
  const stage = useIssueWatch(status, reload);
  const [check, setCheck] = useState<api.LanDNSCheck | null>(null);
  const [checking, setChecking] = useState(false);

  async function runCheck(): Promise<void> {
    setChecking(true);
    try {
      const res = await api.lanDomainDNSCheck();
      setCheck(res.dns_check);
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      setChecking(false);
    }
  }

  async function release(): Promise<void> {
    const ok = await confirm({
      title: t("取消登记 {hostname}？", { hostname: status.hostname ?? "" }),
      body: <p>{t("官网不再为这个域名签发或续签证书；本机证书与私钥一并删除，HTTPS 入口随即失效。你自己 DNS 里的记录不受影响，可自行删除。")}</p>,
      confirmText: t("取消登记"),
      danger: true,
    });
    if (!ok) return;
    setCheck(null);
    await run("release", () => api.lanDomainRelease(), t("已取消登记"));
  }

  if (!status.claimed) {
    return (
      <Card className="gap-3 p-5">
        <span className="font-medium">{t("第二步：登记你的域名")}</span>
        <LastError status={status} />
        <RegisterForm status={status} onDone={reload} />
      </Card>
    );
  }

  const lamp = certLamp(status);
  const records = status.dns_records ?? [];
  return (
    <Card className="gap-4 p-5">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <span className="font-medium">{t("第二步：域名、DNS 与证书")}</span>
        <LampBadge name={t("证书")} label={lamp.label} tone={lamp.tone} title={lamp.title} />
        <LampBadge
          name="HTTPS"
          label={status.https_active ? t("监听 {addr}", { addr: status.https_addr ?? status.https_listen }) : t("未监听")}
          tone={status.https_active ? "ok" : "off"}
        />
        <Button type="button" variant="ghost" size="xs" className="ml-auto" onClick={reload}>
          <RefreshCw />
          {t("刷新")}
        </Button>
      </div>
      <dl className="grid grid-cols-[5rem_1fr] gap-y-1 text-sm">
        <dt className="text-muted-foreground">{t("域名")}</dt>
        <dd>
          {status.url !== undefined ? (
            <a className="font-mono underline" href={status.url} target="_blank" rel="noreferrer">
              {status.url}
            </a>
          ) : (
            <code className="font-mono">{status.hostname}</code>
          )}
        </dd>
        <dt className="text-muted-foreground">{t("解析到")}</dt>
        <dd className="flex flex-wrap items-center gap-2">
          <TargetEditor
            status={status}
            busy={busy !== null}
            onSave={(ip) => {
              setCheck(null);
              void run("target", () => api.lanDomainSetTarget(ip), t("期望解析地址已更新，请同步修改 A 记录"));
            }}
          />
        </dd>
        {status.cert !== undefined ? (
          <>
            <dt className="text-muted-foreground">{t("签发方")}</dt>
            <dd>
              {status.cert.issuer}
              {status.last_ca ? <span className="text-muted-foreground">（{caLabel(status.last_ca)}）</span> : null}
            </dd>
          </>
        ) : null}
        <dt className="text-muted-foreground">{t("HTTPS 监听")}</dt>
        <dd className="flex flex-wrap items-center gap-2">
          <ListenEditor status={status} busy={busy !== null} onSave={(listen) => void run("listen", () => api.updateLanDomain({ https_listen: listen }), t("HTTPS 监听地址已更新"))} />
        </dd>
      </dl>

      <div className="flex flex-col gap-2">
        <div className="flex items-center gap-1.5">
          <span className="text-sm font-medium">{t("在域名的 DNS 服务商添加这些记录")}</span>
          <HelpTip label={t("DNS 记录说明")}>
            {t("A 记录让内网里的设备按名字找到这台盒子；CNAME 记录把证书验证（DNS-01）委托给官网，官网只在委托名上写临时的验证记录。两条记录设一次即可，续期不需要再改。如果你的域名设置了 CAA 记录，还需允许 pki.goog 签发。")}
          </HelpTip>
        </div>
        <div className="overflow-x-auto rounded-md border">
          <table className="w-full text-xs">
            <thead className="text-muted-foreground">
              <tr className="border-b">
                <th className="px-3 py-1.5 text-left font-medium">{t("类型")}</th>
                <th className="px-3 py-1.5 text-left font-medium">{t("名称")}</th>
                <th className="px-3 py-1.5 text-left font-medium">{t("值")}</th>
              </tr>
            </thead>
            <tbody>
              {records.map((rec) => (
                <tr key={rec.type + rec.name} className="border-b last:border-0">
                  <td className="px-3 py-1.5 font-mono">{rec.type}</td>
                  <td className="px-3 py-1.5">
                    <span className="flex items-center gap-1">
                      <code className="font-mono">{rec.name}</code>
                      <CopyButton value={rec.name} done={t("已复制记录名称")} />
                    </span>
                  </td>
                  <td className="px-3 py-1.5">
                    <span className="flex items-center gap-1">
                      <code className="font-mono">{rec.value}</code>
                      <CopyButton value={rec.value} done={t("已复制记录值")} />
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button type="button" size="sm" variant="outline" disabled={checking || busy !== null} onClick={() => void runCheck()}>
            <SearchCheck />
            {checking ? t("检查中…") : t("检查 DNS 设置")}
          </Button>
          <span className="text-muted-foreground text-xs">{t("由官网向公共解析器查询这三条记录的当前状态，不改任何设置。")}</span>
        </div>
        {check !== null ? <DNSCheckResult check={check} /> : null}
      </div>

      {status.issuing ? <IssueProgress stage={stage} /> : <LastError status={status} />}
      <div className="flex flex-wrap gap-2">
        <IssueButton status={status} busy={busy !== null} primary={status.cert === undefined} onStart={() => void run("issue", () => startIssue(reload), "")} />
        <Button type="button" size="sm" variant="outline" disabled={busy !== null || status.issuing} onClick={() => void release()}>
          {t("取消登记")}
        </Button>
      </div>
    </Card>
  );
}

// ---- 分区 ----

export function LanDomainSection({ status, reload }: { status: api.LanDomainStatus; reload: () => void }): React.ReactElement {
  const confirm = useConfirm();
  const [busy, setBusy] = useState(false);

  async function pick(provider: api.LanDomainProvider): Promise<void> {
    if (provider === status.provider) return;
    if (status.claimed) {
      await confirm({
        title: t("先释放当前域名"),
        body: <p>{status.kind === "custom" ? t("请先取消登记当前的自有域名，再改变提供方式。") : t("请先释放已申领的域名，再改变提供方式。")}</p>,
        confirmText: t("知道了"),
      });
      return;
    }
    if (provider === "") {
      const ok = await confirm({
        title: t("不再使用内网域名？"),
        body: <p>{t("账号关联会保留；需要时可再选回其他提供方式。")}</p>,
        confirmText: t("切换"),
      });
      if (!ok) return;
    }
    setBusy(true);
    try {
      await api.updateLanDomain({ provider });
      toast(provider === "" ? t("已关闭内网域名") : provider === "own_domain" ? t("内网域名将使用你自己的域名") : t("内网域名将由 LLM Gate官网提供"));
      reload();
    } catch (err: unknown) {
      toast.error(api.errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      <ProviderCard status={status} busy={busy} onPick={(p) => void pick(p)} />
      {status.provider === "official_site" || status.provider === "own_domain" ? (
        <>
          <LinkCard status={status} reload={reload} />
          {status.link.linked ? status.provider === "own_domain" ? <OwnDomainCard status={status} reload={reload} /> : <DomainCard status={status} reload={reload} /> : null}
        </>
      ) : null}
    </>
  );
}
