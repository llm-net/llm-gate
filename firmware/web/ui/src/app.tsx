// 应用根：引导 → 守卫 → 外壳。对齐管理台 web/admin/src/main.ts 的 render()。
//
// 守卫在 lib/routes.ts 的 resolveRoute 里（三步，顺序不能改）。这里只负责三件事：
// 引导期不判定登录态、把地址栏矫正到守卫结果、按结果渲染。

import { useEffect } from "react";

import { Button } from "@/components/ui/button";
import { AuthShell } from "@/features/auth/auth-shell";
import { LoginView } from "@/features/auth/login-view";
import { AppShell } from "@/features/layout/app-shell";
import { t } from "@/lib/i18n";
import { navigate, useLocationPath } from "@/lib/router";
import { resolveRoute } from "@/lib/routes";
import { useSession } from "@/lib/session";

// 启动失败卡：**只给「连自己的管理面都问不通」准备**。401 是「还没登录」的正常
// 答复，不是故障——那条路径在 SessionProvider 里已经分流成 authed=false。
function FatalCard({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <AuthShell heading={t("无法加载界面")} live={false}>
      <div className="flex flex-col gap-4">
        <p role="alert" className="text-destructive text-sm">
          {message}
        </p>
        <Button className="w-full" onClick={onRetry}>
          {t("重试")}
        </Button>
      </div>
    </AuthShell>
  );
}

export function App(): React.ReactElement | null {
  const path = useLocationPath();
  const { authed, booting, fatal, retryBoot } = useSession();
  const route = resolveRoute(path, authed);

  // 地址栏与界面必须一致。引导期不动地址栏：那时 authed 还是 false，动了就会
  // 把已登录的人闪去登录页再弹回来。
  useEffect(() => {
    if (!booting && fatal === null && path !== route) navigate(route, { replace: true });
  }, [booting, fatal, path, route]);

  if (booting) {
    return <p className="text-muted-foreground flex min-h-svh items-center justify-center text-sm">{t("加载中…")}</p>;
  }
  if (fatal !== null) return <FatalCard message={fatal} onRetry={retryBoot} />;
  // 「接入方法」凭 Key 自证，不看会话：登着的管理员打开它看到的也是 Key 视角。
  // 两个入口保持同一组件实例，切换只更新表单，不重建整张登录页。
  if (route === "/login" || route === "/connect") {
    return <LoginView method={route === "/login" ? "password" : "api-key"} />;
  }
  // 地址栏还没矫正过来时先不渲染，省掉闪一帧错页。上面的 effect 会立刻把它推正。
  if (path !== route) return null;
  return <AppShell route={route} />;
}
