// 迁移期占位页：这一页还在旧管理台上。
//
// 路由值与管理台 hash 一一对应（`/users` ↔ `#/users`），所以旧地址直接由路由拼出来。
// 每迁完一页就把 registry 里那一项换成真页面，本文件在最后一页迁完后连同 /admin/
// 一起删。

import { SquareArrowOutUpRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { t } from "@/lib/i18n";
import type { BusinessRoute } from "@/lib/routes";

export function Pending({ route, title }: { route: BusinessRoute; title: string }): React.ReactElement {
  return (
    <div className="mx-auto w-full max-w-2xl p-6">
      <Card className="gap-3 p-6">
        <h1 className="text-lg font-semibold">{title}</h1>
        <p className="text-muted-foreground text-sm">
          {t("这一页还没搬到新界面，功能都在旧管理台上，可以照常使用。")}
        </p>
        <div>
          <Button asChild variant="outline">
            {/* 整页跳转，不是站内路由：那边是另一份产物。 */}
            <a href={`/admin/#${route}`}>
              {t("在管理台打开")}
              <SquareArrowOutUpRight />
            </a>
          </Button>
        </div>
      </Card>
    </div>
  );
}
