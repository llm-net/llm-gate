// 工作空间 Git 面板：当前目录所在仓库的状态、暂存 / 取消暂存、diff、提交、分支切换、
// 拉取 / 推送与最近提交。全部经守护进程调用主机上的 git；推拉用主机自己的凭据，
// 要交互就直接失败并把 git 的原话显示出来。读数只在进面板、切目录与动作之后重取。

import { Loader2Icon, RefreshCwIcon } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import type * as api from "@/lib/api";
import { errorMessage } from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";

function stateLabel(e: api.GitStatusEntry): { label: string; tone: string } {
  if (e.conflict) return { label: t("冲突"), tone: "text-signal-danger" };
  if (e.untracked) return { label: t("未跟踪"), tone: "text-muted-foreground" };
  const staged = e.index !== ".";
  const code = staged ? e.index : e.worktree;
  const names: Record<string, string> = { M: t("有修改"), A: t("新文件"), D: t("已删除文件"), R: t("已重命名文件"), C: t("已复制文件"), T: t("类型变更") };
  return { label: names[code] ?? code, tone: staged ? "text-signal-ok" : "" };
}

export function GitPanel({
  console: con,
  root,
  active,
  onChanged,
}: {
  console: api.DevConsole;
  /** 仓库顶层；null = 当前目录不在仓库里。 */
  root: string | null;
  active: boolean;
  /** 工作树被改动（放弃改动、切分支、拉取）后通知父组件重取目录与文件。 */
  onChanged: () => void;
}): React.ReactElement {
  const [status, setStatus] = useState<api.GitStatus | null>(null);
  const [log, setLog] = useState<api.GitCommit[]>([]);
  const [branches, setBranches] = useState<api.GitBranch[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [diffFor, setDiffFor] = useState<{ file: string; staged: boolean; text: string } | null>(null);
  const [message, setMessage] = useState("");
  const [output, setOutput] = useState<string | null>(null);
  const [newBranch, setNewBranch] = useState("");
  const confirm = useConfirm();

  const reload = useCallback(() => {
    if (root === null) {
      setStatus(null);
      setLog([]);
      setBranches([]);
      setError(null);
      return;
    }
    setLoading(true);
    Promise.all([con.gitStatus(root), con.gitLog(root, 30), con.gitBranches(root)]).then(
      ([st, lg, br]) => {
        setStatus(st);
        setLog(lg.commits);
        setBranches(br.branches);
        setError(null);
        setLoading(false);
        setSelected((prev) => new Set([...prev].filter((p) => st.entries.some((e) => e.path === p))));
      },
      (err: unknown) => {
        setError(errorMessage(err));
        setLoading(false);
      },
    );
  }, [con, root]);

  // 面板可见且仓库变了才取一次；不可见时不发请求。
  useEffect(() => {
    if (active) reload();
  }, [active, reload]);

  function run(name: string, fn: () => Promise<unknown>, after?: (result: unknown) => void, touchesTree = false): void {
    setBusy(name);
    fn().then(
      (result) => {
        setBusy(null);
        after?.(result);
        reload();
        if (touchesTree) onChanged();
      },
      (err: unknown) => {
        setBusy(null);
        toast.error(errorMessage(err));
        reload();
      },
    );
  }

  function showDiff(entry: api.GitStatusEntry): void {
    if (root === null) return;
    const staged = entry.index !== "." && entry.worktree === ".";
    setBusy(`diff-${entry.path}`);
    con.gitDiff(root, entry.path, staged).then(
      (r) => {
        setBusy(null);
        setDiffFor({ file: entry.path, staged, text: r.diff });
      },
      (err: unknown) => {
        setBusy(null);
        toast.error(errorMessage(err));
      },
    );
  }

  if (root === null) {
    return <p className="text-muted-foreground p-4 text-sm">{t("当前目录不在 Git 仓库里。进入一个仓库目录，这里会显示它的状态。")}</p>;
  }
  if (error !== null) {
    return (
      <div className="flex flex-col gap-2 p-4 text-sm">
        <p role="alert" className="text-destructive">{error}</p>
        <div><Button size="sm" variant="outline" onClick={reload}>{t("重试")}</Button></div>
      </div>
    );
  }
  if (status === null) {
    return <p className="text-muted-foreground p-4 text-sm">{t("读取仓库状态…")}</p>;
  }
  const stagedCount = status.entries.filter((e) => e.index !== "." && !e.untracked).length;
  const picked = [...selected];
  const current = branches.find((b) => b.current)?.name ?? status.branch;

  return (
    <div className="flex h-full min-h-0 flex-col text-sm">
      <div className="flex flex-wrap items-center gap-2 border-b px-3 py-1.5 text-xs">
        <code className="font-mono" title={status.root}>{status.root.split("/").pop()}</code>
        <Badge variant="outline">{status.detached ? t("游离 HEAD") : status.branch}</Badge>
        {status.upstream === undefined ? null : (
          <span className="text-muted-foreground">
            {status.upstream}
            {status.ahead > 0 ? ` ↑${status.ahead}` : ""}
            {status.behind > 0 ? ` ↓${status.behind}` : ""}
          </span>
        )}
        <div className="ml-auto flex items-center gap-1">
          {(["fetch", "pull", "push"] as const).map((action) => (
            <Button key={action} size="sm" variant="outline" disabled={busy !== null}
              onClick={() => run(action, () => con.gitRemote(root, action), (r) => {
                const res = r as api.GitResult;
                setOutput(res.output);
                toast(action === "pull" ? t("已拉取") : action === "push" ? t("已推送") : t("已抓取"));
              }, action === "pull")}>
              {busy === action ? <Loader2Icon className="animate-spin" /> : null}
              {action === "fetch" ? t("抓取") : action === "pull" ? t("拉取") : t("推送")}
            </Button>
          ))}
          <Button size="icon-xs" variant="ghost" title={t("刷新")} aria-label={t("刷新")} onClick={reload}>
            <RefreshCwIcon className={cn(loading && "animate-spin")} />
          </Button>
        </div>
      </div>
      <div className="grid min-h-0 flex-1 grid-cols-1 gap-0 overflow-hidden md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
        <div className="flex min-h-0 flex-col overflow-auto border-b md:border-r md:border-b-0">
          <div className="flex items-center gap-2 px-3 py-1.5 text-xs">
            <span className="font-medium">{t("变更")}</span>
            <span className="text-muted-foreground">{t("{n} 项，已暂存 {s} 项", { n: status.entries.length, s: stagedCount })}</span>
            <div className="ml-auto flex gap-1">
              <Button size="sm" variant="outline" disabled={busy !== null || status.entries.length === 0}
                onClick={() => run("stage-all", () => con.gitStage(root, picked, false))}>
                {picked.length > 0 ? t("暂存所选") : t("全部暂存")}
              </Button>
              <Button size="sm" variant="outline" disabled={busy !== null || stagedCount === 0}
                onClick={() => run("unstage-all", () => con.gitStage(root, picked, true))}>
                {picked.length > 0 ? t("取消暂存所选") : t("全部取消暂存")}
              </Button>
              <Button size="sm" variant="outline" disabled={busy !== null || picked.length === 0}
                onClick={() => {
                  void confirm({
                    title: t("放弃 {n} 个文件的改动？", { n: picked.length }),
                    body: <p>{t("已跟踪的文件恢复到最近一次提交，未跟踪的新文件会被删除，不可恢复。")}</p>,
                    confirmText: t("放弃改动"),
                    danger: true,
                  }).then((ok) => {
                    if (ok) run("discard", () => con.gitDiscard(root, picked), () => setSelected(new Set()), true);
                  });
                }}>
                {t("放弃改动")}
              </Button>
            </div>
          </div>
          {status.entries.length === 0 ? (
            <p className="text-muted-foreground px-3 py-2 text-xs">{t("工作树干净")}</p>
          ) : (
            <ul>
              {status.entries.map((e) => {
                const s = stateLabel(e);
                const checked = selected.has(e.path);
                return (
                  <li key={e.path} className={cn("flex items-center gap-2 px-3 py-0.5", diffFor?.file === e.path && "bg-muted")}>
                    <Checkbox checked={checked} aria-label={t("选择 {name}", { name: e.path })}
                      onCheckedChange={(c) => setSelected((prev) => {
                        const next = new Set(prev);
                        if (c === true) next.add(e.path); else next.delete(e.path);
                        return next;
                      })} />
                    <button type="button" className="min-w-0 flex-1 truncate text-left font-mono text-xs hover:underline" title={e.path} onClick={() => showDiff(e)}>
                      {e.orig_path === undefined ? e.path : `${e.orig_path} → ${e.path}`}
                    </button>
                    <span className={cn("shrink-0 text-[10px]", s.tone)}>{s.label}</span>
                  </li>
                );
              })}
            </ul>
          )}
          <div className="mt-auto flex flex-col gap-2 border-t p-3">
            <Textarea
              value={message}
              placeholder={t("提交说明")}
              rows={2}
              className="font-mono text-xs"
              onChange={(e) => setMessage(e.target.value)}
            />
            <div className="flex flex-wrap items-center gap-2">
              <Button size="sm" disabled={busy !== null || stagedCount === 0 || message.trim() === ""}
                onClick={() => run("commit", () => con.gitCommit(root, message), () => {
                  setMessage("");
                  toast(t("已提交"));
                })}>
                {busy === "commit" ? <Loader2Icon className="animate-spin" /> : null}
                {t("提交已暂存")}
              </Button>
              <Select value={current} onValueChange={(name) => {
                if (name === current) return;
                run("checkout", () => con.gitCheckout(root, name, false), () => toast(t("已切换到 {name}", { name })), true);
              }}>
                <SelectTrigger size="sm" className="w-44" aria-label={t("分支")}>
                  <SelectValue placeholder={t("分支")} />
                </SelectTrigger>
                <SelectContent>
                  {branches.map((b) => (
                    <SelectItem key={b.name} value={b.name}>{b.name}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <form className="flex items-center gap-1" onSubmit={(ev) => {
                ev.preventDefault();
                const name = newBranch.trim();
                if (name === "") return;
                run("branch", () => con.gitCheckout(root, name, true), () => {
                  setNewBranch("");
                  toast(t("已创建并切换到 {name}", { name }));
                }, true);
              }}>
                <Input value={newBranch} placeholder={t("新分支名")} className="h-8 w-36 text-xs" spellCheck={false} onChange={(e) => setNewBranch(e.target.value)} />
                <Button size="sm" variant="outline" type="submit" disabled={busy !== null || newBranch.trim() === ""}>{t("新建分支")}</Button>
              </form>
            </div>
          </div>
        </div>
        <div className="flex min-h-0 flex-col overflow-hidden">
          {diffFor === null ? (
            <div className="min-h-0 flex-1 overflow-auto">
              <div className="px-3 py-1.5 text-xs font-medium">{t("最近提交")}</div>
              {log.length === 0 ? <p className="text-muted-foreground px-3 pb-2 text-xs">{t("还没有提交")}</p> : (
                <ul className="text-xs">
                  {log.map((c) => (
                    <li key={c.hash} className="border-t px-3 py-1.5">
                      <div className="flex items-baseline gap-2">
                        <code className="text-muted-foreground font-mono">{c.short}</code>
                        <span className="min-w-0 flex-1 truncate" title={c.subject}>{c.subject}</span>
                      </div>
                      <div className="text-muted-foreground">{c.author} · {fmtTime(c.date)}</div>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ) : (
            <>
              <div className="flex items-center gap-2 border-b px-3 py-1.5 text-xs">
                <code className="min-w-0 flex-1 truncate font-mono">{diffFor.file}</code>
                {diffFor.staged ? <Badge variant="outline">{t("已暂存")}</Badge> : null}
                <Button size="sm" variant="ghost" onClick={() => setDiffFor(null)}>{t("关闭")}</Button>
              </div>
              <pre className="min-h-0 flex-1 overflow-auto p-3 font-mono text-[11px] leading-4 whitespace-pre">
                {diffFor.text === "" ? t("（无差异）") : diffFor.text.split("\n").map((line, i) => (
                  <span key={i} className={cn("block", line.startsWith("+") && !line.startsWith("+++") && "text-signal-ok", line.startsWith("-") && !line.startsWith("---") && "text-signal-danger", line.startsWith("@@") && "text-muted-foreground")}>
                    {line}
                  </span>
                ))}
              </pre>
            </>
          )}
          {output === null ? null : (
            <div className="border-t">
              <div className="flex items-center px-3 py-1 text-xs">
                <span className="font-medium">{t("git 输出")}</span>
                <Button size="sm" variant="ghost" className="ml-auto" onClick={() => setOutput(null)}>{t("关闭")}</Button>
              </div>
              <pre className="max-h-32 overflow-auto px-3 pb-2 font-mono text-[11px] whitespace-pre-wrap">{output === "" ? t("（无输出）") : output}</pre>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
