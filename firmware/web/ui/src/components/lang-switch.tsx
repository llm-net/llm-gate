// 语言开关：一个方钮画着当前语言的字符标（中 / EN / 日），点开是一行一种语言的单选菜单
// （同一枚字符标 + 该语言的自称）。登录前后各有一处在场——登录页整页右上角、外壳
// 顶栏右端——两处只差配色，所以钮的类名由调用方给，菜单共用这一份。
//
// 它**不收进任何菜单的第二层**：换语言是看不懂当前语言的人第一件要做的事，藏进
// 账户菜单的子菜单等于要求他先读懂「账户」两个字。字符标同理不用 lucide 的地球或
// Languages 图标——那两个只说「这里能换语言」，不说「现在是哪一种」；也不用国旗，
// 国旗认的是国家不是语言，且 Windows 的系统字体根本不画国旗 emoji。
//
// 切换即整页 reload（lib/i18n.tsx 文件头讲了缘由），不需要会话，登录前也改得动。

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/cn";
import { LANGS, langEntry, t, useLang, type Lang } from "@/lib/i18n";

// 字符标画在 16px 方格里，与菜单里 lucide 图标同一格、同一列。两个字母的标（EN）
// 比一个汉字的（中）压一档字号：同字号下两个字母明显更宽更抢眼，压一档才在方格里
// 站得住，也不比汉字重。
function Glyph({ glyph }: { glyph: string }) {
  return (
    <span
      aria-hidden
      className={cn(
        "inline-flex size-4 shrink-0 items-center justify-center font-semibold",
        glyph.length > 1 ? "text-[11px] tracking-tight" : "text-[13px]",
      )}
    >
      {glyph}
    </span>
  );
}

export function LangSwitch({ className }: { className?: string }): React.ReactElement {
  const [lang, switchLang] = useLang();
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" title={t("语言")} className={cn("size-7", className)}>
          <Glyph glyph={langEntry(lang).glyph} />
          <span className="sr-only">{t("语言")}</span>
        </Button>
      </DropdownMenuTrigger>
      {/* 每一项用它自己的语言写（LANGS 的 label 刻意不翻），看不懂当前语言的人
          也找得到自己那一项；单选点标出当前语言，不必另加勾选态。 */}
      <DropdownMenuContent align="end">
        <DropdownMenuRadioGroup value={lang} onValueChange={(v) => switchLang(v as Lang)}>
          {LANGS.map((l) => (
            <DropdownMenuRadioItem key={l.id} value={l.id} lang={l.id}>
              <Glyph glyph={l.glyph} />
              {l.label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
