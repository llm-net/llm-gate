// 界面文案与多语言。**简体中文是源文本，也是查表的键**：代码里只写中文，
// `t("中文")` 按整句去当前语言的目录里查译文，查不到（没有条目或条目为空串）就
// 原样显示中文——所以平时开发只写中文，译文由 `/i18n` 技能定期补齐，缺翻译
// 只是暂时显示中文，从不阻塞构建。目录在 src/locales/<lang>.json，形状恒为
// `{ "中文": "译文" }`；英文需要单复数时值可以写成 `{ "one": …, "other": … }`，
// 按 `count`（缺省取第一个数字参数）用 Intl.PluralRules 选形。
//
// 语言是纯本地显示偏好（与 lib/theme.ts 同一口径）：只存本浏览器 localStorage
// （键 llmgate.ui.lang），不进服务端设置。缺省按浏览器语言在 LANGS 里就近匹配，
// 匹配不上落回简体中文。切换语言整页 reload：导航项、主题标签这类模块级常量在
// import 期就调了 t()，reload 比把每个常量改成 hook 便宜且不会漏。
//
// 目录在 main.tsx 里先于其余模块加载（initI18n → 再动态 import 业务模块），
// 所以任何位置（模块级常量、事件回调、渲染期）都可以直接调 t()，不需要 hook、
// 不需要 Provider。请求 /admin/v1 时 lib/api/client.ts 把当前语言放进
// Accept-Language，服务端错误文案由固件 internal/i18n 按同一套目录思路本地化。
//
// 写法约束（scripts/i18n.mjs 按 AST 抽取，违反就抽不到）：
// - t / tx 的第一个参数必须是**不带表达式的字符串字面量**；变量拼接、模板插值都
//   改成占位符：t("共 {n} 个模型", { n })。
// - 带行内元素的句子用 tx，标签名只认 [A-Za-z_][A-Za-z0-9_]*：
//   tx("把 <c>sk_</c> 填进请求头", { c: (s) => <code>{s}</code> })。
//   目录里没登记为函数的标签（如说明文里的 <模型名>）按字面输出。
// - 确实不该翻译的中文字面量（开发者断言之类）在同一行或上一行加 `// i18n-ignore`。

import { createElement, Fragment, type ReactNode } from "react";

export type Lang = "zh-CN" | "en" | "ja";

/** 源语言：代码里写的就是它，永远不需要目录。 */
export const SOURCE_LANG: Lang = "zh-CN";

/**
 * 一种语言在切换入口里的全部露面：自称 label 与字符标 glyph。两者都**刻意不翻译**
 * ——菜单里每一项都用它自己的语言写，看不懂当前语言的人才找得到自己那一项。
 * glyph 是「当前语言图标」，components/lang-switch.tsx 那个方钮里画的就是它。
 */
export type LangEntry = { id: Lang; label: string; glyph: string };

const ZH: LangEntry = { id: "zh-CN", label: "简体中文", glyph: "中" }; // i18n-ignore
const EN: LangEntry = { id: "en", label: "English", glyph: "EN" };
const JA: LangEntry = { id: "ja", label: "日本語", glyph: "日" }; // i18n-ignore

export const LANGS: LangEntry[] = [ZH, EN, JA];

/** langEntry 取某语言那一条；表外的语言到不了这里（loadLang 只认表里的值）。 */
export function langEntry(id: Lang): LangEntry {
  return LANGS.find((l) => l.id === id) ?? ZH;
}

const STORAGE_KEY = "llmgate.ui.lang";

type Entry = string | Record<string, string>;
type Catalog = Record<string, Entry>;
type Params = Record<string, string | number>;

// 目录按语言分块懒加载：源语言什么都不加载，别的语言只加载自己那份。
// 新增语言 = 放一份 src/locales/<lang>.json + 在 LANGS 里登记。
const catalogs = import.meta.glob<Catalog>("../locales/*.json", { import: "default" });

let lang: Lang = SOURCE_LANG;
let catalog: Catalog = {};
let plural: Intl.PluralRules | null = null;

function isLang(v: unknown): v is Lang {
  return LANGS.some((l) => l.id === v);
}

function primary(tag: string): string {
  return tag.toLowerCase().split("-")[0] ?? "";
}

/** loadLang 决定本次打开页面用哪种语言：存过的 → 浏览器偏好就近匹配 → 源语言。 */
export function loadLang(): Lang {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (isLang(v)) return v;
  } catch {
    // storage 不可用（隐私模式等）就当没存过。
  }
  const prefs = navigator.languages.length > 0 ? navigator.languages : [navigator.language];
  for (const p of prefs) {
    const hit = LANGS.find((l) => primary(l.id) === primary(p));
    if (hit !== undefined) return hit.id;
  }
  return SOURCE_LANG;
}

/**
 * initI18n 在业务模块加载前调用一次：定语言、挂 <html lang>、取目录。
 * 目录取不到（构建缺文件、网络断）就退回源语言显示，界面照常可用。
 */
export async function initI18n(): Promise<void> {
  lang = loadLang();
  document.documentElement.lang = lang;
  if (lang === SOURCE_LANG) return;
  const load = catalogs[`../locales/${lang}.json`];
  if (load === undefined) return;
  try {
    catalog = await load();
  } catch {
    catalog = {};
  }
  try {
    plural = new Intl.PluralRules(lang);
  } catch {
    plural = null;
  }
}

export function currentLang(): Lang {
  return lang;
}

/** setLang 持久化并整页 reload（缘由见文件头）。 */
export function setLang(next: Lang): void {
  if (next === lang) return;
  try {
    localStorage.setItem(STORAGE_KEY, next);
  } catch {
    // 存不上就只能靠浏览器语言了；reload 后仍按 loadLang 的顺序判定。
  }
  location.reload();
}

/**
 * sentenceSeparator 是把两句话拼成一段时中间放的东西：中文、日文的句子之间不留空，
 * 别的语言要一个空格。只在「多句拼一段」这种没法写成一个键的地方用。
 */
export function sentenceSeparator(): string {
  const p = primary(lang);
  return p === "zh" || p === "ja" ? "" : " ";
}

/** useLang 与 useTheme 同形，供 components/lang-switch.tsx 用；选中态不需要 state——切换即 reload。 */
export function useLang(): [Lang, (next: Lang) => void] {
  return [lang, setLang];
}

const PLACEHOLDER = /\{([A-Za-z_][A-Za-z0-9_]*)\}/g;

function pluralCount(params: Params | undefined): number | undefined {
  if (params === undefined) return undefined;
  if (typeof params.count === "number") return params.count;
  for (const v of Object.values(params)) if (typeof v === "number") return v;
  return undefined;
}

// pick 取当前语言的译文原型；null = 没有可用译文，调用方退回中文。
function pick(text: string, params: Params | undefined): string | null {
  const entry = catalog[text];
  if (entry === undefined) return null;
  if (typeof entry === "string") return entry === "" ? null : entry;
  const n = pluralCount(params);
  const category = n !== undefined && plural !== null ? plural.select(n) : "other";
  const form = entry[category] ?? entry.other;
  return form === undefined || form === "" ? null : form;
}

function fill(raw: string, params: Params | undefined): string {
  if (params === undefined) return raw;
  return raw.replace(PLACEHOLDER, (m: string, name: string) => (name in params ? String(params[name]) : m));
}

/** t 翻译一句纯文本；占位符写 {name}，从 params 取值。 */
export function t(text: string, params?: Params): string {
  return fill(pick(text, params) ?? text, params);
}

type Part = ReactNode | ((chunk: ReactNode) => ReactNode);
type Parts = Record<string, Part>;

const TOKEN =
  /\{([A-Za-z_][A-Za-z0-9_]*)\}|<([A-Za-z_][A-Za-z0-9_]*)\s*\/>|<([A-Za-z_][A-Za-z0-9_]*)>|<\/([A-Za-z_][A-Za-z0-9_]*)>/g;

function isWrapper(p: Part | undefined): p is (chunk: ReactNode) => ReactNode {
  return typeof p === "function";
}

// render 把带 {占位} 与 <tag>…</tag> 的译文串装成 ReactNode 列表。子节点全部
// 以独立参数交给 createElement（不是数组），所以不需要 key。
function render(s: string, parts: Parts): ReactNode[] {
  type Frame = { tag: string | null; children: ReactNode[] };
  const stack: Frame[] = [{ tag: null, children: [] }];
  const top = (): Frame => stack[stack.length - 1] as Frame;
  const push = (node: ReactNode): void => {
    top().children.push(node);
  };
  let last = 0;
  for (const m of s.matchAll(TOKEN)) {
    const idx = m.index;
    if (idx > last) push(s.slice(last, idx));
    last = idx + m[0].length;
    const [tok, ph, selfTag, openTag, closeTag] = m;
    if (ph !== undefined) {
      const p = parts[ph];
      push(ph in parts && !isWrapper(p) ? p : tok);
    } else if (selfTag !== undefined) {
      const p = parts[selfTag];
      push(isWrapper(p) ? p(null) : tok);
    } else if (openTag !== undefined) {
      if (isWrapper(parts[openTag])) stack.push({ tag: openTag, children: [] });
      else push(tok);
    } else if (closeTag !== undefined) {
      const frame = top();
      if (frame.tag === closeTag && stack.length > 1) {
        stack.pop();
        const wrap = parts[closeTag];
        if (isWrapper(wrap)) push(wrap(createElement(Fragment, null, ...frame.children)));
      } else {
        push(tok);
      }
    }
  }
  if (last < s.length) push(s.slice(last));
  // 没闭合的标签当字面文本吐回去，不吞内容。
  while (stack.length > 1) {
    const frame = stack.pop() as Frame;
    top().children.push(`<${frame.tag}>`, ...frame.children);
  }
  return (stack[0] as Frame).children;
}

/**
 * tx 翻译带行内元素或 ReactNode 参数的句子。parts 里：函数 = 标签包装器
 * （`<c>…</c>` 的内容交给 parts.c），其余值 = `{name}` 占位符的内容。
 * 数字参数同时参与复数选形。
 */
export function tx(text: string, parts: Parts = {}): ReactNode {
  const numeric: Params = {};
  for (const [k, v] of Object.entries(parts)) if (typeof v === "number") numeric[k] = v;
  const raw = pick(text, numeric) ?? text;
  return createElement(Fragment, null, ...render(raw, parts));
}
