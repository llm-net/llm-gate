// 创作工作空间的素材库：三个子目录各一页——media/（媒体：管理员上传的素材与智能体生成的图像 /
// 视频 / 音频）、docs/（创作文档：智能体交付的脚本、分镜、文案）、agent/（智能体文档：项目说明与
// 它自己的记录）。顶部是子目录切换与上传（按钮或拖进来；文本文件进当前的文档目录、其余进 media/，
// 与固件的落位规则同源），正文是自适应列数的缩略图网格，每格一个文件（图像 / 视频画缩略图，其余
// 按种类画图标；角标标来源：上传 / 生成 / 智能体），点开看大图 / 播视频与生成信息（提示词、订阅 /
// 模型、参数），可下载、改名 / 挪目录、删除、「引用到指令」（把路径填进输入框，智能体按路径取
// 素材）。设备解不开的图像与视频由页面顺序补预览图；列表始终只读缩略图。

import { FileIcon, FileTextIcon, FilmIcon, Loader2Icon, Music2Icon, UploadIcon } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { SegTabs } from "@/features/access/access";
import { captureImagePreview, captureVideoPreview } from "@/features/studio/preview-image";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";

export function fmtBytes(n: number): string {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GiB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MiB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(0)} KiB`;
  return `${n} B`;
}

/** 子目录的显示名。 */
export function dirLabel(dir: api.StudioDir): string {
  switch (dir) {
    case "media":
      return t("媒体");
    case "docs":
      return t("创作文档");
    default:
      return t("智能体文档");
  }
}

export function originLabel(origin: api.StudioFileOrigin): string {
  switch (origin) {
    case "upload":
      return t("上传");
    case "generated":
      return t("生成");
    case "agent":
      return t("智能体写入");
    default:
      return t("未知来源");
  }
}

function KindIcon({ kind, className }: { kind: api.StudioFileKind; className?: string }): React.ReactElement {
  switch (kind) {
    case "video":
      return <FilmIcon className={className} />;
    case "audio":
      return <Music2Icon className={className} />;
    case "text":
      return <FileTextIcon className={className} />;
    default:
      return <FileIcon className={className} />;
  }
}

function Tile({ wsId, file, onOpen }: { wsId: string; file: api.StudioFile; onOpen: () => void }): React.ReactElement {
  const visual = file.kind === "image" || file.kind === "video";
  const [failedSrc, setFailedSrc] = useState<string | null>(null);
  const { base } = api.studioSplitPath(file.name);
  const src = file.has_thumb ? api.studioThumbURL(wsId, file) : null;
  useEffect(() => { setFailedSrc(null); }, [src]);
  return (
    <button
      type="button"
      onClick={onOpen}
      title={file.prompt !== undefined && file.prompt !== "" ? file.prompt : file.name}
      className="group bg-muted/40 hover:border-ring focus-visible:border-ring focus-visible:ring-ring/50 relative flex aspect-square flex-col overflow-hidden rounded-lg border text-left outline-none transition-colors focus-visible:ring-[3px]"
    >
      {visual && src !== null && failedSrc !== src ? (
        <img src={src} alt={base} loading="lazy" className="size-full object-contain" onError={() => setFailedSrc(src)} />
      ) : (
        <span className="text-muted-foreground flex size-full items-center justify-center">
          <KindIcon kind={file.kind} className="size-8" />
        </span>
      )}
      {file.kind === "video" ? <FilmIcon className="absolute top-1.5 right-1.5 size-4 text-white drop-shadow" aria-hidden="true" /> : null}
      <span className="absolute inset-x-0 bottom-0 flex items-center gap-1 bg-gradient-to-t from-black/70 to-transparent px-1.5 pt-4 pb-1">
        <span className="min-w-0 flex-1 truncate text-[11px] text-white">{base}</span>
        <span className={cn("shrink-0 rounded px-1 text-[9px] text-white", file.origin === "generated" ? "bg-primary/80" : file.origin === "upload" ? "bg-white/25" : "bg-black/40")}>
          {originLabel(file.origin)}
        </span>
      </span>
    </button>
  );
}

function Lightbox({ wsId, file, onClose, onChanged, onCite }: { wsId: string; file: api.StudioFile; onClose: () => void; onChanged: () => void; onCite: ((name: string) => void) | undefined }): React.ReactElement {
  const confirm = useConfirm();
  const { dir, base } = api.studioSplitPath(file.name);
  const [renaming, setRenaming] = useState(false);
  const [newName, setNewName] = useState(base);
  const [busy, setBusy] = useState(false);
  const [text, setText] = useState<string | null>(null);
  const src = api.studioFileURL(wsId, file);
  const params = file.params !== undefined && file.params !== "" ? file.params : null;

  useEffect(() => {
    if (file.kind !== "text" || file.bytes > 256 << 10) return;
    let alive = true;
    fetch(src, { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then((body) => { if (alive) setText(body); }, () => { if (alive) setText(null); });
    return () => {
      alive = false;
    };
  }, [file.kind, file.bytes, src]);

  function remove(): void {
    void confirm({ title: t("删除文件 {name}？", { name: file.name }), body: t("从设备上删除这个文件，不可恢复。"), confirmText: t("删除"), danger: true }).then((ok) => {
      if (!ok) return;
      setBusy(true);
      api.deleteStudioFile(wsId, file.name).then(
        () => {
          toast(t("文件已删除"));
          onChanged();
          onClose();
        },
        (err: unknown) => {
          setBusy(false);
          toast.error(api.errorMessage(err));
        },
      );
    });
  }

  function rename(): void {
    // 只填文件名即在原子目录里改名；填「子目录/文件名」即挪到那个子目录（文本只能在 docs/ 与 agent/ 之间）。
    const typed = newName.trim();
    const target = typed.includes("/") ? typed : `${dir}/${typed}`;
    if (typed === "" || target === file.name) {
      setRenaming(false);
      return;
    }
    setBusy(true);
    api.renameStudioFile(wsId, file.name, target).then(
      () => {
        toast(t("已改名"));
        onChanged();
        onClose();
      },
      (err: unknown) => {
        setBusy(false);
        toast.error(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent className="sm:max-w-3xl">
        <div className="flex flex-col gap-3">
          <DialogHeader>
            <DialogTitle className="break-all">{file.name}</DialogTitle>
          </DialogHeader>
          <div className="bg-muted/40 flex items-center justify-center rounded-lg border">
            {file.kind === "image" ? (
              <img src={src} alt={base} className="max-h-[60vh] w-auto max-w-full object-contain" />
            ) : file.kind === "video" ? (
              <video src={src} controls playsInline className="max-h-[60vh] w-full" />
            ) : file.kind === "audio" ? (
              <audio src={src} controls className="w-full p-4" />
            ) : file.kind === "text" && text !== null ? (
              <pre className="max-w-full overflow-x-auto p-3 font-mono text-xs whitespace-pre-wrap">{text}</pre>
            ) : (
              <span className="text-muted-foreground flex items-center gap-2 p-8 text-sm">
                <KindIcon kind={file.kind} className="size-6" />
                {t("无法预览，请下载查看")}
              </span>
            )}
          </div>
          <div className="text-muted-foreground flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
            {dir !== "" ? <Badge variant="outline">{dirLabel(dir)}</Badge> : null}
            <Badge variant="outline">{originLabel(file.origin)}</Badge>
            <span>{fmtBytes(file.bytes)}</span>
            {file.width !== undefined && file.width > 0 ? <span>{file.width}×{file.height}</span> : null}
            {file.provider !== undefined && file.provider !== "" ? <span className="font-mono">{file.provider} / {file.model}</span> : null}
            <span>{fmtTime(file.created_at)}</span>
          </div>
          {file.prompt !== undefined && file.prompt !== "" ? (
            <div className="rounded-md border px-3 py-2">
              <p className="text-muted-foreground mb-1 text-[11px]">{t("生成提示词")}</p>
              <p className="text-sm whitespace-pre-wrap">{file.prompt}</p>
              {params !== null ? <p className="text-muted-foreground mt-1 font-mono text-[11px] break-all">{params}</p> : null}
            </div>
          ) : null}
          {renaming ? (
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="studio-rename">{t("新文件名")}</Label>
              <Input id="studio-rename" value={newName} disabled={busy} autoComplete="off" spellCheck={false} onChange={(e) => setNewName(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); rename(); } }} />
              <p className="text-muted-foreground text-[11px]">
                {file.kind === "text" ? t("只填文件名即在 {dir}/ 里改名；填 docs/… 或 agent/… 可挪到另一个文档目录。", { dir }) : t("媒体文件只能留在 media/ 里改名。")}
              </p>
            </div>
          ) : null}
          <DialogFooter className="flex-wrap">
            {onCite !== undefined ? (
              <Button type="button" variant="outline" disabled={busy} onClick={() => { onCite(file.name); onClose(); }}>
                {t("引用到指令")}
              </Button>
            ) : null}
            <Button type="button" variant="outline" disabled={busy} asChild>
              <a href={api.studioDownloadURL(wsId, file.name)} download={base}>{t("下载")}</a>
            </Button>
            {renaming ? (
              <Button type="button" disabled={busy} onClick={rename}>
                {busy ? <Loader2Icon className="animate-spin" /> : null}
                {t("确认改名")}
              </Button>
            ) : (
              <Button type="button" variant="outline" disabled={busy} onClick={() => setRenaming(true)}>
                {t("改名")}
              </Button>
            )}
            <Button type="button" variant="destructive" disabled={busy} onClick={remove}>
              {t("删除")}
            </Button>
          </DialogFooter>
        </div>
      </DialogContent>
    </Dialog>
  );
}

/**
 * 素材库面板：三个子目录各一页，缺省停在 media/。openName / onOpenChange 可选：给了就由外面控制打开
 * 哪个文件（时间线里点生成结果，路径带子目录），不给就自己管。
 */
export function FilesPanel({ wsId, files, loading, onChanged, onCite, openName, onOpenChange }: {
  wsId: string;
  files: api.StudioFile[];
  loading: boolean;
  onChanged: () => void;
  onCite?: ((name: string) => void) | undefined;
  openName?: string | null | undefined;
  onOpenChange?: ((name: string | null) => void) | undefined;
}): React.ReactElement {
  const [ownOpen, setOwnOpen] = useState<string | null>(null);
  const openedName = openName === undefined ? ownOpen : openName;
  const setOpen = useCallback(
    (f: api.StudioFile | null) => {
      setOwnOpen(f === null ? null : f.name);
      onOpenChange?.(f === null ? null : f.name);
    },
    [onOpenChange],
  );
  const [dir, setDir] = useState<api.StudioDir>("media");
  const [uploading, setUploading] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const captured = useRef(new Set<string>());
  const onChangedRef = useRef(onChanged);
  useEffect(() => { onChangedRef.current = onChanged; }, [onChanged]);

  // 上传去处：文件种类允许就进当前子目录，否则按种类落到 docs/（文本）或 media/（其余）。
  const upload = useCallback(
    (list: FileList | File[]) => {
      const items = Array.from(list);
      if (items.length === 0) return;
      void (async () => {
        let failed = 0;
        let redirected = 0;
        for (const f of items) {
          const target: api.StudioDir = api.studioDirAllows(dir, f.name) ? dir : api.studioIsTextName(f.name) ? "docs" : "media";
          if (target !== dir) redirected++;
          setUploading(f.name);
          try {
            await api.uploadStudioFile(wsId, `${target}/${f.name}`, f);
          } catch (err) {
            failed++;
            toast.error(`${f.name}: ${api.errorMessage(err)}`);
          }
        }
        setUploading(null);
        if (failed < items.length) toast(t("已上传 {n} 个文件", { n: items.length - failed }));
        if (redirected > 0) toast(t("{n} 个文件按种类放进了其它子目录（文本进 docs/，媒体进 media/）。", { n: redirected }));
        onChanged();
      })();
    },
    [wsId, dir, onChanged],
  );

  // 一次只处理一个原件。按文件版本去重，同名覆盖会重新补图；清单变更或离开空间即取消。
  // 图像先让设备按需补缩略图，只有 404 才尝试浏览器解码，避免把可由设备处理的大图拉到页面。
  // 补好一张先刷新清单，再处理下一张，避免刷新取消下一次捕获并重复下载原件。
  useEffect(() => {
    const controller = new AbortController();
    const { signal } = controller;
    const liveKeys = new Set(files.filter((file) => !file.has_thumb).map((file) => api.studioFileURL(wsId, file)));
    for (const key of captured.current) if (!liveKeys.has(key)) captured.current.delete(key);
    void (async () => {
      for (const file of files) {
        if (signal.aborted) break;
        const src = api.studioFileURL(wsId, file);
        if (file.has_thumb || captured.current.has(src) || (file.kind !== "image" && file.kind !== "video")) continue;
        try {
          if (file.kind === "image") {
            const response = await fetch(api.studioThumbURL(wsId, file), { credentials: "same-origin", signal });
            if (response.ok) {
              await response.arrayBuffer();
              if (signal.aborted) break;
              captured.current.add(src);
              onChangedRef.current();
              return;
            }
            if (response.status !== 404) { captured.current.add(src); continue; }
          }
          const blob = await (file.kind === "image" ? captureImagePreview(src, signal) : captureVideoPreview(src, signal));
          if (signal.aborted) break;
          if (blob !== null) {
            await api.uploadStudioThumb(wsId, file.name, blob, file.source_revision, signal);
            if (signal.aborted) break;
            captured.current.add(src);
            onChangedRef.current();
            return;
          }
          captured.current.add(src);
        } catch {
          if (!signal.aborted) captured.current.add(src);
        }
      }
    })();
    return () => controller.abort();
  }, [files, wsId]);

  // 网格里的文件对象随清单刷新：打开的那个跟着换。
  const current = openedName === null ? null : (files.find((f) => f.name === openedName) ?? null);
  const counts: Record<api.StudioDir, number> = { agent: 0, docs: 0, media: 0 };
  for (const f of files) {
    const d = api.studioSplitPath(f.name).dir;
    if (d !== "") counts[d]++;
  }
  const shown = files.filter((f) => api.studioSplitPath(f.name).dir === dir);

  return (
    <div
      className={cn("flex min-h-0 flex-1 flex-col gap-2", dragging && "ring-ring/50 rounded-lg ring-[3px]")}
      onDragOver={(e) => { e.preventDefault(); setDragging(true); }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => { e.preventDefault(); setDragging(false); upload(e.dataTransfer.files); }}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs font-medium">{t("素材库")}</span>
        {loading ? <Loader2Icon className="text-muted-foreground size-3 animate-spin" /> : null}
        <input ref={inputRef} type="file" multiple hidden onChange={(e) => { if (e.target.files) upload(e.target.files); e.target.value = ""; }} />
        <Button size="sm" variant="outline" className="ml-auto" disabled={uploading !== null} onClick={() => inputRef.current?.click()}>
          {uploading !== null ? <Loader2Icon className="animate-spin" /> : <UploadIcon />}
          {uploading !== null ? t("上传中 {name}", { name: uploading }) : dir === "media" ? t("上传素材") : t("上传文档")}
        </Button>
      </div>
      <SegTabs
        size="sm"
        items={(["media", "docs", "agent"] as const).map((d) => ({ key: d, label: dirLabel(d), count: counts[d] }))}
        active={dir}
        onSelect={(k) => setDir(k as api.StudioDir)}
      />
      <div className="min-h-0 flex-1 overflow-y-auto">
        {shown.length === 0 ? (
          <div className="text-muted-foreground flex h-full min-h-32 flex-col items-center justify-center gap-1 rounded-lg border border-dashed p-4 text-center text-xs">
            <UploadIcon className="size-5" />
            {dir === "media" ? (
              <>
                <p>{t("media/ 还没有文件：上传参考图、产品图或视频素材，或直接让智能体生成。")}</p>
                <p>{t("可以把文件拖到这里。")}</p>
              </>
            ) : dir === "docs" ? (
              <p>{t("docs/ 还没有文件：智能体交付的脚本、分镜、文案会放在这里，也可以上传文本文件。")}</p>
            ) : (
              <p>{t("agent/ 还没有文件：智能体在这里维护项目说明 agent/PROJECT.md 与自己的记录。")}</p>
            )}
          </div>
        ) : (
          <div className="grid grid-cols-[repeat(auto-fill,minmax(7rem,1fr))] gap-2">
            {shown.map((f) => (
              <Tile key={f.name} wsId={wsId} file={f} onOpen={() => setOpen(f)} />
            ))}
          </div>
        )}
      </div>
      {current !== null ? <Lightbox wsId={wsId} file={current} onClose={() => setOpen(null)} onChanged={onChanged} onCite={onCite} /> : null}
    </div>
  );
}

/** 时间线使用素材库的缩略图状态，补图完成后出现，文件删除后隐藏。 */
export function GeneratedThumb({ wsId, name, file, onOpen }: { wsId: string; name: string; file: api.StudioFile | undefined; onOpen: (name: string) => void }): React.ReactElement | null {
  const [failedSrc, setFailedSrc] = useState<string | null>(null);
  const src = file?.has_thumb ? api.studioThumbURL(wsId, file) : null;
  useEffect(() => { setFailedSrc(null); }, [src]);
  if (src === null || failedSrc === src) return null;
  return (
    <button type="button" className="hover:border-ring relative overflow-hidden rounded-md border" title={name} onClick={() => onOpen(name)}>
      <img src={src} alt={name} loading="lazy" className="h-24 w-auto max-w-48 object-contain" onError={() => setFailedSrc(src)} />
      {file?.kind === "video" ? <FilmIcon className="absolute top-1 right-1 size-3.5 text-white drop-shadow" aria-hidden="true" /> : null}
    </button>
  );
}
