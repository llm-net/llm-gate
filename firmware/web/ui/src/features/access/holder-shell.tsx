// Key 持有人的外壳：「接入方法」页（/ui/connect）贴入 Key 之后的那一屏。与管理员
// 登录后的外壳同一副骨架（features/layout/shell-frame.tsx）——贯通整幅的顶栏、左侧
// 菜单、主区内滚——两种身份看到的是同一张界面，只是：
//
//   - 顶栏读数按这把 Key 裁剪（设备型号 / 固件版本 / 这把 Key 可用的模型数 / 授权的
//     工具订阅），来源是 Key 自证的两条数据面端点，不打管理 API；
//   - 顶栏右端没有设备名菜单（改名、改密是管理员的事），换成当前密钥的掩码标识；
//     「退出」只清掉内存里的 Key、回到贴 Key 的那一屏，没有会话可退；
//   - 左侧菜单只有「使用指南」一组四项（API调用 / 开发工具接入 / API调测 / 媒体生成），与管理员
//     侧栏同名同图标同顺序；但它们**不是路由**——Key 不进 URL，切页只是组件内的
//     state，刷新即回到贴 Key 的状态（§15.1 同款纪律）。
//
// 侧栏不走 slots 的 sidebar 槽：那个槽里挂的是管理员导航（features/layout/nav.tsx，
// 按会话判断显隐），登着的管理员打开这一页也该看到 Key 视角，所以这里直接渲染
// 自己的 `SidebarContent`。

import { FlaskConical, Code2, KeyRound, LogOut, Terminal, WandSparkles } from "lucide-react";

import { LangSwitch } from "@/components/lang-switch";
import { Button } from "@/components/ui/button";
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from "@/components/ui/sidebar";
import { ShellFrame } from "@/features/layout/shell-frame";
import {
  Cell,
  cloudReading,
  CompactReading,
  ThemeMenu,
  TOPBAR_BUTTON,
  TopBarFrame,
  type Reading,
} from "@/features/layout/topbar";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { keyMask } from "@/lib/format";
import { t } from "@/lib/i18n";

export type HolderTab = "api" | "dev-tools" | "api-debug" | "media";

interface HolderNavItem {
  key: HolderTab;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
}

// 与管理员侧栏「使用指南」组同名、同图标、同顺序（features/layout/nav.tsx 的 GUIDE_ITEMS）。
const HOLDER_ITEMS: HolderNavItem[] = [
  { key: "api", label: t("API调用"), icon: Code2 },
  { key: "dev-tools", label: t("开发工具接入"), icon: Terminal },
  { key: "api-debug", label: t("API调测"), icon: FlaskConical },
  { key: "media", label: t("媒体生成"), icon: WandSparkles },
];

export function holderTabTitle(tab: HolderTab): string {
  return HOLDER_ITEMS.find((item) => item.key === tab)?.label ?? "";
}

function HolderNav({ tab, onSelect }: { tab: HolderTab; onSelect: (tab: HolderTab) => void }) {
  // 窄屏下侧栏是 Sheet：选了菜单项就收起，与点管理员侧栏链接换页后的观感一致。
  const { isMobile, setOpenMobile } = useSidebar();
  return (
    <SidebarContent>
      <SidebarGroup>
        <SidebarGroupLabel>{t("使用指南")}</SidebarGroupLabel>
        <SidebarMenu>
          {HOLDER_ITEMS.map((item) => (
            <SidebarMenuItem key={item.key}>
              <SidebarMenuButton
                type="button"
                isActive={tab === item.key}
                tooltip={item.label}
                onClick={() => {
                  onSelect(item.key);
                  if (isMobile) setOpenMobile(false);
                }}
              >
                <item.icon />
                <span>{item.label}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          ))}
        </SidebarMenu>
      </SidebarGroup>
    </SidebarContent>
  );
}

function HolderReadings({ snap, policy }: { snap: api.KeyAccessSnapshot; policy: api.DevToolPolicy }) {
  const models = cloudReading({ text: snap.models.length, aigc: snap.aigc_models?.length ?? 0, systemone: snap.systemone_models?.length ?? 0 });

  // 工具订阅只数管理员为这把 Key 勾了的（configured）；可用数与订阅面同口径。
  const granted = policy.subscriptions.filter((sub) => sub.configured);
  const live = granted.filter((sub) => sub.available).length;
  const subscriptions: Reading =
    granted.length === 0
      ? { tone: "idle", text: "0", description: t("未授权"), detail: t("这把 API 密钥未获授权使用任何开发工具订阅") }
      : {
          tone: live === 0 ? "alert" : "ok",
          text: `${live}/${granted.length}`,
          description: t("可用订阅 {available}/{total}", { available: live, total: granted.length }),
          detail: granted
            .map((sub) =>
              t("{name}：{state}", {
                name: api.agentProviderLabel(sub.provider),
                state: sub.available ? t("可用") : t("未开通"),
              }),
            )
            .join(" · "),
        };

  return (
    <>
      {snap.hardware_model === "" ? null : (
        <Cell title={t("设备型号")} className="hidden xl:flex">{snap.hardware_model}</Cell>
      )}
      {snap.firmware_version === "" ? null : (
        <Cell title={t("固件版本")} className="hidden xl:flex">{snap.firmware_version}</Cell>
      )}
      <Cell label="API">
        <CompactReading label={t("可用模型")} reading={models} />
      </Cell>
      <Cell title={t("开发工具订阅")}>
        <CompactReading label={t("工具订阅")} reading={subscriptions} />
      </Cell>
    </>
  );
}

function HolderTopBar({ holderKey, snap, policy, onReset }: {
  holderKey: string;
  snap: api.KeyAccessSnapshot;
  policy: api.DevToolPolicy;
  onReset: () => void;
}) {
  return (
    <TopBarFrame readings={<HolderReadings snap={snap} policy={policy} />}>
      <LangSwitch className={TOPBAR_BUTTON} />
      {/* 密钥标识占管理员顶栏里设备名菜单的位置：只显示掩码，不可点、不解封。 */}
      <span
        className="flex h-7 min-w-0 shrink items-center gap-1 px-2 font-mono text-[13px] text-white/90"
        title={t("当前使用的 API 密钥")}
      >
        <KeyRound className="size-3.5 shrink-0" aria-hidden />
        <span className="truncate">{keyMask(holderKey)}</span>
      </span>
      <ThemeMenu />
      <Button variant="ghost" onClick={onReset} title={t("返回登录")}
        className={cn("h-7 gap-1 px-2 text-[13px]", TOPBAR_BUTTON)}>
        <LogOut className="size-3.5" />{t("退出")}
      </Button>
    </TopBarFrame>
  );
}

export function HolderShell({ holderKey, snap, policy, tab, onSelect, onReset, children }: {
  holderKey: string;
  snap: api.KeyAccessSnapshot;
  policy: api.DevToolPolicy;
  tab: HolderTab;
  onSelect: (tab: HolderTab) => void;
  onReset: () => void;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <ShellFrame
      topbar={<HolderTopBar holderKey={holderKey} snap={snap} policy={policy} onReset={onReset} />}
      sidebar={
        <Sidebar collapsible="offcanvas">
          <HolderNav tab={tab} onSelect={onSelect} />
        </Sidebar>
      }
    >
      {children}
    </ShellFrame>
  );
}
