# vendored shadcn 原子层

本目录与 `src/hooks/use-mobile.ts` 是**手工 vendored** 的 shadcn/ui 组件（style
`new-york-v4`，主题 zinc），源自 `https://ui.shadcn.com/r/styles/new-york-v4/<name>.json`。
**不接 shadcn CLI**，仓库里没有 `components.json`；要更新或新增组件就手抓那份 JSON 再按下面
的清单改写，别让工具改配置。

为了让将来重抓时 `diff` 还看得懂，除以下几处外**逐字保留上游源码**（含上游的无分号排版，
本目录不受仓库其它 TS 文件的排版惯例约束）：

- `@/lib/utils` → `@/lib/cn`；`@/registry/new-york-v4/{ui,hooks}/*` → `@/components/ui/*`、`@/hooks/*`。
- 上游用聚合包 `radix-ui`，本仓装的是单包：`import { Dialog as DialogPrimitive } from "radix-ui"`
  → `import * as DialogPrimitive from "@radix-ui/react-dialog"`，`Slot.Root` → `Slot`。
- 删掉 `"use client"`（没有 RSC，留着只会让 Rollup 报模块级指令告警）。
- 用户可见/读屏文案改中文（`关闭`、`切换侧栏`、`侧栏`……）——产品面是 `lang="zh-CN"`。
- 删掉没人用的 cva 变体导出（`buttonVariants`、`badgeVariants`、`tabsListVariants`）；
  常量还在文件里，将来要给 `<a>` 套按钮样式时把 `export` 加回来即可。
- `sonner.tsx` 去掉 `next-themes`（无 Next），`theme` 钉死 `light`；托底配色走
  `--normal-*` token 变量，界面配色（含夜间模式）因此自动生效，不接主题状态。
- `dropdown-menu.tsx` 的 `DropdownMenuCheckboxItem` 不再显式回传 `checked`，改为随 props
  透传：本仓 `exactOptionalPropertyTypes: true`，显式传 `undefined` 会 TS2375。
- **`dialog.tsx` 与 `alert-dialog.tsx` 改成整体滚动**（`firmware/AGENTS.md` 有专门一节）：
  上游缺省是 `fixed top-1/2 left-1/2 -translate-1/2` 的「卡片钉在原地」，内容超出视口时
  两端都够不到。改法是遮罩兼作滚动容器（`fixed inset-0 flex overflow-y-auto`），卡片作为
  它的子节点用 `m-auto` 排布、**不设 `max-height`**。居中必须用 auto margin 而不是
  `items-center`：剩余空间为负时 auto margin 归零、卡片从顶端排起整体滚得到，居中对齐
  会把超出的顶部裁在滚动区外。Radix 的 `RemoveScroll` 就包在 Overlay 上
  （`as={Slot}` + `shards=[contentRef]`），所以把 Content 放进 Overlay 不会被滚动锁吃掉。
  调用方也不能给 `DialogContent` / `AlertDialogContent` 或其内部列表、表单分区添加
  `max-h-*`、`overflow-y-*`、`overflow-auto` 等内部竖向滚动；长内容直接撑高整张卡片。
  重抓上游后这两处要重新覆写。
- **`sidebar.tsx` 的桌面侧栏改成从 `--sidebar-top` 起算**：上游那个 `fixed` 容器写死
  `inset-y-0 h-svh`（贴视口顶、占满整屏高），产品面的顶栏是贯通整幅的，侧栏会钻到它底下去。
  改法是容器换成 `top-(--sidebar-top) bottom-0` 且不给 height（两端都定了，高度自然是那一段），
  `SidebarProvider` 的内联 style 里补一个缺省 `--sidebar-top: 0px`（= 上游行为），由外壳
  （`features/layout/app-shell.tsx`）传真值。重抓上游后这两处要重新覆写。

浮层组件的动画类（`animate-in` / `fade-in-0` / `zoom-in-95` / `slide-in-from-*`）来自
`tw-animate-css`，在 `src/styles/globals.css` 里 `@import`。少了它不报错，只是动画静默失效。
