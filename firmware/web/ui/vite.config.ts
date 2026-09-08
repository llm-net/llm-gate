import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// 设备界面构建配置（React + shadcn，与手写 CSS 的 web/admin 分成两棵树，
// Tailwind preflight 不会污染管理台）。
// - base=/ui/：静态资源经 /ui/* 路径服务（internal/ui/ui.go）。界面全部收在这一个
//   根级前缀下，与厂商 API 命名空间（/v1 /v2 /api/v3 /agents）天然不相交；
//   根路径 / 只做 302 到 /ui/，界面本身只有 /ui/ 一个真值地址。
// - outDir 直指 internal/ui/uidist：构建产物入库，go build 经 //go:embed 打进
//   二进制，绝不依赖 Node；重建产物用 firmware/ 的 `make web`（编 admin + app 两份）。
// - dev 代理：`npm run dev` 时把 /admin/v1 转发到本机 gatewayd（缺省 listen
//   127.0.0.1:8080），与 web/admin 同一条。界面在 /ui/ 下，但管理 API 仍在
//   /admin/v1——那个前缀是内部契约，不随界面挂载点走。
export default defineConfig({
  base: "/ui/",
  build: {
    outDir: "../../internal/ui/uidist",
    emptyOutDir: true,
    target: "es2022",
  },
  server: {
    proxy: {
      "/admin/v1": "http://127.0.0.1:8080",
    },
  },
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": new URL("./src", import.meta.url).pathname,
    },
  },
});
