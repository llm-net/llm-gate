// i18nextract 从固件 Go 源码里抽出可能回给管理界面的中文文案，与
// internal/i18n/locales/<lang>.json 对账。设计见 internal/i18n 包注释。
//
//	go run ./tools/i18nextract            只统计
//	go run ./tools/i18nextract -write     目录对账：缺的键补成 ""，多余的删掉，按键排序
//	go run ./tools/i18nextract -check     校验动词指纹；-strict 时缺译 / 多余键也失败
//	go run ./tools/i18nextract -report [-missing] [-lang en] [-json]
//	                                      键 → 出处（给翻译看上下文）
//
// 抽取规则（宁可多收、不可漏收——多收的条目只是多翻一次，漏收的在界面上就是中文）：
//   - 非测试 .go 文件里每一个含中文的字符串字面量都是键；fmt 格式串原样做键。
//   - `+` 拼接链里只要有一个中文字面量，整条链折成一个 %s 模板做键
//     （"名称不合法：" + err.Error() → "名称不合法：%s"），链内字面量不再单独收。
//   - 日志调用（Debug/Info/Warn/Error/… 及其参数）、panic、flag 用法说明与
//     struct tag 不收：那些不回给界面。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/llm-net/llm-gate/firmware/internal/i18n"
)

var logMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
	"Log": true, "LogAttrs": true, "With": true, "WithGroup": true,
	"Fatal": true, "Fatalf": true, "Fatalln": true,
	"Print": true, "Printf": true, "Println": true,
	"Panic": true, "Panicf": true, "Panicln": true,
	"StringVar": true, "BoolVar": true, "IntVar": true, "Int64Var": true,
	"DurationVar": true, "Float64Var": true, "Var": true, "Func": true,
}

var skipReceivers = map[string]bool{"flag": true, "fs": true, "flags": true, "slog": true}

type ref struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

type row struct {
	Key  string `json:"key"`
	Refs []ref  `json:"refs"`
}

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// extractor 收集一份文件的键。
type extractor struct {
	fset *token.FileSet
	rel  string
	keys map[string][]ref
}

func (x *extractor) add(key string, pos token.Pos) {
	p := x.fset.Position(pos)
	x.keys[key] = append(x.keys[key], ref{File: x.rel, Line: p.Line})
}

func litString(n ast.Node) (string, bool) {
	bl, ok := n.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// flattenConcat 把 a + b + c 展平；非 + 的表达式原样返回单元素。
func flattenConcat(e ast.Expr, out *[]ast.Expr) {
	if be, ok := e.(*ast.BinaryExpr); ok && be.Op == token.ADD {
		flattenConcat(be.X, out)
		flattenConcat(be.Y, out)
		return
	}
	if pe, ok := e.(*ast.ParenExpr); ok {
		flattenConcat(pe.X, out)
		return
	}
	*out = append(*out, e)
}

func skipCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		if logMethods[fn.Sel.Name] {
			return true
		}
		if id, ok := fn.X.(*ast.Ident); ok && skipReceivers[id.Name] {
			return true
		}
	case *ast.Ident:
		if fn.Name == "panic" {
			return true
		}
	}
	return false
}

func (x *extractor) visit(n ast.Node) bool {
	switch v := n.(type) {
	case *ast.ImportSpec:
		return false
	case *ast.Field:
		// 结构体 tag 不收，其余部分照走。
		if v.Tag != nil {
			for _, name := range v.Names {
				ast.Inspect(name, x.visit)
			}
			ast.Inspect(v.Type, x.visit)
			return false
		}
	case *ast.CallExpr:
		if skipCall(v) {
			return false
		}
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return true
		}
		var parts []ast.Expr
		flattenConcat(v, &parts)
		cjk := false
		for _, p := range parts {
			if s, ok := litString(p); ok && hasCJK(s) {
				cjk = true
				break
			}
		}
		if !cjk {
			return true
		}
		// 全是字面量的拼接就是一个常量（多半是折行的格式串），原样连起来；
		// 混有表达式的才是运行时拼接，字面量里的 % 是字面 %，表达式位置补 %s。
		allLit := true
		for _, p := range parts {
			if _, ok := litString(p); !ok {
				allLit = false
				break
			}
		}
		var sb strings.Builder
		for _, p := range parts {
			if s, ok := litString(p); ok {
				if allLit {
					sb.WriteString(s)
				} else {
					sb.WriteString(strings.ReplaceAll(s, "%", "%%"))
				}
			} else {
				sb.WriteString("%s")
				// 非字面量里可能还藏着别的调用，照常往里走。
				ast.Inspect(p, x.visit)
			}
		}
		x.add(sb.String(), v.Pos())
		return false
	case *ast.BasicLit:
		if s, ok := litString(v); ok && hasCJK(s) {
			x.add(s, v.Pos())
		}
	}
	return true
}

func scan(root string) (map[string][]ref, error) {
	keys := map[string][]ref{}
	fset := token.NewFileSet()
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "uidist" || d.Name() == "assets" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			x := &extractor{fset: fset, rel: filepath.ToSlash(rel), keys: keys}
			ast.Inspect(f, x.visit)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func readCatalog(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

func writeCatalog(path string, m map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

func main() {
	root := flag.String("root", ".", "固件模块根目录（含 internal/ 与 cmd/）")
	write := flag.Bool("write", false, "把对账结果写回目录文件")
	check := flag.Bool("check", false, "校验目录；发现问题退出码非 0")
	strict := flag.Bool("strict", false, "配合 -check：缺译与多余键也算失败")
	report := flag.Bool("report", false, "打印键与出处")
	missing := flag.Bool("missing", false, "配合 -report：只列缺译的键")
	lang := flag.String("lang", "en", "配合 -report -missing：目标语言")
	asJSON := flag.Bool("json", false, "配合 -report：JSON 输出")
	flag.Parse()

	keys, err := scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "扫描失败:", err)
		os.Exit(1)
	}
	localeDir := filepath.Join(*root, "internal", "i18n", "locales")
	files, _ := filepath.Glob(filepath.Join(localeDir, "*.json"))
	sort.Strings(files)

	if *report {
		var filter func(string) bool = func(string) bool { return true }
		if *missing {
			cat, err := readCatalog(filepath.Join(localeDir, *lang+".json"))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			filter = func(k string) bool { return cat[k] == "" }
		}
		var rows []row
		for _, k := range sortedKeys(keys) {
			if filter(k) {
				rows = append(rows, row{Key: k, Refs: keys[k]})
			}
		}
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			enc.Encode(rows)
			return
		}
		for _, r := range rows {
			fmt.Printf("%q\n", r.Key)
			for _, ref := range r.Refs {
				fmt.Printf("    %s:%d\n", ref.File, ref.Line)
			}
		}
		return
	}

	failed := false
	fail := func(format string, args ...any) {
		failed = true
		fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	}
	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		if name == string(i18n.Source) {
			continue
		}
		cat, err := readCatalog(path)
		if err != nil {
			fail("%v", err)
			continue
		}
		if *write {
			next := map[string]string{}
			added, removed := 0, 0
			for k := range keys {
				if _, ok := cat[k]; !ok {
					added++
				}
				next[k] = cat[k]
			}
			for k := range cat {
				if _, ok := keys[k]; !ok {
					removed++
				}
			}
			if err := writeCatalog(path, next); err != nil {
				fail("%v", err)
				continue
			}
			cat = next
			fmt.Printf("%s: 新增 %d，删除 %d\n", name, added, removed)
		}
		untranslated, orphaned := 0, 0
		for k, v := range cat {
			if _, ok := keys[k]; !ok {
				orphaned++
				continue
			}
			if v == "" {
				untranslated++
				continue
			}
			if *check && i18n.Signature(k) != i18n.Signature(v) {
				fail("[%s] %q → %q：fmt 动词与中文不一致（%s vs %s）", name, k, v, i18n.Signature(k), i18n.Signature(v))
			}
		}
		for k := range keys {
			if _, ok := cat[k]; !ok {
				untranslated++
			}
		}
		fmt.Printf("%s: 共 %d 键，待翻译 %d，多余 %d\n", name, len(keys), untranslated, orphaned)
		if *check && *strict {
			if untranslated > 0 {
				fail("[%s] 还有 %d 条没翻", name, untranslated)
			}
			if orphaned > 0 {
				fail("[%s] 有 %d 个键源码里已不存在（跑 -write 清掉）", name, orphaned)
			}
		}
	}
	if failed {
		os.Exit(1)
	}
}
