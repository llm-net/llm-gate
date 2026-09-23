package modeldata

// 「数据仓库的产品 → 设备的平台 / 订阅」映射。数据仓库不按云平台或盒子分区，
// 也不知道固件有哪些适配器；接到哪个适配器、走哪个端点根、算哪种计费模式，
// 只在这里定。改映射就是改产品口径，随源码评审。
//
// 一个产品可以拆成多个平台（同一把 Key 的 OpenAI 兼容面与 Anthropic Messages 面
// 各占一条：Key 与端点互不通用时决不合并），多个产品也可以并入一个平台
// （方舟 Agent Plan 与 Coding Plan 都在 /api/plan/v3 那条根上，固件只有一个 ark_plan
// 适配器）。没有映射的产品**不进文件**并在报告里点名；没有对应产品的平台不进
// 文件（通用兼容适配除外，它们本就没有选单）。

import (
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// platformSpec 是一个设备平台的静态档案。Protocols 是这条平台承载的协议面
// （固件 kindProtocols 的词汇）：一个模型的 capabilities.interfaces 与它有交集才进
// 这条平台的选单，upstream_protocols 写交集（文本三面）；AIGC 的 family 取交集里的
// 那一个厂商面。
type platformSpec struct {
	ID            string
	Type          string
	Vendor        string
	BaseURL       string
	BillingMode   string
	SuggestedName string
	Entries       string
	Ability       string
	Note          string
	Protocols     []string
}

// offeringTarget 是数据仓库一个产品在设备上的落点：平台（可多条）或订阅。
type offeringTarget struct {
	Platforms []string // platformSpec.ID
	Agent     string   // store.AgentProvider*
}

var (
	textProtocols     = []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages}
	openAIProtocols   = []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses}
	anthropicProtocol = []string{config.ProtocolAnthropicMessages}
)

// platformSpecs 的**先后就是文件里 platforms 段的先后**，也是管理台各组内的展示顺序：
// 按量 → 套餐 → 通用适配。
var platformSpecs = []platformSpec{
	// ---- API 按量计费 ----
	{ID: "deepseek", Type: config.UpstreamDeepseek, Vendor: "DeepSeek", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "deepseek", Entries: "OpenAI 兼容 与 Anthropic 兼容 双入口", Ability: "支持查询平台余额。",
		Note: "官方按高峰 / 空闲时段分档计价（含周末全天空闲价），分时段价见各模型的 schedule 段。",
		Protocols: textProtocols},
	{ID: "ark", Type: config.UpstreamArk, Vendor: "火山方舟", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "ark", Entries: "OpenAI 兼容单入口；另可挂给视频与图像模型", Ability: "平台余额需要火山引擎 AK/SK 签名，设备不查询。",
		Note: "方舟同时托管豆包自有模型与 DeepSeek 等第三方模型；型号按方舟官方 Model ID 收录，实际可开通的型号以方舟控制台为准。",
		Protocols: []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages, config.ProtocolArkVideo, config.ProtocolArkImage}},
	{ID: "minimax", Type: config.UpstreamMinimax, Vendor: "MiniMax", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "minimax", Entries: "视频模型入口", Ability: "国内站与国际站的账号和 Key 相互独立。",
		Note: "MiniMax 上游只服务 MiniMax 视频协议面（无文本对话入口）；文本对话入口见 MiniMax 开放平台（中国）。",
		Protocols: []string{config.ProtocolMinimaxVideo}},
	{ID: "kimi_cn", Type: config.UpstreamOpenAICompat, Vendor: "Kimi 开放平台", BaseURL: "https://api.moonshot.cn/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "kimi-cn", Entries: "OpenAI 兼容文本入口", Ability: "平台余额在 Kimi 开放平台控制台查看。",
		Note: "月之暗面国内 API；目录只提供文本对话型号。", Protocols: openAIProtocols},
	{ID: "zhipu_cn", Type: config.UpstreamOpenAICompat, Vendor: "智谱 AI 开放平台", BaseURL: "https://open.bigmodel.cn/api/paas/v4", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "zhipu-cn", Entries: "OpenAI 兼容文本入口", Ability: "平台余额在智谱 AI 开放平台控制台查看。",
		Note: "智谱国内按量 API（Chat 端点 /api/paas/v4）。官方另有 Anthropic Messages 端点 https://open.bigmodel.cn/api/anthropic，可用通用 Anthropic 兼容适配录入。",
		Protocols: openAIProtocols},
	{ID: "bailian_cn", Type: config.UpstreamOpenAICompat, Vendor: "阿里云百炼（中国）", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "bailian-cn", Entries: "OpenAI 兼容文本入口", Ability: "按量付费 API Key；余额与账单在阿里云控制台查看。",
		Note: "百炼华北 2（北京）公共兼容端点；业务空间专属端点仍可通过通用兼容入口录入。", Protocols: openAIProtocols},
	{ID: "minimax_text_cn", Type: config.UpstreamOpenAICompat, Vendor: "MiniMax 开放平台（中国）", BaseURL: "https://api.minimaxi.com/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "minimax-text-cn", Entries: "OpenAI 兼容文本入口", Ability: "国内站与国际站的账号和 Key 相互独立。",
		Note: "MiniMax 国内站文本对话入口；与 MiniMax 视频平台档案分开。", Protocols: openAIProtocols},
	{ID: "qianfan_cn", Type: config.UpstreamOpenAICompat, Vendor: "百度千帆", BaseURL: "https://qianfan.baidubce.com/v2", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "qianfan-cn", Entries: "OpenAI 兼容文本入口", Ability: "平台余额与账单在百度智能云控制台查看。",
		Note: "千帆 v2 文本生成和深度思考共用 OpenAI 兼容对话入口。", Protocols: openAIProtocols},
	{ID: "tencent_tokenhub", Type: config.UpstreamOpenAICompat, Vendor: "腾讯云 TokenHub", BaseURL: "https://tokenhub.tencentmaas.com/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "tencent-tokenhub", Entries: "OpenAI 兼容文本入口", Ability: "聚合平台；可用型号与账户权限以 TokenHub 模型列表为准。",
		Note: "TokenHub 广州站按量入口；不混用 Token Plan 的订阅 Key 和 /plan/v3 端点。", Protocols: openAIProtocols},
	{ID: "chenyu", Type: config.UpstreamOpenAICompat, Vendor: "晨羽AI", BaseURL: "https://api.chenyu.pro/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "chenyu", Entries: "OpenAI 兼容文本入口", Ability: "聚合平台；余额、费用与实际可用型号在晨羽控制台查看。",
		Note: "本条只收固件可承载的 OpenAI 兼容文本入口；图片、视频与语音按晨羽官方厂商协议面，不进入本条。同域名同 Key 的 Anthropic Messages 面另见 chenyu_anthropic。",
		Protocols: openAIProtocols},
	{ID: "chenyu_anthropic", Type: config.UpstreamAnthropicCompat, Vendor: "晨羽AI", BaseURL: "https://api.chenyu.pro/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "chenyu-anthropic", Entries: "Anthropic Messages 文本入口", Ability: "聚合平台；与 OpenAI 兼容面同一把 Key、同一份余额，费用在晨羽控制台查看。",
		Note: "与 chenyu 同域名、同一把平台 API Key，只换协议（设备在 /v1 后拼 /messages）；只参与 /v1/messages 选路。",
		Protocols: anthropicProtocol},
	{ID: "openai", Type: config.UpstreamOpenAICompat, Vendor: "OpenAI", BaseURL: "https://api.openai.com/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "openai", Entries: "OpenAI 兼容文本入口", Ability: "API 用量和账单在 OpenAI Platform 查看。",
		Note: "通过 Chat Completions 调用 GPT 文本模型；设备的 /v1/responses 仍使用固件内置的无状态转换子集。", Protocols: openAIProtocols},
	{ID: "anthropic", Type: config.UpstreamAnthropicCompat, Vendor: "Anthropic", BaseURL: "https://api.anthropic.com/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "anthropic", Entries: "Anthropic Messages 文本入口", Ability: "使用 x-api-key 鉴权；API 用量和账单在 Claude Console 查看。",
		Note: "Anthropic 官方 Messages API；只提供文本输出模型。", Protocols: anthropicProtocol},
	{ID: "gemini", Type: config.UpstreamOpenAICompat, Vendor: "Google Gemini API", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "gemini", Entries: "OpenAI 兼容文本入口", Ability: "使用 Gemini API Key；配额和账单在 Google AI Studio / Cloud Console 查看。",
		Note: "使用 Google 官方 OpenAI 兼容层；目录只收文本型号。", Protocols: openAIProtocols},
	{ID: "xai", Type: config.UpstreamOpenAICompat, Vendor: "xAI", BaseURL: "https://api.x.ai/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "xai", Entries: "OpenAI 兼容文本入口", Ability: "API 用量和账单在 xAI Console 查看。",
		Note: "只接入 Grok 文本对话型号；Imagine 和 Voice API 不进入本目录。", Protocols: openAIProtocols},
	{ID: "openrouter", Type: config.UpstreamOpenAICompat, Vendor: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "openrouter", Entries: "OpenAI 兼容文本入口", Ability: "聚合平台；余额、路由和实际可用型号在 OpenRouter 控制台查看。",
		Note: "目录只提供数据仓库收录的路由别名；具体供应商型号可通过「自定义」按 OpenRouter Models API 的 ID 录入。", Protocols: openAIProtocols},
	{ID: "groq", Type: config.UpstreamOpenAICompat, Vendor: "GroqCloud", BaseURL: "https://api.groq.com/openai/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "groq", Entries: "OpenAI 兼容文本入口", Ability: "可用型号和速率限制以 GroqCloud Models API 为准。",
		Note: "只收录 GroqCloud 的文本生成型号。", Protocols: openAIProtocols},
	{ID: "mistral", Type: config.UpstreamOpenAICompat, Vendor: "Mistral AI", BaseURL: "https://api.mistral.ai/v1", BillingMode: platformcatalog.BillingUsage,
		SuggestedName: "mistral", Entries: "OpenAI 兼容文本入口", Ability: "API 用量和账单在 Mistral Studio 查看。",
		Note: "使用无状态 Chat Completions 文本入口。", Protocols: openAIProtocols},

	// ---- API 订阅套餐 ----
	{ID: "ark_plan", Type: config.UpstreamArkPlan, Vendor: "火山方舟", BillingMode: platformcatalog.BillingSubscription,
		SuggestedName: "ark-plan", Entries: "OpenAI 兼容 与 Anthropic 兼容双入口；视频与图像走订阅端点", Ability: "套餐余量在火山方舟控制台查看。",
		Note: "方舟 Agent Plan 与 Coding Plan 订阅共用端点根 /api/plan/v3；选单是两份套餐清单的并集，套餐额度与实际可用型号以方舟控制台为准。",
		Protocols: []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages, config.ProtocolArkVideo, config.ProtocolArkImage}},
	{ID: "qwen_plan", Type: config.UpstreamQwenPlan, Vendor: "阿里云百炼", BillingMode: platformcatalog.BillingSubscription,
		SuggestedName: "qwen-plan", Entries: "OpenAI 兼容 与 Anthropic 兼容双入口", Ability: "Token Plan 套餐余量在阿里云百炼控制台查看。",
		Note: "通义千问 Token Plan 文本入口；只收录套餐清单里的文本模型。", Protocols: textProtocols},
	{ID: "zhipu_coding_plan", Type: config.UpstreamOpenAICompat, Vendor: "智谱 GLM Coding Plan", BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4", BillingMode: platformcatalog.BillingSubscription,
		SuggestedName: "zhipu-coding-plan", Entries: "OpenAI 兼容文本入口", Ability: "套餐额度（5 小时周期 / 每周）在智谱 BigModel 控制台查看；额度用尽后等待周期恢复，不扣账户余额。",
		Note: "GLM Coding Plan 订阅专用端点，与按量端点 /api/paas/v4 不通用，Key 也不通用；官方限定在其支持的编程工具环境内使用。",
		Protocols: openAIProtocols},
	{ID: "kimi_code_plan", Type: config.UpstreamOpenAICompat, Vendor: "Kimi Code 会员", BaseURL: "https://api.kimi.com/coding/v1", BillingMode: platformcatalog.BillingSubscription,
		SuggestedName: "kimi-code-plan", Entries: "OpenAI 兼容文本入口", Ability: "会员额度在 Kimi Code 控制台查看。",
		Note: "Kimi Code 会员权益的 API 入口，Key 在 Kimi Code 控制台创建，与 Kimi 开放平台（api.moonshot.cn，按量）的 Key 和端点互不通用。",
		Protocols: openAIProtocols},
	{ID: "kimi_code_plan_anthropic", Type: config.UpstreamAnthropicCompat, Vendor: "Kimi Code 会员", BaseURL: "https://api.kimi.com/coding/v1", BillingMode: platformcatalog.BillingSubscription,
		SuggestedName: "kimi-code-plan-anthropic", Entries: "Anthropic Messages 文本入口", Ability: "会员额度在 Kimi Code 控制台查看；使用 x-api-key 鉴权。",
		Note: "同一份 Kimi Code 会员 Key 的 Anthropic 兼容入口（设备在 /v1 后拼 /messages）；只参与 /v1/messages 选路。",
		Protocols: anthropicProtocol},
	{ID: "opencode_go", Type: config.UpstreamOpenCodeGo, Vendor: "OpenCode", BillingMode: platformcatalog.BillingSubscription,
		SuggestedName: "opencode-go", Entries: "各模型按上游协议提供 OpenAI Chat 或 Anthropic Messages", Ability: "Go 套餐的 5 小时 / 周 / 月额度在 OpenCode Zen 控制台查看。",
		Note: "OpenCode Go 包月套餐：同一把 sk- Key，端点根 https://opencode.ai/zen/go/v1；各模型只接受 upstream_protocols 声明的上游协议，型号名以 GET /zen/go/v1/models 返回的裸名字为准。Zen 按量余额是另一套权益，不在本档案。",
		Protocols: textProtocols},

	// ---- 通用兼容适配（地址由管理员自填，选单为空、一律「自定义」）----
	{ID: "openai_compat", Type: config.UpstreamOpenAICompat, Vendor: "通用适配", BillingMode: platformcatalog.BillingNone,
		SuggestedName: "openai-compat", Entries: "OpenAI 兼容单入口", Ability: "通用适配器；服务地址与 Key 由管理员一起录入。",
		Note: "通用 OpenAI 兼容适配：能接的模型完全取决于这条账号填的服务地址，型号一律走「自定义」。", Protocols: openAIProtocols},
	{ID: "anthropic_compat", Type: config.UpstreamAnthropicCompat, Vendor: "Anthropic 兼容", BillingMode: platformcatalog.BillingNone,
		SuggestedName: "anthropic-compat", Entries: "Anthropic Messages 兼容单入口", Ability: "通用适配器；服务地址与 Key 由管理员一起录入。",
		Note: "适用于使用 x-api-key 鉴权并提供 Anthropic Messages 兼容接口的服务；模型用自定义方式录入。", Protocols: anthropicProtocol},
}

// offeringTargets：键是 "<provider_id>/<offering_id>"。
var offeringTargets = map[string]offeringTarget{
	"deepseek/api-cn":       {Platforms: []string{"deepseek"}},
	"ark/api-cn":            {Platforms: []string{"ark"}},
	"ark/agent-plan-cn":     {Platforms: []string{"ark_plan"}},
	"ark/coding-plan-cn":    {Platforms: []string{"ark_plan"}},
	"bailian/api-cn":        {Platforms: []string{"bailian_cn"}},
	"bailian/token-plan-cn": {Platforms: []string{"qwen_plan"}},
	"minimax/api-cn":        {Platforms: []string{"minimax_text_cn", "minimax"}},
	"zhipu/api-cn":          {Platforms: []string{"zhipu_cn"}},
	"zhipu/coding-plan-cn":  {Platforms: []string{"zhipu_coding_plan"}},
	"moonshot/api-cn":       {Platforms: []string{"kimi_cn"}},
	"moonshot/kimi-code":    {Platforms: []string{"kimi_code_plan", "kimi_code_plan_anthropic"}},
	"qianfan/api-cn":        {Platforms: []string{"qianfan_cn"}},
	"hunyuan/api-cn":        {Platforms: []string{"tencent_tokenhub"}},
	"chenyu-ai/api":         {Platforms: []string{"chenyu", "chenyu_anthropic"}},
	"opencode/go":           {Platforms: []string{"opencode_go"}},
	"openai/api-global":     {Platforms: []string{"openai"}},
	"anthropic/api-global":  {Platforms: []string{"anthropic"}},
	"gemini/api-global":     {Platforms: []string{"gemini"}},
	"xai/api-global":        {Platforms: []string{"xai"}},
	"openrouter/api-global": {Platforms: []string{"openrouter"}},
	"groq/api-global":       {Platforms: []string{"groq"}},
	"mistral/api-global":    {Platforms: []string{"mistral"}},

	"openai/codex":         {Agent: store.AgentProviderCodex},
	"anthropic/claude-code": {Agent: store.AgentProviderClaude},
	"xai/grok-build":       {Agent: store.AgentProviderGrok},
	"cursor/individual":    {Agent: store.AgentProviderCursor},
}

// agentSpec 是一份工具订阅在文件里的档案。
type agentSpec struct {
	Provider string
	Vendor   string
	Note     string
}

// agentSpecs 的先后就是 agents 段的先后。
var agentSpecs = []agentSpec{
	{Provider: store.AgentProviderCodex, Vendor: "OpenAI",
		Note: "Codex 订阅经设备的 /agents/codex/v1/responses 接入面调用；名义金额按本段各型号的 pricing（官方 API 按量价）记，不折算订阅补贴。"},
	{Provider: store.AgentProviderGrok, Vendor: "xAI",
		Note: "Grok Build 订阅：文本经 /agents/grok/v1/responses 接入面调用，按本段 pricing 记名义金额。订阅自带的 Grok Imagine 图像/视频生成经同一接入面逐字节透传，按次记 0 元，这里不收录 Imagine 型号。"},
	{Provider: store.AgentProviderClaude, Vendor: "Anthropic",
		Note: "Claude Code 订阅：收录 Anthropic 官方在售的型号供设备建计价行，名义金额按本段 pricing 记。它是计价清单不是准入白名单：设备只透传 /v1/messages，成员发没收录的型号照常转发、那次记 0 元。"},
	{Provider: store.AgentProviderCursor, Vendor: "Cursor",
		Note: "Cursor 订阅文本计价清单；按 Cursor 官方标准模型价格记名义金额，只读且随数据升级更新，不创建共享 API 模型、不改变 CLI 的模型发现与选择。未收录的请求 ID 照常转发并标为未定价。"},
}

// cursorAutoName 是 LLM Gate 的本地统计名：Cursor Auto 请求（default / auto）统一
// 归入它计量。数据仓库按官方型号收录、没有这一行；这里按 composer-2.5 的标准价
// 合成一行——Cursor 官方 Auto 按实际路由模型计费、无统一单价，这只是名义统计价，
// 不代表实际路由模型或 Cursor 现金账单。
const (
	cursorAutoName    = "cursor-auto"
	cursorAutoBasis   = "composer-2.5"
	cursorAutoNote    = "Auto 本地统计名：default / auto 请求统一归此；官方按实际路由模型收费、无统一单价，名义价格采用 composer-2.5 标准价，不代表上游实际账单。"
	cursorAutoSourceU = "https://cursor.com/help/models-and-usage/usage-limits"
)

// staticPlatformSources 给没有数据仓库产品的通用兼容适配一个出处（收录纪律：没有
// 出处的条目不入表）。
var staticPlatformSources = map[string]string{
	"openai_compat":    "https://platform.openai.com/docs/api-reference/chat",
	"anthropic_compat": "https://docs.anthropic.com/en/api/messages",
}

// ---- 固件行为档（模型能力）----
//
// Responses↔Chat 转换与 Codex 选择器的 effort 档位是**固件的实现细节**，数据仓库
// 记的是厂商官方的思考控制参数，两者不是一回事；这里按（平台, 型号）钉死已经
// 实测过的组合，与旧目录逐字一致。新增组合先在固件实测，再加进这张表。

var (
	deepseekReasoning = platformcatalog.ModelCapabilities{
		ResponsesChat: platformcatalog.ResponsesChatCapabilities{
			Profile: platformcatalog.ResponsesProfileReasoningReplayV1, ReplayReasoningContent: true,
			ThinkingType: "enabled", DropToolChoice: true,
			EffortMap: map[string]string{"minimal": "high", "low": "high", "medium": "high", "high": "high", "xhigh": "max", "max": "max", "ultra": "max"},
		},
		Codex: platformcatalog.CodexCapabilities{
			DefaultReasoningLevel: "high",
			SupportedReasoningLevels: []platformcatalog.CodexReasoningLevel{
				{Effort: "high", Description: "Deep reasoning"}, {Effort: "xhigh", Description: "Maximum reasoning"},
			},
		},
	}
	glmReasoning = platformcatalog.ModelCapabilities{
		ResponsesChat: platformcatalog.ResponsesChatCapabilities{
			Profile: platformcatalog.ResponsesProfileReasoningReplayV1, ThinkingType: "enabled",
			EffortMap: map[string]string{"minimal": "low", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max", "ultra": "max"},
		},
		Codex: platformcatalog.CodexCapabilities{
			DefaultReasoningLevel: "high",
			SupportedReasoningLevels: []platformcatalog.CodexReasoningLevel{
				{Effort: "low", Description: "Fast reasoning"}, {Effort: "medium", Description: "Balanced reasoning"},
				{Effort: "high", Description: "Deep reasoning"}, {Effort: "xhigh", Description: "Very deep reasoning"},
				{Effort: "max", Description: "Maximum reasoning"},
			},
		},
	}
	kimiReasoning = platformcatalog.ModelCapabilities{
		ResponsesChat: platformcatalog.ResponsesChatCapabilities{
			Profile: platformcatalog.ResponsesProfileReasoningReplayV1, ThinkingType: "enabled",
			EffortMap: map[string]string{"minimal": "minimal", "low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max", "ultra": "max"},
		},
		Codex: platformcatalog.CodexCapabilities{
			DefaultReasoningLevel: "high",
			SupportedReasoningLevels: []platformcatalog.CodexReasoningLevel{
				{Effort: "minimal", Description: "Minimal reasoning"}, {Effort: "low", Description: "Fast reasoning"},
				{Effort: "medium", Description: "Balanced reasoning"}, {Effort: "high", Description: "Deep reasoning"},
				{Effort: "xhigh", Description: "Very deep reasoning"}, {Effort: "max", Description: "Maximum reasoning"},
			},
		},
	}
)

// modelBehaviour 按（平台, 型号）给出固件行为档；没有的返回 false。
func modelBehaviour(platformID, name string) (platformcatalog.ModelCapabilities, bool) {
	isDeepSeek := name == "deepseek-flash" || hasPrefix(name, "deepseek-v4-pro") || hasPrefix(name, "deepseek-v4-flash")
	switch platformID {
	case "deepseek", "ark":
		if isDeepSeek {
			return deepseekReasoning, true
		}
	case "ark_plan":
		switch {
		case isDeepSeek:
			return deepseekReasoning, true
		case name == "glm-5.3":
			return glmReasoning, true
		case name == "kimi-k3":
			return kimiReasoning, true
		}
	}
	return platformcatalog.ModelCapabilities{}, false
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }
