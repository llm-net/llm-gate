import {
  SidebarInset,
  SidebarProvider,
} from "@/components/ui/sidebar";
import { AppSidebar } from "@/features/layout/app-sidebar";
import { TopBar } from "@/features/layout/topbar";
import type { BusinessRoute } from "@/lib/routes";
import { Page } from "@/pages/registry";

/**
 * 登录后的外壳：顶栏横贯整幅，下面才是侧栏 + 主区。
 *
 * 顶栏在侧栏**之上**，但 DOM 上必须留在 `SidebarProvider` **里面**：顶栏左端那个
 * `SidebarTrigger` 要 `useSidebar()`，挂到 provider 外面会在渲染期直接抛
 * 「useSidebar must be used within a SidebarProvider.」，整个外壳白屏。所以这里让
 * provider 自己当 `h-svh` 的竖向 flex 根，顶栏与「侧栏+主区」那一行是它的两个子节点。
 *
 * 侧栏那个 `fixed` 容器因此不能再从视口顶起算——`--sidebar-top` 就是为此传的（CSS
 * 变量沿 DOM 继承，钉在 provider 这一层，底下的侧栏照样取得到），接住它的覆写在
 * components/ui/sidebar.tsx（两处，都有注释）。侧栏宽度（`--sidebar-width`）同理
 * 在这一层给，vendored 那棵树里的缺省值不动。
 *
 * 「侧栏+主区」那一行必须是**同一个父节点下的相邻兄弟**：`SidebarInset` 用
 * `peer-data-*` 读侧栏的状态，拆散或插层就选不中了。
 *
 * 主区只有一副面孔：由 pages/registry 按路由给出的页面。
 *
 * 定高布局：根是 `h-svh` 的竖向 flex，顶栏 `shrink-0`，剩下的一段给那一行。
 * 滚动只发生在主区内部，顶栏、侧栏因此常驻。
 */
export function AppShell({ route }: { route: BusinessRoute }) {
  return (
    <SidebarProvider
      className="h-svh min-h-0 flex-col overflow-hidden"
      style={
        {
          "--sidebar-top": "var(--topbar-h)",
          // 侧栏宽度覆写。上游缺省 16rem（256px）是给英文长标题的会话列表用的，
          // 这里的菜单最宽一项是「图标 16 + 间距 8 + 七个汉字 98 + 内边距 16 = 138px」，
          // 256px 里空掉小一半；13rem（208px）刚好装下最长那项还留一点余量。
          //
          // 改在这里而不是改 components/ui/sidebar.tsx 的 SIDEBAR_WIDTH：那棵树是
          // 逐字保留的 vendored shadcn（见 components/ui/README.md），产品侧的尺寸
          // 决定走 provider 的 style 覆写这条既有缝——`...style` 排在缺省之后，
          // 它赢。窄屏 Sheet 不受影响（那条在 Sheet 自己那层钉了 18rem）。
          "--sidebar-width": "13rem",
        } as React.CSSProperties
      }
    >
      <TopBar />
      <div className="flex w-full min-h-0 flex-1">
        <AppSidebar />
        <SidebarInset className="h-full min-h-0 overflow-hidden">
          {/* `scrollbar-gutter:stable` 恒占住滚动条那一条槽：不占的话，切到内容
              够长的页面时滚动条凭空出现，可视宽从 1024 掉到 1009，居中的
              `mx-auto` 内容整体横移约 7px——每次切页都抖一下。槽常驻就不抖了。
              （用覆盖式滚动条的平台上滚动条本就不占位，这里也不会多留白边。） */}
          <div className="min-h-0 flex-1 overflow-y-auto [scrollbar-gutter:stable]">
            <Page route={route} />
          </div>
        </SidebarInset>
      </div>
    </SidebarProvider>
  );
}
