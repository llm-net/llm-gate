// 智能体 · 主机/SoC：登记可被智能体远程管理的主机与 SoC 开发板。
//
// 两张卡：
//   1. 访问证书——设备自己的一把 ed25519 SSH 密钥对。私钥留在设备上（封存，任何
//      响应都不回显），公钥可下载成 .pub 手工装到主机上；「轮换证书」重新生成并
//      下发到全部已纳管主机，当时不在线的那些留着旧公钥、列在「证书待更新」里。
//   2. 主机清单——添加主机时选定类型、填一次用户名口令，设备用它把公钥写进
//      authorized_keys 并（可选）配好免密 sudo，之后就只用证书登录，口令不保存。类型
//      之后不能切换：受控纳管只能用 Agent远控；工作节点还可安装守护进程 devd；模型服务节点
//      安装守护进程 modeld，多一个「模型服务」页（/host-model?host=<id>）。
//      每一行的「Agent远控」打开这台主机的智能体页（/host-agent?host=<id>）：开对话、
//      逐条下指令、看主机档案与操作日志。工作节点那一行还记着有没有装 devd
//      （llmgate-devd），徽标在行里；安装 / 检查 / 卸载 devd 以及 git、gate 的管理都在
//      「工具配置」页 /host-tools?host=<id>。devd 走同一把访问证书的 SSH 连接到达，
//      主机上没有第二套证书。
//
// 版面取高密度：页头与证书卡各一行读数，主机清单每台两行——第一行身份、类型、状态徽标与动作，
// 第二行系统、免密 sudo、devd 版本、最近检查与失败原因；不放介绍段落、分区说明或字段旁的
// 问号提示，契约以 AGENTS.md「智能体：纳管主机/SoC」为准。
//
// 零轮询：读数只在进页 / 刷新 / 动作之后重取一次。

import { Loader2Icon } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { DevdDialog, type DevdMode, devdBadge, toneClass } from "@/features/agent-hosts/devd";
import { RowActionsMenu } from "@/features/upstreams/row-actions";
import * as api from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { navigate } from "@/lib/router";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

type Run = (name: string, fn: () => Promise<unknown>, after?: () => void) => void;

// ---- 访问证书 ----

function CertificateCard({
  cert,
  busy,
  run,
  onRotated,
}: {
  cert: api.AgentCertificate | undefined;
  busy: string | null;
  run: Run;
  onRotated: (r: api.AgentCertificateRotation) => void;
}): React.ReactElement {
  const confirm = useConfirm();
  if (cert === undefined) {
    return (
      <Card className="flex-row flex-wrap items-center gap-x-3 gap-y-2 px-4 py-2">
        <span className="text-sm font-medium">{t("访问证书")}</span>
        <Badge variant="secondary">{t("未生成")}</Badge>
        <Button size="sm" className="ml-auto" disabled={busy !== null} onClick={() => run("generate", () => api.generateAgentCertificate().then((r) => {
          toast(t("访问证书已生成"));
          return r;
        }))}>
          {busy === "generate" ? <Loader2Icon className="animate-spin" /> : null}
          {t("生成证书")}
        </Button>
      </Card>
    );
  }
  return (
    <Card className="gap-1.5 px-4 py-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        <span className="text-sm font-medium">{t("访问证书")}</span>
        <Badge variant="outline" className="text-signal-ok">
          {t("已生成")}
        </Badge>
        <code className="font-mono">{cert.key_type}</code>
        <code className="font-mono break-all">{cert.fingerprint}</code>
        <span className="text-muted-foreground">{fmtTime(cert.created_at)}</span>
        <div className="ml-auto flex flex-wrap gap-1.5">
          <Button size="sm" variant="outline" onClick={() => api.downloadAgentPublicKey(cert)}>
            {t("下载公钥文件")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              void copyText(cert.public_key).then((ok) => {
                if (ok) toast(t("公钥已复制"));
                else toast.error(t("复制失败，请手动选中复制"));
              });
            }}
          >
            {t("复制公钥")}
          </Button>
          <Button
            size="sm"
            variant="outline"
            disabled={busy !== null}
            onClick={() => {
              void confirm({
                title: t("轮换证书？"),
                body: <p>{t("重新生成密钥对并下发到全部已纳管主机；连不上的主机转为「证书待更新」。")}</p>,
                confirmText: t("轮换证书"),
              }).then((ok) => {
                if (!ok) return;
                run("rotate", () =>
                  api.rotateAgentCertificate().then((r) => {
                    const failed = r.results.filter((x) => !x.ok).length;
                    if (failed === 0) toast(t("证书已轮换，{n} 台主机已同步", { n: r.results.length }));
                    else toast.error(t("证书已轮换，但有 {n} 台主机未同步", { n: failed }));
                    onRotated(r);
                    return r;
                  }),
                );
              });
            }}
          >
            {busy === "rotate" ? <Loader2Icon className="animate-spin" /> : null}
            {t("轮换证书")}
          </Button>
        </div>
      </div>
      <code className="bg-muted/40 block truncate rounded-md border px-2 py-1 font-mono text-[11px]" title={cert.public_key}>
        {cert.public_key}
      </code>
    </Card>
  );
}

// ---- 纳管对话框 ----

/** 主机类型：纳管时选定、之后不改。受控纳管只能用 Agent远控，工作节点还能装 devd，模型服务节点装 modeld。 */
const HOST_KINDS: { id: api.AgentHostKind; label: () => string; hint: () => string }[] = [
  { id: "managed", label: () => t("受控纳管"), hint: () => t("只能使用 Agent远控，不安装 devd") },
  { id: "worker", label: () => t("工作节点"), hint: () => t("Agent远控之外还可安装 devd、git 与 gate") },
  { id: "model_service", label: () => t("模型服务"), hint: () => t("安装 modeld：管理算力服务器、任务队列与模型缓存，对外提供 API，直接运行 Codex / Claude Code") },
];

function kindLabel(kind: api.AgentHostKind): string {
  return kind === "managed" ? t("受控纳管") : kind === "model_service" ? t("模型服务") : t("工作节点");
}

function isKind(v: string): v is api.AgentHostKind {
  return v === "managed" || v === "worker" || v === "model_service";
}

interface EnrollTarget {
  /** 已有主机 = 重新纳管（连接三元组不可改）；undefined = 添加新主机。 */
  host?: api.AgentHost;
}

function EnrollDialog({
  target,
  onClose,
  onDone,
}: {
  target: EnrollTarget;
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const existing = target.host;
  const [name, setName] = useState(existing?.name ?? "");
  const [kind, setKind] = useState<api.AgentHostKind>("managed");
  const [address, setAddress] = useState(existing?.address ?? "");
  const [port, setPort] = useState(String(existing?.port ?? 22));
  const [username, setUsername] = useState(existing?.username ?? "");
  const [password, setPassword] = useState("");
  const [sudo, setSudo] = useState(true);
  const [acceptHostKey, setAcceptHostKey] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    const parsedPort = Number(port);
    if (existing === undefined && (!Number.isInteger(parsedPort) || parsedPort < 1 || parsedPort > 65535)) {
      setError(t("SSH 端口须是 1–65535 的整数"));
      return;
    }
    setBusy(true);
    setError(null);
    const input: api.AgentHostEnrollInput = {
      password,
      configure_sudo: sudo,
      accept_new_host_key: acceptHostKey,
    };
    const call =
      existing === undefined
        ? api.addAgentHost({ ...input, name: name.trim(), kind, address: address.trim(), port: parsedPort, username: username.trim() })
        : api.enrollAgentHost(existing.id, input);
    call.then(
      () => {
        toast(existing === undefined ? t("主机已纳管") : t("主机已重新纳管"));
        onDone();
        onClose();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{existing === undefined ? t("添加主机/SoC") : t("重新纳管主机")}</DialogTitle>
          </DialogHeader>
          {existing === undefined ? (
            <>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="host-name">{t("名称")}</Label>
                <Input id="host-name" value={name} disabled={busy} autoComplete="off" placeholder={t("留空即用地址")}
                  onChange={(e) => setName(e.target.value)} />
              </div>
              {/* 类型纳管后不能切换，所以只在添加时出现；两项各带一句能力说明。 */}
              <div className="flex flex-col gap-1.5">
                <Label>{t("类型")}</Label>
                <RadioGroup value={kind} disabled={busy} className="gap-2" onValueChange={(v) => setKind(isKind(v) ? v : "managed")}>
                  {HOST_KINDS.map((k) => (
                    <label key={k.id} className="flex items-start gap-2 text-sm">
                      <RadioGroupItem value={k.id} className="mt-0.5" />
                      <span className="flex flex-col">
                        <span>{k.label()}</span>
                        <span className="text-muted-foreground text-xs">{k.hint()}</span>
                      </span>
                    </label>
                  ))}
                </RadioGroup>
                <p className="text-muted-foreground text-xs">{t("类型纳管后不能切换")}</p>
              </div>
              <div className="flex gap-3">
                <div className="flex flex-1 flex-col gap-1.5">
                  <Label htmlFor="host-address">{t("地址")}</Label>
                  <Input id="host-address" value={address} required disabled={busy} autoComplete="off" spellCheck={false}
                    placeholder="192.168.1.10" onChange={(e) => setAddress(e.target.value)} />
                </div>
                <div className="flex w-28 flex-col gap-1.5">
                  <Label htmlFor="host-port">{t("SSH 端口")}</Label>
                  <Input id="host-port" value={port} required disabled={busy} inputMode="numeric"
                    onChange={(e) => setPort(e.target.value)} />
                </div>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="host-user">{t("用户名")}</Label>
                <Input id="host-user" value={username} required disabled={busy} autoComplete="off" spellCheck={false}
                  placeholder="admin" onChange={(e) => setUsername(e.target.value)} />
              </div>
            </>
          ) : (
            <code className="text-muted-foreground font-mono text-xs">{`${existing.username}@${existing.address}:${existing.port}`}</code>
          )}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="host-password">{t("登录口令")}</Label>
            <Input id="host-password" type="password" value={password} required disabled={busy} autoComplete="new-password"
              onChange={(e) => setPassword(e.target.value)} />
          </div>
          <label className="flex items-center gap-2 text-sm">
            <Checkbox checked={sudo} disabled={busy} onCheckedChange={(c) => setSudo(c === true)} />
            <span>{t("同时配置免密 sudo")}</span>
          </label>
          {existing === undefined ? null : (
            <label className="flex items-center gap-2 text-sm">
              <Checkbox checked={acceptHostKey} disabled={busy}
                onCheckedChange={(c) => setAcceptHostKey(c === true)} />
              <span>{t("接受新的主机公钥（仅限主机重装过系统）")}</span>
            </label>
          )}
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
              {busy ? t("连接中…") : t("连接并安装公钥")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function RenameDialog({
  host,
  onClose,
  onDone,
}: {
  host: api.AgentHost;
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const [name, setName] = useState(host.name);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault();
            setBusy(true);
            setError(null);
            api.renameAgentHost(host.id, name.trim()).then(
              () => {
                toast(t("名称已修改"));
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
            <DialogTitle>{t("修改名称")}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="rename-host">{t("名称")}</Label>
            <Input id="rename-host" value={name} disabled={busy} autoFocus maxLength={32} onChange={(e) => setName(e.target.value)} />
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
              {t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 主机表 ----

function statusBadge(host: api.AgentHost): { label: string; tone: "ok" | "warn" | "alert" } {
  if (host.status !== "ready") return { label: t("不可用"), tone: "alert" };
  if (!host.cert_current) return { label: t("证书待更新"), tone: "warn" };
  return { label: t("免密可达"), tone: "ok" };
}

/** 第二行的一项读数。标签跟着值走，替代已经去掉的表头。 */
function Fact({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <span className="flex items-center gap-1">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </span>
  );
}

function HostsCard({
  data,
  busy,
  run,
  onAdd,
  onEnroll,
  onRename,
  onDevd,
}: {
  data: api.AgentHostsSnapshot;
  busy: string | null;
  run: Run;
  onAdd: () => void;
  onEnroll: (host: api.AgentHost) => void;
  onRename: (host: api.AgentHost) => void;
  onDevd: (mode: DevdMode, host: api.AgentHost) => void;
}): React.ReactElement {
  const confirm = useConfirm();
  const hasCert = data.certificate !== undefined;
  return (
    <Card className="gap-0 overflow-hidden p-0">
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-1.5">
        <span className="text-sm font-medium">{t("主机/SoC")}</span>
        <span className="text-muted-foreground text-xs">{t("共 {n} 台", { n: data.hosts.length })}</span>
        <div className="ml-auto">
          <Button size="sm" disabled={!hasCert || busy !== null} onClick={onAdd}>
            {t("添加主机")}
          </Button>
        </div>
      </div>
      {/* 每台主机两行：第一行是身份、状态与动作，第二行是读数与失败原因。不用表格——
          七列在英 / 日下每格都要换行，反而看不出哪个值属于哪一项；两行各自成句，窄屏
          也只是把动作挪到下一行。 */}
      <div className="divide-y">
        {data.hosts.length === 0 ? (
          <div className="text-muted-foreground flex h-16 items-center justify-center text-xs">
            {hasCert ? t("还没有纳管任何主机") : t("先生成访问证书")}
          </div>
        ) : (
          data.hosts.map((host) => {
            const badge = statusBadge(host);
            const daemonVersion = api.embeddedDaemonVersion(data, host.kind);
            const devd = devdBadge(host.devd, daemonVersion);
            // 受控纳管没有守护进程与工具配置：不显示徽标，也没有「工具配置」按钮。守护进程按类型
            // 叫 devd（工作节点）或 modeld（模型服务节点）。
            const daemon = api.daemonName(host.kind);
            const allowsDevd = daemon !== "";
            const installed = allowsDevd && host.devd !== undefined;
            return (
              <div key={host.id} className="flex flex-col gap-1 px-3 py-2">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
                  <span className="text-sm font-medium">{host.name === "" ? host.address : host.name}</span>
                  <code className="text-muted-foreground font-mono text-xs">
                    {host.username}@{host.address}:{host.port}
                  </code>
                  <Badge variant="secondary">{kindLabel(host.kind)}</Badge>
                  <Badge variant="outline" className={toneClass(badge.tone)}>
                    {badge.label}
                  </Badge>
                  {/* 徽标带上守护进程名前缀：没有表头了，两个徽标挨着得自己说清各是什么。 */}
                  {allowsDevd ? (
                    <Badge variant={devd.tone === "none" ? "secondary" : "outline"} className={devd.tone === "none" ? "" : toneClass(devd.tone)}>
                      {daemon} · {devd.label}
                    </Badge>
                  ) : null}
                  <div className="ml-auto flex flex-wrap gap-1.5">
                    <Button size="sm" variant="outline" onClick={() => navigate("/host-agent", { search: `?host=${host.id}` })}>
                      {t("Agent远控")}
                    </Button>
                    {host.kind === "model_service" ? (
                      <Button size="sm" variant="outline" onClick={() => navigate("/host-model", { search: `?host=${host.id}` })}>
                        {t("模型服务")}
                      </Button>
                    ) : null}
                    {allowsDevd ? (
                      <Button size="sm" variant="outline" onClick={() => navigate("/host-tools", { search: `?host=${host.id}` })}>
                        {t("工具配置")}
                      </Button>
                    ) : null}
                    <Button size="sm" variant="outline" disabled={busy !== null}
                      onClick={() => run(`check-${host.id}`, () => api.checkAgentHost(host.id).then((r) => {
                        toast(r.host.status === "ready" ? t("连接正常") : t("连接检查未通过"));
                        return r;
                      }))}>
                      {busy === `check-${host.id}` ? <Loader2Icon className="animate-spin" /> : null}
                      {t("连接检查")}
                    </Button>
                    <RowActionsMenu
                      size="icon-sm"
                      label={t("主机 {name} 的更多操作", { name: host.name === "" ? host.address : host.name })}
                      info={[
                        { label: t("主机公钥指纹"), value: <code className="font-mono text-[10px]">{host.host_key_fingerprint ?? "—"}</code> },
                      ]}
                      actions={[
                        {
                          label: t("补发证书"),
                          onSelect: () => run(`push-${host.id}`, () => api.pushAgentHost(host.id).then((r) => {
                            toast(t("当前证书已下发"));
                            return r;
                          })),
                        },
                        { label: t("重新纳管（用口令）"), onSelect: () => onEnroll(host) },
                        { label: t("修改名称"), onSelect: () => onRename(host) },
                        {
                          label: t("解除纳管"),
                          destructive: true,
                          onSelect: () => {
                            if (installed) {
                              onDevd("remove", host);
                              return;
                            }
                            void confirm({
                              title: t("解除纳管？"),
                              body: <p>{t("摘掉设备公钥与免密 sudo 条目后删除这一行；主机不可达也照删。")}</p>,
                              confirmText: t("解除纳管"),
                              danger: true,
                            }).then((ok) => {
                              if (!ok) return;
                              run(`remove-${host.id}`, () =>
                                api.removeAgentHost(host.id, true).then((r) => {
                                  toast(r.key_removed ? t("已解除纳管，公钥已从主机上移除") : t("已解除纳管；主机不可达，公钥需在主机上手工清理"));
                                  return r;
                                }),
                              );
                            });
                          },
                        },
                      ]}
                    />
                  </div>
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                  <Fact label={t("系统")}>{host.system ?? "—"}</Fact>
                  <Fact label={t("免密 sudo")}>{host.sudo_nopasswd ? t("已配置") : t("未配置")}</Fact>
                  {!installed || host.devd === undefined ? null : (
                    <Fact label={daemon}>
                      <code className="font-mono">{host.devd.version === undefined || host.devd.version === "" ? "—" : host.devd.version}</code>
                      {host.devd.tmux ? <span className="text-muted-foreground ml-1">· tmux</span> : null}
                    </Fact>
                  )}
                  <Fact label={t("最近检查")}>
                    <span className="whitespace-nowrap">
                      {host.last_checked_at === undefined || host.last_checked_at === "" ? "—" : fmtTime(host.last_checked_at)}
                    </span>
                  </Fact>
                  {devd.tone === "warn" ? (
                    <span className="text-muted-foreground">
                      {t("本固件内嵌 {version}，请在工具配置页重新安装 {name}", { version: daemonVersion ?? "", name: daemon })}
                    </span>
                  ) : null}
                  {/* 失败原因整句都长，占满一行另起：没有错误时这一条不在，两行的节奏不变。 */}
                  {host.last_error === undefined || host.last_error === "" ? null : (
                    <span className="text-signal-alert basis-full">{host.last_error}</span>
                  )}
                  {!installed || host.devd?.last_error === undefined || host.devd.last_error === "" ? null : (
                    <span className="text-signal-alert basis-full">
                      <span className="text-muted-foreground mr-1">{daemon}</span>
                      {host.devd.last_error}
                    </span>
                  )}
                </div>
              </div>
            );
          })
        )}
      </div>
    </Card>
  );
}

// ---- 页面 ----

export function AgentHostsPage(): React.ReactElement {
  const res = useResource(() => api.getAgentHosts(), []);
  const [busy, setBusy] = useState<string | null>(null);
  const [enrolling, setEnrolling] = useState<EnrollTarget | null>(null);
  const [renaming, setRenaming] = useState<api.AgentHost | null>(null);
  const [devdAction, setDevdAction] = useState<{ mode: DevdMode; host: api.AgentHost } | null>(null);
  const [rotation, setRotation] = useState<api.AgentCertificateRotation | null>(null);

  const run: Run = (name, fn, after) => {
    setBusy(name);
    fn().then(
      () => {
        setBusy(null);
        after?.();
        res.reload();
      },
      (err: unknown) => {
        setBusy(null);
        toast.error(api.errorMessage(err));
        res.reload();
      },
    );
  };

  const failures = rotation === null ? [] : rotation.results.filter((r) => !r.ok);

  return (
    <PageContainer wide compact>
      <PageHeader compact title={t("主机/SoC")} refreshing={res.loading} onRefresh={res.reload} />
      <ResourceGate resource={res}>
        {(data) => (
          <>
            <CertificateCard cert={data.certificate} busy={busy} run={run} onRotated={setRotation} />
            {failures.length === 0 ? null : (
              <Card className="gap-1 px-4 py-2">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-signal-alert text-sm font-medium">{t("这些主机没收到新证书")}</span>
                  <Button size="sm" variant="outline" className="ml-auto" onClick={() => setRotation(null)}>
                    {t("知道了")}
                  </Button>
                </div>
                <ul className="flex flex-col gap-0.5 text-xs">
                  {failures.map((f) => (
                    <li key={f.id}>
                      <code className="font-mono">{f.address}</code>
                      <span className="text-muted-foreground ml-2">{f.error}</span>
                    </li>
                  ))}
                </ul>
              </Card>
            )}
            <HostsCard
              data={data}
              busy={busy}
              run={run}
              onAdd={() => setEnrolling({})}
              onEnroll={(host) => setEnrolling({ host })}
              onRename={setRenaming}
              onDevd={(mode, host) => setDevdAction({ mode, host })}
            />
          </>
        )}
      </ResourceGate>
      {enrolling === null ? null : (
        <EnrollDialog target={enrolling} onClose={() => setEnrolling(null)} onDone={res.reload} />
      )}
      {renaming === null ? null : (
        <RenameDialog host={renaming} onClose={() => setRenaming(null)} onDone={res.reload} />
      )}
      {devdAction === null ? null : (
        <DevdDialog mode={devdAction.mode} host={devdAction.host} onClose={() => setDevdAction(null)} onDone={res.reload} />
      )}
    </PageContainer>
  );
}
