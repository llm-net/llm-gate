// Package catalogcheck 是模型目录数据文件（model-catalog.json）的发布前校验：
// tools/modeldata 合成出来的文件在被复制到官网与固件内嵌副本之前，必须整体通过
// 这里的检查。
//
// 为什么在设备解析之外再设一道：设备侧刻意**宽松**——取回的文件只做结构校验，
// 价目字段集对不对 kind、分时段价合不合形态到建模 / 补价那一步才判。宽松是对的
// （一份写坏的远端文件不该停掉盒子），但发布侧因此需要一道**严格**的闸：未知字段、
// 缺出处、字段集错配、时段重叠、订阅型号没录价、两份副本不一致，这些错在发布前抓
// 比在几百台设备上各自表现成一个小症状便宜得多。
//
// 词汇只此一份：价格字段集用 usage.FieldsFor，成对字段用 usage.CheckPricingPairs，
// 分时段价用 usage.ParseSchedule，结构用 platformcatalog.Parse——本包只把它们
// 串起来，加上设备不检查、但发布必须遵守的纪律（出处、日期、排序、规范格式）。
//
// 两个消费方：tools/catalogcheck（`make -C firmware catalog` 在合成之后执行）与
// 本包测试（把守仓库里的两份副本）。
package catalogcheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// FileName 是目录文件的文件名（官网 /updates/data/ 下与固件内嵌副本同名）。
const FileName = "model-catalog.json"

// 模型名上限（与 internal/admin 建模校验的 catalogNameMaxRunes 同值）。
const nameMaxRunes = 128

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Report 是一次校验的结果。Errors 非空即不通过；Infos 是不阻塞发布的提示
// （如订阅清单里有数据仓库尚无参考价的型号）。
type Report struct {
	Errors []string
	Infos  []string
}

// OK 报告是否通过。
func (r *Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) errorf(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *Report) infof(format string, args ...any) {
	r.Infos = append(r.Infos, fmt.Sprintf(format, args...))
}

// Format 把一份文件重排成规范格式：2 空格缩进、每个数组元素与对象成员各占一行、
// 键序与数值原样、不转义 HTML 字符、末尾一个换行。两份副本要求逐字节一致，规范
// 格式让「同一份内容」只有一种字节形态。
func Format(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(value), "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	// json.Indent 不动转义；合成工具输出的是不转义 HTML 的形态，这里也不引入转义。
	return buf.Bytes(), nil
}

// CheckFile 读一份文件并校验。
func CheckFile(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Check(raw), nil
}

var (
	topKeys           = keySet("schema", "version", "updated_at", "currency", "unit", "source", "notes", "platforms", "agents")
	sourceKeys        = keySet("repository", "tag", "commit", "usd_cny")
	platformKeys      = keySet("id", "type", "vendor", "base_url", "custom_base_url", "billing_mode", "suggested_name", "entries", "ability", "source", "checked_at", "note", "origin", "models")
	platformModelKeys = keySet("name", "kind", "family", "upstream_model_id", "note", "capabilities", "upstream_protocols", "pricing", "schedule", "source", "checked_at", "origin")
	capabilityKeys    = keySet("responses_chat", "codex", "input_modalities")
	responsesChatKeys = keySet("profile", "replay_reasoning_content", "thinking_type", "drop_tool_choice", "effort_map")
	codexKeys         = keySet("default_reasoning_level", "supported_reasoning_levels")
	reasoningKeys     = keySet("effort", "description")
	scheduleKeys      = keySet("timezone", "periods")
	periodKeys        = keySet("label", "days", "hours", "pricing")
	agentKeys         = keySet("provider", "vendor", "source", "checked_at", "note", "origin", "models")
	agentModelKeys    = keySet("name", "kind", "note", "source", "checked_at", "pricing", "origin")
	knownKinds        = keySet("text", "video", "image")
	agentProviders    = keySet("codex", "grok", "claude", "cursor")
	kindFamilies      = map[string][]string{"video": {"ark_video", "minimax_video"}, "image": {"ark_image"}}
	billingRank       = map[string]int{platformcatalog.BillingUsage: 0, platformcatalog.BillingSubscription: 1, platformcatalog.BillingNone: 2}
	billingLabel      = map[string]string{platformcatalog.BillingUsage: "API 按量计费", platformcatalog.BillingSubscription: "API 订阅套餐", platformcatalog.BillingNone: "通用兼容适配"}
)

// Check 校验一份文件的原文。
func Check(raw []byte) *Report {
	r := &Report{}
	checkFormat(r, raw)
	if len(raw) > platformcatalog.MaxBytes {
		r.errorf("文件 %d 字节，超过设备下载上限 %d", len(raw), platformcatalog.MaxBytes)
	}
	doc, err := platformcatalog.Parse(raw)
	if err != nil {
		r.errorf("设备解析失败：%v", err)
		return r
	}
	if !dateRE.MatchString(doc.UpdatedAt) {
		r.errorf("updated_at 必须是 YYYY-MM-DD")
	}
	if doc.Source.Tag == "" || doc.Source.Commit == "" || doc.Source.Repository == "" {
		r.errorf("source 必须写全 repository / tag / commit（文件由数据仓库的正式发布合成）")
	}
	if _, ok := new(usdRat).parse(doc.Source.USDCNY); !ok {
		r.errorf("source.usd_cny 必须是正的十进制汇率")
	}
	// 未知键扫描：设备解析会静默忽略，发布侧必须当错误——拼错的字段会变成
	// 「文件里写了、设备永远读不到」。
	var shape struct {
		Source    map[string]json.RawMessage   `json:"source"`
		Platforms []map[string]json.RawMessage `json:"platforms"`
		Agents    []map[string]json.RawMessage `json:"agents"`
	}
	top := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &top) == nil {
		reportUnknownKeys(r, "顶层", top, topKeys)
		_ = json.Unmarshal(raw, &shape)
		reportUnknownKeys(r, "source", shape.Source, sourceKeys)
	}
	checkPlatforms(r, doc, shape.Platforms)
	checkAgents(r, doc, shape.Agents)
	return r
}

func checkPlatforms(r *Report, doc platformcatalog.Doc, shape []map[string]json.RawMessage) {
	// 三类平台按 按量 → 套餐 → 通用适配 排列：platforms 段的先后就是管理台各组内
	// 的展示顺序。
	lastRank := -1
	for i, p := range doc.Platforms {
		where := fmt.Sprintf("platforms[%d]（%s）", i, clip(p.ID))
		var models []map[string]json.RawMessage
		if i < len(shape) {
			reportUnknownKeys(r, where, shape[i], platformKeys)
			_ = json.Unmarshal(shape[i]["models"], &models)
		}
		if rank := billingRank[p.BillingMode]; rank < lastRank {
			r.errorf("%s 的 %s 排在了后一类平台之后；platforms 段必须按 API 按量计费 → API 订阅套餐 → 通用兼容适配 排列", where, billingLabel[p.BillingMode])
		} else {
			lastRank = rank
		}
		if p.Vendor == "" {
			r.errorf("%s 缺 vendor", where)
		}
		checkProvenance(r, where, p.Source, p.CheckedAt)
		if len(p.Models) == 0 && p.BillingMode != platformcatalog.BillingNone {
			r.infof("%s 的模型选单为空（管理台只提供「自定义」）", where)
		}
		for j, m := range p.Models {
			mw := fmt.Sprintf("%s.models[%d]（%s）", where, j, clip(m.Name))
			if j < len(models) {
				reportUnknownKeys(r, mw, models[j], platformModelKeys)
				checkCapabilityShape(r, mw, models[j]["capabilities"])
			}
			if err := checkName(m.Name); err != nil {
				r.errorf("%s 的 name %s", mw, err)
			}
			if m.UpstreamModelID != "" {
				if err := checkName(m.UpstreamModelID); err != nil {
					r.errorf("%s 的 upstream_model_id %s", mw, err)
				}
			}
			if !knownKinds[m.Kind] {
				r.errorf("%s 的 kind 必须是 text | video | image", mw)
				continue
			}
			if m.Kind == "text" {
				if m.Family != "" {
					r.errorf("%s 是文本模型，不该声明 family", mw)
				}
			} else if !contains(kindFamilies[m.Kind], m.Family) {
				r.errorf("%s 的 family 必须是 %s 之一", mw, strings.Join(kindFamilies[m.Kind], " | "))
			}
			if m.Source != "" || m.CheckedAt != "" {
				checkProvenance(r, mw, m.Source, m.CheckedAt)
			}
			checkPricing(r, mw, m.Kind, m.Pricing, m.Schedule)
		}
	}
}

// checkPricing 校验一条价目（缺席合法）：字段集按 kind、成对字段、分时段价。
func checkPricing(r *Report, where, kind string, pricingRaw, scheduleRaw json.RawMessage) {
	if len(bytes.TrimSpace(pricingRaw)) == 0 {
		if len(bytes.TrimSpace(scheduleRaw)) > 0 {
			r.errorf("%s 有 schedule 却没有标准价 pricing", where)
		}
		return
	}
	pricing, err := usage.ParsePricing(string(pricingRaw))
	if err != nil {
		r.errorf("%s 的 pricing 不合法：%v", where, err)
		return
	}
	if !pricing.Priced() {
		r.errorf("%s 的 pricing 是空表（未定价的模型不带 pricing 字段）", where)
		return
	}
	allowed := keySet(usage.FieldsFor(kind)...)
	for _, f := range sortedKeys(pricing) {
		if !allowed[f] {
			r.errorf("%s 的 pricing 含 %s 模型不认的字段 %q（可配：%s）", where, kind, f, strings.Join(usage.FieldsFor(kind), "、"))
		}
	}
	if err := usage.CheckPricingPairs(pricing); err != nil {
		r.errorf("%s：%v", where, err)
	}
	if len(bytes.TrimSpace(scheduleRaw)) > 0 {
		checkScheduleShape(r, where, scheduleRaw)
		if _, err := usage.ParseSchedule(scheduleRaw, pricing); err != nil {
			r.errorf("%s 的 schedule 不合法：%v", where, err)
		}
	}
}

func checkScheduleShape(r *Report, where string, raw json.RawMessage) {
	var s map[string]json.RawMessage
	if err := json.Unmarshal(raw, &s); err != nil {
		return // ParseSchedule 会报
	}
	reportUnknownKeys(r, where+".schedule", s, scheduleKeys)
	var periods []map[string]json.RawMessage
	if err := json.Unmarshal(s["periods"], &periods); err != nil {
		return
	}
	for i, p := range periods {
		reportUnknownKeys(r, fmt.Sprintf("%s.schedule.periods[%d]", where, i), p, periodKeys)
	}
}

func checkAgents(r *Report, doc platformcatalog.Doc, shape []map[string]json.RawMessage) {
	seenProvider := map[string]bool{}
	seenName := map[[2]string]string{}
	for i, a := range doc.Agents {
		where := fmt.Sprintf("agents[%d]（%s）", i, clip(a.Provider))
		var models []map[string]json.RawMessage
		if i < len(shape) {
			reportUnknownKeys(r, where, shape[i], agentKeys)
			_ = json.Unmarshal(shape[i]["models"], &models)
		}
		if !agentProviders[a.Provider] {
			r.errorf("%s 的 provider 只认 codex | grok | claude | cursor", where)
		}
		if seenProvider[a.Provider] {
			r.errorf("%s 重复出现", where)
		}
		seenProvider[a.Provider] = true
		if a.Vendor == "" {
			r.errorf("%s 缺 vendor", where)
		}
		checkProvenance(r, where, a.Source, a.CheckedAt)
		if len(a.Models) == 0 {
			r.errorf("%s 一个型号都没有", where)
		}
		for j, m := range a.Models {
			mw := fmt.Sprintf("%s.models[%d]（%s）", where, j, clip(m.Name))
			if j < len(models) {
				reportUnknownKeys(r, mw, models[j], agentModelKeys)
			}
			if err := checkName(m.Name); err != nil {
				r.errorf("%s 的 name %s", mw, err)
			}
			if m.Kind != "text" {
				r.errorf("%s 不是文本模型：agents 段只收文本", mw)
			}
			nameKey := [2]string{"", strings.ToLower(m.Name)}
			if a.Provider == "cursor" {
				nameKey[0] = "cursor" // Cursor 独立计价，允许与其他订阅同名。
			}
			if prev, dup := seenName[nameKey]; dup {
				r.errorf("%s 与 %s 重名（一个名字在 agents 段只能有一行）", mw, prev)
			}
			seenName[nameKey] = mw
			if m.Source != "" || m.CheckedAt != "" {
				checkProvenance(r, mw, m.Source, m.CheckedAt)
			}
			if len(bytes.TrimSpace(m.Pricing)) == 0 {
				// 不是错：数据仓库可以先收型号、后补参考价；设备为它建一行 0 元的行。
				r.infof("%s 没有 pricing——设备会为它建一行记 0 元的计价行", mw)
				continue
			}
			checkPricing(r, mw, "text", m.Pricing, nil)
		}
	}
}

func checkCapabilityShape(r *Report, where string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var caps map[string]json.RawMessage
	if json.Unmarshal(raw, &caps) != nil {
		return // 结构解析已报
	}
	reportUnknownKeys(r, where+".capabilities", caps, capabilityKeys)
	var rc map[string]json.RawMessage
	if json.Unmarshal(caps["responses_chat"], &rc) == nil {
		reportUnknownKeys(r, where+".capabilities.responses_chat", rc, responsesChatKeys)
	}
	var cx map[string]json.RawMessage
	if json.Unmarshal(caps["codex"], &cx) == nil {
		reportUnknownKeys(r, where+".capabilities.codex", cx, codexKeys)
		var levels []map[string]json.RawMessage
		if json.Unmarshal(cx["supported_reasoning_levels"], &levels) == nil {
			for i, l := range levels {
				reportUnknownKeys(r, fmt.Sprintf("%s.capabilities.codex.supported_reasoning_levels[%d]", where, i), l, reasoningKeys)
			}
		}
	}
}

// ---- 共用小件 ----

func checkFormat(r *Report, raw []byte) {
	want, err := Format(raw)
	if err != nil {
		return // 后面的解析会报
	}
	if !bytes.Equal(want, raw) {
		r.errorf("不是规范格式（2 空格缩进、每个元素各占一行、末尾一个换行）；用 catalogcheck -fix 重排")
	}
}

func checkProvenance(r *Report, where, source, checkedAt string) {
	u, err := url.Parse(source)
	if source == "" || err != nil || u.Scheme != "https" || u.Host == "" {
		r.errorf("%s 的 source 必须是官方页面的 HTTPS 地址（收录纪律：没有出处的条目不入表）", where)
	}
	if !dateRE.MatchString(checkedAt) {
		r.errorf("%s 的 checked_at 必须是 YYYY-MM-DD 的核对日期", where)
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

func reportUnknownKeys(r *Report, where string, obj map[string]json.RawMessage, allowed map[string]bool) {
	for _, k := range sortedKeys(obj) {
		if !allowed[k] {
			r.errorf("%s 含设备不认识的字段 %q（拼错的字段会被设备静默忽略）", where, k)
		}
	}
}

// usdRat 只为校验汇率是正的十进制数，不引入 big 依赖到别处。
type usdRat struct{}

var decimalRE = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

func (usdRat) parse(s string) (string, bool) {
	if !decimalRE.MatchString(s) || strings.Trim(s, "0.") == "" {
		return "", false
	}
	return s, true
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
