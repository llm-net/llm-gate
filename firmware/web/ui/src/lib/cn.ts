import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

// shadcn 的类名合并助手。vendored 组件源码里的 `@/lib/utils` 一律改指到这里，
// 保持产品面只有一个 cn 实现。
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
