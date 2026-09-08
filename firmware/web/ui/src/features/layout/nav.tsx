// 侧栏导航，也是侧栏里唯一的滚动容器。
//
// 结构上只有一个 `SidebarContent`（`flex-1 min-h-0 overflow-auto`），使用指南、模型接入
// 与设备三组都在它里面：菜单一多就整块一起滚，不出现「某一组自己卷动、别的组钉在
// 原地」。
//
// 设备只有一个管理员，登录进来就都看得见。三组的分工：
//   使用指南   讲局域网里的客户端与开发工具怎样接入这台设备——输出侧；
//   模型接入   四种接入各一页（API按量计费 / API订阅套餐 / API私有部署 / 开发工具订阅）
//              ——输入侧。四种接入在设备上统一成同一组输出，全部是协议面：文本的
//              OpenAI Chat / OpenAI Responses / Anthropic Messages，按厂商与模态各一个的
//              视频 / 图像协议面（火山方舟 视频、火山方舟 图像、MiniMax 视频…），以及
//              各开发工具订阅自己的协议面；
//   设备       管理设备自身的页面。
//
// 一处刻意保留的行为：`/egress` 在轨上高亮「网络/域名/代理」——同页不同标签，
// 别给它单独一项。
//
// 图标用 lucide-react（本树已有依赖）。

import {
  Activity,
  Blocks,
  Bot,
  Code2,
  Coins,
  HardDriveDownload,
  KeyRound,
  Network,
  ReceiptText,
  ServerCog,
  Terminal,
  Ticket,
} from "lucide-react";

import {
  SidebarContent,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";
import { t } from "@/lib/i18n";
import { Link, useLocationPath } from "@/lib/router";
import type { BusinessRoute } from "@/lib/routes";
import { useSession } from "@/lib/session";
import { registerSlot } from "@/slots/registry";

interface NavItem {
  to: BusinessRoute;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  /** 别的路由落在这一项上时也高亮它（同页不同标签）。 */
  aliases?: BusinessRoute[];
}

const GUIDE_ITEMS: NavItem[] = [
  { to: "/model-routing/api", label: t("API调用"), icon: Code2 },
  { to: "/model-routing/dev-tools", label: t("开发工具接入"), icon: Terminal },
];

// 模型接入四页的顺序就是管理员录账号时的心智顺序：先按量、再套餐、再自己部署的，
// 最后是不走 API Key 的开发工具订阅。
const MODEL_ACCESS_ITEMS: NavItem[] = [
  { to: "/upstreams/usage", label: t("API按量计费"), icon: Coins },
  { to: "/upstreams/plan", label: t("API订阅套餐"), icon: Ticket },
  { to: "/upstreams/private", label: t("API私有部署"), icon: ServerCog },
  { to: "/agent-accounts", label: t("开发工具订阅"), icon: Bot },
];

const DEVICE_ITEMS: NavItem[] = [
  { to: "/keys", label: t("API密钥"), icon: KeyRound },
  { to: "/usage", label: t("模型用量"), icon: ReceiptText },
  { to: "/status", label: t("设备状态"), icon: Activity },
  { to: "/network", label: t("网络/域名/代理"), icon: Network, aliases: ["/egress"] },
  { to: "/updates", label: t("设备更新"), icon: HardDriveDownload },
  { to: "/components", label: t("第三方组件"), icon: Blocks },
];

function NavLinks({ items, route }: { items: NavItem[]; route: string }) {
  return (
    <SidebarMenu>
      {items.map((it) => {
        const on = route === it.to || (it.aliases !== undefined && it.aliases.includes(route as BusinessRoute));
        return (
          <SidebarMenuItem key={it.to}>
            <SidebarMenuButton asChild isActive={on} tooltip={it.label}>
              <Link to={it.to}>
                <it.icon />
                <span>{it.label}</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        );
      })}
    </SidebarMenu>
  );
}

function Nav() {
  // slot 注册表渲染贡献时不传 props（registry.tsx 的 Slot 直接 <Component />），
  // 所以当前路由从 hook 取，不从上层传。
  const route = useLocationPath();
  const { authed } = useSession();
  if (!authed) return null;
  return (
    <SidebarContent>
      <SidebarGroup>
        <SidebarGroupLabel>{t("使用指南")}</SidebarGroupLabel>
        <NavLinks items={GUIDE_ITEMS} route={route} />
      </SidebarGroup>
      <SidebarGroup>
        <SidebarGroupLabel>{t("模型接入")}</SidebarGroupLabel>
        <NavLinks items={MODEL_ACCESS_ITEMS} route={route} />
      </SidebarGroup>
      <SidebarGroup>
        <SidebarGroupLabel>{t("设备")}</SidebarGroupLabel>
        <NavLinks items={DEVICE_ITEMS} route={route} />
      </SidebarGroup>
    </SidebarContent>
  );
}

registerSlot("sidebar", Nav, 50);
