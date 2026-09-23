// 创作工作空间的工作台（/workspace?id=<id>，kind = studio）：
//   - 左列对话列表（一个工作空间可开多个对话；新建时选引擎（工作节点上的 Codex / Claude Code）、
//     API 密钥、模型、推理档位，之后不改，new-chat.tsx；对话框里标出这个空间启用的生成模型里这把
//     密钥能用的那些）。对话可归档：会话结束，之后只能查看、不能再下指令，收在列表末尾的「已归档」
//     一组里。设备上的存量空间不能对话（读数 chats_enabled），只能管理文件；
//   - 中列对话：时间线（指令、回复、推理摘要、工具调用、生成记录（带结果缩略图）、文件变更、
//     工作节点上跑过的命令、会话边界）+ 队列 + 底部输入框（文本、贴图；素材库里的「引用到指令」
//     把「子目录/文件名」的路径填进来）；
//   - 右列素材库（features/studio/files-panel.tsx）：按 media/ docs/ agent/ 三个子目录分页，上传
//     素材、看生成结果与文档。
// 读数：进页取一次整份（工作空间、能否对话、对话列表、目录），切到某个对话再取它的时间线，之后
// 按会话 revision 陪等（零轮询例外）；陪等带回生成 / 文件事件时重取一次目录。对话 id 在查询串
// `?chat=`。与 Agent远控页一样不走 PageContainer 的整页滚动：三列各自滚动。

import { LoaderCircle, Plus } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { ChatList, Collapsible, Composer, Markdown, OutputBlock, chatTitle, exitTone, mergeEvents, parseMeta, runStatusLabel, sessionLabel } from "@/features/agentchat/shared";
import { FilesPanel, GeneratedThumb, fmtBytes } from "@/features/studio/files-panel";
import { StudioNewChatDialog } from "@/features/studio/new-chat";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime, fmtTimeSeconds } from "@/lib/format";
import { t } from "@/lib/i18n";
import { Link, navigate } from "@/lib/router";

// ---- 时间线 ----

function toolLabel(name: string): string {
  switch (name) {
    case "view_image":
      return t("查看图像");
    case "read_text":
      return t("读取文本");
    case "list_files":
      return t("列目录");
    case "Read":
      return t("读取文件");
    case "Glob":
      return t("查找文件");
    case "Grep":
      return t("搜索内容");
    default:
      return name;
  }
}

function fileActionLabel(action: unknown): string {
  switch (action) {
    case "write":
      return t("写入文件");
    case "delete":
      return t("删除文件");
    case "rename":
      return t("改名");
    default:
      return t("文件变更");
  }
}

function EventRow({ wsId, ev, file, onOpenFile }: { wsId: string; ev: api.StudioEvent; file: api.StudioFile | undefined; onOpenFile: (name: string) => void }): React.ReactElement | null {
  const meta = parseMeta(ev.meta);
  const time = <span className="text-muted-foreground shrink-0 text-[10px] tabular-nums">{fmtTimeSeconds(ev.at)}</span>;
  switch (ev.kind) {
    case "user": {
      const images = typeof meta["images"] === "number" ? (meta["images"] as number) : 0;
      return (
        <div className="flex justify-end">
          <div className="bg-primary/10 max-w-[85%] rounded-2xl rounded-br-sm px-3 py-2">
            <p className="text-sm whitespace-pre-wrap">{ev.body}</p>
            <div className="mt-1 flex items-center justify-end gap-2">
              {images > 0 ? <span className="text-muted-foreground text-[10px]">{t("{n} 张图片", { n: images })}</span> : null}
              {time}
            </div>
          </div>
        </div>
      );
    }
    case "assistant":
      return (
        <div className="max-w-[92%]">
          <Markdown text={ev.body ?? ""} />
          <div className="mt-0.5">{time}</div>
        </div>
      );
    case "reasoning":
      return (
        <div className="max-w-[92%]">
          <Collapsible summary={t("推理摘要")}>
            <p className="text-muted-foreground mt-1 text-xs whitespace-pre-wrap">{ev.body}</p>
          </Collapsible>
        </div>
      );
    case "tool":
      return (
        <div className="text-muted-foreground flex flex-wrap items-center gap-2 text-xs">
          <Badge variant="outline">{toolLabel(ev.title ?? "")}</Badge>
          {ev.body !== undefined && ev.body !== "" ? (
            <button type="button" className="hover:text-foreground font-mono underline-offset-2 hover:underline" onClick={() => onOpenFile(ev.body ?? "")}>
              {ev.body}
            </button>
          ) : null}
          {time}
        </div>
      );
    case "generate": {
      const failed = meta["status"] !== "succeeded";
      const provider = typeof meta["provider"] === "string" ? (meta["provider"] as string) : "";
      const model = typeof meta["model"] === "string" ? (meta["model"] as string) : "";
      const kindLabel = meta["kind"] === "video" ? t("生成视频") : t("生成图像");
      const bytes = typeof meta["bytes"] === "number" ? (meta["bytes"] as number) : 0;
      return (
        <div className={cn("max-w-[92%] rounded-md border px-3 py-2", failed && "border-signal-alert/50")}>
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <Badge variant="outline" className={failed ? "text-signal-alert" : "text-signal-ok"}>{failed ? t("{kind}失败", { kind: kindLabel }) : kindLabel}</Badge>
            {provider !== "" ? <span className="text-muted-foreground font-mono">{provider} / {model}</span> : null}
            {ev.title !== undefined && ev.title !== "" ? (
              <button type="button" className="font-mono break-all underline-offset-2 hover:underline" onClick={() => onOpenFile(ev.title ?? "")}>
                {ev.title}
              </button>
            ) : null}
            {bytes > 0 ? <span className="text-muted-foreground">{fmtBytes(bytes)}</span> : null}
            {ev.duration_ms !== undefined && ev.duration_ms > 0 ? <span className="text-muted-foreground text-[10px]">{(ev.duration_ms / 1000).toFixed(0)}s</span> : null}
            {time}
          </div>
          {ev.body !== undefined && ev.body !== "" ? <p className="text-muted-foreground mt-1 text-xs whitespace-pre-wrap">{ev.body}</p> : null}
          {failed && typeof meta["error"] === "string" ? <p className="text-signal-alert mt-1 text-xs">{meta["error"] as string}</p> : null}
          {!failed && ev.title !== undefined && ev.title !== "" ? (
            <div className="mt-2">
              <GeneratedThumb wsId={wsId} name={ev.title} file={file} onOpen={onOpenFile} />
            </div>
          ) : null}
        </div>
      );
    }
    case "file": {
      const bytes = typeof meta["bytes"] === "number" ? (meta["bytes"] as number) : 0;
      const from = typeof meta["from"] === "string" ? (meta["from"] as string) : "";
      return (
        <div className="max-w-[92%] rounded-md border border-dashed px-3 py-2">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <span>{fileActionLabel(meta["action"])}</span>
            {from !== "" ? <code className="text-muted-foreground font-mono break-all">{from} →</code> : null}
            <button type="button" className="font-mono break-all underline-offset-2 hover:underline" onClick={() => onOpenFile(ev.title ?? "")}>
              {ev.title}
            </button>
            {bytes > 0 ? <span className="text-muted-foreground text-[10px]">{fmtBytes(bytes)}</span> : null}
            {meta["append"] === true ? <Badge variant="outline">{t("追加")}</Badge> : null}
            {time}
          </div>
          {ev.body !== undefined && ev.body !== "" ? (
            <Collapsible summary={t("内容")}>
              <OutputBlock text={ev.body} />
            </Collapsible>
          ) : null}
        </div>
      );
    }
    case "command": {
      const truncated = meta["truncated"] === true;
      const failed = meta["error"] === true;
      const exit = typeof meta["exit_code"] === "number" ? (meta["exit_code"] as number) : undefined;
      return (
        <div className="max-w-[92%] rounded-md border px-3 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <code className="font-mono text-xs break-all">$ {ev.title}</code>
            {failed ? (
              <Badge variant="outline" className="text-signal-alert">{t("未能执行")}</Badge>
            ) : (
              <Badge variant="outline" className={exitTone(exit)}>exit {exit ?? "?"}</Badge>
            )}
            {ev.duration_ms !== undefined && ev.duration_ms > 0 ? <span className="text-muted-foreground text-[10px]">{(ev.duration_ms / 1000).toFixed(1)}s</span> : null}
            {time}
          </div>
          {ev.body !== undefined && ev.body !== "" ? (
            <Collapsible summary={truncated ? t("输出（已截断）") : t("输出")} defaultOpen={failed || (exit !== undefined && exit !== 0)}>
              <OutputBlock text={ev.body} />
            </Collapsible>
          ) : (
            <p className="text-muted-foreground mt-1 text-[10px]">{t("（无输出）")}</p>
          )}
        </div>
      );
    }
    case "run":
      return (
        <div className="flex items-center gap-2 text-xs">
          <Badge variant="outline" className={ev.title === "failed" ? "text-signal-alert" : ""}>
            {ev.title === "failed" ? t("指令失败") : ev.title === "cancelled" ? t("指令已取消") : runStatusLabel(ev.title as api.AgentRunStatus)}
          </Badge>
          <span className="text-muted-foreground">{ev.reason ?? ev.body}</span>
          {time}
        </div>
      );
    case "session":
      return (
        <div className="text-muted-foreground flex items-center gap-2 text-[11px]">
          <span className="bg-border h-px flex-1" />
          <span>
            {ev.title === "started"
              ? t("会话开始 · {engine}", { engine: ev.body ?? "" })
              : ev.title === "media_updated"
                ? t("媒体生成能力有变化，已告知智能体")
                : t("会话结束 · {reason}", { reason: ev.reason ?? ev.body ?? "" })}
          </span>
          {time}
          <span className="bg-border h-px flex-1" />
        </div>
      );
    case "error":
      return <p className="text-signal-alert text-xs">{ev.reason ?? ev.body}</p>;
    default:
      return null;
  }
}

// ---- 工作台 ----

const FILE_EVENT_KINDS: api.StudioEventKind[] = ["generate", "file", "command"];

export function StudioWorkbench({ ws, chatParam }: { ws: api.Workspace; chatParam: string | null }): React.ReactElement {
  const wsId = ws.id;
  const confirm = useConfirm();
  const [page, setPage] = useState<api.StudioPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [files, setFiles] = useState<api.StudioFile[]>([]);
  const [filesLoading, setFilesLoading] = useState(false);
  const [chat, setChat] = useState<api.StudioChat | null>(null);
  const [chatError, setChatError] = useState<string | null>(null);
  const [events, setEvents] = useState<api.StudioEvent[]>([]);
  const [state, setState] = useState<api.StudioSessionState | null>(null);
  const [exhausted, setExhausted] = useState(false);
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState("");
  const [openFile, setOpenFile] = useState<string | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);
  const filesRequest = useRef(0);

  useEffect(() => () => { filesRequest.current++; }, [wsId]);

  const selectChat = useCallback(
    (id: string | null) => {
      navigate("/workspace", { search: id === null ? `?id=${wsId}` : `?id=${wsId}&chat=${id}` });
    },
    [wsId],
  );

  const reloadFiles = useCallback(() => {
    const request = ++filesRequest.current;
    setFilesLoading(true);
    api.listStudioFiles(wsId).then(
      (r) => {
        if (request !== filesRequest.current) return;
        setFiles(r.files);
        setFilesLoading(false);
      },
      (err: unknown) => {
        if (request !== filesRequest.current) return;
        setFilesLoading(false);
        toast.error(api.errorMessage(err));
      },
    );
  }, [wsId]);

  // 进页读一次整份。
  useEffect(() => {
    let alive = true;
    setPage(null);
    setError(null);
    api.getStudio(wsId).then(
      (p) => {
        if (!alive) return;
        setPage(p);
        setFiles(p.files);
      },
      (err: unknown) => {
        if (alive) setError(api.errorMessage(err));
      },
    );
    return () => {
      alive = false;
    };
  }, [wsId]);

  // 没点名对话时落在最近活动的那个进行中的对话上；全都归档了才落到归档的。
  const activeChatId = chatParam !== null && chatParam !== "" ? chatParam : ((page?.chats.find((c) => c.archived_at === undefined) ?? page?.chats[0])?.id ?? null);

  // 切到某个对话：取它的时间线，然后陪等。
  useEffect(() => {
    setChat(null);
    setChatError(null);
    setEvents([]);
    setState(null);
    setExhausted(false);
    if (activeChatId === null) return;
    const chatId = activeChatId;
    const ctrl = new AbortController();
    let alive = true;
    api.getStudioChat(wsId, chatId).then(
      (p) => {
        if (!alive) return;
        setChat(p.chat);
        setEvents(p.events);
        setState(p.state);
        setExhausted(p.events.length < 200);
        stickRef.current = true;
        let revision = p.state.revision;
        let after = p.events.length > 0 ? (p.events[p.events.length - 1]?.id ?? 0) : 0;
        const loop = async () => {
          while (alive) {
            try {
              const r = await api.waitStudio(wsId, chatId, revision, after, ctrl.signal);
              if (!alive) return;
              revision = r.state.revision;
              setState(r.state);
              setChat((cur) => (cur === null ? cur : { ...cur, status: r.state.status, busy: r.state.current !== undefined || r.state.queue.length > 0 }));
              if (r.events.length > 0) {
                after = r.events[r.events.length - 1]?.id ?? after;
                setEvents((cur) => mergeEvents(cur, r.events));
                if (r.events.some((e) => FILE_EVENT_KINDS.includes(e.kind))) reloadFiles();
                const firstUser = r.events.find((e) => e.kind === "user");
                if (firstUser !== undefined) {
                  setPage((cur) =>
                    cur === null
                      ? cur
                      : { ...cur, chats: cur.chats.map((c) => (c.id === chatId && c.title === "" ? { ...c, title: (firstUser.body ?? "").split("\n")[0]?.trim().slice(0, 40) ?? "" } : c)) },
                  );
                }
              }
            } catch (err) {
              if (!alive || (err instanceof DOMException && err.name === "AbortError")) return;
              if (err instanceof api.ApiError && err.status === 404) {
                setChatError(t("这个对话已不存在。"));
                return;
              }
              await new Promise((res) => setTimeout(res, 2000));
            }
          }
        };
        void loop();
      },
      (err: unknown) => {
        if (alive) setChatError(api.errorMessage(err));
      },
    );
    return () => {
      alive = false;
      ctrl.abort();
    };
  }, [wsId, activeChatId, reloadFiles]);

  useEffect(() => {
    if (state === null) return;
    setPage((cur) =>
      cur === null ? cur : { ...cur, chats: cur.chats.map((c) => (c.id === state.chat_id ? { ...c, status: state.status, busy: state.current !== undefined || state.queue.length > 0 } : c)) },
    );
  }, [state]);

  useEffect(() => {
    const el = scrollRef.current;
    if (el !== null && stickRef.current) el.scrollTop = el.scrollHeight;
  }, [events, state?.live?.text, state?.live?.activity, state?.status]);

  const loadOlder = useCallback(async () => {
    if (events.length === 0 || activeChatId === null) return;
    try {
      const r = await api.getStudioChatEvents(wsId, activeChatId, { before: events[0]?.id ?? 0, limit: 200 });
      if (r.events.length < 200) setExhausted(true);
      setEvents((cur) => [...r.events, ...cur]);
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
  }, [events, wsId, activeChatId]);

  const deleteChat = useCallback(
    (target: api.StudioChat) => {
      void confirm({
        title: t("删除对话「{title}」？", { title: chatTitle(target) }),
        body: t("正在执行的指令会被中止，排队中的全部取消，引擎进程关闭；对话里的指令与回复删除。素材库里的文件保留。"),
        confirmText: t("删除"),
        danger: true,
      }).then((ok) => {
        if (!ok) return;
        api.deleteStudioChat(wsId, target.id).then(
          () => {
            toast(t("对话已删除"));
            setPage((cur) => (cur === null ? cur : { ...cur, chats: cur.chats.filter((c) => c.id !== target.id) }));
            if (target.id === activeChatId) selectChat(null);
          },
          (err: unknown) => toast.error(api.errorMessage(err)),
        );
      });
    },
    [confirm, wsId, activeChatId, selectChat],
  );

  const archiveChat = useCallback(
    (target: api.StudioChat) => {
      void confirm({
        title: t("归档对话「{title}」？", { title: chatTitle(target) }),
        body: t("归档后这个对话只能查看，不能再下指令，也不能恢复。正在执行的指令会被中止，排队中的全部取消，引擎进程关闭；对话记录与素材库里的文件保留。"),
        confirmText: t("归档"),
      }).then((ok) => {
        if (!ok) return;
        api.archiveStudioChat(wsId, target.id).then(
          (r) => {
            toast(t("对话已归档"));
            setPage((cur) => (cur === null ? cur : { ...cur, chats: cur.chats.map((c) => (c.id === target.id ? r.chat : c)) }));
            if (target.id === activeChatId) {
              setChat(r.chat);
              setState(r.state);
            }
          },
          (err: unknown) => toast.error(api.errorMessage(err)),
        );
      });
    },
    [confirm, wsId, activeChatId],
  );

  // 素材库的「引用到指令」：把「子目录/文件名」的路径填进输入框草稿。
  const cite = useCallback((name: string) => {
    setDraft((cur) => (cur === "" ? name + " " : cur.endsWith(" ") ? cur + name + " " : cur + " " + name + " "));
  }, []);

  if (error !== null) {
    return <Card className="text-signal-alert p-5 text-sm">{error}</Card>;
  }
  if (page === null) {
    return <p className="text-muted-foreground flex h-full items-center justify-center text-sm">{t("加载中…")}</p>;
  }
  const chatsReason = page.chats_reason ?? t("这个工作空间不能对话");
  const status = state === null ? null : sessionLabel(state.status);
  const busy = state !== null && state.current !== undefined;
  const archived = chat !== null && chat.archived_at !== undefined;
  const composerDisabled = chat === null || !page.chats_enabled;
  const disabledReason = !page.chats_enabled ? chatsReason : chat === null ? t("先选择或新建一个对话") : undefined;
  const openFromTimeline = (name: string) => {
    if (files.some((f) => f.name === name)) setOpenFile(name);
    else toast(t("文件 {name} 已不在素材库里", { name }));
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      {page.chats_enabled ? null : (
        <Card className="flex flex-wrap items-center gap-2 p-3 text-sm">
          <span className="text-signal-alert">{chatsReason}</span>
          <Link to="/workspaces" className="ml-auto text-xs underline">{t("去「工作空间管理」新建")}</Link>
        </Card>
      )}
      <div className="grid min-h-0 flex-1 gap-2 grid-rows-[auto_minmax(0,3fr)_minmax(0,2fr)] lg:grid-cols-[12rem_minmax(0,3fr)_minmax(18rem,2fr)] lg:grid-rows-1">
        <ChatList chats={page.chats} activeId={activeChatId} onSelect={(id) => selectChat(id)} onCreate={() => (page.chats_enabled ? setCreating(true) : toast.error(chatsReason))} onDelete={deleteChat} onArchive={archiveChat} />
        <Card className="flex min-h-0 flex-col gap-0 overflow-hidden p-0">
          {chat !== null ? (
            <div className="text-muted-foreground flex flex-wrap items-center gap-x-3 gap-y-1 border-b px-4 py-1.5 text-xs">
              <span title={t("对话新建时钉死，不能修改")}>
                {api.studioEngineLabel(chat.engine)} · {chat.model}{chat.effort !== "" ? ` · ${chat.effort}` : ""}{chat.key_display !== "" ? ` · ${chat.key_display}` : ""}
              </span>
              {archived ? <Badge variant="outline">{t("已归档")}</Badge> : status === null ? null : <Badge variant="outline" className={status.tone}>{status.label}</Badge>}
              {state?.engine_started_at !== undefined ? <span className="text-[11px]">{t("会话始于 {time}", { time: fmtTime(state.engine_started_at) })}</span> : null}
            </div>
          ) : null}
          {activeChatId === null ? (
            <div className="text-muted-foreground flex h-full flex-col items-center justify-center gap-3 p-4 text-sm">
              <p>{page.chats_enabled ? t("新建一个对话开始：选好引擎、API 密钥、模型与推理档位，智能体会带着素材库与项目说明开工。") : chatsReason}</p>
              <Button size="sm" disabled={!page.chats_enabled} onClick={() => setCreating(true)}>
                <Plus />
                {t("新建对话")}
              </Button>
            </div>
          ) : chatError !== null ? (
            <div className="text-signal-alert flex h-full items-center justify-center p-4 text-sm">{chatError}</div>
          ) : chat === null || state === null ? (
            <p className="text-muted-foreground flex h-full items-center justify-center text-sm">{t("加载中…")}</p>
          ) : (
            <>
              <div
                ref={scrollRef}
                className="min-h-0 flex-1 space-y-3 overflow-y-auto px-4 py-3"
                onScroll={(e) => {
                  const el = e.currentTarget;
                  stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
                }}
              >
                {exhausted ? null : (
                  <div className="flex justify-center">
                    <Button size="sm" variant="ghost" onClick={() => void loadOlder()}>
                      {t("加载更早的记录")}
                    </Button>
                  </div>
                )}
                {events.length === 0 && !busy && archived ? (
                  <p className="text-muted-foreground flex h-full items-center justify-center text-sm">{t("这个对话没有记录。")}</p>
                ) : events.length === 0 && !busy ? (
                  <div className="text-muted-foreground flex h-full flex-col items-center justify-center gap-2 text-sm">
                    <p>{t("还没有对话。给智能体下一条指令，比如：")}</p>
                    <ul className="list-disc text-xs">
                      <li>{t("看看素材库里的参考图，给我三版春季海报的主视觉")}</li>
                      <li>{t("用 media/hero.png 做首帧，生成一段 6 秒的产品展示视频")}</li>
                      <li>{t("用 ffmpeg 把 media/clip.mp4 裁成 9:16 竖版并压到 10 MB 以内")}</li>
                      <li>{t("把分镜脚本写到 docs/，整理 media/ 里的成稿并更新项目说明")}</li>
                    </ul>
                  </div>
                ) : null}
                {events.map((ev) => (
                  <EventRow key={ev.id} wsId={wsId} ev={ev} file={files.find((file) => file.name === ev.title)} onOpenFile={openFromTimeline} />
                ))}
                {busy && state.current !== undefined ? (
                  <div className="max-w-[92%] space-y-1">
                    {state.live !== undefined && state.live.text !== "" ? <Markdown text={state.live.text} /> : null}
                    <p className="text-muted-foreground flex items-center gap-2 text-xs">
                      <LoaderCircle className="size-3 animate-spin" />
                      {state.status === "waiting"
                        ? t("等待设备上其它任务让出执行槽…")
                        : state.status === "starting"
                          ? t("正在启动引擎会话…")
                          : state.live?.activity !== undefined && state.live.activity !== ""
                            ? state.live.activity
                            : t("智能体正在处理…")}
                    </p>
                  </div>
                ) : null}
              </div>
              {state.queue.length > 0 || (busy && state.current !== undefined) ? (
                <div className="border-t px-4 py-2">
                  <p className="text-muted-foreground mb-1 text-[11px]">{t("队列 · 一条做完才做下一条")}</p>
                  <ul className="space-y-1">
                    {busy && state.current !== undefined ? (
                      <li className="flex items-center gap-2 text-xs">
                        <Badge variant="outline" className="text-signal-ok shrink-0">{t("执行中")}</Badge>
                        <span className="min-w-0 flex-1 truncate">{state.current.text}</span>
                        <Button size="sm" variant="ghost" onClick={() => void api.cancelStudioRun(wsId, chat.id, state.current?.id ?? "").catch((err: unknown) => toast.error(api.errorMessage(err)))}>
                          {t("中止")}
                        </Button>
                      </li>
                    ) : null}
                    {state.queue.map((run, i) => (
                      <li key={run.id} className="flex items-center gap-2 text-xs">
                        <Badge variant="outline" className="shrink-0">{t("第 {n} 位", { n: i + 1 })}</Badge>
                        <span className="min-w-0 flex-1 truncate">{run.text}</span>
                        <Button size="sm" variant="ghost" onClick={() => void api.cancelStudioRun(wsId, chat.id, run.id).catch((err: unknown) => toast.error(api.errorMessage(err)))}>
                          {t("取消")}
                        </Button>
                      </li>
                    ))}
                  </ul>
                </div>
              ) : null}
              {archived ? (
                <p className="text-muted-foreground border-t px-4 py-3 text-center text-xs">
                  {t("对话已于 {time} 归档：只能查看，不能再下指令。", { time: fmtTime(chat.archived_at ?? "") })}
                </p>
              ) : (
                <Composer
                  key={chat.id}
                  draft={draft}
                  onDraftChange={setDraft}
                  disabled={composerDisabled}
                  disabledReason={disabledReason}
                  busyLabel={busy ? t("有指令正在执行：现在提交的会排在后面。") : undefined}
                  onSubmit={(text, images) =>
                    api.submitStudioRun(wsId, chat.id, text, images).then(
                      (r) => {
                        setState(r.state);
                        stickRef.current = true;
                      },
                      (err: unknown) => {
                        toast.error(api.errorMessage(err));
                        throw err;
                      },
                    )
                  }
                />
              )}
            </>
          )}
        </Card>
        <Card className="flex min-h-0 flex-col gap-2 p-3">
          <FilesPanel wsId={wsId} files={files} loading={filesLoading} onChanged={reloadFiles} onCite={cite} openName={openFile} onOpenChange={setOpenFile} />
        </Card>
      </div>
      {creating ? (
        <StudioNewChatDialog
          ws={ws}
          onClose={() => setCreating(false)}
          onCreated={(created) => {
            setPage((cur) => (cur === null ? cur : { ...cur, chats: [created, ...cur.chats] }));
            selectChat(created.id);
          }}
        />
      ) : null}
    </div>
  );
}
