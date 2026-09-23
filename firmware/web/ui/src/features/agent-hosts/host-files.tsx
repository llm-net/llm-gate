// Agent远控页侧栏的「文件」页签：以纳管用的 SSH 用户身份只读浏览这台主机。
//
// 上半是当前目录一层的清单（面包屑、可直接输入路径跳转、按名字筛选），点目录进去、点文件在
// 下半预览：图片整份取回直接显示；其余取开头一段，Markdown 可切换渲染 / 源码，二进制只给元数据
// 与下载。与分界可拖动。不写、不改、不删——改主机是智能体的事。
//
// 读数只在首次切到本页签、进目录、点文件与按刷新时发生（每次设备现连一次主机），不轮询；
// 切走页签不卸载，回来还是原来的目录与预览。

import { ChevronRight, CornerDownLeft, Download, File, FileImage, FileText, Folder, FolderSymlink, House, LoaderCircle, Pencil, RefreshCw, X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable";
import { SegTabs } from "@/features/access/access";
import { Markdown } from "@/features/agentchat/shared";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { fmtBytes } from "@/lib/units";

const IMAGE_EXT = new Set([".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".ico", ".avif", ".svg"]);
const MARKDOWN_EXT = new Set([".md", ".markdown"]);

function extOf(name: string): string {
  const i = name.lastIndexOf(".");
  return i <= 0 ? "" : name.slice(i).toLowerCase();
}

/** 绝对路径拆成面包屑；家目录以内的前缀折成 ~。 */
function crumbs(path: string, home: string | undefined): { label: string; path: string }[] {
  const out: { label: string; path: string }[] = [{ label: "/", path: "/" }];
  let cur = "";
  for (const seg of path.split("/").filter((s) => s !== "")) {
    cur += `/${seg}`;
    out.push({ label: seg, path: cur });
  }
  if (home !== undefined && home !== "" && home !== "/" && (path === home || path.startsWith(`${home}/`))) {
    const homeSegs = home.split("/").filter((s) => s !== "").length;
    return [{ label: "~", path: home }, ...out.slice(homeSegs + 1)];
  }
  return out;
}

function EntryIcon({ entry }: { entry: api.HostFileEntry }): React.ReactElement {
  const cls = "size-3.5 shrink-0";
  if (entry.dir) return entry.symlink ? <FolderSymlink className={cn(cls, "text-primary")} /> : <Folder className={cn(cls, "text-primary")} />;
  if (IMAGE_EXT.has(extOf(entry.name))) return <FileImage className={cn(cls, "text-muted-foreground")} />;
  if (MARKDOWN_EXT.has(extOf(entry.name)) || extOf(entry.name) === ".txt") return <FileText className={cn(cls, "text-muted-foreground")} />;
  return <File className={cn(cls, "text-muted-foreground")} />;
}

// ---- 目录清单 ----

function PathBar({ listing, loading, onNavigate, onReload }: { listing: api.HostFileListing | null; loading: boolean; onNavigate: (path: string) => void; onReload: () => void }): React.ReactElement {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const dir = listing?.path ?? "";
  return (
    <div className="flex items-center gap-1 border-b px-2 py-1.5">
      {editing ? (
        <form
          className="flex min-w-0 flex-1 items-center gap-1"
          onSubmit={(e) => {
            e.preventDefault();
            const p = draft.trim();
            if (p === "") return;
            setEditing(false);
            onNavigate(p === "~" ? "" : p);
          }}
        >
          <Input
            autoFocus
            value={draft}
            spellCheck={false}
            autoComplete="off"
            aria-label={t("前往路径")}
            placeholder="/etc"
            className="h-7 flex-1 font-mono text-xs"
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") setEditing(false);
            }}
          />
          <Button type="submit" size="icon-xs" variant="ghost" title={t("前往")} aria-label={t("前往")}>
            <CornerDownLeft />
          </Button>
          <Button type="button" size="icon-xs" variant="ghost" title={t("取消")} aria-label={t("取消")} onClick={() => setEditing(false)}>
            <X />
          </Button>
        </form>
      ) : (
        <>
          <nav aria-label={t("当前目录")} className="flex min-w-0 flex-1 flex-wrap items-center gap-0.5 text-xs">
            {crumbs(dir, listing?.home).map((c, i, arr) => (
              <span key={c.path} className="flex items-center gap-0.5">
                <button type="button" className={cn("hover:bg-muted rounded px-1 py-0.5 font-mono", i === arr.length - 1 && "font-medium")} onClick={() => onNavigate(c.path)}>
                  {c.label}
                </button>
                {i < arr.length - 1 ? <ChevronRight className="text-muted-foreground size-3" /> : null}
              </span>
            ))}
          </nav>
          <Button size="icon-xs" variant="ghost" title={t("输入路径")} aria-label={t("输入路径")} onClick={() => { setDraft(dir); setEditing(true); }}>
            <Pencil />
          </Button>
          <Button size="icon-xs" variant="ghost" title={t("家目录")} aria-label={t("家目录")} onClick={() => onNavigate("")}>
            <House />
          </Button>
        </>
      )}
      <Button size="icon-xs" variant="ghost" title={t("刷新")} aria-label={t("刷新")} onClick={onReload}>
        <RefreshCw className={cn(loading && "animate-spin")} />
      </Button>
    </div>
  );
}

function FileList({
  listing,
  loading,
  error,
  selected,
  onNavigate,
  onOpen,
}: {
  listing: api.HostFileListing | null;
  loading: boolean;
  error: string | null;
  selected: string | null;
  onNavigate: (path: string) => void;
  onOpen: (entry: api.HostFileEntry) => void;
}): React.ReactElement {
  const [filter, setFilter] = useState("");
  // 换目录时清掉筛选。
  const dir = listing?.path;
  useEffect(() => setFilter(""), [dir]);
  if (listing === null) {
    return (
      <div className="text-muted-foreground flex flex-1 items-center justify-center p-3 text-xs">
        {loading ? <LoaderCircle className="size-4 animate-spin" /> : error !== null ? <p role="alert" className="text-destructive">{error}</p> : null}
      </div>
    );
  }
  const needle = filter.trim().toLowerCase();
  const entries = needle === "" ? listing.entries : listing.entries.filter((e) => e.name.toLowerCase().includes(needle));
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {error === null ? null : <p role="alert" className="text-destructive border-b px-3 py-1.5 text-xs">{error}</p>}
      <div className="border-b px-2 py-1.5">
        <Input value={filter} onChange={(e) => setFilter(e.target.value)} placeholder={t("按名称筛选")} aria-label={t("按名称筛选")} className="h-7 text-xs" spellCheck={false} autoComplete="off" />
      </div>
      <ul className="min-h-0 flex-1 overflow-y-auto text-xs">
        {listing.parent === undefined || needle !== "" ? null : (
          <li>
            <button type="button" className="hover:bg-muted flex w-full items-center gap-2 px-3 py-1 text-left font-mono" onClick={() => onNavigate(listing.parent ?? "/")}>
              ..
            </button>
          </li>
        )}
        {entries.length === 0 ? <li className="text-muted-foreground px-3 py-2">{needle === "" ? t("空目录") : t("没有匹配的条目")}</li> : null}
        {entries.map((entry) => (
          <li key={entry.path}>
            <button
              type="button"
              className={cn("hover:bg-muted flex w-full min-w-0 items-center gap-2 px-3 py-1 text-left", selected === entry.path && "bg-muted")}
              title={`${entry.path}\n${entry.mode}${entry.owner === undefined ? "" : ` ${entry.owner}`} · ${fmtTime(entry.mod_time)}`}
              onClick={() => (entry.dir ? onNavigate(entry.path) : onOpen(entry))}
            >
              <EntryIcon entry={entry} />
              <span className={cn("min-w-0 flex-1 truncate font-mono", entry.dir && "font-medium")}>
                {entry.name}
                {entry.symlink ? <span className="text-muted-foreground"> →</span> : null}
              </span>
              {entry.dir ? null : <span className="text-muted-foreground shrink-0 text-[10px] tabular-nums">{fmtBytes(entry.size)}</span>}
            </button>
          </li>
        ))}
        {listing.truncated ? <li className="text-muted-foreground px-3 py-2">{t("条目太多，只列出了前 {n} 项。", { n: listing.entries.length })}</li> : null}
      </ul>
    </div>
  );
}

// ---- 预览 ----

function FilePreview({ hostId, entry, onClose }: { hostId: number; entry: api.HostFileEntry; onClose: () => void }): React.ReactElement {
  const image = IMAGE_EXT.has(extOf(entry.name));
  const markdown = MARKDOWN_EXT.has(extOf(entry.name));
  const tooLarge = entry.size > api.HOST_FILE_RAW_LIMIT;
  const [preview, setPreview] = useState<api.HostFilePreview | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [rendered, setRendered] = useState(true);
  const [imageBroken, setImageBroken] = useState(false);
  const [nonce, setNonce] = useState(0);

  useEffect(() => {
    setPreview(null);
    setError(null);
    setImageBroken(false);
    if (image) return;
    const ctrl = new AbortController();
    setLoading(true);
    api.previewHostFile(hostId, entry.path, { signal: ctrl.signal }).then(
      (p) => {
        setPreview(p);
        setLoading(false);
      },
      (err: unknown) => {
        if (ctrl.signal.aborted) return;
        setError(api.errorMessage(err));
        setLoading(false);
      },
    );
    return () => ctrl.abort();
  }, [hostId, entry.path, image, nonce]);

  const size = preview?.size ?? entry.size;
  const rawURL = api.hostFileRawURL(hostId, entry.path);

  let body: React.ReactNode;
  if (image) {
    body = tooLarge ? (
      <p className="text-muted-foreground p-3 text-xs">{t("图片超过 {n} MB，不在这里预览，请下载查看。", { n: api.HOST_FILE_RAW_LIMIT >> 20 })}</p>
    ) : imageBroken ? (
      <p className="text-muted-foreground p-3 text-xs">{t("图片读取失败：可能没有读取权限，或不是浏览器能显示的格式。")}</p>
    ) : (
      <div className="flex min-h-full items-center justify-center p-3">
        <img key={nonce} src={`${rawURL}&n=${nonce}`} alt={entry.name} className="max-w-full object-contain" onError={() => setImageBroken(true)} />
      </div>
    );
  } else if (error !== null) {
    body = <p role="alert" className="text-destructive p-3 text-xs">{error}</p>;
  } else if (preview === null) {
    body = loading ? (
      <div className="flex h-full items-center justify-center p-3">
        <LoaderCircle className="text-muted-foreground size-4 animate-spin" />
      </div>
    ) : null;
  } else if (preview.binary) {
    body = <p className="text-muted-foreground p-3 text-xs">{t("这是二进制文件（{size}），不显示内容。", { size: fmtBytes(size) })}</p>;
  } else if (preview.content === "") {
    body = <p className="text-muted-foreground p-3 text-xs">{t("（空文件）")}</p>;
  } else if (markdown && rendered) {
    body = <div className="p-3"><Markdown text={preview.content} /></div>;
  } else {
    body = <pre className="p-3 font-mono text-[11px] leading-5 break-all whitespace-pre-wrap">{preview.content}</pre>;
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-1.5 border-b px-2 py-1.5 text-xs">
        <code className="min-w-0 flex-1 truncate font-mono" title={entry.path}>{entry.name}</code>
        {markdown && preview !== null && !preview.binary ? (
          <SegTabs
            size="sm"
            items={[
              { key: "rendered", label: t("渲染") },
              { key: "source", label: t("源码") },
            ]}
            active={rendered ? "rendered" : "source"}
            onSelect={(k) => setRendered(k === "rendered")}
          />
        ) : null}
        <Button size="icon-xs" variant="ghost" title={t("刷新")} aria-label={t("刷新")} onClick={() => setNonce((n) => n + 1)}>
          <RefreshCw className={cn(loading && "animate-spin")} />
        </Button>
        {tooLarge ? (
          <Button size="icon-xs" variant="ghost" disabled title={t("文件超过 {n} MB，不能在这里下载", { n: api.HOST_FILE_RAW_LIMIT >> 20 })} aria-label={t("下载")}>
            <Download />
          </Button>
        ) : (
          <Button size="icon-xs" variant="ghost" asChild title={t("下载")} aria-label={t("下载")}>
            <a href={api.hostFileRawURL(hostId, entry.path, true)} download={entry.name}>
              <Download />
            </a>
          </Button>
        )}
        <Button size="icon-xs" variant="ghost" title={t("关闭预览")} aria-label={t("关闭预览")} onClick={onClose}>
          <X />
        </Button>
      </div>
      <div className="text-muted-foreground flex flex-wrap gap-x-3 gap-y-0.5 border-b px-3 py-1 text-[10px]">
        <span className="font-mono">{preview?.mode ?? entry.mode}</span>
        {entry.owner === undefined ? null : <span className="font-mono">{entry.owner}</span>}
        <span className="tabular-nums">{fmtBytes(size)}</span>
        <span className="tabular-nums">{fmtTime(preview?.mod_time ?? entry.mod_time)}</span>
        {preview?.truncated ? <Badge variant="secondary" className="h-4 px-1 text-[10px]">{t("只显示开头 {n} MB", { n: api.HOST_FILE_PREVIEW_LIMIT >> 20 })}</Badge> : null}
      </div>
      <div className="min-h-0 flex-1 overflow-auto">{body}</div>
    </div>
  );
}

// ---- 页签 ----

export function HostFilesPanel({ hostId, className }: { hostId: number; className?: string }): React.ReactElement {
  const [listing, setListing] = useState<api.HostFileListing | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<api.HostFileEntry | null>(null);
  const seq = useRef(0);

  const load = useCallback(
    (path: string) => {
      const mine = ++seq.current;
      setLoading(true);
      api.listHostFiles(hostId, path).then(
        (l) => {
          if (mine !== seq.current) return;
          setListing(l);
          setError(null);
          setLoading(false);
        },
        (err: unknown) => {
          if (mine !== seq.current) return;
          // 进不去的目录：留在原地，只提示原因。
          setError(api.errorMessage(err));
          setLoading(false);
        },
      );
    },
    [hostId],
  );

  // 挂载即读家目录（页面在首次切到本页签时才挂载它）。
  useEffect(() => {
    setListing(null);
    setSelected(null);
    load("");
  }, [load]);

  const list = (
    <div className="flex h-full min-h-0 flex-col">
      <PathBar listing={listing} loading={loading} onNavigate={load} onReload={() => load(listing?.path ?? "")} />
      <FileList listing={listing} loading={loading} error={error} selected={selected?.path ?? null} onNavigate={load} onOpen={setSelected} />
    </div>
  );

  return (
    <div className={cn("flex min-h-0 flex-1 flex-col gap-2", className)}>
      <span className="text-muted-foreground text-xs">{t("以纳管用户的身份只读浏览这台主机；改动请交给智能体。")}</span>
      <div className="min-h-0 flex-1 overflow-hidden rounded-md border">
        {/* 清单面板恒在同一位置，打开 / 关闭预览只增减后一个面板：清单不重挂，筛选与滚动位置都留着。 */}
        <ResizablePanelGroup orientation="vertical" className="h-full">
          <ResizablePanel id="list" defaultSize="40%" minSize="5rem" className="min-h-0">
            {list}
          </ResizablePanel>
          {selected === null ? null : (
            <>
              <ResizableHandle withHandle title={t("拖动调整两栏大小")} className="bg-border" />
              <ResizablePanel id="preview" defaultSize="60%" minSize="6rem" className="min-h-0">
                <FilePreview hostId={hostId} entry={selected} onClose={() => setSelected(null)} />
              </ResizablePanel>
            </>
          )}
        </ResizablePanelGroup>
      </div>
    </div>
  );
}
