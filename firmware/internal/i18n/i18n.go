// Package i18n 把管理面回给界面的人类可读文案按 Accept-Language 本地化。
//
// **简体中文是源文本，也是查表的键。** 固件代码里的错误信息、探测摘要、审计
// 说明照常用中文写（errors.New / fmt.Errorf / 拼接都行，不需要改任何调用点）；
// 目录 locales/<lang>.json 的形状恒为 { "中文原文": "译文" }，键由
// tools/i18nextract 从 Go 源码按 AST 抽出来——fmt 格式串原样做键（"固件包超过
// %d 字节上限"），字符串拼接折成 %s 模板（"名称不合法：%s"）。运行时把已经渲染
// 好的消息反向对上这些模板：整句精确命中 → 带动词的键当模式匹配、捕获组递归
// 本地化（%w 链里的内层错误因此也能翻）→ 按 ": " / "：" 切段逐段翻。哪一步都
// 对不上就原样返回中文，从不吐半截。
//
// 只有 Source（zh-CN）以外的语言需要目录；缺条目、条目为空串都视为「还没翻」，
// 显示中文。目录内容由 /i18n 技能定期补齐，固件构建不因缺译失败。
//
// 数据面（/v1、/agents 等给程序看的 API）不经本包；只有管理面
// internal/admin 的 writeError / writeJSON 走它。
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Lang 是 BCP 47 语言标签（与界面 lib/i18n.tsx 的 LANGS 同一组值）。
type Lang string

const (
	// Source 是源语言：代码里写的就是它，永远不需要目录。
	Source Lang = "zh-CN"
	// English 是英文。
	English Lang = "en"
	// Japanese 是日语。
	Japanese Lang = "ja"
)

// TextTag 是响应结构体上标记「这个字段是给人看的文案」的 struct tag：
// `i18n:"text"`。Localize 只翻带这个标记的 string / []string 字段，其余字段
// （模型名、标签、路径……）一概不动。
const TextTag = "i18n"

//go:embed locales/*.json
var localeFS embed.FS

var (
	loadOnce sync.Once
	catalogs map[Lang]*catalog
)

// Supported 列出源语言与全部带目录的语言，源语言在首位。
func Supported() []Lang {
	load()
	out := []Lang{Source}
	var rest []string
	for l := range catalogs {
		rest = append(rest, string(l))
	}
	sort.Strings(rest)
	for _, l := range rest {
		out = append(out, Lang(l))
	}
	return out
}

func load() {
	loadOnce.Do(func() {
		catalogs = map[Lang]*catalog{}
		entries, err := fs.ReadDir(localeFS, "locales")
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			raw, err := localeFS.ReadFile(path.Join("locales", name))
			if err != nil {
				panic(fmt.Sprintf("i18n: read %s: %v", name, err))
			}
			var m map[string]string
			if err := json.Unmarshal(raw, &m); err != nil {
				panic(fmt.Sprintf("i18n: parse %s: %v", name, err))
			}
			lang := Lang(strings.TrimSuffix(name, ".json"))
			if lang == Source {
				continue
			}
			catalogs[lang] = newCatalog(m)
		}
	})
}

func catalogFor(lang Lang) *catalog {
	if lang == Source || lang == "" {
		return nil
	}
	load()
	return catalogs[lang]
}

// Negotiate 从 Accept-Language 头挑出一种受支持的语言：按 q 值降序，先整标签
// 精确匹配再按主语言子标签（en-US → en）匹配；没有头、都不认识或 q=0 都落回
// Source。
func Negotiate(header string) Lang {
	header = strings.TrimSpace(header)
	if header == "" {
		return Source
	}
	type pref struct {
		tag string
		q   float64
		idx int
	}
	var prefs []pref
	for i, part := range strings.Split(header, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		tag := strings.ToLower(strings.TrimSpace(fields[0]))
		if tag == "" {
			continue
		}
		q := 1.0
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			if strings.HasPrefix(f, "q=") {
				if v, err := strconv.ParseFloat(f[2:], 64); err == nil {
					q = v
				}
			}
		}
		if q <= 0 {
			continue
		}
		prefs = append(prefs, pref{tag: tag, q: q, idx: i})
	}
	sort.SliceStable(prefs, func(i, j int) bool { return prefs[i].q > prefs[j].q })
	supported := Supported()
	for _, p := range prefs {
		if p.tag == "*" {
			return Source
		}
		for _, l := range supported {
			if strings.ToLower(string(l)) == p.tag {
				return l
			}
		}
		primary := strings.SplitN(p.tag, "-", 2)[0]
		for _, l := range supported {
			if strings.SplitN(strings.ToLower(string(l)), "-", 2)[0] == primary {
				return l
			}
		}
	}
	return Source
}

// T 把一条已经渲染好的中文消息翻成 lang；翻不了原样返回。
func T(lang Lang, msg string) string {
	c := catalogFor(lang)
	if c == nil || msg == "" {
		return msg
	}
	return c.translate(msg, 0)
}

// Localize 返回 v 的一份副本，其中所有带 `i18n:"text"` 标记的字符串字段都翻成
// lang；没有任何字段变化时原样返回 v（不复制）。指针 / 切片 / map / 接口 /
// 嵌套结构体都会递归进去；不带标记的字段与非导出字段一律不碰。
func Localize(lang Lang, v any) any {
	c := catalogFor(lang)
	if c == nil || v == nil {
		return v
	}
	out, changed := c.walk(reflect.ValueOf(v))
	if !changed {
		return v
	}
	return out.Interface()
}

// ---- 目录 ----

type catalog struct {
	exact    map[string]string
	patterns []*pattern
}

// pattern 是带 fmt 动词的键：中文格式串编成正则去对已渲染的消息，捕获组按
// 参数序号回填进译文模板。
type pattern struct {
	prefix string         // 首个动词之前的字面文本，用来快速排除
	re     *regexp.Regexp // 整句锚定
	args   []int          // 第 i 个捕获组对应的参数序号（从 1 起）
	nargs  int
	tmpl   string // 译文，动词统一改写成 %[n]s
	keyLen int
}

func newCatalog(m map[string]string) *catalog {
	c := &catalog{exact: map[string]string{}}
	for k, v := range m {
		if v == "" {
			continue
		}
		c.exact[k] = v
		if p := compilePattern(k, v); p != nil {
			c.patterns = append(c.patterns, p)
		}
	}
	// 更具体（字面前缀更长、整键更长）的模式先试。
	sort.SliceStable(c.patterns, func(i, j int) bool {
		a, b := c.patterns[i], c.patterns[j]
		if len(a.prefix) != len(b.prefix) {
			return len(a.prefix) > len(b.prefix)
		}
		return a.keyLen > b.keyLen
	})
	return c
}

const maxDepth = 6

func (c *catalog) translate(msg string, depth int) string {
	if depth > maxDepth || msg == "" {
		return msg
	}
	if v, ok := c.exact[msg]; ok {
		return v
	}
	for _, p := range c.patterns {
		if !strings.HasPrefix(msg, p.prefix) {
			continue
		}
		m := p.re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		args := make([]any, p.nargs)
		for i := range args {
			args[i] = ""
		}
		for i, idx := range p.args {
			args[idx-1] = c.translate(m[i+1], depth+1)
		}
		return fmt.Sprintf(p.tmpl, args...)
	}
	for _, sep := range []string{": ", "："} {
		parts := strings.Split(msg, sep)
		if len(parts) < 2 {
			continue
		}
		changed := false
		for i, part := range parts {
			out := c.translate(part, depth+1)
			if out != part {
				changed = true
				parts[i] = out
			}
		}
		if changed {
			return strings.Join(parts, sep)
		}
	}
	return msg
}

// ---- fmt 动词 ----

// verbRE 匹配一个 fmt 动词：可选 [n] 参数序号、旗标、宽度、精度、动词字母；%% 也算。
var verbRE = regexp.MustCompile(`%(?:\[(\d+)\])?([-+# 0]*)(\d+|\*)?(?:\.(\d+|\*)?)?([a-zA-Z%])`)

// Verb 是格式串里的一个动词。
type Verb struct {
	Index  int    // 参数序号（从 1 起）；%% 为 0
	Letter string // 动词字母；%% 为 "%"
	Start  int
	End    int
}

// Verbs 按出现顺序列出 format 里的动词，隐式序号按 fmt 规则递增（显式 %[n]
// 之后的下一个隐式序号是 n+1）。
func Verbs(format string) []Verb {
	var out []Verb
	next := 1
	for _, m := range verbRE.FindAllStringSubmatchIndex(format, -1) {
		letter := format[m[10]:m[11]]
		v := Verb{Letter: letter, Start: m[0], End: m[1]}
		if letter == "%" {
			out = append(out, v)
			continue
		}
		if m[2] >= 0 {
			n, _ := strconv.Atoi(format[m[2]:m[3]])
			v.Index = n
			next = n + 1
		} else {
			v.Index = next
			next++
		}
		out = append(out, v)
	}
	return out
}

// Signature 是译文与中文必须一致的动词指纹：按参数序号排序的动词字母序列。
// 显式序号让译文可以调换语序，但每个参数用什么动词不能变。
func Signature(format string) string {
	verbs := Verbs(format)
	byIdx := map[int]string{}
	max := 0
	for _, v := range verbs {
		if v.Letter == "%" {
			continue
		}
		byIdx[v.Index] = v.Letter
		if v.Index > max {
			max = v.Index
		}
	}
	var sb strings.Builder
	for i := 1; i <= max; i++ {
		sb.WriteString("%")
		if l, ok := byIdx[i]; ok {
			sb.WriteString(l)
		} else {
			sb.WriteString("?")
		}
	}
	return sb.String()
}

func compilePattern(key, value string) *pattern {
	verbs := Verbs(key)
	hasArg := false
	for _, v := range verbs {
		if v.Letter != "%" {
			hasArg = true
			break
		}
	}
	if !hasArg {
		return nil
	}
	var re strings.Builder
	re.WriteString("^")
	p := &pattern{keyLen: len(key)}
	last := 0
	prefixDone := false
	for _, v := range verbs {
		lit := key[last:v.Start]
		if !prefixDone {
			p.prefix += lit
		}
		re.WriteString(regexp.QuoteMeta(lit))
		last = v.End
		if v.Letter == "%" {
			if !prefixDone {
				p.prefix += "%"
			}
			re.WriteString("%")
			continue
		}
		prefixDone = true
		switch v.Letter {
		case "d":
			re.WriteString(`([-+]?\d+)`)
		case "t":
			re.WriteString(`(true|false)`)
		default:
			re.WriteString(`(.*?)`)
		}
		p.args = append(p.args, v.Index)
		if v.Index > p.nargs {
			p.nargs = v.Index
		}
	}
	re.WriteString(regexp.QuoteMeta(key[last:]))
	re.WriteString("$")
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return nil
	}
	p.re = compiled
	p.tmpl = normalizeTemplate(value)
	return p
}

// normalizeTemplate 把译文里的动词统一改写成 %[n]s：捕获到的是已渲染的文本，
// 用 %s 原样放回即可，%d 之类反而会印出 %!d(string=…)。
func normalizeTemplate(value string) string {
	verbs := Verbs(value)
	var sb strings.Builder
	last := 0
	for _, v := range verbs {
		sb.WriteString(value[last:v.Start])
		last = v.End
		if v.Letter == "%" {
			sb.WriteString("%%")
			continue
		}
		sb.WriteString("%[")
		sb.WriteString(strconv.Itoa(v.Index))
		sb.WriteString("]s")
	}
	sb.WriteString(value[last:])
	return sb.String()
}

// ---- 结构体遍历 ----

func (c *catalog) walk(v reflect.Value) (reflect.Value, bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v, false
		}
		elem, changed := c.walk(v.Elem())
		if !changed {
			return v, false
		}
		p := reflect.New(v.Type().Elem())
		p.Elem().Set(elem)
		return p, true
	case reflect.Interface:
		if v.IsNil() {
			return v, false
		}
		inner, changed := c.walk(v.Elem())
		if !changed {
			return v, false
		}
		iv := reflect.New(v.Type()).Elem()
		iv.Set(inner)
		return iv, true
	case reflect.Struct:
		t := v.Type()
		var cp reflect.Value
		changed := false
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fv := v.Field(i)
			var nv reflect.Value
			var ch bool
			if _, tagged := f.Tag.Lookup(TextTag); tagged {
				nv, ch = c.walkText(fv)
			} else {
				nv, ch = c.walk(fv)
			}
			if !ch {
				continue
			}
			if !changed {
				cp = reflect.New(t).Elem()
				cp.Set(v)
				changed = true
			}
			cp.Field(i).Set(nv)
		}
		if !changed {
			return v, false
		}
		return cp, true
	case reflect.Slice:
		if v.IsNil() || !container(v.Type().Elem().Kind()) {
			return v, false
		}
		var cp reflect.Value
		changed := false
		for i := 0; i < v.Len(); i++ {
			nv, ch := c.walk(v.Index(i))
			if !ch {
				continue
			}
			if !changed {
				cp = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
				reflect.Copy(cp, v)
				changed = true
			}
			cp.Index(i).Set(nv)
		}
		if !changed {
			return v, false
		}
		return cp, true
	case reflect.Map:
		if v.IsNil() || !container(v.Type().Elem().Kind()) {
			return v, false
		}
		var cp reflect.Value
		changed := false
		iter := v.MapRange()
		for iter.Next() {
			nv, ch := c.walk(iter.Value())
			if !ch {
				continue
			}
			if !changed {
				cp = reflect.MakeMapWithSize(v.Type(), v.Len())
				it2 := v.MapRange()
				for it2.Next() {
					cp.SetMapIndex(it2.Key(), it2.Value())
				}
				changed = true
			}
			cp.SetMapIndex(iter.Key(), nv)
		}
		if !changed {
			return v, false
		}
		return cp, true
	}
	return v, false
}

// container 说明这一类型的元素里可能藏着带标记的结构体，值得递归。
func container(k reflect.Kind) bool {
	switch k {
	case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Slice, reflect.Map:
		return true
	}
	return false
}

func (c *catalog) walkText(v reflect.Value) (reflect.Value, bool) {
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		out := c.translate(s, 0)
		if out == s {
			return v, false
		}
		nv := reflect.New(v.Type()).Elem()
		nv.SetString(out)
		return nv, true
	case reflect.Pointer:
		if v.IsNil() {
			return v, false
		}
		elem, changed := c.walkText(v.Elem())
		if !changed {
			return v, false
		}
		p := reflect.New(v.Type().Elem())
		p.Elem().Set(elem)
		return p, true
	case reflect.Slice:
		if v.IsNil() || v.Type().Elem().Kind() != reflect.String {
			return v, false
		}
		var cp reflect.Value
		changed := false
		for i := 0; i < v.Len(); i++ {
			nv, ch := c.walkText(v.Index(i))
			if !ch {
				continue
			}
			if !changed {
				cp = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
				reflect.Copy(cp, v)
				changed = true
			}
			cp.Index(i).Set(nv)
		}
		if !changed {
			return v, false
		}
		return cp, true
	}
	return v, false
}
