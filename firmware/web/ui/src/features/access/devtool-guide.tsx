// 开发工具接入页分成两层：先安装 gate，再用 gate 管理五种开发工具；Codex CLI 与
// Codex App 各有一张帮助标签。接入地址和 API 密钥只作为第一层安装脚本的生成选项；
// 工具标签页只切换第二层使用帮助，不能
// 反过来改变 gate 安装命令。订阅授权和额外模型归每一把 API 密钥，在「API密钥 →
// 开发工具」中保存。本页按管理员的显式选择解封一次 Key，把明文只放在组件
// 内存与可复制安装命令里；不进 URL、查询缓存或持久化状态，也不把模型名拼进
// 安装命令。
//
// 同一组件也给「接入方法」页（/connect）的 Key 持有者用（holder 入参）：那时没有
// Key 下拉——Key 是使用者自己贴入的、已在内存里——安装命令直接带它；订阅可用性
// 与各工具的可见模型都来自这把 Key 自己的策略（GET /gate-helper/v1/config），
// 措辞从「去授权」改成「找管理员授权」。工具标签也只列这把 Key 真能用的
// （holderToolEnabled，与数据面子树闸同源）：一个都不开放时整段帮助换成一句说明。

import { toast } from "sonner";

import { DevToolIcon } from "@/components/brand-icon";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Sample,
  SegTabs,
  TargetTabs,
  type AccessTarget,
  type AccessTargets,
} from "@/features/access/access";
import * as api from "@/lib/api";
import { keyDisplay, keyMask } from "@/lib/format";
import { t, tx } from "@/lib/i18n";

export type DevToolHelpTab = api.DevToolName | "codex-app";

interface ToolUI {
  key: DevToolHelpTab;
  provider: api.DevToolName;
  icon: api.DevToolName;
  label: string;
  binary?: string;
  intro: string;
  installNote?: string;
  subscription: boolean;
}

const TOOLS: ToolUI[] = [
  {
    key: "codex",
    provider: "codex",
    icon: "codex",
    label: "Codex CLI",
    binary: "Codex CLI",
    intro: t(
      "用 LLM Gate 专用配置启动 Codex CLI，订阅模型与这把 Key 获准的额外模型会出现在独立模型目录中。直接运行 codex 仍使用原厂配置和登录态。",
    ),
    installNote: t("已有 Codex 时只建立关联；缺失时经设备取得 OpenAI 官方制品，校验 SHA-256 后原子安装。"),
    subscription: true,
  },
  {
    key: "codex-app",
    provider: "codex",
    icon: "codex",
    label: "Codex App",
    intro: t(
      "让不经 gate 启动的 Codex App 使用这台设备提供的 Codex 订阅和这把 Key 获准的额外模型。共享接入只修改当前用户的 Codex 配置，不替换 App。",
    ),
    subscription: true,
  },
  {
    key: "grok",
    provider: "grok",
    icon: "grok",
    label: "Grok Build",
    binary: "Grok Build",
    intro: t(
      "用 LLM Gate 专用配置启动 Grok Build。Grok Build 只使用这把 Key 获准的 Grok 订阅，不会改动直接运行 grok 时使用的配置。",
    ),
    installNote: t("已有 Grok Build 时只建立关联；缺失时经设备取得官方二进制。"),
    subscription: true,
  },
  {
    key: "claude",
    provider: "claude",
    icon: "claude",
    label: "Claude Code",
    binary: "Claude Code",
    intro: t(
      "用 LLM Gate 专用配置启动 Claude Code，读取这把 Key 获准的订阅与额外模型。直接运行 claude 仍使用原厂配置和登录态。",
    ),
    installNote: t(
      "已有 Claude Code 时只建立关联；缺失时经设备取得 Anthropic 官方制品并校验 SHA-256。关联和启动要求 gate 当前保存的是系统信任的 HTTPS 设备地址。",
    ),
    subscription: true,
  },
  {
    key: "opencode",
    provider: "opencode",
    icon: "opencode",
    label: "OpenCode",
    binary: "OpenCode",
    intro: t(
      "用 LLM Gate 专用配置启动 OpenCode，只显示这把 Key 获准的 OpenAI-compatible 目录模型。OpenCode 不使用设备订阅，直接运行 opencode 仍使用原厂配置和登录态。",
    ),
    installNote: t(
      "已有 OpenCode 时只建立关联；缺失时经设备取得官方 release，校验大小与 SHA-256 后原子安装。",
    ),
    subscription: false,
  },
  {
    key: "cursor",
    provider: "cursor",
    icon: "cursor",
    label: "Cursor CLI",
    binary: "cursor-agent",
    intro: t(
      "用 LLM Gate 专用配置启动 Cursor CLI。Cursor CLI 只使用这把 Key 获准的 Cursor 订阅，模型由 CLI 经设备订阅面自行发现，没有额外目录模型；直接运行 cursor-agent 仍使用原厂配置和登录态。",
    ),
    installNote: t(
      "已有 cursor-agent 时只建立关联；缺失时经设备取得 Cursor 官方安装物整树安装。gate 启动时关闭其自动更新，升级统一用 gate cursor update。",
    ),
    subscription: true,
  },
];

interface CommandUI {
  command: string;
  desc: string;
}

function toolCommands(tool: ToolUI): CommandUI[] {
  if (tool.key === "codex-app") {
    return [
      {
        command: "gate codex connect --shared",
        desc: t("自动查找可用的 Codex 内核，并把设备接入写入当前用户的 Codex 配置。"),
      },
      {
        command: t("gate codex connect --shared --path <Codex 内核路径>"),
        desc: t("Windows、Linux 或自动查找失败时，显式指定 Codex App 使用的内核。"),
      },
      {
        command: "gate codex status",
        desc: t("查看绑定的内核、版本和共享配置是否完整。"),
      },
      {
        command: "gate codex disconnect --shared",
        desc: t("撤销共享接入并还原原配置，不删除 Codex App 或内核。"),
      },
    ];
  }
  const prefix = `gate ${tool.key}`;
  const binary = tool.binary ?? "";
  return [
    { command: prefix, desc: t("用 LLM Gate 专用配置启动 {binary}；后续参数原样传给工具。", { binary }) },
    { command: `${prefix} connect`, desc: t("关联 PATH 中已有的 {binary}，不下载、不升级。", { binary }) },
    { command: `${prefix} install`, desc: t("确保 {binary} 可用；已有时只关联，缺失时经设备安装。", { binary }) },
    { command: `${prefix} status`, desc: t("查看关联路径、版本、来源和当前可见模型。") },
    { command: `${prefix} update`, desc: t("升级由 gate 安装和管理的 {binary}。", { binary }) },
    { command: `${prefix} update --adopt`, desc: t("明确改用设备提供的官方安装链，并交由 gate 管理。") },
    { command: `${prefix} disconnect`, desc: t("删除 gate 管理的派生配置，不删除 {binary} 程序。", { binary }) },
  ];
}

function shellURL(base: string): string {
  return base.replace(/["'\\$`<>|;&()\s]/g, (c) => `%${c.charCodeAt(0).toString(16).toUpperCase().padStart(2, "0")}`);
}

/** 明文只活在页面组件内存里；切换工具或接入地址时可以复用，不持久化。 */
export interface KeyFill {
  selectedID: number;
  plain: Map<number, string>;
}

/**
 * Key 持有者视角（「接入方法」页）：plaintext 是使用者贴入的那把 Key（只在内存），
 * policy 是它自己的开发工具策略。给了它，Key 下拉与解封整段不出现。
 */
export interface HolderContext {
  plaintext: string;
  policy: api.DevToolPolicy;
}

// holderToolEnabled 与数据面 /agents/<tool>/ 的子树闸同一判据
// （devtoolpolicy.Snapshot.ToolEnabled）：勾了该工具的订阅，或有目录模型投影到它。
// 两者都没有时这把 Key 打该子树恒 403 devtool_not_allowed，帮助标签就不该出现在
// 持有者视角里。Grok 与 Cursor 没有目录投影，等价于「勾了订阅」；OpenCode 不吃
// 订阅，等价于「有投影模型」。管理员视角不过这道筛子——那一页要看的是设备能提供
// 什么，不是某一把 Key 的授权。
function holderToolEnabled(policy: api.DevToolPolicy, provider: api.DevToolName): boolean {
  const configured = policy.subscriptions.some((sub) => sub.provider === provider && sub.configured);
  return configured || (policy.tools[provider]?.models.length ?? 0) > 0;
}

// HolderModels 列出这把 Key 在某个工具里当前可见的模型（策略投影的结果，与 gate
// 启动时写进派生配置的是同一份）。Cursor 没有模型级读数：由 CLI 经订阅面自行发现。
function HolderModels({ tool, policy }: { tool: ToolUI; policy: api.DevToolPolicy }): React.ReactElement {
  if (tool.provider === "cursor") {
    return (
      <p className="text-muted-foreground text-xs leading-5">
        {t("Cursor CLI 的模型由它自己经设备订阅面发现和选择，这里没有清单。")}
      </p>
    );
  }
  const entry = policy.tools[tool.provider];
  const models = entry?.models ?? [];
  if (models.length === 0) {
    return (
      <p className="text-muted-foreground text-xs leading-5">
        {t("这把 Key 当前在 {label} 里看不到任何模型：订阅未授权或不可用，也没有勾选的额外模型。请联系设备管理员。", {
          label: tool.label,
        })}
      </p>
    );
  }
  return (
    <div className="flex flex-col gap-2">
      <p className="text-muted-foreground text-xs leading-5">
        {t("这把 Key 在 {label} 里当前可见的模型（缺省 {model}）：", {
          label: tool.label,
          model: entry === undefined || entry.default_model === "" ? models[0]!.name : entry.default_model,
        })}
      </p>
      <div className="flex flex-wrap gap-1.5">
        {models.map((m) => (
          <Badge key={m.name} variant={m.source === "subscription" ? "secondary" : "outline"} className="font-mono">
            {m.name}
          </Badge>
        ))}
      </div>
    </div>
  );
}

export function DevToolGuide({
  targets,
  target,
  onTargetSelect,
  snap,
  keys,
  activeTab,
  setActiveTab,
  fill,
  setFill,
  holder,
}: {
  targets: AccessTargets;
  target: AccessTarget;
  onTargetSelect: (target: AccessTarget) => void;
  snap: api.AccessSnapshot;
  keys: api.ApiKey[];
  activeTab: DevToolHelpTab | null;
  setActiveTab: (tab: DevToolHelpTab) => void;
  fill: KeyFill;
  setFill: React.Dispatch<React.SetStateAction<KeyFill>>;
  holder?: HolderContext;
}): React.ReactElement {
  const tools = holder === undefined ? TOOLS : TOOLS.filter((item) => holderToolEnabled(holder.policy, item.provider));
  const hasCodex = tools.some((item) => item.provider === "codex");
  const active = activeTab ?? tools[0]?.key ?? "codex";
  const tool = tools.find((item) => item.key === active) ?? tools[0];
  const access =
    tool !== undefined && tool.subscription
      ? snap.agents?.find((item) => item.provider === tool.provider)
      : undefined;
  // gate 本身接受 HTTP 或 HTTPS；Claude Code 对 HTTPS 的额外要求只属于下方
  // Claude 使用帮助，不能让帮助标签页反过来改写或清空安装脚本。
  const base = target.lines[0]?.base;
  const encoded = base === undefined ? "" : shellURL(base);
  const selected = holder === undefined ? keys.find((key) => key.id === fill.selectedID) : undefined;
  const plaintext =
    holder !== undefined ? holder.plaintext : selected === undefined ? undefined : fill.plain.get(selected.id);
  const keyLoading = holder === undefined && selected !== undefined && plaintext === undefined;
  const bashKey = plaintext === undefined ? "" : ` "${plaintext}"`;
  const powershellKey = plaintext === undefined ? "" : ` '${plaintext}'`;
  const bash =
    base === undefined || keyLoading
      ? ""
      : `curl -fsSL "${encoded}/gate-helper/install.sh" | sh -s -- "${encoded}"${bashKey}`;
  const powershell =
    base === undefined || keyLoading
      ? ""
      : `& ([scriptblock]::Create((irm '${encoded}/gate-helper/install.ps1'))) '${encoded}'${powershellKey}`;

  async function selectKey(raw: string): Promise<void> {
    if (raw === "none") {
      setFill((prev) => ({ ...prev, selectedID: 0 }));
      return;
    }
    const id = Number(raw);
    const key = keys.find((item) => item.id === id);
    if (key === undefined || !key.plaintext_available) return;
    setFill((prev) => ({ ...prev, selectedID: id }));
    if (fill.plain.has(id)) return;
    try {
      const result = await api.revealKey(id);
      setFill((prev) => {
        const plain = new Map(prev.plain);
        plain.set(id, result.plaintext);
        return { ...prev, plain };
      });
    } catch (err) {
      setFill((prev) => (prev.selectedID === id ? { ...prev, selectedID: 0 } : prev));
      toast.error(api.errorMessage(err));
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <Card className="gap-5 p-6">
        <div>
          <h2 className="font-semibold">{t("安装 gate 工具")}</h2>
          <p className="text-muted-foreground mt-1 max-w-4xl text-sm leading-6">
            {t(
              "gate 是运行在开发电脑上的 LLM Gate 开发工具引导器。它保存当前设备地址和这位使用者的 API 密钥，用独立配置关联、安装、更新并启动 Codex CLI、Grok Build、Claude Code、OpenCode 与 Cursor CLI，也可以把 Codex App 共享连接到设备；直接运行原工具仍使用原厂配置和登录态。",
            )}
          </p>
        </div>

        <div className="bg-muted/20 flex flex-col gap-4 rounded-lg border p-4">
          <div>
            <h3 className="text-sm font-medium">{t("安装脚本选项")}</h3>
            <p className="text-muted-foreground mt-1 text-xs">
              {t("接入地址和 API 密钥只用于生成下面的 gate 安装脚本，不会随下方帮助标签改变。")}
            </p>
          </div>
          <div className="grid gap-5 lg:grid-cols-2 lg:items-start">
            <div className="flex min-w-0 flex-col gap-2">
              <span className="text-sm font-medium">{t("接入地址")}</span>
              <div className="max-w-full overflow-x-auto pb-1">
                <TargetTabs targets={targets} active={target} onSelect={onTargetSelect} />
              </div>
              <p className="text-muted-foreground text-xs leading-5">{target.note}</p>
              {base === undefined ? (
                <p className="text-muted-foreground text-xs">{target.empty}</p>
              ) : (
                <p className="text-muted-foreground min-w-0 text-xs">
                  {tx("脚本使用：<c>{base}</c>", {
                    c: (s) => <code className="text-foreground break-all">{s}</code>,
                    base,
                  })}
                </p>
              )}
            </div>
            <div className="flex min-w-0 flex-col gap-2">
              <span className="text-sm font-medium">{holder === undefined ? t("API 密钥（可选）") : t("API 密钥")}</span>
              {holder !== undefined ? (
                <p className="text-muted-foreground text-sm">
                  {tx("安装命令直接带上你贴入的这把 Key（<c>{key}</c>）。", {
                    c: (s) => <code className="text-foreground font-mono">{s}</code>,
                    key: keyMask(holder.plaintext),
                  })}
                </p>
              ) : keys.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  {t("当前没有已启用的 API 密钥，仍可生成不带 Key 的安装命令。")}
                </p>
              ) : (
                <Select
                  value={selected === undefined ? "none" : String(selected.id)}
                  onValueChange={(value) => void selectKey(value)}
                >
                  <SelectTrigger className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">{t("不选择 API 密钥")}</SelectItem>
                    {keys.map((key) => (
                      <SelectItem key={key.id} value={String(key.id)} disabled={!key.plaintext_available}>
                        {key.label === "" ? keyDisplay(key) : t("{label}（{key}）", { label: key.label, key: keyDisplay(key) })}
                        {key.plaintext_available ? "" : t(" — 明文不可恢复")}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              {holder === undefined ? (
                <p className="text-muted-foreground text-xs leading-5">
                  {t("不选择时沿用本机已有 Key；新安装且本机没有 Key 时，脚本从终端隐藏读取。")}
                </p>
              ) : null}
              {keyLoading ? <p className="text-muted-foreground text-sm">{t("正在读取所选 Key…")}</p> : null}
              {plaintext !== undefined ? (
                <p className="text-signal-alert text-xs leading-5">
                  {t("生成的命令包含所选 API Key。只复制到受信任的终端；命令可能保留在 shell history 和短时进程参数中。")}
                </p>
              ) : null}
            </div>
          </div>
        </div>

        {base === undefined ? null : keyLoading ? (
          <p className="text-muted-foreground text-sm">{t("读取完成后生成带 Key 的安装命令。")}</p>
        ) : (
          <div className="grid gap-4 lg:grid-cols-2 lg:items-start">
            <Sample title="macOS / Linux / WSL" text={bash} />
            <Sample title="Windows PowerShell" text={powershell} />
          </div>
        )}
        <p className="text-muted-foreground text-xs leading-5">
          {tx(
            "安装后执行 <c>gate update</c>，即可从当前设备检查并升级 gate，不会改变地址、Key 或工具配置。重复运行脚本也会升级 gate 并更新所选地址。显式选择新 Key 时，脚本验证成功才覆盖本机原 Key，验证失败自动回退；也可分别用 <c>gate -url <地址></c> 和 <c>gate -sk</c> 更新地址与 Key。",
            { c: (s) => <code>{s}</code> },
          )}
        </p>
      </Card>

      <section className="flex flex-col gap-4">
        <div>
          <h2 className="font-semibold">{t("使用帮助")}</h2>
          <p className="text-muted-foreground mt-1 text-xs">
            {tool === undefined
              ? t("这把 Key 目前没有可用的开发工具。")
              : hasCodex
                ? t("选择一个接入方式查看使用方法。Codex CLI 与 Codex App 分开说明，互不混放命令。")
                : t("选择一个接入方式查看使用方法。")}
          </p>
        </div>
        {tool === undefined ? (
          <Card className="p-6">
            <p className="text-muted-foreground text-sm leading-6">
              {t(
                "设备管理员还没有为这把 Key 授权任何开发工具：既没有勾选订阅，也没有把目录模型投影给开发工具。安装 gate 之后仍可用上面的接入地址直接调用 API；要用 Codex、Grok Build、Claude Code、OpenCode 或 Cursor CLI，请联系设备管理员。",
              )}
            </p>
          </Card>
        ) : (
          <>
            <div className="max-w-full overflow-x-auto pb-1">
              <SegTabs
                items={tools.map((item) => ({
                  key: item.key,
                  label: item.label,
                  icon: <DevToolIcon tool={item.icon} label={item.label} />,
                  lit: item.subscription
                    ? snap.agents?.some((agent) => agent.provider === item.provider && agent.available) === true
                    : true,
                }))}
                active={tool.key}
                onSelect={(key) => setActiveTab(key as DevToolHelpTab)}
              />
            </div>
            <Card className="gap-5 p-6">
              <div className="flex flex-wrap items-start gap-3">
                <DevToolIcon tool={tool.icon} label={tool.label} size="list" />
                <div className="min-w-0 flex-1">
                  <h3 className="font-semibold">{t("如何使用 {label}", { label: tool.label })}</h3>
                  <p className="text-muted-foreground mt-1 max-w-4xl text-sm leading-6">{tool.intro}</p>
                </div>
                <Badge variant={!tool.subscription || access?.available === true ? "outline" : "secondary"}>
                  {tool.subscription
                    ? access?.available === true
                      ? holder === undefined
                        ? t("设备订阅可用")
                        : t("这把 Key 可用该订阅")
                      : holder === undefined
                        ? t("设备订阅未连接或不可用")
                        : t("这把 Key 未获授权该订阅，或设备订阅当前不可用")
                    : t("无需设备订阅")}
                </Badge>
              </div>

              {tool.key === "codex-app" ? (
                <div className="grid gap-6 lg:grid-cols-[minmax(16rem,0.75fr)_minmax(0,1.25fr)] lg:items-start">
                  <div className="flex flex-col gap-3">
                    <div>
                      <h4 className="text-sm font-medium">{t("连接步骤")}</h4>
                      <p className="text-muted-foreground mt-1 text-xs leading-5">
                        {tx("Codex App 不经 <c>gate codex</c> 启动，需要用共享接入把设备配置写到当前用户的 Codex 配置目录。", {
                          c: (s) => <code>{s}</code>,
                        })}
                      </p>
                    </div>
                    <ol className="text-muted-foreground flex list-decimal flex-col gap-2 pl-5 text-xs leading-5">
                      <li>{t("先用上方安装命令配置 gate，并确认这把 Key 已获准使用 Codex 订阅或所需的额外模型。")}</li>
                      <li>
                        {tx(
                          "完全退出 Codex App，再在同一系统用户的终端运行共享连接命令。macOS 会自动查找 <c>ChatGPT.app</c> 内置的 Codex 内核。",
                          { c: (s) => <code>{s}</code> },
                        )}
                      </li>
                      <li>{t("命令成功后重新打开 App，再用状态命令检查共享配置。")}</li>
                    </ol>
                    <p className="text-muted-foreground text-xs leading-5">
                      {t(
                        "共享接入会把 API Key 保存在当前用户的 Codex 配置中；已有 ChatGPT 登录态保持不变。只在受信任的电脑上使用，断开前也要完全退出 App。",
                      )}
                    </p>
                  </div>
                  <div className="flex min-w-0 flex-col gap-3">
                    <h4 className="text-sm font-medium">{t("主要命令")}</h4>
                    <CommandList tool={tool} />
                  </div>
                </div>
              ) : (
                <div className="grid gap-6 lg:grid-cols-[minmax(16rem,0.75fr)_minmax(0,1.25fr)] lg:items-start">
                  <div className="flex flex-col gap-3">
                    <div>
                      <h4 className="text-sm font-medium">{t("首次使用")}</h4>
                      <p className="text-muted-foreground mt-1 text-xs leading-5">{tool.installNote}</p>
                    </div>
                    <Sample title={t("安装或关联，然后启动")} text={`gate ${tool.key} install\nsoc ${tool.key}`} />
                    <p className="text-muted-foreground text-xs leading-5">
                      {holder === undefined
                        ? t(
                            "每次关联、查看状态和启动前，gate 都会用自己的 Key 读取设备策略。实际可见模型取决于这把 Key 在「API密钥 → 开发工具」中的授权。",
                          )
                        : t("每次关联、查看状态和启动前，gate 都会用这把 Key 读取设备策略；可见模型由设备管理员为它授权。")}
                    </p>
                    {holder === undefined ? null : <HolderModels tool={tool} policy={holder.policy} />}
                  </div>

                  <div className="flex min-w-0 flex-col gap-3">
                    <h4 className="text-sm font-medium">{t("主要命令")}</h4>
                    <CommandList tool={tool} />
                  </div>
                </div>
              )}
            </Card>
          </>
        )}
      </section>
    </div>
  );
}

function CommandList({ tool }: { tool: ToolUI }): React.ReactElement {
  return (
    <dl className="overflow-hidden rounded-md border text-sm">
      {toolCommands(tool).map((item) => (
        <div
          key={item.command}
          className="grid gap-1 border-b px-3 py-2.5 last:border-b-0 md:grid-cols-[minmax(13rem,auto)_1fr] md:gap-4"
        >
          <dt>
            <code className="font-mono text-xs">{item.command}</code>
          </dt>
          <dd className="text-muted-foreground text-xs leading-5">{item.desc}</dd>
        </div>
      ))}
    </dl>
  );
}
