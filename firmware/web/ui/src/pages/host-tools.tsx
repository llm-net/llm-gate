// 智能体 · 工具配置：一台工作节点 / 模型服务节点上的工具——守护进程（工作节点 devd、模型服务节点
// modeld）、git、tmux、创作工具、开发工具引导器 gate——的状态与安装 / 卸载。主机 id 在查询串
// ?host=<id>，从「主机/SoC」表那一行的「工具配置」进入；受控纳管的主机没有这一页（设备答 409）。
//
// 五张卡各一行读数 + 动作：
//   - devd / modeld：读数在主机行里（安装 / 检查 / 卸载走 /devd 端点，装哪一种由主机类型决定）。
//   - git / tmux：每次进页经 SSH 探一次；用主机上的包管理器装，要 root（同 devd 的 sudo 口令规则）。
//     tmux 是工作空间终端持久会话的前提，装完设备顺手刷新主机行里 devd 的 tmux 读数。
//   - 创作工具：FFmpeg、CJK 字体与可选 ImageMagick，同一次探测；安装共用主机队列与 sudo 规则。
//   - gate：同样现探；安装 / 升级以 SSH 用户身份跑设备自己的安装脚本，主机要有 curl 并能按选定
//     的地址访问设备——地址只能从设备自己公布的三类接入地址里选（内网 IP / 内网域名 / 公网地址，
//     与「使用指南」同一份推导 features/access/access.tsx），不能手填；首次安装须选一把密钥（设备解封明文经 SSH 交给主机，会短暂出现在主机上
//     gate 的 argv 里——对话框里说明）。按下「安装 gate」只是把安装排进这台主机的动作队列（202）：
//     对话框立刻关闭，gate 卡片下出现这条动作的进度（排队 / 连接 / 探测 / 安装中的脉动进度条与
//     主机最近一行输出 / 结果），与开发工具的动作同一条队列、同一套陪等——切走页面照跑、回来
//     照样显示；失败的记录带原因与「重试」（重新打开安装对话框）。已装之后「升级 gate」跑 gate update，不动地址与密钥；
//     「设置 gate」重新选地址与密钥（gate bootstrap，验证通过才保存），不重装。
//     卸载跑 gate uninstall --yes。gate 卡的下半部分是 gate 管的六个开发工具，每个一行：名称、
//     状态徽标（gate 安装 / 外部安装 / PATH 中检测到 / 未安装）、版本与路径，动作是「安装」（PATH
//     上已有同名程序时叫「关联」；要 gate 已装且有密钥）、「升级」（只对 gate 受管的安装）与「⋯」里的
//     「解除关联」「卸载」（卸载只对受管安装）——全部是主机上 gate 自己的生命周期子命令，经 SSH 跑，
//     不经手密钥；安装物由 gate 凭已保存的 Key 从设备取得，可能要等几分钟。
//
// 开发工具的动作不在请求里同步等完：它们排进设备上这台主机的队列（POST …/tools/dev/{tool}/{action}
// 与批量的 …/tools/jobs 都答 202），由设备后台逐个执行；页面用 GET …/tools/jobs/wait 陪等
// （revision 变化或 30 秒回一帧），每帧带着队列（排队 / 执行中 / 最近结束）与最近一次动作结束时
// 复核到的整份读数。离开页面只是不再陪等，队列照跑；回来先读一次快照再接着陪等——所以升级不会
// 被切页打断，也能恢复显示。每行的动作按钮全部平铺（安装 / 关联、升级、解除关联、卸载），
// 「安装 / 升级全部」把该装的装、受管的升级，一次排进队列。
//
// 零轮询：读数只在进页 / 刷新 / 动作之后重取一次；队列陪等是与媒体生成同类的人发起陪等例外。

import { ArrowLeftIcon, CheckIcon, Loader2Icon, XIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue } from "@/components/ui/select";
import { type AccessTarget, accessTargets, defaultTarget } from "@/features/access/access";
import { DevdDialog, type DevdMode, type Tone, devdBadge, needsSudoPassword, toneClass } from "@/features/agent-hosts/devd";
import * as api from "@/lib/api";
import { useConfirm } from "@/lib/confirm";
import { fmtTime, fmtTimeSeconds } from "@/lib/format";
import { t } from "@/lib/i18n";
import { navigate, useLocationSearch } from "@/lib/router";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

type Run = (name: string, fn: () => Promise<unknown>) => void;

/** 第二行的一项读数。 */
function Fact({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <span className="flex items-center gap-1">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </span>
  );
}

function ToolCard({
  name,
  badge,
  actions,
  children,
}: {
  name: string;
  badge: { label: string; tone: Tone };
  actions: React.ReactNode;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <Card className="gap-1 px-3 py-2">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
        <span className="text-sm font-medium">{name}</span>
        <Badge variant={badge.tone === "none" ? "secondary" : "outline"} className={badge.tone === "none" ? "" : toneClass(badge.tone)}>
          {badge.label}
        </Badge>
        <div className="ml-auto flex flex-wrap gap-1.5">{actions}</div>
      </div>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">{children}</div>
    </Card>
  );
}

/** 经包管理器安装的程序（git / tmux）：读数、包管理器与「安装」。文案由调用方给（t() 只收字面量）。 */
function PackageCard({
  name,
  reading,
  packageManager,
  busy,
  installLabel,
  manualHint,
  missingHint,
  onInstall,
}: {
  name: api.HostPackage;
  reading: api.HostToolPackage;
  packageManager: string | undefined;
  busy: string | null;
  installLabel: string;
  manualHint: string;
  missingHint?: string;
  onInstall: () => void;
}): React.ReactElement {
  return (
    <ToolCard
      name={name}
      badge={reading.installed ? { label: t("已安装"), tone: "ok" } : { label: t("未安装"), tone: "none" }}
      actions={
        reading.installed ? null : (
          <Button size="sm" variant="outline" disabled={busy !== null || packageManager === undefined} onClick={onInstall}>
            {busy === name ? <Loader2Icon className="animate-spin" /> : null}
            {installLabel}
          </Button>
        )
      }
    >
      <Fact label={t("版本")}>
        <code className="font-mono">{reading.version === undefined || reading.version === "" ? "—" : reading.version}</code>
      </Fact>
      <Fact label={t("包管理器")}>
        <code className="font-mono">{packageManager ?? "—"}</code>
      </Fact>
      {reading.installed || missingHint === undefined ? null : <span className="text-muted-foreground basis-full">{missingHint}</span>}
      {reading.installed || packageManager !== undefined ? null : (
        <span className="text-muted-foreground basis-full">{manualHint}</span>
      )}
    </ToolCard>
  );
}

// ---- 只要一个 sudo 口令的动作（装 git / tmux / 创作工具）----

function SudoDialog({
  title,
  host,
  action,
  onClose,
  onDone,
}: {
  title: string;
  host: api.AgentHost;
  action: (password: string) => Promise<unknown>;
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault();
            setBusy(true);
            setError(null);
            action(password).then(
              () => {
                onDone();
                onClose();
              },
              (err: unknown) => {
                setBusy(false);
                setError(api.errorMessage(err));
              },
            );
          }}
        >
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
          </DialogHeader>
          <code className="text-muted-foreground font-mono text-xs">{`${host.username}@${host.address}:${host.port}`}</code>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="tool-sudo-password">{t("sudo 口令")}</Label>
            <Input id="tool-sudo-password" type="password" value={password} required disabled={busy} autoComplete="new-password" onChange={(e) => setPassword(e.target.value)} />
          </div>
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {busy ? t("连接中…") : t("连接并安装")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 主机访问本设备的地址：只能从设备公布的接入地址里选 ----

/** 一条可选的设备地址：group 是所属接入路径的显示名，tag 是同一路径内多行时的行前标签。 */
interface AddressOption {
  kind: AccessTarget["kind"] | "saved";
  group: string;
  tag: string;
  base: string;
  hint: string;
}

// addressOptions 把「使用指南」那三条接入路径的可用基址摊平成下拉项：内网 IP 每块网卡一条，
// 内网域名是官网签发的 / 浏览器此刻用的，公网地址是外网映射或 Tunnel。主机上 gate 已保存的
// 地址不在其中时也列进去（设置时不悄悄改道），但标明来源。
function addressOptions(ep: api.DeviceEndpoints, saved: string): AddressOption[] {
  const targets = accessTargets(ep);
  const out: AddressOption[] = [];
  for (const target of targets) {
    for (const line of target.lines) {
      out.push({ kind: target.kind, group: target.label, tag: line.tag, base: line.base, hint: line.hint });
    }
  }
  if (saved !== "" && !out.some((o) => o.base === saved)) {
    out.push({ kind: "saved", group: t("主机上已保存"), tag: t("当前配置"), base: saved, hint: t("主机上 gate 现在用的地址；不在设备公布的接入地址里") });
  }
  return out;
}

// defaultAddress 缺省选主机上已保存的地址（设置时不改道）；首次安装按「使用指南」的缺省规则选内网 IP。
function defaultAddress(options: AddressOption[], ep: api.DeviceEndpoints, saved: string): string {
  if (saved !== "" && options.some((o) => o.base === saved)) return saved;
  const target = defaultTarget(accessTargets(ep));
  const first = options.find((o) => o.kind === target.kind);
  return (first ?? options[0])?.base ?? "";
}

function addressWarning(option: AddressOption | undefined): string | null {
  if (option === undefined) return null;
  switch (option.kind) {
    case "lan_ip":
      return t("纯 IP 明文 HTTP：没有 TLS 保护，只适合离线内网；gate 启动时也会提示这一点。");
    case "external":
      return t("主机会经公网入口访问设备：该入口须开放 /gate-helper/，且主机要能连到它。");
    default:
      return null;
  }
}

// ---- 安装 gate / 设置 gate：选地址与 API 密钥 ----

/** install = 首次安装（跑设备的安装脚本）；config = 已装之后重新选地址与密钥（gate bootstrap）。 */
type GateDialogMode = "install" | "config";

function GateDialog({
  mode,
  tools,
  onClose,
  onDone,
  onQueued,
}: {
  mode: GateDialogMode;
  tools: api.HostTools;
  onClose: () => void;
  /** 设置 gate 完成：整份新读数。 */
  onDone: (next: api.HostTools) => void;
  /** 安装 gate 已排进队列：那一帧队列快照（对话框随即关闭，进度在 gate 卡片下）。 */
  onQueued: (frame: api.HostDevToolJobs) => void;
}): React.ReactElement {
  const saved = tools.gate.base_url ?? "";
  const [options, setOptions] = useState<AddressOption[] | null>(null);
  const [base, setBase] = useState("");
  const [keys, setKeys] = useState<api.ApiKey[] | null>(null);
  const [keyID, setKeyID] = useState<string>(tools.gate.configured ? "keep" : "");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const needKey = !tools.gate.configured;

  useEffect(() => {
    api.listKeys().then(
      (r) => setKeys(r.keys.filter((k) => k.plaintext_available && !k.disabled)),
      (err: unknown) => setError(api.errorMessage(err)),
    );
    api.getEndpoints().then(
      (snap) => {
        const opts = addressOptions(snap.endpoints, saved);
        setOptions(opts);
        setBase(defaultAddress(opts, snap.endpoints, saved));
      },
      (err: unknown) => setError(api.errorMessage(err)),
    );
  }, [saved]);
  const chosen = options?.find((o) => o.base === base);
  const warning = addressWarning(chosen);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    if (base === "") {
      setError(t("请选一个主机访问本设备的地址"));
      return;
    }
    if (keyID === "") {
      setError(t("请选一把密钥"));
      return;
    }
    setBusy(true);
    setError(null);
    const input = { base_url: base, ...(keyID === "keep" ? {} : { key_id: Number(keyID) }) };
    const failed = (err: unknown): void => {
      setBusy(false);
      setError(api.errorMessage(err));
    };
    if (mode === "install") {
      // 只是入队：设备答 202 就关对话框，之后的进度在 gate 卡片下、由页面陪等。
      api.installHostGate(tools.host.id, input).then(
        (frame) => {
          toast(t("安装 gate 已排进队列，进度显示在 gate 卡片下"));
          onQueued(frame);
          onClose();
        },
        failed,
      );
      return;
    }
    api.configureHostGate(tools.host.id, input).then(
      (next) => {
        toast(t("gate 设置已保存"));
        onDone(next);
        onClose();
      },
      failed,
    );
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{mode === "install" ? t("安装 gate") : t("设置 gate")}</DialogTitle>
          </DialogHeader>
          <code className="text-muted-foreground font-mono text-xs">{`${tools.host.username}@${tools.host.address}:${tools.host.port}`}</code>
          {mode === "config" ? (
            <p className="text-muted-foreground text-xs">
              {t("gate 先用新地址与密钥访问设备，验证通过才保存，失败保留原设置。主机上 gate 之后启动的开发工具都用这组设置，用量记在所选密钥名下；正在运行的工具不受影响。")}
            </p>
          ) : null}
          <div className="flex flex-col gap-1.5">
            <Label>{t("主机访问本设备的地址")}</Label>
            <Select value={base} disabled={busy || options === null || options.length === 0} onValueChange={setBase}>
              <SelectTrigger className="w-full font-mono">
                <SelectValue placeholder={t("未选择")} />
              </SelectTrigger>
              <SelectContent>
                {["lan_ip", "lan_domain", "external", "saved"].map((kind) => {
                  const group = (options ?? []).filter((o) => o.kind === kind);
                  const head = group[0];
                  if (head === undefined) return null;
                  return (
                    <SelectGroup key={kind}>
                      <SelectLabel>{head.group}</SelectLabel>
                      {group.map((o) => (
                        <SelectItem key={o.base} value={o.base}>
                          <span className="font-mono">{o.base}</span>
                          <span className="text-muted-foreground ml-2 text-xs">{o.tag}</span>
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  );
                })}
              </SelectContent>
            </Select>
            <p className="text-muted-foreground text-xs">
              {chosen !== undefined
                ? chosen.hint
                : mode === "install"
                  ? t("主机上的 curl 会从这个地址取安装脚本与压缩包，gate 之后也用它访问设备。")
                  : t("gate 之后用这个地址访问设备、升级自己。")}
            </p>
            {warning === null ? null : <p className="text-signal-alert text-xs">{warning}</p>}
            {options !== null && options.length === 0 ? (
              <p className="text-signal-alert text-xs">{t("设备没能给出任何接入地址：请先在「网络/域名/代理」页配置，或检查设备的网卡读数。")}</p>
            ) : null}
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>{t("API密钥")}</Label>
            <Select value={keyID} disabled={busy || keys === null} onValueChange={setKeyID}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder={t("未选择")} />
              </SelectTrigger>
              <SelectContent>
                {needKey ? null : <SelectItem value="keep">{t("沿用主机上已保存的密钥")}</SelectItem>}
                {(keys ?? []).map((k) => (
                  <SelectItem key={k.id} value={String(k.id)}>
                    {k.label === "" ? `${k.display_prefix}…${k.display_last4}` : `${k.label} · ${k.display_prefix}…${k.display_last4}`}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-muted-foreground text-xs">
              {t("密钥明文由设备解封后经 SSH 交给主机、写进主机上 gate 的配置；写入过程中会短暂出现在主机上 gate 的命令行参数里。")}
            </p>
          </div>
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {mode === "install" ? (busy ? t("排队中…") : t("安装 gate")) : busy ? t("保存中…") : t("保存设置")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- gate 管的开发工具：每个一行 + 动作队列 ----

function devToolLabel(name: api.HostDevToolJob["tool"]): string {
  if (name === "studio/ffmpeg") return "FFmpeg";
  if (name === "studio/fonts-cjk") return t("CJK 字体");
  if (name === "studio/imagemagick") return "ImageMagick";
  return api.DEV_TOOLS.find((d) => d.id === name)?.label ?? name;
}

function devToolBadge(dev: api.HostDevTool): { label: string; tone: Tone } {
  if (dev.linked) return dev.origin === "external" ? { label: t("外部安装"), tone: "ok" } : { label: t("gate 安装"), tone: "ok" };
  if (dev.found !== undefined && dev.found !== "") return { label: t("PATH 中检测到"), tone: "warn" };
  return { label: t("未安装"), tone: "none" };
}

function actionLabel(action: api.HostDevToolAction, dev: api.HostDevTool | undefined): string {
  switch (action) {
    case "install":
      return dev !== undefined && !dev.linked && dev.found !== undefined && dev.found !== "" ? t("关联") : t("安装");
    case "update":
      return t("升级");
    case "disconnect":
      return t("解除关联");
    case "uninstall":
      return t("卸载");
  }
}

function stageLabel(job: api.HostDevToolJob): string {
  switch (job.stage) {
    case "connecting":
      return t("连接主机…");
    case "probing":
      if (job.tool.startsWith("studio/")) return t("探测主机上的创作工具…");
      return job.tool === api.GATE_JOB_TOOL ? t("探测主机上的 curl 与已有的 gate…") : t("探测主机上的 gate…");
    case "verifying":
      return t("复核读数…");
    default:
      if (job.tool.startsWith("studio/")) return t("主机正在通过包管理器安装，可能要几分钟…");
      if (job.tool === api.GATE_JOB_TOOL) return t("主机正在从设备取安装脚本与压缩包、校验并安装 gate，随后验证地址与密钥…");
      return job.action === "install" || job.action === "update" ? t("gate 正在取官方安装物并安装（先官方源，不可达再经设备），可能要几分钟…") : t("gate 执行中…");
  }
}

function isActive(job: api.HostDevToolJob): boolean {
  return job.status === "queued" || job.status === "running";
}

/** 已用时长（按帧到达时刻算，不起定时器：每条进度行都会带来一帧）。 */
function elapsed(job: api.HostDevToolJob): string {
  if (job.started_at === undefined) return "";
  const s = Math.max(0, Math.round((Date.now() - Date.parse(job.started_at)) / 1000));
  return s < 60 ? t("{n} 秒", { n: s }) : t("{m} 分 {s} 秒", { m: Math.floor(s / 60), s: s % 60 });
}

/** 一条进度条：percent 缺席即不定长（脉动）。 */
function ProgressBar({ percent, label }: { percent: number | undefined; label: string }): React.ReactElement {
  return (
    <div role="progressbar" aria-label={label} aria-valuemin={0} aria-valuemax={100} {...(percent === undefined ? {} : { "aria-valuenow": Math.min(100, percent) })}
      className="bg-muted h-1.5 w-full overflow-hidden rounded-full">
      {percent === undefined ? (
        <div className="bg-primary/60 h-full w-full animate-pulse" />
      ) : (
        <div className="bg-primary h-full transition-[width] duration-500" style={{ width: `${Math.min(100, percent)}%` }} />
      )}
    </div>
  );
}

/** 一行工具下面的队列状态：排队 / 执行中（进度条）/ 成功 / 失败 / 已取消。 */
function JobStatus({
  job,
  position,
  dev,
  onCancel,
  onRetry,
}: {
  job: api.HostDevToolJob;
  /** 排队中的位次（1 起）。 */
  position: number;
  dev: api.HostDevTool | undefined;
  onCancel: () => void;
  onRetry: () => void;
}): React.ReactElement {
  const label = `${devToolLabel(job.tool)} · ${actionLabel(job.action, dev)}`;
  const dismiss = (
    <Button size="icon-xs" variant="ghost" className="ml-auto shrink-0" aria-label={isActive(job) ? t("取消") : t("清除")} title={isActive(job) ? t("取消") : t("清除")} onClick={onCancel}>
      <XIcon />
    </Button>
  );
  switch (job.status) {
    case "queued":
      return (
        <div className="text-muted-foreground flex w-full items-center gap-2 text-xs">
          <Badge variant="secondary">{t("排队中 · 第 {n} 位", { n: position })}</Badge>
          <span>{t("等待前面的动作完成后自动开始")}</span>
          {dismiss}
        </div>
      );
    case "running":
      return (
        <div className="flex w-full flex-col gap-1 text-xs">
          <div className="flex items-center gap-2">
            <Loader2Icon className="text-signal-ok size-3.5 shrink-0 animate-spin" />
            <span className="font-medium">{actionLabel(job.action, dev)}</span>
            <span className="text-muted-foreground">{stageLabel(job)}</span>
            {job.percent === undefined ? null : <span className="tabular-nums">{job.percent}%</span>}
            <span className="text-muted-foreground ml-auto tabular-nums">{elapsed(job)}</span>
            {dismiss}
          </div>
          <ProgressBar percent={job.percent} label={label} />
          {job.line === undefined || job.line === "" ? null : (
            <code className="text-muted-foreground truncate font-mono text-[11px]" title={job.output}>{job.line}</code>
          )}
        </div>
      );
    case "succeeded":
      return (
        <div className="flex w-full items-center gap-2 text-xs">
          <CheckIcon className="text-signal-ok size-3.5 shrink-0" />
          <span className="text-signal-ok">{t("{action}完成", { action: actionLabel(job.action, dev) })}</span>
          {job.finished_at === undefined ? null : <span className="text-muted-foreground">{fmtTimeSeconds(job.finished_at)}</span>}
          {job.output === undefined || job.output === "" ? null : (
            <code className="text-muted-foreground min-w-0 flex-1 truncate font-mono text-[11px]" title={job.output}>{job.output.split("\n").pop()}</code>
          )}
          {dismiss}
        </div>
      );
    case "cancelled":
      return (
        <div className="text-muted-foreground flex w-full items-center gap-2 text-xs">
          <span>{t("{action}已取消", { action: actionLabel(job.action, dev) })}</span>
          <Button size="xs" variant="ghost" onClick={onRetry}>{t("重试")}</Button>
          {dismiss}
        </div>
      );
    default:
      return (
        <div className="flex w-full flex-col gap-0.5 text-xs">
          <div className="flex items-center gap-2">
            <span className="text-signal-alert">{t("{action}失败", { action: actionLabel(job.action, dev) })}</span>
            {job.finished_at === undefined ? null : <span className="text-muted-foreground">{fmtTimeSeconds(job.finished_at)}</span>}
            <Button size="xs" variant="ghost" onClick={onRetry}>{t("重试")}</Button>
            {dismiss}
          </div>
          {job.error === undefined || job.error === "" ? null : <p className="text-signal-alert whitespace-pre-wrap">{job.error}</p>}
        </div>
      );
  }
}

/** 「安装 / 升级全部」要排的动作：没关联的装（PATH 上有就是关联），受管安装升级，外部安装不动。 */
function batchPlan(tools: api.HostTools): { tool: api.DevTool; action: api.HostDevToolAction }[] {
  const out: { tool: api.DevTool; action: api.HostDevToolAction }[] = [];
  for (const dev of tools.dev_tools) {
    if (!dev.linked) out.push({ tool: dev.name, action: "install" });
    else if (dev.origin !== "external") out.push({ tool: dev.name, action: "update" });
  }
  return out;
}

function DevToolsSection({
  tools,
  jobs,
  busy,
  onEnqueue,
  onCancel,
  onClear,
}: {
  tools: api.HostTools;
  jobs: api.HostDevToolJob[];
  busy: string | null;
  onEnqueue: (specs: { tool: api.DevTool; action: api.HostDevToolAction }[]) => void;
  onCancel: (job: api.HostDevToolJob) => void;
  onClear: () => void;
}): React.ReactElement {
  const confirm = useConfirm();
  const gateReady = tools.gate.installed && tools.gate.configured;
  const devJobs = jobs.filter((j) => api.DEV_TOOLS.some((dev) => dev.id === j.tool));
  const active = devJobs.filter(isActive);
  const done = devJobs.filter((j) => !isActive(j));
  const queued = jobs.filter((j) => j.status === "queued");
  const plan = batchPlan(tools).filter((p) => !active.some((j) => j.tool === p.tool && j.action === p.action));
  // 整体进度：这一轮（历史清掉之前）已结束的 / 全部。
  const total = devJobs.length;
  const finished = done.length;
  const lockedAll = busy !== null || !gateReady;
  const act = (dev: api.HostDevTool, action: api.HostDevToolAction): void => onEnqueue([{ tool: dev.name, action }]);
  return (
    <div className="mt-1 flex w-full min-w-0 basis-full flex-col">
      <div className="mb-1 flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className="text-muted-foreground text-xs">{t("gate 管理的开发工具")}</span>
        {active.length === 0 ? null : (
          <div className="text-muted-foreground flex min-w-32 flex-1 items-center gap-2 text-xs">
            <span className="whitespace-nowrap tabular-nums">{t("队列 {done} / {total}", { done: finished, total })}</span>
            <ProgressBar percent={total === 0 ? 0 : Math.round((finished * 100) / total)} label={t("队列进度")} />
          </div>
        )}
        <div className="ml-auto flex flex-wrap items-center gap-1.5">
          {done.length === 0 ? null : (
            <Button size="xs" variant="ghost" onClick={onClear}>{t("清除记录")}</Button>
          )}
          <Button size="xs" variant="outline" disabled={lockedAll || plan.length === 0}
            title={!gateReady ? t("要先在这台主机上安装 gate 并配置密钥。") : plan.length === 0 ? t("没有要安装或升级的工具") : undefined}
            onClick={() => onEnqueue(plan)}>
            {t("安装 / 升级全部")}
            {plan.length === 0 ? null : <span className="text-muted-foreground tabular-nums">({plan.length})</span>}
          </Button>
        </div>
      </div>
      <div className="min-w-0 divide-y overflow-hidden rounded-md border">
        {tools.dev_tools.map((dev) => {
          const badge = devToolBadge(dev);
          const label = devToolLabel(dev.name);
          const managed = dev.linked && dev.origin !== "external";
          const mine = jobs.filter((j) => j.tool === dev.name);
          const current = mine.find(isActive);
          // 没有进行中的就显示最近一条结果（清除之前一直留着）。
          const shown = current ?? mine[mine.length - 1];
          const rowLocked = lockedAll || current !== undefined;
          return (
            <div key={dev.name} className="flex min-w-0 flex-col gap-1 px-2.5 py-1.5">
              <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                <span className="w-24 shrink-0 text-sm font-medium">{label}</span>
                <Badge variant={badge.tone === "none" ? "secondary" : "outline"} className={badge.tone === "none" ? "" : toneClass(badge.tone)}>
                  {badge.label}
                </Badge>
                {dev.version === undefined || dev.version === "" ? null : <code className="text-muted-foreground font-mono text-xs">{dev.version}</code>}
                {dev.linked ? (
                  <code className="text-muted-foreground min-w-0 flex-1 basis-24 truncate font-mono text-xs" title={dev.path}>{dev.path}</code>
                ) : dev.found === undefined || dev.found === "" ? null : (
                  <code className="text-muted-foreground min-w-0 flex-1 basis-24 truncate font-mono text-xs" title={dev.found}>{dev.found}</code>
                )}
                <div className="ml-auto flex flex-wrap items-center gap-1.5">
                  {dev.linked ? null : (
                    <Button size="xs" variant="outline" disabled={rowLocked}
                      title={gateReady ? undefined : t("要先在这台主机上安装 gate 并配置密钥。")}
                      onClick={() => act(dev, "install")}>
                      {actionLabel("install", dev)}
                    </Button>
                  )}
                  {managed ? (
                    <Button size="xs" variant="outline" disabled={rowLocked} onClick={() => act(dev, "update")}>
                      {t("升级")}
                    </Button>
                  ) : null}
                  {dev.linked ? (
                    <Button size="xs" variant="outline" disabled={busy !== null || current !== undefined}
                      onClick={() => {
                        void confirm({
                          title: t("解除 {tool} 与 gate 的关联？", { tool: label }),
                          body: <p>{t("撤销 gate 写给它的接入配置与密钥副本；程序、会话与使用者原配置保留。")}</p>,
                          confirmText: t("解除关联"),
                        }).then((ok) => { if (ok) act(dev, "disconnect"); });
                      }}>
                      {t("解除关联")}
                    </Button>
                  ) : null}
                  {managed ? (
                    <Button size="xs" variant="outline" className="text-destructive hover:text-destructive" disabled={busy !== null || current !== undefined}
                      onClick={() => {
                        void confirm({
                          title: t("卸载 {tool}？", { tool: label }),
                          body: <p>{t("删除 gate 在这台主机上安装的 {tool} 程序并撤销接入；会话与偏好保留。", { tool: label })}</p>,
                          confirmText: t("卸载"),
                          danger: true,
                        }).then((ok) => { if (ok) act(dev, "uninstall"); });
                      }}>
                      {t("卸载")}
                    </Button>
                  ) : null}
                </div>
              </div>
              {shown === undefined ? null : (
                <JobStatus
                  job={shown}
                  position={queued.findIndex((j) => j.id === shown.id) + 1}
                  dev={dev}
                  onCancel={() => onCancel(shown)}
                  onRetry={() => onEnqueue([{ tool: dev.name, action: shown.action }])}
                />
              )}
            </div>
          );
        })}
      </div>
      {gateReady ? null : (
        <span className="text-muted-foreground mt-1 text-xs">{t("安装或升级开发工具由主机上的 gate 经设备取得官方安装物：要先安装 gate 并配置密钥。")}</span>
      )}
    </div>
  );
}

/** Studio programs are rows in one card, and share the host's action queue. */
function StudioToolsCard({ tools, jobs, busy, onInstall, onCancel, onClear }: {
  tools: api.HostTools;
  jobs: api.HostDevToolJob[];
  busy: string | null;
  onInstall: (names: api.StudioTool[], batch?: boolean) => void;
  onCancel: (job: api.HostDevToolJob) => void;
  onClear: () => void;
}): React.ReactElement {
  const st = tools.studio;
  const ffReady = st.ffmpeg.installed && st.ffmpeg.ffprobe && st.ffmpeg.h264 && st.ffmpeg.aac && st.ffmpeg.subtitles;
  const mine = jobs.filter((j) => j.tool.startsWith("studio/"));
  const active = mine.filter(isActive);
  const queued = jobs.filter((j) => j.status === "queued");
  // 整体进度：这一轮（历史清掉之前）已结束的 / 全部，与开发工具清单同一口径。
  const done = mine.length - active.length;
  const partial = st.ffmpeg.installed || st.ffmpeg.ffprobe || st.fonts_cjk.count > 0;
  const badge: { label: string; tone: Tone } = st.ready ? { label: t("就绪"), tone: "ok" }
    : partial ? { label: t("部分就绪"), tone: "warn" } : { label: t("未就绪"), tone: "none" };
  const rows: { name: api.StudioTool; label: string; installed: boolean; ready: boolean; version: string | undefined; required: boolean }[] = [
    { name: "ffmpeg", label: "FFmpeg", installed: st.ffmpeg.installed, ready: ffReady, version: st.ffmpeg.version, required: true },
    { name: "fonts-cjk", label: t("CJK 字体"), installed: st.fonts_cjk.count > 0, ready: st.fonts_cjk.count > 0, version: st.fonts_cjk.family, required: true },
    { name: "imagemagick", label: "ImageMagick", installed: st.imagemagick.installed, ready: st.imagemagick.installed, version: st.imagemagick.version, required: false },
  ];
  // Only absent required programs belong in install-all. Missing codecs remain visible for repair.
  const plan = rows.filter((row) => row.required && !row.installed && !active.some((j) => j.tool === `studio/${row.name}`)).map((row) => row.name);
  const locked = busy !== null || !tools.package_manager;
  return (
    <ToolCard name={t("创作工具")} badge={badge} actions={
      <>
        {done === 0 ? null : <Button size="sm" variant="ghost" onClick={onClear}>{t("清除记录")}</Button>}
        <Button size="sm" variant="outline" disabled={locked || plan.length === 0} onClick={() => onInstall(plan, true)}>
          {t("安装全部")} <span className="text-muted-foreground tabular-nums">({plan.length})</span>
        </Button>
      </>
    }>
      <p className="text-muted-foreground basis-full">{t("在这台工作节点上处理创作工作空间的音视频、字幕与图像。")}</p>
      <Fact label={t("包管理器")}><code>{tools.package_manager ?? "—"}</code></Fact>
      {!tools.package_manager ? <p className="text-signal-alert basis-full">{t("主机上没有认得的包管理器，请手工安装创作工具。")}</p> : null}
      {st.ffmpeg.subtitles && st.fonts_cjk.count > 0 ? null : <p className="text-signal-alert basis-full">{t("缺少 subtitles 滤镜或 CJK 字体，字幕烧录不可用。")}</p>}
      {st.ffmpeg.installed && !ffReady ? <p className="text-signal-alert basis-full">{t("FFmpeg 能力不完整；安装后仍缺失时，请检查发行版软件仓库与 FFmpeg 构建。")}</p> : null}
      {active.length === 0 ? null : (
        <div className="text-muted-foreground flex w-full items-center gap-2">
          <span className="whitespace-nowrap tabular-nums">{t("队列 {done} / {total}", { done, total: mine.length })}</span>
          <ProgressBar percent={Math.round(done * 100 / mine.length)} label={t("队列进度")} />
        </div>
      )}
      <div className="mt-1 w-full min-w-0 divide-y overflow-hidden rounded-md border">
        {rows.map((row) => {
          const rowJobs = mine.filter((j) => j.tool === `studio/${row.name}`);
          const current = rowJobs.find(isActive);
          const shown = current ?? rowJobs[rowJobs.length - 1];
          return <div key={row.name} className="flex min-w-0 flex-col gap-1 px-2.5 py-1.5">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
              <span className="w-24 shrink-0 text-sm font-medium">{row.label}</span>
              <Badge variant="outline" className={toneClass(row.ready ? "ok" : row.installed ? "warn" : "none")}>{row.ready ? t("就绪") : row.installed ? t("部分就绪") : t("未安装")}</Badge>
              <span className="text-muted-foreground">{row.required ? t("必备") : t("可选")}</span>
              {row.version ? <code className="text-muted-foreground min-w-0 break-words font-mono">{row.version}</code> : null}
              {row.name === "fonts-cjk" ? <span>{t("{n} 个字体族", { n: st.fonts_cjk.count })}</span> : null}
              <Button className="ml-auto" size="xs" variant="outline" disabled={locked || current !== undefined} onClick={() => onInstall([row.name])}>{t("安装")}</Button>
            </div>
            {row.name === "ffmpeg" ? <div className="flex flex-wrap gap-1">
              {([{ label: "ffprobe", ok: st.ffmpeg.ffprobe }, { label: "H.264", ok: st.ffmpeg.h264 }, { label: "AAC", ok: st.ffmpeg.aac }, { label: "subtitles", ok: st.ffmpeg.subtitles }]).map((cap) => (
                <Badge key={cap.label} variant="outline" className={toneClass(cap.ok ? "ok" : "warn")}>{cap.label} · {cap.ok ? t("可用") : t("缺失")}</Badge>
              ))}
            </div> : null}
            {shown === undefined ? null : <JobStatus job={shown} position={queued.findIndex((j) => j.id === shown.id) + 1} dev={undefined}
              onCancel={() => onCancel(shown)} onRetry={() => onInstall([row.name])} />}
          </div>;
        })}
      </div>
    </ToolCard>
  );
}

// ---- 页面 ----

function versionBadge(installed: boolean, version: string | undefined, embedded: string | undefined): { label: string; tone: Tone } {
  if (!installed) return { label: t("未安装"), tone: "none" };
  if (embedded !== undefined && embedded !== "" && version !== undefined && version !== "" && version !== embedded) {
    return { label: t("版本不一致"), tone: "warn" };
  }
  return { label: t("已安装"), tone: "ok" };
}

function ToolsBoard({
  tools,
  jobs,
  busy,
  run,
  replace,
  onDevd,
  onPackage,
  onStudioInstall,
  onGateInstall,
  onGateConfig,
  onEnqueue,
  onCancelJob,
  onClearJobs,
}: {
  tools: api.HostTools;
  jobs: api.HostDevToolJob[];
  busy: string | null;
  run: Run;
  replace: (next: api.HostTools) => void;
  onDevd: (mode: DevdMode) => void;
  onPackage: (name: api.HostPackage) => void;
  onStudioInstall: (names: api.StudioTool[], batch?: boolean) => void;
  onGateInstall: () => void;
  onGateConfig: () => void;
  onEnqueue: (specs: { tool: api.DevTool; action: api.HostDevToolAction }[]) => void;
  onCancelJob: (job: api.HostDevToolJob) => void;
  onClearJobs: (studio: boolean) => void;
}): React.ReactElement {
  const confirm = useConfirm();
  const host = tools.host;
  const devd = host.devd;
  const daemon = api.daemonName(host.kind) || "devd";
  const devdState = devdBadge(devd, tools.daemon_version);
  const canInstallDevd = tools.daemon_version !== undefined && tools.daemon_version !== "";
  const gateState = versionBadge(tools.gate.installed, tools.gate.version, tools.gate_version);
  const canInstallGate = tools.curl && tools.gate_version !== undefined && tools.gate_version !== "";
  // 队列里有动作在跑时不动 gate 本身（升级 / 设置 / 卸载会跟它抢 config.json；安装 gate 也在
  // 这条队列里）。
  const jobsActive = jobs.some(isActive);
  // 安装 gate 的记录摆在 gate 卡片下：进行中的那条，否则最近一条结果（清除之前一直留着）；
  // 开发工具清单只看自己那些。
  const gateJobs = jobs.filter((j) => j.tool === api.GATE_JOB_TOOL);
  const gateJob = gateJobs.find(isActive) ?? gateJobs[gateJobs.length - 1];
  const queued = jobs.filter((j) => j.status === "queued");
  return (
    <>
      <ToolCard
        name={daemon}
        badge={devdState}
        actions={
          <>
            <Button size="sm" variant="outline" disabled={busy !== null || !canInstallDevd || host.status !== "ready"} onClick={() => onDevd("install")}>
              {devd === undefined ? t("安装 {name}", { name: daemon }) : t("重新安装 {name}", { name: daemon })}
            </Button>
            {devd === undefined ? null : (
              <>
                <Button size="sm" variant="outline" disabled={busy !== null}
                  onClick={() => run("devd-check", () => api.checkDevd(host.id).then((r) => {
                    toast(r.host.devd?.status === "ready" ? t("{name} 连接正常", { name: daemon }) : t("{name} 检查未通过", { name: daemon }));
                    return r;
                  }))}>
                  {busy === "devd-check" ? <Loader2Icon className="animate-spin" /> : null}
                  {t("检查 {name}", { name: daemon })}
                </Button>
                <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => onDevd("uninstall")}>
                  {t("卸载 {name}", { name: daemon })}
                </Button>
              </>
            )}
          </>
        }
      >
        <Fact label={t("版本")}>
          <code className="font-mono">{devd?.version === undefined || devd.version === "" ? "—" : devd.version}</code>
          {devd?.tmux ? <span className="text-muted-foreground ml-1">· tmux</span> : null}
        </Fact>
        <Fact label={t("固件内嵌")}>
          <code className="font-mono">{canInstallDevd ? tools.daemon_version : "—"}</code>
        </Fact>
        <Fact label={t("最近检查")}>
          <span className="whitespace-nowrap">{devd?.last_checked_at === undefined || devd.last_checked_at === "" ? "—" : fmtTime(devd.last_checked_at)}</span>
        </Fact>
        {devd?.last_error === undefined || devd.last_error === "" ? null : <span className="text-signal-alert basis-full">{devd.last_error}</span>}
      </ToolCard>

      <PackageCard
        name="git"
        reading={tools.git}
        packageManager={tools.package_manager}
        busy={busy}
        installLabel={t("安装 git")}
        manualHint={t("主机上没有认得的包管理器，请手工安装 git。")}
        onInstall={() => onPackage("git")}
      />

      <PackageCard
        name="tmux"
        reading={tools.tmux}
        packageManager={tools.package_manager}
        busy={busy}
        installLabel={t("安装 tmux")}
        manualHint={t("主机上没有认得的包管理器，请手工安装 tmux。")}
        missingHint={t("没有 tmux 时，工作空间的终端退化为普通登录 shell：断开即结束，会话不能保留。")}
        onInstall={() => onPackage("tmux")}
      />

      <StudioToolsCard tools={tools} jobs={jobs} busy={busy} onInstall={onStudioInstall} onCancel={onCancelJob} onClear={() => onClearJobs(true)} />

      <ToolCard
        name="gate"
        badge={gateState}
        actions={
          <>
            {tools.gate.installed ? (
              <>
                <Button size="sm" variant="outline" disabled={busy !== null || jobsActive || tools.gate.base_url === undefined || tools.gate.base_url === ""}
                  title={tools.gate.base_url === undefined || tools.gate.base_url === "" ? t("主机上 gate 没有设备地址：请先设置 gate。") : undefined}
                  onClick={() => run("gate-update", () => api.updateHostGate(host.id).then((next) => {
                    toast(next.gate.version !== undefined && next.gate.version !== "" && next.gate.version !== tools.gate.version
                      ? t("gate 已升级到 {version}", { version: next.gate.version })
                      : t("gate 已是最新版本"));
                    replace(next);
                    return next;
                  }))}>
                  {busy === "gate-update" ? <Loader2Icon className="animate-spin" /> : null}
                  {t("升级 gate")}
                </Button>
                <Button size="sm" variant="outline" disabled={busy !== null || jobsActive} onClick={onGateConfig}>
                  {t("设置 gate")}
                </Button>
              </>
            ) : (
              <Button size="sm" variant="outline" disabled={busy !== null || jobsActive || !canInstallGate} onClick={onGateInstall}>
                {t("安装 gate")}
              </Button>
            )}
            {tools.gate.installed ? (
              <Button size="sm" variant="outline" disabled={busy !== null || jobsActive}
                onClick={() => {
                  void confirm({
                    title: t("卸载 gate？"),
                    body: <p>{t("删除主机上的 gate 程序、全部工具接入以及保存的设备地址与密钥；工具程序与会话保留。")}</p>,
                    confirmText: t("卸载 gate"),
                    danger: true,
                  }).then((ok) => {
                    if (!ok) return;
                    run("gate-uninstall", () => api.uninstallHostGate(host.id).then((next) => {
                      toast(t("gate 已卸载"));
                      replace(next);
                      return next;
                    }));
                  });
                }}>
                {busy === "gate-uninstall" ? <Loader2Icon className="animate-spin" /> : null}
                {t("卸载 gate")}
              </Button>
            ) : null}
          </>
        }
      >
        <Fact label={t("版本")}>
          <code className="font-mono">{tools.gate.version === undefined || tools.gate.version === "" ? "—" : tools.gate.version}</code>
        </Fact>
        <Fact label={t("固件内嵌")}>
          <code className="font-mono">{tools.gate_version === undefined || tools.gate_version === "" ? "—" : tools.gate_version}</code>
        </Fact>
        {tools.gate.installed ? (
          <>
            <Fact label={t("设备地址")}>
              <code className="font-mono">{tools.gate.base_url === undefined || tools.gate.base_url === "" ? "—" : tools.gate.base_url}</code>
            </Fact>
            <Fact label={t("API密钥")}>{tools.gate.configured ? t("已写入") : <span className="text-signal-alert">{t("未写入")}</span>}</Fact>
            <Fact label={t("路径")}>
              <code className="font-mono">{tools.gate.path ?? "—"}</code>
            </Fact>
          </>
        ) : null}
        {tools.curl ? null : <span className="text-muted-foreground basis-full">{t("主机上没有 curl：安装 gate 要用它从设备取安装脚本与压缩包，请先安装 curl。")}</span>}
        {gateJob === undefined ? null : (
          <div className="mt-1 flex w-full min-w-0 basis-full items-center rounded-md border px-2.5 py-1.5">
            <JobStatus
              job={gateJob}
              position={queued.findIndex((j) => j.id === gateJob.id) + 1}
              dev={undefined}
              onCancel={() => onCancelJob(gateJob)}
              onRetry={onGateInstall}
            />
          </div>
        )}
        <DevToolsSection tools={tools} jobs={jobs} busy={busy} onEnqueue={onEnqueue} onCancel={onCancelJob} onClear={() => onClearJobs(false)} />
        {tools.output === undefined || tools.output === "" ? null : (
          <pre className="text-muted-foreground basis-full font-mono text-[11px] whitespace-pre-wrap">{tools.output}</pre>
        )}
      </ToolCard>
    </>
  );
}

function packageInstalled(name: api.HostPackage): string {
  return name === "git" ? t("git 已安装") : t("tmux 已安装");
}

export function HostToolsPage(): React.ReactElement {
  const search = useLocationSearch();
  const id = Number(search.get("host"));
  const res = useResource(() => api.getHostTools(id), [id]);
  const [override, setOverride] = useState<api.HostTools | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [devdAction, setDevdAction] = useState<DevdMode | null>(null);
  const [pkgDialog, setPkgDialog] = useState<api.HostPackage | null>(null);
  const [studioDialog, setStudioDialog] = useState<{ names: api.StudioTool[]; batch: boolean } | null>(null);
  const [gateDialog, setGateDialog] = useState<GateDialogMode | null>(null);
  const [jobs, setJobs] = useState<api.HostDevToolJob[]>([]);
  // 队列帧里的复核读数只在比页面手里的更新时才采用：进页 / 刷新那次探测之前结束的动作留下的
  // 读数已经过时。revision 交给陪等循环，帧到了就换。
  const appliedAt = useRef("");
  const revisionRef = useRef(0);

  // 动作端点都回整份读数：直接换掉，省一次探测；刷新 / 进页仍以 res 为准。
  const data = override ?? res.data;
  const reload = (): void => {
    appliedAt.current = new Date().toISOString();
    setOverride(null);
    res.reload();
  };
  const run: Run = (name, fn) => {
    setBusy(name);
    fn().then(
      () => setBusy(null),
      (err: unknown) => {
        setBusy(null);
        toast.error(api.errorMessage(err));
        reload();
      },
    );
  };
  const host = data?.host;

  // 采用一帧：换队列，并在复核读数比手里的新时换读数。陪等循环经 ref 取最新的这个。
  const applyFrame = (frame: api.HostDevToolJobs): void => {
    revisionRef.current = frame.revision;
    setJobs(frame.jobs);
    if (frame.tools !== undefined && frame.tools_at !== undefined && Date.parse(frame.tools_at) >= Date.parse(appliedAt.current)) {
      appliedAt.current = frame.tools_at;
      setOverride(frame.tools);
    }
  };
  const applyRef = useRef(applyFrame);
  applyRef.current = applyFrame;

  // 进页先读一次队列快照，然后陪等；离开页面即中止陪等（队列在设备上照跑）。
  useEffect(() => {
    setJobs([]);
    revisionRef.current = 0;
    appliedAt.current = new Date().toISOString();
    if (!Number.isInteger(id) || id <= 0) return;
    const ctrl = new AbortController();
    let alive = true;
    const loop = async (): Promise<void> => {
      while (alive) {
        try {
          const frame = await api.waitHostDevToolJobs(id, revisionRef.current, ctrl.signal);
          if (!alive) return;
          applyRef.current(frame);
        } catch (err) {
          if (!alive || (err instanceof DOMException && err.name === "AbortError")) return;
          // 主机没了 / 受控纳管：不再陪等；其余（网络抖动）歇两秒再来。
          if (err instanceof api.ApiError && (err.status === 404 || err.status === 409)) return;
          await new Promise((resolve) => setTimeout(resolve, 2000));
        }
      }
    };
    api.getHostDevToolJobs(id).then(
      (frame) => {
        if (!alive) return;
        applyRef.current(frame);
        void loop();
      },
      () => {
        // 快照读不到（多半是主机行的问题）：读数那条会把错误摆出来，这里不重复。
      },
    );
    return () => {
      alive = false;
      ctrl.abort();
    };
  }, [id]);

  const enqueue = (specs: { tool: api.DevTool; action: api.HostDevToolAction }[]): void => {
    if (specs.length === 0) return;
    api.enqueueHostDevTools(id, specs).then(applyFrame, (err: unknown) => toast.error(api.errorMessage(err)));
  };
  const installStudio = async (names: api.StudioTool[], password = "", batch = false): Promise<void> => {
    const frame = names.length === 1 && !batch ? await api.installHostStudioTool(id, names[0]!, password)
      : await api.enqueueHostDevTools(id, names.map((name) => ({ tool: `studio/${name}` as api.StudioJobTool, action: "install" })), password);
    applyFrame(frame);
  };
  const cancelJob = (job: api.HostDevToolJob): void => {
    api.cancelHostDevToolJob(id, job.id).then(applyFrame, (err: unknown) => toast.error(api.errorMessage(err)));
  };
  // 清掉一张卡的已结束记录：创作工具卡只清 studio/*，gate 卡清其余（gate 与开发工具）。
  const clearJobs = (studio: boolean): void => {
    void Promise.all(jobs.filter((job) => !isActive(job) && job.tool.startsWith("studio/") === studio)
      .map((job) => api.cancelHostDevToolJob(id, job.id).then(applyFrame)))
      .catch((err: unknown) => toast.error(api.errorMessage(err)));
  };

  return (
    <PageContainer wide compact>
      <PageHeader
        compact
        title={host === undefined ? t("工具配置") : t("工具配置 · {name}", { name: host.name === "" ? host.address : host.name })}
        note={host === undefined ? undefined : `${host.username}@${host.address}:${host.port}${host.system === undefined ? "" : ` · ${host.system}`}`}
        refreshing={res.loading}
        onRefresh={reload}
        leading={
          <Button size="icon-sm" variant="outline" title={t("返回主机/SoC")} aria-label={t("返回主机/SoC")} onClick={() => navigate("/agent-hosts")}>
            <ArrowLeftIcon />
          </Button>
        }
      />
      <ResourceGate resource={res}>
        {(loaded) => (
          <ToolsBoard
            tools={override ?? loaded}
            jobs={jobs}
            busy={busy}
            run={run}
            replace={setOverride}
            onDevd={setDevdAction}
            onPackage={(name) => {
              const cur = override ?? loaded;
              if (needsSudoPassword(cur.host)) {
                setPkgDialog(name);
                return;
              }
              run(name, () => api.installHostPackage(cur.host.id, name).then((next) => {
                toast(packageInstalled(name));
                setOverride(next);
                return next;
              }));
            }}
            onStudioInstall={(names, batch = false) => {
              if (needsSudoPassword((override ?? loaded).host)) { setStudioDialog({ names, batch }); return; }
              run("studio", () => installStudio(names, "", batch));
            }}
            onGateInstall={() => setGateDialog("install")}
            onGateConfig={() => setGateDialog("config")}
            onEnqueue={enqueue}
            onCancelJob={cancelJob}
            onClearJobs={clearJobs}
          />
        )}
      </ResourceGate>
      {devdAction === null || host === undefined ? null : (
        <DevdDialog mode={devdAction} host={host} onClose={() => setDevdAction(null)} onDone={reload} />
      )}
      {pkgDialog === null || host === undefined ? null : (
        <SudoDialog
          title={pkgDialog === "git" ? t("安装 git") : t("安装 tmux")}
          host={host}
          action={(password) => api.installHostPackage(host.id, pkgDialog, password).then((next) => {
            toast(packageInstalled(pkgDialog));
            setOverride(next);
          })}
          onClose={() => setPkgDialog(null)}
          onDone={() => undefined}
        />
      )}
      {studioDialog === null || host === undefined ? null : (
        <SudoDialog title={t("安装创作工具")} host={host} action={(password) => installStudio(studioDialog.names, password, studioDialog.batch)}
          onClose={() => setStudioDialog(null)} onDone={() => undefined} />
      )}
      {gateDialog === null || data === null || data === undefined ? null : (
        <GateDialog mode={gateDialog} tools={data} onClose={() => setGateDialog(null)} onDone={setOverride} onQueued={applyFrame} />
      )}
    </PageContainer>
  );
}
