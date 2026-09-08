// 应用装配。main.tsx 先把语言目录加载完，再动态 import 本模块——这里 import 的
// 每个业务模块都可能在加载期就调 t()（导航项、主题标签这类模块级常量），
// 必须排在目录之后。除此之外本文件就是 React 根的挂载。

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { Toaster } from "@/components/ui/sonner";
import { App } from "@/app";
import { ConfirmProvider } from "@/lib/confirm";
import { takeLegacyHash } from "@/lib/router";
import { SessionProvider } from "@/lib/session";

// 以下都是纯副作用 import：功能模块在加载期调 registerSlot 把自己挂进槽
// （src/slots/registry.tsx）。这里的先后不决定渲染次序——那个看 order；这里只保证
// 模块被执行到。新增一个功能就在这里加一行。
import "@/features/layout/nav";

export function bootstrap(): void {
  // 旧 hash 书签（管理台改名前的 #/assistant 这类）在首帧前一次性矫正成新路径。
  // fragment 到不了服务端，这种归一化只能在客户端做，而且要赶在守卫算路由之前。
  takeLegacyHash();

  const container = document.getElementById("root");
  if (!container) {
    throw new Error("missing #root container in index.html");
  }

  createRoot(container).render(
    <StrictMode>
      <SessionProvider>
        <ConfirmProvider>
          <App />
        </ConfirmProvider>
      </SessionProvider>
      <Toaster />
    </StrictMode>,
  );
}
