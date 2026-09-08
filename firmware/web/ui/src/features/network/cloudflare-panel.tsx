// Cloudflare Tunnel 面板（网络/域名/代理页「公网接入」分区，与外网映射二选一）。
//
// 页面把四层状态分开摆：组件（cloudflared 装没装、装的哪版）、凭据（token 密封了没）、
// connector（进程连没连上 Cloudflare 边缘）、origin（设备侧专用 socket 与路由策略）。
// 一个「已开启」掩盖不了故障在哪一层，四盏灯才说得清。
//
// 组件本身（安装 / 升级 / 回退 / 卸载）不在这里操作：本面板只放一张先决条件卡
// （features/components/prereq-card.tsx）指向「第三方组件」页。
//
// 版面只留状态、动作与数据：解释性文字收进「说明」对话框与字段旁的问号；设置卡片平时
// 只读，点「编辑」才出表单，保存 / 取消收口。
//
// token 明文只活在本组件的输入框内存里：随一次 PUT 发出后即清空，不进全局状态、URL
// 或日志；任何响应都不回显 token。
//
// 零轮询：所有读数都是进页/刷新/动作后重取一次；拉取与公网探测的等待由服务端同步陪等。

import { CircleHelp, Pencil } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { HelpTip } from "@/components/help-tip";
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
import { Switch } from "@/components/ui/switch";
import { ComponentPrereqCard } from "@/features/components/prereq-card";
import { CLOUDFLARED_SPEC } from "@/features/components/specs";
import * as api from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t, tx } from "@/lib/i18n";

// ---- 状态灯 ----

type Tone = "ok" | "alert" | "off";

interface Lamp {
  label: string;
  tone: Tone;
  title?: string;
}

function componentLamp(c: api.CloudflareComponent): Lamp {
  switch (c.state) {
    case "engine_unavailable":
      return { label: t("引擎不可达"), tone: "alert", title: t("升级引擎（llmgate-updated）不可达，无法安装或启动组件") };
    case "downloading":
      return { label: t("拉取中"), tone: "alert" };
    case "blocked":
      return { label: t("已阻断 {version}", { version: c.version ?? "" }), tone: "alert", title: t("已安装的版本被签名清单标为阻断，请尽快更新") };
    case "staged":
      return { label: t("待安装 {version}", { version: c.staged?.version ?? "" }), tone: "alert" };
    case "not_installed":
      return { label: t("未安装"), tone: "off" };
    case "update_available":
      return { label: t("已验证 {version}（可更新）", { version: c.version ?? "" }), tone: "ok" };
    default:
      return { label: t("已验证 {version}", { version: c.version ?? "" }), tone: "ok" };
  }
}

function credentialLamp(state: string): Lamp {
  switch (state) {
    case "sealed":
      return { label: t("已密封"), tone: "ok" };
    case "unreadable":
      return { label: t("需要轮换"), tone: "alert", title: t("密封的 token 解不开（设备密钥被替换或密文损坏），请重新粘贴") };
    default:
      return { label: t("未设置"), tone: "off" };
  }
}

function connectorLamp(c: api.CloudflareConnector): Lamp {
  switch (c.state) {
    case "connected":
      return { label: t("connected（{n} 条）", { n: c.ready_connections }), tone: "ok" };
    case "connecting":
      return { label: "connecting", tone: "alert" };
    case "degraded":
      return { label: t("degraded（重启 {n} 次）", { n: c.restarts }), tone: "alert", title: t("connector 反复重启：检查 token 是否有效、UDP/TCP 7844 是否被阻断") };
    case "stopped":
      return { label: "stopped", tone: "off" };
    default:
      return { label: "unknown", tone: "off", title: t("升级引擎不可达，读不到 connector 状态") };
  }
}

function originLamp(o: api.CloudflareOrigin): Lamp {
  switch (o.state) {
    case "listening":
      return { label: t("socket 已监听"), tone: "ok" };
    case "admin_gated":
      return { label: t("管理面已收窄"), tone: "alert", title: t("管理员口令仍是出厂缺省值：公网只开放 API，管理面暂不暴露") };
    default:
      return { label: t("socket 未创建"), tone: "off" };
  }
}

function toneClass(tone: Tone): string {
  return cn(tone === "ok" && "text-signal-ok", tone === "alert" && "text-signal-alert");
}

function LampBadge({ name, lamp }: { name: string; lamp: Lamp }): React.ReactElement {
  return (
    <span className="flex items-center gap-1.5 text-xs" title={lamp.title}>
      <span className="text-muted-foreground">{name}</span>
      <Badge variant={lamp.tone === "off" ? "secondary" : "outline"} className={toneClass(lamp.tone)}>
        {lamp.label}
      </Badge>
    </span>
  );
}

// ---- 小工具 ----

const EXPOSURE_LABEL: Record<api.CloudflareExposure, string> = {
  api_only: t("只开放 API"),
  api_and_admin: t("API + 管理界面"),
};

function looksLikeHostname(s: string): boolean {
  return /^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)+$/.test(s);
}

type Run = (name: string, fn: () => Promise<unknown>, after?: () => void) => void;

// ---- 说明对话框：注意事项 + 在 Cloudflare 的操作步骤 ----

function HelpDialog({
  open,
  onOpenChange,
  serviceURL,
  hostname,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  serviceURL: string;
  hostname: string;
}): React.ReactElement {
  const host = hostname === "" ? "box.example.com" : hostname;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t("Cloudflare Tunnel 说明")}</DialogTitle>
          <DialogDescription asChild>
            <div className="space-y-3 leading-relaxed">
              <p>
                {t("Tunnel 不进入设备健康前置条件：Cloudflare、DNS、token 或组件故障只让公网入口不可用，内网 IP、本地管理台、网关与手动固件升级/回退照常。公网是否真正可达还受你的 DNS、route、WAF、Access 与套餐影响，本机自连成功不等于完整验收。")}
              </p>
              <p className="text-foreground font-medium">{t("在 Cloudflare 的操作步骤（设备不调用 Cloudflare API）")}</p>
              <ol className="list-decimal space-y-1.5 pl-5">
                <li>{t("把自己的域名加入 Cloudflare 并完成 nameserver 切换，zone 状态为 Active。")}</li>
                <li>
                  {tx("Dashboard → <b>Networking → Tunnels</b> → <b>Create a tunnel</b>，connector 选 <c>cloudflared</c>。", {
                    b: (s) => <b>{s}</b>,
                    c: (s) => <code>{s}</code>,
                  })}
                </li>
                <li>
                  {tx("<b>不要执行</b>给出的安装命令；只把命令末尾以 <c>eyJ</c> 开头的 token 复制到本页「Tunnel token」。", {
                    b: (s) => <b>{s}</b>,
                    c: (s) => <code>{s}</code>,
                  })}
                </li>
                <li>
                  {tx(
                    "该 Tunnel 的 <b>Routes → Add route → Published application</b>：Hostname 填 <c>{host}</c>（推荐一层子域名），不填 path。Service URL 由你在 Cloudflare 侧填写，<b>两种填法结果完全不同</b>：",
                    { b: (s) => <b>{s}</b>, c: (s) => <code>{s}</code>, host },
                  )}
                  <ul className="mt-1 list-disc space-y-1 pl-5">
                    <li>
                      <code className="break-all">{serviceURL}</code>
                      <Button
                        type="button"
                        size="xs"
                        variant="ghost"
                        className="ml-1"
                        onClick={() => {
                          void copyText(serviceURL).then((ok) => {
                            if (ok) toast(t("Service URL 已复制"));
                            else toast.error(t("复制失败，请手动选择文本复制"));
                          });
                        }}
                      >
                        {t("复制")}
                      </Button>
                      {t("（推荐，逐字填写）——经设备可信入口：开放范围、Host 校验、HTTPS Cookie 与真实客户端 IP 审计生效；只限内网的功能不出公网。")}
                    </li>
                    <li>
                      {tx(
                        "<c>http://localhost:80</c>——绕过可信入口：开放范围<b>不生效</b>，内网能访问的每条路由都暴露在公网，审计只见 127.0.0.1，设备无法拦截；「从外网验证」会报未通过。",
                        { b: (s) => <b>{s}</b>, c: (s) => <code>{s}</code> },
                      )}
                    </li>
                  </ul>
                </li>
                <li>{t("为该 hostname 配置 HTTP → HTTPS Redirect Rule，Cache Rule 设为 bypass；用 WAF/限速时先验证 SSE、长请求与大请求体不被误拦。")}</li>
                <li>{t("回到本页填同一个 hostname，选开放范围，确认第三方数据路径后启用。")}</li>
                <li>
                  {tx(
                    "等本页显示「组件已验证、connector connected、socket 已监听」，再从盒子局域网外访问 <c>https://{host}/healthz</c>；Dashboard 应显示 Healthy。",
                    { c: (s) => <code>{s}</code>, host },
                  )}
                </li>
              </ol>
              <p>
                {tx(
                  "Quick Tunnel（*.trycloudflare.com）不支持 SSE 且无 SLA，本设备不使用它。官方文档：<a1>Create a tunnel</a1>、<a2>Published applications</a2>。",
                  {
                    a1: (s) => (
                      <a
                        className="ml-1 underline"
                        href="https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/"
                        target="_blank"
                        rel="noreferrer"
                      >
                        {s}
                      </a>
                    ),
                    a2: (s) => (
                      <a
                        className="underline"
                        href="https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/routing-to-tunnel/"
                        target="_blank"
                        rel="noreferrer"
                      >
                        {s}
                      </a>
                    ),
                  },
                )}
              </p>
            </div>
          </DialogDescription>
        </DialogHeader>
        <DialogFooter showCloseButton />
      </DialogContent>
    </Dialog>
  );
}

// ---- 设置卡片：只读展示，点「编辑」才出表单 ----

function SettingsCard({
  status,
  busy,
  run,
}: {
  status: api.CloudflareStatus;
  busy: string | null;
  run: Run;
}): React.ReactElement {
  const cfg = status.config;
  const [editing, setEditing] = useState(false);
  const [hostname, setHostname] = useState(cfg.hostname);
  const [exposure, setExposure] = useState<api.CloudflareExposure>(cfg.exposure);
  const [autoUpdate, setAutoUpdate] = useState(cfg.auto_update);
  const [token, setToken] = useState("");
  const [error, setError] = useState<string | null>(null);
  const confirm = useConfirm();
  const cred = credentialLamp(status.credential.state);
  const tokenSet = status.credential.state === "sealed";

  function cancel(): void {
    setHostname(cfg.hostname);
    setExposure(cfg.exposure);
    setAutoUpdate(cfg.auto_update);
    setToken("");
    setError(null);
    setEditing(false);
  }

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const host = hostname.trim().toLowerCase();
    if (host !== "" && !looksLikeHostname(host)) {
      setError(t("请填你 Cloudflare zone 下的完整域名，如 box.example.com，不带协议、路径或端口"));
      return;
    }
    const tok = token.trim();
    if (tok !== "" && (/\s/.test(tok) || tok.toLowerCase().includes("cloudflared"))) {
      setError(t("只粘贴 token 本身（以 eyJ 开头的一串），不要粘贴整条安装命令"));
      return;
    }
    setError(null);
    const patch: api.CloudflarePatch = { hostname: host, exposure, auto_update: autoUpdate };
    if (tok !== "") patch.token = tok;
    run("save", () =>
      api.updateCloudflareTunnel(patch).then((r) => {
        setToken("");
        setEditing(false);
        toast(tok === "" ? t("Cloudflare Tunnel 设置已保存") : t("设置已保存，token 已密封"));
        return r;
      }),
    );
  }

  return (
    <Card className="gap-3 p-5">
      <div className="flex items-center gap-2">
        <span className="font-medium">{t("设置")}</span>
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
              <Label htmlFor="cf-hostname">Public hostname</Label>
              <HelpTip label={t("Public hostname 说明")}>
                {t("Cloudflare Tunnel 的 Published application 里填的那个 hostname，推荐一层子域名。它必须属于你自己的 Cloudflare zone（设备无法代为验证），请求的 Host 须与此逐字相等。")}
              </HelpTip>
            </div>
            <Input
              id="cf-hostname"
              value={hostname}
              autoFocus
              autoComplete="off"
              spellCheck={false}
              placeholder="box.example.com"
              onChange={(e) => setHostname(e.target.value)}
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <div className="flex items-center gap-1.5">
              <Label htmlFor="cf-exposure">{t("公网开放范围")}</Label>
              <HelpTip label={t("公网开放范围说明")}>
                {t("「只开放 API」：模型、Agent、模型目录、gate 接入与其固定安装辅助路由、/healthz。「API + 管理界面」：再开放 /ui/ 与 /admin/v1/，要求管理员口令不是出厂缺省值，启用时二次确认；固件上传/安装/回退、网络设置与 API 密钥明文解封永远只限内网。")}
              </HelpTip>
            </div>
            <Select value={exposure} onValueChange={(v) => setExposure(v === "api_and_admin" ? "api_and_admin" : "api_only")}>
              <SelectTrigger id="cf-exposure" size="sm" className="w-48">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="api_only">{EXPOSURE_LABEL.api_only}</SelectItem>
                <SelectItem value="api_and_admin">{EXPOSURE_LABEL.api_and_admin}</SelectItem>
              </SelectContent>
            </Select>
            {exposure === "api_and_admin" && !status.origin.admin_gate_ok ? (
              <p className="text-signal-alert text-xs">{t("管理员口令仍是出厂缺省值：先在顶栏账户菜单改密，才能把管理面暴露到公网。")}</p>
            ) : null}
          </div>
          <div className="flex items-center gap-2">
            <Switch id="cf-auto-update" checked={autoUpdate} onCheckedChange={setAutoUpdate} />
            <Label htmlFor="cf-auto-update">{t("每天检查签名清单并自动更新 cloudflared")}</Label>
            <HelpTip label={t("自动更新说明")}>
              {t("匿名只读官网签名清单。Cloudflare 只支持距最新 release 一年内的版本；关闭后请自行留意支持窗口与安全公告。")}
            </HelpTip>
          </div>
          <div className="flex flex-col gap-1.5">
            <div className="flex items-center gap-1.5">
              <Label htmlFor="cf-token">Tunnel token</Label>
              <HelpTip label={t("Tunnel token 说明")}>
                {t("只粘贴 token 本身（eyJ 开头的一串），不要粘贴安装命令。token 用设备密钥密封入库，任何页面与接口都不回显；拿到它的人就能运行这个 Tunnel 的 connector，请按上游凭据保管。在 Cloudflare 里 Refresh token 后把新值贴回即轮换。")}
              </HelpTip>
            </div>
            <Input
              id="cf-token"
              type="password"
              value={token}
              autoComplete="off"
              spellCheck={false}
              placeholder={tokenSet ? t("已密封；留空保持不变，粘贴新 token 即轮换") : "eyJ…"}
              onChange={(e) => setToken(e.target.value)}
            />
          </div>
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <div className="flex flex-wrap gap-2">
            <Button type="submit" size="sm" disabled={busy !== null}>
              {busy === "save" ? t("保存中…") : t("保存")}
            </Button>
            <Button type="button" size="sm" variant="outline" disabled={busy !== null} onClick={cancel}>
              {t("取消")}
            </Button>
            {tokenSet && !status.enabled ? (
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="text-destructive ml-auto"
                disabled={busy !== null}
                onClick={() => {
                  void confirm({
                    title: t("清除已密封的 token"),
                    body: t("清除后需重新粘贴 token 才能启用。Cloudflare 侧的 Tunnel 不受影响，要彻底作废请到 Dashboard 里 Refresh token 或删除 Tunnel。"),
                    confirmText: t("清除"),
                    danger: true,
                  }).then((ok) => {
                    if (!ok) return;
                    run("clear-token", () =>
                      api.updateCloudflareTunnel({ clear_token: true }).then((r) => {
                        toast(t("token 已清除"));
                        return r;
                      }),
                    );
                  });
                }}
              >
                {t("清除 token")}
              </Button>
            ) : null}
          </div>
        </form>
      ) : (
        <dl className="grid grid-cols-[9rem_1fr] gap-y-1 text-xs">
          <dt className="text-muted-foreground">Public hostname</dt>
          <dd>{cfg.hostname === "" ? "—" : <code className="font-mono">{cfg.hostname}</code>}</dd>
          <dt className="text-muted-foreground">{t("公网开放范围")}</dt>
          <dd>{EXPOSURE_LABEL[cfg.exposure]}</dd>
          <dt className="text-muted-foreground">{t("自动更新 cloudflared")}</dt>
          <dd>{cfg.auto_update ? t("开") : t("关")}</dd>
          <dt className="text-muted-foreground">Tunnel token</dt>
          <dd>
            <Badge variant={cred.tone === "off" ? "secondary" : "outline"} className={toneClass(cred.tone)} title={cred.title}>
              {cred.label}
            </Badge>
          </dd>
        </dl>
      )}
    </Card>
  );
}

// ---- 启用对话框 ----

function EnableDialog({
  status,
  open,
  onClose,
  busy,
  run,
}: {
  status: api.CloudflareStatus;
  open: boolean;
  onClose: () => void;
  busy: string | null;
  run: Run;
}): React.ReactElement {
  const [accept, setAccept] = useState(false);
  const [confirmAdmin, setConfirmAdmin] = useState(false);
  const admin = status.config.exposure === "api_and_admin";
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("启用 Cloudflare Tunnel")}</DialogTitle>
          <DialogDescription>
            {t("公网地址 {url}；设备建立专用 origin socket，由 cloudflared 主动连出到你的 Cloudflare 账号。", {
              url: status.config.hostname === "" ? t("（尚未设置 hostname）") : `https://${status.config.hostname}`,
            })}
          </DialogDescription>
        </DialogHeader>
        <label className="flex items-start gap-2 text-sm">
          <Checkbox checked={accept} onCheckedChange={(v) => setAccept(v === true)} className="mt-0.5" />
          <span>
            {t("我知道公网 TLS 在 Cloudflare 边缘终止，Cloudflare 位于明文数据路径上，可能看到 URL、请求头、API 密钥、提示词、模型响应与上传内容，这不是端到端加密；我接受 cloudflared（Apache-2.0）作为第三方组件运行在设备上，其套餐、配额与服务条款由 Cloudflare 决定。")}
          </span>
        </label>
        {admin ? (
          <label className="flex items-start gap-2 text-sm">
            <Checkbox checked={confirmAdmin} onCheckedChange={(v) => setConfirmAdmin(v === true)} className="mt-0.5" />
            <span className="text-signal-alert">
              {t("我确认把管理界面与管理员 API 暴露到公网；设备口令、会话、CSRF 与 API 密钥继续生效，Cloudflare Access 只是可选的第二层保护。")}
            </span>
          </label>
        ) : null}
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            {t("取消")}
          </Button>
          <Button
            type="button"
            disabled={busy !== null || !accept || (admin && !confirmAdmin)}
            onClick={() =>
              run("enable", () =>
                api.enableCloudflareTunnel(accept, confirmAdmin).then((r) => {
                  onClose();
                  toast(t("Cloudflare Tunnel 已启用"));
                  return r;
                }),
              )
            }
          >
            {busy === "enable" ? t("启用中…") : t("启用")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---- 面板 ----

// enableBlocker 说明「启用」为什么还按不了：按启用前置条件的顺序给出第一条没满足的。
function enableBlocker(status: api.CloudflareStatus): string {
  if (status.config.hostname === "") return t("先设置 hostname");
  if (status.credential.state !== "sealed") return t("先粘贴并保存 token");
  if (!status.component.engine_available) return t("升级引擎不可达");
  return t("先到「第三方组件」页安装 cloudflared");
}

function lastErrorText(err: string): string {
  if (err === "credential_unreadable") return t("开机恢复失败：密封的 token 解不开，请重新粘贴 token 后再启用。");
  if (err === "engine_unavailable") return t("开机恢复时升级引擎不可达：origin socket 已就绪，connector 状态未确认。");
  return t("开机恢复失败：{err}", { err });
}

export function CloudflarePanel({
  status,
  reload,
}: {
  status: api.CloudflareStatus;
  reload: () => void;
}): React.ReactElement {
  const confirm = useConfirm();
  const [busy, setBusy] = useState<string | null>(null);
  const [enableOpen, setEnableOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [report, setReport] = useState<api.CloudflareTestReport | null>(status.last_test ?? null);

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

  const canEnable =
    !status.enabled &&
    status.config.hostname !== "" &&
    status.credential.state === "sealed" &&
    status.component.installed &&
    status.component.engine_available;

  return (
    <div className="flex flex-col gap-3">
      <Card className="gap-3 p-5">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <Badge variant={status.enabled ? "outline" : "secondary"} className={status.enabled ? "text-signal-ok" : ""}>
            {status.enabled ? t("已启用") : t("未启用")}
          </Badge>
          {status.external_url === undefined || status.external_url === "" ? null : (
            <code className="font-mono text-sm">{status.external_url}</code>
          )}
          <LampBadge name={t("组件")} lamp={componentLamp(status.component)} />
          <LampBadge name={t("凭据")} lamp={credentialLamp(status.credential.state)} />
          <LampBadge name="Connector" lamp={connectorLamp(status.connector)} />
          <LampBadge name="Origin" lamp={originLamp(status.origin)} />
          <Button type="button" variant="ghost" size="xs" className="ml-auto" onClick={() => setHelpOpen(true)}>
            <CircleHelp />
            {t("说明")}
          </Button>
        </div>
        {status.last_error === undefined || status.last_error === "" ? null : (
          <p className="text-signal-alert text-xs">{lastErrorText(status.last_error)}</p>
        )}
        <div className="flex flex-wrap items-center gap-2">
          {status.enabled ? (
            <Button
              size="sm"
              variant="outline"
              disabled={busy !== null}
              onClick={() => {
                void confirm({
                  title: t("停用 Cloudflare Tunnel"),
                  body: (
                    <>
                      <p>{t("停止 connector、关闭 origin socket，「API调用」和「开发工具接入」不再给出这个公网地址；已密封的 token 保留。")}</p>
                      <p>{t("Cloudflare 侧的 Tunnel、route 与 DNS 不受影响，要撤销请到 Dashboard 操作。若正经公网访问本页，停用后请改从内网地址继续。")}</p>
                    </>
                  ),
                  confirmText: t("停用（保留 token）"),
                }).then((ok) => {
                  if (!ok) return;
                  run("disable", () =>
                    api.disableCloudflareTunnel(true).then((r) => {
                      toast(t("Cloudflare Tunnel 已停用"));
                      return r;
                    }),
                  );
                });
              }}
            >
              {busy === "disable" ? t("停用中…") : t("停用")}
            </Button>
          ) : (
            <Button size="sm" disabled={busy !== null || !canEnable} onClick={() => setEnableOpen(true)}>
              {t("启用")}
            </Button>
          )}
          <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => run("test", () => api.testCloudflareTunnel(false).then((r) => setReport(r.report)))}>
            {busy === "test" ? t("检查中…") : t("本地自检")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null || !status.enabled}
            title={status.enabled ? t("设备自己对 https://<hostname>/healthz 探一次，不能替代从外网客户端的完整验证") : t("先启用")}
            onClick={() => run("test-public", () => api.testCloudflareTunnel(true).then((r) => setReport(r.report)))}
          >
            {busy === "test-public" ? t("探测中…") : t("从外网验证")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            className="text-destructive"
            disabled={busy !== null}
            onClick={() => {
              void confirm({
                title: t("删除本机 Cloudflare Tunnel 配置"),
                body: (
                  <>
                    <p>{t("销毁密封的 token 与 hostname 等设置，启用中会先停用；已安装的 cloudflared 组件保留。")}</p>
                    <p>{t("设备没有 Cloudflare 账户权限，云侧的 Tunnel、route 与 DNS 请到 Dashboard 自行删除。")}</p>
                  </>
                ),
                confirmText: t("删除"),
                danger: true,
              }).then((ok) => {
                if (!ok) return;
                run("delete", () =>
                  api.deleteCloudflareTunnel(false).then((r) => {
                    toast(t("本机 Tunnel 配置已删除"));
                    return r;
                  }),
                );
              });
            }}
          >
            {t("删除本机配置")}
          </Button>
          {status.enabled || canEnable ? null : <span className="text-muted-foreground text-xs">{t("启用前：{reason}", { reason: enableBlocker(status) })}</span>}
        </div>
        {report === null ? null : (
          <div className="flex flex-col gap-1 text-xs">
            <p className="text-muted-foreground">
              {t("最近一次{kind}（{at}）：{result}", {
                kind: report.public ? t("自检 + 公网探测") : t("自检"),
                at: fmtTime(report.at),
                result: report.ok ? t("通过") : t("未通过"),
              })}
            </p>
            <ul className="space-y-0.5">
              {report.checks.map((c) => (
                <li key={c.layer} className="flex items-baseline gap-2">
                  <span className={cn("inline-block size-1.5 shrink-0 translate-y-[-1px] rounded-full", c.ok ? "bg-signal-ok" : "bg-signal-alert")} />
                  <span className="text-muted-foreground w-16 shrink-0">
                    {c.layer === "component" ? t("组件") : c.layer === "connector" ? "Connector" : c.layer === "origin" ? "Origin" : t("公网")}
                  </span>
                  <span>{c.detail}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </Card>

      <SettingsCard
        key={`${status.config.hostname}|${status.config.exposure}|${String(status.config.auto_update)}|${status.credential.state}`}
        status={status}
        busy={busy}
        run={run}
      />

      <ComponentPrereqCard spec={CLOUDFLARED_SPEC} comp={status.component} />

      <HelpDialog open={helpOpen} onOpenChange={setHelpOpen} serviceURL={status.service_url} hostname={status.config.hostname} />
      <EnableDialog key={String(enableOpen)} status={status} open={enableOpen} onClose={() => setEnableOpen(false)} busy={busy} run={run} />
    </div>
  );
}
