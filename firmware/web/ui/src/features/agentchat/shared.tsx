// 智能体对话的公共零件：Agent远控页与创作工作空间页共用——Markdown（./markdown.tsx）、时间线里的可折叠块、
// 输入盒（文本 + 贴图）、对话列表、推理档位与会话状态文案、事件合并；新建对话对话框只给 Agent远控用
// （创作工作空间另有选引擎的那一个，features/studio/new-chat.tsx）。两页各自只保留自己的时间线渲染与侧栏。

import { Archive, ArrowUp, ChevronDown, ChevronRight, ImagePlus, LoaderCircle, Plus, Trash2, X } from "lucide-react";
import { useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectGroup, SelectItem, SelectLabel, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { Link } from "@/lib/router";

const MAX_IMAGES = 5;
const MAX_IMAGE_BYTES = 8 << 20;

export { Markdown } from "./markdown";

// ---- 时间线零件 ----

export function parseMeta(meta: string | undefined): Record<string, unknown> {
  if (meta === undefined || meta === "") return {};
  try {
    return JSON.parse(meta) as Record<string, unknown>;
  } catch {
    return {};
  }
}

export function Collapsible({ summary, children, defaultOpen = false }: { summary: React.ReactNode; children: React.ReactNode; defaultOpen?: boolean }): React.ReactElement {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <div>
      <button type="button" className="text-muted-foreground hover:text-foreground flex items-center gap-1 text-xs" onClick={() => setOpen((v) => !v)}>
        {open ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
        {summary}
      </button>
      {open ? children : null}
    </div>
  );
}

export function OutputBlock({ text }: { text: string }): React.ReactElement {
  return <pre className="bg-muted/60 mt-1 max-w-full overflow-x-auto rounded-md p-2 font-mono text-xs whitespace-pre-wrap">{text}</pre>;
}

export function exitTone(code: number | undefined): string {
  if (code === undefined) return "";
  return code === 0 ? "text-signal-ok" : "text-signal-alert";
}

export function runStatusLabel(status: api.AgentRunStatus): string {
  switch (status) {
    case "queued":
      return t("排队中");
    case "running":
      return t("执行中");
    case "succeeded":
      return t("已完成");
    case "failed":
      return t("失败");
    case "cancelled":
      return t("已取消");
  }
}

// ---- 输入框 ----

interface Attachment {
  name: string;
  dataUrl: string;
}

function readImage(file: File): Promise<Attachment> {
  return new Promise((resolve, reject) => {
    if (!file.type.startsWith("image/")) {
      reject(new Error(t("只能附加图片文件")));
      return;
    }
    if (file.size > MAX_IMAGE_BYTES) {
      reject(new Error(t("单张图片不能超过 {n} MiB", { n: MAX_IMAGE_BYTES >> 20 })));
      return;
    }
    const reader = new FileReader();
    reader.onload = () => resolve({ name: file.name, dataUrl: String(reader.result) });
    reader.onerror = () => reject(new Error(t("读取图片失败")));
    reader.readAsDataURL(file);
  });
}

/**
 * 输入盒。draft / onDraftChange 可选：给了就是受控草稿（创作工作空间从素材库把文件名填进来），
 * 不给就自己管。
 */
export function Composer({ disabled, disabledReason, busyLabel, draft, onDraftChange, onSubmit }: {
  disabled: boolean;
  disabledReason?: string | undefined;
  busyLabel?: string | undefined;
  draft?: string | undefined;
  onDraftChange?: ((text: string) => void) | undefined;
  onSubmit: (text: string, images: string[]) => Promise<void>;
}): React.ReactElement {
  const [ownText, setOwnText] = useState("");
  const text = draft ?? ownText;
  const setText = useCallback(
    (next: string) => {
      setOwnText(next);
      onDraftChange?.(next);
    },
    [onDraftChange],
  );
  const [images, setImages] = useState<Attachment[]>([]);
  const [sending, setSending] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const addFiles = useCallback((files: FileList | File[]) => {
    const list = Array.from(files);
    void (async () => {
      const added: Attachment[] = [];
      for (const f of list) {
        try {
          added.push(await readImage(f));
        } catch (err) {
          toast.error(err instanceof Error ? err.message : String(err));
        }
      }
      setImages((cur) => {
        const merged = [...cur, ...added];
        if (merged.length > MAX_IMAGES) toast.error(t("一条指令最多附 {n} 张图片", { n: MAX_IMAGES }));
        return merged.slice(0, MAX_IMAGES);
      });
    })();
  }, []);

  const submit = () => {
    const trimmed = text.trim();
    if (disabled || sending || (trimmed === "" && images.length === 0)) return;
    setSending(true);
    onSubmit(trimmed, images.map((i) => i.dataUrl)).then(
      () => {
        setSending(false);
        setText("");
        setImages([]);
      },
      () => setSending(false),
    );
  };

  const canSend = !disabled && !sending && (text.trim() !== "" || images.length > 0);
  // 一个带边框的输入盒：缩略图、文本框、底部工具行（附图在左、发送在右）都在盒子里，
  // 焦点环打在盒子上而不是文本框上。
  return (
    <div className="border-t p-3">
      <div className="bg-background dark:bg-input/30 border-input flex flex-col gap-1 rounded-lg border px-2 pt-1 pb-1.5 shadow-xs transition-[color,box-shadow] focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50">
        {images.length > 0 ? (
          <div className="flex flex-wrap gap-2 px-1 pt-1.5">
            {images.map((img, i) => (
              <div key={`${img.name}-${i}`} className="relative">
                <img src={img.dataUrl} alt={img.name} className="size-14 rounded-md border object-cover" />
                <button type="button" aria-label={t("移除图片")} className="bg-background absolute -top-1.5 -right-1.5 rounded-full border p-0.5" onClick={() => setImages((cur) => cur.filter((_, j) => j !== i))}>
                  <X className="size-3" />
                </button>
              </div>
            ))}
          </div>
        ) : null}
        <input ref={fileRef} type="file" accept="image/*" multiple hidden onChange={(e) => { if (e.target.files) addFiles(e.target.files); e.target.value = ""; }} />
        <Textarea
          value={text}
          disabled={disabled || sending}
          placeholder={disabled && disabledReason !== undefined ? disabledReason : t("给智能体下一条指令；Enter 发送，Shift+Enter 换行，可粘贴图片")}
          className="max-h-48 min-h-9 rounded-none border-0 bg-transparent px-1 py-1.5 shadow-none focus-visible:border-transparent focus-visible:ring-0 dark:bg-transparent"
          rows={1}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault();
              submit();
            }
          }}
          onPaste={(e) => {
            const files = Array.from(e.clipboardData.items).filter((it) => it.kind === "file").map((it) => it.getAsFile()).filter((f): f is File => f !== null);
            if (files.length > 0) {
              e.preventDefault();
              addFiles(files);
            }
          }}
        />
        <div className="flex items-center gap-1">
          <Button type="button" size="icon-sm" variant="ghost" className="text-muted-foreground" title={t("附加图片")} disabled={disabled || sending} onClick={() => fileRef.current?.click()}>
            <ImagePlus />
          </Button>
          {busyLabel !== undefined ? <span className="text-muted-foreground min-w-0 truncate text-[11px]">{busyLabel}</span> : null}
          <Button type="button" size="icon-sm" className="ml-auto rounded-full" title={t("发送")} disabled={!canSend} onClick={submit}>
            {sending ? <LoaderCircle className="animate-spin" /> : <ArrowUp />}
          </Button>
        </div>
      </div>
    </div>
  );
}

// ---- 新建对话 ----

const DEFAULT_MODEL = "__default__";
const DEFAULT_EFFORT = "__default__";

export function effortLabel(effort: string): string {
  switch (effort) {
    case "low":
      return t("低");
    case "medium":
      return t("中");
    case "high":
      return t("高");
    case "xhigh":
      return t("极高");
    case "max":
      return t("最高");
    default:
      return t("引擎缺省");
  }
}

/** 密钥选项的后缀：为什么选不了 / 订阅现状。 */
function keySuffix(k: api.AgentKeyOption): string {
  if (k.disabled) return t("已停用");
  if (!k.plaintext_available) return t("无封存明文");
  if (!k.codex_enabled) return t("未授权 Codex，也无开发工具可见的模型");
  if (k.codex_configured && !k.codex_available) return t("订阅暂不可用");
  if (!k.codex_configured) return t("仅目录模型");
  return "";
}

function keyLabel(k: api.AgentKeyOption): string {
  const suffix = keySuffix(k);
  return (k.label === "" ? k.display : `${k.label} · ${k.display}`) + (suffix === "" ? "" : ` · ${suffix}`);
}

/**
 * Agent远控的新建对话对话框：标题、API 密钥、模型（按密钥分组列出订阅自带与目录模型）、推理档位。
 * loadOptions 取可选项，create 落对话。
 */
export function NewChatDialog<C>({ intro, loadOptions, create, onClose, onCreated }: {
  intro: string;
  loadOptions: () => Promise<api.AgentChatOptions>;
  create: (input: { title: string; key_id: number; model: string; effort: string }) => Promise<C>;
  onClose: () => void;
  onCreated: (chat: C) => void;
}): React.ReactElement {
  const [options, setOptions] = useState<api.AgentChatOptions | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [keyId, setKeyId] = useState<string>("");
  const [model, setModel] = useState<string>(DEFAULT_MODEL);
  const [effort, setEffort] = useState<string>(DEFAULT_EFFORT);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // loadOptions 每次渲染都是新函数：只在挂载时取一次。
  const loadRef = useRef(loadOptions);
  loadRef.current = loadOptions;
  useEffect(() => {
    let alive = true;
    loadRef.current().then(
      (o) => {
        if (!alive) return;
        setOptions(o);
        // 只有一把能用的密钥就直接选上。
        const usable = o.keys.filter((k) => k.codex_enabled && !k.disabled && k.plaintext_available);
        if (usable.length === 1) setKeyId(String(usable[0]?.id ?? ""));
      },
      (err: unknown) => {
        if (alive) setLoadError(api.errorMessage(err));
      },
    );
    return () => {
      alive = false;
    };
  }, []);

  const selectedKey = options?.keys.find((k) => String(k.id) === keyId);
  const subscriptionModels = selectedKey?.models.filter((m) => m.source === "subscription") ?? [];
  const catalogModels = selectedKey?.models.filter((m) => m.source === "catalog") ?? [];

  function submit(event: React.FormEvent): void {
    event.preventDefault();
    if (keyId === "") {
      setError(t("请选择 API 密钥"));
      return;
    }
    setBusy(true);
    setError(null);
    create({ title: title.trim(), key_id: Number(keyId), model: model === DEFAULT_MODEL ? "" : model, effort: effort === DEFAULT_EFFORT ? "" : effort }).then(
      (chat) => {
        onCreated(chat);
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
            <DialogTitle>{t("新建对话")}</DialogTitle>
          </DialogHeader>
          <p className="text-muted-foreground text-xs leading-relaxed">{intro}</p>
          {loadError !== null ? <p className="text-signal-alert text-sm">{loadError}</p> : null}
          {options === null ? (
            loadError === null ? <p className="text-muted-foreground text-sm">{t("加载中…")}</p> : null
          ) : (
            <>
              {options.engine.ready ? null : <p className="text-signal-alert text-xs">{options.engine.reason ?? t("智能体引擎未就绪")}</p>}
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="chat-title">{t("标题（可选）")}</Label>
                <Input id="chat-title" value={title} maxLength={80} disabled={busy} placeholder={t("留空则用第一条指令的首行")} onChange={(e) => setTitle(e.target.value)} />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label>{t("API 密钥")}</Label>
                <Select
                  value={keyId}
                  disabled={busy}
                  onValueChange={(v) => {
                    setKeyId(v);
                    setModel(DEFAULT_MODEL);
                  }}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue placeholder={t("未选择")} />
                  </SelectTrigger>
                  <SelectContent>
                    {options.keys.map((k) => (
                      <SelectItem key={k.id} value={String(k.id)} disabled={!k.codex_enabled || k.disabled || !k.plaintext_available}>
                        {keyLabel(k)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {options.keys.length === 0 ? (
                  <Link to="/keys" className="text-xs underline">{t("还没有 API 密钥，去创建")}</Link>
                ) : selectedKey !== undefined && !selectedKey.codex_enabled ? (
                  <Link to="/keys" className="text-xs underline">{t("去为这把密钥授权 Codex 订阅或勾选开发工具可见的模型")}</Link>
                ) : null}
              </div>
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="flex flex-col gap-1.5">
                  <Label>{t("模型")}</Label>
                  <Select value={model} disabled={busy || keyId === ""} onValueChange={setModel}>
                    <SelectTrigger className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value={DEFAULT_MODEL}>
                        {selectedKey === undefined || selectedKey.default_model === "" ? t("缺省模型") : t("缺省（{model}）", { model: selectedKey.default_model })}
                      </SelectItem>
                      {subscriptionModels.length > 0 ? (
                        <SelectGroup>
                          <SelectLabel>{t("Codex 订阅自带")}</SelectLabel>
                          {subscriptionModels.map((m) => (
                            <SelectItem key={m.name} value={m.name}>{m.name}</SelectItem>
                          ))}
                        </SelectGroup>
                      ) : null}
                      {catalogModels.length > 0 ? (
                        <SelectGroup>
                          <SelectLabel>{t("目录模型")}</SelectLabel>
                          {catalogModels.map((m) => (
                            <SelectItem key={m.name} value={m.name}>{m.name}</SelectItem>
                          ))}
                        </SelectGroup>
                      ) : null}
                    </SelectContent>
                  </Select>
                  {selectedKey !== undefined && selectedKey.models.length === 0 ? (
                    <p className="text-muted-foreground text-xs">
                      {selectedKey.codex_configured && !selectedKey.codex_available
                        ? t("钉死的 Codex 订阅账号暂不可用，订阅自带的模型此刻不可选。")
                        : t("这把密钥在 Codex 面还没有可见模型。")}
                    </p>
                  ) : null}
                </div>
                <div className="flex flex-col gap-1.5">
                  <Label>{t("推理档位")}</Label>
                  <Select value={effort} disabled={busy} onValueChange={setEffort}>
                    <SelectTrigger className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value={DEFAULT_EFFORT}>{effortLabel("")}</SelectItem>
                      {options.efforts.map((e) => (
                        <SelectItem key={e} value={e}>{effortLabel(e)} · {e}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              </div>
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
            <Button type="submit" disabled={busy || options === null || !options.engine.ready || keyId === ""}>
              {busy ? <LoaderCircle className="animate-spin" /> : null}
              {t("新建")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 对话列表 ----

/** 对话列表认的形状：Agent远控与创作工作空间的对话都满足。 */
export interface ChatLike {
  id: string;
  title: string;
  status: api.AgentSessionStatus;
  busy: boolean;
  model: string;
  effort: string;
  updated_at: string;
  /** 有值即已归档（只有支持归档的页面会带）：排在列表末尾的「已归档」一组里，只能查看。 */
  archived_at?: string;
}

function chatStatusTone(chat: ChatLike): string {
  if (chat.status === "running") return "text-signal-ok";
  if (chat.busy) return "";
  return "text-muted-foreground";
}

export function chatTitle(chat: { title: string }): string {
  return chat.title === "" ? t("未命名对话") : chat.title;
}

export function ChatList<C extends ChatLike>({ chats, activeId, onSelect, onCreate, onDelete, onArchive }: { chats: C[]; activeId: string | null; onSelect: (id: string) => void; onCreate: () => void; onDelete: (chat: C) => void; /** 给了才显示归档按钮（创作工作空间）。 */ onArchive?: (chat: C) => void }): React.ReactElement {
  const live = chats.filter((c) => c.archived_at === undefined);
  const archived = chats.filter((c) => c.archived_at !== undefined);
  const [showArchived, setShowArchived] = useState(false);
  // 正在看的对话在归档组里时，这一组保持展开。
  const archivedOpen = showArchived || archived.some((c) => c.id === activeId);
  const item = (chat: C) => {
    const active = chat.id === activeId;
    const isArchived = chat.archived_at !== undefined;
    return (
      <li key={chat.id} className="shrink-0 lg:shrink">
        <div
          role="button"
          tabIndex={0}
          onClick={() => onSelect(chat.id)}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              onSelect(chat.id);
            }
          }}
          className={cn("group flex w-56 cursor-pointer flex-col gap-0.5 rounded-md border px-2.5 py-1.5 text-left lg:w-full", active ? "bg-primary/10 border-primary/40" : "hover:bg-muted/60", isArchived && !active ? "opacity-70" : "")}
        >
          <div className="flex items-center gap-1">
            <span className="min-w-0 flex-1 truncate text-xs font-medium">{chatTitle(chat)}</span>
            {chat.status === "running" || chat.busy ? <LoaderCircle className={cn("size-3 shrink-0 animate-spin", chatStatusTone(chat))} /> : null}
            {onArchive !== undefined && !isArchived ? (
              <button
                type="button"
                aria-label={t("归档对话 {title}", { title: chatTitle(chat) })}
                title={t("归档")}
                className="text-muted-foreground hover:text-foreground shrink-0 rounded p-0.5 opacity-0 group-hover:opacity-100 focus:opacity-100"
                onClick={(e) => {
                  e.stopPropagation();
                  onArchive(chat);
                }}
              >
                <Archive className="size-3" />
              </button>
            ) : null}
            <button
              type="button"
              aria-label={t("删除对话 {title}", { title: chatTitle(chat) })}
              className="text-muted-foreground hover:text-signal-alert shrink-0 rounded p-0.5 opacity-0 group-hover:opacity-100 focus:opacity-100"
              onClick={(e) => {
                e.stopPropagation();
                onDelete(chat);
              }}
            >
              <Trash2 className="size-3" />
            </button>
          </div>
          <div className="text-muted-foreground flex items-center gap-1 text-[10px]">
            <span className="truncate font-mono">{chat.model}</span>
            {chat.effort !== "" ? <span className="shrink-0">· {chat.effort}</span> : null}
          </div>
          <span className="text-muted-foreground text-[10px] tabular-nums">{fmtTime(chat.updated_at)}</span>
        </div>
      </li>
    );
  };
  return (
    <Card className="flex min-h-0 flex-col gap-0 overflow-hidden p-0 lg:h-full">
      <div className="flex items-center gap-2 border-b px-3 py-2">
        <span className="text-xs font-medium">{t("对话")}</span>
        <span className="text-muted-foreground text-[11px]">{t("{n} 个", { n: live.length })}</span>
        <Button size="sm" variant="outline" className="ml-auto" onClick={onCreate}>
          <Plus />
          {t("新建")}
        </Button>
      </div>
      {chats.length === 0 ? (
        <p className="text-muted-foreground px-3 py-3 text-xs">{t("还没有对话。新建一个，选好 API 密钥与模型再开始。")}</p>
      ) : (
        <ul className="flex min-h-0 gap-1 overflow-x-auto p-2 lg:flex-1 lg:flex-col lg:overflow-x-hidden lg:overflow-y-auto">
          {live.map(item)}
          {archived.length > 0 ? (
            <li className="shrink-0 self-center lg:self-auto lg:pt-1">
              <button type="button" aria-expanded={archivedOpen} className="text-muted-foreground hover:text-foreground flex items-center gap-1 px-1 text-[11px]" onClick={() => setShowArchived((v) => !v)}>
                {archivedOpen ? <ChevronDown className="size-3" /> : <ChevronRight className="size-3" />}
                {t("已归档 {n} 个", { n: archived.length })}
              </button>
            </li>
          ) : null}
          {archivedOpen ? archived.map(item) : null}
        </ul>
      )}
    </Card>
  );
}

// ---- 会话状态与事件合并 ----

export function sessionLabel(status: api.AgentSessionStatus): { label: string; tone: string } {
  switch (status) {
    case "running":
      return { label: t("执行中"), tone: "text-signal-ok" };
    case "waiting":
      return { label: t("等待执行"), tone: "" };
    case "starting":
      return { label: t("启动引擎"), tone: "" };
    case "stopping":
      return { label: t("正在结束"), tone: "" };
    default:
      return { label: t("空闲"), tone: "" };
  }
}

/** 把新事件并进列表：按 id 去重、保持升序。 */
export function mergeEvents<T extends { id: number }>(cur: T[], incoming: T[]): T[] {
  if (incoming.length === 0) return cur;
  const seen = new Set(cur.map((e) => e.id));
  const added = incoming.filter((e) => !seen.has(e.id));
  if (added.length === 0) return cur;
  return [...cur, ...added].sort((a, b) => a.id - b.id);
}
