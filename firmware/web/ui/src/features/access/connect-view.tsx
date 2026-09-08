// 「接入方法」页（/ui/connect）：整个 /ui/ 里第二张无会话页面（另一张是登录页）。
//
// 给**拿到一把 API 密钥、但不是设备管理员**的人看。贴入自己的 Key → 页面用这把
// Key 打数据面两条只读端点（GET /gate-helper/v1/endpoints、GET /gate-helper/v1/config）
// → 按这把 Key 渲染「开发工具接入」与「API调用」两个标签页，缺省停在开发工具接入。
// 正文与管理台同名页面是同一组组件（features/access），只是数据按 Key 裁剪、
// 安装命令直接带这把 Key。
//
// **Key 只活在登录入口与本组件的 state 里**：不进 localStorage / sessionStorage / URL /
// 查询缓存，刷新即回到贴 Key 的状态（§15.1 同款纪律）。没有会话、没有 Cookie、
// 没有「退出」——「返回登录」只是清掉内存、回到贴 Key 的那一屏。管理员登着的时候打开它看到的也是
// Key 视角：这一页不问会话。
//
// 两段外观：贴 Key 用登录页那块设备面板（AuthShell），拿到读数后换成一条窄顶栏 +
// 铺满内容区的正文——正文是双栏、摆着整段命令，登录卡那么窄摆不下。

import { ArrowRight, KeyRound, LoaderCircle } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import logoUrl from "@/assets/logo.svg";
import { LangSwitch } from "@/components/lang-switch";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  accessTargets,
  defaultTarget,
  SegTabs,
  TargetTabs,
  type TargetKind,
} from "@/features/access/access";
import { ApiGuide } from "@/features/access/api-guide";
import { DevToolGuide, type DevToolHelpTab, type KeyFill } from "@/features/access/devtool-guide";
import { ModelsCard } from "@/features/access/models-list";
import * as api from "@/lib/api";
import * as brand from "@/lib/brand";
import { keyMask } from "@/lib/format";
import { t } from "@/lib/i18n";
import { PageContainer, PageHeader } from "@/pages/page-shell";

export interface HolderData {
  snap: api.KeyAccessSnapshot;
  policy: api.DevToolPolicy;
}

export interface KeyHolder {
  key: string;
  data: HolderData;
}

// loadHolder 并发取两份读数；任一 401 即整体失败（Key 无效）。
async function loadHolder(key: string): Promise<HolderData> {
  const [snap, policy] = await Promise.all([api.keyEndpoints(key), api.keyDevToolConfig(key)]);
  return { snap, policy };
}

// holderError 把数据面的错误翻成人话：401 是 Key 的事，网络错误已是中文，其余只报状态码
// （数据面的 message 是给程序看的英文，不直接展示）。
function holderError(err: unknown): string {
  if (err instanceof api.ApiError) {
    if (err.status === 401) return t("API 密钥无效或已停用，请核对后重试。");
    if (err.status === 0) return err.message;
    return t("读取接入信息失败（HTTP {status}），请稍后重试。", { status: err.status });
  }
  return api.errorMessage(err);
}

export function KeyEntry({
  initialError,
  onAccepted,
}: {
  initialError: string | null;
  onAccepted: (key: string, data: HolderData) => void;
}): React.ReactElement {
  const [input, setInput] = useState("");
  const [error, setError] = useState<string | null>(initialError);
  const [busy, setBusy] = useState(false);

  const alive = useRef(false);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (busy) return;
    const key = input.trim();
    if (key === "") return;
    setError(null);
    setBusy(true);
    loadHolder(key).then(
      (data) => { if (alive.current) onAccepted(key, data); },
      (err: unknown) => {
        if (!alive.current) return;
        setBusy(false);
        setError(holderError(err));
      },
    );
  }

  return (
    <form className="auth-form" onSubmit={submit} aria-busy={busy}>
      <p className="auth-form-intro">{t("查看这台设备的调用地址、可用模型与开发工具接入方法。")}</p>
      <label className="auth-field">
        {t("API 密钥")}
        <span className="auth-input-wrap">
          <KeyRound aria-hidden />
          <Input
            required
            type="password"
            value={input}
            autoComplete="off"
            spellCheck={false}
            placeholder="sk_…"
            aria-invalid={error !== null}
            aria-describedby={error === null ? "key-entry-hint" : "key-entry-hint key-entry-error"}
            onChange={(e) => setInput(e.target.value)}
            className="auth-input font-mono"
          />
        </span>
      </label>
      <p id="key-entry-hint" className="auth-form-hint">
        {t("Key 只用于向这台设备读取你自己的接入信息，不会保存在浏览器里；刷新或关闭页面后需要重新贴入。")}
      </p>
      <div className="auth-form-message">
        {error === null ? null : <p id="key-entry-error" role="alert" className="text-destructive text-sm">{error}</p>}
      </div>
      <Button type="submit" disabled={busy} className="auth-submit">
        {busy ? <LoaderCircle aria-hidden className="animate-spin motion-reduce:animate-none" /> : null}
        {busy ? t("正在读取…") : t("查看接入方法")}
        {busy ? null : <ArrowRight aria-hidden className="auth-submit-arrow" />}
      </Button>
    </form>
  );
}

type HolderTab = "api" | "dev-tools";

function HolderView({
  holderKey,
  data,
  refreshing,
  onRefresh,
  onReset,
}: {
  holderKey: string;
  data: HolderData;
  refreshing: boolean;
  onRefresh: () => void;
  onReset: () => void;
}): React.ReactElement {
  const [tab, setTab] = useState<HolderTab>("dev-tools");
  const [kind, setKind] = useState<TargetKind | null>(null);
  const [activeToolTab, setActiveToolTab] = useState<DevToolHelpTab | null>(null);
  // DevToolGuide 的 Key 下拉状态在持有者视角里用不到（holder 入参接管了 Key），
  // 但组件契约要它在场；给一份空的、永不写入的。
  const [fill, setFill] = useState<KeyFill>({ selectedID: 0, plain: new Map() });

  const targets = accessTargets(data.snap.endpoints);
  const target = targets.find((tg) => tg.kind === kind) ?? defaultTarget(targets);
  // DevToolGuide 吃的是管理员那份快照形状；订阅可用性从这把 Key 自己的策略里拼。
  const snap: api.AccessSnapshot = {
    endpoints: data.snap.endpoints,
    models: data.snap.models,
    ...(data.snap.aigc_models === undefined ? {} : { aigc_models: data.snap.aigc_models }),
    agents: data.policy.subscriptions.map((sub) => ({
      provider: sub.provider,
      available: sub.available,
      default_model: sub.default_model,
    })),
    firmware_version: data.snap.firmware_version,
    hardware_model: data.snap.hardware_model,
  };

  return (
    <div className="flex h-svh min-h-0 flex-col">
      <header className="bg-card flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 border-b px-4 py-2.5 sm:px-6">
        <div className="flex min-w-0 items-center gap-2">
          <img src={logoUrl} alt="" aria-hidden className="size-6 shrink-0" />
          <span className="font-semibold">{brand.productName}</span>
          <span className="text-muted-foreground text-sm">· {t("接入方法")}</span>
        </div>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <Badge variant="outline" className="gap-1 font-mono" title={t("当前使用的 API 密钥")}>
            <KeyRound className="size-3" aria-hidden />
            {keyMask(holderKey)}
          </Badge>
          <Button type="button" size="sm" variant="outline" onClick={onReset}>
            {t("返回登录")}
          </Button>
          <LangSwitch />
        </div>
      </header>
      <main className="min-h-0 flex-1 overflow-y-auto">
        <PageContainer wide>
          <PageHeader
            title={tab === "api" ? t("API调用") : t("开发工具接入")}
            note={
              tab === "api"
                ? t("先选从哪个地址连到这台设备，再复制对应的调用地址和示例。")
                : t("先安装 gate 工具，再查看各开发工具的独立接入方法。")
            }
            actions={
              <SegTabs
                items={[
                  { key: "dev-tools", label: t("开发工具接入") },
                  { key: "api", label: t("API调用") },
                ]}
                active={tab}
                onSelect={(key) => setTab(key as HolderTab)}
                size="sm"
              />
            }
            refreshing={refreshing}
            onRefresh={onRefresh}
          />
          {tab === "api" ? (
            <>
              <div className="flex flex-col gap-2">
                <div className="flex flex-wrap items-center gap-3">
                  <span className="text-sm font-medium">{t("接入地址")}</span>
                  <TargetTabs targets={targets} active={target} onSelect={(next) => setKind(next.kind)} />
                </div>
                <p className="text-muted-foreground text-xs">{target.note}</p>
              </div>
              <ApiGuide target={target} models={data.snap.models} audience="holder" />
              <ModelsCard models={data.snap.models} aigc={data.snap.aigc_models ?? []} audience="holder" />
            </>
          ) : (
            <DevToolGuide
              targets={targets}
              target={target}
              onTargetSelect={(next) => setKind(next.kind)}
              snap={snap}
              keys={[]}
              activeTab={activeToolTab}
              setActiveTab={setActiveToolTab}
              fill={fill}
              setFill={setFill}
              holder={{ plaintext: holderKey, policy: data.policy }}
            />
          )}
        </PageContainer>
      </main>
    </div>
  );
}

export function ConnectView({ initialHolder, onReset }: {
  initialHolder: KeyHolder;
  onReset: (error: string | null) => void;
}): React.ReactElement {
  const [holder, setHolder] = useState(initialHolder);
  const [refreshing, setRefreshing] = useState(false);
  const alive = useRef(false);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);

  const { key } = holder;
  function refresh(): void {
    setRefreshing(true);
    loadHolder(key).then(
      (data) => {
        if (!alive.current) return;
        setRefreshing(false);
        setHolder({ key, data });
      },
      (err: unknown) => {
        if (!alive.current) return;
        setRefreshing(false);
        const message = holderError(err);
        if (err instanceof api.ApiError && err.status === 401) {
          // Key 在这期间被停用或删除：回到贴 Key 的状态，把原因带过去。
          onReset(message);
          return;
        }
        toast.error(message);
      },
    );
  }

  return (
    <HolderView
      holderKey={key}
      data={holder.data}
      refreshing={refreshing}
      onRefresh={refresh}
      onReset={() => onReset(null)}
    />
  );
}
