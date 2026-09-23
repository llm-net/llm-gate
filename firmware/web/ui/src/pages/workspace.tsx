// 智能体 · 工作空间：打开一个工作空间。id 在查询串 ?id=<id>，从「工作空间管理」清单的「打开」进入。
//
// 两种类型两套工作台：创作工作空间（kind = studio）是 features/studio/workbench.tsx（对话列表、
// 智能体对话、素材库，对话 id 在 ?chat=），顶栏右端多一个「配置媒体生成能力」
// （features/studio/media-config.tsx）；开发工作空间是下面的三栏。
//
// 进页收起左侧菜单。开发工作空间三栏按 1/4、1/4、1/2 分宽：左边「文件 / Git」（目录清单钉在工作空间目录以内，不往上走）、中间文件预览 / 编辑、
// 右边 tmux 终端（只列本工作空间的会话 ws-<name>-main 与 ws-<name>-t<n>，标签上显示 main / t<n>，
// 没有就自动开 main，起始目录就是工作空间）。文件、Git 与终端都经这台主机的 devd 透传
// （features/devconsole/*）；主机的 devd 不可用时给出去处。目录与 Git 读数只在进页、切目录
// 与动作之后重取；终端面板的会话清单每 5 秒重取一次以探测会话里的开发工具（见 terminal-panel.tsx）。

import { ArrowLeftIcon } from "lucide-react";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable";
import { useSidebar } from "@/components/ui/sidebar";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Editor } from "@/features/devconsole/editor";
import { FileBrowser } from "@/features/devconsole/file-browser";
import { GitPanel } from "@/features/devconsole/git-panel";
import { TerminalPanel } from "@/features/devconsole/terminal-panel";
import { MediaConfigButton } from "@/features/studio/media-config";
import { StudioWorkbench } from "@/features/studio/workbench";
import * as api from "@/lib/api";
import { t } from "@/lib/i18n";
import { navigate, useLocationSearch } from "@/lib/router";
import { useResource } from "@/lib/use-resource";

import { PageHeader } from "./page-shell";

/** 路径是否在工作空间目录以内（含它自己）。 */
function inside(root: string, path: string): boolean {
  return path === root || path.startsWith(root.endsWith("/") ? root : `${root}/`);
}

function Workbench({ ws }: { ws: api.Workspace }): React.ReactElement {
  const con = useMemo(() => api.devConsole(ws.host_id), [ws.host_id]);
  const root = ws.path;
  const [dir, setDir] = useState<string>(root);
  const [listing, setListing] = useState<api.DevListing | null>(null);
  const [listLoading, setListLoading] = useState(true);
  const [file, setFile] = useState<api.DevFile | null>(null);
  const [fileLoading, setFileLoading] = useState(false);
  const [tab, setTab] = useState<"file" | "git">("file");

  const loadDir = useCallback((path: string) => {
    // 目录清单钉在工作空间以内：往上走到根为止。
    const target = inside(root, path) ? path : root;
    setListLoading(true);
    con.list(target).then(
      (l) => {
        setListing(l);
        setDir(l.path);
        setListLoading(false);
      },
      (err: unknown) => {
        setListLoading(false);
        toast.error(api.errorMessage(err));
      },
    );
  }, [con, root]);

  useEffect(() => {
    loadDir(root);
  }, [root, loadDir]);

  function openFile(entry: api.DevEntry): void {
    setFileLoading(true);
    setTab("file");
    con.read(entry.path).then(
      (f) => {
        setFile(f);
        setFileLoading(false);
      },
      (err: unknown) => {
        setFileLoading(false);
        toast.error(api.errorMessage(err));
      },
    );
  }

  const gitRoot = listing?.git_root ?? null;
  // 到了工作空间根就不再给「..」。
  const scoped = useMemo((): api.DevListing | null => {
    if (listing === null || listing.path !== root) return listing;
    const { parent: _parent, ...rest } = listing;
    return rest;
  }, [listing, root]);
  const prefix = api.workspaceSessionPrefix(ws);

  return (
    <ResizablePanelGroup orientation="horizontal" className="min-h-0 flex-1">
      <ResizablePanel defaultSize="25%" minSize="15%" className="bg-card border-r">
        <Tabs value={tab} onValueChange={(v) => setTab(v === "git" ? "git" : "file")} className="flex h-full min-h-0 flex-col gap-0">
          <TabsList className="m-1 self-start">
            <TabsTrigger value="file">{t("文件")}</TabsTrigger>
            <TabsTrigger value="git">Git</TabsTrigger>
          </TabsList>
          <TabsContent value="file" className="min-h-0 flex-1 border-t">
            <FileBrowser console={con} listing={scoped} home={root} loading={listLoading} selected={file?.path ?? null} onNavigate={loadDir} onOpen={openFile} onReload={() => loadDir(dir)} />
          </TabsContent>
          <TabsContent value="git" className="min-h-0 flex-1 border-t">
            <GitPanel console={con} root={gitRoot} active={tab === "git"} onChanged={() => {
              loadDir(dir);
              if (file !== null) con.read(file.path).then(setFile, () => setFile(null));
            }} />
          </TabsContent>
        </Tabs>
      </ResizablePanel>
      <ResizableHandle withHandle />
      <ResizablePanel defaultSize="25%" minSize="15%" className="bg-card">
        <Editor console={con} file={file} loading={fileLoading} onSaved={setFile} />
      </ResizablePanel>
      <ResizableHandle withHandle />
      <ResizablePanel defaultSize="50%" minSize="20%" className="bg-card border-l">
        <TerminalPanel console={con} storageKey={`llmgate.workspace.${ws.id}.terminal`} dir={root} prefix={prefix} />
      </ResizablePanel>
    </ResizablePanelGroup>
  );
}

/** 进页收起左侧菜单，把宽度让给三栏；离页时若进页前是展开的就恢复。 */
function useCollapsedSidebar(): void {
  const { open, setOpen } = useSidebar();
  const setOpenRef = useRef(setOpen);
  setOpenRef.current = setOpen;
  const wasOpen = useRef(open);
  useEffect(() => {
    setOpenRef.current(false);
    return () => {
      if (wasOpen.current) setOpenRef.current(true);
    };
  }, []);
}

export function WorkspacePage(): React.ReactElement {
  useCollapsedSidebar();
  const search = useLocationSearch();
  const id = search.get("id") ?? "";
  const chatParam = search.get("chat");
  const res = useResource(() => (id === "" ? Promise.resolve(null) : api.getWorkspace(id)), [id]);
  const ws = res.data?.workspace;
  const studio = ws?.kind === "studio";

  return (
    <div className="flex h-full min-h-0 flex-col gap-2 p-2 sm:p-3">
      <PageHeader
        compact
        title={ws === undefined ? t("工作空间") : studio ? t("创作工作空间 · {name}", { name: ws.name }) : t("工作空间 · {name}", { name: ws.name })}
        note={
          ws === undefined
            ? undefined
            : `${studio && ws.template_name !== undefined ? `${ws.template_name} · ` : ""}${studio && ws.host_id === 0 ? t("本设备") : ws.host_name === "" ? ws.host_address : ws.host_name} · ${ws.path}${ws.repo_url === undefined ? "" : ` · ${ws.repo_url}${ws.branch === undefined ? "" : ` @ ${ws.branch}`}`}`
        }
        leading={
          <Button size="sm" variant="outline" title={t("返回工作空间")} aria-label={t("返回工作空间")} onClick={() => navigate("/workspaces")}>
            <ArrowLeftIcon />
            {t("返回")}
          </Button>
        }
        actions={ws !== undefined && studio ? <MediaConfigButton wsId={ws.id} /> : undefined}
      />
      {res.error !== null ? (
        <p role="alert" className="text-destructive text-sm">{res.error}</p>
      ) : id === "" || (res.data !== null && ws === undefined) ? (
        <p className="text-muted-foreground text-sm">{t("没有这个工作空间：请从工作空间清单打开。")}</p>
      ) : ws === undefined ? null : studio && (ws.host_id === 0 || ws.host_ready) ? (
        <StudioWorkbench ws={ws} chatParam={chatParam} />
      ) : !ws.host_ready ? (
        <p className="text-muted-foreground text-sm">{t("这台主机的 devd 当前不可用：请先在主机/SoC 页面安装或检查。")}</p>
      ) : (
        <Workbench ws={ws} />
      )}
    </div>
  );
}
