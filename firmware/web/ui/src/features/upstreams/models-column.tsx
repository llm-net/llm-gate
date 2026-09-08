// API按量计费与 API订阅套餐共用的紧凑模型列。名称/动作、目录价、协议面连续横排，
// 来源在宽屏按列对齐，窄屏自然换行；完整模型 ID 不依赖横向滚动。
// 模型按文本 / 视频 / 图像排序，分类计数集中在列头。搜索匹配模型名、上游账号与上游侧模型 ID。
// 订阅承载的文本计价行归 agent-models.tsx，归属判据在 domain.ts。
//
// 编辑模型、添加上游与按来源测试是可见动作；停用、删除与来源编辑在更多菜单里。
// 新模型从账号卡片上的「添加模型」录入。异常状态保留文字与处理入口，禁用读数压暗，
// 操作按钮保持可用。目录价明确展示计价单位，未定价与显式 0 元分开表达。

import { Boxes, Plus } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { PlatformIcon } from "@/components/brand-icon";
import { HelpTip } from "@/components/help-tip";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import * as api from "@/lib/api";
import { cn } from "@/lib/cn";
import { useConfirm } from "@/lib/confirm";
import { t } from "@/lib/i18n";

import { CatalogSearch } from "./card-parts";
import { agentPricingModel } from "./domain";
import { RowActionsMenu } from "./row-actions";

const modelKinds: api.ModelKind[] = ["text", "video", "image"];

// 取一个 AIGC 模型的协议面：以**建模时的声明**为准。空串 = 存量未声明的行，回退按
// 第一条能服务的来源推（全部来源必同族，服务端守卫保证）；连来源也没有时返回 null。
function aigcFamilyOf(m: api.Model): api.Protocol | null {
  if (m.family !== "") return m.family;
  for (const s of m.sources) {
    const [first] = s.protocols;
    if (first !== undefined) return first;
  }
  return null;
}

// 这个模型对客户端开放的入口路径（与服务端路由同表，单点定义在 lib/api 的
// vendorSurfaceSubmitPaths）。文本按 kind 定，AIGC 按**协议面**定——同一个 video
// 种类下方舟与 MiniMax 的官方路径完全不同，按 kind 给一份就必然对一半人是错的。
function entryPaths(m: api.Model): string[] {
  if (m.kind === "text") return api.TextProtocolSurfaces.map((surface) => `POST ${surface.path}`);
  const fam = aigcFamilyOf(m);
  return fam === null ? [] : api.vendorSurfaceSubmitPaths(fam);
}

function entryNote(m: api.Model): string | null {
  if (m.kind === "text") return null;
  switch (aigcFamilyOf(m)) {
    case "minimax_video":
      return t("MiniMax 视频协议面：把 SDK 的 base_url 换成「设备地址 + /minimax」，官方接口原样转发；提交拿厂商任务 ID，查询/取消按官方路径");
    case "ark_video":
      return t("火山方舟 视频协议面：把 SDK 的 base_url 换成「设备地址 + /ark」，官方接口原样转发；提交拿厂商任务 ID，查询/取消走 /ark/api/v3/contents/generations/tasks/{id}");
    case "ark_image":
      return t("火山方舟 图像协议面：把 SDK 的 base_url 换成「设备地址 + /ark」，官方接口原样转发，同步返回，图像地址 24 小时有效");
    default:
      return t("尚未挂上可服务的上游，协议面未定");
  }
}

function noEntryTitle(kind: api.ModelKind): string {
  switch (kind) {
    case "text":
      return t("该上游当前不服务任何协议面（mock 上游需配置 base_url）");
    case "video":
      return t("该上游类型不服务任何视频协议面——视频模型的上游必须服务它声明的协议面");
    case "image":
      return t("该上游类型不服务任何图像协议面——图像模型的上游必须服务它声明的协议面");
  }
}

const noTestTitle = t(
  "视频/图像模型的上游不支持连通性测试——对这类入口的最小探测就是真实提交一次付费生成任务",
);

function pricingOf(m: api.Model): api.Pricing {
  return m.pricing ?? {};
}

// ---- 展示件 ----

/**
 * 把一张价目表渲染成「档位名 + 金额」，单位只在不是「百万 token」时才写出来：按 token
 * 计价是文本目录价的缺省，写出来只是每档都重复一遍同一句话（订阅接入那张表同样不写）。
 * 「秒 / 张」这类非缺省单位必须留着——同一个 kind 下两族的计价形态不同。完整单位恒挂在
 * 每档自己的 title 上。
 */
export function PricingItems({ kind, pricing }: { kind: api.ModelKind; pricing: api.Pricing }) {
  return (
    <>
      {api
        .pricingFieldsFor(kind)
        .filter((f) => pricing[f.name] !== undefined)
        .map((f) => (
          <span key={f.name} className="inline-flex items-baseline gap-1" title={`${f.group} · ${f.label} /${f.unit}`}>
            <span className="text-muted-foreground">{f.label}</span>
            <b className="font-mono tabular-nums">{api.fmtMoney(pricing[f.name] ?? 0)}</b>
            {f.unit === api.UnitTokens ? null : <span className="text-muted-foreground">{`/${f.unit}`}</span>}
          </span>
        ))}
    </>
  );
}

// 「未定价」警示徽章：未定价的模型照常转发，但每一次调用都记 0 元，账上看不出这台
// 设备在替谁花钱——这是需要操作者处理的状态，用警示色而非熄灭色。头行没有价格读数时
// 它就是那个位置上唯一的话，所以后果与出路都挂在它的 title 上。
function PricedBadge({ m }: { m: api.Model }) {
  if (Object.keys(pricingOf(m)).length > 0) return null;
  return (
    <Badge
      variant="outline"
      className="text-signal-alert"
      title={t(
        "尚未录入目录价：该模型照常转发，但每次调用的金额都记 0 元，用量页上看不到它的花费——点「编辑」录入，或到「设备更新 → 数据升级」更新官方价",
      )}
    >
      {t("未定价")}
    </Badge>
  );
}

// 「协议面」读数：文本模型是三个协议面开关（关掉的压暗并标「已关闭」），AIGC 模型按
// 协议面给出名称，完整官方路径与说明收在旁边的 HelpTip 中。
function EntryValue({ m }: { m: api.Model }) {
  if (m.kind === "text") {
    const entries = api.TextProtocolSurfaces.map((surface) => ({
      on: m[surface.field], label: api.protocolLabel(surface.protocol), path: `POST ${surface.path}`,
    }));
    return (
      <>
        {entries.map((e) => (
          <span key={e.path} title={e.path} className={cn("inline-flex flex-wrap items-center gap-1 leading-5", !e.on && "text-muted-foreground")}>
            <span className="whitespace-nowrap">{e.label}</span>
            {e.on ? null : (
              <Badge variant="secondary" title={t("该协议面已关闭，调用会 404；「编辑」可重新开启")}>
                {t("已关闭")}
              </Badge>
            )}
          </span>
        ))}
      </>
    );
  }
  const family = aigcFamilyOf(m);
  return (
    <span className="leading-5" title={entryNote(m) ?? undefined}>
      {family === null ? t("尚未挂上可服务的上游，协议面未定") : api.protocolLabel(family)}
    </span>
  );
}

// 目录价横排；单位保留在读数旁，空档不显示，显式 0 仍是有效价格。
function ModelPrices({ m }: { m: api.Model }) {
  const pricing = pricingOf(m);
  const fields = api.pricingFieldsFor(m.kind).filter((f) => pricing[f.name] !== undefined);
  if (fields.length === 0) return null;
  const units = new Set(fields.map((f) => f.unit));
  const commonUnit = units.size === 1 ? fields[0]?.unit : undefined;
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-xs leading-5">
      <span className="text-muted-foreground">{t("目录价")}</span>
      <dl className="flex min-w-0 flex-wrap gap-x-3 gap-y-0.5">
        {fields.map((f) => (
          <div key={f.name} className="inline-flex min-w-0 flex-wrap items-baseline gap-x-1" title={`${f.group} · ${f.label} /${f.unit}`}>
            <dt className="text-muted-foreground">{f.label}</dt>
            <dd className="font-mono font-semibold tabular-nums [overflow-wrap:anywhere]">
              {api.fmtMoney(pricing[f.name] ?? 0)}
              {commonUnit === undefined ? <span className="text-muted-foreground font-sans font-normal">{`/${f.unit}`}</span> : null}
            </dd>
          </div>
        ))}
      </dl>
      {commonUnit === undefined ? null : <span className="text-muted-foreground">{t("元 / {unit}", { unit: commonUnit })}</span>}
    </div>
  );
}

// 来源的可服务入口徽标。徽标是来源的**能力**（与测试的探测清单同口径，无视启停位与
// 入口开关）；被模型入口开关关掉的入口压暗标出——能力还在、当下不服务，两个事实都要
// 看得见。
function ProtocolBadges({ model, src }: { model: api.Model; src: api.ModelSource }) {
  if (src.protocols.length === 0) {
    return (
      <Badge variant="outline" className="text-signal-alert" title={noEntryTitle(model.kind)}>
        {t("无可用协议面")}
      </Badge>
    );
  }
  return (
    <>
      {src.protocols.map((p) => {
        const off =
          model.kind === "text" &&
          ((p === "openai_chat" && !model.entry_openai) ||
            (p === "openai_responses" && !model.entry_responses) ||
            (p === "anthropic_messages" && !model.entry_anthropic));
        return (
          <span
            key={p}
            className={cn("inline-flex flex-wrap items-baseline gap-1", off && "text-muted-foreground")}
            title={off ? t("该协议面已在本模型上关闭（「编辑」可重新开启）") : undefined}
          >
            <span className="whitespace-nowrap">{api.protocolLabel(p)}</span>
            {off ? <span>{t("已关闭")}</span> : null}
          </span>
        );
      })}
    </>
  );
}

// ---- 对话框：编辑模型 ----

// 改模型名、协议面开关与目录价。同处一个对话框是有意的：三件事都是「这个模型对外
// 是什么」，而 PATCH 本来就一次收得下。
function EditModelDialog({
  target,
  onClose,
  onSaved,
}: {
  target: api.Model | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(target?.name ?? "");
  const [openai, setOpenai] = useState(target?.entry_openai ?? true);
  const [responses, setResponses] = useState(target?.entry_responses ?? true);
  const [anthropic, setAnthropic] = useState(target?.entry_anthropic ?? true);
  const [prices, setPrices] = useState<Record<string, string>>(() => {
    const cur = target === null ? {} : pricingOf(target);
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(cur)) out[k] = api.yuanText(v);
    return out;
  });
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (target === null) return <Dialog open={false} onOpenChange={() => undefined} />;
  const m = target;

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    // 收集价目：空 = 不配这一档；非法值当场拦下（金额禁浮点，走 parseYuan 的字符串拆分）。
    const pricing: api.Pricing = {};
    for (const f of api.pricingFieldsFor(m.kind)) {
      const text = (prices[f.name] ?? "").trim();
      if (text === "") continue;
      const micro = api.parseYuan(text);
      if (micro === null) {
        setError(
          t("「{label}」填得不对：请填 0 或正数，最多 {max} 位小数", { label: f.label, max: api.YuanInputMaxDecimals }),
        );
        return;
      }
      pricing[f.name] = micro;
    }
    const patch: api.ModelPatch = {};
    if (name.trim() !== m.name) patch.name = name.trim();
    if (m.kind === "text") {
      if (openai !== m.entry_openai) patch.entry_openai = openai;
      if (responses !== m.entry_responses) patch.entry_responses = responses;
      if (anthropic !== m.entry_anthropic) patch.entry_anthropic = anthropic;
    }
    // 价目整表提交：有档位就给表，一档不配就给 null（服务端语义：清空定价）。
    const before = JSON.stringify(pricingOf(m));
    const after = JSON.stringify(pricing);
    if (before !== after) patch.pricing = Object.keys(pricing).length === 0 ? null : pricing;
    if (Object.keys(patch).length === 0) {
      onClose();
      toast(t("未做任何修改"));
      return;
    }
    setError(null);
    setBusy(true);
    api.updateModel(m.id, patch).then(
      () => {
        setBusy(false);
        onClose();
        toast(t("模型「{name}」已更新", { name: m.name }));
        onSaved();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  let lastGroup = "";
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
            <DialogTitle>{t("编辑模型 — {name}", { name: m.name })}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="m-name">{t("模型名")}</Label>
            <Input
              id="m-name"
              autoFocus
              required
              value={name}
              maxLength={api.CatalogNameMaxLen}
              autoComplete="off"
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          {m.kind === "text" ? (
            <div className="flex flex-col gap-2">
              <Label>{t("协议面")}</Label>
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={openai} onCheckedChange={(c) => setOpenai(c === true)} />
                {`${api.protocolLabel("openai_chat")}（POST /v1/chat/completions）`}
              </label>
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={responses} onCheckedChange={(c) => setResponses(c === true)} />
                {`${api.protocolLabel("openai_responses")}（POST /v1/responses）`}
              </label>
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={anthropic} onCheckedChange={(c) => setAnthropic(c === true)} />
                {`${api.protocolLabel("anthropic_messages")}（POST /v1/messages）`}
              </label>
              <p className="text-muted-foreground text-xs">{t("关闭的协议面调用会返回 404；至少保留一个协议面，暂停全部调用请停用模型。")}</p>
            </div>
          ) : null}
          <div className="flex flex-col gap-2">
            {api.pricingFieldsFor(m.kind).map((f) => {
              const head = f.group !== lastGroup ? f.group : null;
              lastGroup = f.group;
              return (
                <div key={f.name} className="flex flex-col gap-1.5">
                  {head === null ? null : <h3 className="mt-1 text-sm font-medium">{head}</h3>}
                  <Label>
                    {f.label}
                    <span className="text-muted-foreground ml-1 text-xs font-normal">
                      {t("元 / {unit}", { unit: f.unit })}
                    </span>
                  </Label>
                  <Input
                    value={prices[f.name] ?? ""}
                    placeholder={t("留空 = 不配这一档")}
                    autoComplete="off"
                    spellCheck={false}
                    onChange={(e) => setPrices({ ...prices, [f.name]: e.target.value })}
                  />
                </div>
              );
            })}
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
    </Dialog>
  );
}

// ---- 对话框：添加 / 编辑上游 ----

function SourceDialog({
  model,
  src,
  upstreams,
  onClose,
  onSaved,
}: {
  model: api.Model | null;
  src: api.ModelSource | null;
  upstreams: api.Upstream[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const editing = src !== null;
  const [upstreamID, setUpstreamID] = useState(
    String(src?.upstream_id ?? upstreams.find((u) => !u.disabled)?.id ?? ""),
  );
  const [modelID, setModelID] = useState(src?.upstream_model_id ?? "");
  const [priority, setPriority] = useState(String(src?.priority ?? api.DefaultSourcePriority));
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (model === null) return <Dialog open={false} onOpenChange={() => undefined} />;
  const m = model;

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    const prio = Number(priority.trim());
    if (!Number.isSafeInteger(prio) || prio < 0) {
      setError(t("优先级须为非负整数（数值小者优先；同值时订阅先于用量）"));
      return;
    }
    setError(null);
    setBusy(true);
    const done = (): void => {
      setBusy(false);
      onClose();
      toast(editing ? t("来源已更新") : t("已为模型「{name}」添加上游", { name: m.name }));
      onSaved();
    };
    const fail = (err: unknown): void => {
      setBusy(false);
      setError(api.errorMessage(err));
    };
    if (editing && src !== null) {
      // 只带真正变了的字段。
      const patch: api.SourcePatch = {};
      if (modelID.trim() !== src.upstream_model_id) patch.upstream_model_id = modelID.trim();
      if (prio !== src.priority) patch.priority = prio;
      if (Object.keys(patch).length === 0) {
        setBusy(false);
        onClose();
        toast(t("未做任何修改"));
        return;
      }
      api.updateModelSource(src.id, patch).then(done, fail);
      return;
    }
    api.createModelSource(m.id, Number(upstreamID), modelID.trim(), prio).then(done, fail);
  }

  const candidates = upstreams.filter((u) => !u.disabled);
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
            <DialogTitle>
              {editing ? t("编辑上游 — {name}", { name: m.name }) : t("添加上游 — {name}", { name: m.name })}
            </DialogTitle>
          </DialogHeader>
          {editing ? (
            <p className="text-muted-foreground text-xs">
              {t("上游账号：{name}（不可更换，换账号请移除后重新添加）", { name: src?.upstream_name ?? "" })}
            </p>
          ) : (
            <div className="flex flex-col gap-1.5">
              <Label>{t("上游账号")}</Label>
              <Select value={upstreamID} onValueChange={setUpstreamID}>
                <SelectTrigger className="w-full">
                  <SelectValue placeholder={t("选择账号")} />
                </SelectTrigger>
                <SelectContent>
                  {candidates.map((u) => (
                    <SelectItem key={u.id} value={String(u.id)}>
                      {`${u.name}（${u.platform_label}${u.billing_mode === "none" ? "" : ` · ${api.billingModeLabel(u.billing_mode)}`}）`}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {candidates.length === 0 ? (
                <p className="text-muted-foreground text-xs">{t("没有可用账号：先在「API按量计费」或「API订阅套餐」页新建一个。")}</p>
              ) : null}
            </div>
          )}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="src-mid">{t("上游侧模型 ID")}</Label>
            <Input
              id="src-mid"
              value={modelID}
              maxLength={api.CatalogNameMaxLen}
              placeholder={t("与模型名相同时留空")}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setModelID(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">{t("上游那边的真实模型 ID；与模型名一致时留空即可。")}</p>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="src-prio">{t("优先级")}</Label>
            <Input
              id="src-prio"
              value={priority}
              autoComplete="off"
              spellCheck={false}
              onChange={(e) => setPriority(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              {t("数值小者优先；同值时订阅先于用量。订阅缺省 {sub}，用量缺省 {usage}。", {
                sub: api.SubscriptionSourcePriority,
                usage: api.UsageSourcePriority,
              })}
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
              {editing ? t("保存") : t("添加")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 对话框：来源连通性测试 ----

function fmtLatency(ms: number): string {
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(1)} s`;
}

// 服务端对该来源能服务的每个协议面各发一次最小探测请求（与 protocols 徽标同序）；
// results 为空表示该来源当前不服务任何协议面。
function TestDialog({
  target,
  onClose,
}: {
  target: { model: api.Model; sources: api.ModelSource[] } | null;
  onClose: () => void;
}) {
  const [reports, setReports] = useState<Map<number, api.SourceTestReport | string>>(new Map());
  const [running, setRunning] = useState(false);
  const [started, setStarted] = useState(false);

  function run(): void {
    if (target === null) return;
    setRunning(true);
    setStarted(true);
    const next = new Map<number, api.SourceTestReport | string>();
    void Promise.all(
      target.sources.map((s) =>
        api.testModelSource(s.id).then(
          (r) => next.set(s.id, r),
          (err: unknown) => next.set(s.id, api.errorMessage(err)),
        ),
      ),
    ).then(() => {
      setReports(next);
      setRunning(false);
    });
  }

  return (
    <Dialog
      open={target !== null}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      {target === null ? null : (
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t("测试上游 — {name}", { name: target.model.name })}</DialogTitle>
          </DialogHeader>
          <p className="text-muted-foreground text-xs">
            {t("按来源的协议面测试上游连通性。OpenAI Chat 与 OpenAI Responses 共用一次 Chat 上游探测；真实调用可能产生少量费用。")}
          </p>
          {target.sources.map((s) => {
            const rep = reports.get(s.id);
            return (
              <div key={s.id} className="flex flex-col gap-1 rounded-md border p-3">
                <div className="flex flex-wrap items-center gap-2 text-sm">
                  <b>{s.upstream_name}</b>
                  <span className="text-muted-foreground text-xs">
                    {s.upstream_platform_label}
                    {s.upstream_billing_mode === "none" ? "" : `（${api.billingModeLabel(s.upstream_billing_mode)}）`}
                  </span>
                </div>
                {rep === undefined ? (
                  <span className="text-muted-foreground text-xs">{running ? t("测试中…") : t("尚未测试")}</span>
                ) : typeof rep === "string" ? (
                  <span className="text-destructive text-xs">{rep}</span>
                ) : rep.results.length === 0 ? (
                  <span className="text-muted-foreground text-xs">{t("该来源当前不服务任何协议面。")}</span>
                ) : (
                  rep.results.map((r) => (
                    <div key={r.protocol} className="flex flex-wrap items-center gap-2 text-xs">
                      <Badge variant="outline" className={r.ok ? "text-signal-ok" : "text-signal-alert"}>
                        {r.ok ? t("通过") : t("失败")}
                      </Badge>
                      <span>{api.protocolLabel(r.protocol)}</span>
                      <span className="text-muted-foreground font-mono">
                        {`HTTP ${r.status} · ${fmtLatency(r.latency_ms)}`}
                      </span>
                      {r.message === "" ? null : <span className="text-muted-foreground">{r.message}</span>}
                    </div>
                  ))
                )}
              </div>
            );
          })}
          <DialogFooter>
            <Button variant="outline" onClick={onClose}>
              {t("关闭")}
            </Button>
            <Button disabled={running} onClick={run}>
              {running ? t("测试中…") : started ? t("重新测试") : t("开始测试")}
            </Button>
          </DialogFooter>
        </DialogContent>
      )}
    </Dialog>
  );
}

// ---- 来源行 ----

function SourceRow({
  model,
  src,
  onEdit,
  onTest,
  reload,
}: {
  model: api.Model;
  src: api.ModelSource;
  onEdit: () => void;
  onTest: () => void;
  reload: () => void;
}) {
  const confirm = useConfirm();

  async function toggle(): Promise<void> {
    try {
      await api.updateModelSource(src.id, { disabled: !src.disabled });
      toast(src.disabled ? t("来源已启用") : t("来源已禁用"));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  async function remove(): Promise<void> {
    const ok = await confirm({
      title: t("移除上游"),
      body: (
        <>
          <p>
            {t("确定把上游「{upstream}」从模型「{model}」上移除？", { upstream: src.upstream_name, model: model.name })}
          </p>
          <p className="text-muted-foreground text-xs">{t("上游账号本身不受影响，只是这个模型不再走它。")}</p>
        </>
      ),
      confirmText: t("确定移除"),
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteModelSource(src.id);
      toast(t("来源已移除"));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  return (
    <li className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-x-2 gap-y-0.5 px-2.5 py-1.5 text-xs leading-4 @2xl/model:grid-cols-[minmax(0,1fr)_minmax(0,1.15fr)_4.5rem_minmax(0,1.8fr)_auto]">
      <div className={cn("col-start-1 row-start-1 flex min-w-0 items-center gap-1.5", src.disabled && "opacity-60")}>
        <PlatformIcon id={src.upstream_catalog_id} type={src.upstream_type} label={src.upstream_platform_label} />
        <span
          className="min-w-0 font-medium [overflow-wrap:anywhere]"
          title={t("上游账号：{name}（{platform}）", {
            name: src.upstream_name,
            platform: src.upstream_billing_mode === "none"
              ? src.upstream_platform_label
              : `${src.upstream_platform_label} · ${api.billingModeLabel(src.upstream_billing_mode)}`,
          })}
        >
          {src.upstream_name}
        </span>
      </div>
      <div className={cn("col-start-1 row-start-2 min-w-0 @2xl/model:col-start-2 @2xl/model:row-start-1", src.disabled && "opacity-60")}>
        <span className="sr-only">{t("上游侧模型 ID")}</span>
        <code className="font-mono [overflow-wrap:anywhere]" title={t("上游侧模型 ID")}>
          {src.upstream_model_id || t("同模型名")}
        </code>
      </div>
      <dl className={cn("col-start-2 row-start-2 flex flex-wrap justify-end gap-x-1 @2xl/model:col-start-3 @2xl/model:row-start-1", src.disabled && "opacity-60")} title={t("优先级：数值小者优先；同值时订阅先于用量")}>
        <dt className="text-muted-foreground">{t("优先级")}</dt>
        <dd className="font-mono tabular-nums">{src.priority}</dd>
      </dl>
      <div className={cn("col-span-2 col-start-1 row-start-3 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5 @2xl/model:col-span-1 @2xl/model:col-start-4 @2xl/model:row-start-1", src.disabled && "opacity-60")}>
        <ProtocolBadges model={model} src={src} />
        {src.disabled ? <Badge variant="secondary">{t("已禁用")}</Badge> : null}
        {src.upstream_disabled ? (
          <Badge variant="secondary" title={t("该上游账号已被整体禁用（在它所在页的账号列表可重新启用），它名下的全部挂载都退出候选")}>
            {t("账号已禁用")}
          </Badge>
        ) : null}
        {src.upstream_egress_mode === "proxy" ? (
          <Badge variant="outline" title={t("该账号单独设为经代理出站；同一模型挂了直连来源时，切换来源的重试会经不同出口")}>
            {t("经代理")}
          </Badge>
        ) : src.upstream_egress_mode === "direct" ? (
          <Badge variant="outline" title={t("该账号单独设为直连出站，不跟随设备级「模型与订阅接口」的出口")}>
            {t("直连")}
          </Badge>
        ) : null}
      </div>
      <div className="col-start-2 row-start-1 flex shrink-0 justify-end gap-1 @2xl/model:col-start-5">
        <Button size="xs" variant="ghost" disabled={model.kind !== "text"} title={model.kind !== "text" ? noTestTitle : undefined} onClick={onTest}>
          {t("测试")}
        </Button>
        <RowActionsMenu
          label={t("上游 {name} 的更多操作", { name: src.upstream_name })}
          actions={[
            { label: t("编辑上游"), onSelect: onEdit },
            { label: src.disabled ? t("启用来源") : t("禁用来源"), onSelect: () => void toggle() },
            { label: t("移除上游"), destructive: true, onSelect: () => void remove() },
          ]}
        />
      </div>
    </li>
  );
}

// ---- 模型卡 ----

function ModelCard({
  m,
  onEditModel,
  onAddSource,
  onEditSource,
  onTest,
  reload,
}: {
  m: api.Model;
  onEditModel: () => void;
  onAddSource: () => void;
  onEditSource: (s: api.ModelSource) => void;
  onTest: (sources: api.ModelSource[]) => void;
  reload: () => void;
}) {
  const confirm = useConfirm();

  async function toggle(): Promise<void> {
    if (!m.disabled) {
      const ok = await confirm({
        title: t("停用模型"),
        body: (
          <>
            <p>{t("确定停用模型「{name}」？", { name: m.name })}</p>
            <p className="text-destructive">{t("客户端调用将立即 404，且不再出现在 /v1/models。")}</p>
          </>
        ),
        confirmText: t("确定停用"),
      });
      if (!ok) return;
    }
    try {
      await api.updateModel(m.id, { disabled: !m.disabled });
      toast(
        m.disabled ? t("模型「{name}」已启用", { name: m.name }) : t("模型「{name}」已停用", { name: m.name }),
      );
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  async function remove(): Promise<void> {
    const ok = await confirm({
      title: t("删除模型"),
      body: (
        <>
          <p>{t("确定删除模型「{name}」？", { name: m.name })}</p>
          {m.sources.length > 0 ? (
            <p className="text-destructive">
              {t("其挂载的 {n} 个上游将一并移除（上游账号本身不受影响），客户端调用该模型立即 404。", {
                n: m.sources.length,
              })}
            </p>
          ) : null}
        </>
      ),
      confirmText: t("确定删除"),
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteModel(m.id);
      toast(t("模型「{name}」已删除", { name: m.name }));
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  // AIGC 模型的「测试上游」不进菜单（禁用的菜单项悬停不出提示，解释留给来源行上
  // 那颗禁用的「测试」）；文本模型有来源才给。
  const menuActions = [
    ...(m.kind === "text" && m.sources.length > 0
      ? [{ label: t("测试上游"), onSelect: () => onTest(m.sources) }]
      : []),
    { label: m.disabled ? t("启用模型") : t("停用模型"), onSelect: () => void toggle() },
    { label: t("删除模型"), destructive: true, onSelect: () => void remove() },
  ];
  return (
    <article className="@container/model bg-card min-w-0 overflow-hidden rounded-md border">
      <div className="flex flex-col gap-1 px-2.5 py-2">
        <div className="flex flex-wrap items-start gap-x-2 gap-y-1">
          {/* 已停用只压暗读数，动作组留亮——「启用模型」就在那颗菜单里。 */}
          <div
            className={cn("flex min-w-0 flex-1 basis-full flex-wrap items-center gap-x-2 gap-y-1 @sm/model:basis-0", m.disabled && "opacity-60")}
          >
            {/* 模型名可换行，窄屏仍能读到客户端需要的完整字面量。 */}
            <h3 className="max-w-full min-w-0 font-mono text-[13px] leading-6 font-semibold [overflow-wrap:anywhere]">
              {m.name}
            </h3>
            {/* 种类徽章：文本是常态不标，视频/图像亮出来——它们走另外的入口。 */}
            {m.kind === "text" ? null : (
              <Badge
                variant="outline"
                title={t("{kind}模型：{note}", { kind: api.kindLabel(m.kind), note: entryNote(m) ?? "" })}
              >
                {api.kindLabel(m.kind)}
              </Badge>
            )}
            {m.disabled ? <Badge variant="secondary">{t("已禁用")}</Badge> : null}
            <PricedBadge m={m} />
            <span className="text-muted-foreground text-xs tabular-nums">{t("上游来源")} {m.sources.length}</span>
          </div>
          <div className="ml-auto flex shrink-0 gap-1">
            {/* 「编辑」外露：改价/改名/开关入口是这列最常用的写动作，「未定价」的
                出路文案也点名它。其余动作在菜单里。 */}
            <Button size="xs" variant="ghost" onClick={onAddSource}>
              <Plus aria-hidden="true" />{t("添加上游")}
            </Button>
            <Button size="xs" variant="outline" onClick={onEditModel}>
              {t("编辑")}
            </Button>
            <RowActionsMenu label={t("模型 {name} 的更多操作", { name: m.name })} actions={menuActions} />
          </div>
        </div>
        <div className={cn("flex flex-col gap-0.5", m.disabled && "opacity-60")}>
          <ModelPrices m={m} />
          <dl className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs leading-5">
            <dt className="text-muted-foreground flex shrink-0 items-center gap-1.5">
              {t("协议面")}
              <HelpTip label={t("查看协议面路径")}>
                <div className="grid gap-2">
                  {entryPaths(m).map((path) => <code key={path} className="block font-mono [overflow-wrap:anywhere]">{path}</code>)}
                  {entryNote(m) === null ? null : <p>{entryNote(m)}</p>}
                </div>
              </HelpTip>
            </dt>
            <dd className="flex min-w-0 flex-wrap gap-x-2 gap-y-0.5">
              <EntryValue m={m} />
            </dd>
          </dl>
        </div>
      </div>
      {/* 上游区：挂着的每个上游一行；没挂的把出路直接摆在那句话旁边。 */}
      <div className="border-t">
        {m.sources.length === 0 ? (
          <div className="flex flex-wrap items-center gap-1.5 px-2.5 py-1.5 text-xs leading-5">
            <Badge variant="outline" className="text-signal-alert">
              {t("无上游")}
            </Badge>
            <span className="text-muted-foreground">{t("请添加上游来源。")}</span>
            <HelpTip label={t("无上游")}>{t("没挂上游的模型不会出现在 /v1/models，调用一律 404——点「添加上游」挂一个。")}</HelpTip>
          </div>
        ) : (
          <ul className="divide-y" aria-label={t("上游来源")}>
            {m.sources.map((s) => (
              <SourceRow
                key={s.id}
                model={m}
                src={s}
                reload={reload}
                onEdit={() => onEditSource(s)}
                onTest={() => onTest([s])}
              />
            ))}
          </ul>
        )}
      </div>
    </article>
  );
}

// ---- 列 ----

// 渲染成 Fragment：列头与卡片列表是页面网格的两个直接子节点（两列的列头钉在同一网格
// 行上，卡片顶对齐），对话框都是 Portal、不占格。
export function ModelsColumn({
  all,
  upstreams,
  reload,
}: {
  all: api.Model[];
  upstreams: api.Upstream[];
  reload: () => void;
}): React.ReactElement {
  // 订阅承载的文本计价行归「开发工具订阅」页（agent-models.tsx），这里只剩 API 上游承载的；
  // 按量 / 套餐两页之间的分流由页面做完再传进来（pages/upstreams.tsx）。
  const models = all.filter((m) => !agentPricingModel(m));
  const [search, setSearch] = useState("");
  const query = search.trim().toLocaleLowerCase();
  const visible = models.filter((m) =>
    [m.name, ...m.sources.flatMap((s) => [s.upstream_name, s.upstream_model_id])]
      .some((value) => value.toLocaleLowerCase().includes(query)),
  );
  // 按种类排序；多种模型并存时，分类计数收在列头，不为每组单独占一行。
  const groups = modelKinds
    .map((k) => ({ kind: k, models: visible.filter((m) => m.kind === k) }))
    .filter((g) => g.models.length > 0);

  const [editModel, setEditModel] = useState<api.Model | null>(null);
  const [sourceDlg, setSourceDlg] = useState<{ model: api.Model; src: api.ModelSource | null } | null>(null);
  const [test, setTest] = useState<{ model: api.Model; sources: api.ModelSource[] } | null>(null);

  return (
    <>
      <div className="mt-2 flex min-w-0 flex-col gap-1.5 @3xl/upstreams:col-start-2 @3xl/upstreams:row-start-1 @3xl/upstreams:mt-0">
        <div className="flex min-h-6 flex-wrap items-center gap-1.5">
          <Boxes className="text-primary size-4" aria-hidden="true" />
          <h2 className="text-sm font-semibold">{t("模型")}</h2>
          <Badge variant="secondary" title={t("共 {n} 个模型", { n: models.length })}>
            {models.length}
          </Badge>
          <HelpTip label={t("模型与上游如何关联")}>
            {t("模型名即客户端请求里的 model 值，卡片下半是它挂着的上游。挂多个上游即可按优先级选路：高优先级上游不可用时自动切换，客户端无感。新模型在账号卡片上点「添加模型」录入。")}
          </HelpTip>
          {groups.length > 1 ? (
            <span className="text-muted-foreground ml-auto flex flex-wrap gap-x-3 text-xs tabular-nums">
              {groups.map((g) => <span key={g.kind}>{api.kindLabel(g.kind)} {g.models.length}</span>)}
            </span>
          ) : null}
        </div>
        <CatalogSearch value={search} onChange={setSearch} label={t("搜索模型或上游账号")} />
      </div>
      <div className="flex min-w-0 flex-col gap-1.5 @3xl/upstreams:col-start-2 @3xl/upstreams:row-start-2">
        {models.length === 0 ? (
          <div className="bg-card flex flex-col items-center gap-2 rounded-md border border-dashed p-4 text-center">
            <h3 className="text-sm font-semibold">{t("从账号添加模型")}</h3>
            <p className="text-muted-foreground max-w-sm text-xs leading-6">
              {t("在账号卡片上点击「添加模型」，即可配置模型和它的上游来源。")}
            </p>
          </div>
        ) : visible.length === 0 ? (
          <p role="status" className="text-muted-foreground rounded-md border border-dashed px-3 py-4 text-center text-xs">
            {t("没有匹配的模型，请尝试其他模型名或上游账号。")}
          </p>
        ) : (
          groups.flatMap((g) => g.models).map((m) => (
            <ModelCard
              key={m.id}
              m={m}
              reload={reload}
              onEditModel={() => setEditModel(m)}
              onAddSource={() => setSourceDlg({ model: m, src: null })}
              onEditSource={(src) => setSourceDlg({ model: m, src })}
              onTest={(sources) => setTest({ model: m, sources })}
            />
          ))
        )}
      </div>

      <EditModelDialog
        key={`em-${editModel?.id ?? "none"}`}
        target={editModel}
        onClose={() => setEditModel(null)}
        onSaved={reload}
      />
      <SourceDialog
        key={`sd-${sourceDlg?.model.id ?? "none"}-${sourceDlg?.src?.id ?? "new"}`}
        model={sourceDlg?.model ?? null}
        src={sourceDlg?.src ?? null}
        upstreams={upstreams}
        onClose={() => setSourceDlg(null)}
        onSaved={reload}
      />
      <TestDialog key={`td-${test?.model.id ?? "none"}`} target={test} onClose={() => setTest(null)} />
    </>
  );
}
