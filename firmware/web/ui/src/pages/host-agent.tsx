// 智能体 · Agent远控：用智能体管理一台已纳管的主机 / SoC。
//
// 四块：
//   - 对话列表（左）：一台主机可开多个对话。新建时选 API 密钥、模型、推理档位（之后不改），
//     并注入当时的主机档案与最近操作；可删（只带走对话内容，对主机的操作记录留在操作日志里）。
//   - 对话（中）：时间线（指令、回复、在主机上执行的每条命令 / 写过的文件 / 档案变更、会话
//     边界）+ 排队中的指令 + 底部输入框（文本、可贴图片）。执行中再提交只会排队，一条做完
//     才做下一条；正在执行的只能中止。
//   - 侧栏（右）：「主机档案」（智能体维护的 Markdown，渲染显示，管理员可改）、「操作日志」（对
//     这台主机的全部操作，跨全部对话，可导出 .md）与「文件」（经 SSH 只读浏览主机的目录与文件
//     预览，features/agent-hosts/host-files.tsx，首次切到才读）；与对话之间的分界可拖动（宽屏左右、窄屏上下），
//     可向右收起成一条窄边，收起状态与分界位置记在 localStorage。
//   - 页头：左端「返回主机列表」，然后是主机、当前对话钉死的引擎 · 模型 · 档位 · 密钥、会话状态。
//     引擎会话由设备在空闲后自动关掉，页面上不提供结束会话的按钮。
//
// 读数：进页取一次整份（主机、对话列表、档案、操作日志），切到某个对话再取它的时间线，之后
// 按那个对话的会话 revision 陪等（GET …/chats/{id}/wait，30 秒一帧），有新事件 / 状态变化就回
// ——这是「人在页面上」的零轮询例外，页面关了陪等随之中止。对话 id 在查询串 `?chat=`。与模型
// 预览页一样不走 PageContainer 的整页滚动：页头与输入框常驻，只有列表、时间线与侧栏各自滚动。

import { ArrowLeft, Download, LoaderCircle, PanelRightClose, PanelRightOpen, Plus } from "lucide-react";
import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from "@/components/ui/resizable";
import { Textarea } from "@/components/ui/textarea";
import { SegTabs } from "@/features/access/access";
import { HostFilesPanel } from "@/features/agent-hosts/host-files";
import { ChatList, Collapsible, Composer, Markdown, NewChatDialog, OutputBlock, chatTitle, exitTone, mergeEvents, parseMeta, runStatusLabel, sessionLabel } from "@/features/agentchat/shared";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime, fmtTimeSeconds } from "@/lib/format";
import { t } from "@/lib/i18n";
import { Link, navigate, useLocationSearch } from "@/lib/router";


// ---- 时间线 ----

function EventRow({ ev }: { ev: api.AgentEvent }): React.ReactElement | null {
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
    case "command": {
      const truncated = meta["truncated"] === true;
      const failed = meta["error"] === true;
      return (
        <div className="max-w-[92%] rounded-md border px-3 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <code className="font-mono text-xs break-all">$ {ev.title}</code>
            {failed ? (
              <Badge variant="outline" className="text-signal-alert">{t("未能执行")}</Badge>
            ) : (
              <Badge variant="outline" className={exitTone(ev.exit_code)}>exit {ev.exit_code ?? "?"}</Badge>
            )}
            {ev.duration_ms !== undefined && ev.duration_ms > 0 ? <span className="text-muted-foreground text-[10px]">{(ev.duration_ms / 1000).toFixed(1)}s</span> : null}
            {time}
          </div>
          {ev.body !== undefined && ev.body !== "" ? (
            <Collapsible summary={truncated ? t("输出（已截断）") : t("输出")} defaultOpen={failed || (ev.exit_code !== undefined && ev.exit_code !== 0)}>
              <OutputBlock text={ev.body} />
            </Collapsible>
          ) : (
            <p className="text-muted-foreground mt-1 text-[10px]">{t("（无输出）")}</p>
          )}
        </div>
      );
    }
    case "file": {
      const bytes = typeof meta["bytes"] === "number" ? (meta["bytes"] as number) : 0;
      const sudo = meta["sudo"] === true;
      const failed = meta["error"] === true || (ev.exit_code !== undefined && ev.exit_code !== 0);
      return (
        <div className="max-w-[92%] rounded-md border px-3 py-2">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-xs">{meta["append"] === true ? t("追加写入") : t("写入文件")}</span>
            <code className="font-mono text-xs break-all">{ev.title}</code>
            <span className="text-muted-foreground text-[10px]">{t("{n} 字节", { n: bytes })}</span>
            {sudo ? <Badge variant="outline">sudo</Badge> : null}
            {failed ? <Badge variant="outline" className="text-signal-alert">{t("失败")}</Badge> : null}
            {time}
          </div>
          {ev.body !== undefined && ev.body !== "" ? (
            <Collapsible summary={failed ? t("错误") : t("内容")} defaultOpen={failed}>
              <OutputBlock text={ev.body} />
            </Collapsible>
          ) : null}
        </div>
      );
    }
    case "profile":
      return (
        <div className="max-w-[92%] rounded-md border border-dashed px-3 py-2">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <span>{ev.title === "admin" ? t("管理员更新了主机档案") : t("智能体更新了主机档案")}</span>
            {time}
          </div>
          <Collapsible summary={t("查看档案")}>
            <div className="bg-muted/30 mt-1 rounded-md border p-3">
              <Markdown text={ev.body ?? ""} />
            </div>
          </Collapsible>
        </div>
      );
    case "run":
      return (
        <div className="flex items-center gap-2 text-xs">
          <Badge variant="outline" className={ev.title === "failed" ? "text-signal-alert" : ""}>
            {ev.title === "failed" ? t("指令失败") : ev.title === "cancelled" ? t("指令已取消") : ev.title}
          </Badge>
          <span className="text-muted-foreground">{ev.reason ?? ev.body}</span>
          {time}
        </div>
      );
    case "session":
      return (
        <div className="text-muted-foreground flex items-center gap-2 text-[11px]">
          <span className="bg-border h-px flex-1" />
          <span>{ev.title === "started" ? t("会话开始 · {engine}", { engine: ev.body ?? "" }) : t("会话结束 · {reason}", { reason: ev.reason ?? ev.body ?? "" })}</span>
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

// ---- 操作日志导出 ----

function opsMarkdown(host: api.AgentHost, events: api.AgentEvent[]): string {
  const lines: string[] = [`# ${t("操作日志")} · ${host.name === "" ? host.address : host.name}`, "", `${host.username}@${host.address}:${host.port}`, ""];
  for (const ev of events) {
    const at = fmtTimeSeconds(ev.at);
    switch (ev.kind) {
      case "user":
        lines.push(`- ${at} **${t("指令")}**：${(ev.body ?? "").replace(/\n/g, " ")}`);
        break;
      case "command":
        lines.push(`- ${at} **exec** \`${ev.title ?? ""}\` → exit ${ev.exit_code ?? "?"}${ev.duration_ms ? ` (${(ev.duration_ms / 1000).toFixed(1)}s)` : ""}`);
        if (ev.body) lines.push("", "  ```", ...ev.body.split("\n").map((l) => "  " + l), "  ```", "");
        break;
      case "file":
        lines.push(`- ${at} **write_file** \`${ev.title ?? ""}\`${ev.exit_code !== undefined && ev.exit_code !== 0 ? ` (${t("失败")})` : ""}`);
        break;
      case "profile":
        lines.push(`- ${at} **${t("主机档案")}**（${ev.title ?? ""}）`);
        break;
      case "run":
        lines.push(`- ${at} **${runStatusLabel(ev.title as api.AgentRunStatus)}** ${ev.reason ?? ev.body ?? ""}`);
        break;
      case "session":
        lines.push(`- ${at} **${ev.title === "started" ? t("会话开始") : t("会话结束")}** ${ev.title === "started" ? (ev.body ?? "") : (ev.reason ?? ev.body ?? "")}`);
        break;
      default:
        break;
    }
  }
  return lines.join("\n") + "\n";
}

// ---- 侧栏：主机档案 / 操作日志（「文件」页签在 features/agent-hosts/host-files.tsx）----

type SideTab = "profile" | "ops" | "files";

const OPS_KINDS: api.AgentEventKind[] = ["command", "file", "profile", "run", "session", "user"];

function ProfilePanel({ hostId, profile, onSaved }: { hostId: number; profile: api.AgentProfile; onSaved: (p: api.AgentProfile) => void }): React.ReactElement {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-muted-foreground text-xs">
          {profile.updated_at === undefined || profile.updated_at === "" || profile.updated_at.startsWith("0001")
            ? t("还没有档案：智能体完成第一次任务后会建立。")
            : t("更新于 {time} · {by}", { time: fmtTime(profile.updated_at), by: profile.updated_by === "admin" ? t("管理员") : t("智能体") })}
        </span>
        <div className="ml-auto flex gap-1">
          {editing ? (
            <>
              <Button size="sm" variant="outline" disabled={busy} onClick={() => setEditing(false)}>
                {t("取消")}
              </Button>
              <Button
                size="sm"
                disabled={busy}
                onClick={() => {
                  setBusy(true);
                  api.putHostAgentProfile(hostId, draft).then(
                    (r) => {
                      setBusy(false);
                      setEditing(false);
                      onSaved(r.profile);
                      toast(t("主机档案已保存"));
                    },
                    (err: unknown) => {
                      setBusy(false);
                      toast.error(api.errorMessage(err));
                    },
                  );
                }}
              >
                {busy ? <LoaderCircle className="animate-spin" /> : null}
                {t("保存")}
              </Button>
            </>
          ) : (
            <>
              <Button size="sm" variant="outline" disabled={profile.content === ""} onClick={() => api.downloadText(`host-profile-${hostId}.md`, profile.content)}>
                <Download />
                {t("导出")}
              </Button>
              <Button size="sm" variant="outline" onClick={() => { setDraft(profile.content); setEditing(true); }}>
                {t("编辑")}
              </Button>
            </>
          )}
        </div>
      </div>
      {editing ? (
        <Textarea className="min-h-0 flex-1 resize-none font-mono text-xs" value={draft} disabled={busy} onChange={(e) => setDraft(e.target.value)} spellCheck={false} />
      ) : (
        <div className="min-h-0 flex-1 overflow-y-auto rounded-md border p-3">
          {profile.content === "" ? <p className="text-muted-foreground text-xs">{t("（空）")}</p> : <Markdown text={profile.content} />}
        </div>
      )}
    </div>
  );
}

function OpsPanel({ host, ops, onLoadOlder, exhausted }: { host: api.AgentHost; ops: api.AgentEvent[]; onLoadOlder: () => Promise<void>; exhausted: boolean }): React.ReactElement {
  const [loading, setLoading] = useState(false);
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-muted-foreground text-xs">{t("对这台主机的全部操作（跨全部对话），按时间倒序。")}</span>
        <Button size="sm" variant="outline" className="ml-auto" disabled={ops.length === 0} onClick={() => api.downloadText(`host-ops-${host.id}.md`, opsMarkdown(host, ops))}>
          <Download />
          {t("导出 .md")}
        </Button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto rounded-md border">
        {ops.length === 0 ? (
          <p className="text-muted-foreground p-3 text-xs">{t("还没有操作记录")}</p>
        ) : (
          <ul className="divide-y">
            {[...ops].reverse().map((ev) => (
              <li key={ev.id} className="flex flex-col gap-0.5 px-3 py-2 text-xs">
                <div className="flex items-center gap-2">
                  <span className="text-muted-foreground shrink-0 tabular-nums">{fmtTimeSeconds(ev.at)}</span>
                  <Badge variant="outline" className={cn("shrink-0", ev.kind === "command" ? exitTone(ev.exit_code) : "")}>
                    {ev.kind === "command" ? "exec" : ev.kind === "file" ? "write_file" : ev.kind === "profile" ? t("档案") : ev.kind === "user" ? t("指令") : ev.kind === "run" ? runStatusLabel(ev.title as api.AgentRunStatus) : t("会话")}
                  </Badge>
                </div>
                <span className="font-mono break-all">
                  {ev.kind === "command" || ev.kind === "file" ? ev.title : ev.kind === "user" ? (ev.body ?? "").split("\n")[0] : ev.kind === "session" && ev.title === "started" ? ev.body : (ev.reason ?? ev.body)}
                  {ev.kind === "command" && ev.exit_code !== undefined ? ` → exit ${ev.exit_code}` : ""}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>
      {exhausted ? null : (
        <Button
          size="sm"
          variant="outline"
          disabled={loading}
          onClick={() => {
            setLoading(true);
            void onLoadOlder().finally(() => setLoading(false));
          }}
        >
          {loading ? <LoaderCircle className="animate-spin" /> : null}
          {t("加载更早的记录")}
        </Button>
      )}
    </div>
  );
}


// ---- 页头：返回按钮在左，然后是主机名与读数 ----

function HostHeader({ title, note }: { title: string; note?: React.ReactNode }): React.ReactElement {
  return (
    <header data-slot="page-header" className="bg-card flex min-h-12 w-full items-center gap-2 rounded-lg border px-2 py-1.5 sm:px-3">
      <Button size="icon-sm" variant="ghost" className="shrink-0" title={t("返回主机列表")} aria-label={t("返回主机列表")} onClick={() => navigate("/agent-hosts")}>
        <ArrowLeft />
      </Button>
      <div className="flex min-w-0 flex-1 flex-wrap items-baseline gap-x-3 gap-y-0.5">
        <h1 className="shrink-0 text-lg font-semibold tracking-tight">{title}</h1>
        {note === undefined ? null : <p className="text-muted-foreground min-w-0 text-xs leading-5">{note}</p>}
      </div>
    </header>
  );
}

// ---- 侧栏收起状态：记在 localStorage，读写失败都当作展开 ----

const SIDE_KEY = "llmgate.ui.hostAgentSide";

function readSideOpen(): boolean {
  try {
    return localStorage.getItem(SIDE_KEY) !== "closed";
  } catch {
    return true;
  }
}

function writeSideOpen(open: boolean): void {
  try {
    localStorage.setItem(SIDE_KEY, open ? "open" : "closed");
  } catch {
    // 无法持久化时只影响下次进页的缺省状态。
  }
}

// ---- 对话与侧栏的分界：拖动后记下两栏的百分比，宽屏（左右）与窄屏（上下）各记一份 ----

const SPLIT_KEY = "llmgate.ui.hostAgentSplit";

type Split = { chat: number; side: number };

function readSplit(wide: boolean): Split | undefined {
  try {
    const all = JSON.parse(localStorage.getItem(SPLIT_KEY) ?? "{}") as Record<string, Partial<Split>>;
    const v = all[wide ? "wide" : "narrow"];
    if (typeof v?.chat === "number" && typeof v.side === "number" && v.chat > 0 && v.side > 0) return { chat: v.chat, side: v.side };
  } catch {
    // 读不到就用缺省比例。
  }
  return undefined;
}

function writeSplit(wide: boolean, split: Split): void {
  try {
    const all = JSON.parse(localStorage.getItem(SPLIT_KEY) ?? "{}") as Record<string, Split>;
    all[wide ? "wide" : "narrow"] = split;
    localStorage.setItem(SPLIT_KEY, JSON.stringify(all));
  } catch {
    // 无法持久化时只影响下次进页的缺省比例。
  }
}

// 与 Tailwind 的 lg 断点一致：宽屏三栏并排，窄屏上下堆叠。
const WIDE_QUERY = "(min-width: 1024px)";

function subscribeWide(onChange: () => void): () => void {
  const mql = window.matchMedia(WIDE_QUERY);
  mql.addEventListener("change", onChange);
  return () => mql.removeEventListener("change", onChange);
}

function useWide(): boolean {
  return useSyncExternalStore(subscribeWide, () => window.matchMedia(WIDE_QUERY).matches);
}


export function HostAgentPage(): React.ReactElement {
  const search = useLocationSearch();
  const hostId = Number(search.get("host") ?? "");
  const chatParam = search.get("chat");
  const confirm = useConfirm();
  const [page, setPage] = useState<api.AgentPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [ops, setOps] = useState<api.AgentEvent[]>([]);
  const [opsExhausted, setOpsExhausted] = useState(false);
  const [chat, setChat] = useState<api.AgentChat | null>(null);
  const [chatError, setChatError] = useState<string | null>(null);
  const [events, setEvents] = useState<api.AgentEvent[]>([]);
  const [state, setState] = useState<api.AgentSessionState | null>(null);
  const [exhausted, setExhausted] = useState(false);
  const [tab, setTab] = useState<SideTab>("profile");
  // 「文件」页签首次切到才挂载（才去连主机），之后切走只隐藏，回来还是原来的目录与预览。
  const [filesOpened, setFilesOpened] = useState(false);
  const [sideOpen, setSideOpen] = useState(readSideOpen);
  const wide = useWide();
  const [creating, setCreating] = useState(false);
  const scrollRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);

  const selectChat = useCallback(
    (id: string | null) => {
      navigate("/host-agent", { search: id === null ? `?host=${hostId}` : `?host=${hostId}&chat=${id}` });
    },
    [hostId],
  );

  // 进页读一次整份：主机、对话列表、档案、操作日志。
  useEffect(() => {
    if (!Number.isInteger(hostId) || hostId <= 0) {
      setError(t("地址里没有主机：请从「主机/SoC」列表的「Agent远控」按钮进入。"));
      return;
    }
    let alive = true;
    setPage(null);
    setOps([]);
    setError(null);
    setOpsExhausted(false);
    api.getHostAgent(hostId).then(
      (p) => {
        if (!alive) return;
        setPage(p);
        setOps(p.ops);
        setOpsExhausted(p.ops.length < 200);
      },
      (err: unknown) => {
        if (alive) setError(api.errorMessage(err));
      },
    );
    return () => {
      alive = false;
    };
  }, [hostId]);

  // 地址里没指定对话时落到最近活动的那个。
  const activeChatId = chatParam !== null && chatParam !== "" ? chatParam : (page?.chats[0]?.id ?? null);

  // 切到某个对话：取它的时间线，然后陪等。
  useEffect(() => {
    setChat(null);
    setChatError(null);
    setEvents([]);
    setState(null);
    setExhausted(false);
    if (activeChatId === null || !Number.isInteger(hostId) || hostId <= 0) return;
    const chatId = activeChatId;
    const ctrl = new AbortController();
    let alive = true;
    api.getHostAgentChat(hostId, chatId).then(
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
              const r = await api.waitHostAgent(hostId, chatId, revision, after, ctrl.signal);
              if (!alive) return;
              revision = r.state.revision;
              setState(r.state);
              setChat((cur) => (cur === null ? cur : { ...cur, status: r.state.status, busy: r.state.current !== undefined || r.state.queue.length > 0 }));
              if (r.events.length > 0) {
                after = r.events[r.events.length - 1]?.id ?? after;
                setEvents((cur) => mergeEvents(cur, r.events));
                // 这个对话里对主机的操作也进操作日志；智能体改了档案，侧栏的档案视图跟着换。
                setOps((cur) => mergeEvents(cur, r.events.filter((e) => OPS_KINDS.includes(e.kind))));
                const changed = [...r.events].reverse().find((e) => e.kind === "profile");
                if (changed !== undefined) {
                  setPage((cur) =>
                    cur === null ? cur : { ...cur, profile: { ...cur.profile, content: changed.body ?? "", updated_by: changed.title ?? "", updated_at: changed.at } },
                  );
                }
                // 第一条指令给没标题的对话起了标题：列表跟着换。
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
  }, [hostId, activeChatId]);

  // 列表里的状态跟着当前对话的读数走。
  useEffect(() => {
    if (state === null) return;
    setPage((cur) =>
      cur === null ? cur : { ...cur, chats: cur.chats.map((c) => (c.id === state.chat_id ? { ...c, status: state.status, busy: state.current !== undefined || state.queue.length > 0 } : c)) },
    );
  }, [state]);

  // 贴底自动滚：人没往上翻时，新事件与流式正文到来就滚到底。
  useEffect(() => {
    const el = scrollRef.current;
    if (el !== null && stickRef.current) el.scrollTop = el.scrollHeight;
  }, [events, state?.live?.text, state?.live?.activity, state?.status]);

  const loadOlder = useCallback(async () => {
    if (events.length === 0 || activeChatId === null) return;
    try {
      const r = await api.getHostAgentChatEvents(hostId, activeChatId, { before: events[0]?.id ?? 0, limit: 200 });
      if (r.events.length < 200) setExhausted(true);
      setEvents((cur) => [...r.events, ...cur]);
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
  }, [events, hostId, activeChatId]);

  const loadOlderOps = useCallback(async () => {
    if (ops.length === 0) return;
    try {
      const r = await api.getHostAgentEvents(hostId, { before: ops[0]?.id ?? 0, limit: 200, ops: true });
      if (r.events.length < 200) setOpsExhausted(true);
      setOps((cur) => [...r.events, ...cur]);
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
  }, [ops, hostId]);

  const deleteChat = useCallback(
    (target: api.AgentChat) => {
      void confirm({
        title: t("删除对话「{title}」？", { title: chatTitle(target) }),
        body: (
          <>
            <p>{t("正在执行的指令会被中止，排队中的全部取消，引擎进程关闭；对话里的指令与回复删除。")}</p>
            <p>{t("智能体在主机上执行过的命令、写过的文件与档案变更仍留在这台主机的操作日志里。")}</p>
          </>
        ),
        confirmText: t("删除"),
      }).then((ok) => {
        if (!ok) return;
        api.deleteHostAgentChat(hostId, target.id).then(
          () => {
            toast(t("对话已删除"));
            setPage((cur) => (cur === null ? cur : { ...cur, chats: cur.chats.filter((c) => c.id !== target.id) }));
            if (target.id === activeChatId) selectChat(null);
          },
          (err: unknown) => toast.error(api.errorMessage(err)),
        );
      });
    },
    [confirm, hostId, activeChatId, selectChat],
  );

  if (error !== null) {
    return (
      <div className="flex h-full flex-col gap-3 p-4">
        <HostHeader title={t("Agent远控")} />
        <Card className="text-signal-alert p-5 text-sm">{error}</Card>
      </div>
    );
  }
  if (page === null) {
    return <p className="text-muted-foreground flex h-full items-center justify-center text-sm">{t("加载中…")}</p>;
  }
  const host = page.host;
  const engine = page.engine;
  const hostName = host.name === "" ? host.address : host.name;
  const hostReady = host.status === "ready";
  const status = state === null ? null : sessionLabel(state.status);
  const busy = state !== null && state.current !== undefined;
  const composerDisabled = chat === null || !engine.ready || !hostReady;
  const disabledReason = !engine.ready ? (engine.reason ?? t("智能体引擎未就绪")) : !hostReady ? t("主机当前不可用，先到主机列表检查连接") : chat === null ? t("先选择或新建一个对话") : undefined;

  return (
    <div className="flex h-full min-h-0 w-full flex-col gap-3 p-3 sm:p-4">
      <HostHeader
        title={hostName}
        note={
          <span className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <code className="font-mono text-xs">{host.username}@{host.address}:{host.port}</code>
            {chat === null ? (
              <span>{engine.label}</span>
            ) : (
              <span title={t("对话新建时钉死，不能修改")}>
                {engine.label} · {chat.model}{chat.effort !== "" ? ` · ${chat.effort}` : ""}{chat.key_display !== "" ? ` · ${chat.key_display}` : ""}
              </span>
            )}
            {status === null ? null : <Badge variant="outline" className={status.tone}>{status.label}</Badge>}
            {state?.engine_started_at !== undefined ? <span className="text-[11px]">{t("会话始于 {time}", { time: fmtTime(state.engine_started_at) })}</span> : null}
          </span>
        }
      />
      {engine.ready ? null : (
        <Card className="flex flex-wrap items-center gap-2 p-3 text-sm">
          <span className="text-signal-alert">{engine.reason ?? t("智能体引擎未就绪")}</span>
          <Link to="/components" className="ml-auto text-xs underline">{t("去「组件管理」安装")}</Link>
        </Card>
      )}
      <div className="grid min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] gap-3 lg:grid-cols-[13rem_minmax(0,1fr)] lg:grid-rows-1">
        <ChatList chats={page.chats} activeId={activeChatId} onSelect={(id) => selectChat(id)} onCreate={() => setCreating(true)} onDelete={deleteChat} />
        <div className="flex min-h-0 min-w-0 flex-col gap-3 lg:flex-row">
          <ResizablePanelGroup
            key={`${wide ? "wide" : "narrow"}-${sideOpen ? "open" : "closed"}`}
            orientation={wide ? "horizontal" : "vertical"}
            className="min-h-0 min-w-0 flex-1"
            defaultLayout={sideOpen ? readSplit(wide) : undefined}
            onLayoutChanged={(layout, meta) => {
              if (sideOpen && meta.isUserInteraction && layout.chat !== undefined && layout.side !== undefined) writeSplit(wide, { chat: layout.chat, side: layout.side });
            }}
          >
            <ResizablePanel id="chat" defaultSize={sideOpen ? "60%" : "100%"} minSize={wide ? "22rem" : "10rem"} className="flex min-h-0 min-w-0 flex-col" style={{ overflow: "visible" }}>
            <Card className="flex min-h-0 flex-1 flex-col gap-0 overflow-hidden p-0">
              {activeChatId === null ? (
                <div className="text-muted-foreground flex h-full flex-col items-center justify-center gap-3 p-4 text-sm">
                  <p>{t("新建一个对话开始：选好 API 密钥、模型与推理档位，智能体会带着这台主机的档案与最近操作开工。")}</p>
                  <Button size="sm" onClick={() => setCreating(true)}>
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
                    {events.length === 0 && !busy ? (
                      <div className="text-muted-foreground flex h-full flex-col items-center justify-center gap-2 text-sm">
                        <p>{t("还没有对话。给智能体下一条指令，比如：")}</p>
                        <ul className="list-disc text-xs">
                          <li>{t("看看这台机器的硬件、系统与磁盘使用情况，建立主机档案")}</li>
                          <li>{t("检查 SSH 与防火墙配置是否安全")}</li>
                          <li>{t("安装 Docker 并把当前用户加入 docker 组")}</li>
                        </ul>
                      </div>
                    ) : null}
                    {events.map((ev) => (
                      <EventRow key={ev.id} ev={ev} />
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
                            <Button size="sm" variant="ghost" onClick={() => void api.cancelHostAgentRun(hostId, chat.id, state.current?.id ?? "").catch((err: unknown) => toast.error(api.errorMessage(err)))}>
                              {t("中止")}
                            </Button>
                          </li>
                        ) : null}
                        {state.queue.map((run, i) => (
                          <li key={run.id} className="flex items-center gap-2 text-xs">
                            <Badge variant="outline" className="shrink-0">{t("第 {n} 位", { n: i + 1 })}</Badge>
                            <span className="min-w-0 flex-1 truncate">{run.text}</span>
                            <Button size="sm" variant="ghost" onClick={() => void api.cancelHostAgentRun(hostId, chat.id, run.id).catch((err: unknown) => toast.error(api.errorMessage(err)))}>
                              {t("取消")}
                            </Button>
                          </li>
                        ))}
                      </ul>
                    </div>
                  ) : null}
                  <Composer
                    disabled={composerDisabled}
                    disabledReason={disabledReason}
                    busyLabel={busy ? t("有指令正在执行：现在提交的会排在后面。") : undefined}
                    onSubmit={(text, images) =>
                      api.submitHostAgentRun(hostId, chat.id, text, images).then(
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
                </>
              )}
            </Card>
            </ResizablePanel>
            {sideOpen ? (
              <>
              <ResizableHandle
                withHandle
                title={t("拖动调整两栏大小")}
                className="data-[separator=active]:bg-border data-[separator=hover]:bg-border/60 w-3 rounded-full bg-transparent transition-colors aria-[orientation=horizontal]:h-3 aria-[orientation=horizontal]:w-full"
              />
              <ResizablePanel id="side" defaultSize="40%" minSize={wide ? "18rem" : "8rem"} className="flex min-h-0 min-w-0 flex-col" style={{ overflow: "visible" }}>
              <Card className="flex min-h-0 flex-1 flex-col gap-2 p-3">
                <div className="flex items-center gap-2">
                  <SegTabs
                    size="sm"
                    items={[
                      { key: "profile", label: t("主机档案") },
                      { key: "ops", label: t("操作日志") },
                      { key: "files", label: t("文件") },
                    ]}
                    active={tab}
                    onSelect={(k) => {
                      const next: SideTab = k === "ops" || k === "files" ? k : "profile";
                      if (next === "files") setFilesOpened(true);
                      setTab(next);
                    }}
                  />
                  <Button size="icon-sm" variant="ghost" className="text-muted-foreground ml-auto" title={t("收起侧栏")} aria-label={t("收起侧栏")} onClick={() => { setSideOpen(false); writeSideOpen(false); }}>
                    <PanelRightClose />
                  </Button>
                </div>
                {tab === "profile" ? (
                  <ProfilePanel hostId={hostId} profile={page.profile} onSaved={(p) => setPage({ ...page, profile: p })} />
                ) : tab === "ops" ? (
                  <OpsPanel host={host} ops={ops} onLoadOlder={loadOlderOps} exhausted={opsExhausted} />
                ) : null}
                {filesOpened ? <HostFilesPanel key={hostId} hostId={hostId} className={tab === "files" ? "" : "hidden"} /> : null}
              </Card>
              </ResizablePanel>
              </>
            ) : null}
          </ResizablePanelGroup>
          {sideOpen ? null : (
            <button
              type="button"
              title={t("展开侧栏")}
              className="bg-card text-muted-foreground hover:bg-muted/60 hover:text-foreground flex items-center gap-2 rounded-xl border px-2 py-1.5 text-xs shadow-sm lg:w-10 lg:flex-col lg:justify-start lg:px-0 lg:py-2"
              onClick={() => { setSideOpen(true); writeSideOpen(true); }}
            >
              <PanelRightOpen className="size-4 shrink-0" />
              <span className="lg:[writing-mode:vertical-rl]">{t("主机档案 · 操作日志 · 文件")}</span>
            </button>
          )}
        </div>
      </div>
      {creating ? (
        <NewChatDialog
          intro={t("对话新建时会注入当前的主机档案与最近的操作日志；下面选的 API 密钥、模型与推理档位在对话里不能再改。模型调用按这把密钥结算，可选的模型就是它在 Codex 面可见的模型。")}
          loadOptions={() => api.getHostAgentOptions(hostId)}
          create={(input) => api.createHostAgentChat(hostId, input).then((r) => r.chat)}
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

