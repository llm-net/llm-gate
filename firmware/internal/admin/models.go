package admin

// 模型目录与来源管理端点（iteration-5 决策 8）：
// GET/POST /admin/v1/models、PATCH/DELETE /admin/v1/models/{id}、
// POST /admin/v1/models/{id}/sources、PATCH/DELETE /admin/v1/sources/{id}。
//
// 模型名就是客户端可见的原始名（iteration-5 反转架构 §11.1 的对客隐藏决策），
// 改名即改客户端调用名，需知会调用方。一个模型可挂多条来源，按 priority
// 选路（小者优先，同值订阅套餐型上游先于按量、再按创建序——计费模式裁决位
// 见 store.billingRankSQL）；来源的 upstream_model_id 留空表示"与模型名
// 相同"，管理视图保留这个空串供 UI 拿模型名做占位符。
//
// 模型自迭代 8 起带 kind（text/video/image）：创建时显式给出（缺省 text，
// 兼容不带 kind 的既有前端），建后不可改——改 kind 等于换模型，PATCH 对改动
// 回 kind_immutable（照上游 type_immutable 先例，store 层本就没有写方法）。
//
// AIGC 模型自 0014 起再带 family（协议面声明，值即厂商协议面的协议标识，如
// ark_video；对外 JSON 另带派生的 protocol_face = 路径首段）：H3 这类开源模型
// 可能由多家平台以不同 API 格式承载，「模型名 → 厂商」不成立，协议面是模型对
// 客户端的接口承诺，只能声明、不能推断。video 建时二选一（可缺省 = 未声明，
// 由首条来源懒钉——兼容旧调用方，管理台恒显式传）、image 缺省补 ark_image、
// text 恒空（family_text_only）；建后不可改（family_immutable）。挂来源对声明
// 校验，契约见 docs/firmware-aigc.md。
//
// 每条来源的 protocols 表示设备通过它能提供的协议面，按模型 kind 圈定。
// 文本依次为 OpenAI Chat、OpenAI Responses、Anthropic Messages；Responses 通过
// Chat 上游转换。视频与图像是各厂商的协议面（config.ProtocolFace）。来源能力
// 不受模型协议面开关影响。空数组表示没有可服务的协议面。
//
// 删模型连带删除其全部来源（外键 ON DELETE CASCADE）；删来源不影响上游。
// quota 是预留列，本迭代无任何读写语义（决策 7），故不出现在这里。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// 模型与来源的变更审计事件（entity 形如 model:3 / source:7；detail 只含
// 模型名、上游名、来源侧模型 ID 与优先级等非敏感字段）。
const (
	EventModelCreate  = "model.create"
	EventModelRename  = "model.rename"
	EventModelEnable  = "model.enable"
	EventModelDisable = "model.disable"
	EventModelDelete  = "model.delete"
	// EventModelPricing 记目录价变更（detail 带价格摘要——字段名与整数微元
	// 都是运营参数，不是凭证也不是内容）。改价即改钱，单列一个事件。
	EventModelPricing = "model.pricing"
	// EventModelEntries 记协议面开关变更（detail 带模型名与新开关组合）。
	EventModelEntries = "model.entries"

	EventSourceCreate = "source.create"
	EventSourceUpdate = "source.update"
	EventSourceDelete = "source.delete"
)

// 新来源的两档缺省优先级（小者优先，留出前后插队的空隙）：订阅型先用满、
// 按量与泛型兜底排后。defaultSourcePriority 与 YAML 导入器的首级同值，也是
// 通用挂载面在调用方没给 priority 时用的那个；usageSourcePriority 由
// defaultPriorityForType 按上游计费模式选出（管理台 api.ts 的
// defaultPriorityFor 是同一张表的另一处消费）。
const (
	defaultSourcePriority int64 = 100
	usageSourcePriority   int64 = 200
)

func entityModel(id int64) string  { return fmt.Sprintf("model:%d", id) }
func entitySource(id int64) string { return fmt.Sprintf("source:%d", id) }

// sourceJSON 是一条来源的对外形态，带上游展示字段与可服务入口。
type sourceJSON struct {
	ID                    int64  `json:"id"`
	ModelID               int64  `json:"model_id"`
	UpstreamID            int64  `json:"upstream_id"`
	UpstreamName          string `json:"upstream_name"`
	UpstreamType          string `json:"upstream_type"`
	UpstreamCatalogID     string `json:"upstream_catalog_id"`
	UpstreamPlatformLabel string `json:"upstream_platform_label"`
	UpstreamBillingMode   string `json:"upstream_billing_mode"`
	UpstreamDisabled      bool   `json:"upstream_disabled"`
	// UpstreamEgressMode 是该来源所在账号的出站方式覆盖（inherit|direct|proxy）：同一模型
	// 的来源出口不一致时界面据此提示「不同尝试可能经不同出口」。
	UpstreamEgressMode string `json:"upstream_egress_mode"`
	// UpstreamModelID 空串表示与模型名相同（UI 用模型名做占位符）。
	UpstreamModelID string `json:"upstream_model_id"`
	Priority        int64  `json:"priority"`
	Disabled        bool   `json:"disabled"`
	// Protocols 是这条来源能服务的入口协议，恒非 nil（可能为空数组）。
	Protocols []string `json:"protocols"`
}

// modelJSON 是模型的对外形态，嵌套其全部来源（含已停用的，按 priority、id 序）。
type modelJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // text | video | image，建后不可改
	// Family 是 AIGC 模型声明的协议面（协议标识，如 ark_video；0014，建后不可改；
	// text 恒空串，空串的 video 行 = 存量未声明、由首条来源懒钉）。
	Family string `json:"family"`
	// ProtocolFace 是 Family 所属厂商的路径首段（config.ProtocolFace，如 ark），
	// 纯派生、只读：管理台据它拼客户端路径与展示名。text 与未声明行为空串。
	ProtocolFace string `json:"protocol_face"`
	// Agent 非空 = 生效的模型目录数据把这个名字列在该订阅的 agents 段里
	// （codex|grok|claude|cursor），本行因此是订阅流量的计价行——数据面按名取价
	// （applyAgentModelPricing）记名义金额。注记逐次按文件现状算出、不落库、
	// 仅 text 行会带；管理台据此把**无上游**的这类行归「订阅接入」标签页并
	// 渲染订阅侧调用入口，挂了上游的照旧按 API 模型渲染。
	Agent    string `json:"agent"`
	Disabled bool   `json:"disabled"`
	// EntryOpenAI / EntryResponses / EntryAnthropic 是协议面开关（仅 text 有语义，默认全开；
	// AIGC 模型恒回 true 但 UI 不渲染）。数据面对关掉的入口不组候选，两条
	// servable 读数同步排除全关的行——契约见 docs/firmware-gateway.md。
	EntryOpenAI    bool `json:"entry_openai"`
	EntryResponses bool `json:"entry_responses"`
	EntryAnthropic bool `json:"entry_anthropic"`
	// Pricing 是目录价「字段名 → 整数微元」表，**null = 未定价**（照常转发、
	// 金额记 0，UI 挂警示徽章）。与显式 0 价（定价为免费）是两个状态：后者
	// 是一张有键的表。可配字段集按 kind 定，见 usage.FieldsFor。
	Pricing usage.Pricing `json:"pricing"`
	Sources []sourceJSON  `json:"sources"`
}

// modelPricingJSON 把库里的目录价原文摊成对外形态。原文坏了（只可能来自被
// 手工改过的库）按未定价渲染——与计价函数遇到同一张坏表时的处置一致
// （0 元 + 警示徽章），管理台因此不会显示一张连计价都不认的价目表。
func modelPricingJSON(raw string) usage.Pricing {
	p, err := usage.ParsePricing(raw)
	if err != nil {
		return nil
	}
	return p
}

type modelResponse struct {
	Model modelJSON `json:"model"`
}

type sourceResponse struct {
	Source sourceJSON `json:"source"`
}

// kindProtocols 给出各模型种类的协议面全集，顺序固定。
// 文本为三个协议面；视频与图像按模型声明的厂商协议面限制来源。
func kindProtocols(kind string) []string {
	switch kind {
	case store.ModelKindVideo:
		return []string{config.ProtocolArkVideo, config.ProtocolMinimaxVideo}
	case store.ModelKindImage:
		return []string{config.ProtocolArkImage}
	default: // text
		return []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages}
	}
}

// servableProtocols 按运行时口径算出某上游在 kind 圈内能服务的入口协议
// （内置 (type, 协议) 端点表优先于 base_url，见 upstream.Account.Endpoint
// 的说明）。顺序固定，UI 直接按序渲染徽标。
func servableProtocols(kind, upstreamType, baseURL string) []string {
	acct := upstream.Account{Type: upstreamType, BaseURL: baseURL}
	out := make([]string, 0, 3)
	for _, p := range kindProtocols(kind) {
		if _, ok := acct.Endpoint(upstream.CatalogWireProtocol(p)); ok {
			out = append(out, p)
		}
	}
	return out
}

// sourceProtocols 是具体来源的协议能力，测试、徽标与接入读数共用；账号级
// 的种类选单仍用 servableProtocols，不把某个型号的限制当成整个账号的限制。
func sourceProtocols(doc platformcatalog.Doc, kind, model, upstreamModelID, upstreamType, catalogID, baseURL string) []string {
	acct := upstream.Account{Type: upstreamType, BaseURL: baseURL}
	out := make([]string, 0, 3)
	for _, p := range kindProtocols(kind) {
		if _, ok := acct.ModelEndpoint(doc, catalogID, model, upstreamModelID, upstream.CatalogWireProtocol(p)); ok {
			out = append(out, p)
		}
	}
	return out
}

// toSourceJSON 组装来源的对外形态。kind 是所属模型的种类（决定 protocols 的
// 圈定范围）；上游展示字段由调用方从管理视图或 GetUpstreamByID 取得——
// 凭证不参与，这里拿不到也不需要明文。
func toSourceJSON(doc platformcatalog.Doc, kind, model string, src store.ModelSource, upName, upType, upCatalogID, upBillingMode, upBaseURL string, upDisabled bool, upEgressMode string) sourceJSON {
	return sourceJSON{
		ID:                    src.ID,
		ModelID:               src.ModelID,
		UpstreamID:            src.UpstreamID,
		UpstreamName:          upName,
		UpstreamType:          upType,
		UpstreamCatalogID:     upCatalogID,
		UpstreamPlatformLabel: catalogPlatformLabel(doc, upCatalogID, upType),
		UpstreamBillingMode:   upBillingMode,
		UpstreamDisabled:      upDisabled,
		UpstreamEgressMode:    egressModeOf(upEgressMode),
		UpstreamModelID:       src.UpstreamModelID,
		Priority:              src.Priority,
		Disabled:              src.Disabled,
		Protocols:             sourceProtocols(doc, kind, model, src.UpstreamModelID, upType, upCatalogID, upBaseURL),
	}
}

// listModelJSON 取全部模型的嵌套视图。一次联查取完：base_url 随
// ListModelsWithSources 的来源行带回（算 protocols 需要它——mock 靠
// base_url 才有端点），不再额外拉上游列表（那会为算 last4 逐行解密凭证）。
func (s *Server) listModelJSON(ctx context.Context) ([]modelJSON, error) {
	models, err := s.st.ListModelsWithSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]modelJSON, 0, len(models))
	doc, _ := s.effectivePlatformModels(ctx)
	for _, m := range models {
		out = append(out, toModelJSON(doc, m))
	}
	s.annotateAgentModels(ctx, out)
	return out, nil
}

// annotateAgentModels 给模型视图补 agent 注记（modelJSON.Agent 的唯一写点）：
// 索引来自模型目录数据的 agents 段（agentCatalogIndex），成本是一次 settings
// 点读加一遍小文件解析，只在真有模型时才花；索引为空（读取降级）就是无操作。
//
// 两个条件缺一不可：名字在目录的 agents 段里，且这一行**一条来源都没挂**。
// 后者是"归谁"的分水岭——挂了 API 上游的同名行是管理员的 API 模型，照旧在
// API密钥接入标签页按常规渲染（docs/firmware-usage-metering.md「Agent 订阅模型」）。
//
// 注记只给 text 行：订阅内含的图像/视频行由协议面自己认得出来，
// 管理台按 family 就能把它们归到订阅接入标签页。
func (s *Server) annotateAgentModels(ctx context.Context, models []modelJSON) {
	if len(models) == 0 {
		return
	}
	agents := s.agentCatalogIndex(ctx)
	if len(agents) == 0 {
		return
	}
	for i := range models {
		models[i].Agent = agentSubscriptionProvider(agents, models[i].Name, models[i].Kind, len(models[i].Sources))
	}
}

// agentSubscriptionProvider 是「这一行是不是订阅带来的文本模型、属于哪份订阅」
// 的**唯一判据**，非空返回值即 provider。两个消费方：管理台模型视图的 agent
// 注记（上面那个函数）与数据面 `GET /agents/v1/models` 的读数
// （[Server.AgentSubscriptionModels]）。抽出来是因为那两处必须逐字一致——
// 管理台把某一行画在订阅接入标签页上，订阅用户的 CLI 就该在模型清单里
// 看见同一个名字；各写一遍判据，下次改条件必漏一边。
//
// 两个条件缺一不可：名字在目录 agents 段里（那一段只收文本模型），且这一行
// **一条来源都没挂**——挂了 API 上游的同名行是管理员的 API 模型，归 API密钥接入那一侧。
func agentSubscriptionProvider(agents map[string]string, name, kind string, sources int) string {
	if kind != store.ModelKindText || sources > 0 {
		return ""
	}
	return agents[strings.ToLower(name)]
}

// AgentSubscriptionModels 实现数据面的 gateway.AgentModels：给出「模型名 →
// 订阅 provider」，供 `GET /agents/v1/models` 列出订阅带来的文本模型
// （契约见 docs/firmware-agents-codex.md）。判据与管理台订阅接入标签页
// 同源（agentSubscriptionProvider），只多一条数据面自己的收窄：**停用的行不
// 对外广告**——目录里停着的模型调过去也只会被拒，列出来是假信号。
//
// 目录数据读不到时 agentCatalogIndex 已降级为空索引，这里如实回空表（那条
// 端点随之答空列表）：不知道谁属于哪份订阅时，编一个清单比给空清单糟得多。
func (s *Server) AgentSubscriptionModels(ctx context.Context) (map[string]string, error) {
	agents := s.agentCatalogIndex(ctx)
	if len(agents) == 0 {
		return nil, nil
	}
	models, err := s.st.ListModelsWithSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, m := range models {
		if m.Disabled {
			continue
		}
		if p := agentSubscriptionProvider(agents, m.Name, m.Kind, len(m.Sources)); p != "" {
			out[m.Name] = p
		}
	}
	return out, nil
}

// toModelJSON 组装单个模型嵌套视图的对外形态。
func toModelJSON(doc platformcatalog.Doc, m store.ModelWithSources) modelJSON {
	mj := modelJSON{
		ID: m.ID, Name: m.Name, Kind: m.Kind, Family: m.Family, ProtocolFace: config.ProtocolFace(m.Family), Disabled: m.Disabled,
		EntryOpenAI: m.EntryOpenAI, EntryResponses: m.EntryResponses, EntryAnthropic: m.EntryAnthropic,
		Pricing: modelPricingJSON(m.Pricing),
		Sources: make([]sourceJSON, 0, len(m.Sources)),
	}
	for _, src := range m.Sources {
		mj.Sources = append(mj.Sources, toSourceJSON(doc, m.Kind, m.Name, src.ModelSource,
			src.UpstreamName, src.UpstreamType, src.UpstreamCatalogID, src.UpstreamBillingMode,
			src.UpstreamBaseURL, src.UpstreamDisabled, src.UpstreamEgressMode))
	}
	return mj
}

// modelDetail 取单个模型的嵌套视图；不存在返回 store.ErrNotFound。
func (s *Server) modelDetail(ctx context.Context, id int64) (*modelJSON, error) {
	m, err := s.st.GetModelWithSources(ctx, id)
	if err != nil {
		return nil, err
	}
	doc, _ := s.effectivePlatformModels(ctx)
	one := []modelJSON{toModelJSON(doc, *m)}
	s.annotateAgentModels(ctx, one)
	return &one[0], nil
}

// maxPricingFieldName 是错误文案里回显字段名的长度上限（rune）。字段名是
// 调用方给的请求体内容：完全不说是哪个字段管理员就没法改错，原样回显又等于
// 把一段无界的请求体片段搬进响应——取中间，截断并 %q 转义。
const maxPricingFieldName = 32

// parseModelPricing 校验并**归一**一次目录价入参。三态与限额字段同规：
//
//	present=false          字段缺席（PATCH 不改；POST 视为未定价）
//	present=true, ""       显式 null 或 {} = 未定价
//	present=true, JSON     规范化后的价目表原文（键有序，可直接落库）
//
// 形态字段集按模型 kind 圈定，词汇**只有 usage.FieldsFor 那一份**——在这里
// 抄第二份，加一档价就会漏改一处，而漏改的症状是「管理台录得进、计价读不到」。
// 未知字段一律拒绝：静默丢掉一个拼错的字段名，管理员会以为价配好了。
func parseModelPricing(kind string, raw json.RawMessage) (pricing string, present bool, err error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	// 解进 map[string]any 而非 map[string]json.Number：后者会把带引号的
	// "1000" 也照收（json.Number 的底层类型是 string）。
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return "", false, errors.New("须为「价格字段名 → 非负整数微元」的 JSON 对象，或 null（清除定价）")
	}
	if len(fields) == 0 {
		// null 与 {} 都当"清除定价"：空对象若原样落库，就成了一张永远记 0 元
		// 却又不挂「未定价」徽章的隐身价目表（store.validatePricing 同款拒绝）。
		return "", true, nil
	}
	allowed := usage.FieldsFor(kind)
	if len(allowed) == 0 {
		return "", false, fmt.Errorf("模型种类 %s 没有可配置的价格字段", kind)
	}
	allow := make(map[string]bool, len(allowed))
	for _, f := range allowed {
		allow[f] = true
	}
	out := make(map[string]int64, len(fields))
	for field, v := range fields {
		if !allow[field] {
			return "", false, fmt.Errorf("未知价格字段 %s；%s 模型可配：%s",
				clipPricingField(field), kind, strings.Join(allowed, "、"))
		}
		num, ok := v.(json.Number)
		if !ok {
			return "", false, fmt.Errorf("价格字段 %s 须为整数微元（不接受字符串、小数、null）", field)
		}
		n, err := num.Int64()
		if err != nil {
			return "", false, fmt.Errorf("价格字段 %s 须为 int64 内的整数微元", field)
		}
		if n < 0 {
			return "", false, fmt.Errorf("价格字段 %s 不得为负", field)
		}
		if n > store.MaxPricingMicro {
			return "", false, fmt.Errorf("价格字段 %s 超出上限 %d 微元", field, store.MaxPricingMicro)
		}
		out[field] = n
	}
	// 成对字段的词汇与校验只有 usage.PricingPairs 一份（校验工具与本端点共用）。
	if err := usage.CheckPricingPairs(out); err != nil {
		return "", false, err
	}
	// 重新序列化而不是原样落库：入库的字节从此**恰好**是校验过的那张表
	// （键有序、无多余空白、无重复键的歧义）。
	encoded, err := json.Marshal(out)
	if err != nil { // map[string]int64 不可能失败；防御分支
		return "", false, errors.New("目录价无法序列化")
	}
	return string(encoded), true, nil
}

// clipPricingField 把字段名裁成可安全进错误文案的形态（截断 + %q 转义）。
func clipPricingField(field string) string {
	runes := []rune(field)
	if len(runes) > maxPricingFieldName {
		return fmt.Sprintf("%q…", string(runes[:maxPricingFieldName]))
	}
	return fmt.Sprintf("%q", field)
}

// pricingAudit 把目录价渲染成审计 detail 里的摘要（字段名按字典序，未定价
// 记 `-`）。价格是运营参数，如实记录。
func pricingAudit(raw string) string {
	p, err := usage.ParsePricing(raw)
	if err != nil {
		return "invalid"
	}
	if len(p) == 0 {
		return "-"
	}
	fields := make([]string, 0, len(p))
	for f := range p {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%s:%d", f, p[f]))
	}
	return strings.Join(parts, ",")
}

// handleListModels 列出全部模型（名字典序）及其全部来源（含已停用的）。
//
// 数据升级状态由 GET /admin/v1/system/data 单独返回，本端点只给模型目录。
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.listModelJSON(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Models []modelJSON `json:"models"`
	}{Models: models})
}

// handleCreateModel 建模型（name 即客户端可见的原始名；kind 三选一，缺省
// text——既有前端不带 kind 字段，显式非法值仍拒绝）。新模型没有来源，也就
// 不会出现在任何入口的候选里，挂上第一条启用来源后才可服务。
func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
		// Family 是协议面声明（0014，仅 AIGC）：video 二选一（可缺省 = 未声明、
		// 由首条来源懒钉），image 缺省补 ark_image，text 不收。
		Family  string          `json:"family"`
		Pricing json.RawMessage `json:"pricing"`
		// 协议面开关（仅 text；缺省全开）。指针三态：不传 = 取默认。
		EntryOpenAI    *bool `json:"entry_openai"`
		EntryResponses *bool `json:"entry_responses"`
		EntryAnthropic *bool `json:"entry_anthropic"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := validateCatalogName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_name", "模型名不合法："+err.Error())
		return
	}
	kind := req.Kind
	switch kind {
	case "":
		kind = store.ModelKindText
	case store.ModelKindText, store.ModelKindVideo, store.ModelKindImage:
	default:
		writeError(w, http.StatusBadRequest, "invalid_kind",
			fmt.Sprintf("未知模型种类 %q（可选 %s|%s|%s）", req.Kind,
				store.ModelKindText, store.ModelKindVideo, store.ModelKindImage))
		return
	}
	// 协议面声明先校验（0014）：协议面是模型对客户端的 API 格式承诺，只能显式
	// 声明不能推断。text 不收（静默吞掉会让调用方以为设置生效了，同入口
	// 开关的口径）；image 缺省补唯一一个；video 传了就按 kind 圈内校验，
	// 缺省 = 未声明（由首条来源懒钉，兼容不带该字段的旧调用方）。
	family := req.Family
	if kind == store.ModelKindText {
		if family != "" {
			writeError(w, http.StatusBadRequest, "family_text_only", "协议面仅视频/图像模型可声明")
			return
		}
	} else {
		if family == "" && kind == store.ModelKindImage {
			family = config.ProtocolArkImage
		}
		if family != "" && !familyServesKind(kind, family) {
			writeError(w, http.StatusBadRequest, "family_invalid",
				fmt.Sprintf("协议面与模型种类不符（%s 模型可选 %s）", kind, strings.Join(kindProtocols(kind), "|")))
			return
		}
	}
	// 文本侧同理，只是名字才是判据：目录数据点过名的模型是 Agent 订阅模型，
	// 在这里建一条空行没有意义——连上订阅它自己会出现，而收敛器下一轮会把这条
	// 没挂任何上游的同名空行当成自己的行回收掉（那才是真正难查的意外）。
	// 要把这个名字接到 API 上游上，走「API按量计费」/「API订阅套餐」页账号卡片的
	// 「添加模型」：那条路建行连来源一起建，行自此归 API 侧、照旧可编辑。
	if kind == store.ModelKindText {
		if provider, ok := s.agentCatalogIndex(r.Context())[strings.ToLower(req.Name)]; ok {
			writeError(w, http.StatusBadRequest, "model_agent_managed",
				fmt.Sprintf("模型名 %q 属于 %s 订阅、由模型目录数据管理：连上那份订阅它会自动出现；"+
					"要把它接到 API 上游，请在「API按量计费」或「API订阅套餐」页的账号卡片上点「添加模型」", req.Name, provider))
			return
		}
	}
	// 协议面开关先校验后落库：非 text 不收这些字段（AIGC 入口没有开关，
	// 静默吞掉会让调用方以为设置生效了），text 至少留一个开着。
	entryOpenAI, entryResponses, entryAnthropic := true, true, true
	if req.EntryOpenAI != nil {
		entryOpenAI = *req.EntryOpenAI
	}
	// 不带 Responses 字段的创建请求沿用 Chat 的初始设置。
	entryResponses = entryOpenAI
	if req.EntryResponses != nil {
		entryResponses = *req.EntryResponses
	}
	if req.EntryAnthropic != nil {
		entryAnthropic = *req.EntryAnthropic
	}
	if kind != store.ModelKindText && (req.EntryOpenAI != nil || req.EntryResponses != nil || req.EntryAnthropic != nil) {
		writeError(w, http.StatusBadRequest, "entries_text_only", "协议面开关仅文本模型可设")
		return
	}
	if kind == store.ModelKindText && !entryOpenAI && !entryResponses && !entryAnthropic {
		writeError(w, http.StatusBadRequest, "entries_required", "至少开启一个协议面")
		return
	}
	// 目录价可在建模时一并录（缺省未定价——照常转发、金额记 0、管理台挂
	// 警示徽章）。形态字段集按刚定下的 kind 校验。
	pricing, _, perr := parseModelPricing(kind, req.Pricing)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_pricing", "目录价不合法："+perr.Error())
		return
	}
	m, err := s.st.CreateModel(r.Context(), req.Name, kind, pricing)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			writeError(w, http.StatusConflict, "model_name_taken", "模型名已存在")
		default:
			s.writeCatalogError(w, r, err, "模型不存在") // ErrInvalidPricing → 400
		}
		return
	}
	// 建时关某个入口 / 声明协议面是第二条 UPDATE（CreateModel 走表默认）：
	// 新模型没有来源、进不了任何候选，两步之间的窗口对数据面不可观测，
	// 不值得为它给 CreateModel 加参数并连坐全部既有调用点。
	if !entryOpenAI || !entryResponses || !entryAnthropic {
		if err := s.st.SetModelEntries(r.Context(), m.ID, entryOpenAI, entryResponses, entryAnthropic); err != nil {
			s.writeCatalogError(w, r, err, "模型不存在")
			return
		}
		m.EntryOpenAI, m.EntryResponses, m.EntryAnthropic = entryOpenAI, entryResponses, entryAnthropic
	}
	if family != "" {
		if err := s.st.SetModelFamilyIfUnset(r.Context(), m.ID, family); err != nil {
			s.writeCatalogError(w, r, err, "模型不存在")
			return
		}
		m.Family = family
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventModelCreate,
		Entity:   entityModel(m.ID),
		Detail:   fmt.Sprintf("name=%s kind=%s family=%s entries=%s pricing=%s", m.Name, m.Kind, familyAudit(m.Family), entriesLabel(m.EntryOpenAI, m.EntryResponses, m.EntryAnthropic), pricingAudit(m.Pricing)),
		RemoteIP: remoteIP(r),
	})
	one := []modelJSON{{
		ID: m.ID, Name: m.Name, Kind: m.Kind, Family: m.Family, ProtocolFace: config.ProtocolFace(m.Family), Disabled: m.Disabled,
		EntryOpenAI: m.EntryOpenAI, EntryResponses: m.EntryResponses, EntryAnthropic: m.EntryAnthropic,
		Pricing: modelPricingJSON(m.Pricing), Sources: []sourceJSON{},
	}}
	s.annotateAgentModels(r.Context(), one)
	writeJSON(w, http.StatusCreated, modelResponse{Model: one[0]})
}

// familyServesKind 判断 family 是否该 kind 的协议面之一（词汇只有
// kindProtocols 那一份，不抄第二份）。
func familyServesKind(kind, family string) bool {
	for _, p := range kindProtocols(kind) {
		if p == family {
			return true
		}
	}
	return false
}

// familyAudit 是审计 detail 里协议面声明的写法（未声明记 `-`，同 pricingAudit
// 的空值口径）。
func familyAudit(family string) string {
	if family == "" {
		return "-"
	}
	return family
}

// entriesLabel 把协议面开关组合写成审计 detail 里的短语（none 理论不可达
// ——三个协议面全关在写入前就被 entries_required 拒绝）。
func entriesLabel(openai, responses, anthropic bool) string {
	parts := []string{}
	if openai {
		parts = append(parts, config.ProtocolOpenAIChat)
	}
	if responses {
		parts = append(parts, config.ProtocolOpenAIResponses)
	}
	if anthropic {
		parts = append(parts, config.ProtocolAnthropicMessages)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "+")
}

// handlePatchModel 改名与启停。改名改的就是客户端可见名；停用对数据面
// 即时生效（每请求点查，无缓存）。kind 只接受与现值相同的回传，改动即 400
// kind_immutable（照上游 type_immutable 先例——改 kind 等于换模型）。
func (s *Server) handlePatchModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "模型不存在")
		return
	}
	var req struct {
		Name     *string `json:"name"`
		Kind     string  `json:"kind"`   // 仅允许回传现值（见 kind_immutable）
		Family   string  `json:"family"` // 仅允许回传现值（见 family_immutable）
		Disabled *bool   `json:"disabled"`
		// 不传 = 不改价，null / {} = 清为未定价，对象 = 整张表覆盖。
		// 覆盖而非逐字段合并：半张价目表比没有价目表更难发现（一个档位
		// 悄悄按另一档记账），要清哪一档就把它从表里去掉。
		Pricing json.RawMessage `json:"pricing"`
		// 协议面开关（仅 text）。指针三态：不传 = 不改。
		EntryOpenAI    *bool `json:"entry_openai"`
		EntryResponses *bool `json:"entry_responses"`
		EntryAnthropic *bool `json:"entry_anthropic"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	current, err := s.st.GetModelByID(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "模型不存在")
		return
	}
	// Agent 订阅承载的模型**整组只读**（2026-08-15 产品决定，此前只拦改名与
	// 录价）：这批行由模型目录数据定义、由收敛器建删（agentmodels.go），名字是
	// 厂商的官方模型名（改了数据面按新名转发必打不通）、价来自官方价目文件、
	// 启停对订阅流量本就无效。管理台连按钮都不摆，这里是纵深防御。
	managed, err := s.agentCatalogManagedModel(r.Context(), current)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if managed {
		agentCatalogManagedError(w)
		return
	}
	if req.Kind != "" && req.Kind != current.Kind {
		writeError(w, http.StatusBadRequest, "kind_immutable",
			"模型种类不可修改，请删除后按新种类重建")
		return
	}
	// 协议面同 kind：声明即承诺，改协议面等于换模型（客户端要换一套官方路径
	// 与报文）。未声明的存量行也不开 PATCH 口——它由首条来源懒钉（唯一写点）。
	if req.Family != "" && req.Family != current.Family {
		writeError(w, http.StatusBadRequest, "family_immutable",
			"协议面不可修改，请删除后按新协议面重建")
		return
	}
	// 价格先校验后落库：与用户/密钥的限额同一条纪律——不合法就在动第一个
	// 字段之前 400，绝不留下半个已生效的改动。
	pricing, pricingGiven, perr := parseModelPricing(current.Kind, req.Pricing)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "invalid_pricing", "目录价不合法："+perr.Error())
		return
	}
	// 协议面开关同样先校验（不合法时其余字段一个都不动）：非 text 不收，
	// 覆盖后至少留一个开着。
	entryOpenAI, entryResponses, entryAnthropic := current.EntryOpenAI, current.EntryResponses, current.EntryAnthropic
	entriesGiven := req.EntryOpenAI != nil || req.EntryResponses != nil || req.EntryAnthropic != nil
	if entriesGiven {
		if current.Kind != store.ModelKindText {
			writeError(w, http.StatusBadRequest, "entries_text_only", "协议面开关仅文本模型可设")
			return
		}
		if req.EntryOpenAI != nil {
			entryOpenAI = *req.EntryOpenAI
		}
		if req.EntryResponses != nil {
			entryResponses = *req.EntryResponses
		}
		if req.EntryAnthropic != nil {
			entryAnthropic = *req.EntryAnthropic
		}
		if !entryOpenAI && !entryResponses && !entryAnthropic {
			writeError(w, http.StatusBadRequest, "entries_required", "至少开启一个协议面")
			return
		}
	}
	name := current.Name
	if req.Name != nil && *req.Name != current.Name {
		if err := validateCatalogName(*req.Name); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_name", "模型名不合法："+err.Error())
			return
		}
		if err := s.st.RenameModel(r.Context(), id, *req.Name); err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeError(w, http.StatusConflict, "model_name_taken", "模型名已存在")
			} else {
				s.writeCatalogError(w, r, err, "模型不存在")
			}
			return
		}
		name = *req.Name
		s.audit(r.Context(), store.AuditEvent{
			Event:  EventModelRename,
			Entity: entityModel(id), Detail: fmt.Sprintf("name=%s kind=%s renamed_from=%s", name, current.Kind, current.Name),
			RemoteIP: remoteIP(r),
		})
	}
	if pricingGiven && pricing != current.Pricing {
		if err := s.st.SetModelPricing(r.Context(), id, pricing); err != nil {
			s.writeCatalogError(w, r, err, "模型不存在")
			return
		}
		// 改价**只影响其后的请求**（记账时点价），历史行永不重算；视频任务
		// 按清算时点价——这两句是产品口径，不是实现细节，审计里只留新价。
		s.audit(r.Context(), store.AuditEvent{
			Event:    EventModelPricing,
			Entity:   entityModel(id),
			Detail:   fmt.Sprintf("name=%s kind=%s pricing=%s", name, current.Kind, pricingAudit(pricing)),
			RemoteIP: remoteIP(r),
		})
	}
	if entriesGiven && (entryOpenAI != current.EntryOpenAI || entryResponses != current.EntryResponses || entryAnthropic != current.EntryAnthropic) {
		if err := s.st.SetModelEntries(r.Context(), id, entryOpenAI, entryResponses, entryAnthropic); err != nil {
			s.writeCatalogError(w, r, err, "模型不存在")
			return
		}
		// 开关对数据面即时生效（每请求点查）；关掉的入口下一个请求就 404。
		s.audit(r.Context(), store.AuditEvent{
			Event:    EventModelEntries,
			Entity:   entityModel(id),
			Detail:   fmt.Sprintf("name=%s kind=%s entries=%s", name, current.Kind, entriesLabel(entryOpenAI, entryResponses, entryAnthropic)),
			RemoteIP: remoteIP(r),
		})
	}
	if req.Disabled != nil && *req.Disabled != current.Disabled {
		if err := s.st.SetModelDisabled(r.Context(), id, *req.Disabled); err != nil {
			s.writeCatalogError(w, r, err, "模型不存在")
			return
		}
		event := EventModelEnable
		if *req.Disabled {
			event = EventModelDisable
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:  event,
			Entity: entityModel(id), Detail: fmt.Sprintf("name=%s kind=%s", name, current.Kind),
			RemoteIP: remoteIP(r),
		})
	}
	detail, err := s.modelDetail(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "模型不存在")
		return
	}
	writeJSON(w, http.StatusOK, modelResponse{Model: *detail})
}

// handleDeleteModel 删模型，其全部来源随外键级联删除（UI 需在确认框里
// 明示这一点）；上游不受影响。
func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "模型不存在")
		return
	}
	detail, err := s.modelDetail(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "模型不存在")
		return
	}
	// 只读守卫同 PATCH：撤掉一个订阅模型的办法是撤掉那份订阅（收敛器随即
	// 回收这些行），不是在模型列表里删一行——删了下次收敛还会长回来。
	current, err := s.st.GetModelByID(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "模型不存在")
		return
	}
	managed, err := s.agentCatalogManagedModel(r.Context(), current)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if managed {
		agentCatalogManagedError(w)
		return
	}
	if err := s.st.DeleteModel(r.Context(), id); err != nil {
		s.writeCatalogError(w, r, err, "模型不存在")
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventModelDelete,
		Entity:   entityModel(id),
		Detail:   fmt.Sprintf("name=%s kind=%s sources=%d", detail.Name, detail.Kind, len(detail.Sources)),
		RemoteIP: remoteIP(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleCreateModelSource 给模型挂一条来源。upstream_model_id 留空即"与模型名
// 相同"；priority 缺省 100（小者优先）。同一 (模型, 上游) 只允许一条来源。
func (s *Server) handleCreateModelSource(w http.ResponseWriter, r *http.Request) {
	modelID, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "模型不存在")
		return
	}
	var req struct {
		UpstreamID      int64  `json:"upstream_id"`
		UpstreamModelID string `json:"upstream_model_id"`
		Priority        *int64 `json:"priority"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UpstreamID <= 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "缺少 upstream_id")
		return
	}
	if req.UpstreamModelID != "" {
		if err := validateCatalogName(req.UpstreamModelID); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_upstream_model_id",
				"来源侧模型 ID 不合法："+err.Error())
			return
		}
	}
	model, err := s.st.GetModelByID(r.Context(), modelID)
	if err != nil {
		s.writeCatalogError(w, r, err, "模型不存在")
		return
	}
	up, err := s.st.GetUpstreamByID(r.Context(), req.UpstreamID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "upstream_not_found", "指定的上游不存在")
		} else {
			s.internalError(w, r, err)
		}
		return
	}
	// kind≠text 的来源守卫（0014 起对**声明的协议面**校验）：一个 AIGC 模型固定
	// 一种 API 格式（源头厂商官方接口），所以它的每条来源都必须服务该 kind 的
	// 协议面，且协议面等于模型声明——异面「多上游」等于让同一个模型分裂在两套
	// 客户端路径上，正是纯转发要消灭的形态。文本模型不在此列（文本三协议是行业
	// 通形，入口按协议各自过滤）。写入时校验而非选路时过滤：死配置当场报错，
	// 不留到客户端 404 才被发现。
	if model.Kind != store.ModelKindText {
		fam := servableProtocols(model.Kind, up.Type, up.BaseURL)
		if len(fam) == 0 {
			writeError(w, http.StatusBadRequest, "source_kind_unservable",
				"该上游类型不服务此模型种类的任何协议面，无法作为来源")
			return
		}
		// 每个上游类型在一个 kind 圈内至多服务一个协议面（内置端点表如此），
		// fam[0] 即这条来源的协议面。未声明的存量行（迁移回填不到的无来源 video
		// 模型）由第一条来源懒钉——SetModelFamilyIfUnset 带 family='' 守卫；
		// 并发懒钉出异值收敛为协议面冲突。
		if model.Family == "" {
			if err := s.st.SetModelFamilyIfUnset(r.Context(), model.ID, fam[0]); err != nil {
				if errors.Is(err, store.ErrConflict) {
					writeError(w, http.StatusConflict, "source_family_mismatch",
						"该上游走的协议面与模型的协议面不符——模型的全部上游必须服务它声明的协议面")
				} else {
					s.internalError(w, r, err)
				}
				return
			}
			model.Family = fam[0]
		} else if fam[0] != model.Family {
			writeError(w, http.StatusConflict, "source_family_mismatch",
				"该上游走的协议面与模型声明的协议面不符——模型的全部上游必须服务它声明的协议面")
			return
		}
	}
	priority := defaultSourcePriority
	if req.Priority != nil {
		priority = *req.Priority
	}
	src, err := s.st.CreateModelSource(r.Context(), modelID, req.UpstreamID, req.UpstreamModelID, priority)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// 模型与上游都刚查过存在，冲突几乎只可能是 UNIQUE(model_id, upstream_id)。
			writeError(w, http.StatusConflict, "source_exists",
				"该模型已在这个上游上有来源，请直接编辑它")
		} else {
			s.internalError(w, r, err)
		}
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventSourceCreate,
		Entity: entitySource(src.ID),
		Detail: fmt.Sprintf("model=%s kind=%s upstream=%s type=%s upstream_model_id=%s priority=%d",
			model.Name, model.Kind, up.Name, up.Type, src.UpstreamModelID, src.Priority),
		RemoteIP: remoteIP(r),
	})
	doc, _ := s.effectivePlatformModels(r.Context())
	writeJSON(w, http.StatusCreated, sourceResponse{
		Source: toSourceJSON(doc, model.Kind, model.Name, *src, up.Name, up.Type, up.CatalogID, up.BillingMode, up.BaseURL, up.Disabled, up.EgressMode)})
}

// handlePatchModelSource 改来源的优先级 / 来源侧模型 ID / 启停。上游归属不可改
// （换上游即删了重挂——UNIQUE(model_id, upstream_id) 决定它本就是另一条来源）。
func (s *Server) handlePatchModelSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "模型来源不存在")
		return
	}
	var req struct {
		UpstreamModelID *string `json:"upstream_model_id"`
		Priority        *int64  `json:"priority"`
		Disabled        *bool   `json:"disabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	current, err := s.st.GetModelSourceByID(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "模型来源不存在")
		return
	}
	// 模型行与上游行在写入**之前**取齐（外键保证两行都在）：审计 detail 与
	// 响应都要它们，放到写入之后取会让"库已改、取名失败 → 500"变成一次没有
	// 审计记录的变更（客户端中途断开就足以触发）。
	model, err := s.st.GetModelByID(r.Context(), current.ModelID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	up, err := s.st.GetUpstreamByID(r.Context(), current.UpstreamID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	upstreamModelID, priority := current.UpstreamModelID, current.Priority
	if req.UpstreamModelID != nil {
		if *req.UpstreamModelID != "" {
			if err := validateCatalogName(*req.UpstreamModelID); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_upstream_model_id",
					"来源侧模型 ID 不合法："+err.Error())
				return
			}
		}
		upstreamModelID = *req.UpstreamModelID
	}
	if req.Priority != nil {
		priority = *req.Priority
	}
	changed := upstreamModelID != current.UpstreamModelID || priority != current.Priority
	if changed {
		if err := s.st.UpdateModelSource(r.Context(), id, upstreamModelID, priority); err != nil {
			s.writeCatalogError(w, r, err, "模型来源不存在")
			return
		}
	}
	disabled := current.Disabled
	if req.Disabled != nil && *req.Disabled != current.Disabled {
		if err := s.st.SetModelSourceDisabled(r.Context(), id, *req.Disabled); err != nil {
			s.writeCatalogError(w, r, err, "模型来源不存在")
			return
		}
		disabled = *req.Disabled
		changed = true
	}
	if changed {
		s.audit(r.Context(), store.AuditEvent{
			Event:  EventSourceUpdate,
			Entity: entitySource(id),
			Detail: fmt.Sprintf("model=%s kind=%s upstream=%s type=%s upstream_model_id=%s priority=%d disabled=%t",
				model.Name, model.Kind, up.Name, up.Type, upstreamModelID, priority, disabled),
			RemoteIP: remoteIP(r),
		})
	}
	updated := *current
	updated.UpstreamModelID, updated.Priority, updated.Disabled = upstreamModelID, priority, disabled
	doc, _ := s.effectivePlatformModels(r.Context())
	writeJSON(w, http.StatusOK, sourceResponse{
		Source: toSourceJSON(doc, model.Kind, model.Name, updated, up.Name, up.Type, up.CatalogID, up.BillingMode, up.BaseURL, up.Disabled, up.EgressMode)})
}

// handleDeleteModelSource 删来源；模型与上游都不受影响。
func (s *Server) handleDeleteModelSource(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "模型来源不存在")
		return
	}
	current, err := s.st.GetModelSourceByID(r.Context(), id)
	if err != nil {
		s.writeCatalogError(w, r, err, "模型来源不存在")
		return
	}
	// 审计要写模型名与上游名，删之前先取齐（同 handlePatchModelSource：
	// 写入之后才失败的取名会让这次删除没有审计记录）。
	model, err := s.st.GetModelByID(r.Context(), current.ModelID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	up, err := s.st.GetUpstreamByID(r.Context(), current.UpstreamID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.st.DeleteModelSource(r.Context(), id); err != nil {
		s.writeCatalogError(w, r, err, "模型来源不存在")
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventSourceDelete,
		Entity:   entitySource(id),
		Detail:   fmt.Sprintf("model=%s kind=%s upstream=%s type=%s priority=%d", model.Name, model.Kind, up.Name, up.Type, current.Priority),
		RemoteIP: remoteIP(r),
	})
	w.WriteHeader(http.StatusNoContent)
}
