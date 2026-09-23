package modeldata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// DefaultRepository 是数据仓库地址（写进文件 source 段，供人追溯）。
const DefaultRepository = "https://github.com/llm-net/llm-model-data"

// DefaultUSDCNY 是美元价折人民币的固定汇率（十进制字符串），随文件 source 段发布。
// 汇率变动大时改这里再合成，已同步进设备的价照旧可在模型页改。
const DefaultUSDCNY = "6.75"

// Options 是一次合成的参数。
type Options struct {
	Repository string // 缺省 DefaultRepository
	USDCNY     string // 缺省 DefaultUSDCNY
}

// Output 是合成结果：规范格式的文件字节与给维护者看的报告。
type Output struct {
	Raw    []byte
	Report []string
}

// ---- 输出形态（键序即文件里的键序；与 platformcatalog 的读取形态逐字对齐）----

type outDoc struct {
	Schema    string        `json:"schema"`
	Version   int64         `json:"version"`
	UpdatedAt string        `json:"updated_at"`
	Currency  string        `json:"currency"`
	Unit      string        `json:"unit"`
	Source    outSource     `json:"source"`
	Notes     []string      `json:"notes"`
	Platforms []outPlatform `json:"platforms"`
	Agents    []outAgent    `json:"agents"`
}

type outSource struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Commit     string `json:"commit"`
	USDCNY     string `json:"usd_cny"`
}

type outPlatform struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	Vendor        string     `json:"vendor"`
	BaseURL       string     `json:"base_url,omitempty"`
	CustomBaseURL bool       `json:"custom_base_url,omitempty"`
	BillingMode   string     `json:"billing_mode"`
	SuggestedName string     `json:"suggested_name"`
	Entries       string     `json:"entries"`
	Ability       string     `json:"ability"`
	Source        string     `json:"source"`
	CheckedAt     string     `json:"checked_at"`
	Note          string     `json:"note,omitempty"`
	Origin        []string   `json:"origin,omitempty"`
	Models        []outModel `json:"models"`
}

type outModel struct {
	Name              string                             `json:"name"`
	Kind              string                             `json:"kind"`
	Family            string                             `json:"family,omitempty"`
	UpstreamModelID   string                             `json:"upstream_model_id,omitempty"`
	Note              string                             `json:"note,omitempty"`
	UpstreamProtocols []string                           `json:"upstream_protocols,omitempty"`
	Capabilities      *platformcatalog.ModelCapabilities `json:"capabilities,omitempty"`
	Pricing           map[string]int64                   `json:"pricing,omitempty"`
	Schedule          *scheduleDoc                       `json:"schedule,omitempty"`
	Source            string                             `json:"source,omitempty"`
	CheckedAt         string                             `json:"checked_at,omitempty"`
	Origin            string                             `json:"origin,omitempty"`
}

type outAgent struct {
	Provider  string          `json:"provider"`
	Vendor    string          `json:"vendor"`
	Source    string          `json:"source"`
	CheckedAt string          `json:"checked_at"`
	Note      string          `json:"note,omitempty"`
	Origin    []string        `json:"origin,omitempty"`
	Models    []outAgentModel `json:"models"`
}

type outAgentModel struct {
	Name      string           `json:"name"`
	Kind      string           `json:"kind"`
	Note      string           `json:"note,omitempty"`
	Source    string           `json:"source,omitempty"`
	CheckedAt string           `json:"checked_at,omitempty"`
	Pricing   map[string]int64 `json:"pricing,omitempty"`
	Origin    string           `json:"origin,omitempty"`
}

var fileNotes = []string{
	"LLM Gate 的模型目录数据：哪个平台 / 订阅有哪些模型、各自多少钱。设备通过「数据升级」匿名下载本文件，官网不可达时用固件内嵌的同一份基线。",
	"本文件由固件工具 tools/modeldata 从公开数据仓库 github.com/llm-net/llm-model-data 的正式发布 tag（source.tag）合成，不手工维护；改数据到数据仓库，改映射到固件源码 internal/modeldata。",
	"version = tag 日期 YYYYMMDD × 1000 + 当日序号，与 tag 单调同序；设备取版本号更大的那份生效。",
	"platforms 段按管理台「模型接入」分三类：billing_mode = usage（API 按量计费）、subscription（API 订阅套餐）、none（通用兼容适配，地址由管理员自填）；先后即各组的展示顺序。",
	"  平台 id 与固件适配器 type 分开：内置平台两者相同；数据平台复用 openai_compat / anthropic_compat 并给固定 HTTPS base_url。固定 base_url 在管理员录入 Key 时快照进设备。",
	"  models[].name 是客户端请求里的 model 值；kind 是 text | video | image；video / image 带 family（厂商协议面）。upstream_protocols 是该模型在这条平台实际接受的上游协议，只能收窄适配器。",
	"  models[].pricing 是这条平台上这个模型的估算目录价（整数微元；token 类字段为 微元 / 百万 token，按张 / 按秒类为 微元 / 单位），按量产品取公开按量价，订阅产品取同平台或开发方的按量参考价。schedule 是分时段价，形态权威是固件 internal/usage.ParseSchedule。",
	"  设备只给「未定价」的模型填价：从这条平台添加模型时用这条平台的价，管理员随时可在模型页改成自己的口径；已定价的模型绝不覆盖。",
	"agents 段是工具订阅（codex | grok | claude | cursor）的计价清单：连接订阅后设备照此建只读计价行，名义金额按 pricing（官方 API 按量价）记，不折算订阅补贴。Cursor 独立计价、不建共享行。",
	"美元价按 source.usd_cny 的固定汇率折算后取整；金额全链路整数微元，1 元 = 1000000 微元。",
	"origin 字段记数据仓库里的来源（provider/offering[/model]），只供追溯。",
}

// Build 把一次输入合成为设备目录文件。
func Build(in Input, opts Options) (Output, error) {
	if opts.Repository == "" {
		opts.Repository = DefaultRepository
	}
	if opts.USDCNY == "" {
		opts.USDCNY = DefaultUSDCNY
	}
	f, err := newFX(opts.USDCNY)
	if err != nil {
		return Output{}, err
	}
	version, err := in.Tag.Version()
	if err != nil {
		return Output{}, err
	}
	b := &builder{in: in, fx: f, tagDate: in.Tag.Date()}
	doc := outDoc{
		Schema: platformcatalog.Schema, Version: version, UpdatedAt: in.Tag.Date(),
		Currency: "CNY", Unit: "micro_yuan",
		Source: outSource{Repository: opts.Repository, Tag: in.Tag.Name, Commit: in.Commit, USDCNY: opts.USDCNY},
		Notes:  fileNotes,
	}
	// 每个产品是否被映射、每个模型是否落到了至少一条平台 / 订阅。
	for key := range b.offeringKeys() {
		if _, ok := offeringTargets[key]; !ok {
			b.reportf("未映射的产品 %s：不进文件（要接入请在 internal/modeldata/mapping.go 加映射）", key)
		}
	}
	for _, spec := range platformSpecs {
		p, ok, err := b.platform(spec)
		if err != nil {
			return Output{}, fmt.Errorf("平台 %s：%v", spec.ID, err)
		}
		if ok {
			doc.Platforms = append(doc.Platforms, p)
		}
	}
	for _, spec := range agentSpecs {
		if len(b.catalogsFor("", spec.Provider)) == 0 {
			b.reportf("订阅 %s：数据仓库没有对应产品，本次不进文件（连上该订阅的设备将列不出计价行）", spec.Provider)
			continue
		}
		a, err := b.agent(spec)
		if err != nil {
			return Output{}, fmt.Errorf("订阅 %s：%v", spec.Provider, err)
		}
		doc.Agents = append(doc.Agents, a)
	}
	for _, key := range b.sortedOfferingKeys() {
		for _, m := range b.landed[key].missing() {
			b.reportf("产品 %s 的模型 %s 没有落到任何平台 / 订阅（协议面、模态或请求名不被设备承载）", key, m)
		}
	}
	raw, err := format(doc)
	if err != nil {
		return Output{}, err
	}
	if _, err := platformcatalog.Parse(raw); err != nil {
		return Output{}, fmt.Errorf("合成结果不被设备接受：%v", err)
	}
	if len(raw) > platformcatalog.MaxBytes {
		return Output{}, fmt.Errorf("合成结果 %d 字节，超过设备下载上限 %d", len(raw), platformcatalog.MaxBytes)
	}
	sort.Strings(b.report)
	return Output{Raw: raw, Report: b.report}, nil
}

type landing struct {
	all    []string
	landed map[string]bool
}

func (l *landing) missing() []string {
	var out []string
	for _, m := range l.all {
		if !l.landed[m] {
			out = append(out, m)
		}
	}
	return out
}

type builder struct {
	in      Input
	fx      fx
	tagDate string
	report  []string
	landed  map[string]*landing
}

func (b *builder) reportf(format string, args ...any) {
	b.report = append(b.report, fmt.Sprintf(format, args...))
}

func offeringKey(c Catalog) string { return c.ProviderID + "/" + c.ID }

func (b *builder) offeringKeys() map[string]bool {
	keys := map[string]bool{}
	for _, c := range b.in.Catalogs {
		keys[offeringKey(c)] = true
	}
	return keys
}

func (b *builder) sortedOfferingKeys() []string {
	keys := make([]string, 0, len(b.landed))
	for k := range b.landed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// track 记录一个模型被考虑过 / 落地了。
func (b *builder) track(c Catalog, m Model, landed bool) {
	if b.landed == nil {
		b.landed = map[string]*landing{}
	}
	key := offeringKey(c)
	l := b.landed[key]
	if l == nil {
		l = &landing{landed: map[string]bool{}}
		b.landed[key] = l
	}
	if _, seen := l.landed[m.ID]; !seen {
		l.all = append(l.all, m.ID)
		l.landed[m.ID] = false
	}
	if landed {
		l.landed[m.ID] = true
	}
}

// catalogsFor 取落到某条平台 / 订阅的全部产品（按 provider、offering 排序）。
func (b *builder) catalogsFor(platformID, agent string) []Catalog {
	var out []Catalog
	for _, c := range b.in.Catalogs {
		t, ok := offeringTargets[offeringKey(c)]
		if !ok {
			continue
		}
		if (platformID != "" && contains(t.Platforms, platformID)) || (agent != "" && t.Agent == agent) {
			out = append(out, c)
		}
	}
	return out
}

// requestName 取模型的请求名；没有（null）就不可调用，不进文件。
func requestName(m Model) (string, bool) {
	if m.RequestID == nil || strings.TrimSpace(*m.RequestID) == "" {
		return "", false
	}
	return *m.RequestID, true
}

func modelNote(m Model, name string) string {
	var parts []string
	if m.DisplayName != "" && m.DisplayName != name {
		parts = append(parts, m.DisplayName)
	}
	if m.AliasOf != nil && *m.AliasOf != "" {
		parts = append(parts, "别名，同 "+*m.AliasOf)
	}
	return strings.Join(parts, "；")
}

// platform 合成一条平台。数据仓库没有对应产品的平台不进文件（ok=false）——一条
// 空选单的平台只会让管理台多一张点不出型号的卡片；通用兼容适配例外。
func (b *builder) platform(spec platformSpec) (outPlatform, bool, error) {
	p := outPlatform{
		ID: spec.ID, Type: spec.Type, Vendor: spec.Vendor, BaseURL: spec.BaseURL,
		CustomBaseURL: spec.BillingMode == platformcatalog.BillingNone,
		BillingMode:   spec.BillingMode, SuggestedName: spec.SuggestedName,
		Entries: spec.Entries, Ability: spec.Ability, Note: spec.Note, Models: []outModel{},
	}
	acct := upstream.Account{Type: spec.Type, BaseURL: spec.BaseURL}
	if p.CustomBaseURL {
		acct.BaseURL = "https://api.example.invalid/v1" // 只为算协议能力
	}
	seen := map[string]string{} // 小写名 → 首次来源
	latest := ""
	for _, c := range b.catalogsFor(spec.ID, "") {
		prov := b.in.Providers[c.ProviderID]
		p.Origin = append(p.Origin, offeringKey(c))
		if p.Source == "" {
			p.Source = prov.sourceURL(Verification{}, "models", "pricing")
		}
		for _, m := range c.Models {
			origin := offeringKey(c) + "/" + m.ID
			name, ok := requestName(m)
			if !ok || m.Availability == "retired" {
				b.track(c, m, true) // 有意不收：不算「没落地」
				continue
			}
			kind := kindOf(m.Modalities)
			if kind == "" {
				b.track(c, m, true)
				continue
			}
			protocols := intersect(spec.Protocols, m.Capabilities.Interfaces)
			if len(protocols) == 0 {
				b.track(c, m, false)
				continue
			}
			om := outModel{Name: name, Kind: kind, Note: modelNote(m, name), Origin: origin}
			face := ""
			if kind == store.ModelKindText {
				om.UpstreamProtocols = intersect(textProtocols, protocols)
				servable := false
				for _, pr := range om.UpstreamProtocols {
					if _, ok := acct.Endpoint(upstream.CatalogWireProtocol(pr)); ok {
						servable = true
					}
				}
				if !servable {
					b.track(c, m, false)
					continue
				}
			} else {
				fam, err := familyOf(kind, m.Capabilities.Interfaces, spec.Protocols)
				if err != nil {
					b.track(c, m, false)
					continue
				}
				if _, ok := acct.Endpoint(upstream.CatalogWireProtocol(fam)); !ok {
					b.track(c, m, false)
					continue
				}
				om.Family, face = fam, fam
			}
			if prev, dup := seen[strings.ToLower(name)]; dup {
				b.reportf("平台 %s：%s 与 %s 同名，只保留前者", spec.ID, origin, prev)
				b.track(c, m, true)
				continue
			}
			seen[strings.ToLower(name)] = origin
			if caps, ok := modelBehaviour(spec.ID, name); ok {
				c := caps
				om.Capabilities = &c
			}
			if kind == store.ModelKindText && len(m.Capabilities.InputModalities) > 0 {
				if om.Capabilities == nil {
					om.Capabilities = &platformcatalog.ModelCapabilities{}
				}
				om.Capabilities.InputModalities = append([]string(nil), m.Capabilities.InputModalities...)
			}
			om.Source = prov.sourceURL(m.Verification, "models", "pricing")
			om.CheckedAt = checkedDate(m.Verification)
			if rule := pricingSource(m); rule != nil {
				pricing, dropped, err := b.fx.convertRates(kind, face, rule.Currency, rule.Rates)
				if err != nil {
					return outPlatform{}, false, fmt.Errorf("%s 的价格：%v", origin, err)
				}
				for _, d := range dropped {
					b.reportf("平台 %s：%s 的分量 %s 设备价目装不下，已丢弃", spec.ID, origin, d)
				}
				if len(pricing) == 0 {
					b.reportf("平台 %s：%s 有价格但没有一个分量落到设备字段，按未定价收录", spec.ID, origin)
				} else {
					om.Pricing = pricing
					sched, err := b.fx.convertSchedule(kind, face, rule.Currency, rule.TimePricing, rule.Rates, pricing)
					if err != nil {
						return outPlatform{}, false, fmt.Errorf("%s 的分时价：%v", origin, err)
					}
					om.Schedule = sched
				}
				if om.CheckedAt == "" {
					om.CheckedAt = checkedDate(rule.Verification)
				}
				if om.Source == "" {
					om.Source = prov.sourceURL(rule.Verification, "pricing", "models")
				}
			} else {
				b.reportf("平台 %s：%s 没有已发布的价格，按未定价收录", spec.ID, origin)
			}
			if om.CheckedAt == "" {
				om.CheckedAt = b.tagDate
			}
			if om.CheckedAt > latest {
				latest = om.CheckedAt
			}
			p.Models = append(p.Models, om)
			b.track(c, m, true)
		}
	}
	if len(p.Models) == 0 {
		switch {
		case spec.BillingMode == platformcatalog.BillingNone:
			// 通用适配本就没有选单。
		case len(p.Origin) == 0:
			b.reportf("平台 %s：数据仓库没有对应产品，本次不进文件", spec.ID)
			return outPlatform{}, false, nil
		default:
			b.reportf("平台 %s：没有任何型号落地，选单为空", spec.ID)
		}
	}
	if p.Source == "" {
		p.Source = staticPlatformSources[spec.ID]
	}
	if p.Source == "" {
		return outPlatform{}, false, errors.New("没有出处（数据仓库的 provider.json 没登记任何来源）")
	}
	p.CheckedAt = latest
	if p.CheckedAt == "" {
		p.CheckedAt = b.tagDate
	}
	return p, true, nil
}

func (b *builder) agent(spec agentSpec) (outAgent, error) {
	a := outAgent{Provider: spec.Provider, Vendor: spec.Vendor, Note: spec.Note, Models: []outAgentModel{}}
	seen := map[string]string{}
	latest := ""
	var cursorBasis *outAgentModel
	for _, c := range b.catalogsFor("", spec.Provider) {
		prov := b.in.Providers[c.ProviderID]
		a.Origin = append(a.Origin, offeringKey(c))
		if a.Source == "" {
			a.Source = prov.sourceURL(Verification{}, "subscription", "pricing", "models")
		}
		for _, m := range c.Models {
			origin := offeringKey(c) + "/" + m.ID
			name, ok := requestName(m)
			if !ok || m.Availability == "retired" {
				b.track(c, m, true)
				continue
			}
			if kindOf(m.Modalities) != store.ModelKindText {
				b.track(c, m, true) // 订阅只收文本：图像 / 视频恒记 0 元，不进清单
				continue
			}
			if prev, dup := seen[strings.ToLower(name)]; dup {
				b.reportf("订阅 %s：%s 与 %s 同名，只保留前者", spec.Provider, origin, prev)
				b.track(c, m, true)
				continue
			}
			seen[strings.ToLower(name)] = origin
			om := outAgentModel{Name: name, Kind: store.ModelKindText, Note: modelNote(m, name), Origin: origin}
			om.Source = prov.sourceURL(m.Verification, "models", "pricing")
			om.CheckedAt = checkedDate(m.Verification)
			if rule := pricingSource(m); rule != nil {
				pricing, dropped, err := b.fx.convertRates(store.ModelKindText, "", rule.Currency, rule.Rates)
				if err != nil {
					return outAgent{}, fmt.Errorf("%s 的价格：%v", origin, err)
				}
				for _, d := range dropped {
					b.reportf("订阅 %s：%s 的分量 %s 设备价目装不下，已丢弃", spec.Provider, origin, d)
				}
				om.Pricing = pricing
				if om.CheckedAt == "" {
					om.CheckedAt = checkedDate(rule.Verification)
				}
			}
			if len(om.Pricing) == 0 {
				b.reportf("订阅 %s：%s 没有已发布的参考价，设备会为它建一行记 0 元的模型", spec.Provider, origin)
			}
			if om.CheckedAt == "" {
				om.CheckedAt = b.tagDate
			}
			if om.CheckedAt > latest {
				latest = om.CheckedAt
			}
			a.Models = append(a.Models, om)
			b.track(c, m, true)
			if spec.Provider == store.AgentProviderCursor && name == cursorAutoBasis {
				basis := om
				cursorBasis = &basis
			}
		}
	}
	if spec.Provider == store.AgentProviderCursor {
		if cursorBasis == nil || len(cursorBasis.Pricing) == 0 {
			return outAgent{}, fmt.Errorf("Cursor 清单里没有带价格的 %s，合成不出 %s 统计行", cursorAutoBasis, cursorAutoName)
		}
		auto := outAgentModel{
			Name: cursorAutoName, Kind: store.ModelKindText, Note: cursorAutoNote,
			Source: cursorAutoSourceU, CheckedAt: cursorBasis.CheckedAt, Pricing: cursorBasis.Pricing,
			Origin: cursorBasis.Origin,
		}
		a.Models = append([]outAgentModel{auto}, a.Models...)
	}
	if len(a.Models) == 0 {
		return outAgent{}, errors.New("数据仓库没有对应产品或一个文本型号都没有")
	}
	if a.Source == "" {
		return outAgent{}, errors.New("没有出处")
	}
	a.CheckedAt = latest
	if a.CheckedAt == "" {
		a.CheckedAt = b.tagDate
	}
	return a, nil
}

// format 输出规范格式：2 空格缩进、UTF-8 原样（不转义 HTML 字符）、末尾一个换行。
func format(doc outDoc) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
