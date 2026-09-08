// 贯通整幅的顶栏：设备面板那一条。左端品牌与侧栏开关，中段是整机读数
// （设备型号 / 固件版本 / API按量 / API订阅 / API私有 / 开发工具订阅），右端是语言开关、
// 设备名菜单（修改设备名 / 修改管理密码）、独立配色圆点与退出按钮。
//
// 语言开关是右端**独立的一个钮**（components/lang-switch.tsx），不收进账户菜单：
// 看不懂当前语言的人得先能一眼认出它。
//
// 设备名是持久化的本地显示名称，随接入快照读取；修改成功后直接更新顶栏与浏览器标题。
//
// 它铺满整幅、压在侧栏之上（侧栏由 `--sidebar-top` 让出这一条，见
// components/ui/sidebar.tsx 的两处覆写），所以品牌只在这里出现一次，侧栏里不再
// 另放一份。底纹与配色在 styles/globals.css 的 `.app-topbar` 一节。
//
// **取数纪律**：这一条是整个外壳唯一的请求——挂载时读一次 GET /admin/v1/endpoints，
// 之后不再动。零轮询是产品约定，别在这里加定时或聚焦重取（lib/use-resource.ts
// 开头写了缘由）。固件版本也从这一份快照里读，不另发一次请求。

import { ChevronDown, KeyRound, LogOut, Pencil } from "lucide-react";
import { useEffect, useState } from "react";
import { toast } from "sonner";

import logoUrl from "@/assets/logo.svg";
import { LangSwitch } from "@/components/lang-switch";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { DeviceNameDialog } from "@/features/layout/device-name-dialog";
import * as api from "@/lib/api";
import * as brand from "@/lib/brand";
import { cn } from "@/lib/cn";
import { t } from "@/lib/i18n";
import { useSession } from "@/lib/session";
import { THEMES, useTheme, type ThemeId } from "@/lib/theme";
import { useResource, type Resource } from "@/lib/use-resource";

// 读数灯沿用登录页规格铭牌的 LED 语义：绿 = 正常、琥珀 = 需留意、灰 = 还没有读数。
// **颜色从不单独承担语义**——每盏灯旁边都写着那句话本身。
type Tone = "ok" | "alert" | "idle";

function Dot({ tone }: { tone: Tone }) {
  return (
    <span
      aria-hidden
      className={cn(
        "inline-block size-1.5 shrink-0 rounded-full",
        tone === "ok" ? "bg-signal-ok" : tone === "alert" ? "bg-signal-alert" : "bg-white/35",
      )}
    />
  );
}

// 一格读数。`h-7` 与左端的 SidebarTrigger（size-7）同高，divide 出来的竖线因此
// 和开关按钮上下对齐，不是一段浮在中间、跟谁都不搭的短线。
//
// 顶栏**只有一个字号**：标签与读数一律 13px + `leading-4`，主次靠颜色
// 分（标签 white/55、读数 white/90），不靠字号。字号一混，两个 span 的盒高就差半
// 像素，`items-center` 一居中便是肉眼可见的一高一低。读数也不再用等宽字族——同字号
// 下等宽字面比比例字明显小一圈，一条里两种字族看着就是"没对齐"；数字要成列改用
// `tabular-nums`：字族不变，只把数字宽度钉齐。
function Cell({
  label,
  title,
  className,
  children,
}: {
  label?: string;
  title?: string;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div className={cn("flex h-7 shrink-0 items-center gap-2 px-3 whitespace-nowrap", className)} title={title}>
      {label ? <span className="text-[13px] leading-4 text-white/55">{label}</span> : null}
      <span className="flex items-center gap-3 text-[13px] leading-4 tabular-nums text-white/90">
        {children}
      </span>
    </div>
  );
}

type Reading = { tone: Tone; text: string; description: string; detail: string };

// 顶栏只显示模型数；数量单位与模型种类放到无障碍文本和悬停说明里。
function cloudReading({ text, aigc }: api.APIModelCount): Reading {
  const detail =
    aigc === 0
      ? t("文本模型 {text} 个", { text })
      : t("文本模型 {text} 个 · 视频/图像模型 {aigc} 个", { text, aigc });
  if (text + aigc === 0) {
    return { tone: "alert", text: "0", description: t("未接入"), detail: t("还没有任何可调用的模型") };
  }
  return { tone: "ok", text: String(text + aigc), description: t("{n} 个模型", { n: text + aigc }), detail };
}

function CompactReading({ label, reading }: { label: string; reading: Reading }) {
  return (
    <span className="flex items-center gap-1.5" title={reading.detail}>
      <span className="text-white/55">{label}</span>
      <Dot tone={reading.tone} />
      <span aria-hidden>{reading.text}</span>
      <span className="sr-only">{reading.description}</span>
    </span>
  );
}

function Readings({ access }: { access: Resource<api.AccessSnapshot> }) {
  const data = access.data;

  const pending: Reading =
    access.error !== null || data !== null
      ? { tone: "alert", text: "!", description: t("读不到"), detail: access.error || t("读不到") }
      : { tone: "idle", text: "…", description: t("读取中…"), detail: t("读取中…") };
  const counts = data?.api_model_counts;
  const usage = counts ? cloudReading(counts.usage) : pending;
  const subscription = counts ? cloudReading(counts.subscription) : pending;

  // 固件版本是固件唯一的版本名称，与云平台读数同一份快照。取不到时照云平台
  // 那一格的口径给字，不留空格子。
  const fwVersion =
    data !== null && data.firmware_version !== ""
      ? data.firmware_version
      : access.error !== null
        ? t("读不到")
        : t("读取中…");

  const agents = data?.agents ?? [];
  const live = agents.filter((a) => a.available).length;
  const agentsDetail =
    agents.length === 0
      ? ""
      : agents
          .map((a) =>
            t("{name}：{state}", {
              name: api.agentProviderLabel(a.provider),
              state: a.available ? t("可用") : t("未开通"),
            }),
          )
          .join(" · ");

  return (
    <>
      {data === null || data.hardware_model === "" ? null : (
        <Cell title={t("设备型号")} className="hidden xl:flex">{data.hardware_model}</Cell>
      )}
      <Cell title={t("固件版本")} className="hidden xl:flex">{fwVersion}</Cell>
      <Cell label="API">
        <CompactReading label={t("按量")} reading={usage} />
        <CompactReading label={t("订阅")} reading={subscription} />
        <CompactReading label={t("私有")} reading={{
          tone: "idle", text: "—", description: t("未接入"),
          detail: t("这一页预留给私有化部署的模型服务，当前尚未开放。"),
        }} />
      </Cell>
      <Cell title={t("开发工具订阅")}>
        <CompactReading label={t("工具订阅")} reading={data === null ? pending : {
          tone: live === 0 ? "alert" : "ok", text: `${live}/${agents.length}`,
          description: t("可用订阅 {available}/{total}", { available: live, total: agents.length }),
          detail: agentsDetail,
        }} />
      </Cell>
    </>
  );
}

// 改密对话框：验旧口令 → 服务端清空全部旧会话并为本浏览器轮换新会话，
// 不需要重新登录。
function PasswordDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [oldPw, setOldPw] = useState("");
  const [newPw, setNewPw] = useState("");
  const [confirmPw, setConfirmPw] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function reset(): void {
    setOldPw("");
    setNewPw("");
    setConfirmPw("");
    setError(null);
  }

  function submit(ev: React.FormEvent): void {
    ev.preventDefault();
    if (newPw !== confirmPw) {
      setError(t("两次输入的新密码不一致"));
      return;
    }
    setError(null);
    setBusy(true);
    api.changePassword(oldPw, newPw).then(
      () => {
        setBusy(false);
        reset();
        onOpenChange(false);
        toast(t("密码已修改，其他登录会话已全部失效"));
      },
      (err: unknown) => {
        setBusy(false);
        setError(api.errorMessage(err));
      },
    );
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next) reset();
        onOpenChange(next);
      }}
    >
      <DialogContent>
        <form className="flex flex-col gap-4" onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("修改管理密码")}</DialogTitle>
          </DialogHeader>
          <p className="text-muted-foreground text-xs">
            {t("这是登录这台设备管理界面的唯一口令。修改后，除当前浏览器外的全部登录会话立即失效。")}
          </p>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="old-pw">{t("旧密码")}</Label>
            <Input
              id="old-pw"
              type="password"
              required
              value={oldPw}
              autoComplete="current-password"
              onChange={(e) => setOldPw(e.target.value)}
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="new-pw">
              {t("新密码（{min}–{max} 个字符）", { min: api.PasswordMinLen, max: api.PasswordMaxLen })}
            </Label>
            <Input
              id="new-pw"
              type="password"
              required
              value={newPw}
              minLength={api.PasswordMinLen}
              maxLength={api.PasswordMaxLen}
              autoComplete="new-password"
              onChange={(e) => setNewPw(e.target.value)}
            />
          </div>
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="confirm-pw">{t("确认新密码")}</Label>
            <Input
              id="confirm-pw"
              type="password"
              required
              value={confirmPw}
              autoComplete="new-password"
              onChange={(e) => setConfirmPw(e.target.value)}
            />
          </div>
          {error === null ? null : (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="submit" disabled={busy}>
              {t("修改管理密码")}
            </Button>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              {t("取消")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function TopBar() {
  const { authed, logout } = useSession();
  const access = useResource(() => api.getEndpoints(), []);
  const [pwOpen, setPwOpen] = useState(false);
  const [nameOpen, setNameOpen] = useState(false);
  const [savedName, setSavedName] = useState<string | null>(null);
  const deviceName = savedName ?? access.data?.device_name ?? "";

  useEffect(() => {
    document.title = deviceName ? `${brand.productName} - ${deviceName}` : brand.productName;
    return () => {
      document.title = brand.productName;
    };
  }, [deviceName]);

  // 首帧的真值由 index.html 的内联脚本挂到 <html> 上；这里的 state 只喂菜单的
  // 选中点（lib/theme.ts 的 useTheme，与登录页那条铭牌上的入口共用一份）。
  const [theme, switchTheme] = useTheme();
  // `relative z-20`：侧栏那个容器是 `fixed z-10`，不抬一层的话顶栏的投影会被它盖住
  //（定位元素恒画在非定位元素之上）。
  return (
    <header className="app-topbar relative z-20 flex shrink-0 items-center gap-3 px-3">
      <SidebarTrigger className="text-white/70 hover:bg-white/10 hover:text-white" />
      <div className="flex shrink-0 items-center gap-2">
        <img src={logoUrl} alt="" className="size-7 shrink-0" />
        <span className="hidden text-[15px] font-semibold tracking-tight text-white/95 sm:inline">
          {brand.productName}
        </span>
      </div>
      {/* 读数条：窄屏整段收起，API 三类共享标题与分隔，右端操作钮始终可见。
          `border-l` 是**首格前面那条线**：divide-x 只画格与格之间，没有它时
          第一格左边空一段、后面每格都带线，整条读数的节奏是歪的。补上之后
          每格都是「线 + 12px + 内容 + 12px」，一路等距。 */}
      <div className="hidden h-7 min-w-0 items-center divide-x divide-white/10 overflow-x-auto border-l border-white/10 [scrollbar-width:none] lg:flex">
        <Readings access={access} />
      </div>
      {/* 操作钮与读数同高；窄屏优先缩略设备名，为配色和退出留出空间。 */}
      <div className="ml-auto flex h-7 min-w-0 items-center gap-0.5">
        <LangSwitch className="text-white/70 hover:bg-white/10 hover:text-white" />
        {!authed ? null : <>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" title={deviceName || t("设备名")}
                aria-label={t("设备名：{name}", { name: deviceName || t("设备") })}
                className="h-7 min-w-0 shrink gap-1 px-2 text-[13px] text-white/90 hover:bg-white/10 hover:text-white">
                <span className="max-w-24 truncate">{deviceName || t("设备")}</span>
                <ChevronDown className="size-3 shrink-0" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuItem onSelect={() => setNameOpen(true)}><Pencil />{t("修改设备名")}</DropdownMenuItem>
              <DropdownMenuItem onSelect={() => setPwOpen(true)}><KeyRound />{t("修改管理密码")}</DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button variant="ghost" size="icon" title={t("界面配色")} aria-label={t("界面配色")}
                className="size-7 text-white/70 hover:bg-white/10 hover:text-white">
                <span aria-hidden className="size-3 rounded-full border border-white/60"
                  style={{ backgroundColor: THEMES.find((option) => option.id === theme)?.swatch }} />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              <DropdownMenuRadioGroup value={theme} onValueChange={(value) => switchTheme(value as ThemeId)}>
                {THEMES.map((option) => <DropdownMenuRadioItem key={option.id} value={option.id}>
                  <span aria-hidden className="border-foreground/20 size-2.5 shrink-0 rounded-full border"
                    style={{ backgroundColor: option.swatch }} />
                  {option.label}
                </DropdownMenuRadioItem>)}
              </DropdownMenuRadioGroup>
            </DropdownMenuContent>
          </DropdownMenu>
          <Button variant="ghost" onClick={logout} title={t("退出登录")}
            className="h-7 gap-1 px-2 text-[13px] text-white/70 hover:bg-white/10 hover:text-white">
            <LogOut className="size-3.5" />{t("退出")}
          </Button>
        </>}
      </div>
      <PasswordDialog open={pwOpen} onOpenChange={setPwOpen} />
      {nameOpen ? <DeviceNameDialog name={deviceName} onClose={() => setNameOpen(false)} onSaved={setSavedName} /> : null}
    </header>
  );
}
