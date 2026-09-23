// 智能体 · 凭证管理：交给智能体使用的第三方凭证。
//
// 目前只有一种类型 git——托管站点（github.com / gitee.com）的账号 + 令牌。令牌只在添加 /
// 修改的对话框里填一次，设备封存后任何响应都不回显，行里只带末 4 位作辨认；修改时令牌
// 留空即不换。类型与站点的词汇表由读数带回（kinds），页面不自己维护一份。
//
// 版面同「主机/SoC」：页头一行 + 凭证清单，每份两行——第一行名称、类型徽标、账号@站点、
// 令牌末 4 位与动作，第二行添加 / 更新时刻。零轮询：读数只在进页 / 刷新 / 动作之后重取一次。

import { Loader2Icon } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { RowActionsMenu } from "@/features/upstreams/row-actions";
import * as api from "@/lib/api";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

/** 第二行的一项读数。 */
function Fact({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <span className="flex items-center gap-1">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </span>
  );
}

function credentialTitle(c: api.Credential): string {
  return c.name === "" ? `${c.username}@${c.host}` : c.name;
}

/** 站点各自的令牌叫法与获取处，只作提示。 */
function hostHint(host: string): string {
  switch (host) {
    case "github.com":
      return t("GitHub：Settings → Developer settings → Personal access tokens 生成，填令牌本身。");
    case "gitee.com":
      return t("Gitee：设置 → 私人令牌 生成，填令牌本身。");
    default:
      return "";
  }
}

// ---- 添加 / 修改 ----

function CredentialDialog({
  kinds,
  current,
  onClose,
  onDone,
}: {
  kinds: api.CredentialKindSpec[];
  /** 缺席 = 添加。 */
  current?: api.Credential | undefined;
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const editing = current !== undefined;
  const [kind, setKind] = useState<api.CredentialKind>(current?.kind ?? kinds[0]?.kind ?? "git");
  const spec = kinds.find((k) => k.kind === kind);
  const hosts = spec?.hosts ?? [];
  const [host, setHost] = useState(current?.host ?? hosts[0] ?? "");
  const [username, setUsername] = useState(current?.username ?? "");
  const [secret, setSecret] = useState("");
  const [name, setName] = useState(current?.name ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    setBusy(true);
    setError(null);
    const input: api.CredentialInput = { name: name.trim(), host, username: username.trim(), secret };
    const call = editing ? api.updateCredential(current.id, input) : api.createCredential({ kind, ...input });
    call.then(
      () => {
        toast(editing ? t("凭证已修改") : t("凭证已添加"));
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
            <DialogTitle>{editing ? t("修改凭证") : t("添加凭证")}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label>{t("类型")}</Label>
            <Select
              value={kind}
              disabled={busy || editing}
              onValueChange={(v) => {
                const next = v as api.CredentialKind;
                setKind(next);
                setHost(kinds.find((k) => k.kind === next)?.hosts[0] ?? "");
              }}
            >
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {kinds.map((k) => (
                  <SelectItem key={k.kind} value={k.kind}>
                    {api.credentialKindLabel(k.kind)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>{t("站点")}</Label>
            <Select value={host} disabled={busy} onValueChange={setHost}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder={t("未选择")} />
              </SelectTrigger>
              <SelectContent>
                {hosts.map((h) => (
                  <SelectItem key={h} value={h}>
                    {h}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="credential-username">{t("用户名")}</Label>
            <Input id="credential-username" value={username} required disabled={busy} autoComplete="off" spellCheck={false} maxLength={128} onChange={(e) => setUsername(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="credential-secret">{editing ? t("令牌（留空则保持不变）") : t("令牌")}</Label>
            <Input id="credential-secret" type="password" value={secret} required={!editing} disabled={busy} autoComplete="new-password" spellCheck={false} onChange={(e) => setSecret(e.target.value)} />
            <p className="text-muted-foreground text-xs">
              {hostHint(host)} {t("令牌封存在设备上，保存后不再回显。")}
            </p>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="credential-name">{t("名称（可选）")}</Label>
            <Input id="credential-name" value={name} disabled={busy} maxLength={64} onChange={(e) => setName(e.target.value)} />
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
            <Button type="submit" disabled={busy || host === ""}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 清单 ----

function CredentialsCard({
  data,
  busy,
  onAdd,
  onEdit,
  onDelete,
}: {
  data: api.CredentialsSnapshot;
  busy: string | null;
  onAdd: () => void;
  onEdit: (c: api.Credential) => void;
  onDelete: (c: api.Credential) => void;
}): React.ReactElement {
  return (
    <Card className="gap-0 overflow-hidden p-0">
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-1.5">
        <span className="text-sm font-medium">{t("凭证")}</span>
        <span className="text-muted-foreground text-xs">{t("共 {n} 份", { n: data.credentials.length })}</span>
        <div className="ml-auto">
          <Button size="sm" disabled={busy !== null || data.kinds.length === 0} onClick={onAdd}>
            {t("添加凭证")}
          </Button>
        </div>
      </div>
      <div className="divide-y">
        {data.credentials.length === 0 ? (
          <div className="text-muted-foreground flex h-16 items-center justify-center text-xs">{t("还没有保存任何凭证")}</div>
        ) : (
          data.credentials.map((c) => (
            <div key={c.id} className="flex flex-col gap-1 px-3 py-2">
              <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
                <span className="text-sm font-medium">{credentialTitle(c)}</span>
                <Badge variant="secondary">{api.credentialKindLabel(c.kind)}</Badge>
                <code className="text-muted-foreground font-mono text-xs">
                  {c.username}@{c.host}
                </code>
                {c.secret_hint === undefined || c.secret_hint === "" ? null : (
                  <code className="text-muted-foreground font-mono text-xs">••••{c.secret_hint}</code>
                )}
                <div className="ml-auto flex flex-wrap gap-1.5">
                  <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => onEdit(c)}>
                    {busy === `edit-${c.id}` ? <Loader2Icon className="animate-spin" /> : null}
                    {t("编辑")}
                  </Button>
                  <RowActionsMenu
                    size="icon-sm"
                    label={t("凭证 {name} 的更多操作", { name: credentialTitle(c) })}
                    actions={[{ label: t("删除"), destructive: true, onSelect: () => onDelete(c) }]}
                  />
                </div>
              </div>
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                <Fact label={t("添加于")}>
                  <span className="whitespace-nowrap">{fmtTime(c.created_at)}</span>
                </Fact>
                <Fact label={t("更新于")}>
                  <span className="whitespace-nowrap">{fmtTime(c.updated_at)}</span>
                </Fact>
              </div>
            </div>
          ))
        )}
      </div>
    </Card>
  );
}

export function CredentialsPage(): React.ReactElement {
  const res = useResource(() => api.listCredentials(), []);
  const confirm = useConfirm();
  const [busy, setBusy] = useState<string | null>(null);
  const [dialog, setDialog] = useState<{ current?: api.Credential } | null>(null);

  function remove(c: api.Credential): void {
    void confirm({
      title: t("删除凭证？"),
      body: <p>{t("删除 {name} 保存在设备上的令牌；站点上的令牌本身不受影响。", { name: `${c.username}@${c.host}` })}</p>,
      confirmText: t("删除"),
      danger: true,
    }).then((ok) => {
      if (!ok) return;
      setBusy(`delete-${c.id}`);
      api.deleteCredential(c.id).then(
        () => {
          setBusy(null);
          toast(t("凭证已删除"));
          res.reload();
        },
        (err: unknown) => {
          setBusy(null);
          toast.error(api.errorMessage(err));
          res.reload();
        },
      );
    });
  }

  return (
    <PageContainer wide compact>
      <PageHeader
        compact
        title={t("凭证管理")}
        note={t("交给智能体使用的第三方凭证；令牌封存在设备上，不回显。")}
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {(data) => (
          <CredentialsCard
            data={data}
            busy={busy}
            onAdd={() => setDialog({})}
            onEdit={(c) => setDialog({ current: c })}
            onDelete={remove}
          />
        )}
      </ResourceGate>
      {dialog === null || res.data === null ? null : (
        <CredentialDialog kinds={res.data.kinds} current={dialog.current} onClose={() => setDialog(null)} onDone={res.reload} />
      )}
    </PageContainer>
  );
}
