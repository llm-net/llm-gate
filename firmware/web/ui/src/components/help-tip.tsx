// HelpTip 是字段旁的问号：说明只在点开/悬停时出现，不占版面。受控 open + 点击切换
// 与侧栏「使用指南」那颗一致，触屏上也点得开。

import { CircleHelp } from "lucide-react";
import { useState } from "react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";

export function HelpTip({ label, children }: { label: string; children: React.ReactNode }): React.ReactElement {
  const [open, setOpen] = useState(false);
  return (
    <Tooltip open={open} onOpenChange={setOpen}>
      <TooltipTrigger asChild>
        <button
          type="button"
          aria-label={label}
          aria-expanded={open}
          onClick={() => setOpen((o) => !o)}
          className="text-muted-foreground hover:text-foreground inline-flex size-4 items-center justify-center rounded-sm"
        >
          <CircleHelp className="size-3.5" />
        </button>
      </TooltipTrigger>
      <TooltipContent side="top" align="start" sideOffset={4} className="max-w-72 leading-relaxed">
        {children}
      </TooltipContent>
    </Tooltip>
  );
}
