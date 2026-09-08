import type { ComponentType } from "react";

/**
 * 极薄 slot 注册表：只保留编译期贡献点，不引入 Cordis 或运行时插件系统。
 *
 * 契约只有两条：
 * - 功能模块在**模块加载期**调 `registerSlot` 把自己挂到某个槽上；
 * - 壳（features/layout）用 `<Slot name="…" />` 把该槽的贡献按 order 依次渲染。
 *
 * 故意不做的事：没有运行时增删、没有依赖注入、没有订阅。注册全部发生在 main.tsx
 * `createRoot().render()` 之前的 import 副作用里，注册表在首帧之前就已冻结，所以
 * `Slot` 直接读数组即可，不需要 useSyncExternalStore。将来真要动态贡献（比如设置卡
 * 按上游能力增减），再补订阅，别提前把这层做厚。
 *
 * 槽内的排版归贡献方：`<Slot>` 只决定「渲染在哪、按什么顺序」，不套任何容器元素。
 * 侧栏贡献自带 `SidebarHeader` / `SidebarGroup` / `SidebarFooter`，槽为空时页面上
 * 就真的什么都不出现。
 *
 * 注意重名：这里的 `Slot` 是产品面的槽出口，和 `@radix-ui/react-slot` 的 `Slot`
 * （vendored 原子层用来实现 `asChild`）没有关系，别在功能层把两者混着 import。
 */
export type SlotName = "sidebar";

type SlotEntry = {
  Component: ComponentType;
  /** 小的先渲染。约定：品牌 0 / 导航 50 / 设置 100（置底）。 */
  order: number;
  /** 注册序号，兼作 React key；同 order 时按它稳定排列。 */
  seq: number;
};

const slots: Record<SlotName, SlotEntry[]> = {
  sidebar: [],
};

let nextSeq = 0;

export function registerSlot(
  name: SlotName,
  Component: ComponentType,
  order = 0,
): void {
  const entries = slots[name];
  entries.push({ Component, order, seq: nextSeq++ });
  entries.sort((a, b) => a.order - b.order || a.seq - b.seq);
}

export function Slot({ name }: { name: SlotName }) {
  return (
    <>
      {slots[name].map(({ Component, seq }) => (
        <Component key={seq} />
      ))}
    </>
  );
}
