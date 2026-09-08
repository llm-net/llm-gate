// Package platformcatalog 持「平台模型信息」——哪儿有哪些模型。文件里是两段，
// **分量不同**：
//
//	platforms  哪个上游平台有哪些可选模型 → 「添加模型」对话框的一份选单
//	agents     哪份 Agent 订阅带哪些模型 → 设备照着建行的权威清单（v2，2026-08-15）
//
// 它是管理台「模型接入 → API密钥接入 → 添加模型」那份选单的数据源：录完账号之后，
// 操作者要挑的是"这家平台上的哪个模型"，而设备本身无从枚举——厂商没有统一的
// 模型枚举端点，各家的型号也随时上新。于是把这份清单做成一份**可发布的数据
// 文件**，而不是编译进代码的常量表。
//
// 一个维护源、两份副本，副本都必须在场：
//
//	维护源  公开发布仓库 github.com/llm-net/llm-gate 的 catalog/platform-models.json
//	    （同目录的 README.md 与任务文档说明维护流程）。改动只在那里做，用
//	    `make -C firmware catalog` 校验并复制到下面两处。
//	内嵌（本包 platform-models.json，go:embed）  设备的**基线**。一台从未联网
//	    的设备照样要选得出模型，所以基线随固件发布、开箱即在。
//	官网（website/public/updates/data/platform-models.json）  同一份文件的逐字节
//	    拷贝，供设备在「数据升级」时取更新的版本——厂商上新不必等固件升级。
//	    三份逐字节一致由 platformcatalog_test.go 与 internal/catalogcheck 的
//	    绊线测试把守（同 codexhelper 的口径）。
//
// 同步下来的版本**号大者胜**（比较 version，见 admin 侧的挑选逻辑）：官网文件
// 只会比内嵌的新，回退到旧版本没有产品意义，比日期更不会踩到时钟问题。
//
// 与同目录那份 official-pricing.json 的分工：那份记「一个模型多少钱」（写进
// models.pricing，影响记账），这份记「哪儿有哪些模型」。两份一次同步一起下载。
//
// agents 段（2026-08-15）打破了本包"只是选单、不影响任何运行期判定"的旧口径，
// 这一点必须说在明处：**订阅带哪些模型不该由管理员一个个点进来**——那些型号是
// 厂商定的，设备只是照着显示。于是订阅一连上，internal/admin 的收敛器就按这一段
// 把模型行建出来（管理台对它们不提供任何编辑入口），厂商上新改这份文件即可，
// 不必等固件升级。Cursor 在本文件按独立 ID 记价，其余订阅取官方价目文件。
//
// 本包只做**结构**校验（形态标识、条目数、字节上限），与 officialsite 对价目文件
// 的处置同一条纪律：模型名合不合目录规范、kind/协议面对不对，由 internal/admin
// 用与手工建模**同一条**校验路径判定——目录词汇的合法性只该有一份定义。
package platformcatalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sync"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/cursorwire"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const (
	// Schema 是文件的形态标识。校验它不是形式主义：静态托管对未知路径可能
	// 做页面 fallback，文件缺失时拿回的是 HTTP 200 的 index.html——没有这道
	// 闸，"文件没发布"会表现成一次语焉不详的解析失败。
	Schema = "llmgate.platform-models/v2"

	// MaxBytes 是下载（也是落库）的字节上限。当前文件不到 5 KiB，256 KiB 是
	// 五十倍余量；上限防的是配置错到别的地址时无界读。
	MaxBytes = 256 << 10

	// MaxPlatforms / maxModelsPerPlatform 挡的是形态正确但异常巨大的输入——
	// 清单要整份渲染进一个对话框，条目数没有上限就等于把界面交给远端文件。
	// 按量、套餐、通用适配三组平台同住 platforms 段，一家厂商的按量与套餐、
	// 不同地域、不同协议的入口各占一条，64 给两倍余量。
	MaxPlatforms         = 64
	maxModelsPerPlatform = 300

	// maxAgents / maxModelsPerAgent 同理，但收得更紧：agents 段不是选单而是
	// 设备照着建行的**权威清单**（2026-08-15），一份错得离谱的文件在这里的
	// 代价是几百行凭空出现的模型目录行，不是一个长对话框。
	maxAgents         = 16
	maxModelsPerAgent = 100
)

const (
	AdapterDeepSeek        = "deepseek"
	AdapterArk             = "ark"
	AdapterArkPlan         = "ark_plan"
	AdapterQwenPlan        = "qwen_plan"
	AdapterOpenCodeGo      = "opencode_go"
	AdapterMiniMax         = "minimax"
	AdapterOpenAICompat    = "openai_compat"
	AdapterAnthropicCompat = "anthropic_compat"

	BillingUsage        = "usage"
	BillingSubscription = "subscription"
	BillingNone         = "none"

	ResponsesProfileReasoningReplayV1 = "reasoning_replay_v1"
)

var capabilityNameRE = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// Doc 是平台模型信息文件的解析结果。
type Doc struct {
	Schema    string     `json:"schema"`
	Version   int64      `json:"version"`
	UpdatedAt string     `json:"updated_at"`
	Platforms []Platform `json:"platforms"`
	// Agents 是「哪份 Agent 订阅带哪些模型」（v2，2026-08-15）。旧文件没有
	// 这一段，解析出空切片即可——设备那时什么都不建，与升级前同形。
	Agents []Agent `json:"agents"`
}

// Platform 是一个上游平台的条目。Type 是固件稳定适配器标识；未知取值整份
// 拒绝，继续使用上一份可用目录。数据新增平台只能用独立 ID 复用通用兼容
// 适配器。Source/CheckedAt 是收录纪律的落点。
type Platform struct {
	// ID 是数据目录里的平台身份；Type 是固件内置的稳定协议适配器。两者分开后，
	// 多家 OpenAI-compatible 平台可以共享一个适配器，同时各有自己的模型清单
	// 与端点快照。v1 文件没有 id，解析时按 type 补齐。
	ID            string  `json:"id"`
	Type          string  `json:"type"`
	Vendor        string  `json:"vendor"`
	BaseURL       string  `json:"base_url"`
	CustomBaseURL bool    `json:"custom_base_url"`
	BillingMode   string  `json:"billing_mode"`
	SuggestedName string  `json:"suggested_name"`
	Entries       string  `json:"entries"`
	Ability       string  `json:"ability"`
	Source        string  `json:"source"`
	CheckedAt     string  `json:"checked_at"`
	Note          string  `json:"note"`
	Models        []Model `json:"models"`
}

// Model 是清单里的一个可选模型。Name 是客户端请求里的 model 值（也就是模型在
// 设备目录里的名字）；UpstreamModelID 只在上游那边的真实 ID 与 Name 不同时才
// 有值（空 = 与模型名相同，同 store 的口径）。Family 是 AIGC 的协议面声明，
// text 恒空——实际写库时以「这条账号服务哪个协议面」为准（admin 侧推导），
// 本字段只作清单展示与一致性校验。
type Model struct {
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	Family          string            `json:"family"`
	UpstreamModelID string            `json:"upstream_model_id"`
	Note            string            `json:"note"`
	Capabilities    ModelCapabilities `json:"capabilities"`
	// UpstreamProtocols 声明这个来源侧模型实际接受的上游协议。省略时沿用
	// 适配器能力；空数组表示没有可用协议。声明只能收窄适配器，不能增添转换算法。
	UpstreamProtocols []string `json:"upstream_protocols"`
}

// ModelCapabilities 是可通过数据升级发布的模型行为。它只组合固件已经实现、
// 有界且经过测试的能力，不是远程脚本或通用表达式语言。需要一种全新转换算法时
// 必须先由固件实现新的 profile；旧固件会拒绝整个未知 profile 的目录并继续用
// 上一份可用数据。
type ModelCapabilities struct {
	ResponsesChat ResponsesChatCapabilities `json:"responses_chat"`
	Codex         CodexCapabilities         `json:"codex"`
}

type ResponsesChatCapabilities struct {
	Profile                string            `json:"profile"`
	ReplayReasoningContent bool              `json:"replay_reasoning_content"`
	ThinkingType           string            `json:"thinking_type"`
	DropToolChoice         bool              `json:"drop_tool_choice"`
	EffortMap              map[string]string `json:"effort_map"`
}

type CodexCapabilities struct {
	DefaultReasoningLevel    string                `json:"default_reasoning_level"`
	SupportedReasoningLevels []CodexReasoningLevel `json:"supported_reasoning_levels"`
}

type CodexReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// Agent 是一份 Agent 订阅带的模型清单（2026-08-15）。Provider 是设备侧的订阅
// 标识（store.AgentProvider* 同域：codex | grok | claude | cursor）；不认识的
// 取值由消费方忽略——新 provider 不该炸旧固件。
//
// 与 Platform 的**分量不同**，这是这一段唯一要记住的事：Platform 是「添加模型」
// 对话框里的一份选单，进目录还要管理员点一下并过一遍建模校验；Agent 是设备照着
// 它自动建行的权威清单——订阅一连上，这里的模型就出现在管理台的订阅接入标签页
// 上，管理员改不动（细则见 docs/firmware-usage-metering.md「Agent 订阅模型」）。
type Agent struct {
	Provider  string       `json:"provider"`
	Vendor    string       `json:"vendor"`
	Source    string       `json:"source"`
	CheckedAt string       `json:"checked_at"`
	Note      string       `json:"note"`
	Models    []AgentModel `json:"models"`
}

// AgentModel 是订阅清单里的一个模型，只收文本（Kind 恒为 text，设备侧建行时
// 校验，其余种类当发布错误跳过）。没有 UpstreamModelID：订阅代理按客户端送来的
// model 名分流/转发，不存在"来源侧另一个 ID"这回事。Source/CheckedAt 可逐条
// 覆盖 provider 那条。
//
// Codex/Grok/Claude 按 official-pricing.json 同名条目记名义金额；Cursor
// 使用本条 Pricing，避免与 API 或其他订阅的同名型号串价，也不创建共享模型行。
type AgentModel struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Note      string          `json:"note"`
	Source    string          `json:"source"`
	CheckedAt string          `json:"checked_at"`
	Pricing   json.RawMessage `json:"pricing,omitempty"`
}

//go:embed platform-models.json
var builtinRaw []byte

// builtin 解析内嵌副本。内嵌文件是编译期资产，解析不了是构建错误而不是运行期
// 状况——那时 panic 比让「添加模型」悄悄空着好（绊线测试 TestBuiltinParses
// 把这次 panic 提前到 CI）。
var builtin = sync.OnceValue(func() Doc {
	doc, err := Parse(builtinRaw)
	if err != nil {
		panic("platformcatalog: 内嵌平台模型文件损坏：" + err.Error())
	}
	return doc
})

// Builtin 返回随固件发布的基线清单。
func Builtin() Doc { return builtin() }

// BuiltinRaw 返回内嵌副本的原文（拷贝，调用方改不动内嵌资产）。
func BuiltinRaw() []byte {
	out := make([]byte, len(builtinRaw))
	copy(out, builtinRaw)
	return out
}

// Parse 解析并**结构**校验一份平台模型信息文件。
func Parse(raw []byte) (Doc, error) {
	var doc Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Doc{}, errors.New("内容不是合法 JSON")
	}
	if doc.Schema != Schema {
		return Doc{}, fmt.Errorf("形态不符：期望 %q，实际 %q", Schema, clipRunes(doc.Schema, 64))
	}
	if doc.Version <= 0 {
		return Doc{}, errors.New("version 必须是正整数")
	}
	if len(doc.Platforms) == 0 {
		return Doc{}, errors.New("一个平台条目都没有")
	}
	if len(doc.Platforms) > MaxPlatforms {
		return Doc{}, fmt.Errorf("平台条目数超过上限 %d", MaxPlatforms)
	}
	platformIDs := make(map[string]struct{}, len(doc.Platforms))
	for i := range doc.Platforms {
		p := &doc.Platforms[i]
		if p.ID == "" {
			p.ID = p.Type
		}
		if p.BillingMode == "" {
			p.BillingMode = defaultBillingMode(p.Type)
		}
		if err := validatePlatform(*p, doc.Schema == Schema); err != nil {
			return Doc{}, err
		}
		if _, exists := platformIDs[p.ID]; exists {
			return Doc{}, fmt.Errorf("平台 id %q 重复", clipRunes(p.ID, 32))
		}
		platformIDs[p.ID] = struct{}{}
		if len(p.Models) > maxModelsPerPlatform {
			return Doc{}, fmt.Errorf("平台 %q 的模型条目数超过上限 %d", clipRunes(p.Type, 32), maxModelsPerPlatform)
		}
		modelNames := make(map[string]struct{}, len(p.Models))
		for _, m := range p.Models {
			if _, exists := modelNames[m.Name]; exists {
				return Doc{}, fmt.Errorf("平台 %q 的模型 %q 重复", clipRunes(p.ID, 32), clipRunes(m.Name, 64))
			}
			modelNames[m.Name] = struct{}{}
			seenProtocols := make(map[string]bool)
			for _, protocol := range m.UpstreamProtocols {
				if m.Kind != "text" || (protocol != config.ProtocolOpenAIChat && protocol != config.ProtocolOpenAIResponses && protocol != config.ProtocolAnthropicMessages) || seenProtocols[protocol] {
					return Doc{}, fmt.Errorf("平台 %q 的模型 %q 的 upstream_protocols 无效", clipRunes(p.ID, 32), clipRunes(m.Name, 64))
				}
				seenProtocols[protocol] = true
			}
			if err := validateCapabilities(p.ID, m); err != nil {
				return Doc{}, err
			}
		}
	}
	// agents 段整段可缺（v1 的文件就没有）：缺席不是错误，只是这台设备的
	// 订阅接入标签页暂时列不出模型。
	if len(doc.Agents) > maxAgents {
		return Doc{}, fmt.Errorf("订阅条目数超过上限 %d", maxAgents)
	}
	cursorSeen := false
	for _, a := range doc.Agents {
		if len(a.Models) > maxModelsPerAgent {
			return Doc{}, fmt.Errorf("订阅 %q 的模型条目数超过上限 %d", clipRunes(a.Provider, 32), maxModelsPerAgent)
		}
		if a.Provider != "cursor" {
			if a.Provider != "codex" && a.Provider != "grok" && a.Provider != "claude" {
				continue // 不认识的订阅仍由旧固件忽略。
			}
			for _, model := range a.Models {
				if len(model.Pricing) > 0 {
					return Doc{}, errors.New("只有 Cursor 订阅在平台目录中定义价格")
				}
			}
			continue
		}
		if cursorSeen {
			return Doc{}, errors.New("Cursor 订阅目录重复")
		}
		cursorSeen = true
		names := make(map[string]bool)
		for _, model := range a.Models {
			if !cursorwire.ValidModel(model.Name) || model.Kind != "text" || names[model.Name] {
				return Doc{}, errors.New("Cursor 目录模型标识、类型无效或重复")
			}
			names[model.Name] = true
			if _, err := usage.ParseCursorPrice(string(model.Pricing)); err != nil {
				return Doc{}, err
			}
		}
	}
	return doc, nil
}

func validatePlatform(p Platform, strictV2 bool) error {
	if !capabilityNameRE.MatchString(p.ID) {
		return fmt.Errorf("平台 id %q 不合法", clipRunes(p.ID, 32))
	}
	switch p.Type {
	case AdapterDeepSeek, AdapterArk, AdapterArkPlan, AdapterQwenPlan, AdapterOpenCodeGo, AdapterMiniMax,
		AdapterOpenAICompat, AdapterAnthropicCompat:
	default:
		return fmt.Errorf("平台 %q 使用了固件不支持的适配器 %q", clipRunes(p.ID, 32), clipRunes(p.Type, 32))
	}
	switch p.BillingMode {
	case BillingUsage, BillingSubscription, BillingNone:
	default:
		return fmt.Errorf("平台 %q 的 billing_mode %q 不合法", clipRunes(p.ID, 32), clipRunes(p.BillingMode, 32))
	}
	if p.BaseURL != "" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("平台 %q 的 base_url 必须是无凭据、查询串与 fragment 的 HTTPS 端点根", clipRunes(p.ID, 32))
		}
	}
	if strictV2 && p.ID != p.Type {
		if p.Type != AdapterOpenAICompat && p.Type != AdapterAnthropicCompat {
			return fmt.Errorf("数据新增平台 %q 只能复用通用兼容适配器", clipRunes(p.ID, 32))
		}
		if p.BaseURL == "" || p.CustomBaseURL {
			return fmt.Errorf("数据新增平台 %q 必须给出固定 HTTPS base_url", clipRunes(p.ID, 32))
		}
	}
	if p.CustomBaseURL && p.Type != AdapterOpenAICompat && p.Type != AdapterAnthropicCompat {
		return fmt.Errorf("平台 %q 的适配器不允许管理员自填 base_url", clipRunes(p.ID, 32))
	}
	return nil
}

func validateCapabilities(platformID string, m Model) error {
	r := m.Capabilities.ResponsesChat
	if r.Profile == "" {
		if r.ReplayReasoningContent || r.ThinkingType != "" || r.DropToolChoice || len(r.EffortMap) != 0 {
			return fmt.Errorf("平台 %q 的模型 %q 配了 Responses 行为但没有 profile", clipRunes(platformID, 32), clipRunes(m.Name, 64))
		}
	} else if r.Profile != ResponsesProfileReasoningReplayV1 {
		return fmt.Errorf("平台 %q 的模型 %q 需要固件不支持的 Responses profile %q", clipRunes(platformID, 32), clipRunes(m.Name, 64), clipRunes(r.Profile, 64))
	}
	if r.ThinkingType != "" && r.ThinkingType != "enabled" {
		return fmt.Errorf("平台 %q 的模型 %q 使用了不支持的 thinking_type", clipRunes(platformID, 32), clipRunes(m.Name, 64))
	}
	for from, to := range r.EffortMap {
		if !capabilityNameRE.MatchString(from) || !capabilityNameRE.MatchString(to) {
			return fmt.Errorf("平台 %q 的模型 %q effort_map 含非法档位", clipRunes(platformID, 32), clipRunes(m.Name, 64))
		}
	}
	c := m.Capabilities.Codex
	if c.DefaultReasoningLevel != "" && !capabilityNameRE.MatchString(c.DefaultReasoningLevel) {
		return fmt.Errorf("平台 %q 的模型 %q Codex 默认 effort 不合法", clipRunes(platformID, 32), clipRunes(m.Name, 64))
	}
	if len(c.SupportedReasoningLevels) > 16 {
		return fmt.Errorf("平台 %q 的模型 %q Codex effort 档位超过上限 16", clipRunes(platformID, 32), clipRunes(m.Name, 64))
	}
	seen := make(map[string]struct{}, len(c.SupportedReasoningLevels))
	for _, level := range c.SupportedReasoningLevels {
		if !capabilityNameRE.MatchString(level.Effort) || len([]rune(level.Description)) > 160 {
			return fmt.Errorf("平台 %q 的模型 %q Codex effort 条目不合法", clipRunes(platformID, 32), clipRunes(m.Name, 64))
		}
		if _, exists := seen[level.Effort]; exists {
			return fmt.Errorf("平台 %q 的模型 %q Codex effort %q 重复", clipRunes(platformID, 32), clipRunes(m.Name, 64), clipRunes(level.Effort, 32))
		}
		seen[level.Effort] = struct{}{}
	}
	if c.DefaultReasoningLevel != "" {
		if _, ok := seen[c.DefaultReasoningLevel]; !ok {
			return fmt.Errorf("平台 %q 的模型 %q Codex 默认 effort 不在支持列表里", clipRunes(platformID, 32), clipRunes(m.Name, 64))
		}
	}
	return nil
}

func defaultBillingMode(adapter string) string {
	switch adapter {
	case AdapterArkPlan, AdapterQwenPlan, AdapterOpenCodeGo:
		return BillingSubscription
	case AdapterDeepSeek, AdapterArk, AdapterMiniMax:
		return BillingUsage
	default:
		return BillingNone
	}
}

// Platform 取某个上游类型的平台条目；没有收录时返回 false（调用方据此只提供
// 「自定义」——一个平台没进清单不是错误，是"还没核对官方型号表"）。
// 同一个 type 出现多次时取第一条：文件是人工维护的，重复条目属发布错误，
// 取第一条与「按顺序读」的直觉一致。
func (d Doc) Platform(upstreamType string) (Platform, bool) {
	for _, p := range d.Platforms {
		if p.Type == upstreamType {
			return p, true
		}
	}
	return Platform{}, false
}

// PlatformByID 取数据目录里的具体平台。catalogID 为空时退回适配器身份，兼容
// 迁移前建立的上游账号。
func (d Doc) PlatformByID(catalogID, adapter string) (Platform, bool) {
	if catalogID == "" {
		catalogID = adapter
	}
	for _, p := range d.Platforms {
		if p.ID == catalogID {
			return p, true
		}
	}
	return Platform{}, false
}

// ModelProtocolsFor 只按实际发往上游的模型 ID 查协议声明。显式映射优先于
// 逻辑名，不能把同名逻辑模型的能力借给另一个上游型号。
func (d Doc) ModelProtocolsFor(catalogID, adapter, logicalModel, upstreamModelID string) []string {
	if upstreamModelID == "" {
		upstreamModelID = logicalModel
	}
	p, ok := d.PlatformByID(catalogID, adapter)
	if !ok {
		return nil
	}
	for _, m := range p.Models {
		id := m.UpstreamModelID
		if id == "" {
			id = m.Name
		}
		if id == upstreamModelID {
			return m.UpstreamProtocols
		}
	}
	return nil
}

// ModelCapabilitiesFor 按具体平台与来源侧模型 ID 找能力；找不到来源侧 ID 时
// 再按客户端可见名找，兼容逻辑模型别名与同名直连两种建模方式。
func (d Doc) ModelCapabilitiesFor(catalogID, adapter, logicalModel, upstreamModelID string) (ModelCapabilities, bool) {
	p, ok := d.PlatformByID(catalogID, adapter)
	if !ok {
		return ModelCapabilities{}, false
	}
	for _, name := range []string{upstreamModelID, logicalModel} {
		if name == "" {
			continue
		}
		for _, m := range p.Models {
			if m.Name == name || (m.UpstreamModelID != "" && m.UpstreamModelID == name) {
				return m.Capabilities, true
			}
		}
	}
	return ModelCapabilities{}, false
}

// Agent 取某份订阅的模型清单；没有收录时返回 false。收录不等于成员可见集：
// cursor 的条目只供计价与展示，成员侧模型由 CLI 经订阅面自发现。
// 同一个 provider 出现多次时取第一条，同 Platform 的口径。
func (d Doc) Agent(provider string) (Agent, bool) {
	for _, a := range d.Agents {
		if a.Provider == provider {
			return a, true
		}
	}
	return Agent{}, false
}

// clipRunes 裁截回显给错误文案的远端字段：这些值来自外部文件，原样搬进错误
// 就等于把一段无界的远端文本搬进日志。
func clipRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
