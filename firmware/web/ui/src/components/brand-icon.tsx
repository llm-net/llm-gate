import { Blocks } from "lucide-react";

import anthropicIcon from "@/assets/providers/anthropic.svg";
import baiduCloudIcon from "@/assets/providers/baiducloud.svg";
import bailianIcon from "@/assets/providers/bailian.svg";
import chenyuIcon from "@/assets/providers/chenyu.svg";
import claudeIcon from "@/assets/providers/claude.svg";
import codexIcon from "@/assets/providers/codex.svg";
import cursorIcon from "@/assets/providers/cursor.svg";
import deepseekIcon from "@/assets/providers/deepseek.svg";
import geminiIcon from "@/assets/providers/gemini.svg";
import grokIcon from "@/assets/providers/grok.svg";
import groqIcon from "@/assets/providers/groq.svg";
import kimiIcon from "@/assets/providers/kimi.svg";
import minimaxIcon from "@/assets/providers/minimax.svg";
import mistralIcon from "@/assets/providers/mistral.svg";
import openaiIcon from "@/assets/providers/openai.svg";
import opencodeIcon from "@/assets/providers/opencode.svg";
import openrouterIcon from "@/assets/providers/openrouter.svg";
import siliconCloudIcon from "@/assets/providers/siliconcloud.svg";
import stepfunIcon from "@/assets/providers/stepfun.svg";
import tencentCloudIcon from "@/assets/providers/tencentcloud.svg";
import togetherIcon from "@/assets/providers/together.svg";
import volcengineIcon from "@/assets/providers/volcengine.svg";
import xaiIcon from "@/assets/providers/xai.svg";
import zhipuIcon from "@/assets/providers/zhipu.svg";
import type { AgentProvider, DevToolName } from "@/lib/api";
import { cn } from "@/lib/cn";

type BrandIconSize = "tab" | "list" | "card";

const DEV_TOOL_ICONS: Record<DevToolName, string> = {
  codex: codexIcon,
  grok: grokIcon,
  claude: claudeIcon,
  opencode: opencodeIcon,
  cursor: cursorIcon,
};

// 平台身份来自可升级数据，先按 id 找具体品牌，再按稳定适配器 type 兜底。
// 图标取自 lobe-icons 收录的各厂商官方标（同目录 lobe-icons.LICENSE）；同一厂商的多个
// 平台身份共用同一品牌标（两种方舟计费形态用火山引擎、三种百炼形态用百炼、双站点
// MiniMax 文本面用 MiniMax、两个腾讯云面用腾讯云）；数据升级新增而这里没登记的 id
// 落到适配器的协议品牌。
const PLATFORM_ICONS: Record<string, string> = {
  chenyu: chenyuIcon,
  deepseek: deepseekIcon,
  ark: volcengineIcon,
  ark_plan: volcengineIcon,
  qwen_plan: bailianIcon,
  opencode_go: opencodeIcon,
  bailian_cn: bailianIcon,
  bailian_sg: bailianIcon,
  minimax: minimaxIcon,
  minimax_text_cn: minimaxIcon,
  minimax_text_intl: minimaxIcon,
  kimi_cn: kimiIcon,
  zhipu_cn: zhipuIcon,
  siliconflow_cn: siliconCloudIcon,
  qianfan_cn: baiduCloudIcon,
  gemini: geminiIcon,
  xai: xaiIcon,
  openrouter: openrouterIcon,
  stepfun_cn: stepfunIcon,
  tencent_tokenhub: tencentCloudIcon,
  tencent_lkeap: tencentCloudIcon,
  groq: groqIcon,
  mistral: mistralIcon,
  together: togetherIcon,
  openai_compat: openaiIcon,
  anthropic_compat: anthropicIcon,
};

const SHELL_SIZE: Record<BrandIconSize, string> = {
  tab: "size-5 rounded-md",
  list: "size-8 rounded-lg",
  card: "size-11 rounded-xl shadow-sm",
};

const IMAGE_SIZE: Record<BrandIconSize, string> = {
  tab: "size-3.5",
  list: "size-5",
  card: "size-8",
};

function IconShell({
  src,
  label,
  size,
}: {
  src: string | undefined;
  label: string;
  size: BrandIconSize;
}): React.ReactElement {
  return (
    <span
      className={cn(
        "inline-flex shrink-0 items-center justify-center border border-black/10 bg-white",
        SHELL_SIZE[size],
      )}
      title={label}
      aria-hidden="true"
    >
      {src === undefined ? (
        <Blocks className={cn("text-neutral-700", IMAGE_SIZE[size])} strokeWidth={1.8} />
      ) : (
        <img className={cn("object-contain", IMAGE_SIZE[size])} src={src} alt="" />
      )}
    </span>
  );
}

export function AgentProviderIcon({
  provider,
  size = "tab",
}: {
  provider: AgentProvider;
  size?: BrandIconSize;
}): React.ReactElement {
  return <IconShell src={DEV_TOOL_ICONS[provider]} label={provider} size={size} />;
}

export function DevToolIcon({
  tool,
  label,
  size = "tab",
}: {
  tool: DevToolName;
  label: string;
  size?: BrandIconSize;
}): React.ReactElement {
  return <IconShell src={DEV_TOOL_ICONS[tool]} label={label} size={size} />;
}

export function PlatformIcon({
  id,
  type,
  label,
  size = "tab",
}: {
  id: string;
  type: string;
  label: string;
  size?: BrandIconSize;
}): React.ReactElement {
  return <IconShell src={PLATFORM_ICONS[id] ?? PLATFORM_ICONS[type]} label={label} size={size} />;
}
