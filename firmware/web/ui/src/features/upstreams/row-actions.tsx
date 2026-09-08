// 行尾「⋯」动作菜单：模型接入页的卡片与行共用（账号卡 / 模型行 / 来源行 / 订阅卡）。
//
// 动作分两档摆：**高频动作留在行上做成可见按钮**（添加模型、查询余额、编辑、测试——
// 也是各处文案点名的那些），其余低频与危险动作（启停、删除、移除…）收进这个菜单。
// 收纳的理由是密度：这页一屏就有十来个实体，每个实体把全部动词平铺成按钮时，
// 26rem 的账号列必然换行、模型列一眼扫过去全是按钮。
//
// 危险动作用 variant="destructive"（与顶栏账户菜单同款），确认对话框仍由调用方持有
// ——菜单只负责入口，不改任何一步确认语义。

import { EllipsisVertical } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { t } from "@/lib/i18n";

export interface RowAction {
  label: string;
  onSelect: () => void;
  /** 危险动作（删除/移除）标红；确认框仍在 onSelect 里走 useConfirm。 */
  destructive?: boolean;
  /** 置灰但保留在菜单里：告知“有这回事但当下做不了”（如 AIGC 模型的测试）。 */
  disabled?: boolean;
  /** 置灰原因等悬停说明。 */
  title?: string;
}

export function RowActionsMenu({
  label,
  actions,
}: {
  /** 无障碍名：如「账号 deepseek 的更多操作」。 */
  label: string;
  actions: RowAction[];
}): React.ReactElement {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button size="icon-xs" variant="outline" aria-label={label} title={t("更多操作")}>
          <EllipsisVertical />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {actions.map((a) => (
          <DropdownMenuItem
            key={a.label}
            variant={a.destructive === true ? "destructive" : "default"}
            disabled={a.disabled === true}
            title={a.title}
            onSelect={a.onSelect}
          >
            {a.label}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
