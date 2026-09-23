// 工作空间文件目录：当前目录一层的清单（不是整棵树——远端目录可以很大，一次只取一层），
// 面包屑上下切换，右键菜单式的行内动作（新建 / 改名 / 删除）。读数只在进目录、动作之后重取。

import { ChevronRightIcon, FilePlusIcon, FolderPlusIcon, RefreshCwIcon } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { RowActionsMenu } from "@/features/upstreams/row-actions";
import type * as api from "@/lib/api";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { t } from "@/lib/i18n";

function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

function joinPath(dir: string, name: string): string {
  return dir.endsWith("/") ? dir + name : `${dir}/${name}`;
}

/** 把绝对路径拆成面包屑：每一段带它自己的完整路径。 */
function crumbs(path: string, home: string | undefined): { label: string; path: string }[] {
  const out: { label: string; path: string }[] = [{ label: "/", path: "/" }];
  let cur = "";
  for (const seg of path.split("/").filter((s) => s !== "")) {
    cur += `/${seg}`;
    out.push({ label: seg, path: cur });
  }
  if (home !== undefined && path.startsWith(home)) {
    // 家目录以内的路径把前缀折成 ~，面包屑短一截。
    const homeSegs = home.split("/").filter((s) => s !== "").length;
    return [{ label: "~", path: home }, ...out.slice(homeSegs + 1)];
  }
  return out;
}

function NameDialog({
  title,
  label,
  initial,
  submitText,
  onSubmit,
  onClose,
}: {
  title: string;
  label: string;
  initial: string;
  submitText: string;
  onSubmit: (name: string) => Promise<unknown>;
  onClose: () => void;
}): React.ReactElement {
  const [name, setName] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !busy) onClose(); }}>
      <DialogContent>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault();
            const trimmed = name.trim();
            if (trimmed === "" || trimmed.includes("/")) {
              setError(t("名称不能为空，也不能包含 /"));
              return;
            }
            setBusy(true);
            setError(null);
            onSubmit(trimmed).then(onClose, (err: unknown) => {
              setBusy(false);
              setError(errorMessage(err));
            });
          }}
        >
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="fs-name">{label}</Label>
            <Input id="fs-name" value={name} disabled={busy} autoFocus spellCheck={false} autoComplete="off" onChange={(e) => setName(e.target.value)} />
          </div>
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">{error}</p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" disabled={busy} onClick={onClose}>{t("取消")}</Button>
            <Button type="submit" disabled={busy}>{submitText}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

type Pending = { kind: "file" | "dir" } | { kind: "rename"; entry: api.DevEntry };

export function FileBrowser({
  console: con,
  listing,
  home,
  loading,
  selected,
  onNavigate,
  onOpen,
  onReload,
}: {
  console: api.DevConsole;
  listing: api.DevListing | null;
  home: string | undefined;
  loading: boolean;
  /** 编辑器里打开的文件路径（高亮）。 */
  selected: string | null;
  onNavigate: (path: string) => void;
  onOpen: (entry: api.DevEntry) => void;
  onReload: () => void;
}): React.ReactElement {
  const [pending, setPending] = useState<Pending | null>(null);
  const confirm = useConfirm();
  const dir = listing?.path ?? "";

  function act(fn: () => Promise<unknown>, done: string): void {
    fn().then(
      () => {
        toast(done);
        onReload();
      },
      (err: unknown) => toast.error(errorMessage(err)),
    );
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-1 border-b px-2 py-1.5">
        <nav aria-label={t("当前目录")} className="flex min-w-0 flex-1 flex-wrap items-center gap-0.5 text-xs">
          {crumbs(dir, home).map((c, i, arr) => (
            <span key={c.path} className="flex items-center gap-0.5">
              <button
                type="button"
                className={cn("hover:bg-muted rounded px-1 py-0.5 font-mono", i === arr.length - 1 && "font-medium")}
                onClick={() => onNavigate(c.path)}
              >
                {c.label}
              </button>
              {i < arr.length - 1 ? <ChevronRightIcon className="text-muted-foreground size-3" /> : null}
            </span>
          ))}
        </nav>
        <Button size="icon-xs" variant="ghost" title={t("新建文件")} aria-label={t("新建文件")} disabled={listing === null} onClick={() => setPending({ kind: "file" })}>
          <FilePlusIcon />
        </Button>
        <Button size="icon-xs" variant="ghost" title={t("新建目录")} aria-label={t("新建目录")} disabled={listing === null} onClick={() => setPending({ kind: "dir" })}>
          <FolderPlusIcon />
        </Button>
        <Button size="icon-xs" variant="ghost" title={t("刷新")} aria-label={t("刷新")} onClick={onReload}>
          <RefreshCwIcon className={cn(loading && "animate-spin")} />
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        {listing === null ? (
          <p className="text-muted-foreground p-3 text-xs">{loading ? t("读取目录…") : t("目录读不到")}</p>
        ) : (
          <ul className="text-sm">
            {listing.parent === undefined ? null : (
              <li>
                <button type="button" className="hover:bg-muted flex w-full items-center gap-2 px-3 py-1 text-left font-mono text-xs" onClick={() => onNavigate(listing.parent ?? "/")}>
                  ..
                </button>
              </li>
            )}
            {listing.entries.length === 0 ? (
              <li className="text-muted-foreground px-3 py-2 text-xs">{t("空目录")}</li>
            ) : null}
            {listing.entries.map((entry) => (
              <li key={entry.path} className={cn("group flex items-center gap-1 pr-1", selected === entry.path && "bg-muted")}>
                <button
                  type="button"
                  className="hover:bg-muted flex min-w-0 flex-1 items-center gap-2 px-3 py-1 text-left"
                  title={entry.path}
                  onDoubleClick={() => { if (entry.dir) onNavigate(entry.path); }}
                  onClick={() => (entry.dir ? onNavigate(entry.path) : onOpen(entry))}
                >
                  <span className={cn("truncate font-mono text-xs", entry.dir && "font-medium")}>
                    {entry.dir ? `${entry.name}/` : entry.name}
                    {entry.symlink ? " →" : ""}
                  </span>
                  {entry.dir ? null : <span className="text-muted-foreground ml-auto shrink-0 text-[10px]">{fmtSize(entry.size)}</span>}
                </button>
                <span className="opacity-0 group-hover:opacity-100 focus-within:opacity-100">
                  <RowActionsMenu
                    label={t("{name} 的更多操作", { name: entry.name })}
                    info={[{ label: t("权限"), value: <code className="font-mono text-[10px]">{entry.mode}</code> }]}
                    actions={[
                      { label: t("重命名"), onSelect: () => setPending({ kind: "rename", entry }) },
                      {
                        label: t("删除"),
                        destructive: true,
                        onSelect: () => {
                          void confirm({
                            title: entry.dir ? t("删除目录 {name}？", { name: entry.name }) : t("删除文件 {name}？", { name: entry.name }),
                            body: <p>{entry.dir ? t("目录及其中全部内容都会从主机上删除，不可恢复。") : t("文件会从主机上删除，不可恢复。")}</p>,
                            confirmText: t("删除"),
                            danger: true,
                          }).then((ok) => {
                            if (ok) act(() => con.remove(entry.path, entry.dir), t("已删除"));
                          });
                        },
                      },
                    ]}
                  />
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>
      {pending === null ? null : pending.kind === "rename" ? (
        <NameDialog
          title={t("重命名")}
          label={t("新名称")}
          initial={pending.entry.name}
          submitText={t("保存")}
          onClose={() => setPending(null)}
          onSubmit={(name) => con.rename(pending.entry.path, joinPath(dir, name)).then(() => { toast(t("已重命名")); onReload(); })}
        />
      ) : (
        <NameDialog
          title={pending.kind === "file" ? t("新建文件") : t("新建目录")}
          label={t("名称")}
          initial=""
          submitText={t("创建")}
          onClose={() => setPending(null)}
          onSubmit={(name) =>
            (pending.kind === "file" ? con.write(joinPath(dir, name), "") : con.mkdir(joinPath(dir, name))).then(() => {
              toast(t("已创建"));
              onReload();
            })
          }
        />
      )}
    </div>
  );
}
