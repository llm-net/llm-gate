import { AppSidebar } from "@/features/layout/app-sidebar";
import { ShellFrame } from "@/features/layout/shell-frame";
import { TopBar } from "@/features/layout/topbar";
import type { BusinessRoute } from "@/lib/routes";
import { Page } from "@/pages/registry";

/**
 * 登录后的外壳：骨架在 shell-frame.tsx（与 Key 持有人的「接入方法」页共用），
 * 这里只装上管理员的顶栏与侧栏导航。主区只有一副面孔：由 pages/registry 按路由
 * 给出的页面。
 */
export function AppShell({ route }: { route: BusinessRoute }) {
  return (
    <ShellFrame topbar={<TopBar />} sidebar={<AppSidebar />}>
      <Page route={route} />
    </ShellFrame>
  );
}
