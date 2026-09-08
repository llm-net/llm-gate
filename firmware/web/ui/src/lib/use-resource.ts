// 取数 hook。管理台的对应物是 main.ts 里的 `renderSeq` 竞态防护 + `PageCtx.refresh()`
// 整页重渲染这两件事，这里把它们搬成 React 的形状。
//
// **零轮询是产品约定，不是这里漏了。** 管理台每个页头都写着「按需读取，页面不自动
// 刷新」——设备上的读数（用量、状态、余额）都由人主动触发，别在这层加定时重取、
// 也别加窗口聚焦重取。要新数据就调 reload()。
//
// 不引查询库（TanStack Query 之类）的硬理由：Key 明文
// （POST /admin/v1/me/keys/{id}/plaintext）只许活在组件内存里，带 gcTime 的缓存、
// devtools 可见的 store 都碰不得。这层因此刻意不做任何跨组件缓存。

import { useCallback, useEffect, useRef, useState } from "react";

import { ApiError, errorMessage } from "./api";

export interface Resource<T> {
  data: T | null;
  /** 已本地化的错误文案（服务端 message 直接可展示）；null 表示没出错。 */
  error: string | null;
  loading: boolean;
  /** 重新取一次。语义同管理台的 ctx.refresh()，只是作用域收到本资源。 */
  reload: () => void;
}

/**
 * useResource 取一份数据。
 *
 * `deps` 变化或 `reload()` 被调用时重取；返回时若已被后续请求取代（序号不符）
 * 或组件已卸载，结果一律丢弃——这就是 main.ts:75 那个 `renderSeq` 的作用，
 * 少了它快速切页时旧请求会盖掉新页面。React StrictMode 下 effect 双挂载，
 * 同一道闸也把第一次的结果挡掉。
 *
 * 401 unauthorized 不当错误展示：api 层的会话失效回调已经把人送去登录页了，
 * 再在页面上留一条红字只是噪音（对齐 main.ts:239 的分支）。
 */
export function useResource<T>(load: () => Promise<T>, deps: readonly unknown[]): Resource<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [epoch, setEpoch] = useState(0);

  // seq 单调递增，只认最后一次发起的请求。
  const seq = useRef(0);
  // load 每次渲染都是新函数（调用方通常写成内联箭头），放进 deps 会自激；
  // 用 ref 取最新的那个，重取时机完全交给 deps 与 epoch。
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    const mine = ++seq.current;
    let alive = true;
    setLoading(true);
    loadRef.current().then(
      (value) => {
        if (!alive || mine !== seq.current) return;
        setData(value);
        setError(null);
        setLoading(false);
      },
      (err: unknown) => {
        if (!alive || mine !== seq.current) return;
        setLoading(false);
        if (err instanceof ApiError && err.status === 401 && err.code === "unauthorized") {
          return; // 会话失效回调已跳登录
        }
        setError(errorMessage(err));
      },
    );
    return () => {
      alive = false;
    };
    // deps 由调用方给（本资源真正依赖的入参），epoch 承接 reload()。
  }, [...deps, epoch]);

  const reload = useCallback(() => setEpoch((n) => n + 1), []);
  return { data, error, loading, reload };
}
