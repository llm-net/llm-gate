// Package usage 是用量计量与人民币预算的内核（iteration-9）：把每一次数据面
// 消费折成金额、按小时桶聚合、喂预算计数器，并把异步任务的账在完成后补记回来。
//
// 三条贯穿全包的纪律：
//
//   - **金额恒为 int64 微元**（1 元 = 10⁶ 微元），全程无浮点
//     （根 AGENTS.md 的硬约束「金额与配额恒为定点整数，全链路禁止浮点」）。
//     「用量 → 微元」的换算**只在 Cost 一处发生**：别的地方要金额就调它，
//     不要就地乘一遍价格——第二处换算就是第二套取整口径。
//   - **记账时点价**：金额按记账那一刻的目录价折算，改价只影响其后的请求，
//     历史行永不重算。视频任务按**清算时点**的价（它的账在完成后才结）。
//   - **请求路径零写库**：Record 只动内存（小时桶 delta、预算计数器、环形
//     缓冲）；落盘由 Run 的协程每 flushInterval 一个事务批量 UPSERT。掉电最多
//     丢一个冲刷窗口的计数——只会少记，预算只会变松不会变紧，方向安全。
//
// §15.1 边界：本包处理的每一个存储与展示单元只含计数、金额、时长与标识
// （模型名、上游账户名、用户名、密钥展示串）。提示词、模型响应、任何内容片段、
// 完整密钥都不进本包的任何结构体、任何日志。估算路径只对已在内存里的请求 map
// 数**字符**，算完即弃，不复制不留存文本。
package usage

import (
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/cursorwire"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 数据面入口词汇（usage_hourly.entry 的取值，也是计价形态的分流依据）。
// gateway 与 admin 一律引用这里的常量，别再各写一份字面量——两家 cache 口径
// 相反，entry 写错一个字母就是整条入口的账算错了。
const (
	EntryChat     = "chat"     // POST /v1/chat/completions（OpenAI 形 usage）
	EntryMessages = "messages" // POST /v1/messages（Anthropic 形 usage）
	EntryVideo    = "video"    // POST /v1/video/generations（异步任务面）
	EntryImage    = "image"    // POST /v1/images/generations（同步出图）
	// EntryResponses 是 POST /v1/responses（标准 Responses 目录面，2026-08-13，
	// docs/firmware-gateway.md「Responses 目录面」）：走目录选路、按目录价正常
	// 记账，金额是真钱。上游打的是所选来源的 chat 端点，usage 是 OpenAI 形——
	// EntryKind 与 Cost 因此都走既有 text/chat 分支，不新增计价形态。
	//
	// 命名沿革（2026-08-13 产品裁决）：`responses` 这个本名归标准目录面（初版
	// 曾叫 responses_catalog），订阅代理让位改记 responses_agents（下条）。
	// 历史账本行由迁移 0015 同步改写——entry 改名若不改历史，旧的订阅代理流量
	// 就会顶着目录面的名字进报表。
	EntryResponses = "responses"
	// EntryResponsesAgents 是 POST /agents/{codex,grok}/v1/responses（Agents / Codex+Grok
	// 订阅代理，迭代 11；2026-08-13 由 responses 改名，历史行随迁移 0015 改写）。
	// 计量口径与 chat **完全同形**（OpenAI 形 usage：input_tokens 已含缓存命中，
	// 价格字段同为 in/out/cache_read），所以 EntryKind 与 Cost 都走那条既有分支，
	// 不新增计价形态。它单独占一个 entry 取值只为**在报表里认得出订阅流量**
	// ——决策 2 不给它建模型目录行，`entry=responses_agents` 是唯一的分辨维度。
	// 与目录面的 EntryResponses **必须分开**：这边 0 元是真值（订阅无边际成本），
	// 那边按目录价记的是真钱。
	EntryResponsesAgents = "responses_agents"
	// EntryClaudeCode 是 POST /agents/claude/v1/messages（调用者个人
	// setup-token 的固定 Anthropic 上游透传）。usage 形态与 EntryMessages
	// 完全相同；独占 entry 是为了与目录 messages、设备持有的订阅流量分开。
	EntryClaudeCode = "claude_code"
	// EntryCursorAgent 是 POST /agents/cursor/agent.v1.AgentService/{Run,RunSSE}
	// 的 Cursor 订阅对话计量入口。每次 Run/RunSSE 记一次请求，旁路提取请求
	// 模型及最终实际用量；按 Cursor 单独价格或同名文本目录价计费。缺失完整
	// 用量明确标记未计费。辅助 RPC 只透传，不计消费、不做预算/RPM 准入。
	EntryCursorAgent = "cursor_agent"
	// CursorAgentModelDimension 是 Cursor 订阅对话在「按模型」和最近请求里的
	// 稳定展示维度。它是服务标签，不声称是 Cursor 实际选择的模型。
	CursorAgentModelDimension = "Cursor Agent 对话"

	CursorAgentRunRPC    = "agent.v1.AgentService/Run"
	CursorAgentRunSSERPC = "agent.v1.AgentService/RunSSE"
)

// IsCursorAgentRunRPC 报告这个 Cursor RPC 是否是真正开始一轮模型对话的
// 消费入口。其他 aiserver.v1 / agent.v1 方法都是协议辅助流量。
func IsCursorAgentRunRPC(rpc string) bool {
	return rpc == CursorAgentRunRPC || rpc == CursorAgentRunSSERPC
}

// canonicalModelDimension 收敛不是模型名的协议维度。既用于新样本入账，也用于
// 报表读取历史小时行，因此升级后无需改写原始账本就能立即消除旧 RPC 名分组。
func canonicalModelDimension(entry, model string) string {
	if entry == EntryCursorAgent && !cursorwire.ValidModel(model) {
		return CursorAgentModelDimension
	}
	return model
}

// reportModelDimension 在报表读取时剥掉账本里不是模型消费的 Cursor
// 辅助 RPC。CursorAgentModelDimension 是当前只由 Run/RunSSE 写入的维度；
// 明确的 Run/RunSSE RPC 名则是仍需兼容读取的账本取值。其余 cursor_agent
// 行不能证明发生过模型对话，宁可不展示，也不冒充请求数。
func reportModelDimension(entry, model string) (string, bool) {
	if entry != EntryCursorAgent {
		return model, true
	}
	if model == CursorAgentModelDimension || IsCursorAgentRunRPC(model) {
		return CursorAgentModelDimension, true
	}
	if cursorwire.ValidModel(model) || model == UnknownModelDim {
		return model, true
	}
	return "", false
}

// EntryImagineImage / EntryImagineVideo 是 Grok Build 订阅侧的 Grok Imagine
// 画图门与视频提交门（POST /agents/grok/v1/images/{generations,edits} 与
// POST /agents/grok/v1/videos/generations，gateway/imagine_grok.go）。
//
//   - 金额恒 0 元是**真值**（订阅无边际成本），不打 estimated；管理员在模型目录
//     建一行同名 image 模型并录 ark_image_each 时，画图门按张数记名义金额——
//     同 Codex/Grok 文本订阅代理的名义价旋钮，只是形态换成按张；
//   - 画图门记张数（TaskUsage.GeneratedImages = 响应 data[] 长度）；视频提交只记
//     「1 次请求、0 元」，没有任务行与清算；GET /videos/{request_id} 轮询不计量，
//     只进访问日志（同 Cursor 的辅助 RPC 口径）。
//
// 取值与账本里同名的历史行完全相同，历史小时行不需要迁移。
const (
	EntryImagineImage = "imagine_image"
	EntryImagineVideo = "imagine_video"
)

// EntryKind 返回某入口服务的模型 kind。入口与 kind 是多对一（chat/messages/
// responses 同属文本），聚合表因此不把 kind 放进唯一索引——它由 entry 唯一决定。
func EntryKind(entry string) string {
	switch entry {
	case EntryVideo, EntryImagineVideo:
		return store.ModelKindVideo
	case EntryImage, EntryImagineImage:
		return store.ModelKindImage
	default:
		return store.ModelKindText
	}
}

// Tokens 是文本面的 token 四分量，**按各家原始口径如实保留**（不在这里归一）。
// 两家的 cache 口径正好相反，是 Cost 按 entry 分流处理的，不是靠调用方摆平：
//
//   - chat / responses（OpenAI 形）：Prompt **已包含**缓存读写。chat 的 CacheWrite
//     恒 0；Responses 仅在上游回传 input_tokens_details.cache_write_tokens 时记录，
//     未回传时为 0。缓存写按普通输入价计，不额外加进总量或金额。
//     Responses 的 input_tokens / output_tokens / input_tokens_details.cached_tokens
//     分别对应 Prompt / Completion / CacheRead，落位在 gateway 侧完成。
//   - messages / claude_code（Anthropic 形）：Prompt（= input_tokens）**不含** CacheRead 与
//     CacheWrite（= cache_creation_input_tokens），三者相加才是真实输入量。
//   - cursor_agent：Prompt 已包含 CacheRead 与 CacheWrite，缓存不得重复加总。
type Tokens struct {
	Prompt     int64
	Completion int64
	CacheRead  int64
	CacheWrite int64
}

// Total 按入口协议折算统一辅读数（usage_hourly.total_tokens）：chat/responses 侧
// prompt + completion；messages / claude_code 侧 input + output + cache 两项——查询端因此
// 不必懂两家方言。负分量（不该出现）按 0 计，总量恒非负。
func (t Tokens) Total(entry string) int64 {
	sum := nonNeg(t.Prompt) + nonNeg(t.Completion)
	if entry == EntryMessages || entry == EntryClaudeCode {
		sum += nonNeg(t.CacheRead) + nonNeg(t.CacheWrite)
	}
	return sum
}

// empty 报告四分量是否全为零（无 usage 可记）。
func (t Tokens) empty() bool {
	return t.Prompt == 0 && t.Completion == 0 && t.CacheRead == 0 && t.CacheWrite == 0
}

// Sample 是一次消费的完整记账事实，由数据面在请求收尾时提交给 Record，或由
// 懒对账在任务清算后提交。字段分三组：
//
//	归属与维度   —— 落进 usage_hourly 的五列唯一键与两列快照；
//	计价输入     —— Pricing（记账时点价）+ 用量（Tokens 或任务 usage）+ 计费特征；
//	请求事实     —— 状态码、尝试次数、耗时、估算/拒绝标记。
//
// 全部字段都是计数与标识，没有任何内容字段（§15.1）。
type Sample struct {
	// At 是记账时刻；零值取 Meter 的当前时间。桶号由它折算（UTC 整小时）。
	At time.Time

	KeyID      int64
	KeyDisplay string
	// ModelName 是客户端可见的原始模型名；UpstreamName 是**最终提交响应的**
	// 上游账户名快照（被切换掉的失败尝试不单独成行，尝试次数在 Attempts 里）。
	// 准入被拒的样本没走到选路，UpstreamName 留空。
	ModelName    string
	UpstreamName string
	// ModelKnown 报告 ModelName 在模型目录里真的有一行（含已禁用的行）。
	// 为假 = 这个名字是客户端随口给的（拼错、扫描器乱打、或准入在选路之前
	// 就把请求拦了），账本因此对它做基数封顶——见 Meter.addDeltaLocked。
	ModelKnown bool
	// UpstreamType 是上游产品类型（config.Upstream* 同集），
	// 只用于选计价形态，**不进任何存储列**——它是厂商身份，管理面之外不外露。
	UpstreamType string
	Entry        string
	// Kind 留空时由 Entry 推出（EntryKind）。
	Kind string
	// Pricing 是记账时点的 models.pricing 原文（空串 = 未定价 → 金额记 0，
	// 照常记用量，管理台挂警示徽章）。
	Pricing string

	Tokens Tokens
	// TaskUsage 是视频/图片形态的用量（厂商 usage 原文解析结果）。
	TaskUsage TaskUsage
	// HasVideoInput / Resolution 是提交时即固化的计费特征（Seedance 分档判据 /
	// H3 秒价档判据），来自 aigc_tasks 行。
	HasVideoInput bool
	Resolution    string

	// Status 是客户端可见的最终 HTTP 状态；Attempts 是尝试过的来源数。
	Status     int
	Attempts   int
	DurationMs int64

	// Estimated 标记本笔 token 含估算成分（显式 include_usage:false、断流、
	// 上游没报 usage），或任务金额因厂商 usage 不可得而记 0。
	Estimated        bool
	UsageUnavailable bool
	// Rejected 标记本请求被准入拦下（429）：它没到上游，单列统计，
	// **绝不混进 errors**——那会把「密钥超限」误读成「上游故障」。
	Rejected bool
	// RejectReason 是拒绝档位（rpm / budget_day / budget_month …），只进内存
	// 环形缓冲供排障，不落库、不外发给数据面调用方。
	RejectReason string
	// TaskID 是异步任务的设备 id（清算样本才有），只进环形缓冲。
	TaskID string
}

// measure 摊出计价所需的形态化视图。
func (s Sample) measure() Measure {
	return Measure{
		Kind:          s.kind(),
		Entry:         s.Entry,
		UpstreamType:  s.UpstreamType,
		Tokens:        s.Tokens,
		TaskUsage:     s.TaskUsage,
		HasVideoInput: s.HasVideoInput,
		Resolution:    s.Resolution,
	}
}

// kind 取样本的模型 kind：显式给了就用，否则从入口推。
func (s Sample) kind() string {
	if s.Kind != "" {
		return s.Kind
	}
	return EntryKind(s.Entry)
}

// quantities 是一次消费**记进账本的量**：token 四分量与它们的合计，加上两种
// 非 token 的计费量（秒 / 张）。金额之外的一切读数都从这里来。
type quantities struct {
	tokens  Tokens
	total   int64
	seconds int64
	images  int64
}

// quantities 按 kind 摊出本样本的记账量。**按 kind 二选一，不相加**：
// 文本面的量在 Tokens 里，视频/图片面的在 TaskUsage 里（厂商 usage 的解析
// 结果），一个样本只可能是其中一种形态。相加看着更"宽容"，实际是给未来某天
// 两处同时有值时的重复计数留门。
//
// 视频/图片形态的落位（字段名带厂商前缀，天然互斥，故同列可以合并）：
//
//	completion  ← 方舟 Seedance 的 completion_tokens、Seedream 的 output_tokens、
//	              H3 Context-IR 的 completion_tokens
//	prompt      ← H3 Context-IR 的 prompt_tokens
//	seconds     ← H3 的 output_seconds + input_seconds（同一秒价，合计即计价量）
//	images      ← H3 的 input_image_count 或 Seedream 的 generated_images
func (s Sample) quantities() quantities {
	if s.kind() == store.ModelKindText {
		return quantities{tokens: s.Tokens, total: s.Tokens.Total(s.Entry)}
	}
	u := s.TaskUsage
	tok := Tokens{
		Prompt:     nonNeg(u.PromptTokens),
		Completion: nonNeg(u.CompletionTokens) + nonNeg(u.OutputTokens),
	}
	return quantities{
		tokens:  tok,
		total:   tok.Prompt + tok.Completion,
		seconds: nonNeg(u.OutputSeconds) + nonNeg(u.InputSeconds),
		images:  nonNeg(u.InputImages) + nonNeg(u.GeneratedImages),
	}
}

// Event 是内存环形缓冲的一条：「刚才发生了什么」的排障读数。请求级明细**只**
// 存在于这里——持久层只有小时聚合，每请求一行 INSERT 是 SD 卡写放大的最大头
// （方案 §3.3）。重启即失是有意的。
//
// 环里没有任何内容字段。
type Event struct {
	At               time.Time `json:"at"`
	KeyID            int64     `json:"key_id"`
	KeyDisplay       string    `json:"key_display"`
	ModelName        string    `json:"model_name"`
	Upstream         string    `json:"upstream_name"`
	Entry            string    `json:"entry"`
	Kind             string    `json:"kind"`
	Status           int       `json:"status"`
	Attempts         int       `json:"attempts"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	// VideoSeconds / ImageCount 是非 token 形态的计费量（H3 按秒、按参考图张数，
	// Seedream 按出图张数）。文本行恒为 0——视频行的「Token 0」不是没花钱，
	// 是这一行的量本来就不按 token 计。
	VideoSeconds     int64  `json:"video_seconds"`
	ImageCount       int64  `json:"image_count"`
	CostMicro        int64  `json:"cost_micro"`
	DurationMs       int64  `json:"duration_ms"`
	Estimated        bool   `json:"estimated,omitempty"`
	UsageUnavailable bool   `json:"usage_unavailable,omitempty"`
	Rejected         bool   `json:"rejected,omitempty"`
	RejectReason     string `json:"reject_reason,omitempty"`
	TaskID           string `json:"task_id,omitempty"`
}

// nonNeg 把负数钳成 0（上游偶尔把重叠计数报成负差值，见 Cost 的 (·)⁺）。
func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
