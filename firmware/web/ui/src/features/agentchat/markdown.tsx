// 智能体文本的 Markdown 渲染：Agent远控的主机档案与两页时间线里的回复共用。不引库，只认
// GFM 的常用子集——块：标题、段落、围栏代码、引用、分隔线、有序 / 无序 / 任务列表（可嵌套）、
// 表格（含对齐）；行内：代码、粗体、斜体、删除线、链接与裸 http(s) 地址、反斜杠转义。
// 原始 HTML 一律按文本显示；图片语法只渲染成链接（设备可能离线，也不替智能体给出的地址发请求）；
// 链接只放行 http(s) / mailto，新窗口打开。段落内的单个换行照原样保留（对话里的回复常这样写）。

import { Fragment } from "react";

import { cn } from "@/lib/cn";

// ---- 行内 ----

const INLINE = new RegExp(
  [
    /(?<code>(`+)(?<codeBody>[\s\S]*?[^`])\2(?!`))/u.source,
    /\\(?<esc>[\\`*_{}[\]()#+\-.!|~<>])/u.source,
    /!?\[(?<linkText>(?:\\.|[^\]\\])*)\]\(\s*<?(?<linkHref>(?:[^()\s<>]|\([^()\s]*\))+)>?(?:\s+"[^"]*")?\s*\)/u.source,
    /<(?<auto>(?:https?:\/\/|mailto:)[^>\s]+)>/u.source,
    /(?<url>https?:\/\/[^\s<>`]*[^\s<>`.,;:!?'")\]）。，；：！？」』])/u.source,
    /\*\*(?<bold>(?=\S)[\s\S]*?\S)\*\*/u.source,
    /(?<![\p{L}\p{N}_])__(?<bold2>(?=\S)[\s\S]*?\S)__(?![\p{L}\p{N}_])/u.source,
    /~~(?<strike>(?=\S)[\s\S]*?\S)~~/u.source,
    /\*(?<em>(?=[^\s*])[\s\S]*?[^\s*])\*/u.source,
    /(?<![\p{L}\p{N}_])_(?<em2>(?=[^\s_])[\s\S]*?[^\s_])_(?![\p{L}\p{N}_])/u.source,
  ].join("|"),
  "gu",
);

function safeHref(href: string): string | null {
  return /^(https?:\/\/|mailto:)/i.test(href) ? href : null;
}

function ExtLink({ href, children }: { href: string; children: React.ReactNode }): React.ReactElement {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" className="text-primary break-all underline underline-offset-2">
      {children}
    </a>
  );
}

function inline(text: string, key: string): React.ReactNode[] {
  const out: React.ReactNode[] = [];
  let last = 0;
  let i = 0;
  for (const m of text.matchAll(INLINE)) {
    const g = m.groups ?? {};
    const idx = m.index ?? 0;
    if (idx > last) out.push(text.slice(last, idx));
    last = idx + m[0].length;
    const k = `${key}.${i++}`;
    if (g.code !== undefined) {
      const body = g.codeBody ?? "";
      // 两端各一个空格是为了包住反引号，按 CommonMark 去掉。
      const trimmed = body.length > 2 && body.startsWith(" ") && body.endsWith(" ") ? body.slice(1, -1) : body;
      out.push(<code key={k} className="bg-muted rounded px-1 font-mono text-[0.85em] break-words">{trimmed}</code>);
    } else if (g.esc !== undefined) {
      out.push(g.esc);
    } else if (g.linkHref !== undefined) {
      const label = inline(g.linkText ?? "", k);
      const href = safeHref(g.linkHref);
      out.push(href === null ? <Fragment key={k}>{label}</Fragment> : <ExtLink key={k} href={href}>{label.length > 0 ? label : href}</ExtLink>);
    } else if (g.auto !== undefined || g.url !== undefined) {
      const href = g.auto ?? g.url ?? "";
      out.push(<ExtLink key={k} href={href}>{href.replace(/^mailto:/i, "")}</ExtLink>);
    } else if (g.bold !== undefined || g.bold2 !== undefined) {
      out.push(<strong key={k} className="font-semibold">{inline(g.bold ?? g.bold2 ?? "", k)}</strong>);
    } else if (g.strike !== undefined) {
      out.push(<del key={k}>{inline(g.strike, k)}</del>);
    } else if (g.em !== undefined || g.em2 !== undefined) {
      out.push(<em key={k}>{inline(g.em ?? g.em2 ?? "", k)}</em>);
    }
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

// ---- 块 ----

const FENCE = /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)[^`]*$/;
const HEADING = /^ {0,3}(#{1,6})(?:\s+(.*?))?(?:\s+#+)?\s*$/;
const HR = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const QUOTE = /^ {0,3}>\s?(.*)$/;
const LIST_ITEM = /^( *)([-*+]|\d{1,9}[.)])(?:\s+(.*)|\s*$)/;
const TABLE_DELIM = /^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*)*\|?\s*$/;

type Align = "left" | "center" | "right" | undefined;

function expandTabs(line: string): string {
  return line.replace(/^[ \t]+/, (lead) => lead.replace(/\t/g, "    "));
}

function indentOf(line: string): number {
  return line.length - line.trimStart().length;
}

function isBlank(line: string): boolean {
  return line.trim() === "";
}

/** 按未转义、不在行内代码里的 `|` 切开一行表格。 */
function splitRow(line: string): string[] {
  let s = line.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
  const cells: string[] = [];
  let cur = "";
  let tick = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s[i] ?? "";
    if (c === "\\" && s[i + 1] === "|") {
      cur += "|";
      i++;
    } else if (c === "`") {
      let run = 1;
      while (s[i + run] === "`") run++;
      tick = tick === 0 ? run : tick === run ? 0 : tick;
      cur += "`".repeat(run);
      i += run - 1;
    } else if (c === "|" && tick === 0) {
      cells.push(cur.trim());
      cur = "";
    } else {
      cur += c;
    }
  }
  cells.push(cur.trim());
  return cells;
}

function isTableStart(lines: string[], i: number): boolean {
  const head = lines[i] ?? "";
  const delim = lines[i + 1] ?? "";
  if (!head.includes("|") || !TABLE_DELIM.test(delim)) return false;
  if (!delim.includes("|") && !/-\s*$/.test(delim)) return false;
  return splitRow(head).length === splitRow(delim).length;
}

/** 这一行能不能打断正在收集的段落。 */
function startsBlock(lines: string[], i: number): boolean {
  const line = lines[i] ?? "";
  return FENCE.test(line) || HEADING.test(line) || HR.test(line) || QUOTE.test(line) || LIST_ITEM.test(line) || isTableStart(lines, i);
}

const HEADING_CLASS = [
  "",
  "mt-4 mb-2 border-b pb-1 text-lg font-semibold",
  "mt-4 mb-2 border-b pb-1 text-base font-semibold",
  "mt-3 mb-1.5 text-sm font-semibold",
  "mt-3 mb-1 text-sm font-semibold",
  "mt-2 mb-1 text-sm font-medium",
  "text-muted-foreground mt-2 mb-1 text-sm font-medium",
];

function blocks(lines: string[], key: string, tight = false): React.ReactNode[] {
  const out: React.ReactNode[] = [];
  let i = 0;
  let n = 0;
  const nextKey = () => `${key}.${n++}`;
  while (i < lines.length) {
    const line = lines[i] ?? "";
    if (isBlank(line)) {
      i++;
      continue;
    }

    const fence = FENCE.exec(line);
    if (fence) {
      const marker = fence[1] ?? "```";
      const indent = indentOf(line);
      const code: string[] = [];
      i++;
      while (i < lines.length) {
        const l = lines[i] ?? "";
        const close = l.trim();
        if (close.startsWith(marker) && close.replace(new RegExp(`^\\${marker[0]}+`), "") === "") {
          i++;
          break;
        }
        code.push(l.slice(Math.min(indent, indentOf(l))));
        i++;
      }
      out.push(
        <pre key={nextKey()} className="bg-muted/60 my-2 overflow-x-auto rounded-md p-2 font-mono text-xs leading-5 whitespace-pre">
          {code.join("\n")}
        </pre>,
      );
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading) {
      const level = (heading[1] ?? "#").length;
      const Tag = `h${level}` as "h1";
      out.push(<Tag key={nextKey()} className={HEADING_CLASS[level]}>{inline(heading[2] ?? "", `${key}.h${n}`)}</Tag>);
      i++;
      continue;
    }

    if (HR.test(line)) {
      out.push(<hr key={nextKey()} className="my-3 border-t" />);
      i++;
      continue;
    }

    if (QUOTE.test(line)) {
      const inner: string[] = [];
      while (i < lines.length && !isBlank(lines[i] ?? "")) {
        const q = QUOTE.exec(lines[i] ?? "");
        if (q) inner.push(q[1] ?? "");
        else if (inner.length > 0 && !startsBlock(lines, i)) inner.push(lines[i] ?? "");
        else break;
        i++;
      }
      const k = nextKey();
      out.push(
        <blockquote key={k} className="text-muted-foreground my-2 border-l-2 pl-3">
          {blocks(inner, k)}
        </blockquote>,
      );
      continue;
    }

    if (isTableStart(lines, i)) {
      const head = splitRow(line);
      const aligns: Align[] = splitRow(lines[i + 1] ?? "").map((d) => {
        const l = d.startsWith(":");
        const r = d.endsWith(":");
        return l && r ? "center" : r ? "right" : l ? "left" : undefined;
      });
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && !isBlank(lines[i] ?? "") && (lines[i] ?? "").includes("|") && !FENCE.test(lines[i] ?? "")) {
        rows.push(splitRow(lines[i] ?? ""));
        i++;
      }
      const k = nextKey();
      out.push(
        <div key={k} className="my-2 max-w-full overflow-x-auto">
          <table className="w-max min-w-full border-collapse text-xs">
            <thead>
              <tr>
                {head.map((c, ci) => (
                  <th key={ci} style={{ textAlign: aligns[ci] }} className="bg-muted/50 border px-2 py-1 text-left font-medium">
                    {inline(c, `${k}.h${ci}`)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.map((row, ri) => (
                <tr key={ri}>
                  {head.map((_, ci) => (
                    <td key={ci} style={{ textAlign: aligns[ci] }} className="border px-2 py-1 align-top">
                      {inline(row[ci] ?? "", `${k}.${ri}.${ci}`)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>,
      );
      continue;
    }

    const item = LIST_ITEM.exec(line);
    if (item) {
      const base = (item[1] ?? "").length;
      const ordered = /\d/.test(item[2] ?? "");
      const start = ordered ? Number.parseInt(item[2] ?? "1", 10) : 1;
      const items: string[][] = [];
      let loose = false;
      while (i < lines.length) {
        const m = LIST_ITEM.exec(lines[i] ?? "");
        if (!m || (m[1] ?? "").length > base + 1 || /\d/.test(m[2] ?? "") !== ordered) break;
        const content = (m[1] ?? "").length + (m[2] ?? "").length + Math.max(1, Math.min(4, (lines[i] ?? "").length - (m[1] ?? "").length - (m[2] ?? "").length - (m[3] ?? "").length));
        const body: string[] = [m[3] ?? ""];
        i++;
        // 这一项的后续行：缩进过标记的都归它（嵌套列表、续行、代码块）；空行后若不再缩进即结束。
        while (i < lines.length) {
          const l = lines[i] ?? "";
          if (isBlank(l)) {
            let j = i + 1;
            while (j < lines.length && isBlank(lines[j] ?? "")) j++;
            if (j < lines.length && indentOf(lines[j] ?? "") >= content) {
              body.push("");
              i++;
              continue;
            }
            break;
          }
          if (indentOf(l) >= content) {
            body.push(l.slice(content));
          } else if (indentOf(l) > base && LIST_ITEM.test(l)) {
            body.push(l.slice(Math.min(indentOf(l), base + 2)));
          } else if (!startsBlock(lines, i) && !isBlank(body[body.length - 1] ?? "")) {
            body.push(l.trim());
          } else {
            break;
          }
          i++;
        }
        items.push(body);
        // 项与项之间隔着空行即为松散列表。
        let j = i;
        while (j < lines.length && isBlank(lines[j] ?? "")) j++;
        const nextItem = LIST_ITEM.exec(lines[j] ?? "");
        if (j > i && nextItem && (nextItem[1] ?? "").length <= base + 1 && /\d/.test(nextItem[2] ?? "") === ordered) {
          loose = true;
          i = j;
        }
      }
      const k = nextKey();
      const children = items.map((body, ii) => {
        const task = /^\[([ xX])\]\s+/.exec(body[0] ?? "");
        const rest = task ? [(body[0] ?? "").slice(task[0].length), ...body.slice(1)] : body;
        return (
          <li key={ii} className={cn("my-0.5 pl-0.5", task && "list-none")}>
            {task ? (
              <div className="-ml-5 flex gap-1.5">
                <input type="checkbox" checked={task[1] !== " "} readOnly disabled className="mt-1.5 size-3.5 shrink-0" />
                <div className="min-w-0 flex-1">{blocks(rest, `${k}.${ii}`, !loose)}</div>
              </div>
            ) : (
              blocks(rest, `${k}.${ii}`, !loose)
            )}
          </li>
        );
      });
      const listClass = "marker:text-muted-foreground my-1.5 pl-5";
      out.push(
        ordered ? (
          <ol key={k} start={start !== 1 ? start : undefined} className={cn(listClass, "list-decimal")}>{children}</ol>
        ) : (
          <ul key={k} className={cn(listClass, "list-disc")}>{children}</ul>
        ),
      );
      continue;
    }

    // 段落：一直收到空行或能打断它的块为止。
    const para: string[] = [line.trim()];
    i++;
    while (i < lines.length && !isBlank(lines[i] ?? "") && !startsBlock(lines, i)) {
      para.push((lines[i] ?? "").trim());
      i++;
    }
    const k = nextKey();
    out.push(
      <p key={k} className={cn("whitespace-pre-wrap", tight ? "my-0" : "my-1.5")}>
        {inline(para.join("\n"), k)}
      </p>,
    );
  }
  return out;
}

export function Markdown({ text, className }: { text: string; className?: string }): React.ReactElement {
  const lines = text.replace(/\r\n?/g, "\n").split("\n").map(expandTabs);
  return <div className={cn("min-w-0 text-sm leading-6 break-words [&>:first-child]:mt-0 [&>:last-child]:mb-0", className)}>{blocks(lines, "md")}</div>;
}
