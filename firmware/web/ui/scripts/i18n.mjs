#!/usr/bin/env node
// 界面文案目录工具（src/lib/i18n.tsx 的配套）。用 @babel/parser 按 AST 抽取
// t("…") / tx("…") 的键，与 src/locales/<lang>.json 对账；顺便找出没包进 t/tx
// 的中文字面量。设计与写法约束见 src/lib/i18n.tsx 文件头与 firmware/AGENTS.md。
//
//   node scripts/i18n.mjs sync                 目录对账：缺的键补成 ""，多余的删掉，按键排序
//   node scripts/i18n.mjs check [--strict]     校验；目录形状/占位符/标签不一致即失败，
//                                              --strict 时缺译、硬编码中文、动态键也失败
//   node scripts/i18n.mjs report [--lang en] [--missing] [--json]
//                                              键 → 出处与源码行（给翻译看上下文）
//   node scripts/i18n.mjs hardcoded [--json]   列出没包进 t/tx 的中文字面量
//
// 不是构建产物的一部分，也不会被打进二进制。

import { readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

import { parse } from "@babel/parser";

const ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");
const SRC = join(ROOT, "src");
const LOCALES = join(SRC, "locales");

// 写成转义而不是字面字符：编辑器的 NFC 规整会把 U+F900 换成 U+8C48，区间一变就连
// 代理对（emoji、数学字母）都算成中文。
const CJK = /[\u3400-\u4DBF\u4E00-\u9FFF\uF900-\uFAFF]/;
const PLACEHOLDER = /\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
const TAG = /<\/?([A-Za-z_][A-Za-z0-9_]*)\s*\/?>/g;
const PLURAL_CATEGORIES = new Set(["zero", "one", "two", "few", "many", "other"]);
const CALLEES = new Set(["t", "tx"]);

// ---- 源码扫描 ----

function listSources(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) {
      if (p !== LOCALES) listSources(p, out);
      continue;
    }
    if (/\.(ts|tsx)$/.test(name) && !name.endsWith(".d.ts")) out.push(p);
  }
  return out.sort();
}

const SKIP_KEYS = new Set([
  "loc", "start", "end", "range", "extra", "comments", "leadingComments",
  "trailingComments", "innerComments", "tokens", "errors",
]);

function* children(node) {
  for (const k of Object.keys(node)) {
    if (SKIP_KEYS.has(k)) continue;
    const v = node[k];
    if (Array.isArray(v)) {
      for (const c of v) if (c && typeof c.type === "string") yield [k, c];
    } else if (v && typeof v.type === "string") {
      yield [k, v];
    }
  }
}

function literalKey(arg) {
  if (!arg) return { key: null, dynamic: false };
  if (arg.type === "StringLiteral") return { key: arg.value, dynamic: false };
  if (arg.type === "TemplateLiteral" && arg.expressions.length === 0) {
    return { key: arg.quasis[0].value.cooked, dynamic: false };
  }
  return { key: null, dynamic: true };
}

/**
 * scanFile 返回 { keys: [{key, line, context}], dynamic: [{line}], hardcoded: [{line, text}] }。
 */
function scanFile(file) {
  const code = readFileSync(file, "utf8");
  const lines = code.split("\n");
  const rel = relative(ROOT, file);
  const ast = parse(code, {
    sourceType: "module",
    plugins: file.endsWith(".tsx") ? ["typescript", "jsx"] : ["typescript"],
    errorRecovery: true,
    attachComment: false,
  });
  // `i18n-ignore` 出现在注释里：同一行或上一行的中文字面量不算硬编码。
  const ignoreLines = new Set();
  for (const c of ast.comments ?? []) {
    if (c.value.includes("i18n-ignore")) ignoreLines.add(c.loc.start.line);
  }
  const ignored = (line) => ignoreLines.has(line) || ignoreLines.has(line - 1);

  const keyNodes = new Set();
  const result = { keys: [], dynamic: [], hardcoded: [] };
  const ctx = (line) => (lines[line - 1] ?? "").trim();

  function visit(node, parent, parentKey) {
    if (node.type === "CallExpression" && node.callee.type === "Identifier" && CALLEES.has(node.callee.name)) {
      const arg = node.arguments[0];
      const { key, dynamic } = literalKey(arg);
      const line = node.loc.start.line;
      if (dynamic) result.dynamic.push({ file: rel, line, context: ctx(line) });
      else if (key !== null) {
        keyNodes.add(arg);
        result.keys.push({ key, file: rel, line, context: ctx(line) });
      }
    }
    let text = null;
    switch (node.type) {
      case "StringLiteral":
        if (keyNodes.has(node)) break;
        if (parent && (parent.type === "ImportDeclaration" || parent.type === "ExportNamedDeclaration" ||
          parent.type === "ExportAllDeclaration" || parent.type === "TSLiteralType" ||
          parent.type === "TSImportType")) break;
        // 对象属性名写成中文是数据不是文案（极少见），也不报。
        if (parent && parent.type === "ObjectProperty" && parentKey === "key") break;
        text = node.value;
        break;
      case "TemplateLiteral":
        if (keyNodes.has(node)) break;
        text = node.quasis.map((q) => q.value.cooked ?? "").join("…");
        break;
      case "JSXText":
        text = node.value.trim();
        break;
      default:
        break;
    }
    if (text !== null && CJK.test(text) && !ignored(node.loc.start.line)) {
      result.hardcoded.push({ file: rel, line: node.loc.start.line, text, context: ctx(node.loc.start.line) });
    }
    for (const [k, c] of children(node)) visit(c, node, k);
  }
  visit(ast.program, null, null);
  return result;
}

function scanAll() {
  const keys = new Map(); // key -> [{file,line,context}]
  const dynamic = [];
  const hardcoded = [];
  for (const f of listSources(SRC)) {
    const r = scanFile(f);
    for (const k of r.keys) {
      if (!keys.has(k.key)) keys.set(k.key, []);
      keys.get(k.key).push({ file: k.file, line: k.line, context: k.context });
    }
    dynamic.push(...r.dynamic);
    hardcoded.push(...r.hardcoded);
  }
  return { keys, dynamic, hardcoded };
}

// ---- 目录 ----

function listCatalogs() {
  return readdirSync(LOCALES)
    .filter((n) => n.endsWith(".json"))
    .sort()
    .map((n) => ({ lang: n.slice(0, -5), path: join(LOCALES, n) }));
}

function readCatalog(path) {
  const raw = JSON.parse(readFileSync(path, "utf8"));
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) {
    throw new Error(`${relative(ROOT, path)}: 顶层必须是对象`);
  }
  return raw;
}

function writeCatalog(path, obj) {
  writeFileSync(path, `${JSON.stringify(obj, null, 2)}\n`);
}

function sortKeys(keys) {
  return [...keys].sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
}

function placeholders(s) {
  return [...s.matchAll(PLACEHOLDER)].map((m) => m[1]).sort();
}

function tags(s) {
  return [...s.matchAll(TAG)].map((m) => m[0].replace(/\s+/g, "")).sort();
}

function same(a, b) {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

function forms(entry) {
  if (typeof entry === "string") return [["", entry]];
  return Object.entries(entry);
}

/** validateCatalog 返回 { errors: [], missing: [], orphaned: [] }。 */
function validateCatalog(lang, cat, sourceKeys) {
  const errors = [];
  const missing = [];
  const orphaned = [];
  const tag = (k) => `[${lang}] ${JSON.stringify(k)}`;
  for (const [k, entry] of Object.entries(cat)) {
    if (!sourceKeys.has(k)) orphaned.push(k);
    if (typeof entry === "string") {
      if (entry === "") {
        if (sourceKeys.has(k)) missing.push(k);
        continue;
      }
    } else if (entry !== null && typeof entry === "object" && !Array.isArray(entry)) {
      const cats = Object.keys(entry);
      if (!cats.includes("other")) errors.push(`${tag(k)}: 复数形式缺少 "other"`);
      for (const c of cats) {
        if (!PLURAL_CATEGORIES.has(c)) errors.push(`${tag(k)}: 未知复数类别 "${c}"`);
        if (typeof entry[c] !== "string" || entry[c] === "") errors.push(`${tag(k)}: 复数形式 "${c}" 必须是非空字符串`);
      }
    } else {
      errors.push(`${tag(k)}: 值必须是字符串或 {one, other} 复数对象`);
      continue;
    }
    const kp = placeholders(k);
    const kt = tags(k);
    for (const [c, v] of forms(entry)) {
      if (typeof v !== "string" || v === "") continue;
      const where = c === "" ? tag(k) : `${tag(k)} (${c})`;
      if (!same(kp, placeholders(v))) errors.push(`${where}: 占位符与中文不一致（中文 {${kp.join(",")}}，译文 {${placeholders(v).join(",")}}）`);
      if (!same(kt, tags(v))) errors.push(`${where}: 行内标签与中文不一致（中文 ${kt.join(" ") || "无"}，译文 ${tags(v).join(" ") || "无"}）`);
    }
  }
  for (const k of sourceKeys) if (!(k in cat)) missing.push(k);
  return { errors, missing: sortKeys(new Set(missing)), orphaned: sortKeys(orphaned) };
}

// ---- 子命令 ----

function cmdSync() {
  const { keys, dynamic } = scanAll();
  for (const d of dynamic) console.warn(`动态键（抽不到，改成字面量）: ${d.file}:${d.line}  ${d.context}`);
  for (const { lang, path } of listCatalogs()) {
    const old = readCatalog(path);
    const next = {};
    let added = 0;
    let removed = 0;
    for (const k of sortKeys(keys.keys())) {
      if (!(k in old)) added++;
      next[k] = old[k] ?? "";
    }
    for (const k of Object.keys(old)) if (!keys.has(k)) removed++;
    writeCatalog(path, next);
    const missing = Object.values(next).filter((v) => v === "").length;
    console.log(`${lang}: 共 ${keys.size} 键，新增 ${added}，删除 ${removed}，待翻译 ${missing}`);
  }
}

function cmdCheck(strict) {
  const { keys, dynamic, hardcoded } = scanAll();
  const sourceKeys = new Set(keys.keys());
  let failed = false;
  const fail = (msg) => {
    failed = true;
    console.error(`错误: ${msg}`);
  };
  for (const d of dynamic) {
    const msg = `动态键 ${d.file}:${d.line}  ${d.context}`;
    if (strict) fail(msg);
    else console.warn(`警告: ${msg}`);
  }
  for (const { lang, path } of listCatalogs()) {
    let cat;
    try {
      cat = readCatalog(path);
    } catch (e) {
      fail(e.message);
      continue;
    }
    const { errors, missing, orphaned } = validateCatalog(lang, cat, sourceKeys);
    for (const e of errors) fail(e);
    if (orphaned.length > 0) {
      const msg = `[${lang}] ${orphaned.length} 个键源码里已不存在（跑 sync 清掉）`;
      if (strict) fail(msg);
      else console.warn(`警告: ${msg}`);
    }
    const msg = `[${lang}] ${keys.size} 键，待翻译 ${missing.length}`;
    if (strict && missing.length > 0) fail(msg);
    else console.log(msg);
  }
  const hc = `没包进 t/tx 的中文字面量 ${hardcoded.length} 处（hardcoded 子命令列出）`;
  if (strict && hardcoded.length > 0) fail(hc);
  else console.log(hc);
  if (failed) process.exit(1);
}

function cmdReport(lang, onlyMissing, json) {
  const { keys } = scanAll();
  let filter = () => true;
  if (onlyMissing) {
    const path = join(LOCALES, `${lang}.json`);
    const cat = readCatalog(path);
    filter = (k) => !(k in cat) || cat[k] === "";
  }
  const rows = sortKeys(keys.keys())
    .filter(filter)
    .map((k) => ({ key: k, refs: keys.get(k) }));
  if (json) {
    console.log(JSON.stringify(rows, null, 2));
    return;
  }
  for (const r of rows) {
    console.log(JSON.stringify(r.key));
    for (const ref of r.refs) console.log(`    ${ref.file}:${ref.line}  ${ref.context}`);
  }
}

function cmdHardcoded(json) {
  const { hardcoded } = scanAll();
  if (json) {
    console.log(JSON.stringify(hardcoded, null, 2));
    return;
  }
  for (const h of hardcoded) console.log(`${h.file}:${h.line}  ${h.context}`);
  console.log(`共 ${hardcoded.length} 处`);
}

const [cmd, ...rest] = process.argv.slice(2);
const flag = (name) => rest.includes(name);
const opt = (name, dflt) => {
  const i = rest.indexOf(name);
  return i >= 0 && rest[i + 1] !== undefined ? rest[i + 1] : dflt;
};
switch (cmd) {
  case "sync":
    cmdSync();
    break;
  case "check":
    cmdCheck(flag("--strict"));
    break;
  case "report":
    cmdReport(opt("--lang", "en"), flag("--missing"), flag("--json"));
    break;
  case "hardcoded":
    cmdHardcoded(flag("--json"));
    break;
  default:
    console.error("用法: node scripts/i18n.mjs <sync|check [--strict]|report [--lang L] [--missing] [--json]|hardcoded [--json]>");
    process.exit(2);
}
