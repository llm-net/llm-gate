// 智能体 · 工作空间：工作节点上的目录。开发工作空间可选从 git 仓库克隆而来；创作工作空间是素材
// 目录，智能体（节点上的 Codex / Claude Code）在里面生成与整理图像 / 视频。早先落在设备上的创作空间
// 另列一节「本设备」，只能管理文件、不能再新建。
//
// 创建时选一台工作节点、起一个名字（主机上落成 ~/workspaces/<name>），可选填仓库地址、分支
// 与 git 凭证：点「创建」时设备经 SSH 在主机上先校验仓库、分支与凭证，任一不通就不建目录也
// 不落行；通过后建目录、克隆（可能要等几分钟）。建成后「打开」进入 /workspace?id=<id>：
// 左边文件 / Git，中间文件预览 / 编辑，右边 tmux 终端——要这台主机的 devd 可用。
//
// 版面：页头一行（标题、说明、「创建工作空间」、刷新），正文**按工作节点分组**——每台工作
// 节点一节，节头是主机名、登录地址、devd 状态与数量，节内是自适应列数的卡片网格（每列
// 至少 17rem），每张卡片是一个工作空间：顶部图标（有仓库 FolderGit2 / 空目录 Folder）、名称
// 与「⋯」菜单，中部仓库（只显示 owner/repo，悬停看全地址）、分支徽标与主机上的路径，底部
// 创建时刻与「打开」。节头末尾一个「+ 创建空间」小按钮，点它打开创建框并预选这台主机；
// 没有一台工作节点时正文是一张引导卡。零轮询：读数只在进页 / 刷新 / 动作之后重取一次。

import { FolderGit2Icon, FolderIcon, GitBranchIcon, KeyRoundIcon, Loader2Icon, PlusIcon, ServerIcon, SparklesIcon } from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { toneClass } from "@/features/agent-hosts/devd";
import { RowActionsMenu } from "@/features/upstreams/row-actions";
import { cn } from "@/lib/cn";
import * as api from "@/lib/api";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { navigate } from "@/lib/router";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

/** 卡片里的一项读数：图标 + 文本，文本单行截断，全文放 title。 */
function Fact({ icon, text, mono = false, title }: { icon: React.ReactNode; text: string; mono?: boolean; title?: string }): React.ReactElement {
  return (
    <span className="text-muted-foreground flex min-w-0 items-center gap-1.5 text-xs" title={title ?? text}>
      <span className="shrink-0 [&>svg]:size-3.5">{icon}</span>
      <span className={cn("min-w-0 truncate", mono && "font-mono")}>{text}</span>
    </span>
  );
}

/**
 * 仓库地址的短名：卡片一列只有十几个字符宽，整条 URL 放不下也没必要。取路径最后两段
 * （owner/repo），去掉尾部 .git；https 与 scp 形 `git@host:owner/repo.git` 都认；认不出
 * 就原样返回，全地址一律留在 title 里。
 */
function repoLabel(url: string): string {
  let path = url.trim().replace(/\.git\/?$/, "").replace(/\/+$/, "");
  const scp = /^[^/@]+@[^:/]+:(.+)$/.exec(path);
  if (scp?.[1] !== undefined) path = scp[1];
  else path = path.replace(/^[a-z][a-z0-9+.-]*:\/\/[^/]+\/?/i, "");
  const parts = path.split("/").filter((p) => p !== "");
  if (parts.length === 0) return url;
  return parts.slice(-2).join("/");
}

function hostTitle(h: { name: string; address: string }): string {
  return h.name === "" ? h.address : h.name;
}

const NONE = "__none__";

// ---- 创建 ----

function CreateDialog({
  hosts,
  credentials,
  templates,
  initialKind,
  initialHostId,
  onClose,
  onDone,
}: {
  hosts: api.WorkspaceHost[];
  credentials: { id: string; label: string }[];
  /** 创作工作空间可选的创作类型（首项是缺省）。 */
  templates: api.StudioTemplate[];
  /** 从某一节的「创建空间」进来时预选类型；缺省开发。 */
  initialKind?: api.WorkspaceKind | undefined;
  /** 从某台主机那节的「创建空间」进来时预选它；缺省选第一台。 */
  initialHostId?: number | undefined;
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const preset = hosts.find((h) => h.id === initialHostId) ?? hosts[0];
  const [kind, setKind] = useState<api.WorkspaceKind>(initialKind ?? "dev");
  const [hostId, setHostId] = useState<string>(preset === undefined ? "" : String(preset.id));
  // 创作类型：创建时选定一次、之后不能改。
  const [template, setTemplate] = useState<string>(templates[0]?.id ?? "");
  const [name, setName] = useState("");
  const [repo, setRepo] = useState("");
  const [branch, setBranch] = useState("");
  const [credential, setCredential] = useState<string>(NONE);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const withRepo = repo.trim() !== "";

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    setBusy(true);
    setError(null);
    const input: api.WorkspaceInput = kind === "studio" ? { kind: "studio", host_id: Number(hostId), name: name.trim() } : { host_id: Number(hostId), name: name.trim() };
    if (kind === "studio" && template !== "") input.template = template;
    if (kind === "dev" && withRepo) {
      input.repo_url = repo.trim();
      if (branch.trim() !== "") input.branch = branch.trim();
      if (credential !== NONE) input.credential_id = credential;
    }
    api.createWorkspace(input).then(
      () => {
        toast(t("工作空间已创建"));
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
            <DialogTitle>{t("创建工作空间")}</DialogTitle>
          </DialogHeader>
          <RadioGroup value={kind} disabled={busy} onValueChange={(v) => setKind(v === "studio" ? "studio" : "dev")} className="grid gap-2 sm:grid-cols-2">
            {(
              [
                { key: "dev", icon: <FolderGit2Icon />, label: t("开发工作空间"), note: t("工作节点上的项目目录：文件、Git、终端与开发工具。") },
                { key: "studio", icon: <SparklesIcon />, label: t("创作工作空间"), note: t("工作节点上的素材目录：智能体在节点上生成图像 / 视频并整理成果。") },
              ] as const
            ).map((opt) => (
              <label key={opt.key} className={cn("hover:border-ring flex cursor-pointer items-start gap-2 rounded-lg border p-3", kind === opt.key && "border-primary/50 bg-primary/5")}>
                <RadioGroupItem value={opt.key} className="mt-0.5" />
                <span className="flex min-w-0 flex-col gap-0.5">
                  <span className="flex items-center gap-1.5 text-sm font-medium [&>svg]:size-4">
                    {opt.icon}
                    {opt.label}
                  </span>
                  <span className="text-muted-foreground text-xs">{opt.note}</span>
                </span>
              </label>
            ))}
          </RadioGroup>
          <div className="flex flex-col gap-1.5">
            <Label>{t("工作节点")}</Label>
            <Select value={hostId} disabled={busy} onValueChange={setHostId}>
              <SelectTrigger className="w-full">
                <SelectValue placeholder={t("未选择")} />
              </SelectTrigger>
              <SelectContent>
                {hosts.map((h) => (
                  <SelectItem key={h.id} value={String(h.id)}>
                    {hostTitle(h)} · {h.username}@{h.address}
                    {h.devd_ready ? "" : ` · ${t("devd 不可用")}`}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {hosts.length === 0 ? (
              <p className="text-signal-alert text-xs">{t("还没有工作节点：请先在主机/SoC 页面以「工作节点」类型纳管一台主机。")}</p>
            ) : kind === "studio" ? (
              <p className="text-muted-foreground text-xs">{t("素材目录落在这台工作节点的 ~/workspaces/<名称>，经 devd 读写，缩略图缓存在设备上。要求这台主机的 devd 可用。")}</p>
            ) : null}
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="workspace-name">{t("名称")}</Label>
            <Input id="workspace-name" value={name} required disabled={busy} autoComplete="off" spellCheck={false} maxLength={24} pattern="[A-Za-z0-9][A-Za-z0-9._-]*" onChange={(e) => setName(e.target.value)} />
            <p className="text-muted-foreground text-xs">
              {t("会在工作节点上创建目录 ~/workspaces/<名称>；字母或数字开头，只含字母、数字、点、下划线与连字符。")}
            </p>
          </div>
          {kind === "studio" && templates.length > 0 ? (
          <div className="flex flex-col gap-1.5">
            <Label>{t("创作类型")}</Label>
            <Select value={template} disabled={busy} onValueChange={setTemplate}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {templates.map((tpl) => (
                  <SelectItem key={tpl.id} value={tpl.id}>
                    {tpl.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-muted-foreground text-xs leading-relaxed">
              {templates.find((tpl) => tpl.id === template)?.description ?? ""} {t("创作类型决定智能体的工作规程与项目说明的初始栏目，创建后不能更改。")}
            </p>
          </div>
          ) : null}
          {kind === "studio" ? (
            <p className="text-muted-foreground text-xs leading-relaxed">
              {t("打开后上传参考图、产品图或视频素材，在对话里给智能体下指令：智能体（Codex 或 Claude Code，新建对话时选）在这台工作节点上运行，直接用节点上的 ffmpeg 等处理素材，并用对话选定的 API 密钥所授权的生成模型生成图像与视频，结果保存回同一个目录。先在这台节点的「工具配置」页用 gate 安装 Codex 或 Claude Code。")}
            </p>
          ) : null}
          {kind === "dev" ? (
          <>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="workspace-repo">{t("仓库地址（可选）")}</Label>
            <Input id="workspace-repo" value={repo} disabled={busy} autoComplete="off" spellCheck={false} placeholder="https://github.com/org/repo.git" onChange={(e) => setRepo(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="workspace-branch">{t("分支（可选）")}</Label>
            <Input id="workspace-branch" value={branch} disabled={busy || !withRepo} autoComplete="off" spellCheck={false} placeholder="main" onChange={(e) => setBranch(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>{t("凭证（可选）")}</Label>
            <Select value={credential} disabled={busy || !withRepo} onValueChange={setCredential}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={NONE}>{t("不使用凭证")}</SelectItem>
                {credentials.map((c) => (
                  <SelectItem key={c.id} value={c.id}>
                    {c.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <p className="text-muted-foreground text-xs">
              {t("私有仓库选一份「凭证管理」里的 git 凭证。点「创建」时先在工作节点上校验仓库、分支与凭证，不通过就不会创建；通过后克隆仓库，可能要等几分钟。")}
            </p>
          </div>
          </>
          ) : null}
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy || (kind === "dev" && hostId === "")}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {busy ? t("正在校验并创建…") : t("创建")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 删除 ----

function DeleteDialog({ ws, onClose, onDone }: { ws: api.Workspace; onClose: () => void; onDone: () => void }): React.ReactElement {
  // 「同时删除工作节点上的目录」的缺省：创作空间与没有仓库的开发空间，目录里只有设备落成的内容，缺省勾上——
  // 留下的目录会挡住同名重建；克隆了仓库的开发空间可能有未提交的改动，缺省不勾。
  const [removeDir, setRemoveDir] = useState(ws.kind === "studio" || ws.repo_url === undefined);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  function submit(): void {
    setBusy(true);
    setError(null);
    api.deleteWorkspace(ws.id, removeDir).then(
      () => {
        toast(t("工作空间已删除"));
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
        <div className="flex flex-col gap-4">
          <DialogHeader>
            <DialogTitle>{t("删除工作空间 {name}？", { name: ws.name })}</DialogTitle>
          </DialogHeader>
          {ws.kind === "studio" && ws.host_id === 0 ? (
            <p className="text-sm">{t("删除这个创作工作空间的全部对话，并删除设备上的目录 {path} 及其中的素材与生成结果（不可恢复）。", { path: ws.path })}</p>
          ) : ws.kind === "studio" ? (
            <>
              <p className="text-sm">{t("删除这个创作工作空间的全部对话与设备上的缩略图缓存。不删工作节点上的目录时，同名空间要等目录清掉才能再建。")}</p>
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={removeDir} disabled={busy} onCheckedChange={(c) => setRemoveDir(c === true)} />
                <span>{t("同时删除工作节点上的目录 {path}（不可恢复）", { path: ws.path })}</span>
              </label>
            </>
          ) : (
            <>
              <p className="text-sm">{ws.repo_url === undefined ? t("从设备上删除这条记录。不删工作节点上的目录时，同名空间要等目录清掉才能再建。") : t("从设备上删除这条记录；克隆的仓库缺省保留在工作节点上，勾选下面一项才会连目录一起删。")}</p>
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={removeDir} disabled={busy} onCheckedChange={(c) => setRemoveDir(c === true)} />
                <span>{t("同时删除工作节点上的目录 {path}（不可恢复）", { path: ws.path })}</span>
              </label>
            </>
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
            <Button type="button" variant="destructive" disabled={busy} onClick={submit}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {t("删除")}
            </Button>
          </DialogFooter>
        </div>
      </DialogContent>
    </Dialog>
  );
}

// ---- 卡片 ----

const GRID = "grid grid-cols-[repeat(auto-fill,minmax(min(100%,17rem),1fr))] gap-3";

function WorkspaceCard({ ws, onDelete }: { ws: api.Workspace; onDelete: () => void }): React.ReactElement {
  const studio = ws.kind === "studio";
  const hasRepo = ws.repo_url !== undefined;
  const canOpen = (studio && ws.host_id === 0) || ws.host_ready;
  const openTitle = canOpen ? undefined : t("这台主机的 devd 当前不可用，请先在主机/SoC 页面安装或检查。");
  return (
    <Card className="group gap-0 p-0 transition-shadow hover:shadow-md">
      <div className="flex items-start gap-2.5 px-4 pt-4">
        <span className={cn("bg-muted text-muted-foreground flex size-9 shrink-0 items-center justify-center rounded-lg [&>svg]:size-5", (hasRepo || studio) && "text-foreground")}>
          {studio ? <SparklesIcon /> : hasRepo ? <FolderGit2Icon /> : <FolderIcon />}
        </span>
        <div className="flex min-w-0 flex-1 flex-col gap-0.5 pt-0.5">
          <span className="truncate text-sm font-semibold leading-5" title={ws.name}>
            {ws.name}
          </span>
          {studio ? (
            <span className="text-muted-foreground text-xs leading-4">
              {ws.template_name === undefined ? t("创作工作空间") : t("创作工作空间 · {type}", { type: ws.template_name })}
              {ws.host_id === 0 ? ` · ${t("仅文件")}` : ""}
            </span>
          ) : hasRepo ? (
            <span className="text-muted-foreground truncate font-mono text-xs leading-4" title={ws.repo_url}>
              {repoLabel(ws.repo_url ?? "")}
            </span>
          ) : (
            <span className="text-muted-foreground text-xs leading-4">{t("空目录")}</span>
          )}
        </div>
        <RowActionsMenu size="icon-xs" label={t("工作空间 {name} 的更多操作", { name: ws.name })} actions={[{ label: t("删除"), destructive: true, onSelect: onDelete }]} />
      </div>
      <div className="flex flex-wrap items-center gap-1.5 px-4 pt-3 empty:hidden">
        {ws.branch === undefined ? null : (
          <Badge variant="secondary" className="max-w-full font-mono">
            <GitBranchIcon />
            <span className="truncate">{ws.branch}</span>
          </Badge>
        )}
        {ws.credential_label === undefined ? null : (
          <Badge variant="outline" className="max-w-full">
            <KeyRoundIcon />
            <span className="truncate">{ws.credential_label}</span>
          </Badge>
        )}
        {canOpen ? null : (
          <Badge variant="outline" className={toneClass("alert")}>
            {t("devd 不可用")}
          </Badge>
        )}
      </div>
      <div className="flex flex-col gap-1 px-4 pt-3 pb-3">
        <Fact icon={<FolderIcon />} text={ws.path} mono />
      </div>
      <div className="mt-auto flex items-center gap-2 border-t px-4 py-2.5">
        <span className="text-muted-foreground min-w-0 truncate text-xs" title={fmtTime(ws.created_at)}>
          {t("创建于")} {fmtTime(ws.created_at)}
        </span>
        <Button size="sm" className="ml-auto" disabled={!canOpen} title={openTitle} onClick={() => navigate("/workspace", { search: `?id=${ws.id}` })}>
          {t("打开")}
        </Button>
      </div>
    </Card>
  );
}

/** 节头右端的小按钮：在这一节里创建。 */
function CreateInSection({ onClick }: { onClick: () => void }): React.ReactElement {
  return (
    <Button size="sm" variant="ghost" className="text-muted-foreground hover:text-foreground h-6 gap-1 px-2 text-xs" onClick={onClick}>
      <PlusIcon />
      {t("创建空间")}
    </Button>
  );
}

interface HostGroup {
  id: number;
  title: string;
  address: string;
  ready: boolean;
  workspaces: api.Workspace[];
}

/** 按工作节点分组：先列快照里的每台工作节点（含还没有工作空间的），再补上行里指向已不在名单里的主机。 */
function groupByHost(data: api.WorkspacesSnapshot): HostGroup[] {
  const groups = new Map<number, HostGroup>();
  for (const h of data.hosts) {
    groups.set(h.id, { id: h.id, title: hostTitle(h), address: `${h.username}@${h.address}`, ready: h.devd_ready, workspaces: [] });
  }
  for (const ws of data.workspaces) {
    if (ws.kind === "studio" && ws.host_id === 0) continue;
    let g = groups.get(ws.host_id);
    if (g === undefined) {
      g = { id: ws.host_id, title: hostTitle({ name: ws.host_name, address: ws.host_address }), address: ws.host_address, ready: ws.host_ready, workspaces: [] };
      groups.set(ws.host_id, g);
    }
    g.workspaces.push(ws);
  }
  for (const g of groups.values()) g.workspaces.sort((a, b) => a.name.localeCompare(b.name));
  return [...groups.values()];
}

function HostSection({ group, onCreate, onDelete }: { group: HostGroup; onCreate: () => void; onDelete: (ws: api.Workspace) => void }): React.ReactElement {
  return (
    <section className="flex min-w-0 flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <ServerIcon className="text-muted-foreground size-4 shrink-0" />
        <h2 className="text-sm font-medium">{group.title}</h2>
        <code className="text-muted-foreground font-mono text-xs">{group.address}</code>
        <Badge variant="outline" className={toneClass(group.ready ? "ok" : "alert")}>
          {group.ready ? t("devd 可用") : t("devd 不可用")}
        </Badge>
        <span className="text-muted-foreground text-xs">{t("共 {n} 个", { n: group.workspaces.length })}</span>
        <CreateInSection onClick={onCreate} />
      </div>
      {group.workspaces.length === 0 ? (
        <p className="text-muted-foreground text-xs">{t("这台工作节点上还没有工作空间。")}</p>
      ) : (
        <div className={GRID}>
          {group.workspaces.map((ws) => (
            <WorkspaceCard key={ws.id} ws={ws} onDelete={() => onDelete(ws)} />
          ))}
        </div>
      )}
    </section>
  );
}

function NoHostsCard(): React.ReactElement {
  return (
    <Card className="items-center gap-2 px-6 py-10 text-center">
      <span className="bg-muted text-muted-foreground flex size-10 items-center justify-center rounded-lg [&>svg]:size-5">
        <ServerIcon />
      </span>
      <p className="text-sm font-medium">{t("还没有工作节点")}</p>
      <p className="text-muted-foreground max-w-md text-xs">{t("工作空间落在工作节点上：请先在主机/SoC 页面以「工作节点」类型纳管一台主机，再回到这里创建。")}</p>
      <Button size="sm" variant="outline" className="mt-2" onClick={() => navigate("/agent-hosts")}>
        {t("前往主机/SoC")}
      </Button>
    </Card>
  );
}

/** 「本设备」一节：早先落在设备数据目录下的创作工作空间（只能管理文件，不能再新建或对话）。 */
function StudioSection({ workspaces, onDelete }: { workspaces: api.Workspace[]; onDelete: (ws: api.Workspace) => void }): React.ReactElement {
  return (
    <section className="flex min-w-0 flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <SparklesIcon className="text-muted-foreground size-4 shrink-0" />
        <h2 className="text-sm font-medium">{t("本设备")}</h2>
        <span className="text-muted-foreground text-xs">{t("早先建在设备上的创作工作空间：可以查看、下载与整理文件，不能再对话；新的创作空间请建在工作节点上")}</span>
        <span className="text-muted-foreground text-xs">{t("共 {n} 个", { n: workspaces.length })}</span>
      </div>
      <div className={GRID}>
        {workspaces.map((ws) => (
          <WorkspaceCard key={ws.id} ws={ws} onDelete={() => onDelete(ws)} />
        ))}
      </div>
    </section>
  );
}

function WorkspacesBody({ data, onCreate, onDelete }: { data: api.WorkspacesSnapshot; onCreate: (kind: api.WorkspaceKind, hostId?: number) => void; onDelete: (ws: api.Workspace) => void }): React.ReactElement {
  const groups = useMemo(() => groupByHost(data), [data]);
  const studios = useMemo(() => data.workspaces.filter((ws) => ws.kind === "studio" && ws.host_id === 0).sort((a, b) => a.name.localeCompare(b.name)), [data]);
  return (
    <div className="flex flex-col gap-6">
      {studios.length > 0 ? <StudioSection workspaces={studios} onDelete={onDelete} /> : null}
      {groups.length === 0 ? (
        <NoHostsCard />
      ) : (
        groups.map((g) => <HostSection key={g.id} group={g} onCreate={() => onCreate("dev", g.id)} onDelete={onDelete} />)
      )}
    </div>
  );
}

export function WorkspacesPage(): React.ReactElement {
  const res = useResource(() => api.listWorkspaces(), []);
  const [dialog, setDialog] = useState<{ kind: "create"; wsKind?: api.WorkspaceKind | undefined; hostId?: number | undefined } | { kind: "delete"; ws: api.Workspace } | null>(null);

  return (
    <PageContainer wide compact>
      <PageHeader
        compact
        title={t("工作空间管理")}
        note={t("两种工作空间都在工作节点上：开发工作空间是项目目录（文件、Git、终端）；创作工作空间是素材目录，智能体在节点上生成与整理图像 / 视频。")}
        actions={
          <Button size="sm" disabled={res.data === null} title={t("创建工作空间")} onClick={() => setDialog({ kind: "create" })}>
            <PlusIcon />
            <span className="hidden sm:inline">{t("创建工作空间")}</span>
          </Button>
        }
        refreshing={res.loading}
        onRefresh={res.reload}
      />
      <ResourceGate resource={res}>
        {(data) => <WorkspacesBody data={data} onCreate={(wsKind, hostId) => setDialog({ kind: "create", wsKind, hostId })} onDelete={(ws) => setDialog({ kind: "delete", ws })} />}
      </ResourceGate>
      {dialog === null || res.data === null ? null : dialog.kind === "create" ? (
        <CreateDialog hosts={res.data.hosts} credentials={res.data.credentials} templates={res.data.studio_templates} initialKind={dialog.wsKind} initialHostId={dialog.hostId} onClose={() => setDialog(null)} onDone={res.reload} />
      ) : (
        <DeleteDialog ws={dialog.ws} onClose={() => setDialog(null)} onDone={res.reload} />
      )}
    </PageContainer>
  );
}
