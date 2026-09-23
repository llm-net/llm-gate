// 智能体 · 模型服务：一台模型服务节点上守护进程 modeld 管的东西——算力服务器、任务队列、模型
// 缓存、对外 API 与引擎会话。主机 id 在查询串 ?host=<id>，从「主机/SoC」表那一行的「模型服务」
// 进入；只有 model_service 类型的主机有这一页（设备答 409）。
//
// 六张卡：
//   - modeld：读数在主机行里（安装 / 检查 / 卸载走 /devd 端点，与工具配置页同一套对话框）。
//     没装 modeld 时其余卡不显示——它们的读数全在守护进程里。
//   - 算力服务器：登记地址、承载的模型与并发槽位；守护进程每 30 秒探活，行里是在线 / 忙闲 / 计数。
//   - 任务队列：按优先级排队、按「支持该模型、有空槽、负载最低」派发；可手工提交、取消、重试。
//   - 模型缓存：节点上的一块目录，从 URL 拉取（可核 SHA-256），算力服务器经对外 API 取用。
//   - 对外 API：监听地址与 Bearer 令牌（明文只在签发那一次显示）。
//   - 引擎会话：直接在节点上拉起 Codex App Server / Claude Code；模型调用经本设备、以所选密钥结算，
//     密钥由设备解封后经 SSH 交给守护进程、只落实例目录。打开一段会话后按 seq 陪等事件。
//
// 零轮询：读数只在进页 / 刷新 / 动作之后重取一次；会话事件的陪等（wait=1，守护进程最多等 25 秒）
// 是与 Agent远控同类的人发起陪等例外，关掉会话面板即中止。

import { ArrowLeftIcon, Loader2Icon } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { accessTargets, defaultTarget } from "@/features/access/access";
import { DevdDialog, type DevdMode, type Tone, devdBadge, toneClass } from "@/features/agent-hosts/devd";
import * as api from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { useConfirm } from "@/lib/confirm";
import { fmtTime, fmtTimeSeconds } from "@/lib/format";
import { t } from "@/lib/i18n";
import { navigate, useLocationSearch } from "@/lib/router";
import { fmtBytes } from "@/lib/units";
import { useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

type Run = (name: string, fn: () => Promise<unknown>, after?: () => void) => void;

function Fact({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  return (
    <span className="flex items-center gap-1">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </span>
  );
}

function SectionCard({
  title,
  badge,
  actions,
  children,
}: {
  title: string;
  badge?: { label: string; tone: Tone };
  actions?: React.ReactNode;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <Card className="gap-1 px-3 py-2">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
        <span className="text-sm font-medium">{title}</span>
        {badge === undefined ? null : (
          <Badge variant={badge.tone === "none" ? "secondary" : "outline"} className={badge.tone === "none" ? "" : toneClass(badge.tone)}>
            {badge.label}
          </Badge>
        )}
        <div className="ml-auto flex flex-wrap gap-1.5">{actions}</div>
      </div>
      {children}
    </Card>
  );
}

function ErrorLine({ error }: { error: string | null }): React.ReactElement | null {
  if (error === null) return null;
  return (
    <p role="alert" className="text-destructive text-sm">
      {error}
    </p>
  );
}

// ---- 算力服务器 ----

function backendBadge(b: api.ModelBackend): { label: string; tone: Tone } {
  if (!b.enabled) return { label: t("已停用"), tone: "none" };
  if (b.status === "error") return { label: t("不可达"), tone: "alert" };
  if (b.status === "ready") return b.active >= b.slots ? { label: t("满载"), tone: "warn" } : { label: t("在线"), tone: "ok" };
  return { label: t("未探测"), tone: "none" };
}

function BackendDialog({
  hostId,
  backend,
  onClose,
  onDone,
}: {
  hostId: number;
  backend: api.ModelBackend | null;
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const [name, setName] = useState(backend?.name ?? "");
  const [base, setBase] = useState(backend?.base_url ?? "");
  const [models, setModels] = useState(backend?.models.join(", ") ?? "");
  const [slots, setSlots] = useState(String(backend?.slots ?? 1));
  const [note, setNote] = useState(backend?.note ?? "");
  const [enabled, setEnabled] = useState(backend?.enabled ?? true);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    const parsedSlots = Number(slots);
    if (!Number.isInteger(parsedSlots) || parsedSlots <= 0) {
      setError(t("并发槽位须是正整数"));
      return;
    }
    setBusy(true);
    setError(null);
    const input: api.ModelBackendInput = {
      name: name.trim(),
      base_url: base.trim(),
      models: models.split(/[,\s]+/).map((m) => m.trim()).filter((m) => m !== ""),
      slots: parsedSlots,
      enabled,
      note: note.trim(),
    };
    const call = backend === null ? api.createModelBackend(hostId, input) : api.updateModelBackend(hostId, backend.id, input);
    call.then(
      () => {
        toast(backend === null ? t("算力服务器已登记") : t("算力服务器已更新"));
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
            <DialogTitle>{backend === null ? t("登记算力服务器") : t("修改算力服务器")}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="bk-base">{t("地址")}</Label>
            <Input id="bk-base" value={base} required disabled={busy} autoComplete="off" spellCheck={false} placeholder="http://10.0.0.21:9000"
              onChange={(e) => setBase(e.target.value)} />
            <p className="text-muted-foreground text-xs">{t("算力服务器须提供 GET /healthz 与 POST/GET/DELETE /v1/tasks，契约见文档。")}</p>
          </div>
          <div className="flex gap-3">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="bk-name">{t("名称")}</Label>
              <Input id="bk-name" value={name} disabled={busy} autoComplete="off" placeholder={t("留空即用地址")} onChange={(e) => setName(e.target.value)} />
            </div>
            <div className="flex w-28 flex-col gap-1.5">
              <Label htmlFor="bk-slots">{t("并发槽位")}</Label>
              <Input id="bk-slots" value={slots} required disabled={busy} inputMode="numeric" onChange={(e) => setSlots(e.target.value)} />
            </div>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="bk-models">{t("承载的模型")}</Label>
            <Input id="bk-models" value={models} disabled={busy} autoComplete="off" spellCheck={false} placeholder="h3-video, h3-context-ir"
              onChange={(e) => setModels(e.target.value)} />
            <p className="text-muted-foreground text-xs">{t("逗号分隔；留空表示任何模型都接。")}</p>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="bk-note">{t("备注")}</Label>
            <Input id="bk-note" value={note} disabled={busy} autoComplete="off" onChange={(e) => setNote(e.target.value)} />
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={enabled} disabled={busy} onChange={(e) => setEnabled(e.target.checked)} />
            {t("启用（参与调度）")}
          </label>
          <ErrorLine error={error} />
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{t("取消")}</Button>
            <Button type="submit" disabled={busy}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {backend === null ? t("登记") : t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function BackendsCard({
  hostId,
  backends,
  busy,
  run,
  onEdit,
}: {
  hostId: number;
  backends: api.ModelBackend[];
  busy: string | null;
  run: Run;
  onEdit: (b: api.ModelBackend | null) => void;
}): React.ReactElement {
  const confirm = useConfirm();
  return (
    <SectionCard
      title={t("算力服务器")}
      actions={
        <Button size="sm" onClick={() => onEdit(null)}>{t("登记算力服务器")}</Button>
      }
    >
      <div className="divide-y">
        {backends.length === 0 ? (
          <div className="text-muted-foreground flex h-12 items-center text-xs">{t("还没有登记算力服务器")}</div>
        ) : (
          backends.map((b) => {
            const badge = backendBadge(b);
            return (
              <div key={b.id} className="flex flex-col gap-1 py-2">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
                  <span className="text-sm font-medium">{b.name}</span>
                  <code className="text-muted-foreground font-mono text-xs">{b.base_url}</code>
                  <Badge variant={badge.tone === "none" ? "secondary" : "outline"} className={badge.tone === "none" ? "" : toneClass(badge.tone)}>
                    {badge.label}
                  </Badge>
                  <div className="ml-auto flex flex-wrap gap-1.5">
                    <Button size="sm" variant="outline" disabled={busy !== null}
                      onClick={() => run(`bk-check-${b.id}`, () => api.checkModelBackend(hostId, b.id))}>
                      {busy === `bk-check-${b.id}` ? <Loader2Icon className="animate-spin" /> : null}
                      {t("探活")}
                    </Button>
                    <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => onEdit(b)}>{t("修改")}</Button>
                    <Button size="sm" variant="outline" disabled={busy !== null}
                      onClick={() => {
                        void confirm({ title: t("移除算力服务器？"), body: <p>{b.name}</p>, confirmText: t("移除"), danger: true }).then((ok) => {
                          if (ok) run(`bk-del-${b.id}`, () => api.deleteModelBackend(hostId, b.id));
                        });
                      }}>
                      {t("移除")}
                    </Button>
                  </div>
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                  <Fact label={t("模型")}>
                    <span>{b.models.length === 0 ? t("任意") : b.models.join(", ")}</span>
                  </Fact>
                  <Fact label={t("占用 / 槽位")}>
                    <span>{b.active} / {b.slots}</span>
                  </Fact>
                  <Fact label={t("已完成 / 失败")}>
                    <span>{b.completed} / {b.failed}</span>
                  </Fact>
                  {b.latency_ms === undefined || b.latency_ms === 0 ? null : (
                    <Fact label={t("延迟")}>
                      <span>{b.latency_ms} ms</span>
                    </Fact>
                  )}
                  <Fact label={t("最近探活")}>
                    <span className="whitespace-nowrap">{b.checked_at === undefined ? "—" : fmtTime(b.checked_at)}</span>
                  </Fact>
                  {b.note === undefined || b.note === "" ? null : <span className="text-muted-foreground">{b.note}</span>}
                  {b.last_error === undefined || b.last_error === "" ? null : <span className="text-signal-alert basis-full">{b.last_error}</span>}
                </div>
              </div>
            );
          })
        )}
      </div>
    </SectionCard>
  );
}

// ---- 任务队列 ----

function taskBadge(s: api.ModelTaskStatus): { label: string; tone: Tone } {
  switch (s) {
    case "queued":
      return { label: t("排队中"), tone: "none" };
    case "running":
      return { label: t("执行中"), tone: "warn" };
    case "succeeded":
      return { label: t("成功"), tone: "ok" };
    case "failed":
      return { label: t("失败"), tone: "alert" };
    default:
      return { label: t("已取消"), tone: "none" };
  }
}

function TaskDialog({
  hostId,
  models,
  onClose,
  onDone,
}: {
  hostId: number;
  models: string[];
  onClose: () => void;
  onDone: () => void;
}): React.ReactElement {
  const [model, setModel] = useState(models[0] ?? "");
  const [input, setInput] = useState("{\n  \"prompt\": \"\"\n}");
  const [priority, setPriority] = useState("0");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    let parsed: unknown;
    try {
      parsed = input.trim() === "" ? {} : JSON.parse(input);
    } catch {
      setError(t("入参不是合法 JSON"));
      return;
    }
    setBusy(true);
    setError(null);
    api.createModelTask(hostId, { model: model.trim(), input: parsed, priority: Number(priority) || 0 }).then(
      () => {
        toast(t("任务已入队"));
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
            <DialogTitle>{t("提交任务")}</DialogTitle>
          </DialogHeader>
          <div className="flex gap-3">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="task-model">{t("模型")}</Label>
              <Input id="task-model" value={model} required disabled={busy} autoComplete="off" spellCheck={false} list="task-model-list"
                onChange={(e) => setModel(e.target.value)} />
              <datalist id="task-model-list">
                {models.map((m) => <option key={m} value={m} />)}
              </datalist>
            </div>
            <div className="flex w-28 flex-col gap-1.5">
              <Label htmlFor="task-priority">{t("优先级")}</Label>
              <Input id="task-priority" value={priority} disabled={busy} inputMode="numeric" onChange={(e) => setPriority(e.target.value)} />
            </div>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="task-input">{t("入参（JSON）")}</Label>
            <Textarea id="task-input" value={input} disabled={busy} rows={8} className="font-mono text-xs" spellCheck={false}
              onChange={(e) => setInput(e.target.value)} />
            <p className="text-muted-foreground text-xs">{t("原样交给算力服务器的 POST /v1/tasks；数字越大越先派发。")}</p>
          </div>
          <ErrorLine error={error} />
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{t("取消")}</Button>
            <Button type="submit" disabled={busy}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {t("入队")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function short(v: unknown, max = 200): string {
  if (v === undefined || v === null) return "";
  const s = typeof v === "string" ? v : JSON.stringify(v);
  return s.length > max ? `${s.slice(0, max)}…` : s;
}

function QueueCard({
  hostId,
  tasks,
  stats,
  busy,
  run,
  onSubmit,
}: {
  hostId: number;
  tasks: api.ModelTask[];
  stats: api.ModelQueueStats;
  busy: string | null;
  run: Run;
  onSubmit: () => void;
}): React.ReactElement {
  const finished = tasks.some((x) => x.status === "succeeded" || x.status === "failed" || x.status === "cancelled");
  return (
    <SectionCard
      title={t("任务队列")}
      actions={
        <>
          {finished ? (
            <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => run("tasks-clear", () => api.clearModelTasks(hostId))}>
              {t("清除已结束")}
            </Button>
          ) : null}
          <Button size="sm" onClick={onSubmit}>{t("提交任务")}</Button>
        </>
      }
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        <Fact label={t("排队")}><span>{stats.queued}</span></Fact>
        <Fact label={t("执行中")}><span>{stats.running}</span></Fact>
        <Fact label={t("成功 / 失败 / 取消")}><span>{stats.succeeded} / {stats.failed} / {stats.cancelled}</span></Fact>
        <Fact label={t("占用 / 总槽位")}><span>{stats.busy} / {stats.capacity}</span></Fact>
      </div>
      <div className="divide-y">
        {tasks.length === 0 ? (
          <div className="text-muted-foreground flex h-12 items-center text-xs">{t("没有任务")}</div>
        ) : (
          tasks.map((x) => {
            const badge = taskBadge(x.status);
            const active = x.status === "queued" || x.status === "running";
            return (
              <div key={x.id} className="flex flex-col gap-1 py-2">
                <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
                  <code className="font-mono text-xs">{x.id}</code>
                  <span className="text-sm">{x.model}</span>
                  <Badge variant={badge.tone === "none" ? "secondary" : "outline"} className={badge.tone === "none" ? "" : toneClass(badge.tone)}>
                    {badge.label}
                    {x.status === "queued" && x.position !== undefined ? ` #${x.position}` : ""}
                  </Badge>
                  <Badge variant="secondary">{x.source === "api" ? "API" : t("管理台")}</Badge>
                  <div className="ml-auto flex flex-wrap gap-1.5">
                    {active ? (
                      <Button size="sm" variant="outline" disabled={busy !== null}
                        onClick={() => run(`task-cancel-${x.id}`, () => api.cancelModelTask(hostId, x.id))}>
                        {busy === `task-cancel-${x.id}` ? <Loader2Icon className="animate-spin" /> : null}
                        {t("取消")}
                      </Button>
                    ) : (
                      <Button size="sm" variant="outline" disabled={busy !== null}
                        onClick={() => run(`task-retry-${x.id}`, () => api.retryModelTask(hostId, x.id))}>
                        {t("重试")}
                      </Button>
                    )}
                  </div>
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                  {x.backend_name === undefined || x.backend_name === "" ? null : (
                    <Fact label={t("算力服务器")}><span>{x.backend_name}</span></Fact>
                  )}
                  <Fact label={t("尝试")}><span>{x.attempts}</span></Fact>
                  <Fact label={t("提交")}><span className="whitespace-nowrap">{fmtTimeSeconds(x.created_at)}</span></Fact>
                  {x.finished_at === undefined ? null : (
                    <Fact label={t("结束")}><span className="whitespace-nowrap">{fmtTimeSeconds(x.finished_at)}</span></Fact>
                  )}
                  {x.output === undefined ? null : (
                    <code className="text-muted-foreground basis-full truncate font-mono" title={short(x.output, 2000)}>{short(x.output)}</code>
                  )}
                  {x.error === undefined || x.error === "" ? null : <span className="text-signal-alert basis-full">{x.error}</span>}
                </div>
              </div>
            );
          })
        )}
      </div>
    </SectionCard>
  );
}

// ---- 模型缓存 ----

function PullDialog({ hostId, onClose, onDone }: { hostId: number; onClose: () => void; onDone: () => void }): React.ReactElement {
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");
  const [sha, setSha] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    setBusy(true);
    setError(null);
    api.pullModelCache(hostId, { url: url.trim(), name: name.trim(), sha256: sha.trim() }).then(
      () => {
        toast(t("已开始拉取，进度在模型缓存卡片里"));
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
            <DialogTitle>{t("拉取模型文件")}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="pull-url">{t("下载地址")}</Label>
            <Input id="pull-url" value={url} required disabled={busy} autoComplete="off" spellCheck={false} placeholder="https://…/model.safetensors"
              onChange={(e) => setUrl(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="pull-name">{t("保存为")}</Label>
            <Input id="pull-name" value={name} disabled={busy} autoComplete="off" spellCheck={false} placeholder={t("留空即用地址里的文件名")}
              onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="pull-sha">{t("SHA-256（可选）")}</Label>
            <Input id="pull-sha" value={sha} disabled={busy} autoComplete="off" spellCheck={false} className="font-mono" onChange={(e) => setSha(e.target.value)} />
            <p className="text-muted-foreground text-xs">{t("填了就在下载完成后核对，不符即丢弃。")}</p>
          </div>
          <ErrorLine error={error} />
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{t("取消")}</Button>
            <Button type="submit" disabled={busy}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {t("开始拉取")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function CacheCard({
  hostId,
  cache,
  busy,
  run,
  onPull,
}: {
  hostId: number;
  cache: { files: api.ModelCacheFile[]; stats: api.ModelCacheStats; pulls: api.ModelPull[] };
  busy: string | null;
  run: Run;
  onPull: () => void;
}): React.ReactElement {
  const confirm = useConfirm();
  const pulls = cache.pulls.filter((p) => p.status === "running" || p.status === "failed");
  return (
    <SectionCard title={t("模型缓存")} actions={<Button size="sm" onClick={onPull}>{t("拉取模型文件")}</Button>}>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        <Fact label={t("目录")}><code className="font-mono">{cache.stats.dir}</code></Fact>
        <Fact label={t("文件")}><span>{cache.stats.files}</span></Fact>
        <Fact label={t("占用")}><span>{fmtBytes(cache.stats.bytes)}</span></Fact>
        <Fact label={t("剩余空间")}><span>{cache.stats.free_bytes === 0 ? "—" : fmtBytes(cache.stats.free_bytes)}</span></Fact>
      </div>
      {pulls.length === 0 ? null : (
        <div className="flex flex-col gap-1 text-xs">
          {pulls.map((p) => (
            <div key={p.id} className="flex flex-wrap items-center gap-x-2 gap-y-1">
              <code className="font-mono">{p.name}</code>
              {p.status === "running" ? (
                <>
                  <span className="text-muted-foreground">
                    {fmtBytes(p.received)}{p.total > 0 ? ` / ${fmtBytes(p.total)}` : ""}
                  </span>
                  <div className="bg-muted h-1.5 w-40 overflow-hidden rounded">
                    <div className="bg-primary h-full" style={{ width: p.total > 0 ? `${Math.min(100, (p.received / p.total) * 100)}%` : "30%" }} />
                  </div>
                </>
              ) : (
                <span className="text-signal-alert">{p.error}</span>
              )}
              <Button size="sm" variant="outline" className="ml-auto" disabled={busy !== null}
                onClick={() => run(`pull-cancel-${p.id}`, () => api.cancelModelPull(hostId, p.id))}>
                {p.status === "running" ? t("取消") : t("清除")}
              </Button>
            </div>
          ))}
        </div>
      )}
      <div className="divide-y">
        {cache.files.length === 0 ? (
          <div className="text-muted-foreground flex h-12 items-center text-xs">{t("缓存里还没有文件")}</div>
        ) : (
          cache.files.map((f) => (
            <div key={f.name} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-1.5 text-xs">
              <code className="font-mono text-sm">{f.name}</code>
              <span>{fmtBytes(f.size)}</span>
              <span className="text-muted-foreground whitespace-nowrap">{fmtTime(f.modified_at)}</span>
              {f.sha256 === undefined ? null : <code className="text-muted-foreground font-mono">{f.sha256.slice(0, 12)}…</code>}
              <Button size="sm" variant="outline" className="ml-auto" disabled={busy !== null}
                onClick={() => {
                  void confirm({ title: t("删除缓存文件？"), body: <code className="font-mono">{f.name}</code>, confirmText: t("删除"), danger: true }).then((ok) => {
                    if (ok) run(`cache-del-${f.name}`, () => api.deleteModelCacheFile(hostId, f.name));
                  });
                }}>
                {t("删除")}
              </Button>
            </div>
          ))
        )}
      </div>
    </SectionCard>
  );
}

// ---- 对外 API ----

function ApiCard({
  hostId,
  status,
  tokens,
  busy,
  run,
}: {
  hostId: number;
  status: api.ModelApiStatus;
  tokens: api.ModelToken[];
  busy: string | null;
  run: Run;
}): React.ReactElement {
  const confirm = useConfirm();
  const [listen, setListen] = useState(status.listen);
  const [tokenName, setTokenName] = useState("");
  const [plaintext, setPlaintext] = useState<string | null>(null);
  useEffect(() => setListen(status.listen), [status.listen]);
  const badge: { label: string; tone: Tone } = status.running
    ? { label: t("已监听"), tone: "ok" }
    : status.error !== undefined && status.error !== ""
      ? { label: t("监听失败"), tone: "alert" }
      : { label: t("未开启"), tone: "none" };
  return (
    <SectionCard title={t("对外 API")} badge={badge}>
      <form
        className="flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          run("api-listen", () => api.setModelApiListen(hostId, listen.trim()).then(() => toast(t("监听地址已保存"))));
        }}
      >
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="api-listen">{t("监听地址")}</Label>
          <Input id="api-listen" value={listen} disabled={busy !== null} autoComplete="off" spellCheck={false} className="w-56 font-mono" placeholder="0.0.0.0:8790"
            onChange={(e) => setListen(e.target.value)} />
        </div>
        <Button type="submit" size="sm" variant="outline" disabled={busy !== null || listen.trim() === status.listen}>
          {busy === "api-listen" ? <Loader2Icon className="animate-spin" /> : null}
          {t("保存")}
        </Button>
        <span className="text-muted-foreground text-xs">{t("留空即关闭对外 API。客户端与算力服务器凭下面的令牌访问 /api/v1/*。")}</span>
        {status.error === undefined || status.error === "" ? null : <span className="text-signal-alert basis-full text-xs">{status.error}</span>}
      </form>
      <div className="flex flex-col gap-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium">{t("令牌")}</span>
          <form
            className="ml-auto flex items-center gap-1.5"
            onSubmit={(e) => {
              e.preventDefault();
              run("token-create", () => api.createModelToken(hostId, tokenName.trim()).then((r) => {
                setPlaintext(r.plaintext);
                setTokenName("");
              }));
            }}
          >
            <Input value={tokenName} disabled={busy !== null} autoComplete="off" placeholder={t("令牌名称")} className="h-8 w-40"
              onChange={(e) => setTokenName(e.target.value)} />
            <Button type="submit" size="sm" disabled={busy !== null}>{t("签发令牌")}</Button>
          </form>
        </div>
        {plaintext === null ? null : (
          <div className="bg-muted/40 flex flex-wrap items-center gap-2 rounded-md border px-2 py-1.5 text-xs">
            <span className="text-signal-alert">{t("令牌明文只显示这一次，请立即复制：")}</span>
            <code className="font-mono break-all">{plaintext}</code>
            <Button size="sm" variant="outline" className="ml-auto" onClick={() => { void copyText(plaintext).then(() => toast(t("已复制"))); }}>
              {t("复制")}
            </Button>
            <Button size="sm" variant="outline" onClick={() => setPlaintext(null)}>{t("知道了")}</Button>
          </div>
        )}
        <div className="divide-y">
          {tokens.length === 0 ? (
            <div className="text-muted-foreground flex h-10 items-center text-xs">{t("还没有签发令牌；对外 API 在签发之前拒绝一切请求。")}</div>
          ) : (
            tokens.map((k) => (
              <div key={k.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 py-1.5 text-xs">
                <span className="text-sm">{k.name}</span>
                <code className="text-muted-foreground font-mono">{k.prefix}…</code>
                <Fact label={t("签发")}><span className="whitespace-nowrap">{fmtTime(k.created_at)}</span></Fact>
                <Fact label={t("最近使用")}><span className="whitespace-nowrap">{k.last_used_at === undefined ? "—" : fmtTime(k.last_used_at)}</span></Fact>
                <Button size="sm" variant="outline" className="ml-auto" disabled={busy !== null}
                  onClick={() => {
                    void confirm({ title: t("吊销令牌？"), body: <p>{k.name}</p>, confirmText: t("吊销"), danger: true }).then((ok) => {
                      if (ok) run(`token-del-${k.id}`, () => api.deleteModelToken(hostId, k.id));
                    });
                  }}>
                  {t("吊销")}
                </Button>
              </div>
            ))
          )}
        </div>
      </div>
    </SectionCard>
  );
}

// ---- 引擎会话 ----

const EFFORTS: Record<api.ModelEngine, string[]> = {
  codex: ["low", "medium", "high", "xhigh"],
  claude: ["low", "medium", "high", "xhigh", "max"],
};

function SessionDialog({
  hostId,
  engines,
  onClose,
  onDone,
}: {
  hostId: number;
  engines: api.ModelEngineInfo[];
  onClose: () => void;
  onDone: (s: api.ModelSession) => void;
}): React.ReactElement {
  const ready = engines.filter((e) => e.ready);
  const [engine, setEngine] = useState<api.ModelEngine>(ready[0]?.id ?? "codex");
  const [keys, setKeys] = useState<api.ApiKey[] | null>(null);
  const [keyID, setKeyID] = useState("");
  const [bases, setBases] = useState<{ base: string; label: string }[]>([]);
  const [base, setBase] = useState("");
  const [model, setModel] = useState("");
  const [effort, setEffort] = useState("");
  const [workdir, setWorkdir] = useState("");
  const [instructions, setInstructions] = useState("");
  const [title, setTitle] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.listKeys().then(
      (r) => setKeys(r.keys.filter((k) => k.plaintext_available && !k.disabled)),
      (err: unknown) => setError(api.errorMessage(err)),
    );
    api.getEndpoints().then(
      (snap) => {
        const targets = accessTargets(snap.endpoints);
        const opts: { base: string; label: string }[] = [];
        for (const target of targets) {
          for (const line of target.lines) opts.push({ base: line.base, label: `${target.label} · ${line.tag}` });
        }
        setBases(opts);
        const def = defaultTarget(targets);
        setBase(opts.find((o) => o.label.startsWith(def.label))?.base ?? opts[0]?.base ?? "");
      },
      (err: unknown) => setError(api.errorMessage(err)),
    );
  }, []);

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    if (keyID === "") {
      setError(t("请选一把密钥"));
      return;
    }
    if (base === "") {
      setError(t("请选一个节点访问本设备的地址"));
      return;
    }
    setBusy(true);
    setError(null);
    api.createModelSession(hostId, {
      engine, key_id: Number(keyID), base_url: base, model: model.trim(), effort, workdir: workdir.trim(),
      instructions: instructions.trim(), title: title.trim(),
    }).then(
      (r) => {
        toast(t("引擎会话已启动"));
        onDone(r.session);
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
            <DialogTitle>{t("新建引擎会话")}</DialogTitle>
          </DialogHeader>
          <div className="flex gap-3">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label>{t("引擎")}</Label>
              <Select value={engine} disabled={busy} onValueChange={(v) => setEngine(v === "claude" ? "claude" : "codex")}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  {engines.map((e) => (
                    <SelectItem key={e.id} value={e.id} disabled={!e.ready}>
                      {e.label}{e.ready ? "" : ` · ${t("节点上没有")}`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="flex flex-1 flex-col gap-1.5">
              <Label>{t("API 密钥")}</Label>
              <Select value={keyID} disabled={busy || keys === null} onValueChange={setKeyID}>
                <SelectTrigger><SelectValue placeholder={t("选一把密钥")} /></SelectTrigger>
                <SelectContent>
                  {(keys ?? []).map((k) => (
                    <SelectItem key={k.id} value={String(k.id)}>{k.label === "" ? `${k.display_prefix}…${k.display_last4}` : k.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label>{t("节点访问本设备的地址")}</Label>
            <Select value={base} disabled={busy || bases.length === 0} onValueChange={setBase}>
              <SelectTrigger><SelectValue placeholder={t("选一个地址")} /></SelectTrigger>
              <SelectContent>
                {bases.map((o) => <SelectItem key={o.base} value={o.base}>{o.label} · {o.base}</SelectItem>)}
              </SelectContent>
            </Select>
            <p className="text-muted-foreground text-xs">{t("引擎的模型调用打到这个地址的开发工具接入面，用量记在所选密钥名下；密钥由设备解封后只落节点上的实例目录。")}</p>
          </div>
          <div className="flex gap-3">
            <div className="flex flex-1 flex-col gap-1.5">
              <Label htmlFor="ses-model">{t("模型")}</Label>
              <Input id="ses-model" value={model} disabled={busy} autoComplete="off" spellCheck={false} placeholder={t("留空即缺省")} onChange={(e) => setModel(e.target.value)} />
            </div>
            <div className="flex w-36 flex-col gap-1.5">
              <Label>{t("推理档位")}</Label>
              <Select value={effort === "" ? "default" : effort} disabled={busy} onValueChange={(v) => setEffort(v === "default" ? "" : v)}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="default">{t("缺省")}</SelectItem>
                  {EFFORTS[engine].map((e) => <SelectItem key={e} value={e}>{e}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="ses-workdir">{t("工作目录")}</Label>
            <Input id="ses-workdir" value={workdir} disabled={busy} autoComplete="off" spellCheck={false} className="font-mono" placeholder={t("留空即守护进程用户的家目录")}
              onChange={(e) => setWorkdir(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="ses-instructions">{t("开发者指令")}</Label>
            <Textarea id="ses-instructions" value={instructions} disabled={busy} rows={4} onChange={(e) => setInstructions(e.target.value)} />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="ses-title">{t("标题")}</Label>
            <Input id="ses-title" value={title} disabled={busy} autoComplete="off" placeholder={t("留空即用第一条指令")} onChange={(e) => setTitle(e.target.value)} />
          </div>
          <ErrorLine error={error} />
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{t("取消")}</Button>
            <Button type="submit" disabled={busy || ready.length === 0}>
              {busy ? <Loader2Icon className="animate-spin" /> : null}
              {busy ? t("启动中…") : t("启动会话")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function sessionBadge(s: api.ModelSession["status"]): { label: string; tone: Tone } {
  switch (s) {
    case "running":
      return { label: t("执行中"), tone: "warn" };
    case "idle":
      return { label: t("空闲"), tone: "ok" };
    case "closed":
      return { label: t("已关闭"), tone: "none" };
    default:
      return { label: t("启动中"), tone: "none" };
  }
}

function eventLabel(e: api.ModelEvent): string {
  switch (e.type) {
    case "user":
      return t("指令");
    case "message":
      return t("回复");
    case "reasoning":
      return t("推理");
    case "activity":
      return t("动态");
    case "command":
      return t("命令");
    case "file":
      return t("文件");
    case "usage":
      return t("用量");
    case "error":
      return t("错误");
    case "done":
      return t("结束");
    default:
      return t("系统");
  }
}

/** 一段会话的事件面板：进面板先读一次，然后按 seq 陪等（wait=1）；离开即中止。 */
function SessionPanel({
  hostId,
  session,
  onClose,
  onClosed,
}: {
  hostId: number;
  session: api.ModelSession;
  onClose: () => void;
  onClosed: () => void;
}): React.ReactElement {
  const [view, setView] = useState(session);
  const [events, setEvents] = useState<api.ModelEvent[]>([]);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const bottom = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    let alive = true;
    let after = 0;
    const loop = async (): Promise<void> => {
      let wait = false;
      while (alive) {
        try {
          const r = await api.getModelSessionEvents(hostId, session.id, after, wait, ctrl.signal);
          if (!alive) return;
          setView(r.session);
          const last = r.events[r.events.length - 1];
          if (last !== undefined) {
            after = last.seq;
            setEvents((prev) => [...prev, ...r.events]);
          }
          if (r.session.status === "closed") return;
          wait = true;
        } catch (err) {
          if (!alive || ctrl.signal.aborted) return;
          toast.error(api.errorMessage(err));
          await new Promise((resolve) => setTimeout(resolve, 3000));
        }
      }
    };
    void loop();
    return () => {
      alive = false;
      ctrl.abort();
    };
  }, [hostId, session.id]);

  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [events.length, view.partial]);

  function send(event: React.FormEvent): void {
    event.preventDefault();
    if (text.trim() === "") return;
    setBusy(true);
    api.sendModelSessionTurn(hostId, session.id, text).then(
      () => {
        setBusy(false);
        setText("");
      },
      (err: unknown) => {
        setBusy(false);
        toast.error(api.errorMessage(err));
      },
    );
  }

  const badge = sessionBadge(view.status);
  return (
    <Card className="gap-2 px-3 py-2">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1.5">
        <Button size="icon-sm" variant="ghost" aria-label={t("返回会话列表")} onClick={onClose}>
          <ArrowLeftIcon />
        </Button>
        <span className="text-sm font-medium">{view.title === undefined || view.title === "" ? view.id : view.title}</span>
        <Badge variant="secondary">{view.engine === "claude" ? "Claude Code" : "Codex"}</Badge>
        <Badge variant={badge.tone === "none" ? "secondary" : "outline"} className={badge.tone === "none" ? "" : toneClass(badge.tone)}>{badge.label}</Badge>
        <span className="text-muted-foreground text-xs">{t("{i} 入 / {o} 出 token", { i: view.input_tokens, o: view.output_tokens })}</span>
        <div className="ml-auto flex flex-wrap gap-1.5">
          {view.status === "running" ? (
            <Button size="sm" variant="outline" onClick={() => api.interruptModelSession(hostId, session.id).catch((err: unknown) => toast.error(api.errorMessage(err)))}>
              {t("中止")}
            </Button>
          ) : null}
          {view.status === "closed" ? null : (
            <Button size="sm" variant="outline" onClick={() => api.closeModelSession(hostId, session.id).then(onClosed, (err: unknown) => toast.error(api.errorMessage(err)))}>
              {t("结束会话")}
            </Button>
          )}
        </div>
      </div>
      <div className="flex flex-col gap-1.5 text-xs">
        {events.map((e) => (
          <div key={e.seq} className="flex gap-2">
            <span className="text-muted-foreground w-10 shrink-0">{eventLabel(e)}</span>
            <div className="min-w-0 flex-1">
              {e.type === "usage" ? (
                <span className="text-muted-foreground">{t("{i} 入 / {o} 出 token", { i: e.input_tokens ?? 0, o: e.output_tokens ?? 0 })}</span>
              ) : e.type === "done" ? (
                <span className={e.outcome === "completed" ? "text-signal-ok" : "text-signal-alert"}>{e.outcome}{e.text === undefined || e.text === "" ? "" : ` · ${e.text}`}</span>
              ) : e.type === "command" ? (
                <div className="flex flex-col gap-0.5">
                  <code className="font-mono break-all">{e.text}</code>
                  {e.exit_code === undefined ? null : <span className="text-muted-foreground">{t("退出码 {code}", { code: e.exit_code })}</span>}
                  {e.output === undefined || e.output === "" ? null : <pre className="bg-muted/40 max-w-full overflow-x-auto rounded p-1 font-mono whitespace-pre-wrap">{e.output}</pre>}
                </div>
              ) : (
                <span className={e.type === "error" ? "text-signal-alert whitespace-pre-wrap" : "whitespace-pre-wrap"}>{e.text}</span>
              )}
            </div>
          </div>
        ))}
        {view.partial === undefined || view.partial === "" ? null : (
          <div className="flex gap-2">
            <span className="text-muted-foreground w-10 shrink-0">{t("回复")}</span>
            <span className="text-muted-foreground whitespace-pre-wrap">{view.partial}</span>
          </div>
        )}
        <div ref={bottom} />
      </div>
      {view.status === "closed" ? null : (
        <form className="flex items-end gap-2" onSubmit={send}>
          <Textarea value={text} rows={2} disabled={busy || view.status !== "idle"} placeholder={t("给引擎的指令")} onChange={(e) => setText(e.target.value)} />
          <Button type="submit" size="sm" disabled={busy || view.status !== "idle" || text.trim() === ""}>{t("发送")}</Button>
        </form>
      )}
    </Card>
  );
}

function EnginesCard({
  hostId,
  engines,
  sessions,
  busy,
  onNew,
  onOpen,
  run,
}: {
  hostId: number;
  engines: api.ModelEngineInfo[];
  sessions: api.ModelSession[];
  busy: string | null;
  onNew: () => void;
  onOpen: (s: api.ModelSession) => void;
  run: Run;
}): React.ReactElement {
  return (
    <SectionCard
      title={t("引擎会话")}
      actions={<Button size="sm" disabled={!engines.some((e) => e.ready)} onClick={onNew}>{t("新建引擎会话")}</Button>}
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
        {engines.map((e) => (
          <Fact key={e.id} label={e.label}>
            {e.ready ? <code className="font-mono">{e.binary}</code> : <span className="text-muted-foreground">{t("未安装（去工具配置页用 gate 安装）")}</span>}
          </Fact>
        ))}
      </div>
      <div className="divide-y">
        {sessions.length === 0 ? (
          <div className="text-muted-foreground flex h-12 items-center text-xs">{t("没有会话")}</div>
        ) : (
          sessions.map((s) => {
            const badge = sessionBadge(s.status);
            return (
              <div key={s.id} className="flex flex-wrap items-center gap-x-2 gap-y-1 py-2 text-xs">
                <button type="button" className="text-sm font-medium hover:underline" onClick={() => onOpen(s)}>
                  {s.title === undefined || s.title === "" ? s.id : s.title}
                </button>
                <Badge variant="secondary">{s.engine === "claude" ? "Claude Code" : "Codex"}</Badge>
                <Badge variant={badge.tone === "none" ? "secondary" : "outline"} className={badge.tone === "none" ? "" : toneClass(badge.tone)}>{badge.label}</Badge>
                {s.model === undefined || s.model === "" ? null : <code className="text-muted-foreground font-mono">{s.model}</code>}
                <code className="text-muted-foreground font-mono">{s.workdir}</code>
                <span className="text-muted-foreground whitespace-nowrap">{fmtTime(s.last_active_at)}</span>
                <div className="ml-auto flex gap-1.5">
                  <Button size="sm" variant="outline" onClick={() => onOpen(s)}>{t("打开")}</Button>
                  <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => run(`ses-close-${s.id}`, () => api.closeModelSession(hostId, s.id))}>
                    {t("结束")}
                  </Button>
                </div>
              </div>
            );
          })
        )}
      </div>
    </SectionCard>
  );
}

// ---- 页面 ----

interface Board {
  hosts: api.AgentHostsSnapshot;
  host: api.AgentHost | undefined;
  summary: api.ModelSummary | null;
  tasks: { tasks: api.ModelTask[]; stats: api.ModelQueueStats } | null;
  cache: { files: api.ModelCacheFile[]; stats: api.ModelCacheStats; pulls: api.ModelPull[] } | null;
  apiInfo: { api: api.ModelApiStatus; tokens: api.ModelToken[] } | null;
  sessions: { sessions: api.ModelSession[]; engines: api.ModelEngineInfo[] } | null;
}

async function loadBoard(id: number): Promise<Board> {
  const hosts = await api.getAgentHosts();
  const host = hosts.hosts.find((h) => h.id === id);
  const board: Board = { hosts, host, summary: null, tasks: null, cache: null, apiInfo: null, sessions: null };
  if (host === undefined || host.kind !== "model_service" || host.devd === undefined) return board;
  const [summary, tasks, cache, apiInfo, sessions] = await Promise.all([
    api.getModelSummary(id), api.listModelTasks(id), api.getModelCache(id), api.getModelApi(id), api.listModelSessions(id),
  ]);
  return { ...board, summary, tasks, cache, apiInfo, sessions };
}

export function HostModelPage(): React.ReactElement {
  const search = useLocationSearch();
  const id = Number(search.get("host"));
  const res = useResource(() => loadBoard(id), [id]);
  const [busy, setBusy] = useState<string | null>(null);
  const [devdAction, setDevdAction] = useState<DevdMode | null>(null);
  const [backendDialog, setBackendDialog] = useState<{ backend: api.ModelBackend | null } | null>(null);
  const [taskDialog, setTaskDialog] = useState(false);
  const [pullDialog, setPullDialog] = useState(false);
  const [sessionDialog, setSessionDialog] = useState(false);
  const [openSession, setOpenSession] = useState<api.ModelSession | null>(null);

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

  const host = res.data?.host;
  const hostName = host === undefined ? "" : host.name === "" ? host.address : host.name;
  const back = (
    <Button size="icon-sm" variant="ghost" aria-label={t("返回主机/SoC")} onClick={() => navigate("/agent-hosts")}>
      <ArrowLeftIcon />
    </Button>
  );

  return (
    <PageContainer wide compact>
      <PageHeader compact leading={back} title={hostName === "" ? t("模型服务") : t("模型服务 · {name}", { name: hostName })} refreshing={res.loading} onRefresh={res.reload} />
      <ResourceGate resource={res}>
        {(data) => {
          const h = data.host;
          if (h === undefined) {
            return <Card className="text-muted-foreground px-4 py-6 text-sm">{t("主机不存在")}</Card>;
          }
          if (h.kind !== "model_service") {
            return <Card className="text-muted-foreground px-4 py-6 text-sm">{t("只有模型服务节点才有这一页")}</Card>;
          }
          const daemonVersion = data.hosts.model_daemon_version;
          const devdState = devdBadge(h.devd, daemonVersion);
          const canInstall = daemonVersion !== undefined && daemonVersion !== "";
          const info = data.summary?.info;
          return (
            <>
              <SectionCard
                title="modeld"
                badge={devdState}
                actions={
                  <>
                    <Button size="sm" variant="outline" disabled={busy !== null || !canInstall || h.status !== "ready"} onClick={() => setDevdAction("install")}>
                      {h.devd === undefined ? t("安装 modeld") : t("重新安装 modeld")}
                    </Button>
                    {h.devd === undefined ? null : (
                      <>
                        <Button size="sm" variant="outline" disabled={busy !== null}
                          onClick={() => run("devd-check", () => api.checkDevd(h.id).then((r) => {
                            toast(r.host.devd?.status === "ready" ? t("modeld 连接正常") : t("modeld 检查未通过"));
                            return r;
                          }))}>
                          {busy === "devd-check" ? <Loader2Icon className="animate-spin" /> : null}
                          {t("检查 modeld")}
                        </Button>
                        <Button size="sm" variant="outline" disabled={busy !== null} onClick={() => setDevdAction("uninstall")}>{t("卸载 modeld")}</Button>
                      </>
                    )}
                    <Button size="sm" variant="outline" onClick={() => navigate("/host-tools", { search: `?host=${h.id}` })}>{t("工具配置")}</Button>
                  </>
                }
              >
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                  <Fact label={t("版本")}><code className="font-mono">{h.devd?.version === undefined || h.devd.version === "" ? "—" : h.devd.version}</code></Fact>
                  <Fact label={t("固件内嵌")}><code className="font-mono">{canInstall ? daemonVersion : "—"}</code></Fact>
                  {info === undefined ? null : (
                    <>
                      <Fact label={t("状态目录")}><code className="font-mono">{info.state_dir}</code></Fact>
                      <Fact label={t("启动于")}><span className="whitespace-nowrap">{fmtTime(info.started_at)}</span></Fact>
                    </>
                  )}
                  <Fact label={t("最近检查")}>
                    <span className="whitespace-nowrap">{h.devd?.last_checked_at === undefined || h.devd.last_checked_at === "" ? "—" : fmtTime(h.devd.last_checked_at)}</span>
                  </Fact>
                  {h.devd?.last_error === undefined || h.devd.last_error === "" ? null : <span className="text-signal-alert basis-full">{h.devd.last_error}</span>}
                  {h.devd === undefined ? <span className="text-muted-foreground basis-full">{t("装上 modeld 之后才有算力服务器、任务队列、模型缓存、对外 API 与引擎会话。")}</span> : null}
                </div>
              </SectionCard>
              {data.summary === null || data.tasks === null || data.cache === null || data.apiInfo === null || data.sessions === null ? null : openSession !== null ? (
                <SessionPanel hostId={h.id} session={openSession} onClose={() => { setOpenSession(null); res.reload(); }} onClosed={() => { setOpenSession(null); res.reload(); }} />
              ) : (
                <>
                  <BackendsCard hostId={h.id} backends={data.summary.backends} busy={busy} run={run} onEdit={(backend) => setBackendDialog({ backend })} />
                  <QueueCard hostId={h.id} tasks={data.tasks.tasks} stats={data.tasks.stats} busy={busy} run={run} onSubmit={() => setTaskDialog(true)} />
                  <CacheCard hostId={h.id} cache={data.cache} busy={busy} run={run} onPull={() => setPullDialog(true)} />
                  <ApiCard hostId={h.id} status={data.apiInfo.api} tokens={data.apiInfo.tokens} busy={busy} run={run} />
                  <EnginesCard hostId={h.id} engines={data.sessions.engines} sessions={data.sessions.sessions} busy={busy} run={run}
                    onNew={() => setSessionDialog(true)} onOpen={setOpenSession} />
                </>
              )}
              {devdAction === null ? null : <DevdDialog mode={devdAction} host={h} onClose={() => setDevdAction(null)} onDone={res.reload} />}
              {backendDialog === null ? null : <BackendDialog hostId={h.id} backend={backendDialog.backend} onClose={() => setBackendDialog(null)} onDone={res.reload} />}
              {taskDialog ? (
                <TaskDialog hostId={h.id} models={Array.from(new Set((data.summary?.backends ?? []).flatMap((b) => b.models)))} onClose={() => setTaskDialog(false)} onDone={res.reload} />
              ) : null}
              {pullDialog ? <PullDialog hostId={h.id} onClose={() => setPullDialog(false)} onDone={res.reload} /> : null}
              {sessionDialog && data.sessions !== null ? (
                <SessionDialog hostId={h.id} engines={data.sessions.engines} onClose={() => setSessionDialog(false)} onDone={(s) => { setOpenSession(s); res.reload(); }} />
              ) : null}
            </>
          );
        }}
      </ResourceGate>
    </PageContainer>
  );
}
