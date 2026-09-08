// 开发工具订阅两列共用的小件：provider 顺序、修复动词、default_model 语义与状态灯。
// 左列的订阅卡与右列目录价表的分组行读的是同一份状态判据，改这里两边一起变。
// 灯的样子（Pill）在 card-parts.tsx，与账号卡同一个件。

import * as api from "@/lib/api";
import { t } from "@/lib/i18n";

import { Pill } from "./card-parts";

/** 四种订阅的固定顺序：左列铺位、右列分组都按它排。 */
export const PROVIDERS: api.AgentProvider[] = ["codex", "grok", "claude", "cursor"];

// 修复动作的名字：claude / cursor 的凭据是管理员粘贴封存的固定值，没有「登录」
// 可重来——状态灯与修复按钮都说「重新连接」（走各自的粘贴流）；codex / grok 说
// 「重新登录」（走浏览器登录流）。
export function reconnectVerb(p: api.AgentProvider): string {
  return p === "claude" || p === "cursor" ? t("重新连接") : t("重新登录");
}

// Claude 的 default_model 是「对成员可见的模型」（收窄模型发现）；Codex / Grok
// 是写进成员 CLI 配置的默认模型。Cursor 透明代理没有模型配置字段。
export function visibleModelSemantics(p: api.AgentProvider): boolean {
  return p === "claude";
}

// 状态灯：三态各一盏，灯旁恒写着那句话本身——颜色从不单独承担语义。auth_expired 是
// 「需要管理员留意」的琥珀，不是故障红：凭据还在，只是要重新登录一次；已停用是熄灭灰。
export function StatusPill({ a }: { a: api.AgentAccount }): React.ReactElement {
  if (a.status === "disabled") {
    return (
      <Pill dot="bg-muted-foreground/40" className="text-muted-foreground">
        {t("已停用")}
      </Pill>
    );
  }
  if (a.status === "auth_expired") {
    return (
      <Pill dot="bg-signal-alert" className="text-signal-alert">
        {t("需{verb}", { verb: reconnectVerb(a.provider) })}
      </Pill>
    );
  }
  if (a.provider === "claude" && !a.setup_token_configured) {
    return <Pill dot="bg-signal-alert" className="text-signal-alert">{t("待配置 setup-token")}</Pill>;
  }
  return <Pill dot="bg-signal-ok">{t("已连接")}</Pill>;
}
