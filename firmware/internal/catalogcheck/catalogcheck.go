// Package catalogcheck 是目录数据源的发布前校验：official-pricing.json（官方目录价）
// 与 platform-models.json（平台模型信息）两份文件在被复制到官网与固件内嵌副本
// 之前，必须整体通过这里的检查。
//
// 为什么在设备解析之外再设一道：设备侧刻意**宽松**——取回的价目文件只查形态标识
// 与条目数，分时段价原样落库不校验；平台清单只做结构校验，逐条合法性到建模那
// 一步才判。宽松是对的（一份写坏的远端文件不该停掉盒子），但发布侧因此需要一道
// **严格**的闸：未知字段、缺出处、时段重叠、订阅型号没录价、三份副本不一致，
// 这些错在发布前抓比在几百台设备上各自表现成一个小症状便宜得多。
//
// 词汇只此一份：价格字段集用 usage.FieldsFor，成对字段用 usage.CheckPricingPairs，
// 分时段价用 usage.ParseSchedule，平台结构用 platformcatalog.Parse——本包只把它们
// 串起来，加上设备不检查、但发布必须遵守的纪律（出处、日期、排序、规范格式）。
//
// 两个消费方：tools/catalogcheck（维护者与维护文档指引的开发工具在维护源目录
// 执行）与本包测试（把守仓库里的两份副本）。
package catalogcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// 两份文件在维护源目录里的文件名（与官网 /updates/data/ 下的同名）。
const (
	OfficialPricingFile = "official-pricing.json"
	PlatformModelsFile  = "platform-models.json"
)

// 模型名上限（与 internal/admin 建模校验的 catalogNameMaxRunes 同值）。
const nameMaxRunes = 128

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Report 是一次校验的结果。Errors 非空即不通过；Infos 是不阻塞发布的提示
// （如价目文件里有目录尚未收录的订阅型号）。
type Report struct {
	Errors []string
	Infos  []string
}

// OK 报告是否通过。
func (r *Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) errorf(file, format string, args ...any) {
	r.Errors = append(r.Errors, file+": "+fmt.Sprintf(format, args...))
}

func (r *Report) infof(file, format string, args ...any) {
	r.Infos = append(r.Infos, file+": "+fmt.Sprintf(format, args...))
}

// Format 把一份文件重排成规范格式：2 空格缩进、每个数组元素与对象成员各占一行、
// 键序与数值原样、末尾一个换行。三份副本要求逐字节一致，规范格式让「同一份内容」
// 只有一种字节形态，不同工具改出来的文件才比得了。
func Format(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(raw), "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// CheckDir 读维护源目录里的两份文件并校验。
func CheckDir(dir string) (*Report, error) {
	pricing, err := os.ReadFile(filepath.Join(dir, OfficialPricingFile))
	if err != nil {
		return nil, err
	}
	platform, err := os.ReadFile(filepath.Join(dir, PlatformModelsFile))
	if err != nil {
		return nil, err
	}
	return Check(pricing, platform), nil
}

// Check 校验两份文件的原文。
func Check(pricingRaw, platformRaw []byte) *Report {
	r := &Report{}
	pricedAgent := checkOfficialPricing(r, pricingRaw)
	agentModels := checkPlatformModels(r, platformRaw)
	crossCheck(r, pricedAgent, agentModels)
	return r
}

// ---- 官方目录价 ----

var (
	pricingTopKeys   = keySet("schema", "version", "updated_at", "currency", "unit", "notes", "models")
	pricingModelKeys = keySet("name", "kind", "vendor", "agent", "pricing", "schedule", "source", "checked_at", "note")
	scheduleKeys     = keySet("timezone", "periods")
	periodKeys       = keySet("label", "days", "hours", "pricing")
	knownKinds       = keySet("text", "video", "image")
	agentValues      = keySet("codex", "grok", "claude")
	kindFamilies     = map[string][]string{"video": {"ark_video", "minimax_video"}, "image": {"ark_image"}}
)

// checkOfficialPricing 返回「带 agent 标记的文本型号名 → agent」供交叉检查。
func checkOfficialPricing(r *Report, raw []byte) map[string]string {
	const file = OfficialPricingFile
	pricedAgent := map[string]string{}
	checkFormat(r, file, raw)
	if len(raw) > officialsite.OfficialPricingMaxBytes {
		r.errorf(file, "文件 %d 字节，超过设备下载上限 %d", len(raw), officialsite.OfficialPricingMaxBytes)
	}
	var doc officialsite.OfficialPricing
	if err := json.Unmarshal(raw, &doc); err != nil {
		r.errorf(file, "不是合法 JSON：%v", err)
		return pricedAgent
	}
	if doc.Schema != officialsite.OfficialPricingSchema {
		r.errorf(file, "schema 必须是 %q", officialsite.OfficialPricingSchema)
	}
	if doc.Version <= 0 {
		r.errorf(file, "version 必须是正整数")
	}
	if !dateRE.MatchString(doc.UpdatedAt) {
		r.errorf(file, "updated_at 必须是 YYYY-MM-DD")
	}
	if doc.Currency != "CNY" || doc.Unit != "micro_yuan" {
		r.errorf(file, "currency / unit 必须是 CNY / micro_yuan（全链路整数微元）")
	}
	if len(doc.Models) == 0 {
		r.errorf(file, "一条模型价目都没有")
	}
	if len(doc.Models) > officialsite.OfficialPricingMaxModels {
		r.errorf(file, "条目数 %d 超过设备上限 %d", len(doc.Models), officialsite.OfficialPricingMaxModels)
	}
	// 未知键：设备解析会静默忽略，发布侧必须当错误——拼错的 schedule 会变成
	// 「文件里写了、设备永远读不到」。
	var shape struct {
		Rest   map[string]json.RawMessage `json:"-"`
		Models []map[string]json.RawMessage
	}
	top := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &top); err == nil {
		reportUnknownKeys(r, file, "顶层", top, pricingTopKeys)
		_ = json.Unmarshal(top["models"], &shape.Models)
	}
	seen := map[string]int{}
	for i, m := range doc.Models {
		where := fmt.Sprintf("models[%d]（%s）", i, clip(m.Name))
		if i < len(shape.Models) {
			reportUnknownKeys(r, file, where, shape.Models[i], pricingModelKeys)
		}
		if err := checkName(m.Name); err != nil {
			r.errorf(file, "%s 的 name %s", where, err)
		}
		key := strings.ToLower(m.Name)
		if prev, dup := seen[key]; dup {
			r.errorf(file, "%s 与 models[%d] 重名（设备按名不分大小写匹配，重名整批拒收）", where, prev)
		}
		seen[key] = i
		if !knownKinds[m.Kind] {
			r.errorf(file, "%s 的 kind 必须是 text | video | image", where)
			continue
		}
		if m.Vendor == "" {
			r.errorf(file, "%s 缺 vendor", where)
		}
		if m.Agent != "" {
			if !agentValues[m.Agent] {
				r.errorf(file, "%s 的 agent 只认 codex | grok | claude", where)
			} else if m.Kind != "text" {
				r.errorf(file, "%s 带 agent 标记但不是文本模型（订阅内含的图片/视频恒记 0 元，不录价）", where)
			} else {
				pricedAgent[m.Name] = m.Agent
			}
		}
		checkProvenance(r, file, where, m.Source, m.CheckedAt)
		pricing, err := usage.ParsePricing(string(m.Pricing))
		if err != nil {
			r.errorf(file, "%s 的 pricing 不合法：%v", where, err)
			continue
		}
		if !pricing.Priced() {
			r.errorf(file, "%s 没有 pricing（未定价的模型不进价目文件）", where)
			continue
		}
		allowed := keySet(usage.FieldsFor(m.Kind)...)
		for _, f := range sortedKeys(pricing) {
			if !allowed[f] {
				r.errorf(file, "%s 的 pricing 含 %s 模型不认的字段 %q（可配：%s）", where, m.Kind, f, strings.Join(usage.FieldsFor(m.Kind), "、"))
			}
		}
		if err := usage.CheckPricingPairs(pricing); err != nil {
			r.errorf(file, "%s：%v", where, err)
		}
		if len(bytes.TrimSpace(m.Schedule)) > 0 {
			checkScheduleShape(r, file, where, m.Schedule)
			if _, err := usage.ParseSchedule(m.Schedule, pricing); err != nil {
				r.errorf(file, "%s 的 schedule 不合法：%v", where, err)
			}
		}
	}
	return pricedAgent
}

// checkScheduleShape 只查未知键；语义校验在 usage.ParseSchedule。
func checkScheduleShape(r *Report, file, where string, raw json.RawMessage) {
	var s map[string]json.RawMessage
	if err := json.Unmarshal(raw, &s); err != nil {
		return // ParseSchedule 会报
	}
	reportUnknownKeys(r, file, where+".schedule", s, scheduleKeys)
	var periods []map[string]json.RawMessage
	if err := json.Unmarshal(s["periods"], &periods); err != nil {
		return
	}
	for i, p := range periods {
		reportUnknownKeys(r, file, fmt.Sprintf("%s.schedule.periods[%d]", where, i), p, periodKeys)
	}
}

// ---- 平台模型信息 ----

var (
	platformTopKeys      = keySet("schema", "version", "updated_at", "notes", "platforms", "agents")
	platformKeys         = keySet("id", "type", "vendor", "base_url", "custom_base_url", "billing_mode", "suggested_name", "entries", "ability", "source", "checked_at", "note", "models")
	platformModelKeys    = keySet("name", "kind", "family", "upstream_model_id", "note", "capabilities", "upstream_protocols")
	capabilityKeys       = keySet("responses_chat", "codex")
	responsesChatKeys    = keySet("profile", "replay_reasoning_content", "thinking_type", "drop_tool_choice", "effort_map")
	codexCapabilityKeys  = keySet("default_reasoning_level", "supported_reasoning_levels")
	reasoningLevelKeys   = keySet("effort", "description")
	agentKeys            = keySet("provider", "vendor", "source", "checked_at", "note", "models")
	agentModelKeys       = keySet("name", "kind", "note", "source", "checked_at", "pricing")
	billingRank          = map[string]int{platformcatalog.BillingUsage: 0, platformcatalog.BillingSubscription: 1, platformcatalog.BillingNone: 2}
	billingCategoryLabel = map[string]string{platformcatalog.BillingUsage: "API 按量计费", platformcatalog.BillingSubscription: "API 订阅套餐", platformcatalog.BillingNone: "通用兼容适配"}
)

// agentModel 是 agents 段里的一条文本型号（交叉检查用）。
type agentModel struct {
	Provider string
	Name     string
}

func checkPlatformModels(r *Report, raw []byte) []agentModel {
	const file = PlatformModelsFile
	checkFormat(r, file, raw)
	if len(raw) > platformcatalog.MaxBytes {
		r.errorf(file, "文件 %d 字节，超过设备下载上限 %d", len(raw), platformcatalog.MaxBytes)
	}
	doc, err := platformcatalog.Parse(raw)
	if err != nil {
		r.errorf(file, "设备解析失败：%v", err)
		return nil
	}
	if !dateRE.MatchString(doc.UpdatedAt) {
		r.errorf(file, "updated_at 必须是 YYYY-MM-DD")
	}
	// 未知键扫描（结构解析忽略未知字段，这里补上）。
	var shape struct {
		Platforms []map[string]json.RawMessage `json:"platforms"`
		Agents    []map[string]json.RawMessage `json:"agents"`
	}
	top := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &top) == nil {
		reportUnknownKeys(r, file, "顶层", top, platformTopKeys)
		_ = json.Unmarshal(raw, &shape)
	}
	// 三类平台按 按量 → 套餐 → 通用适配 排列：platforms 段的先后就是管理台各组内
	// 的展示顺序，混排会让同一组的平台在文件里散落、维护时漏看。
	lastRank := -1
	for i, p := range doc.Platforms {
		where := fmt.Sprintf("platforms[%d]（%s）", i, clip(p.ID))
		if i < len(shape.Platforms) {
			reportUnknownKeys(r, file, where, shape.Platforms[i], platformKeys)
			var models []map[string]json.RawMessage
			_ = json.Unmarshal(shape.Platforms[i]["models"], &models)
			for j, m := range models {
				mw := fmt.Sprintf("%s.models[%d]", where, j)
				reportUnknownKeys(r, file, mw, m, platformModelKeys)
				checkCapabilityShape(r, file, mw, m["capabilities"])
			}
		}
		if rank := billingRank[p.BillingMode]; rank < lastRank {
			r.errorf(file, "%s 的 %s 排在了后一类平台之后；platforms 段必须按 API 按量计费 → API 订阅套餐 → 通用兼容适配 排列", where, billingCategoryLabel[p.BillingMode])
		} else {
			lastRank = rank
		}
		if p.Vendor == "" {
			r.errorf(file, "%s 缺 vendor", where)
		}
		checkProvenance(r, file, where, p.Source, p.CheckedAt)
		if len(p.Models) == 0 && p.BillingMode != platformcatalog.BillingNone {
			r.errorf(file, "%s 的模型选单为空", where)
		}
		for j, m := range p.Models {
			mw := fmt.Sprintf("%s.models[%d]（%s）", where, j, clip(m.Name))
			if err := checkName(m.Name); err != nil {
				r.errorf(file, "%s 的 name %s", mw, err)
			}
			if m.UpstreamModelID != "" {
				if err := checkName(m.UpstreamModelID); err != nil {
					r.errorf(file, "%s 的 upstream_model_id %s", mw, err)
				}
			}
			if !knownKinds[m.Kind] {
				r.errorf(file, "%s 的 kind 必须是 text | video | image", mw)
				continue
			}
			if m.Kind == "text" {
				if m.Family != "" {
					r.errorf(file, "%s 是文本模型，不该声明 family", mw)
				}
				continue
			}
			if !contains(kindFamilies[m.Kind], m.Family) {
				r.errorf(file, "%s 的 family 必须是 %s 之一", mw, strings.Join(kindFamilies[m.Kind], " | "))
			}
		}
	}
	var out []agentModel
	seenProvider := map[string]bool{}
	seenName := map[[2]string]string{}
	for i, a := range doc.Agents {
		where := fmt.Sprintf("agents[%d]（%s）", i, clip(a.Provider))
		if i < len(shape.Agents) {
			reportUnknownKeys(r, file, where, shape.Agents[i], agentKeys)
			var models []map[string]json.RawMessage
			_ = json.Unmarshal(shape.Agents[i]["models"], &models)
			for j, m := range models {
				reportUnknownKeys(r, file, fmt.Sprintf("%s.models[%d]", where, j), m, agentModelKeys)
			}
		}
		if !agentValues[a.Provider] && a.Provider != "cursor" {
			r.errorf(file, "%s 的 provider 只认 codex | grok | claude | cursor", where)
		}
		if seenProvider[a.Provider] {
			r.errorf(file, "%s 重复出现", where)
		}
		seenProvider[a.Provider] = true
		if a.Vendor == "" {
			r.errorf(file, "%s 缺 vendor", where)
		}
		checkProvenance(r, file, where, a.Source, a.CheckedAt)
		if len(a.Models) == 0 {
			r.errorf(file, "%s 一个型号都没有", where)
		}
		for j, m := range a.Models {
			mw := fmt.Sprintf("%s.models[%d]（%s）", where, j, clip(m.Name))
			if err := checkName(m.Name); err != nil {
				r.errorf(file, "%s 的 name %s", mw, err)
			}
			if m.Kind != "text" {
				r.errorf(file, "%s 不是文本模型：agents 段只收文本", mw)
			}
			nameKey := [2]string{"", strings.ToLower(m.Name)}
			if a.Provider == "cursor" {
				nameKey[0] = "cursor" // Cursor 独立计价，允许与其他订阅同名。
			}
			if prev, dup := seenName[nameKey]; dup {
				r.errorf(file, "%s 与 %s 重名（一个名字在 agents 段只能有一行）", mw, prev)
			}
			seenName[nameKey] = mw
			// 逐模型的 source / checked_at 是对 provider 那条的**覆盖**，各自可选：
			// 写了就得合形。
			if m.Source != "" {
				if u, err := url.Parse(m.Source); err != nil || u.Scheme != "https" || u.Host == "" {
					r.errorf(file, "%s 的 source 必须是官方页面的 HTTPS 地址", mw)
				}
			}
			if m.CheckedAt != "" && !dateRE.MatchString(m.CheckedAt) {
				r.errorf(file, "%s 的 checked_at 必须是 YYYY-MM-DD", mw)
			}
			if a.Provider != "cursor" {
				out = append(out, agentModel{Provider: a.Provider, Name: m.Name})
			}
		}
	}
	return out
}

func checkCapabilityShape(r *Report, file, where string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var caps map[string]json.RawMessage
	if json.Unmarshal(raw, &caps) != nil {
		return // 结构解析已报
	}
	reportUnknownKeys(r, file, where+".capabilities", caps, capabilityKeys)
	var rc map[string]json.RawMessage
	if json.Unmarshal(caps["responses_chat"], &rc) == nil {
		reportUnknownKeys(r, file, where+".capabilities.responses_chat", rc, responsesChatKeys)
	}
	var cx map[string]json.RawMessage
	if json.Unmarshal(caps["codex"], &cx) == nil {
		reportUnknownKeys(r, file, where+".capabilities.codex", cx, codexCapabilityKeys)
		var levels []map[string]json.RawMessage
		if json.Unmarshal(cx["supported_reasoning_levels"], &levels) == nil {
			for i, l := range levels {
				reportUnknownKeys(r, file, fmt.Sprintf("%s.capabilities.codex.supported_reasoning_levels[%d]", where, i), l, reasoningLevelKeys)
			}
		}
	}
}

// ---- 两份文件之间 ----

// crossCheck：agents 段的每个文本型号都要在价目文件里有带同名、同 agent 标记的
// 条目（否则设备建出一行永远记 0 元的模型）；价目文件里带标记但目录没收录的只
// 提示——价目可以先于目录收录，也是管理员手工建同名行借价那条旋钮的用法。
func crossCheck(r *Report, pricedAgent map[string]string, agentModels []agentModel) {
	inCatalog := map[string]bool{}
	for _, m := range agentModels {
		inCatalog[m.Name] = true
		got, ok := pricedAgent[m.Name]
		switch {
		case !ok:
			r.errorf(PlatformModelsFile, "agents 段 %s 的型号 %q 在 %s 里没有带 agent 标记的同名条目（名字须逐字节一致）", m.Provider, m.Name, OfficialPricingFile)
		case got != m.Provider:
			r.errorf(OfficialPricingFile, "型号 %q 的 agent = %q，目录 agents 段里归 %q", m.Name, got, m.Provider)
		}
	}
	for _, name := range sortedKeys(pricedAgent) {
		if !inCatalog[name] {
			r.infof(OfficialPricingFile, "带 agent 标记的 %q 不在 %s 的 agents 段里——设备不会自动为它建行", name, PlatformModelsFile)
		}
	}
}

// ---- 共用小件 ----

func checkFormat(r *Report, file string, raw []byte) {
	want, err := Format(raw)
	if err != nil {
		return // 后面的解析会报
	}
	if !bytes.Equal(want, raw) {
		r.errorf(file, "不是规范格式（2 空格缩进、每个元素各占一行、末尾一个换行）；用 catalogcheck -fix 重排")
	}
}

func checkProvenance(r *Report, file, where, source, checkedAt string) {
	u, err := url.Parse(source)
	if source == "" || err != nil || u.Scheme != "https" || u.Host == "" {
		r.errorf(file, "%s 的 source 必须是当次核对的官方页面 HTTPS 地址（收录纪律：没有出处的条目不入表）", where)
	}
	if !dateRE.MatchString(checkedAt) {
		r.errorf(file, "%s 的 checked_at 必须是 YYYY-MM-DD 的核对日期", where)
	}
}

func checkName(name string) error {
	if name == "" {
		return errors.New("不能为空")
	}
	if utf8.RuneCountInString(name) > nameMaxRunes {
		return fmt.Errorf("长度须不超过 %d 个字符", nameMaxRunes)
	}
	for _, c := range name {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return errors.New("不得包含空白或控制字符")
		}
	}
	return nil
}

func reportUnknownKeys(r *Report, file, where string, obj map[string]json.RawMessage, allowed map[string]bool) {
	for _, k := range sortedKeys(obj) {
		if !allowed[k] {
			r.errorf(file, "%s 含设备不认识的字段 %q（拼错的字段会被设备静默忽略）", where, k)
		}
	}
}

func keySet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// clip 裁截回显进报告的远端字段。
func clip(s string) string {
	runes := []rune(s)
	if len(runes) <= 48 {
		return s
	}
	return string(runes[:48]) + "…"
}
