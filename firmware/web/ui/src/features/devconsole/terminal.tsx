// 工作空间终端：一个 xterm.js 实例对接一条到设备的 WebSocket，设备再把帧原样转给主机上的
// 守护进程（tmux 客户端）。浏览器断开只是断了这条 tmux 客户端，会话本身留在主机上。
//
// 尺寸：FitAddon 按容器算 cols/rows，变化就发一帧 Resize；连接建立时把当时的尺寸带在
// 地址里，守护进程按它开伪终端。零轮询：没有心跳，连接状态只随 WebSocket 事件变。
//
// 鼠标与剪贴板：tmux 开着 mouse 模式（守护进程 attach 时 source 内嵌的 tmux.conf），滚轮、
// 拖选、单击都交给 tmux，tmux 的复制经 OSC 52 由 clipboard addon 写进浏览器剪贴板；按住
// Shift（macOS 为 Option）拖选走 xterm.js 自己的选区，Ctrl+Shift+C / Ctrl+Insert 或带选区
// 时的 Ctrl+C 复制它（macOS 用 ⌘C），粘贴用浏览器自己的快捷键。剪贴板 API 只在安全上下文
// 里有，纯 IP HTTP 下退到 execCommand。

import { ClipboardAddon } from "@xterm/addon-clipboard";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";

import { t } from "@/lib/i18n";

import { FRAME_DATA, FRAME_EXIT, decodeFrame, encodeFrame, encodeResize } from "./wire";

export type TerminalState = "idle" | "connecting" | "open" | "closed";

async function copyText(text: string): Promise<void> {
  if (text === "") return;
  try {
    await navigator.clipboard.writeText(text);
    return;
  } catch {
    // 非安全上下文或未授权：退到旧 API。
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand("copy");
  } finally {
    ta.remove();
  }
}

export function TerminalView({
  url,
  active,
  onState,
}: {
  /** 终端 WebSocket 地址的生成器：拿到首帧尺寸后再拼地址。 */
  url: (cols: number, rows: number) => string;
  /** 当前标签是否可见：不可见时不 fit（隐藏容器算不出尺寸）。 */
  active: boolean;
  onState?: (state: TerminalState, reason?: string) => void;
}): React.ReactElement {
  const holder = useRef<HTMLDivElement | null>(null);
  const termRef = useRef<Terminal | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const stateRef = useRef(onState);
  stateRef.current = onState;

  useEffect(() => {
    const el = holder.current;
    if (el === null) return;
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace",
      fontSize: 13,
      scrollback: 5000,
      allowProposedApi: false,
      macOptionClickForcesSelection: true,
      theme: { background: "#0b0f14" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.loadAddon(new ClipboardAddon());
    term.open(el);
    fit.fit();
    term.attachCustomKeyEventHandler((ev) => {
      if (ev.type !== "keydown" || ev.metaKey || ev.altKey) return true;
      const copyKey = (ev.ctrlKey && ev.shiftKey && ev.code === "KeyC") || (ev.ctrlKey && !ev.shiftKey && ev.code === "Insert");
      if (copyKey || (ev.ctrlKey && !ev.shiftKey && ev.code === "KeyC" && term.hasSelection())) {
        void copyText(term.getSelection());
        term.clearSelection();
        ev.preventDefault();
        return false;
      }
      return true;
    });
    termRef.current = term;
    fitRef.current = fit;

    const encoder = new TextEncoder();
    const ws = new WebSocket(url(term.cols, term.rows));
    ws.binaryType = "arraybuffer";
    wsRef.current = ws;
    stateRef.current?.("connecting");
    let exited = false;
    ws.onopen = () => {
      stateRef.current?.("open");
      ws.send(encodeResize(term.cols, term.rows));
      term.focus();
    };
    ws.onmessage = (ev: MessageEvent<ArrayBuffer>) => {
      const f = decodeFrame(ev.data);
      if (f === null) return;
      if (f.type === FRAME_DATA) {
        term.write(f.payload);
      } else if (f.type === FRAME_EXIT) {
        exited = true;
        const reason = new TextDecoder().decode(f.payload);
        setNotice(reason === "" ? t("会话已结束") : reason);
        stateRef.current?.("closed", reason);
      }
    };
    ws.onclose = () => {
      if (!exited) {
        setNotice(t("连接已断开"));
        stateRef.current?.("closed");
      }
    };
    ws.onerror = () => {
      // onclose 紧随其后，这里不重复处理。
    };
    const dataSub = term.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encodeFrame(FRAME_DATA, encoder.encode(data)));
    });
    const binarySub = term.onBinary((data) => {
      if (ws.readyState !== WebSocket.OPEN) return;
      const bytes = new Uint8Array(data.length);
      for (let i = 0; i < data.length; i += 1) bytes[i] = data.charCodeAt(i) & 0xff;
      ws.send(encodeFrame(FRAME_DATA, bytes));
    });
    const resizeSub = term.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(encodeResize(cols, rows));
    });
    const observer = new ResizeObserver(() => {
      if (el.offsetParent !== null) fit.fit();
    });
    observer.observe(el);
    return () => {
      observer.disconnect();
      dataSub.dispose();
      binarySub.dispose();
      resizeSub.dispose();
      ws.close();
      term.dispose();
      termRef.current = null;
      fitRef.current = null;
      wsRef.current = null;
    };
    // url 由父组件按会话名固定；变了就是另一个终端（父组件换 key）。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (!active) return;
    const id = window.requestAnimationFrame(() => {
      fitRef.current?.fit();
      termRef.current?.focus();
    });
    return () => window.cancelAnimationFrame(id);
  }, [active]);

  return (
    <div className="relative h-full w-full min-h-0">
      <div ref={holder} className="h-full w-full [&_.xterm]:h-full [&_.xterm-viewport]:!overflow-y-auto" />
      {notice === null ? null : (
        <div className="bg-background/90 text-muted-foreground absolute inset-x-0 bottom-0 border-t px-3 py-1.5 text-xs">{notice}</div>
      )}
    </div>
  );
}
