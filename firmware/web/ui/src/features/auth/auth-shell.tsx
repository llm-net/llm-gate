// 设备正面：左列大字标，右列一块「通电的操作面板」。登录卡与启动失败卡共用
// 这张脸（对齐旧管理台 views/auth.ts 的 authShell）。
//
// 右列刻意做成设备本体的深色面板：石墨机身 + 机加工点阵，与外壳顶栏同一套
// 视觉语言；外缘一道缓慢巡回的能量光带（styles/globals.css 的 .auth-edge），
// 底部一条蚀刻铭牌。面板整块挂 `dark` 类，卡内 shadcn 组件（输入框、按钮、
// 错误文案）自动切到深色 token——发光的主按钮与浅色错误红都从这里来，
// 不必逐个组件改色。机身色与两档信号色跟着界面配色走（globals.css 的
// --auth-body / --auth-brand / --auth-neon），四种外观各是自己的一台设备。
//
// live=false 时隐藏头部的「在线」LED——启动检查失败的卡片不宣称设备正常。
// 不印网站地址、不印公司名，也**不印出厂缺省凭据**：印出来等于把它从「随机器
// 交付的一句话」变成「任何能打开这一页的人都看得见」。

import { LogIn } from "lucide-react";

import logoUrl from "@/assets/logo.svg";
import { LangSwitch } from "@/components/lang-switch";
import { Card } from "@/components/ui/card";
import * as api from "@/lib/api";
import * as brand from "@/lib/brand";
import { t } from "@/lib/i18n";
import { THEMES, useTheme } from "@/lib/theme";
import { useResource } from "@/lib/use-resource";

// 大字标拆成两行不是排版偶然：单行 LLM Gate 在左列里只能缩到很小，两行才撑得住
// 「一眼看见的大字」。字号、渐变与流光全在 styles/globals.css 的大字标一节，这里
// 只出结构。大小写按产品名原样，不做全大写。
//
// 读屏该把两行读成一句，所以整块挂 aria-label。Logo 是 .auth-wordmark-line 的
// 兄弟而不是子节点：那一层有 background-clip:text，会把 SVG 裁成透明。
function Wordmark() {
  return (
    <h1 aria-label={brand.productName} className="auth-wordmark select-none">
      <span className="auth-wordmark-gate">
        <span className="auth-wordmark-line">LLM</span>
        <img src={logoUrl} alt="" aria-hidden className="auth-wordmark-logo" />
      </span>
      <span className="auth-wordmark-line auth-wordmark-agent">Gate</span>
    </h1>
  );
}

// 蚀刻铭牌：印在面板底部凹槽里的一行规格，像激光刻在机壳上的小字。
// 「在线」LED 不在这里——它挂在面板头部，紧贴标题。
function SpecPlate() {
  // 固件版本走免会话的 GET /admin/v1/version：这一页在会话之前，别的读数一概
  // 取不到。取不到版本（启动失败卡片就常是这种处境）整格不渲染——铭牌宁可少
  // 刻一项，也不印一个写死的假版本。
  const version = useResource(() => api.firmwareVersion(), []);
  return (
    <dl className="text-muted-foreground flex min-h-4 flex-wrap items-baseline gap-x-6 gap-y-1 font-mono text-xs">
      {version.data === null || version.data.hardware_model === "" ? null : (
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <dt className="whitespace-nowrap">{t("设备型号")}</dt>
          <dd className="text-foreground/85">{version.data.hardware_model}</dd>
        </div>
      )}
      {version.data === null || version.data.version === "" ? null : (
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <dt className="whitespace-nowrap">{t("固件版本")}</dt>
          <dd className="text-foreground/85 whitespace-nowrap">{version.data.version}</dd>
        </div>
      )}
    </dl>
  );
}

// 配色开关：铭牌右端的四粒指示灯，一眼看见、一下点到。登录前没有顶栏，账户菜单
// 那个入口这时还不存在，所以入口摆在这条铭牌上——挑配色是纯本地显示偏好
// （lib/theme.ts），不需要会话，改完立刻生效。
//
// 每粒灯是原生 <input type="radio"> + 一个兄弟色点：单选语义、方向键切换、读屏
// 报「单选按钮 已选中」全由浏览器给，不必自己接键盘。输入框 sr-only（是裁切不是
// display:none，仍可聚焦），亮起来靠 peer-checked。
//
// 悬停反馈用放大而不是加亮描边：hover 与 peer-checked 同为 (0,2,0)，Tailwind 把
// hover 排在后面，两者都改 ring 颜色的话，鼠标划过选中那粒反而会把它的白圈调暗。
function ThemePicker() {
  const [theme, switchTheme] = useTheme();
  return (
    <div className="flex items-center gap-2.5">
      <span id="auth-theme-label" className="text-muted-foreground font-mono text-xs">
        {t("界面配色")}
      </span>
      <div
        role="radiogroup"
        aria-labelledby="auth-theme-label"
        className="flex items-center gap-0.5"
      >
        {THEMES.map((th) => (
          <label
            key={th.id}
            title={th.label}
            className="flex cursor-pointer items-center justify-center p-1"
          >
            <input
              type="radio"
              name="auth-theme"
              value={th.id}
              checked={theme === th.id}
              onChange={() => switchTheme(th.id)}
              className="peer sr-only"
            />
            <span className="sr-only">{th.label}</span>
            <span
              aria-hidden
              style={{ backgroundColor: th.swatch }}
              className="block size-3.5 rounded-full ring-1 ring-white/30 transition hover:scale-110 peer-checked:ring-2 peer-checked:ring-white/90 peer-focus-visible:ring-2 peer-focus-visible:ring-white"
            />
          </label>
        ))}
      </div>
    </div>
  );
}

export function AuthShell({
  heading,
  note = "",
  live = true,
  children,
}: {
  heading: string;
  note?: string;
  live?: boolean;
  children: React.ReactNode;
}): React.ReactElement {
  return (
    <div className="auth-backdrop relative isolate flex min-h-svh flex-col items-center justify-start overflow-clip px-5 pt-16 pb-9 lg:justify-center lg:px-[clamp(24px,5vw,80px)] lg:py-16">
      <div aria-hidden className="auth-ambient"><span /><span /></div>
      {/* 语言开关摆整页右上角：登录前没有顶栏，这是「右上角」唯一空着的地方，也
          正对登录后顶栏右端那同一个钮。它站在页面底纹上（不在深色面板里），所以
          用常规 token 取色；`z-10` 保证矮视口下面板挨上来时它仍在最上层。
          配色开关不跟着搬——四粒灯排在铭牌上比收进菜单更快点到。

          这一处比顶栏那一处大一档、加一圈淡描边：那边有账户钮和读数条作伴，
          一眼就知道是排可按的东西；这边孤零零一个字符标浮在底纹上，不描出边界
          就只像页角印了两个字母。 */}
      <LangSwitch className="border-foreground/15 bg-background/50 text-foreground/70 hover:text-foreground absolute top-5 right-5 z-10 size-8 rounded-md border lg:top-7 lg:right-8" />
      {/* 两列摆得下（lg 起）才是大字标 | 操作卡；窄屏改单列堆叠：大字标 → 操作卡，
          登录框保持在首屏附近。左列吃掉全部富余宽度，右列只留够放表单的一段。 */}
      <div className="auth-layout grid w-[min(500px,100%)] grid-cols-1 items-center gap-y-8 lg:w-[min(1280px,100%)] lg:grid-cols-[minmax(0,1fr)_clamp(420px,35vw,500px)] lg:gap-x-[clamp(40px,6vw,96px)]">
        <div className="auth-mark relative">
          <div aria-hidden className="auth-orbits"><span /><span /><span /></div>
          <p className="auth-eyebrow">{t("本地 AI 网关")}</p>
          <Wordmark />
          <p className="auth-tagline">{t("连接模型与开发工具")}</p>
          <div aria-hidden className="auth-circuit"><span /><span /><span /></div>
        </div>
        {/* .auth-edge 是能量光带外圈；进场动画只演一次，短促上浮后面板落定。
            面板底色 bg-(--auth-body) 写在 JSX：utilities 层才盖得住 Card 自带的
            bg-card，.auth-panel 只补 background-image 质感层。 */}
        <div className="auth-edge w-full lg:justify-self-end">
          <Card className="dark auth-panel relative gap-6 overflow-clip rounded-[22px] border-white/10 bg-(--auth-body) p-6 pb-0 shadow-[inset_0_1px_0_rgb(255_255_255/0.07)] sm:p-8 sm:pb-0">
            <div className="flex items-center justify-between gap-3">
              <div className="flex items-center gap-3">
                <span aria-hidden className="auth-heading-icon"><LogIn className="size-5" /></span>
                <h2 className="text-xl font-semibold tracking-wide sm:text-2xl">{heading}</h2>
              </div>
              {live ? (
                <span className="auth-status flex items-center gap-2 font-mono text-xs">
                  <span aria-hidden className="auth-led inline-block size-1.5 rounded-full bg-signal-ok" />
                  {t("在线")}
                </span>
              ) : null}
            </div>
            {note === "" ? null : <p className="text-muted-foreground text-sm">{note}</p>}
            {children}
            <div className="-mx-6 flex flex-wrap items-center justify-between gap-x-6 gap-y-3 border-t border-white/10 bg-black/20 px-6 py-4 sm:-mx-8 sm:px-8">
              <SpecPlate />
              <ThemePicker />
            </div>
          </Card>
        </div>
      </div>
    </div>
  );
}
