// 持静态上游 Key 的账号（upstreams 表）列，卡片竖排。「API按量计费」与「API订阅套餐」两页
// 共用它：页面按账号的 billing_mode 分页后把本页的账号传进来（domain.ts 的 apiBillingPage），
// 新建账号对话框按同一判据只列本页那一组平台。
//
// 账号按紧凑行展示：名称与菜单、平台/状态/Key、地址、模型数与动作。
// 状态始终有文字；凭证解不开时保留重新录入提示与编辑入口，详细原因可点开查看。
// 禁用只压暗读数，启用与其他操作继续可用。
//
// 用词（术语表「账号 words」）：账号侧动作一律说「账号」（新建账号/编辑账号/禁用账号/
// 删除账号），「上游」留给模型列的挂载动作（添加上游/移除上游）——合并后两套动作同处
// 一页，同词就分不清「建账号」与「给模型挂账号」。
//
// **凭证纪律**：api_key 明文只往请求里走一次，任何响应都只回 last4，界面上永远拿不到
// 也不缓存明文——「换 Key」是覆盖写，留空即不动。非 mock 上游的 last4 为空意味着密文
// 解不开（换过设备密钥）或凭证过短，数据面走到它会直接失败；本页是唯一能看出这个状态
// 的地方，故卡片直接亮琥珀。
//
// base_url 的不对称：mock 任意地址；minimax 在双站点白名单内换；openai_compat 任意地址
// 但**改址必须同请求重录 Key**（服务端 base_url_requires_key 强制并原子落库——封存的旧
// Key 绝不会被发往管理员没为它输入过 Key 的主机）；其余产品上游只有历史行可清空。

import { KeyRound, Plus, TriangleAlert } from "lucide-react";
import { useEffect, useState } from "react";
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

import { CatalogSearch, Pill } from "./card-parts";
import { apiBillingPage, type ApiBillingPage } from "./domain";
import { RowActionsMenu, type RowAction } from "./row-actions";

// minimaxIsIntl 判定 minimax 上游当前选的站点（base_url 空 = 国内缺省，显式国内值与空
// 等价）。
function minimaxIsIntl(baseURL: string): boolean {
  return baseURL === api.MinimaxSiteIntl;
}

function MinimaxSiteHint() {
  return (
    <p className="text-muted-foreground text-xs">
      {t("两个站点的账号与上游 Key 互相独立、余额不互通，请按这把 Key 的开户站点选择；选错站上游会回 401。")}
    </p>
  );
}

function MinimaxSiteSelect({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  // 选项值就是要提交的 base_url（空 = 国内缺省），白名单之外没有第三种值可选。
  return (
    <Select value={minimaxIsIntl(value) ? api.MinimaxSiteIntl : "cn"} onValueChange={(v) => onChange(v === "cn" ? "" : v)}>
      <SelectTrigger className="w-full">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value="cn">{t("国内站（api.minimaxi.com）")}</SelectItem>
        <SelectItem value={api.MinimaxSiteIntl}>{t("国际站（api.minimax.io）")}</SelectItem>
      </SelectContent>
    </Select>
  );
}

function BillingBadge({ mode }: { mode: api.BillingMode | "none" }) {
  if (mode === "none") return null;
  return (
    <Badge
      variant={mode === "subscription" ? "default" : "secondary"}
      title={
        mode === "subscription"
          ? t("订阅套餐：调度时默认优先（先用满已付费额度）")
          : t("按 token 计费：作为兜底通道")
      }
    >
      {api.billingModeLabel(mode)}
    </Badge>
  );
}

// 凭证解不开：非 mock 上游的空 last4 是故障信号（密文解不开——换过设备密钥——或凭证
// 过短），数据面走到它必然失败。mock 无凭证是正常状态。本页是唯一能看出这个状态的地方，
// 所以卡片上状态灯、琥珀提示条与事实带三处都点名它。
function keyBroken(u: api.Upstream): boolean {
  return u.api_key_last4 === "" && u.type !== "mock";
}

// 事实带「上游 Key」那一格的读数。
function keyValue(u: api.Upstream): React.ReactNode {
  if (u.api_key_last4 !== "") {
    return (
      <code className="font-mono" title={t("上游 Key 的末 4 位（明文永不回显）")}>
        {`····${u.api_key_last4}`}
      </code>
    );
  }
  if (u.type === "mock") return <span className="text-muted-foreground">{t("无需 Key")}</span>;
  return <span className="text-signal-alert">{t("需重新录入")}</span>;
}

// 上游地址：空 = 走内置端点表（特化平台的常态）；minimax 的地址是站点选择，按站点名
// 读出、完整地址挂在 title 上；openai_compat/mock 恒有地址。
function baseURLValue(u: api.Upstream): React.ReactNode {
  if (u.type === "minimax") {
    const intl = minimaxIsIntl(u.base_url);
    return <span title={intl ? api.MinimaxSiteIntl : api.MinimaxSiteCN}>{intl ? t("国际站") : t("国内站")}</span>;
  }
  if (u.base_url === "") return <span className="text-muted-foreground">{t("内置端点表")}</span>;
  return (
    <code className="font-mono" title={u.base_url}>
      {u.base_url}
    </code>
  );
}

// ---- 平台余额（特化能力） ----

// 金额只在这里出现一次：关掉弹框就没有了——**设备不入库、不记日志**，要最新数就再查
// 一次（金额是厂商侧账户的读数，与设备内的预算/按量额度词汇无关）。
function BalanceDialog({
  value,
  onClose,
}: {
  value: { u: api.Upstream; res: api.UpstreamBalance } | null;
  onClose: () => void;
}) {
  return (
    <Dialog
      open={value !== null}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      {value === null ? null : (
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("平台余额 — {name}", { name: value.u.name })}</DialogTitle>
          </DialogHeader>
          {value.res.ok ? (
            <>
              <div>
                {value.res.available ? (
                  <Badge variant="outline" className="text-signal-ok">
                    {t("余额可用")}
                  </Badge>
                ) : (
                  <Badge variant="outline" className="text-signal-alert" title={t("平台报告余额不足以继续调用，请前往平台充值")}>
                    {t("余额不足")}
                  </Badge>
                )}
              </div>
              {value.res.balances.map((b, i) => (
                <dl key={i} className="grid grid-cols-[5rem_1fr] gap-y-1 text-sm">
                  {b.currency === "" ? null : (
                    <>
                      <dt className="text-muted-foreground">{t("币种")}</dt>
                      <dd>
                        <code className="font-mono">{b.currency}</code>
                      </dd>
                    </>
                  )}
                  <dt className="text-muted-foreground">{t("总余额")}</dt>
                  <dd>
                    <b>{b.total === "" ? "—" : b.total}</b>
                  </dd>
                  {b.granted === "" ? null : (
                    <>
                      <dt className="text-muted-foreground">{t("其中赠金")}</dt>
                      <dd>{b.granted}</dd>
                    </>
                  )}
                  {b.topped_up === "" ? null : (
                    <>
                      <dt className="text-muted-foreground">{t("其中充值")}</dt>
                      <dd>{b.topped_up}</dd>
                    </>
                  )}
                </dl>
              ))}
              <p className="text-muted-foreground text-xs">
                {t(
                  "查询耗时 {ms} ms。金额由平台实时返回、仅本次展示——设备不入库、不记日志，要最新数就再查一次。",
                  { ms: value.res.latency_ms },
                )}
              </p>
            </>
          ) : (
            <>
              <p>
                {value.res.status !== 0
                  ? t("查询失败：平台返回 HTTP {status}", { status: value.res.status })
                  : t("查询失败：未收到平台响应")}
              </p>
              {value.res.message === "" ? null : (
                <p className="text-muted-foreground text-sm">{value.res.message}</p>
              )}
              <p className="text-muted-foreground text-xs">
                {t("常见原因：Key 无效或已被平台吊销、平台侧网络不可达。可先用「模型」列表的「测试」核对这把 Key。")}
              </p>
            </>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={onClose}>
              {t("关闭")}
            </Button>
          </DialogFooter>
        </DialogContent>
      )}
    </Dialog>
  );
}

// ---- 新建账号 ----

// 两步式：第一步「选择平台」——搜索框过滤 + 按计费方式分组的平台网格（订阅套餐 /
// 按量计费 / 通用兼容适配，组内保持目录顺序），平台多到一屏放不下时靠过滤而不是
// 长列表滚动；点中平台即进第二步，按所选平台渲染能力说明与录入参数，「换平台」可
// 退回重选。名称/Key/地址等输入值跨平台共享，切换时不丢。
//
// 平台网格只列本页那一组：按量页给「按量计费」与「通用兼容适配」，套餐页只给
// 「订阅套餐」——在哪一页新建的账号就落在哪一页，不会建完找不到。
function CreateUpstreamDialog({
  billing,
  open,
  onOpenChange,
  onDone,
}: {
  billing: ApiBillingPage;
  open: boolean;
  onOpenChange: (o: boolean) => void;
  onDone: () => void;
}) {
  const [platforms, setPlatforms] = useState<api.UpstreamPlatform[]>([]);
  const [pickedID, setPickedID] = useState("");
  // choosing 为真时显示第一步（平台网格）；每次打开对话框都从选平台开始。
  const [choosing, setChoosing] = useState(true);
  const [query, setQuery] = useState("");
  const [name, setName] = useState("");
  // 账户名跟随平台联动预填，操作者一旦手动改过就不再覆盖。
  const [nameAuto, setNameAuto] = useState(true);
  const [apiKey, setApiKey] = useState("");
  const [site, setSite] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const picked = platforms.find((p) => p.id === pickedID) ?? null;

  useEffect(() => {
    if (!open) return undefined;
    let active = true;
    setChoosing(true);
    setQuery("");
    api.listUpstreamPlatforms().then(
      (res) => {
        if (!active) return;
        setError(null);
        setPlatforms(res.platforms);
        setPickedID((current) => (res.platforms.some((p) => p.id === current) ? current : ""));
      },
      (err: unknown) => {
        if (active) setError(api.errorMessage(err));
      },
    );
    return () => {
      active = false;
      setApiKey("");
    };
  }, [open]);

  function pickPlatform(p: api.UpstreamPlatform): void {
    setPickedID(p.id);
    if (nameAuto) setName(p.suggested_name);
    setChoosing(false);
    setError(null);
  }

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (picked === null) {
      setError(t("平台数据尚未加载，请稍后重试"));
      return;
    }
    setError(null);
    setBusy(true);
    let base = "";
    if (picked.type === "minimax") base = site;
    if (picked.custom_base_url) base = baseURL.trim();
    api.createUpstream(name.trim(), picked.type, apiKey, base, picked.id).then(
      (res) => {
        setBusy(false);
        onOpenChange(false);
        toast(t("账号「{name}」已创建", { name: res.upstream.name }));
        onDone();
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  const kw = query.trim().toLowerCase();
  const visible =
    kw === ""
      ? platforms
      : platforms.filter(
          (p) =>
            p.vendor.toLowerCase().includes(kw) ||
            p.id.toLowerCase().includes(kw) ||
            p.suggested_name.toLowerCase().includes(kw),
        );
  // 组的语义与 BillingBadge 的提示同一口径：订阅默认优先、用量作兜底、无计费标记的
  // 通用兼容适配也按兜底处理。
  const groups = [
    { mode: "subscription", label: t("订阅套餐"), hint: t("已付费套餐，调度时默认优先、先用满") },
    { mode: "usage", label: t("按量计费"), hint: t("按 token 计费，作为兜底通道") },
    { mode: "none", label: t("通用兼容适配"), hint: t("目录之外的兼容服务，自填服务地址接入") },
  ]
    .filter((g) => apiBillingPage(g.mode as api.BillingMode | "none") === billing)
    .map((g) => ({ ...g, items: visible.filter((p) => p.billing_mode === g.mode) }))
    .filter((g) => g.items.length > 0);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className={choosing || picked === null ? "sm:max-w-3xl" : "sm:max-w-lg"}>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{billing === "plan" ? t("新建 API订阅套餐账号") : t("新建 API按量计费账号")}</DialogTitle>
          </DialogHeader>
          {choosing || picked === null ? (
            <>
              {/* 这段说明从账号列头搬进来：它讲的是「录账号时该知道什么」，日常进页每次
                  读一遍是噪音，而真需要它的时刻就是现在。 */}
              <p className="text-muted-foreground text-xs">
                {t(
                  "这类接入持上游平台签发的 API密钥。平台与模型能力来自设备当前生效的数据目录；固定端点平台在创建时保存端点快照，通用兼容适配由管理员填写地址。账号建好后可在卡片上添加模型。上游 Key 加密入库、任何界面都不回显明文。",
                )}
              </p>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="up-platform-filter">{t("选择平台（账号建后不可更换平台）")}</Label>
                <Input
                  id="up-platform-filter"
                  value={query}
                  placeholder={t("搜索平台名称…")}
                  autoComplete="off"
                  onChange={(e) => setQuery(e.target.value)}
                />
              </div>
              {platforms.length === 0 ? (
                <p className="text-muted-foreground text-sm">
                  {error === null ? t("正在读取平台数据…") : t("平台数据读取失败")}
                </p>
              ) : groups.length === 0 ? (
                <p className="text-muted-foreground text-sm">{t("没有匹配的平台，换个关键词试试")}</p>
              ) : (
                groups.map((g) => (
                  <div key={g.mode} className="flex flex-col gap-2">
                    <div className="flex flex-wrap items-baseline gap-x-2">
                      <p className="text-sm font-medium">{g.label}</p>
                      <p className="text-muted-foreground text-xs">{g.hint}</p>
                    </div>
                    <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
                      {g.items.map((p) => (
                        <button
                          key={p.id}
                          type="button"
                          title={p.vendor}
                          aria-pressed={p.id === pickedID}
                          onClick={() => pickPlatform(p)}
                          className={cn(
                            "flex min-w-0 items-center gap-2.5 rounded-lg border p-2 text-left text-sm",
                            "hover:bg-accent hover:text-accent-foreground",
                            p.id === pickedID && "border-primary",
                          )}
                        >
                          <PlatformIcon id={p.id} type={p.type} label={p.vendor} size="list" />
                          <span className="min-w-0 flex-1 truncate font-medium">{p.vendor}</span>
                        </button>
                      ))}
                    </div>
                  </div>
                ))
              )}
            </>
          ) : (
            <>
              <div className="flex flex-col gap-3">
                <div className="flex items-center gap-2.5">
                  <PlatformIcon id={picked.id} type={picked.type} label={picked.vendor} size="list" />
                  <span className="flex min-w-0 flex-1 flex-wrap items-center gap-2">
                    <b>{picked.vendor}</b>
                    <BillingBadge mode={picked.billing_mode} />
                  </span>
                  <Button type="button" variant="outline" size="sm" onClick={() => setChoosing(true)}>
                    {t("换平台")}
                  </Button>
                </div>
                {/* 这家平台的账号能服务的协议面：文本三面与厂商视频/图像面同一列徽标，
                    名字里已带厂商与模态（「火山方舟 视频」），不再分组。 */}
                {picked.protocols.length > 0 ? (
                  <div className="flex flex-wrap items-center gap-1.5 text-xs">
                    <span className="text-muted-foreground">{t("协议面")}</span>
                    {picked.protocols.map((protocol) => (
                      <Badge key={protocol} variant="outline">{api.protocolLabel(protocol)}</Badge>
                    ))}
                  </div>
                ) : null}
                <p className="text-muted-foreground text-xs">{picked.ability}</p>
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="up-name">{t("名称")}</Label>
                  <Input
                    id="up-name"
                    required
                    value={name}
                    maxLength={api.CatalogNameMaxLen}
                    autoComplete="off"
                    onChange={(e) => {
                      setNameAuto(false);
                      setName(e.target.value);
                    }}
                  />
                </div>
                {picked.type === "minimax" ? (
                  <div className="flex flex-col gap-1.5">
                    <Label>{t("站点")}</Label>
                    <MinimaxSiteSelect value={site} onChange={setSite} />
                    <MinimaxSiteHint />
                  </div>
                ) : null}
                {picked.custom_base_url ? (
                  <div className="flex flex-col gap-1.5">
                    <Label htmlFor="up-base">{t("服务地址（base_url）")}</Label>
                    <Input
                      id="up-base"
                      required
                      value={baseURL}
                      placeholder="https://api.example.com/v1"
                      autoComplete="off"
                      onChange={(e) => setBaseURL(e.target.value)}
                    />
                    <p className="text-muted-foreground text-xs">
                      {t("填兼容服务的端点根（通常以 /v1 结尾）。建成后修改地址必须同时重新输入 Key。")}
                    </p>
                  </div>
                ) : null}
                {picked.id !== picked.type && picked.base_url !== undefined ? (
                  <div className="flex flex-col gap-1.5">
                    <Label>{t("平台端点")}</Label>
                    <code className="font-mono text-xs break-all">{picked.base_url}</code>
                    <p className="text-muted-foreground text-xs">
                      {t("端点来自当前数据目录，创建时与平台身份一起快照；后续数据升级不会改动这条账号的目标地址。")}
                    </p>
                  </div>
                ) : null}
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor="up-key">{t("上游 Key")}</Label>
                  <Input
                    id="up-key"
                    type="password"
                    required
                    value={apiKey}
                    minLength={api.UpstreamKeyMinLen}
                    maxLength={api.UpstreamKeyMaxLen}
                    placeholder={t("上游平台签发的 Key")}
                    autoComplete="off"
                    onChange={(e) => setApiKey(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    {picked.custom_base_url
                      ? t("上游 Key 加密入库，之后只能覆盖、不能读回；服务无鉴权时可填任意占位串（至少 8 位）。")
                      : t("平台端点不可在账号中修改；上游 Key 加密入库，之后只能覆盖、不能读回。")}
                  </p>
                </div>
              </div>
              <p className="text-muted-foreground text-xs">
                {t(
                  "「订阅」为已付费套餐，挂给模型时默认优先级 100、先用满；「用量」默认 200、作兜底；无计费标记的通用兼容适配也按兜底优先级处理。",
                )}
              </p>
            </>
          )}
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              {t("取消")}
            </Button>
            {choosing || picked === null ? null : (
              <Button type="submit" disabled={busy}>
                {t("创建")}
              </Button>
            )}
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// ---- 编辑账号 ----

// 改名 + 换 Key 恒可用；地址按类型分五种形态（mock 可改、openai_compat 可改但须同时
// 重录 Key、minimax 在双站点白名单内换、历史行只能清空、其余产品上游无此项）。
function EditUpstreamDialog({
  target,
  onClose,
  onSaved,
}: {
  target: api.Upstream | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(target?.name ?? "");
  const [baseURL, setBaseURL] = useState(target?.base_url ?? "");
  const [site, setSite] = useState(target?.base_url ?? "");
  const [clearBase, setClearBase] = useState(false);
  const [apiKey, setApiKey] = useState("");
  const [egressMode, setEgressMode] = useState<api.EgressMode>(target?.egress_mode ?? "inherit");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (target === null) {
    return (
      <Dialog open={false} onOpenChange={() => undefined}>
        <DialogContent />
      </Dialog>
    );
  }
  const u = target;
  const isMock = u.type === "mock";
  const isMinimax = u.type === "minimax";
  const isCompatAdapter = u.type === "openai_compat" || u.type === "anthropic_compat";
  const isCompat = isCompatAdapter && u.catalog_id === u.type;
  const isCatalogFixed = isCompatAdapter && u.catalog_id !== u.type;

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    // 服务端拒绝未知字段，且只认「本次要改的字段」：逐项按需装配。
    const patch: api.UpstreamPatch = {};
    const newName = name.trim();
    if (newName !== u.name) patch.name = newName;
    if (isMock || isCompat) {
      const newBase = baseURL.trim();
      if (newBase !== u.base_url) patch.base_url = newBase;
    } else if (isMinimax) {
      // 按站点语义比较（空与显式国内值等价），换站才提交；提交值即选项值。
      if (minimaxIsIntl(site) !== minimaxIsIntl(u.base_url)) patch.base_url = site;
    } else if (clearBase && u.base_url !== "") {
      patch.base_url = "";
    }
    if (apiKey !== "") patch.api_key = apiKey;
    if (egressMode !== u.egress_mode) patch.egress_mode = egressMode;
    // openai_compat 改址须同请求带新 Key（服务端 base_url_requires_key 同规则，
    // 这里提前拦下省一次往返）。
    if (isCompat && patch.base_url !== undefined && patch.api_key === undefined) {
      setError(t("修改服务地址时必须同时重新输入上游 Key（旧 Key 不能被发往新地址）"));
      return;
    }
    if (Object.keys(patch).length === 0) {
      onClose();
      toast(t("未做任何修改"));
      return;
    }
    setError(null);
    setBusy(true);
    api.updateUpstream(u.id, patch).then(
      (res) => {
        setBusy(false);
        onClose();
        toast(
          patch.api_key === undefined
            ? t("账号「{name}」已更新", { name: res.upstream.name })
            : t("账号「{name}」已更新，新 Key 即时生效", { name: res.upstream.name }),
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
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
    >
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("编辑账号 — {name}", { name: u.name })}</DialogTitle>
          </DialogHeader>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="eu-name">{t("名称")}</Label>
            <Input
              id="eu-name"
              autoFocus
              required
              value={name}
              maxLength={api.CatalogNameMaxLen}
              autoComplete="off"
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <p className="text-muted-foreground text-xs">
            {t("平台：{platform}（不可修改，换平台请删除后重建）", {
              platform:
                u.billing_mode === "none"
                  ? u.platform_label
                  : `${u.platform_label}（${api.billingModeLabel(u.billing_mode)}）`,
            })}
          </p>
          {isMock ? (
            <div className="flex flex-col gap-1.5">
              <Label>{t("上游地址（base_url）")}</Label>
              <Input required value={baseURL} autoComplete="off" onChange={(e) => setBaseURL(e.target.value)} />
            </div>
          ) : isCompat ? (
            <div className="flex flex-col gap-1.5">
              <Label>{t("服务地址（base_url）")}</Label>
              <Input
                required
                value={baseURL}
                placeholder="https://api.example.com/v1"
                autoComplete="off"
                onChange={(e) => setBaseURL(e.target.value)}
              />
              <p className="text-muted-foreground text-xs">
                {t("修改地址必须同时在下方重新输入上游 Key：封存的旧 Key 不会被发往新地址（服务端强制、原子生效）。")}
              </p>
            </div>
          ) : isMinimax ? (
            <div className="flex flex-col gap-1.5">
              <Label>{t("站点")}</Label>
              <MinimaxSiteSelect value={site} onChange={setSite} />
              <MinimaxSiteHint />
            </div>
          ) : isCatalogFixed ? (
            <div className="flex flex-col gap-1.5">
              <Label>{t("平台端点（创建快照）")}</Label>
              <Input readOnly value={baseURL} autoComplete="off" />
              <p className="text-muted-foreground text-xs">{t("数据目录平台的端点不可修改；换地址请按目标平台重新创建账号。")}</p>
            </div>
          ) : u.base_url !== "" ? (
            <div className="flex flex-col gap-1.5">
              <Label>{t("上游地址（base_url）")}</Label>
              <Input readOnly value={baseURL} autoComplete="off" />
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={clearBase} onCheckedChange={(c) => setClearBase(c === true)} />
                {t("清空地址，改回内置端点表（产品上游不能改指向新地址）")}
              </label>
            </div>
          ) : (
            <p className="text-muted-foreground text-xs">{t("上游地址：内置端点表（特化平台不可配置）。")}</p>
          )}
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="eu-key">{t("更换上游 Key")}</Label>
            <Input
              id="eu-key"
              type="password"
              value={apiKey}
              minLength={api.UpstreamKeyMinLen}
              maxLength={api.UpstreamKeyMaxLen}
              placeholder={t("留空表示不修改")}
              autoComplete="off"
              onChange={(e) => setApiKey(e.target.value)}
            />
            <p className="text-muted-foreground text-xs">
              {t("上游 Key 不可读回，只能覆盖；留空即保持现有 Key 不变。")}
            </p>
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="eu-egress">{t("出站方式")}</Label>
            <Select value={egressMode} onValueChange={(v) => setEgressMode(v as api.EgressMode)}>
              <SelectTrigger id="eu-egress" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="inherit">{t("跟随设备设置（模型与订阅接口）")}</SelectItem>
                <SelectItem value="direct">{t("直连")}</SelectItem>
                <SelectItem value="proxy">{t("经代理")}</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-muted-foreground text-xs">
              {t("发往这个账号的请求走哪个出口；代理端点在「网络/域名/代理」页配置。经代理要求代理已配置，代理不可用时该账号的请求直接失败、不会改走直连。")}
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
    </Dialog>
  );
}

// ---- 账号卡片 ----

function modelCountOf(u: api.Upstream, models: api.Model[]): number {
  return models.filter((m) => m.sources.some((s) => s.upstream_id === u.id)).length;
}

// 状态灯三档：禁用 = 熄灭灰（管理动作，压过一切）；启用但凭证解不开 = 琥珀（需处理）；
// 其余 = 绿。
function AccountPill({ u }: { u: api.Upstream }): React.ReactElement {
  if (u.disabled) {
    return (
      <Pill dot="bg-muted-foreground/40" className="text-muted-foreground">
        {t("已禁用")}
      </Pill>
    );
  }
  if (keyBroken(u)) {
    return (
      <Pill dot="bg-signal-alert" className="text-signal-alert">
        {t("需重新录入")}
      </Pill>
    );
  }
  return <Pill dot="bg-signal-ok">{t("已启用")}</Pill>;
}

function UpstreamCard({
  u,
  modelCount,
  onEdit,
  onAddModels,
  onBalance,
  reload,
}: {
  u: api.Upstream;
  modelCount: number;
  onEdit: () => void;
  onAddModels: () => void;
  onBalance: () => Promise<void>;
  reload: () => void;
}): React.ReactElement {
  const confirm = useConfirm();
  const [balanceBusy, setBalanceBusy] = useState(false);
  const broken = keyBroken(u) && !u.disabled;

  async function toggle(): Promise<void> {
    if (!u.disabled) {
      const ok = await confirm({
        title: t("禁用账号"),
        body: (
          <>
            <p>{t("确定禁用账号「{name}」？", { name: u.name })}</p>
            <p className="text-destructive">{t("挂着它的模型将立即失去这条上游候选，只挂了它一个上游的模型会 404。")}</p>
          </>
        ),
        confirmText: t("确定禁用"),
      });
      if (!ok) return;
    }
    try {
      await api.updateUpstream(u.id, { disabled: !u.disabled });
      toast(
        u.disabled ? t("账号「{name}」已启用", { name: u.name }) : t("账号「{name}」已禁用", { name: u.name }),
      );
    } catch (err) {
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  async function remove(): Promise<void> {
    const ok = await confirm({
      title: t("删除账号"),
      body: (
        <>
          <p>{t("确定删除账号「{name}」？", { name: u.name })}</p>
          <p className="text-destructive">{t("其上游 Key 一并销毁，无法恢复。")}</p>
        </>
      ),
      confirmText: t("确定删除"),
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteUpstream(u.id);
      toast(t("账号「{name}」已删除", { name: u.name }));
    } catch (err) {
      // 含 409 upstream_in_use：服务端消息已说明「先删来源或改挂其他上游」。
      toast.error(api.errorMessage(err));
    }
    reload();
  }

  // 已禁用时「启用」外露在动作行上，菜单里不再重复。
  const menu: RowAction[] = [{ label: t("编辑账号"), onSelect: onEdit }];
  if (!u.disabled) menu.push({ label: t("禁用账号"), onSelect: () => void toggle() });
  menu.push({ label: t("删除账号"), destructive: true, onSelect: () => void remove() });

  return (
    <article className={cn("bg-card min-w-0 rounded-md border px-2.5 py-2", broken && "border-signal-alert/60")}>
      <div className="flex min-w-0 items-center gap-1.5">
        <PlatformIcon id={u.catalog_id} type={u.type} label={u.platform_label} />
        <h3 className={cn("min-w-0 flex-1 truncate text-[13px] font-semibold leading-5", u.disabled && "opacity-60")} title={u.name}>
          {u.name}
        </h3>
        <RowActionsMenu label={t("账号 {name} 的更多操作", { name: u.name })} actions={menu} />
      </div>
      <div className={cn("mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs leading-4", u.disabled && "opacity-60")}>
        <span className="text-muted-foreground">{u.platform_label}</span>
        <BillingBadge mode={u.billing_mode} />
        <AccountPill u={u} />
        {broken ? (
          <HelpTip label={t("上游 Key")}>{t("上游 Key 的密文无法解开（设备密钥已更换）或 Key 过短，请编辑该上游重新录入")}</HelpTip>
        ) : (
          <span className="inline-flex items-baseline gap-1">
            <span className="text-muted-foreground">Key</span>{keyValue(u)}
          </span>
        )}
      </div>
      <dl className={cn("mt-1 flex min-w-0 items-baseline gap-2 text-xs leading-4", u.disabled && "opacity-60")}>
        <dt className="text-muted-foreground shrink-0">{u.type === "minimax" ? t("站点") : t("上游地址")}</dt>
        <dd className="min-w-0 [overflow-wrap:anywhere]">{baseURLValue(u)}</dd>
      </dl>
      <div className="mt-1.5 flex flex-wrap items-center gap-1.5">
        <span className="text-muted-foreground text-xs tabular-nums" title={t("这个账号承载 {n} 个模型（见「模型」列表）", { n: modelCount })}>
          {t("模型")} {modelCount}
        </span>
        {u.egress_mode === "inherit" ? null : (
          <span className="text-muted-foreground text-xs" title={t("该账号单独指定了出口，不跟随「网络/域名/代理」页的设备级设置")}>
            · {u.egress_mode === "proxy" ? t("经代理") : t("直连")}
          </span>
        )}
        <div className="ml-auto flex flex-wrap gap-1">
          {broken ? (
            <Button size="xs" variant="ghost" className="text-signal-alert" onClick={onEdit}>
              <TriangleAlert aria-hidden="true" />{t("编辑账号")}
            </Button>
          ) : null}
          {u.disabled ? (
            <Button size="xs" variant="ghost" onClick={() => void toggle()}>{t("启用")}</Button>
          ) : null}
          <Button size="xs" variant="outline" onClick={onAddModels}>
            <Plus aria-hidden="true" />{t("添加模型")}
          </Button>
          {/* 余额能力只信服务端的 balance_supported。 */}
          {u.balance_supported ? (
            <Button
              size="xs"
              variant="ghost"
              disabled={balanceBusy}
              onClick={() => {
                setBalanceBusy(true);
                void Promise.resolve(onBalance()).finally(() => setBalanceBusy(false));
              }}
            >
              {balanceBusy ? t("查询中…") : t("查询余额")}
            </Button>
          ) : null}
        </div>
      </div>
    </article>
  );
}

// ---- 账号列 ----

// 渲染成 Fragment：列头与卡片列表是页面网格的两个直接子节点（与订阅列同一套：两列的
// 列头钉在同一网格行上，卡片顶对齐），对话框都是 Portal、不占格。
export function ApiAccountsColumn({
  billing,
  upstreams,
  models,
  reload,
  onAddModels,
}: {
  billing: ApiBillingPage;
  upstreams: api.Upstream[];
  models: api.Model[];
  reload: () => void;
  onAddModels: (u: api.Upstream) => void;
}): React.ReactElement {
  const [creating, setCreating] = useState(false);
  const [search, setSearch] = useState("");
  const query = search.trim().toLocaleLowerCase();
  const visible = upstreams.filter((u) =>
    [u.name, u.platform_label, u.base_url].some((value) => value.toLocaleLowerCase().includes(query)),
  );
  const [editing, setEditing] = useState<api.Upstream | null>(null);
  const [balance, setBalance] = useState<{ u: api.Upstream; res: api.UpstreamBalance } | null>(null);

  async function queryBalance(u: api.Upstream): Promise<void> {
    try {
      const res = await api.queryUpstreamBalance(u.id);
      setBalance({ u, res });
    } catch (err) {
      // HTTP 层错误（不支持/凭证解不开/网络）走 toast，200 的成败都进结果弹框。
      toast.error(api.errorMessage(err));
    }
  }

  return (
    <>
      <div className="flex min-w-0 flex-col gap-1.5 @3xl/upstreams:col-start-1 @3xl/upstreams:row-start-1">
        <div className="flex min-h-6 flex-wrap items-center gap-1.5">
          <KeyRound className="text-primary size-4" aria-hidden="true" />
          <h2 className="text-sm font-semibold">{t("账号")}</h2>
          <Badge variant="secondary" title={t("共 {n} 个账号", { n: upstreams.length })}>
            {upstreams.length}
          </Badge>
          <HelpTip label={t("如何添加账号与模型")}>
            {billing === "plan"
              ? t("持上游平台 Key 的套餐账号，同一平台可以有多个；模型从账号卡片上的「添加模型」录入。")
              : t("录入平台 Key，再从账号卡片添加模型。")}
          </HelpTip>
          <Button size="xs" className="ml-auto" onClick={() => setCreating(true)}>
            <Plus aria-hidden="true" />
            {t("新建账号")}
          </Button>
        </div>
        <CatalogSearch value={search} onChange={setSearch} label={t("搜索账号或平台")} />
      </div>
      <div className="flex min-w-0 flex-col gap-1.5 @3xl/upstreams:col-start-1 @3xl/upstreams:row-start-2">
        {upstreams.length === 0 ? (
          <div className="bg-card flex flex-col items-center gap-2 rounded-md border border-dashed p-4 text-center">
            <h3 className="text-sm font-semibold">{t("添加第一个平台账号")}</h3>
            <p className="text-muted-foreground text-xs leading-6">
              {billing === "plan"
                ? t("暂无订阅套餐账号。先「新建账号」选一个套餐平台录入 Key，再在账号卡片上点「添加模型」——模型与它的上游一步建好。")
                : t("暂无按量计费账号。先「新建账号」录入平台 Key，再在账号卡片上点「添加模型」——模型与它的上游一步建好。")}
            </p>
            <Button size="sm" onClick={() => setCreating(true)}>
              <Plus aria-hidden="true" />{t("新建账号")}
            </Button>
          </div>
        ) : visible.length === 0 ? (
          <p role="status" className="text-muted-foreground rounded-md border border-dashed px-3 py-4 text-center text-xs">
            {t("没有匹配的账号，请尝试其他名称或平台。")}
          </p>
        ) : (
          visible.map((u) => (
            <UpstreamCard
              key={u.id}
              u={u}
              modelCount={modelCountOf(u, models)}
              reload={reload}
              onEdit={() => setEditing(u)}
              onAddModels={() => onAddModels(u)}
              onBalance={() => queryBalance(u)}
            />
          ))
        )}
      </div>

      <CreateUpstreamDialog billing={billing} open={creating} onOpenChange={setCreating} onDone={reload} />
      <EditUpstreamDialog
        key={`edit-up-${editing?.id ?? "none"}`}
        target={editing}
        onClose={() => setEditing(null)}
        onSaved={reload}
      />
      <BalanceDialog value={balance} onClose={() => setBalance(null)} />
    </>
  );
}
