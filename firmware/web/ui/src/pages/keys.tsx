// API密钥页：签发、复制、分享、限额、按量额度、启停、删除，以及每把 Key 的两份
// 授权——「可用订阅」（Agent 订阅四开关）与「可用模型」（调用范围 + 开发工具
// 可见）。
//
// 设备只有一个管理员，密钥因此没有「属主」这一维（用户概念退场前本页还带
// 属主列与属主过滤，签发时还要先选一个人）。一台设备一串密钥，这一页就是它们
// 的全部。
//
// 列表**永不含明文与摘要**（服务端契约）：能看的是 display_prefix…last4；
// 明文封存入库，随时可经「复制」按钮解封取回一次（服务端记一条 key.reveal
// 审计）。「分享」走同一次解封，只是把明文与「接入方法」页地址拼成一段发得出去
// 的文本（features/keys/key-share.tsx）。签发于旧固件的行没有封存明文，那些行
// 不给复制按钮、分享按钮置灰——明文物理上不可恢复，出路是重签一把。
//
// 密钥级限额：日/周/月预算（元）、一次性按量额度与每分钟请求数上限。准入
// 固定检查 RPM → 日 → 周 → 月；RPM 命中立即拒绝，预算命中时有按量额度则
// 继续放行并扣减，额度耗尽才拒绝。它只挡**新**请求，在途的流与已提交的
// 视频任务照常跑完并入账。

import { useEffect, useState } from "react";
import { Check, Copy, Pencil } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { KeyPlaintextDialog, type KeyPlaintext } from "@/features/keys/key-plaintext";
import { buildShareText, KeyShareDialog } from "@/features/keys/key-share";
import {
  budgetNote,
  budgetValue,
  BudgetGauge,
  LimitInput,
  LimitsCell,
  readBudget,
  readRPM,
  rpmValue,
  StatusBadge,
} from "@/features/limits/limits";
import { RowActionsMenu } from "@/features/upstreams/row-actions";
import * as api from "@/lib/api";
import { copyText } from "@/lib/clipboard";
import { useConfirm } from "@/lib/confirm";
import { fmtTime, keyDisplay } from "@/lib/format";
import { t, tx } from "@/lib/i18n";
import { type Resource, useResource } from "@/lib/use-resource";

import { PageContainer, PageHeader, ResourceGate } from "./page-shell";

// ---- 可用订阅对话框 ----

// 这把 Key 能使用设备上的哪些 Agent 订阅（Codex / Grok Build / Claude Code /
// Cursor）。目录模型层面的授权（调用范围与开发工具可见）在「可用模型」里配。
function SubscriptionsDialog({
  target,
  onClose,
}: {
  target: api.ApiKey | null;
  onClose: () => void;
}) {
  const [data, setData] = useState<api.DevToolConfig | null>(null);
  const [subscriptions, setSubscriptions] = useState<api.DevToolSubscriptionChoices>({
    codex: false,
    grok: false,
    claude: false,
    cursor: false,
  });
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (target === null) return;
    let active = true;
    setData(null);
    setError(null);
    api.getKeyDevTools(target.id).then(
      (got) => {
        if (!active) return;
        setData(got);
        setSubscriptions(got.subscriptions);
      },
      (err: unknown) => {
        if (active) setError(api.errorMessage(err));
      },
    );
    return () => {
      active = false;
    };
  }, [target]);

  function subscriptionState(provider: api.AgentProvider): string {
    const state = data?.subscription_status.find((item) => item.provider === provider);
    if (state?.available) return t("设备订阅当前可用");
    return subscriptions[provider] ? t("已配置，当前不可用") : t("设备订阅当前不可用");
  }

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (target === null) return;
    setBusy(true);
    setError(null);
    api.putKeyDevTools(target.id, subscriptions).then(
      () => {
        setBusy(false);
        toast(t("Key {key} 的可用订阅已更新", { key: keyDisplay(target) }));
        onClose();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog open={target !== null} onOpenChange={(open) => !open && onClose()}>
      {target === null ? null : (
        <DialogContent>
          <form className="flex flex-col gap-4" onSubmit={submit}>
            <DialogHeader>
              <DialogTitle>{t("可用订阅 — {key}", { key: keyDisplay(target) })}</DialogTitle>
            </DialogHeader>
            {data === null && error === null ? <p className="text-muted-foreground text-sm">{t("正在读取配置…")}</p> : null}
            {data === null ? null : (
              <>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  {t("勾选这把 Key 能使用的 Agent 订阅，四项独立授权；订阅未连接或凭据失效时仍可保留选择。目录模型的授权在「可用模型」里配置。")}
                </p>
                <div className="flex flex-col gap-3">
                  {(
                    [
                      ["codex", "Codex"],
                      ["grok", "Grok Build"],
                      ["claude", "Claude Code"],
                      ["cursor", "Cursor"],
                    ] as const
                  ).map(([provider, label]) => (
                    <label key={provider} className="flex cursor-pointer items-start gap-3 rounded-md border p-3">
                      <Checkbox
                        checked={subscriptions[provider]}
                        onCheckedChange={(checked) =>
                          setSubscriptions((current) => ({ ...current, [provider]: checked === true }))
                        }
                      />
                      <span className="min-w-0">
                        <span className="block text-sm font-medium">{label}</span>
                        <span className="text-muted-foreground block text-xs">{subscriptionState(provider)}</span>
                      </span>
                    </label>
                  ))}
                </div>
              </>
            )}
            {error === null ? null : (
              <p role="alert" className="text-destructive text-sm">
                {error}
              </p>
            )}
            <DialogFooter>
              <Button type="button" variant="outline" onClick={onClose}>{t("取消")}</Button>
              <Button type="submit" disabled={busy || data === null}>{t("保存")}</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      )}
    </Dialog>
  );
}

// ---- 可用模型对话框 ----

// 一张清单回答两个叠加的问题：这把 Key 能调用哪些模型（「可用」列，管厂商兼容
// API 面——/v1/chat/completions、/v1/messages、/v1/responses、AIGC 官方路径，
// OpenCode 也经这个面），以及其中哪些要出现在开发工具里（「开发工具可见」列，
// 即写进 Codex / Claude Code / OpenCode 模型列表的目录模型）。候选恒为
// **API密钥接入**承载的模型：订阅接入的模型没有可路由的上游来源，本来就调不通
// API 入口，不进这份清单；订阅的授权在「可用订阅」里。
//
// 调用范围两态而不是「空即不限」：缺省不限制（既有 Key 行为不变、新加的模型
// 自动可用），切到「仅勾选的模型可用」才按勾选收窄——那样「一个都不给」也说得
// 出来。规则只有一条：开发工具可见 ⊆ 可用。收窄时取消「可用」连带取消可见，
// 保存时服务端同规校验；不限制模式不留潜藏的勾选（保存即清空 model_ids），
// 再次收窄从「全部可用」重新出发，不会冒出上一轮的旧选择。
const DEV_TOOL_REASON: Record<string, string> = {
  not_text: t("非文本模型"),
  model_disabled: t("模型已停用"),
  no_source: t("没有挂上游来源"),
  protocol_incompatible: t("协议面不兼容"),
  source_unavailable: t("来源或其上游已停用"),
};

function ModelsDialog({
  target,
  onClose,
}: {
  target: api.ApiKey | null;
  onClose: () => void;
}) {
  const [data, setData] = useState<api.KeyAPIModels | null>(null);
  const [restricted, setRestricted] = useState(false);
  const [selected, setSelected] = useState<number[]>([]);
  const [devSelected, setDevSelected] = useState<number[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (target === null) return;
    let active = true;
    setData(null);
    setError(null);
    api.getKeyAPIModels(target.id).then(
      (got) => {
        if (!active) return;
        setData(got);
        setRestricted(got.restricted);
        setSelected(got.model_ids);
        // 收窄配置下越出可用范围的可见选择（只可能来自绕过界面的历史写入）
        // 按同一条规则归位显示；保存即修正。
        setDevSelected(
          got.restricted
            ? got.dev_tool_model_ids.filter((id) => got.model_ids.includes(id))
            : got.dev_tool_model_ids,
        );
      },
      (err: unknown) => {
        if (active) setError(api.errorMessage(err));
      },
    );
    return () => {
      active = false;
    };
  }, [target]);

  const models = data?.models ?? [];

  function switchMode(nextRestricted: boolean): void {
    if (!nextRestricted) {
      setRestricted(false);
      return;
    }
    // 首次收窄从「全部可用」出发预勾全部，管理员做减法；开发工具可见的选择
    // 因此不会被切换动作顺手清掉。
    let nextSelected = selected;
    if (selected.length === 0) {
      nextSelected = models.filter((m) => m.selectable).map((m) => m.id);
      setSelected(nextSelected);
    }
    setDevSelected((current) => current.filter((id) => nextSelected.includes(id)));
    setRestricted(true);
  }

  function toggleAvailable(id: number, next: boolean): void {
    setSelected((current) =>
      next ? Array.from(new Set([...current, id])) : current.filter((x) => x !== id),
    );
    if (!next) setDevSelected((current) => current.filter((x) => x !== id));
  }

  function toggleDevTool(id: number, next: boolean): void {
    setDevSelected((current) =>
      next ? Array.from(new Set([...current, id])) : current.filter((x) => x !== id),
    );
  }

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (target === null) return;
    setBusy(true);
    setError(null);
    api.putKeyAPIModels(target.id, restricted, restricted ? selected : [], devSelected).then(
      () => {
        setBusy(false);
        toast(t("Key {key} 的可用模型已更新", { key: keyDisplay(target) }));
        onClose();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog open={target !== null} onOpenChange={(open) => !open && onClose()}>
      {target === null ? null : (
        <DialogContent className="sm:max-w-3xl">
          <form className="flex flex-col gap-4" onSubmit={submit}>
            <DialogHeader>
              <DialogTitle>{t("可用模型 — {key}", { key: keyDisplay(target) })}</DialogTitle>
            </DialogHeader>
            {data === null && error === null ? <p className="text-muted-foreground text-sm">{t("正在读取配置…")}</p> : null}
            {data === null ? null : (
              <>
                {/* 开关语义是「不限制」：开 = 全部可用（缺省），关 = 只用勾选的。 */}
                <label className="flex w-fit cursor-pointer items-center gap-2">
                  <Switch
                    checked={!restricted}
                    aria-label={t("可用所有模型（包括后续新增）")}
                    onCheckedChange={(next) => switchMode(next !== true)}
                  />
                  <span className="text-sm">{t("可用所有模型（包括后续新增）")}</span>
                </label>
                {models.length === 0 ? (
                  <p className="text-muted-foreground rounded-md border border-dashed p-4 text-sm">
                    {t("当前没有 API密钥接入 的模型。到「模型接入」下的「API按量计费」或「API订阅套餐」页录入账号并添加模型。")}
                  </p>
                ) : (
                  <div className="rounded-md border">
                    <div className="bg-muted/40 flex items-center gap-3 border-b px-3 py-2">
                      <span className="text-muted-foreground min-w-0 flex-1 truncate text-xs font-medium">
                        {restricted
                          ? t("可用 {selected} / {total} · 可见 {visible}", {
                              selected: selected.length,
                              total: models.length,
                              visible: devSelected.length,
                            })
                          : t("全部 {total} 个可用 · 可见 {visible}", {
                              total: models.length,
                              visible: devSelected.length,
                            })}
                      </span>
                      {restricted ? (
                        <span className="flex shrink-0 gap-2">
                          <Button
                            type="button"
                            size="xs"
                            variant="outline"
                            onClick={() => setSelected(models.filter((m) => m.selectable).map((m) => m.id))}
                          >
                            {t("全选")}
                          </Button>
                          <Button
                            type="button"
                            size="xs"
                            variant="outline"
                            onClick={() => {
                              // 可见集合只能勾在可用集合里，清空可用连带清空可见。
                              setSelected([]);
                              setDevSelected([]);
                            }}
                          >
                            {t("清空")}
                          </Button>
                        </span>
                      ) : null}
                      <span className="text-muted-foreground w-24 shrink-0 text-xs font-medium">
                        {t("开发工具可见")}
                      </span>
                    </div>
                    {models.map((model) => {
                      const apiChecked = !restricted || selected.includes(model.id);
                      const devChecked = devSelected.includes(model.id);
                      // 曾勾选、后来失去兼容性的行保留可见（不然取消不掉），
                      // 一旦取消就不能再勾回。
                      const devEligible = model.dev_tools.length > 0 || model.dev_tool_selected;
                      const devDisabled = !apiChecked || (model.dev_tools.length === 0 && !devChecked);
                      return (
                        // 单行紧凑布局：目录会很长，说明性文字全部收进悬停提示。
                        <div key={model.id} className="flex items-center gap-3 border-b px-3 py-2 last:border-b-0">
                          {restricted ? (
                            <Checkbox
                              checked={selected.includes(model.id)}
                              disabled={!model.selectable}
                              aria-label={t("可用 — {model}", { model: model.name })}
                              onCheckedChange={(next) => toggleAvailable(model.id, next === true)}
                            />
                          ) : (
                            // 不限制模式下逐行画一枚只读对勾：全部可用，无需逐个勾。
                            <Check aria-hidden className="text-muted-foreground/70 size-4 shrink-0" />
                          )}
                          <span className="min-w-0 flex-1 truncate font-mono text-sm" title={model.name}>
                            {model.name}
                          </span>
                          <span className="flex shrink-0 items-center gap-1">
                            <Badge variant="outline">{api.kindLabel(model.kind)}</Badge>
                            {model.platforms.map((platform) => (
                              <Badge key={platform} variant="secondary">
                                {platform}
                              </Badge>
                            ))}
                            {model.servable ? null : (
                              <Badge
                                variant="outline"
                                className="text-muted-foreground"
                                title={model.reason ?? t("当前不可调用")}
                              >
                                {t("不可用")}
                              </Badge>
                            )}
                          </span>
                          <div className="w-24 shrink-0">
                            {devEligible ? (
                              <label
                                className={`flex items-center gap-2 ${devDisabled ? "" : "cursor-pointer"}`}
                                title={
                                  !apiChecked
                                    ? t("先勾选左侧的「可用」")
                                    : model.dev_tools.length > 0
                                      ? t("投影到：{tools}", {
                                          tools: model.dev_tools.map((tool) => api.devToolLabel(tool)).join(" / "),
                                        })
                                      : t("当前不兼容：{reason}", {
                                          reason:
                                            DEV_TOOL_REASON[model.dev_tool_reason ?? ""] ?? t("已失去开发工具兼容性"),
                                        })
                                }
                              >
                                <Checkbox
                                  checked={devChecked}
                                  disabled={devDisabled}
                                  aria-label={t("开发工具可见 — {model}", { model: model.name })}
                                  onCheckedChange={(next) => toggleDevTool(model.id, next === true)}
                                />
                                <span
                                  className={`text-xs font-semibold ${
                                    model.dev_tools.length > 0 ? "text-muted-foreground" : "text-destructive"
                                  }`}
                                >
                                  {t("可见")}
                                </span>
                              </label>
                            ) : (
                              <span className="text-muted-foreground text-xs" title={t("非开发工具模型（仅文本模型可投影）")}>
                                —
                              </span>
                            )}
                          </div>
                        </div>
                      );
                    })}
                  </div>
                )}
                <p className="text-muted-foreground text-xs leading-relaxed">
                  {t("「开发工具可见」的模型会按兼容性进入 Codex、Claude Code、OpenCode 的模型列表，悬停「可见」可查看该模型投影到的工具；Grok Build 与 Cursor 只使用各自订阅，不含目录模型。")}
                </p>
              </>
            )}
            {error === null ? null : (
              <p role="alert" className="text-destructive text-sm">
                {error}
              </p>
            )}
            <DialogFooter>
              <Button type="button" variant="outline" onClick={onClose}>{t("取消")}</Button>
              <Button type="submit" disabled={busy || data === null}>{t("保存")}</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      )}
    </Dialog>
  );
}

// ---- 限额对话框 ----

// 改完即时生效——准入读的是每次鉴权点查带回来的限额，没有缓存要失效。
function EditLimitsDialog({
  target,
  onClose,
  onSaved,
}: {
  target: api.ApiKey | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  // 用目标当前值初始化。调用方拿 target.id 做 key，换目标即重挂载，这几个
  // 初始值因此每次都是那把 Key 的现值。
  const [day, setDay] = useState(() => budgetValue(target?.budget_day_micro ?? null));
  const [week, setWeek] = useState(() => budgetValue(target?.budget_week_micro ?? null));
  const [month, setMonth] = useState(() => budgetValue(target?.budget_month_micro ?? null));
  const [rpm, setRpm] = useState(() => rpmValue(target?.rpm_limit ?? null));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (target === null) return;
    const d = readBudget(day, t("日预算"));
    if (!d.ok) return setError(d.msg);
    const w = readBudget(week, t("周预算"));
    if (!w.ok) return setError(w.msg);
    const m = readBudget(month, t("月预算"));
    if (!m.ok) return setError(m.msg);
    const r = readRPM(rpm, t("每分钟请求上限"));
    if (!r.ok) return setError(r.msg);

    // 限额三态：没变就不带这个字段，清空带 null，其余带数字。服务端 JSON 解码
    // 拒绝未知字段，也别把 GET 回来的对象整个回传。
    const patch: api.KeyPatch = {};
    if (d.value !== target.budget_day_micro) patch.budget_day_micro = d.value;
    if (w.value !== target.budget_week_micro) patch.budget_week_micro = w.value;
    if (m.value !== target.budget_month_micro) patch.budget_month_micro = m.value;
    if (r.value !== target.rpm_limit) patch.rpm_limit = r.value;
    if (Object.keys(patch).length === 0) {
      onClose();
      toast(t("未做任何修改"));
      return;
    }
    setError(null);
    setBusy(true);
    api.updateKey(target.id, patch).then(
      () => {
        setBusy(false);
        onClose();
        toast(t("Key {key} 的限额已更新", { key: keyDisplay(target) }));
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
      open={target !== null}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      {target === null ? null : (
        <DialogContent>
          <form className="flex flex-col gap-4" onSubmit={submit}>
            <DialogHeader>
              <DialogTitle>{t("限额 — {key}", { key: keyDisplay(target) })}</DialogTitle>
            </DialogHeader>
            <p className="text-muted-foreground text-xs">{budgetNote}</p>
            <div className="flex flex-col gap-3">
              {(
                [
                  [t("日预算"), t("元 / 自然日"), day, setDay],
                  [t("周预算"), t("元 / 自然周（周一起算）"), week, setWeek],
                  [t("月预算"), t("元 / 自然月"), month, setMonth],
                  [t("每分钟请求上限"), t("次 / 60 秒滑动窗口"), rpm, setRpm],
                ] as const
              ).map(([label, unit, value, set]) => (
                <div key={label} className="flex flex-col gap-1.5">
                  <Label>
                    {label}
                    <span className="text-muted-foreground ml-1 text-xs font-normal">{unit}</span>
                  </Label>
                  <LimitInput value={value} onChange={(e) => set(e.target.value)} />
                </div>
              ))}
            </div>
            <p className="text-muted-foreground text-xs">
              {t("检查序是 速率 → 日预算 → 周预算 → 月预算。速率超限立即拒绝；预算用完后有按量额度仍可继续使用。")}
            </p>
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
      )}
    </Dialog>
  );
}

// ---- 新建 Key ----

function CreateKeyDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (res: { key: api.ApiKey; plaintext: string }) => void;
}) {
  const [label, setLabel] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    setError(null);
    setBusy(true);
    api.createKey(label.trim()).then(
      (res) => {
        setBusy(false);
        setLabel("");
        onOpenChange(false);
        onCreated(res);
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("新建 API密钥")}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="new-key-label">{t("标签")}</Label>
            <Input
              id="new-key-label"
              value={label}
              maxLength={api.LabelMaxLen}
              placeholder={t("用途备注，可留空")}
              autoComplete="off"
              onChange={(e) => setLabel(e.target.value)}
            />
          </div>
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy}>
              {t("签发")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 编辑标签 ----

function EditLabelDialog({
  target,
  onClose,
  onSaved,
}: {
  target: api.ApiKey | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [label, setLabel] = useState(target?.label ?? "");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (target === null) return;
    const next = label.trim();
    if (next === target.label) {
      onClose();
      toast(t("标签未做修改"));
      return;
    }
    setError(null);
    setBusy(true);
    api.updateKey(target.id, { label: next }).then(
      () => {
        setBusy(false);
        onClose();
        toast(
          next === ""
            ? t("Key {key} 的标签已清除", { key: keyDisplay(target) })
            : t("Key {key} 的标签已更新", { key: keyDisplay(target) }),
        );
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
      open={target !== null}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      {target === null ? null : (
        <DialogContent>
          <form className="flex flex-col gap-4" onSubmit={submit}>
            <DialogHeader>
              <DialogTitle>{t("编辑标签 — {key}", { key: keyDisplay(target) })}</DialogTitle>
            </DialogHeader>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="edit-key-label">{t("标签")}</Label>
              <Input
                id="edit-key-label"
                value={label}
                autoFocus
                maxLength={api.LabelMaxLen}
                placeholder={t("用途备注，可留空")}
                autoComplete="off"
                onChange={(e) => setLabel(e.target.value)}
              />
              <p className="text-muted-foreground text-xs">
                {t("标签仅用于管理识别，不会改变 API 密钥或影响客户端调用。")}
              </p>
            </div>
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
      )}
    </Dialog>
  );
}

// ---- 按量额度对话框 ----

function AdjustAllowanceDialog({
  target,
  onClose,
  onSaved,
}: {
  target: api.ApiKey | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [mode, setMode] = useState<"add" | "subtract">("add");
  const [amount, setAmount] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (target === null) return;
    const micro = api.parseYuan(amount);
    if (micro === null || micro <= 0) {
      setError(
        t("调整金额须为大于 0 的数，最多 {maxDecimals} 位小数，且不超过 {max} 元", {
          maxDecimals: api.YuanInputMaxDecimals,
          max: api.fmtYuan(api.MaxPricingMicro),
        }),
      );
      return;
    }
    if (mode === "subtract" && micro > target.metered_allowance_micro) {
      setError(t("扣减金额不能超过当前剩余 {remaining}", { remaining: api.fmtMoney(target.metered_allowance_micro) }));
      return;
    }
    setError(null);
    setBusy(true);
    const delta = mode === "add" ? micro : -micro;
    api.adjustKeyMeteredAllowance(target.id, delta).then(
      (res) => {
        setBusy(false);
        onClose();
        const amounts = { amount: api.fmtMoney(micro), remaining: api.fmtMoney(res.key.metered_allowance_micro) };
        toast(
          mode === "add"
            ? t("已增加 {amount}，剩余 {remaining}", amounts)
            : t("已扣减 {amount}，剩余 {remaining}", amounts),
        );
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
      open={target !== null}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      {target === null ? null : (
        <DialogContent>
          <form className="flex flex-col gap-4" onSubmit={submit}>
            <DialogHeader>
              <DialogTitle>{t("按量额度 — {key}", { key: keyDisplay(target) })}</DialogTitle>
            </DialogHeader>
            <div className="bg-muted/50 rounded-md border p-3">
              <p className="text-muted-foreground text-xs">{t("当前剩余")}</p>
              <p className="mt-1 font-mono text-lg font-semibold tabular-nums">
                {api.fmtMoney(target.metered_allowance_micro)}
              </p>
            </div>
            <p className="text-muted-foreground text-xs leading-relaxed">
              {t("日、周或月预算用完后，新消费会继续从按量额度中扣减；额度用完后才拒绝新请求。按量额度不会自动恢复，也不能绕过每分钟请求上限。")}
            </p>
            <div className="grid grid-cols-2 gap-2">
              <Button type="button" variant={mode === "add" ? "default" : "outline"} onClick={() => setMode("add")}>
                {t("增加")}
              </Button>
              <Button
                type="button"
                variant={mode === "subtract" ? "default" : "outline"}
                onClick={() => setMode("subtract")}
              >
                {t("扣减")}
              </Button>
            </div>
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="allowance-amount">{t("调整金额（元）")}</Label>
              <Input
                id="allowance-amount"
                value={amount}
                autoFocus
                inputMode="decimal"
                autoComplete="off"
                spellCheck={false}
                placeholder={t("例如：50")}
                onChange={(e) => setAmount(e.target.value)}
              />
            </div>
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
                {mode === "add" ? t("确认增加") : t("确认扣减")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      )}
    </Dialog>
  );
}

// ---- 页面 ----

// 「消费 / 预算」列：有计量读数就画三窗仪表，没有（服务端没接计量，spend
// 整块缺席）就退回只显示额度的 LimitsCell——**绝不把缺席当 0 画进仪表**，
// 那会把「没接线」读成「没花钱」。
function SpendCell({ k }: { k: api.ApiKey }) {
  if (k.spend === undefined) {
    return (
      <LimitsCell
        day={k.budget_day_micro}
        week={k.budget_week_micro}
        month={k.budget_month_micro}
      />
    );
  }
  return (
    <BudgetGauge
      windows={[
        { label: t("今日"), spentMicro: k.spend.day_micro, limitMicro: k.budget_day_micro },
        { label: t("本周"), spentMicro: k.spend.week_micro, limitMicro: k.budget_week_micro },
        { label: t("本月"), spentMicro: k.spend.month_micro, limitMicro: k.budget_month_micro },
      ]}
    />
  );
}

function KeysPageContent({ resource }: { resource: Resource<{ keys: api.ApiKey[] }> }) {
  const keys = resource.data?.keys ?? [];
  const reload = resource.reload;
  const confirm = useConfirm();
  const [creating, setCreating] = useState(false);
  const [labeling, setLabeling] = useState<api.ApiKey | null>(null);
  const [editing, setEditing] = useState<api.ApiKey | null>(null);
  const [adjusting, setAdjusting] = useState<api.ApiKey | null>(null);
  const [subsTarget, setSubsTarget] = useState<api.ApiKey | null>(null);
  const [modelsTarget, setModelsTarget] = useState<api.ApiKey | null>(null);
  const [plain, setPlain] = useState<KeyPlaintext | null>(null);
  // 分享降级窗的文本：剪贴板写得进去时恒为 null，写不进去才落到这里让人手动复制。
  const [shareText, setShareText] = useState<string | null>(null);

  async function toggleKey(k: api.ApiKey): Promise<void> {
    const disp = keyDisplay(k);
    if (!k.disabled) {
      // 可逆动作用缺省键，不染红。
      const ok = await confirm({
        title: t("禁用 Key"),
        body: (
          <p>
            {tx("确定禁用 Key <c>{key}</c>？数据面调用将立即 401。", {
              c: (chunk) => <code className="font-mono">{chunk}</code>,
              key: disp,
            })}
          </p>
        ),
        confirmText: t("确定禁用"),
      });
      if (!ok) return;
    }
    try {
      await api.setKeyDisabled(k.id, !k.disabled);
      toast(k.disabled ? t("Key {key} 已启用", { key: disp }) : t("Key {key} 已禁用", { key: disp }));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  // 复制：向服务端解封一次明文（记 key.reveal 审计），明文只活在对话框里。
  async function revealKey(k: api.ApiKey): Promise<void> {
    try {
      const res = await api.revealKey(k.id);
      setPlain({ key: k, plaintext: res.plaintext, title: `Key ${keyDisplay(k)}` });
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
  }

  // 分享：解封一次明文（同 revealKey，服务端记一条 key.reveal 审计），与「接入方法」
  // 页地址拼成一段发给使用者的文本直接进剪贴板。明文不落 state——只有浏览器拒写
  // 剪贴板时才退到降级窗，由那一窗持有到关闭为止。
  async function shareKey(k: api.ApiKey): Promise<void> {
    let text: string;
    try {
      const res = await api.revealKey(k.id);
      text = buildShareText(res.plaintext);
    } catch (err) {
      toast.error(api.errorMessage(err));
      return;
    }
    if (await copyText(text)) toast(t("接入信息已复制到剪贴板"));
    else setShareText(text);
  }

  // 删除不可撤销（封存明文随行删除，删掉即无从恢复）；想留档就用禁用。
  async function deleteKey(k: api.ApiKey): Promise<void> {
    const disp = keyDisplay(k);
    const ok = await confirm({
      title: t("删除 Key"),
      body: (
        <>
          <p>
            {tx("确定删除 Key <c>{key}</c>？", {
              c: (chunk) => <code className="font-mono">{chunk}</code>,
              key: disp,
            })}
          </p>
          <p className="text-destructive">{t("删除不可撤销，正在使用它的客户端会立即 401。")}</p>
          <p className="text-muted-foreground text-xs">{t("仅需暂停请改用「禁用」。")}</p>
        </>
      ),
      confirmText: t("确定删除"),
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteKey(k.id);
      toast(t("Key {key} 已删除", { key: disp }));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  return (
    <>
      <PageHeader
        title={t("API密钥")}
        note={t("签发和管理供局域网客户端调用模型的 API 密钥、预算、按量额度与速率限制。")}
        actions={
          <>
            {resource.data === null ? null : (
              <Badge variant="secondary" title={t("共 {n} 个 Key", { n: keys.length })}>
                {keys.length}
              </Badge>
            )}
            <Button size="sm" onClick={() => setCreating(true)}>
              {t("新建 Key")}
            </Button>
          </>
        }
        refreshing={resource.loading}
        onRefresh={reload}
      />
      <ResourceGate resource={resource}>
        {() => (
          <>
            <div className="bg-card w-full min-w-0 overflow-hidden rounded-lg border shadow-xs">
              {/* 操作列要并排放下「分享」「可用订阅」「可用模型」三颗按钮加更多菜单，
                  比其余页宽一档；表格总宽随之从 68rem 抬到 77rem。 */}
              <Table className="min-w-[77rem] table-fixed">
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-[13rem] pl-4">{t("Key / 标签")}</TableHead>
                    <TableHead className="w-44">{t("状态")}</TableHead>
                    <TableHead className="w-24">{t("速率")}</TableHead>
                    <TableHead className="w-[20rem]">{t("消费 / 预算")}</TableHead>
                    <TableHead className="w-36">{t("按量额度")}</TableHead>
                    <TableHead className="w-72 pr-4 text-right">{t("操作")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {keys.length === 0 ? (
                    <TableRow>
                      <TableCell colSpan={6} className="text-muted-foreground h-28 text-center">
                        {t("暂无 Key")}
                      </TableCell>
                    </TableRow>
                  ) : (
                    keys.map((k) => (
                      <TableRow key={k.id} className={k.disabled ? "opacity-60" : undefined}>
                        <TableCell className="pl-4">
                          <div className="min-w-0">
                            <div className="flex w-fit max-w-full items-center gap-1">
                              <code className="min-w-0 truncate font-mono text-xs" title={keyDisplay(k)}>
                                {keyDisplay(k)}
                              </code>
                              {/* 签发于旧固件的行没有封存明文，不给复制入口。 */}
                              {k.plaintext_available ? (
                                <Button
                                  size="icon-xs"
                                  variant="ghost"
                                  aria-label={t("复制 Key {key}", { key: keyDisplay(k) })}
                                  title={t("复制 Key")}
                                  onClick={() => void revealKey(k)}
                                >
                                  <Copy />
                                </Button>
                              ) : null}
                            </div>
                            <div className="mt-1 flex w-fit max-w-full items-center gap-1">
                              <span
                                className="text-muted-foreground min-w-0 truncate text-xs"
                                title={k.label === "" ? t("未填写标签") : k.label}
                              >
                                {k.label === "" ? t("未填写标签") : k.label}
                              </span>
                              <Button
                                size="icon-xs"
                                variant="ghost"
                                aria-label={t("编辑 Key {key} 的标签", { key: keyDisplay(k) })}
                                title={t("编辑标签")}
                                onClick={() => setLabeling(k)}
                              >
                                <Pencil />
                              </Button>
                            </div>
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="text-xs">
                            <StatusBadge disabled={k.disabled} />
                            <div className="mt-1.5">
                              <span className="text-muted-foreground">{t("最近")} </span>
                              {k.last_used_at === undefined ? t("从未使用") : fmtTime(k.last_used_at)}
                            </div>
                            <div className="text-muted-foreground mt-1">
                              {t("创建 {time}", { time: fmtTime(k.created_at) })}
                            </div>
                          </div>
                        </TableCell>
                        <TableCell className="tabular-nums">
                          {k.rpm_limit === null ? (
                            <span className="text-muted-foreground">{t("不限")}</span>
                          ) : (
                            t("{n} 次/分", { n: k.rpm_limit })
                          )}
                        </TableCell>
                        <TableCell>
                          <div className="inline-flex max-w-full items-center gap-2 align-middle">
                            <div className="min-w-0">
                              <SpendCell k={k} />
                            </div>
                            <Button size="xs" variant="outline" onClick={() => setEditing(k)}>
                              {t("限额")}
                            </Button>
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className="inline-flex max-w-full items-center gap-2 align-middle">
                            <div className="min-w-0">
                              <div className="font-mono text-xs font-medium tabular-nums">
                                {api.fmtMoney(k.metered_allowance_micro)}
                              </div>
                              <div className="text-muted-foreground mt-1 text-[11px]">
                                {k.metered_allowance_micro > 0 ? t("预算用完后可用") : t("无剩余额度")}
                              </div>
                            </div>
                            <Button size="xs" variant="outline" onClick={() => setAdjusting(k)}>
                              {t("调整")}
                            </Button>
                          </div>
                        </TableCell>
                        <TableCell className="pr-4">
                          <div className="flex items-center justify-end gap-2">
                            {/* 分享：接入方法页地址 + 这把 Key 的明文一起进剪贴板。
                                没有封存明文的老行分享不出 Key，置灰并说明原因——
                                disabled 按钮自己吃不到 hover（disabled:pointer-events-none），
                                title 得挂在外层 span 上才显示得出来。 */}
                            {k.plaintext_available ? (
                              <Button
                                size="xs"
                                variant="outline"
                                aria-label={t("分享 Key {key} 的接入信息", { key: keyDisplay(k) })}
                                title={t("把接入方法地址与这把 Key 的明文复制到剪贴板")}
                                onClick={() => void shareKey(k)}
                              >
                                {t("分享")}
                              </Button>
                            ) : (
                              <span
                                className="inline-flex"
                                title={t("这把 Key 签发于旧固件，没有封存明文，无法分享")}
                              >
                                <Button size="xs" variant="outline" disabled>
                                  {t("分享")}
                                </Button>
                              </span>
                            )}
                            <Button size="xs" variant="outline" onClick={() => setSubsTarget(k)}>
                              {t("可用订阅")}
                            </Button>
                            <Button size="xs" variant="outline" onClick={() => setModelsTarget(k)}>
                              {t("可用模型")}
                            </Button>
                            <RowActionsMenu
                              label={t("Key {key} 的更多操作", { key: keyDisplay(k) })}
                              actions={[
                                { label: k.disabled ? t("启用") : t("禁用"), onSelect: () => void toggleKey(k) },
                                { label: t("删除"), destructive: true, onSelect: () => void deleteKey(k) },
                              ]}
                            />
                          </div>
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>
            </div>

            <CreateKeyDialog open={creating} onOpenChange={setCreating} onCreated={(res) => setPlain(res)} />
            <EditLabelDialog
              key={labeling?.id ?? "none"}
              target={labeling}
              onClose={() => setLabeling(null)}
              onSaved={reload}
            />
            {/* key=id 让换目标时重挂载，输入框跟着重填成那把 Key 的当前限额。 */}
            <EditLimitsDialog
              key={editing?.id ?? "none"}
              target={editing}
              onClose={() => setEditing(null)}
              onSaved={reload}
            />
            <AdjustAllowanceDialog
              key={adjusting?.id ?? "none"}
              target={adjusting}
              onClose={() => setAdjusting(null)}
              onSaved={reload}
            />
            <SubscriptionsDialog target={subsTarget} onClose={() => setSubsTarget(null)} />
            <ModelsDialog target={modelsTarget} onClose={() => setModelsTarget(null)} />
            <KeyPlaintextDialog
              value={plain}
              onClose={() => {
                setPlain(null);
                reload();
              }}
            />
            {/* 分享不改任何行数据，关窗不必重取列表。 */}
            <KeyShareDialog value={shareText} onClose={() => setShareText(null)} />
          </>
        )}
      </ResourceGate>
    </>
  );
}

export function KeysPage(): React.ReactElement {
  const res = useResource(() => api.listKeys(), []);
  return (
    <PageContainer wide>
      <KeysPageContent resource={res} />
    </PageContainer>
  );
}
