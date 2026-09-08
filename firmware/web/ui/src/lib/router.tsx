// 极薄 History 路由。全仓没有路由库（官网 website/ 和这棵树都没有，管理台走
// hash），这里也不引一个：需要的只是「路径 ↔ 当前页」加一次 pushState，几十行的事。
//
// 界面挂在 /ui/ 下，路由值一律是**不带前缀的内部路径**（`/users`），只有落到
// 地址栏时才补 BASE（`/ui/users`）。服务端对 /ui/ 未知子路径回退 index.html，
// 所以深链直达与刷新都不会 404。

import { useSyncExternalStore } from "react";

import type { Route } from "./routes";
import { LEGACY_HASH } from "./routes";

/** 界面前缀，与 vite.config.ts 的 base、internal/app/ui.go 的 TrimPrefix 同值。 */
export const BASE = "/ui";

/** toHref 把内部路由折成地址栏里的真路径。 */
export function toHref(route: Route): string {
  return `${BASE}${route}`;
}

/**
 * fromPath 把地址栏路径收回内部路径；不在 /ui/ 下的一律回 "/"。
 * "/" 自己不是路由——守卫（routes.ts 的 LEGACY_PATH）把它归一化到落地页。
 */
export function fromPath(pathname: string): string {
  if (pathname === BASE || pathname === `${BASE}/`) return "/";
  return pathname.startsWith(`${BASE}/`) ? pathname.slice(BASE.length) : "/";
}

const listeners = new Set<() => void>();
function emit(): void {
  for (const l of listeners) l();
}

/**
 * navigate 换页。**路径没变也要 emit** —— 对齐管理台 main.ts 的 `nav()`：那边
 * hash 未变不触发 hashchange，要手动重渲染，这里同理，否则「已经在这一页时再点
 * 一次导航」会没有反应。
 */
export function navigate(to: Route, opts?: { replace?: boolean }): void {
  const href = toHref(to);
  if (window.location.pathname !== href) {
    if (opts?.replace === true) {
      window.history.replaceState(null, "", href);
    } else {
      window.history.pushState(null, "", href);
    }
  }
  emit();
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  window.addEventListener("popstate", cb);
  return () => {
    listeners.delete(cb);
    window.removeEventListener("popstate", cb);
  };
}

function snapshot(): string {
  return window.location.pathname;
}

/** useLocationPath 订阅地址栏路径（前进/后退与 navigate 都会触发）。 */
export function useLocationPath(): string {
  return fromPath(useSyncExternalStore(subscribe, snapshot));
}

/**
 * takeLegacyHash 消化管理台改名前的旧 hash 书签（`/ui/#/assistant` 这类），
 * 命中就把地址栏换成新路径并返回它。fragment 到不了服务端，这种矫正只能在客户端做。
 */
export function takeLegacyHash(): Route | null {
  const mapped = LEGACY_HASH[window.location.hash];
  if (mapped === undefined) return null;
  window.history.replaceState(null, "", toHref(mapped));
  return mapped;
}

/**
 * Link 是站内跳转的 <a>：保留真 href（可中键新开、可复制），左键走 pushState。
 *
 * **`onClick` 必须单独接出来、并且 `{...rest}` 要摊在自己的 onClick 之前。**
 * 侧栏菜单是 `<SidebarMenuButton asChild tooltip=…><Link/></SidebarMenuButton>`：
 * Radix 的 `TooltipTrigger asChild` 会把它自己的 onClick（关掉 tooltip）经 Slot
 * 合并进来，落到这里就在 rest 里。早先 rest 摊在 onClick 之后，等于把本组件的
 * preventDefault + navigate 整个盖掉，左键点菜单退化成原生 <a> 跳转——**整页重载**，
 * 600 KB bundle 重新解析、会话重新 boot，看起来就是切页闪一下白。
 * 现在两个 handler 组合执行：先跑外面传进来的，再判 defaultPrevented 决定要不要接管。
 */
export function Link({
  to,
  children,
  onClick,
  ...rest
}: { to: Route } & Omit<React.ComponentProps<"a">, "href">): React.ReactElement {
  return (
    <a
      href={toHref(to)}
      {...rest}
      onClick={(e) => {
        onClick?.(e);
        // 中键、右键与带修饰键的点击交回浏览器（新标签页、下载等）；
        // 外部 handler 已经 preventDefault 的也不再接管。
        if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
        e.preventDefault();
        navigate(to);
      }}
    >
      {children}
    </a>
  );
}
