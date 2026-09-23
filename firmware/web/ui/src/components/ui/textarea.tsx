import * as React from "react"

import { cn } from "@/lib/cn"

// min-w-0 + wrap-anywhere：field-sizing:content 会把无空格长串（回调 URL、
// auth.json）的整行宽度算进最小尺寸，在 grid/flex 里把弹框撑破；允许任意处折行
// 后最小宽度回到单字符，文本框老实待在容器里。
function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        "flex field-sizing-content min-h-16 w-full min-w-0 wrap-anywhere rounded-md border border-input bg-transparent px-3 py-2 text-base shadow-xs transition-[color,box-shadow] outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-destructive/20 md:text-sm dark:bg-input/30 dark:aria-invalid:ring-destructive/40",
        className
      )}
      {...props}
    />
  )
}

export { Textarea }
