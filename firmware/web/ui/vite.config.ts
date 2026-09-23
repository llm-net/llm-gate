import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const outDir = "../../internal/ui/uidist";

// keepGitkeep：outDir 被 emptyOutDir 清空后补回 .gitkeep（主仓只跟踪这一个文件）。
// 经打包器的 emitFile 写出，不依赖 Node 类型定义（tsc --noEmit 也检查本文件）。
function keepGitkeep(): Plugin {
  return {
    name: "llmgate-keep-gitkeep",
    generateBundle() {
      this.emitFile({ type: "asset", fileName: ".gitkeep", source: "" });
    },
  };
}

// 设备界面构建配置（React + shadcn，与手写 CSS 的 web/admin 分成两棵树，
// Tailwind preflight 不会污染管理台）。
// - base=/ui/：静态资源经 /ui/* 路径服务（internal/ui/ui.go）。界面全部收在这一个
//   根级前缀下，与厂商 API 命名空间（/v1 /v2 /api/v3 /agents）天然不相交；
//   根路径 / 只做 302 到 /ui/，界面本身只有 /ui/ 一个真值地址。
// - outDir 直指 internal/ui/uidist：产物经 //go:embed 打进二进制，但不入库（目录只
//   跟踪 .gitkeep，firmware/ 的 `make build|test` 每次先经 `make web` 重建）。
//   emptyOutDir 会连 .gitkeep 一起清掉，keepGitkeep 插件在打包收尾时把它补回来，
//   `npm run build` 之后工作树不会出现「删除了 .gitkeep」。
// - dev 代理：`npm run dev` 时把 /admin/v1 转发到本机 gatewayd（缺省 listen
//   127.0.0.1:8080），与 web/admin 同一条。界面在 /ui/ 下，但管理 API 仍在
//   /admin/v1——那个前缀是内部契约，不随界面挂载点走。
export default defineConfig({
  base: "/ui/",
  build: {
    outDir,
    emptyOutDir: true,
    target: "es2022",
  },
  server: {
    proxy: {
      "/admin/v1": "http://127.0.0.1:8080",
    },
  },
  plugins: [react(), tailwindcss(), keepGitkeep()],
  resolve: {
    alias: {
      "@": new URL("./src", import.meta.url).pathname,
    },
  },
});
