// 迁过来的管理页共用的排版件。承接管理台 style.css 的 `.container` / `.page-head`
// / `.page-note` 三层，只是换成 Tailwind。
//
// 外壳（features/layout/app-shell.tsx）已经给了 `min-h-0 flex-1 overflow-y-auto`
// 的滚动区，页面自己**不要**再套 overflow——那就是内部二次滚动。

import { RefreshCw } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";
import type { Resource } from "@/lib/use-resource";

/**
 * 页面内容区。标题栏恒铺满可用宽度；普通正文钉在 max-w-5xl 上，让表单与清单保持
 * 舒服的行长上限。
 *
 * `wide` 解除上限、把内容区撑满：给那些正文本来就是双栏、栏里还摆着整段可复制
 * 命令的页面用（限宽只会把长命令挤成横滚条）。别顺手给普通表单页加。
 */
export function PageContainer({
  children,
  wide = false,
  compact = false,
}: {
  children: React.ReactNode;
  wide?: boolean;
  compact?: boolean;
}): React.ReactElement {
  return (
    <div
      data-slot="page-container"
      className={cn(
        "page-container flex w-full flex-col gap-4 p-4 sm:p-6",
        wide && "page-container-wide",
        compact && "gap-2 p-3 sm:p-4",
      )}
    >
      {children}
    </div>
  );
}

export function PageHeader({
  title,
  note,
  actions,
  refreshing = false,
  onRefresh,
  compact = false,
}: {
  title: string;
  note?: React.ReactNode;
  actions?: React.ReactNode;
  refreshing?: boolean;
  onRefresh?: () => void;
  compact?: boolean;
}): React.ReactElement {
  return (
    <header
      data-slot="page-header"
      className={cn(
        "bg-card flex min-h-16 w-full flex-wrap items-center gap-x-4 gap-y-2 rounded-lg border px-4 py-3 shadow-xs",
        compact && "grid min-h-12 grid-cols-[minmax(0,1fr)_auto] gap-x-2 gap-y-1 px-3 py-1.5 shadow-none sm:flex",
      )}
    >
      <div className={cn("flex min-w-48 flex-1 flex-wrap items-baseline gap-x-3 gap-y-1", compact && "contents sm:flex")}>
        <h1 className={cn("shrink-0 text-xl font-semibold tracking-tight", compact && "text-lg")}>{title}</h1>
        {note === undefined ? null : (
          <p className={cn("text-muted-foreground min-w-0 text-sm leading-5", compact && "col-span-2 row-start-2 text-xs")}>{note}</p>
        )}
      </div>
      {actions === undefined && onRefresh === undefined ? null : (
        <div className={cn("ml-auto flex max-w-full flex-wrap items-center justify-end gap-2", compact && "col-start-2 row-start-1")}>
          {actions}
          {onRefresh === undefined ? null : (
            <RefreshButton refreshing={refreshing} onRefresh={onRefresh} />
          )}
        </div>
      )}
    </header>
  );
}

// 板上请求通常几十毫秒就返回，只跟随真实 loading 旋转会短到看不清。点击后至少保留
// 500ms 的「刷新中」反馈；它只延长按钮动效，不延迟数据渲染，也不引入轮询。
const MIN_REFRESH_FEEDBACK_MS = 500;

function RefreshButton({
  refreshing,
  onRefresh,
}: {
  refreshing: boolean;
  onRefresh: () => void;
}): React.ReactElement {
  const [acknowledging, setAcknowledging] = useState(false);
  const clickedAt = useRef(0);

  useEffect(() => {
    if (!acknowledging || refreshing) return;
    const elapsed = Date.now() - clickedAt.current;
    const timer = window.setTimeout(
      () => setAcknowledging(false),
      Math.max(0, MIN_REFRESH_FEEDBACK_MS - elapsed),
    );
    return () => window.clearTimeout(timer);
  }, [acknowledging, refreshing]);

  const busy = refreshing || acknowledging;
  return (
    <Button
      type="button"
      size="sm"
      variant="outline"
      disabled={busy}
      aria-busy={busy}
      onClick={() => {
        clickedAt.current = Date.now();
        setAcknowledging(true);
        onRefresh();
      }}
    >
      <RefreshCw className={cn(busy && "animate-spin motion-reduce:animate-none")} aria-hidden="true" />
      <span aria-live="polite">{busy ? t("刷新中") : t("刷新")}</span>
    </Button>
  );
}

/**
 * 分区小标题。一页里摆多个并列分区时用它分段（「网络/域名/代理」「设备更新」两页都是
 * 这个形状）：标题一行、下面一行灰色小字说清这个分区在管什么。
 */
export function SectionHead({ title, desc }: { title: string; desc: string }): React.ReactElement {
  return (
    <div className="mt-2 flex flex-col gap-1">
      <h2 className="font-semibold">{title}</h2>
      <p className="text-muted-foreground text-xs">{desc}</p>
    </div>
  );
}

/**
 * 「加载中…」压后 200ms 再出。板上是内网直连，绝大多数页 40～60ms 就拿到数据，
 * 立刻渲染那行字只会得到两三帧的一闪：内容区先塌到一行高，随即又被撑回整页。
 * 压后之后这类快请求全程不显示加载态，慢的（实测「网络/域名/代理」约 400ms）才显示，
 * 那时它是真的在等，值得给个交代。
 */
function useDelayedFlag(active: boolean, ms = 200): boolean {
  const [shown, setShown] = useState(false);
  useEffect(() => {
    if (!active) {
      setShown(false);
      return;
    }
    const timer = setTimeout(() => setShown(true), ms);
    return () => clearTimeout(timer);
  }, [active, ms]);
  return shown;
}

/**
 * ResourceGate 把「加载中 / 出错 / 有数据」三态收成一处，省得每页各写一遍。
 * 错误文案来自服务端，已是 zh-CN，直接展示。
 */
export function ResourceGate<T>({
  resource,
  children,
}: {
  resource: Resource<T>;
  children: (data: T) => React.ReactNode;
}): React.ReactElement {
  const showLoading = useDelayedFlag(resource.data === null && resource.error === null);
  if (resource.error !== null) {
    return (
      <p role="alert" className="text-destructive text-sm">
        {resource.error}
      </p>
    );
  }
  if (resource.data === null) {
    return showLoading ? <p className="text-muted-foreground text-sm">{t("加载中…")}</p> : <></>;
  }
  return <>{children(resource.data)}</>;
}
