// 工作空间终端面板：tmux 会话标签页（xterm.js ↔ WebSocket ↔ 设备 ↔ 守护进程）。只列本工作
// 空间的会话（prefix 前缀），没有会话时自动开 main。
//
// 会话名：第一个叫 main（tmux 会话 <prefix>-main），之后按 t1、t2、t3 顺序取最小的空号
// （<prefix>-t<n>），主机清单与已开标签里已占用的都算，不重名；标签上只显示 main / t<n>。
// 启动开发工具的会话同样按这个顺序取名，工具本身由探测标出。
//
// 会话清单每 5 秒重取一次（页面可见时）：守护进程沿每个 pane 的子进程树认出会话里正在
// 跑的开发工具（gate 拉起的五种之一，或直接运行的工具），标签上以短徽标标出，「+」菜单
// 里也标出该工具已在哪个标签里跑。这是管理台零轮询纪律的例外，只在本面板挂载期间
// 生效；终端连接是唯一的长连接。

import { CircleHelpIcon, PlusIcon, RefreshCwIcon, XIcon } from "lucide-react";
import { Suspense, lazy, useCallback, useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { t } from "@/lib/i18n";

import type { TerminalState } from "./terminal";

// xterm.js 只有终端用：按需加载，别让每次进管理台都下载它。
const TerminalView = lazy(() => import("./terminal").then((m) => ({ default: m.TerminalView })));

/** 会话清单（含工具探测）的重取间隔。 */
const POLL_MS = 5000;

interface OpenTab {
  name: string;
  /** 是否是主机上的 tmux 会话（true）还是没有 tmux 时的非持久 shell（false）。 */
  tmux: boolean;
  /** 标签被激活过才建终端连接：进页时会话全部列出，但只连人点到的那个。 */
  attached: boolean;
  state: TerminalState;
  /** 探测到会话里正在跑的开发工具。 */
  tool?: api.DevTool | undefined;
}

/** 会话名在标签上的显示名：去掉 <prefix>- 前缀；不带这个前缀的（旧会话）原样显示。 */
export function sessionLabel(prefix: string, name: string): string {
  return name.startsWith(`${prefix}-`) ? name.slice(prefix.length + 1) : name;
}

/** 本工作空间的会话：<prefix> 本身或 <prefix>-…。 */
export function sessionInScope(prefix: string, name: string): boolean {
  return name === prefix || name.startsWith(`${prefix}-`);
}

/** 下一个会话名：main 空着就用 main，否则 t1、t2… 里最小的空号。taken 是主机清单与已开标签里的名字。 */
export function nextSessionName(prefix: string, taken: Set<string>): string {
  const main = `${prefix}-main`;
  if (!taken.has(main)) return main;
  for (let n = 1; ; n += 1) {
    const candidate = `${prefix}-t${n}`;
    if (!taken.has(candidate)) return candidate;
  }
}

/** 标签排序：main 最前，t<n> 按数字，其余按名字。 */
function sessionOrder(prefix: string, a: string, b: string): number {
  const rank = (name: string): [number, number, string] => {
    const label = sessionLabel(prefix, name);
    if (label === "main") return [0, 0, label];
    const m = /^t(\d+)$/.exec(label);
    if (m !== null) return [1, Number(m[1]), label];
    return [2, 0, label];
  };
  const [ga, na, la] = rank(a);
  const [gb, nb, lb] = rank(b);
  if (ga !== gb) return ga - gb;
  if (na !== nb) return na - nb;
  return la < lb ? -1 : la > lb ? 1 : 0;
}

// 终端标签列表镜像主机上本工作空间的 tmux 会话：进页、刷新、切页回来与每次周期重取都把
// 会话对上，会话本身一直在主机后台跑；关标签 = 结束会话（要确认）。当前激活的标签名记在
// sessionStorage 里（storageKey），回来时接着看同一个。
function readActive(key: string): string | null {
  try {
    return window.sessionStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeActive(key: string, name: string | null): void {
  try {
    if (name === null) window.sessionStorage.removeItem(key);
    else window.sessionStorage.setItem(key, name);
  } catch {
    // 私密窗口等拿不到 sessionStorage：不记就是了。
  }
}

export function TerminalPanel({
  console: con,
  storageKey,
  dir,
  prefix,
}: {
  console: api.DevConsole;
  /** sessionStorage 里记「上次激活的标签」的键。 */
  storageKey: string;
  /** 新会话的起始目录。 */
  dir: string;
  /** 本工作空间的会话名前缀（api.workspaceSessionPrefix）：只列它名下的会话，新会话按 main / t<n> 取名。 */
  prefix: string;
}): React.ReactElement {
  const [sessions, setSessions] = useState<api.TmuxList | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [listLoading, setListLoading] = useState(true);
  const [tabs, setTabs] = useState<OpenTab[]>([]);
  const [active, setActiveState] = useState<string | null>(null);
  const confirm = useConfirm();

  // 最新的 tabs / active 镜像：对账与关标签要读它们，又不想让 effect 依赖它们自激。
  const tabsRef = useRef(tabs);
  tabsRef.current = tabs;
  const activeRef = useRef(active);
  activeRef.current = active;
  // 自动开 main 只在第一次拿到清单时做一次：人把最后一个关掉就该是空的。
  const autoDoneRef = useRef(false);
  // 只认最后一次发起的清单请求（切主机或手动刷新时旧结果不能盖新的）。
  const seqRef = useRef(0);

  // 取一次会话清单。manual 为真时转刷新图标；周期重取静默，不闪。
  const load = useCallback((manual: boolean) => {
    const mine = ++seqRef.current;
    if (manual) setListLoading(true);
    con.tmuxList().then(
      (data) => {
        if (mine !== seqRef.current) return;
        setSessions(data);
        setListError(null);
        setListLoading(false);
      },
      (err: unknown) => {
        if (mine !== seqRef.current) return;
        setListLoading(false);
        if (err instanceof api.ApiError && err.status === 401 && err.code === "unauthorized") return;
        setListError(api.errorMessage(err));
      },
    );
  }, [con]);

  // 进页取一次，之后页面可见时每 POLL_MS 重取一次（工具探测靠它）；切回前台立刻补一次。
  useEffect(() => {
    load(true);
    const tick = (): void => {
      if (document.visibilityState === "visible") load(false);
    };
    const id = window.setInterval(tick, POLL_MS);
    document.addEventListener("visibilitychange", tick);
    return () => {
      window.clearInterval(id);
      document.removeEventListener("visibilitychange", tick);
      seqRef.current += 1;
    };
  }, [load]);

  const setActive = useCallback((name: string | null) => {
    setActiveState(name);
    writeActive(storageKey, name);
    if (name !== null) setTabs((prev) => prev.map((x) => (x.name === name && !x.attached ? { ...x, attached: true } : x)));
  }, [storageKey]);

  // 每次拿到会话清单就把标签列表对上：主机上有而列表里没有的补进来，主机上已经没了的
  // 拿掉（非持久 shell 不在清单里，留着），探测到的工具写到标签上。没有激活标签时优先回到上次看的那个。
  useEffect(() => {
    const data = sessions;
    if (data === null) return;
    const visible = data.sessions.filter((s) => sessionInScope(prefix, s.name)).sort((a, b) => sessionOrder(prefix, a.name, b.name));
    const names = new Set(visible.map((s) => s.name));
    const tools = new Map(visible.map((s) => [s.name, s.tool] as const));
    // 一个会话都没有时自动开 main（attach 时 tmux 会顺手建会话；没有 tmux 就是非持久 shell）。
    const main = `${prefix}-main`;
    const auto = !autoDoneRef.current && visible.length === 0 && !tabsRef.current.some((x) => x.name === main) ? main : null;
    autoDoneRef.current = true;
    if (auto !== null) names.add(auto);
    const cur = activeRef.current;
    const curTab = tabsRef.current.find((x) => x.name === cur);
    let next = cur;
    if (cur === null || !(names.has(cur) || (curTab !== undefined && !curTab.tmux))) {
      const remembered = readActive(storageKey);
      next = remembered !== null && names.has(remembered) ? remembered : (auto ?? visible[0]?.name ?? null);
    }
    setTabs((prev) => {
      const kept = prev.filter((x) => !x.tmux || names.has(x.name)).map((x) => (x.tmux && x.tool !== tools.get(x.name) ? { ...x, tool: tools.get(x.name) } : x));
      const have = new Set(kept.map((x) => x.name));
      const added = visible.filter((s) => !have.has(s.name)).map((s): OpenTab => ({ name: s.name, tmux: true, attached: false, state: "idle", tool: s.tool }));
      const all = [...kept, ...added].sort((a, b) => sessionOrder(prefix, a.name, b.name));
      if (auto !== null && !have.has(auto)) all.unshift({ name: auto, tmux: data.available, attached: false, state: "idle" });
      return all.map((x) => (x.name === next && !x.attached ? { ...x, attached: true } : x));
    });
    if (next !== cur) {
      setActiveState(next);
      writeActive(storageKey, next);
    }
  }, [sessions, storageKey, prefix]);

  function open(name: string, tmux: boolean): void {
    const tab: OpenTab = { name, tmux, attached: true, state: "connecting" };
    setTabs((prev) => (prev.some((x) => x.name === name) ? prev : [...prev, tab].sort((a, b) => sessionOrder(prefix, a.name, b.name))));
    setActive(name);
  }

  function remove(name: string): void {
    const rest = tabsRef.current.filter((x) => x.name !== name);
    setTabs((prev) => prev.filter((x) => x.name !== name));
    if (activeRef.current === name) setActive(rest[0]?.name ?? null);
  }

  function close(tab: OpenTab): void {
    if (!tab.tmux) {
      remove(tab.name);
      return;
    }
    const label = sessionLabel(prefix, tab.name);
    void confirm({ title: t("结束终端 {name}？", { name: label }), body: <p>{t("会话里正在运行的程序都会被结束。")}</p>, confirmText: t("结束会话"), danger: true }).then((ok) => {
      if (!ok) return;
      con.tmuxKill(tab.name).then(
        () => {
          remove(tab.name);
          load(false);
        },
        (err: unknown) => {
          if (err instanceof api.ApiError && err.code === "not_found") {
            // 会话已经在主机上结束（比如在里面敲了 exit）：直接收掉标签。
            remove(tab.name);
            load(false);
            return;
          }
          toast.error(api.errorMessage(err));
        },
      );
    });
  }

  // 纯终端：建一个会话。带 tool：守护进程先确认主机上 gate 可用，会话建好后直接敲入
  // `gate <tool>`；主机没有 gate / 没配密钥 / 没有 tmux 都原样报错，不退化成裸 shell。
  // 名字按 main / t<n> 顺序取，主机清单与已开标签（含非持久 shell）里的都算已占用。
  function create(tool?: api.DevTool): void {
    const taken = new Set((sessions?.sessions ?? []).map((s) => s.name).concat(tabsRef.current.map((x) => x.name)));
    const name = nextSessionName(prefix, taken);
    con.tmuxNew(name, dir, tool).then(
      () => {
        open(name, true);
        load(false);
      },
      (err: unknown) => {
        if (tool === undefined && err instanceof api.ApiError && err.code === "tmux_missing") {
          // 没有 tmux 也能开一个非持久的 shell：守护进程会退化成直接起 shell。
          toast(t("主机上没有 tmux，改为直接打开 shell（关闭标签即结束）"));
          open(name, false);
          return;
        }
        toast.error(api.errorMessage(err));
      },
    );
  }

  const tmuxMissing = sessions !== null && !sessions.available;
  // 每个开发工具正在哪些标签里跑（「+」菜单里标出来，免得重复开）。
  const runningIn = new Map<api.DevTool, string[]>();
  for (const tab of tabs) {
    if (tab.tool === undefined) continue;
    runningIn.set(tab.tool, [...(runningIn.get(tab.tool) ?? []), sessionLabel(prefix, tab.name)]);
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-1 overflow-x-auto border-b px-2 py-1">
        {tabs.map((tab) => {
          const label = sessionLabel(prefix, tab.name);
          const toolLabel = tab.tool === undefined ? null : api.devToolLabel(tab.tool);
          return (
            <div key={tab.name} className={cn("flex items-center gap-1 rounded-md border px-2 py-0.5 text-xs", active === tab.name ? "bg-muted" : "text-muted-foreground")}>
              <button
                type="button"
                className="flex items-center font-mono"
                title={`${tab.tmux ? t("tmux 会话 {name}", { name: tab.name }) : t("非持久 shell {name}", { name: tab.name })}${toolLabel === null ? "" : ` · ${t("正在运行 {tool}", { tool: toolLabel })}`}`}
                onClick={() => setActive(tab.name)}
              >
                <span className={cn("mr-1 inline-block size-1.5 rounded-full", tab.state === "open" ? "bg-signal-ok" : tab.state === "closed" ? "bg-signal-danger" : tab.state === "connecting" ? "bg-signal-alert" : "bg-muted-foreground")} />
                {label}
                {tab.tool === undefined ? null : (
                  <span className="bg-primary/15 text-primary ml-1 rounded px-1 text-[10px] leading-4" aria-label={t("正在运行 {tool}", { tool: toolLabel ?? tab.tool })}>
                    {tab.tool}
                  </span>
                )}
              </button>
              <button type="button" aria-label={tab.tmux ? t("结束终端 {name}", { name: label }) : t("关闭标签 {name}", { name: label })} className="hover:bg-background rounded p-0.5" onClick={() => close(tab)}>
                <XIcon className="size-3" />
              </button>
            </div>
          );
        })}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button size="icon-xs" variant="ghost" aria-label={t("新终端或启动开发工具")} title={t("新终端或启动开发工具")}>
              <PlusIcon />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start">
            <DropdownMenuItem onSelect={() => create()}>
              {t("新终端")}
              <span className="text-muted-foreground ml-auto font-mono text-xs">{sessionLabel(prefix, nextSessionName(prefix, new Set((sessions?.sessions ?? []).map((s) => s.name).concat(tabs.map((x) => x.name)))))}</span>
            </DropdownMenuItem>
            <DropdownMenuSeparator />
            <DropdownMenuLabel>{t("新终端并启动开发工具")}</DropdownMenuLabel>
            {api.DEV_TOOLS.map((tool) => {
              const where = runningIn.get(tool.id);
              return (
                <DropdownMenuItem key={tool.id} onSelect={() => create(tool.id)}>
                  {tool.label}
                  <span className="text-muted-foreground ml-auto font-mono text-xs">{where === undefined ? `gate ${tool.id}` : t("运行中：{tabs}", { tabs: where.join(", ") })}</span>
                </DropdownMenuItem>
              );
            })}
            <DropdownMenuSeparator />
            <p className="text-muted-foreground max-w-64 px-2 py-1 text-xs">{t("用主机上 gate 保存的设备地址与密钥启动，起始目录同纯终端。")}</p>
          </DropdownMenuContent>
        </DropdownMenu>
        <div className="ml-auto flex items-center gap-2 text-xs">
          {tmuxMissing ? <span className="text-signal-alert">{t("主机上没有 tmux：终端不持久")}</span> : null}
          {listError !== null ? <span className="text-signal-danger">{listError}</span> : null}
          <span className="text-muted-foreground" title={t("滚轮翻看历史；拖选即复制（选区保留高亮，单击取消）；按住 Shift（macOS 为 Option）拖选走浏览器选区，Ctrl+Shift+C 或带选区时的 Ctrl+C 复制（macOS 为 ⌘C）；右键用浏览器菜单粘贴。")} aria-label={t("终端操作说明")}>
            <CircleHelpIcon className="size-3.5" />
          </span>
          <Button size="icon-xs" variant="ghost" aria-label={t("重新读取会话清单")} title={t("重新读取会话清单")} disabled={listLoading} onClick={() => load(true)}>
            <RefreshCwIcon className={cn(listLoading && "animate-spin")} />
          </Button>
        </div>
      </div>
      <div className="relative min-h-0 flex-1 bg-[#0b0f14]">
        {tabs.length === 0 ? (
          <p className="text-muted-foreground absolute inset-0 flex items-center justify-center text-sm">{sessions === null ? t("正在读取主机上的 tmux 会话…") : t("主机上没有 tmux 会话：点「+」开一个。")}</p>
        ) : null}
        {tabs.filter((tab) => tab.attached).map((tab) => (
          <div key={tab.name} className={cn("absolute inset-0 p-1", active === tab.name ? "block" : "hidden")}>
            <Suspense fallback={<p className="text-muted-foreground p-3 text-xs">{t("加载终端…")}</p>}>
              <TerminalView
                url={(cols, rows) => con.terminalURL(tab.name, dir, cols, rows)}
                active={active === tab.name}
                onState={(state) => setTabs((prev) => prev.map((x) => (x.name === tab.name ? { ...x, state } : x)))}
              />
            </Suspense>
          </div>
        ))}
      </div>
    </div>
  );
}
