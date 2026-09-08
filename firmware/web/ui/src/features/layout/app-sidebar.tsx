import { Sidebar } from "@/components/ui/sidebar";
import { Slot } from "@/slots/registry";

/**
 * 侧栏外壳：只提供 shadcn `Sidebar` 容器，里面的导航是 sidebar 槽的贡献
 * （features/layout/nav.tsx），自带 `SidebarContent`。
 *
 * 槽里**只能有一个 `SidebarContent`**（nav 那个）：它是 `flex-1 overflow-auto`，
 * 所有菜单组都得在它里面才会一起滚；再来一个就是两条各滚各的滚动轴。
 *
 * 品牌不在这里——顶栏贯通整幅，logo 与产品名只在那一条上出现一次
 * （features/layout/topbar.tsx）。
 *
 * `collapsible="offcanvas"` 是 shadcn 缺省：桌面折叠时整条侧栏滑出视口；窄屏由
 * `Sidebar` 内部的 `useIsMobile` 自动换成 Sheet 呼出，壳这边不用自己判断视口。
 * 侧栏上沿由外壳传的 `--sidebar-top` 决定（顶栏高度），见 app-shell.tsx。
 */
export function AppSidebar() {
  return (
    <Sidebar collapsible="offcanvas">
      <Slot name="sidebar" />
    </Sidebar>
  );
}
