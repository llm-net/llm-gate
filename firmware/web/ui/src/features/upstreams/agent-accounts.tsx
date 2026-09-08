// 开发工具订阅列：把一份已有的 Agent 订阅（Codex / Grok Build / Claude Code / Cursor）
// 关联到这台设备。订阅凭据只留在设备里，不下发到任何人的电脑上。
//
// 一个 provider 只有一份订阅（服务端 UNIQUE(provider)，重复连接是覆盖），所以这一列
// 恒按四种订阅铺位：已连接的画成整张卡片，没连的画成一条虚线槽位、连接入口就长在槽上
// ——扫一眼就知道四种里哪几种接上了，不必先看空列表再去找按钮。
//
// 卡片三段：头部（品牌 + 名称 + 状态灯）、事实带（账号 / 默认或可见模型 / 最近刷新）、
// 动作行（自检、编辑外露，重新登录/停用/删除收进「⋯」菜单）。凭据过期时头部下方加一条
// 琥珀提示，修复入口直接长在那句话旁边；已停用整卡压暗、动作行留亮，「启用」就在动作行上。
//
// 各订阅按自己的官方授权方式接入：
//   Codex   授权码 + 人肉搬运回调 URL——授权完浏览器会停在一个**打不开的**
//           localhost:1455 页面上，不写清楚管理员会以为登录失败。
//   Grok    设备码流（RFC 8628）：显示 user_code，去浏览器批准，回来点「完成连接」。
//           **还没批准时服务端回 agent_login_pending，那不是失败**——留在原地提示
//           稍后再点，会话仍在。丢了这条分支，用户会以为连接失败。
//           设备不轮询，管理员的每一次点击就是一次尝试。
//   Claude  setup-token 用于模型调用；浏览器授权后粘回授权码，独立查询额度。
//   Cursor  API Key 粘贴：管理员在 cursor.com/dashboard 的 API Keys 页签发，粘给
//           设备封存。连接离线完成、不做联网验证（同 setup-token 纪律）；Key 是否
//           有效由「自检」（设备重新 exchange）与真实转发揭晓。

import { CircleHelp, TriangleAlert } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { AgentProviderIcon } from "@/components/brand-icon";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { SegTabs } from "@/features/access/access";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { fmtTime } from "@/lib/format";
import { t } from "@/lib/i18n";

import { PROVIDERS, reconnectVerb, StatusPill, visibleModelSemantics } from "./agent-status";
import { FactStrip, type Fact } from "./card-parts";
import { ClaudeConnect } from "./claude-connect";
import { AgentQuotaPanel } from "./agent-quota";
import { RowActionsMenu, type RowAction } from "./row-actions";

const DEFAULT_PROVIDER: api.AgentProvider = "codex";

function SubscriptionHelpDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{t("开发工具订阅说明")}</DialogTitle>
          <DialogDescription asChild>
            <div className="space-y-3 leading-relaxed">
              <p>
                {t("将已有的 Codex、Grok Build、Claude Code 或 Cursor 订阅接入设备后，获授权的成员可用自己的 API密钥，在对应的官方开发工具中使用这份订阅。订阅凭据只保存在设备中，不会下发给成员。")}
              </p>
              <p>
                {t("多人共享或高频使用可能触发上游限流、风控、停用或其他账号处置；设备不能保证订阅持续可用，也不能规避上游限制。")}
              </p>
              <p className="text-foreground font-medium">
                {t("仅接入你有权管理和共享的订阅，并确保所有使用者遵守对应上游的订阅协议、账号规则、组织政策和使用限制。")}
              </p>
            </div>
          </DialogDescription>
        </DialogHeader>
        <DialogFooter showCloseButton />
      </DialogContent>
    </Dialog>
  );
}

// ---- 连接对话框 ----

function ConnectDialog({
  open,
  initialProvider,
  onOpenChange,
  accounts,
  onDone,
}: {
  open: boolean;
  /** 打开时预选的订阅类型：列头「连接订阅」给缺省，卡片「重新登录/重新连接」给自己那家。 */
  initialProvider: api.AgentProvider;
  onOpenChange: (o: boolean) => void;
  accounts: api.AgentAccount[];
  onDone: () => void;
}) {
  const [provider, setProvider] = useState<api.AgentProvider>(initialProvider);
  const existingOf = (p: api.AgentProvider) => accounts.find((a) => a.provider === p);
  const existing = existingOf(provider);

  // 该 provider 已有账号时是「重新连接」，名称与默认模型预填其现值（服务端 Upsert 的
  // 「空即保持」口径）。切 provider 时重填。
  const [label, setLabel] = useState(existing?.label ?? "");
  const [model, setModel] = useState(existing?.default_model ?? "");
  const [error, setError] = useState<string | null>(null);

  // Codex / Grok 的第一步结果。
  const [start, setStart] = useState<api.AgentLoginStart | null>(null);
  const [starting, setStarting] = useState(false);
  const [callback, setCallback] = useState("");
  const [pending, setPending] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [importText, setImportText] = useState("");
  const [busy, setBusy] = useState(false);

  function switchProvider(p: api.AgentProvider): void {
    setProvider(p);
    const ex = existingOf(p);
    setLabel(ex?.label ?? "");
    setModel(ex?.default_model ?? "");
    setError(null);
    setStart(null);
    setCallback("");
    setPending("");
    setApiKey("");
    setImportText("");
  }

  function done(msg: string): void {
    setBusy(false);
    onOpenChange(false);
    toast(msg);
    onDone();
  }

  function beginLogin(p: api.OAuthAgentProvider): void {
    setStarting(true);
    setError(null);
    api.startAgentLogin(p).then(
      (res) => {
        setStarting(false);
        setStart(res);
      },
      (err: unknown) => {
        setStarting(false);
        setError(api.errorMessage(err));
      },
    );
  }

  function completeCodex(): void {
    const pasted = callback.trim();
    if (pasted === "") {
      setError(t("请先把浏览器地址栏里的整条地址粘贴进来"));
      return;
    }
    setError(null);
    setBusy(true);
    api.completeAgentLogin("codex", pasted, label.trim(), model.trim()).then(
      () => done(t("Codex 订阅已连接")),
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  function completeGrok(): void {
    setError(null);
    setPending("");
    setBusy(true);
    api.completeAgentLogin("grok", "", label.trim(), model.trim()).then(
      () => done(t("Grok Build 订阅已连接")),
      (err: unknown) => {
        setBusy(false);
        // **还没批准不是失败**：留在原地，提示稍后再点，会话仍在。
        if (err instanceof api.ApiError && err.code === "agent_login_pending") {
          setPending(api.errorMessage(err));
          return;
        }
        setError(api.errorMessage(err));
      },
    );
  }


  function submitCursor(): void {
    const value = apiKey.trim();
    if (value === "") {
      setError(t("请先粘贴 Cursor API Key"));
      return;
    }
    setError(null);
    setBusy(true);
    api.connectCursorAPIKey(value, label.trim()).then(
      () => {
        setApiKey("");
        done(t("Cursor 订阅已连接"));
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  function submitImport(): void {
    const value = importText.trim();
    if (value === "") {
      setError(t("请先粘贴 auth.json 的完整内容"));
      return;
    }
    setError(null);
    setBusy(true);
    api.importAgentAuth(provider as api.OAuthAgentProvider, value, label.trim(), model.trim()).then(
      () => done(t("{provider} 订阅已连接", { provider: api.agentProviderLabel(provider) })),
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  const secure = window.location.protocol === "https:";
  const redirect = start?.redirect_uri ?? "";
  const verifyURL = start?.verification_uri_complete ?? start?.verification_uri ?? "#";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>{t("连接 Agent 订阅")}</DialogTitle>
        </DialogHeader>
        <div className="flex flex-wrap items-center gap-3">
          <span className="text-sm font-medium">{t("订阅类型")}</span>
          <div className="max-w-full overflow-x-auto pb-1">
            <SegTabs
              items={PROVIDERS.map((p) => ({
                key: p,
                label: api.agentProviderLabel(p),
                icon: <AgentProviderIcon provider={p} />,
                lit: existingOf(p) !== undefined,
              }))}
              active={provider}
              onSelect={(k) => switchProvider(k as api.AgentProvider)}
            />
          </div>
        </div>
        <div className="flex flex-col gap-3">
          <div className="flex flex-col gap-1.5">
            <Label>
              {t("名称")}
              <span className="text-muted-foreground ml-1 text-xs font-normal">{t("仅本页展示")}</span>
            </Label>
            <Input
              value={label}
              maxLength={api.LabelMaxLen}
              placeholder={t("给这份订阅起个名字，可留空")}
              autoComplete="off"
              onChange={(e) => setLabel(e.target.value)}
            />
          </div>
          {provider === "cursor" ? (
            <p className="text-muted-foreground text-xs">
              {t("Cursor 的可用模型由 cursor-agent 经订阅面自行发现并保存选择，设备不维护默认模型或可见模型清单。")}
            </p>
          ) : (
            <div className="flex flex-col gap-1.5">
              {/* 同一个 default_model 字段两种语义：codex/grok 是写进成员 CLI 的默认
                  模型，claude 是「对成员可见的模型」（收窄模型发现）。 */}
              <Label>
                {visibleModelSemantics(provider) ? t("对成员可见的模型") : t("默认模型")}
                <span className="text-muted-foreground ml-1 text-xs font-normal">
                  {visibleModelSemantics(provider) ? t("留空 = 不收窄") : t("写进成员的 CLI 配置")}
                </span>
              </Label>
              <Input
                value={model}
                maxLength={api.CatalogNameMaxLen}
                placeholder={
                  provider === "claude" ? t("例如 claude-fable-5，可留空") : t("例如 gpt-5-codex / grok-4.5，可留空")
                }
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => setModel(e.target.value)}
              />
            </div>
          )}
        </div>

        {provider === "claude" ? (
          <ClaudeConnect label={label} model={model} secure={secure} account={existing} onDone={() => done(t("Claude 凭据已保存"))} />
        ) : provider === "cursor" ? (
          <div className="flex flex-col gap-2">
            <h3 className="text-sm font-medium">{t("在 Cursor Dashboard 签发 API Key")}</h3>
            <p className="text-muted-foreground text-xs">
              {t(
                "用你的 Cursor 订阅账号登录 cursor.com/dashboard，在 API Keys 页新建一把 API Key。设备只封存这把 Key，不索取账号密码，也不仿造登录流程；连接离线完成、不做联网验证，Key 是否有效由「自检」与真实转发揭晓。",
              )}
            </p>
            {secure ? (
              <p className="text-muted-foreground text-xs">
                {t("把生成的整串 Key 粘贴到下面。提交后由设备密钥封存，不会在响应、界面、日志或成员接入脚本里回显。")}
              </p>
            ) : (
              <p className="text-signal-alert text-xs">
                {t(
                  "当前管理页使用明文 HTTP，API Key 可能在链路上被截获。建议改用系统信任的 HTTPS 地址；如由反向代理提供 HTTPS，可在代理处终止 TLS 后以 HTTP 回源设备。",
                )}
              </p>
            )}
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="cursor-key">API Key</Label>
              <Input
                id="cursor-key"
                type="password"
                value={apiKey}
                autoComplete="off"
                spellCheck={false}
                placeholder={t("粘贴 Cursor Dashboard 生成的 API Key")}
                onChange={(e) => setApiKey(e.target.value)}
              />
            </div>
            <div>
              <Button disabled={busy} onClick={submitCursor}>
                {t("连接并封存")}
              </Button>
            </div>
          </div>
        ) : (
          <>
            <div className="flex flex-col gap-2">
              <h3 className="text-sm font-medium">{t("用浏览器登录（推荐）")}</h3>
              <p className="text-muted-foreground text-xs">
                {provider === "grok"
                  ? t("点「开始连接」拿到一串授权码，在浏览器里登录 xAI 并批准它。")
                  : t("点「开始连接」拿到授权链接，在浏览器里完成 OpenAI 登录与授权。")}
              </p>
              <div>
                <Button
                  disabled={starting}
                  onClick={() => beginLogin(provider === "grok" ? "grok" : "codex")}
                >
                  {start === null ? t("开始连接") : t("重新开始连接")}
                </Button>
              </div>
              {start === null ? null : provider === "grok" ? (
                <>
                  <p className="text-muted-foreground text-xs">
                    {t("在浏览器里打开授权页，登录 xAI 后核对并批准下面这串代码，然后回到本页点「完成连接」。")}
                  </p>
                  <div className="flex items-center gap-3 rounded-md border p-3">
                    <span className="text-muted-foreground text-xs">{t("授权码")}</span>
                    <code className="font-mono text-lg tracking-widest">{start.user_code ?? ""}</code>
                  </div>
                  <div>
                    <Button asChild>
                      <a href={verifyURL} target="_blank" rel="noopener noreferrer" title={t("在新标签页打开 xAI 授权页")}>
                        {t("打开授权页")}
                      </a>
                    </Button>
                  </div>
                  <p className="text-muted-foreground text-xs">
                    {t("授权码 {n} 分钟内有效，超时请重新点「开始连接」。", { n: Math.round(start.expires_in / 60) })}
                  </p>
                  <div>
                    <Button disabled={busy} onClick={completeGrok}>
                      {t("完成连接")}
                    </Button>
                  </div>
                  {/* 还没批准：留在原地，会话仍在——这不是错误提示。 */}
                  {pending === "" ? null : <p className="text-muted-foreground text-xs">{pending}</p>}
                </>
              ) : (
                <>
                  <p className="text-signal-alert text-xs">
                    {t(
                      "授权完成后，浏览器会跳到一个打不开的页面（地址以 {redirect} 开头）——这是正常的，本设备并不监听那个地址。请把浏览器地址栏里的整条地址复制下来，粘贴到下面的框里。",
                      { redirect },
                    )}
                  </p>
                  <div>
                    <Button asChild>
                      <a
                        href={start.authorize_url ?? "#"}
                        target="_blank"
                        rel="noopener noreferrer"
                        title={t("在新标签页打开 OpenAI 授权页")}
                      >
                        {t("打开授权页")}
                      </a>
                    </Button>
                  </div>
                  <p className="text-muted-foreground text-xs">
                    {t(
                      "授权链接 {n} 分钟内有效，超时请重新点「开始连接」。地址栏那条 URL 里带着一次性授权码，除本页外不要发给任何人。",
                      { n: Math.round(start.expires_in / 60) },
                    )}
                  </p>
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="codex-cb">{t("粘贴回调地址")}</Label>
                    <Textarea
                      id="codex-cb"
                      rows={3}
                      value={callback}
                      spellCheck={false}
                      placeholder={`${redirect}?code=...&state=...`}
                      onChange={(e) => setCallback(e.target.value)}
                    />
                  </div>
                  <div>
                    <Button disabled={busy} onClick={completeCodex}>
                      {t("完成连接")}
                    </Button>
                  </div>
                </>
              )}
            </div>

            {/* 兜底：粘贴在别处登录得到的 auth.json。 */}
            <div className="flex flex-col gap-2 border-t pt-3">
              <h3 className="text-sm font-medium">{t("或粘贴已有的 auth.json")}</h3>
              <p className="text-muted-foreground text-xs">
                {provider === "grok"
                  ? t("在别处登录过 Grok Build 的话，把 ~/.grok/auth.json 的完整内容粘进来。")
                  : t("在别处登录过 Codex 的话，把 ~/.codex/auth.json 的完整内容粘进来。")}
              </p>
              <Textarea
                rows={3}
                value={importText}
                spellCheck={false}
                placeholder='{"tokens":{...}}'
                onChange={(e) => setImportText(e.target.value)}
              />
              <div>
                <Button variant="outline" disabled={busy} onClick={submitImport}>
                  {t("导入并封存")}
                </Button>
              </div>
            </div>
          </>
        )}

        {error === null ? null : (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t("关闭")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// ---- 编辑 ----

function EditAgentDialog({
  target,
  onClose,
  onSaved,
}: {
  target: api.AgentAccount | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [label, setLabel] = useState(target?.label ?? "");
  const [model, setModel] = useState(target?.default_model ?? "");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (target === null) return <Dialog open={false} onOpenChange={() => undefined} />;
  const a = target;

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const patch: api.AgentAccountPatch = {};
    if (label.trim() !== a.label) patch.label = label.trim();
    if (a.provider !== "cursor" && model.trim() !== a.default_model) {
      patch.default_model = model.trim();
    }
    if (Object.keys(patch).length === 0) {
      onClose();
      toast(t("未做任何修改"));
      return;
    }
    setError(null);
    setBusy(true);
    api.updateAgentAccount(a.id, patch).then(
      () => {
        setBusy(false);
        onClose();
        toast(t("订阅已更新"));
        onSaved();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("编辑订阅 — {provider}", { provider: api.agentProviderLabel(a.provider) })}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="ag-label">{t("名称")}</Label>
            <Input
              id="ag-label"
              autoFocus
              value={label}
              maxLength={api.LabelMaxLen}
              placeholder={t("仅本页展示，可留空")}
              autoComplete="off"
              onChange={(e) => setLabel(e.target.value)}
            />
          </div>
          {a.provider === "cursor" ? null : (
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="ag-model">{visibleModelSemantics(a.provider) ? t("对成员可见的模型") : t("默认模型")}</Label>
              <Input
                id="ag-model"
                value={model}
                maxLength={api.CatalogNameMaxLen}
                placeholder={visibleModelSemantics(a.provider) ? t("留空 = 不收窄") : t("写进成员的 CLI 配置，可留空")}
                autoComplete="off"
                spellCheck={false}
                onChange={(e) => setModel(e.target.value)}
              />
            </div>
          )}
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy}>
              {t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 卡片 ----

// 一种订阅的连接方式，写在没连上的槽位上：管理员点「连接订阅」之前就知道要准备什么。
function connectMethod(p: api.AgentProvider): string {
  switch (p) {
    case "codex":
      return t("浏览器授权 OpenAI 账号");
    case "grok":
      return t("浏览器授权 xAI 账号");
    case "claude":
      return t("setup-token 用于调用，OAuth 授权用于查询额度");
    case "cursor":
      return t("粘贴 Cursor API Key");
  }
}

// 没连上的槽位：虚线框 + 压暗的品牌图标，连接入口就在槽上。
function ProviderSlot({
  provider,
  onConnect,
}: {
  provider: api.AgentProvider;
  onConnect: () => void;
}): React.ReactElement {
  return (
    <div className="flex items-center gap-3 rounded-xl border border-dashed px-4 py-3">
      <span className="opacity-60 grayscale">
        <AgentProviderIcon provider={provider} size="list" />
      </span>
      <div className="min-w-0 flex-1">
        <div className="text-sm leading-5 font-medium">{api.agentProviderLabel(provider)}</div>
        <div className="text-muted-foreground truncate text-xs leading-4">
          {t("未连接 · {method}", { method: connectMethod(provider) })}
        </div>
      </div>
      <Button size="sm" variant="outline" onClick={onConnect}>
        {t("连接订阅")}
      </Button>
    </div>
  );
}

function Muted({ children }: { children: React.ReactNode }): React.ReactElement {
  return <span className="text-muted-foreground">{children}</span>;
}

function AgentCard({
  a,
  onEdit,
  onReconnect,
  reload,
}: {
  a: api.AgentAccount;
  onEdit: () => void;
  onReconnect: () => void;
  reload: () => void;
}): React.ReactElement {
  const confirm = useConfirm();
  const [checking, setChecking] = useState(false);
  const disabled = a.status === "disabled";
  const expired = a.status === "auth_expired";

  async function selfCheck(): Promise<void> {
    setChecking(true);
    try {
      await api.refreshAgentAccount(a.id);
      toast(t("自检完成"));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    setChecking(false);
    reload();
  }

  async function toggle(): Promise<void> {
    try {
      await api.updateAgentAccount(a.id, { disabled: !disabled });
      toast(disabled ? t("订阅已启用") : t("订阅已停用"));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  async function remove(): Promise<void> {
    const ok = await confirm({
      title: t("删除订阅"),
      body: (
        <>
          <p>{t("确定删除这份 {provider} 订阅？", { provider: api.agentProviderLabel(a.provider) })}</p>
          <p className="text-destructive">{t("封存的订阅凭据一并销毁，成员经它转发的调用立即失败。")}</p>
        </>
      ),
      confirmText: t("确定删除"),
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteAgentAccount(a.id);
      toast(t("订阅已删除"));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  const facts: Fact[] = [
    {
      label: t("账号"),
      // claude / cursor 的连接流不存上游账号标识，
      // 这一格恒读作「不提供」，不是「未知」。
      value:
        a.provider === "claude" || a.provider === "cursor" ? (
          <Muted>{t("不提供")}</Muted>
        ) : a.account_id === "" ? (
          <Muted>{t("未知")}</Muted>
        ) : (
          <code className="font-mono" title={a.account_id}>
            {a.account_id}
          </code>
        ),
    },
  ];
  if (a.provider !== "cursor") {
    // Claude 这格读作「对成员可见的模型」：未设 = 不收窄 = 成员看得到全部。
    const visible = visibleModelSemantics(a.provider);
    facts.push({
      label: visible ? t("可见模型") : t("默认模型"),
      value:
        a.default_model === "" ? (
          <Muted>{visible ? t("全部") : t("未设")}</Muted>
        ) : (
          <code className="font-mono" title={a.default_model}>
            {a.default_model}
          </code>
        ),
    });
  }
  // Claude OAuth 展示续期时间，setup-token 展示固定凭据；Cursor 走常规时间戳，读作最近
  // 一次凭据自检/换发成功的时刻（自检 = 设备重新 exchange）。
  facts.push({
    label: a.provider === "claude" ? t("额度授权最近续期") : t("最近刷新"),
    value:
      a.provider === "claude" && !a.quota_oauth_configured ? (
        <Muted>{t("固定凭据")}</Muted>
      ) : a.last_refresh_at === null ? (
        <Muted>{t("从未刷新")}</Muted>
      ) : (
        fmtTime(a.last_refresh_at)
      ),
  });

  if (a.provider === "claude") {
    facts.push({ label: t("模型调用"), value: a.setup_token_configured ? expired ? t("setup-token 已失效") : t("setup-token 已配置") : t("待配置 setup-token") });
    facts.push({ label: t("额度授权"), value: a.quota_oauth_configured ? a.quota_oauth_expired ? t("需重新授权") : t("已授权") : t("未授权") });
  }

  // 凭据过期时修复入口已经长在琥珀提示上，菜单里不再重复；也先不提供启停——先修凭据。
  // cursor 的「重新连接」打开粘贴 API Key 的连接流（它没有 login/start 可走）。
  const menu: RowAction[] = [];
  if (!expired) menu.push({ label: reconnectVerb(a.provider), onSelect: onReconnect });
  if (!expired && !disabled) menu.push({ label: t("停用订阅"), onSelect: () => void toggle() });
  menu.push({ label: t("删除订阅"), destructive: true, onSelect: () => void remove() });

  return (
    <article
      className={cn("bg-card overflow-hidden rounded-xl border shadow-xs", expired && "border-signal-alert/60")}
    >
      <div className={cn("flex items-center gap-3 p-4", disabled && "opacity-60")}>
        <AgentProviderIcon provider={a.provider} size="card" />
        <div className="min-w-0 flex-1">
          <h3 className="text-base leading-6 font-semibold">{api.agentProviderLabel(a.provider)}</h3>
          {a.label === "" ? null : (
            <p className="text-muted-foreground truncate text-xs leading-5" title={a.label}>
              {a.label}
            </p>
          )}
        </div>
        <StatusPill a={a} />
      </div>
      {expired ? (
        <div className="bg-signal-alert/10 flex flex-wrap items-center gap-2 border-t px-4 py-2 text-xs">
          <TriangleAlert className="text-signal-alert size-3.5 shrink-0" aria-hidden="true" />
          <span className="min-w-0 flex-1">{t("凭据已失效，成员经这份订阅的调用会失败。")}</span>
          <Button size="xs" onClick={onReconnect}>
            {reconnectVerb(a.provider)}
          </Button>
        </div>
      ) : null}
      <FactStrip facts={facts} dim={disabled} />
      {a.provider === "claude" && <div className="flex items-center justify-between gap-2 border-t px-4 py-2 text-xs">
        <span className="text-muted-foreground">{t("setup-token 用于调用，OAuth 授权用于查询额度")}</span>
        <Button size="xs" variant="outline" onClick={onReconnect}>{t("配置 Claude 凭据")}</Button>
      </div>}
      <AgentQuotaPanel account={a} reload={reload} />
      <div className="flex items-center gap-1.5 border-t px-3 py-2">
        {disabled ? (
          <Button size="xs" variant="outline" onClick={() => void toggle()}>
            {t("启用")}
          </Button>
        ) : null}
        {a.provider === "claude" && !a.quota_oauth_configured ? null : (
          <Button size="xs" variant="outline" disabled={checking} onClick={() => void selfCheck()}>
            {checking ? t("自检中…") : a.provider === "claude" ? t("额度授权自检") : t("自检")}
          </Button>
        )}
        <Button size="xs" variant="outline" onClick={onEdit}>
          {t("编辑")}
        </Button>
        <div className="ml-auto">
          <RowActionsMenu
            label={t("订阅 {provider} 的更多操作", { provider: api.agentProviderLabel(a.provider) })}
            actions={menu}
          />
        </div>
      </div>
    </article>
  );
}

// ---- 列 ----

// 渲染成 Fragment：列头与卡片列表是页面网格的两个直接子节点（页面把两列的列头钉在
// 同一网格行上，两列的卡片顶因此对齐），对话框都是 Portal、不占格。
export function AgentAccountsColumn({
  accounts,
  reload,
}: {
  accounts: api.AgentAccount[];
  reload: () => void;
}): React.ReactElement {
  // null = 关闭；有值 = 打开且预选这一家（槽位给自己那家，卡片「重新登录」也给自己那家）。
  const [connecting, setConnecting] = useState<api.AgentProvider | null>(null);
  const [editing, setEditing] = useState<api.AgentAccount | null>(null);
  const [helpOpen, setHelpOpen] = useState(false);

  // 已连接的按 PROVIDERS 顺序排前面，没连的槽位跟在后面：整张卡片是「在用的」，
  // 虚线槽位是「还能接的」，两段各自连续。
  const connected = PROVIDERS.flatMap((p) => accounts.filter((a) => a.provider === p));
  const vacant = PROVIDERS.filter((p) => !accounts.some((a) => a.provider === p));

  return (
    <>
      <div className="flex flex-col gap-1 lg:col-start-1 lg:row-start-1">
        <div className="flex min-h-8 flex-wrap items-center gap-2">
          <h2 className="text-base font-semibold">{t("订阅")}</h2>
          <Badge variant="secondary" title={t("共 {n} 份订阅", { n: connected.length })}>
            {connected.length}
          </Badge>
          <Button variant="ghost" size="xs" className="text-muted-foreground" onClick={() => setHelpOpen(true)}>
            <CircleHelp />
            {t("说明")}
          </Button>
        </div>
        <p className="text-muted-foreground text-xs">
          {t("四种订阅各接一份；凭据只封存在设备里，成员用自己的 API密钥经设备使用。")}
        </p>
      </div>
      <div className="flex flex-col gap-3 lg:col-start-1 lg:row-start-2">
        {connected.map((a) => (
          <AgentCard
            key={a.id}
            a={a}
            reload={reload}
            onEdit={() => setEditing(a)}
            onReconnect={() => setConnecting(a.provider)}
          />
        ))}
        {vacant.map((p) => (
          <ProviderSlot key={p} provider={p} onConnect={() => setConnecting(p)} />
        ))}
      </div>

      <SubscriptionHelpDialog open={helpOpen} onOpenChange={setHelpOpen} />

      <ConnectDialog
        key={`connect-${connecting ?? "closed"}`}
        open={connecting !== null}
        initialProvider={connecting ?? DEFAULT_PROVIDER}
        onOpenChange={(o) => {
          if (!o) setConnecting(null);
        }}
        accounts={accounts}
        onDone={reload}
      />
      <EditAgentDialog
        key={`ea-${editing?.id ?? "none"}`}
        target={editing}
        onClose={() => setEditing(null)}
        onSaved={reload}
      />
    </>
  );
}
