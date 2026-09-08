// metering.go 是 iteration-9 的网关侧计量接线：把一次数据面消费的用量事实攒在
// reqInfo 上，请求收尾时折成一条 usage.Sample 交给 internal/usage 的 Meter。
//
// 三个接触点，各自只做一件事：
//
//	beginEntry    入口 handler 在「请求体已解析、模型名已知」处标记本请求要入账；
//	observe 闭包  proxy.go 把已解析的响应载荷（非流式整体 / SSE 每个 data 行）
//	              交过来，这里取 usage 真值、顺带累计输出文本（估算兜底与
//	              长流中间结算都要它）；
//	recordUsage   withAccessLog 收尾时记一笔（含出错与被拒的请求）。
//
// 为什么 usage 要在这里解析、而不是复用透传路径上已有的字节：透传契约是字节
// 保真，响应体在 proxy.go 里本来就为了 model 回写解析过一次——观察器搭的是
// 那一次解析的车，不额外解析、不额外分配。观察器是**旁路**：不写响应、不改
// 载荷、解析不出来就当没看见，绝不因为记账失败影响转发。
//
// §15.1：本文件只从载荷里取**数字**（token 计数、张数）与已在内存的输出文本的
// **rune 计数**，算完即弃。提示词、模型响应、任何内容片段都不进 reqInfo、不进
// Sample、不进任何日志——包括估算路径（usage.TextCounter 只累计 rune 数）。
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// provisionalInterval 是长流中间结算的节奏（路线第 7 项点名的 30s）。
const provisionalInterval = 30 * time.Second

// Metering 是数据面的计量出口（生产实现是 *usage.Meter，装配在 cmd/llmgate）。
// 只留请求路径真正用得着的方法。
//
// 未装配（nil）时整条计量链路静默跳过，转发语义一字不变——计量是旁路能力，
// 它没起来不该让网关少转发一个字节。**准入也一样**：没有计量就没有已用额，
// 拿不出判据就不该拦请求（Phase 4 口径：宁松勿紧）。
type Metering interface {
	// Record 记一次消费（含出错与被准入拒绝的请求）。
	Record(s usage.Sample)
	// Admit 判一次预算准入（纯内存，不查库）。
	Admit(ka store.KeyAuth) usage.Decision
	// AddProvisional 把一笔临时金额并进预算计数器，返回它落在哪一组预算窗口
	// （日/周/月）。
	AddProvisional(keyID, deltaMicro int64) usage.ProvisionalWindow
	// ReverseProvisional 冲正临时金额；窗口已翻转的那部分整个丢弃。
	ReverseProvisional(keyID, dayMicro, weekMicro, monthMicro int64, win usage.ProvisionalWindow)
	// SettleTask 就地清算一个已终态的任务行（查询路径顺手调用）。
	SettleTask(ctx context.Context, task store.AIGCTask)
}

// EnableMetering 接入计量出口。装配期调用一次（Run 之前）；不调用 = 不计量。
func (s *Server) EnableMetering(m Metering) { s.meter = m }

// billState 是一个请求的计量累计，挂在 reqInfo 上。同一请求单 goroutine 读写
// （观察器与收尾记账都在 handler 那条 goroutine 上），与 reqInfo 其余字段同规则。
type billState struct {
	// entry 非空即「本请求要入账」，取值是 usage.Entry*。
	// /v1/models 与任务查询/下载/取消不标记，也就不进账本——账本记的是消费，
	// 不是流量（查已经花掉的钱不该再记一笔）。
	entry string
	// kind/pricing/upstreamType 由选路与转发回填：kind 与 pricing 来自模型行
	// （pricing 是**记账时点**的目录价原文），upstreamType 只用于选计价形态，
	// 不进任何存储列。
	kind         string
	pricing      string
	upstreamType string
	// model 是客户端提交的模型名（客户端可见的原始名）；keyDisplay 是密钥
	// 展示串快照。两者都是账本的维度列。
	model      string
	keyDisplay string
	// modelKnown 由选路点查命中时置真（fetchModelRoute），交给账本做维度基数
	// 封顶：目录里有的名字照实记，客户端随口给的名字每个冲刷窗口只放行 50 个。
	// 准入拒绝的请求没走到选路，因此恒为假——那正是要限流的那条路。
	modelKnown bool
	// sse 标记本响应是流式：中间结算只在流上跑。
	sse bool

	// tokens 是上游报的 usage 真值（逐字段非零覆盖合并，见 mergeChatUsage /
	// mergeMessagesUsage）；usageSeen 为假表示上游一个可用字段都没给，
	// 收尾时走估算并打 estimated 标。
	tokens    usage.Tokens
	usageSeen bool
	cursor    *cursorCallUsage
	// outputFinal 标记「输出侧 token 已是最终值」。它只对 messages 入口有
	// 意义：Anthropic 的 message_start 就带 usage，其中 output_tokens 恒为
	// 占位的 1——流断在 message_delta 之前时 usageSeen 早已为真，若不另记
	// 一位，这条流会以「Completion=1、非估算」入账（Phase 3.5 外审发现，
	// chat 侧没有这个洞：那边的 usage 只在末帧一次到齐）。
	outputFinal bool
	// text 累计输出侧文本的 rune 数（只累计计数，不留文本）。
	text usage.TextCounter
	// estimateInput 是输入侧估算器，由文本入口挂上；nil = 本入口不估算
	// （count_tokens 只计请求，视频/图片按方案不估算）。estIn/estInDone 是
	// 它的一次性备忘——中间结算与收尾各要一次，而它要序列化 messages。
	estimateInput func() int64
	estIn         int64
	estInDone     bool

	// pricingParsed/pricingDone 是目录价的一次性解析备忘（中间结算用；
	// 收尾那一笔由 Meter.Record 自己按原文解析，口径同一份代码）。
	pricingParsed usage.Pricing
	pricingDone   bool
	// provisional 是已上报的累计临时金额（单调递增，中间结算据此只补差额），
	// provisionalAt 是上次结算时刻。
	provisional   int64
	provisionalAt time.Time
	// provDayMicro/provWeekMicro/provMonthMicro 是**分窗口**记的已并入额，
	// provWin 是它们所属的那组窗口键。收尾冲正必须按窗口分别回退：一条跨过
	// 本地零点的长流，记在昨天的那半笔已随日计数器清零蒸发，再从今天减回去
	// 就是白送额度（Phase 3.5 外审发现）。日窗口翻了而周/月窗口没翻是常态，
	// 所以三个数分开记。
	provDayMicro   int64
	provWeekMicro  int64
	provMonthMicro int64
	provWin        usage.ProvisionalWindow

	// rejected/rejectReason 标记本请求被预算准入拦下（Phase 4 写入）：
	// 单列统计，绝不混进 errors——那会把「密钥超限」误读成「上游故障」。
	rejected     bool
	rejectReason string
}

// beginEntry 标记「本请求是某个消费入口对某个模型的一次调用」——记账的最小
// 单位。各消费入口在**请求体解析成功、模型名已知之后**各调一次；完整清单见
// docs/firmware-usage-metering.md「Where the gate sits」。
//
// 调用之前就失败的请求（体读不出来 / 不是合法 JSON / 缺 model）不进账本：
// 那种请求还没构成「对某个模型的调用」，记进去只会在按模型分解里堆出一行
// 空模型名。选路失败（model_not_found）之后的请求**要**入账——模型名是
// 客户端指定的，那一笔 404 值得出现在这个模型的错误数里。
//
// 模型名按 modelDimLimit **截断**后才进账本：这个字符串是客户端给的，而它
// 随后会成为内存 delta 表的键与一行 usage_hourly 的维度列。目录里的真名恒
// ≤128 字符（admin 的 validateCatalogName），所以截断对任何能选到路的请求都
// 是空操作；它拦的是「一个请求往 SD 卡上写一条 100 KB 维度」这种放大。
// 名字的**基数**仍不受限（外审遗留，见 iteration-9 Phase 4 报告）。
func beginEntry(r *http.Request, entry, model string) *reqInfo {
	info := infoFrom(r.Context())
	info.bill.entry, info.bill.model = entry, clipModelDim(model)
	return info
}

// modelDimLimit 是账本里模型维度的长度上限（rune），取 admin 侧目录名上限
// catalogNameMaxRunes 的同一个值——两处必须同步，否则合法模型名会被截断，
// 账就按两个名字分成两段。
const modelDimLimit = 128

// clipModelDim 把模型名裁到可安全入账的长度。
func clipModelDim(model string) string {
	if utf8.RuneCountInString(model) <= modelDimLimit {
		return model
	}
	return string([]rune(model)[:modelDimLimit])
}

// admit 是消费入口的预算准入闸，插在**鉴权之后、选路/固定上游出站之前**（beginEntry
// 紧随其后的那一步）：被拒的请求要带着模型维度进账本，但绝不该消耗一次选路
// 点查，更不该碰上游。返回 true 表示放行；返回 false 时 429 已写出。
//
// 只有「新消费」过这道闸：count_tokens（只计一次请求）、/v1/models、以及任务
// 的查询/下载/取消都不调用它——查已经花掉的钱不该被预算拦住。
//
// 计量未装配时一律放行：没有已用额就没有判据，宁松勿紧。
func (s *Server) admit(w http.ResponseWriter, r *http.Request, style errorStyle) bool {
	if s.meter == nil {
		return true
	}
	info := infoFrom(r.Context())
	d := s.meter.Admit(info.limits)
	if d.Allowed {
		return true
	}
	// 记号交给收尾记账：这一笔进 rejected_requests 而不是 errors——把「密钥
	// 超限」混进错误数会让人去查上游健康。
	info.bill.rejected, info.bill.rejectReason = true, d.Reason
	if d.RetryAfterSec > 0 {
		// 必须早于 style()：那里面就 WriteHeader 了。
		w.Header().Set("Retry-After", strconv.Itoa(d.RetryAfterSec))
	}
	code, message := admitRejection(d.Reason)
	style(w, http.StatusTooManyRequests, code, message)
	return false
}

// admitRejection 把拒绝档位翻成对客户端的错误码与文案。
//
// 文案里**没有金额、没有限额数值、没有上游信息，也不说是哪一档限额**：调用方
// 需要知道的只有「被本设备的配额挡住了、什么时候可以再来」，具体额度是管理台
// 的事。档位（key/user × 日/月）留在 Decision.Reason 里，只进内存环给管理员
// 排障（§11.1 不给探测面的延伸）。
func admitRejection(reason string) (code, message string) {
	if reason == usage.RejectRPM {
		return "rate_limited",
			"Too many requests. Please slow down and retry after the interval given in the Retry-After header."
	}
	return "budget_exceeded",
		"The configured spending budget has been used up. Contact your administrator, " +
			"or retry in the next budget window."
}

// recordUsage 是请求收尾的记账点（withAccessLog 内，紧挨着访问日志那一行）。
// status 是对客户端生效的最终状态码，elapsed 是整个请求的墙钟耗时。
func (s *Server) recordUsage(info *reqInfo, status int, elapsed time.Duration) {
	if s.meter == nil || info.bill.entry == "" {
		return
	}
	b := &info.bill
	// 长流中间结算的冲正：把这条流期间并进预算计数器的临时金额减回去，
	// 紧接着 Record 记真值。两步之间不放任何可能提前返回的分支——中间跑掉
	// 就等于把一笔临时金额永久留在计数器上。
	//
	// **按窗口分别回退**：跨过本地零点的那半笔记在昨天的计数器上，而昨天的
	// 计数器已经清零，硬减回来就是从今天别人的合法消费里扣钱（Meter 侧按
	// provWin 裁掉，这里只如实交出两个分窗口金额）。
	//
	// 这两步各自取一次 Meter 的锁，不是一个原子事务：并发的准入检查有可能
	// 恰好落在中间那一瞬，看到偏低的已用额而多放一个请求进来。可以接受——
	// 预算本来就是「近似且偏松」的（5 分钟冲刷窗口、估算兜底都是同一方向），
	// 为这一瞬引入跨包的组合 API 不划算。
	if b.provDayMicro != 0 || b.provWeekMicro != 0 || b.provMonthMicro != 0 {
		s.meter.ReverseProvisional(info.keyID,
			b.provDayMicro, b.provWeekMicro, b.provMonthMicro, b.provWin)
		b.provisional, b.provDayMicro, b.provWeekMicro, b.provMonthMicro = 0, 0, 0, 0
	}

	sample := usage.Sample{
		KeyID:        info.keyID,
		KeyDisplay:   b.keyDisplay,
		ModelName:    b.model,
		ModelKnown:   b.modelKnown,
		UpstreamName: info.upstream,
		UpstreamType: b.upstreamType,
		Entry:        b.entry,
		Kind:         b.kind,
		Pricing:      b.pricing,
		Status:       status,
		Attempts:     info.attempts,
		DurationMs:   elapsed.Milliseconds(),
		Rejected:     b.rejected,
		RejectReason: b.rejectReason,
	}
	if b.cursor != nil {
		// Cursor's final counts are authoritative even if writing the final
		// response chunk fails. Missing fields never become a free, known-zero bill.
		sample.Tokens = b.tokens
		b.cursor.mu.Lock()
		failed := b.cursor.failed
		b.cursor.mu.Unlock()
		if failed && sample.Status < http.StatusBadRequest {
			sample.Status = http.StatusBadGateway
		}
		sample.UsageUnavailable = !b.usageSeen && !b.rejected && status < http.StatusBadRequest
		s.meter.Record(sample)
		return
	}
	// 用量三态：真值 → 估算 → 零。
	switch {
	case status >= http.StatusBadRequest:
		// 客户端看到的最终状态是失败：不记 token。上游没产出（4xx/5xx 错误体
		// 里即便带了数字也不作数），被准入拒绝的请求更是压根没出去。
	case b.usageSeen:
		sample.Tokens = b.tokens
		if (b.entry == usage.EntryMessages || b.entry == usage.EntryClaudeCode) &&
			!b.outputFinal && b.estimateInput != nil {
			// Anthropic 流断在 message_delta 之前：输入侧是 message_start 给的
			// 真值（留着，比估算准），输出侧还停在占位的 1——按已收到的增量
			// 估算并打标，绝不把「1 个 output token」当成真值记账。
			sample.Tokens.Completion = b.text.Tokens()
			sample.Estimated = true
		}
	case b.estimateInput != nil:
		// 上游一个 usage 字段都没给（显式 include_usage:false、中途断流、
		// 或它本来就不报）：按估算记账并打标，绝不让这类请求隐身。
		sample.Tokens = usage.Tokens{Prompt: b.inputEstimate(), Completion: b.text.Tokens()}
		sample.Estimated = true
	}
	if b.entry == usage.EntryImage && status < http.StatusBadRequest {
		// 判据是 entry **精确等于** EntryImage，不是 kind==image：只有按量出图
		// 入口有「厂商 usage 不可得」这回事，新增按量图片入口时才该进这个分支。
		//
		// 视频侧这里根本没有分支：按量视频的账在任务清算时才结（settle.go），
		// 提交这一步只记「1 次请求、0 元」。要给按量视频补「usage 不可得」处理
		// 时，判据同样得是 entry 精确等值，不能写成 kind==video。
		//
		// 形态按 kind + 上游族判：认错族解析出的是一份空测量值，那会让一次
		// 真实出图以 0 元、**非估算**入账（Phase 3.5 加固）。上游族取不到时
		// 同样判不可得——走下面的打标 + 告警，不静默记 0。
		tu, ok := usage.ParseTaskUsage(info.imageUsage, usage.EntryKind(b.entry), b.upstreamType)
		sample.TaskUsage = tu
		if !ok {
			// 图片不估算（张数与 token 猜不出来也不该猜）：记 0 元 + 打标 +
			// 告警，让这笔缺账在用量页与日志里都看得见。
			sample.Estimated = true
			s.log.Warn("图片调用未取得厂商 usage，本次记 0 元",
				"request_id", info.id, "model", b.model, "upstream", info.upstream)
		}
	}
	if b.entry == usage.EntryImagineImage && status < http.StatusBadRequest {
		// Grok Imagine 画图门（imagine_grok.go）：订阅无边际成本，0 元是真值——
		// **不**打 estimated、不告警；张数从响应 data[] 数出来，只为报表读数与
		// 名义价旋钮（image 行的 ark_image_each × 张数）。
		sample.TaskUsage = usage.TaskUsage{GeneratedImages: int64(info.imageCount)}
	}
	s.meter.Record(sample)
}

// settleTaskIfTerminal 在客户端查询到任务终态时就地把这笔账清算入库。
//
// 不这么做，视频任务的钱要等懒对账那一轮才进账本——最长 15 分钟（10 分钟
// 陈旧阈值 + 5 分钟轮次），而客户端明明刚刚才告诉我们它完成了。重复清算无害：
// 一次性语义压在 store 的条件更新上（cost_micro IS NULL），与懒对账重叠也
// 只入账一次。
//
// 客户端此刻断开也要落（账单事实，WithoutCancel——同 observeVideoTask 的口径）。
func (s *Server) settleTaskIfTerminal(r *http.Request, task *store.AIGCTask) {
	if s.meter == nil || task == nil || task.CostMicro != nil || !aigcTerminal(task.Status) {
		return
	}
	s.meter.SettleTask(context.WithoutCancel(r.Context()), *task)
}

// inputEstimate 取输入侧估算（懒算一次并缓存）：中间结算与收尾都要它，而它
// 要把 messages/system 序列化一遍——同一请求算第二遍纯属白烧板子的 CPU。
func (b *billState) inputEstimate() int64 {
	if b.estimateInput == nil {
		return 0
	}
	if !b.estInDone {
		b.estInDone = true
		b.estIn = b.estimateInput()
	}
	return b.estIn
}

// parsedPricing 解析记账时点价并缓存（只服务中间结算）。价目表坏了按未定价
// 处理——收尾那一笔由 Meter.Record 走同一判定并负责告警，这里不重复刷屏。
func (b *billState) parsedPricing() usage.Pricing {
	if !b.pricingDone {
		b.pricingDone = true
		b.pricingParsed, _ = usage.ParsePricing(b.pricing)
	}
	return b.pricingParsed
}

// ---- 入口观察器（proxy.go 的 forwardSpec.observe） ----

// chatObserver 构造 chat 入口的响应观察器：非流式整读的响应体、SSE 的每条
// data: 行都会各经它一次。
//
// usage 取真值、文本增量顺带累计——两者都要：真值到得晚（usage chunk 在最后
// 一帧，断流就永远不来），而中间结算等不到那时候。
func (s *Server) chatObserver(info *reqInfo) func(map[string]any) {
	return func(m map[string]any) {
		if u, ok := m["usage"].(map[string]any); ok {
			info.bill.mergeChatUsage(u)
		}
		for _, c := range jsonArray(m["choices"]) {
			ch, ok := c.(map[string]any)
			if !ok {
				continue
			}
			// 非流式产出在 message、流式在 delta；reasoning_content 是
			// DeepSeek 的思维链增量，同样是模型产出，估算要计。
			for _, key := range [...]string{"message", "delta"} {
				d, ok := ch[key].(map[string]any)
				if !ok {
					continue
				}
				info.bill.text.Add(jsonString(d["content"]))
				info.bill.text.Add(jsonString(d["reasoning_content"]))
			}
		}
		s.tickProvisional(info)
	}
}

// messagesObserver 构造 messages 入口的响应观察器。
//
// Anthropic 的流式 usage 分两处到达：message_start 带输入与 cache（那时
// output_tokens 恒为 1），message_delta 带最终 output_tokens。逐字段非零覆盖
// 合并因此不是偷懒而是必须——后到的 message_delta 只有 output 一个字段，
// 整体覆盖会把输入抹成 0。
func (s *Server) messagesObserver(info *reqInfo) func(map[string]any) {
	return func(m map[string]any) {
		// 非流式响应与 message_delta 的 usage 在顶层，message_start 的挂在
		// message 里；两处都收，但**只有顶层那一处的 output_tokens 是终值**
		// （message_start 里恒为占位的 1）。
		if u, ok := m["usage"].(map[string]any); ok {
			info.bill.mergeMessagesUsage(u, true)
		}
		if msg, ok := m["message"].(map[string]any); ok {
			if u, ok := msg["usage"].(map[string]any); ok {
				info.bill.mergeMessagesUsage(u, false)
			}
		}
		// 输出文本：SSE 的 content_block_delta（正文 text 与工具入参
		// partial_json 都是产出），非流式的 content[] 文本块。
		if d, ok := m["delta"].(map[string]any); ok {
			info.bill.text.Add(jsonString(d["text"]))
			info.bill.text.Add(jsonString(d["partial_json"]))
		}
		for _, c := range jsonArray(m["content"]) {
			if blk, ok := c.(map[string]any); ok {
				info.bill.text.Add(jsonString(blk["text"]))
			}
		}
		s.tickProvisional(info)
	}
}

// responsesObserver 构造 Responses 入口（Agents / Codex 订阅代理）的响应
// 观察器。载荷形态与另外两个文本入口都不同，所以自成一个闭包：
//
//	非流式  顶层就是 response 对象：usage 在顶层，产出文本在 output[].content[].text；
//	SSE     事件帧：usage 只在 response.completed 的 response.usage 里，
//	        产出文本是各类 *.delta 事件的 delta 字符串（正文、推理摘要、
//	        工具入参都算模型产出）。
//
// **产出文本只从这两处各取一次，不重复计**：response.completed 事件里带着一份
// 完整的 response.output（正文全文），而那时逐条 delta 已经计过了——所以这里
// 只认**顶层**的 output（非流式才有），不认 response.output。重复计只会让
// 估算虚高，而估算恰恰是流被截断、没有 usage 时唯一的记账依据。
func (s *Server) responsesObserver(info *reqInfo) func(map[string]any) {
	return func(m map[string]any) {
		if u, ok := m["usage"].(map[string]any); ok {
			info.bill.mergeResponsesUsage(u)
		}
		if rsp, ok := m["response"].(map[string]any); ok {
			if u, ok := rsp["usage"].(map[string]any); ok {
				info.bill.mergeResponsesUsage(u)
			}
		}
		// SSE 增量：response.output_text.delta / reasoning_summary_text.delta /
		// function_call_arguments.delta 的 delta 都是**字符串**（不是 chat 那种
		// 对象），取到就算产出。
		info.bill.text.Add(jsonString(m["delta"]))
		addResponsesOutputText(&info.bill.text, m["output"])
		s.tickProvisional(info)
	}
}

// addResponsesOutputText 累计非流式 Responses 响应的产出文本：
// output[] 里每个 item 的 content[] 里每个块的 text。
// 取不到就当没有——估算是兜底，不该为形态不符报错或中断转发。
func addResponsesOutputText(c *usage.TextCounter, output any) {
	for _, item := range jsonArray(output) {
		it, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, part := range jsonArray(it["content"]) {
			if blk, ok := part.(map[string]any); ok {
				c.Add(jsonString(blk["text"]))
			}
		}
	}
}

// mergeResponsesUsage 合并 Responses 形 usage，逐字段非零覆盖。
//
// input_tokens 已含缓存读写；缓存分项只记录上游明确回传的计数，不推算。
// 缓存写入仍按普通输入价计，计价照 chat 那支公式走（usage.costText）。
//
//	input_tokens                           → Prompt
//	output_tokens                          → Completion
//	input_tokens_details.cached_tokens      → CacheRead
//	input_tokens_details.cache_write_tokens → CacheWrite（未回传时为 0）
//
// output_tokens_details.reasoning_tokens 有意不单独记：它是 output_tokens 的
// **子集**（厂商口径），另记一列就是在账本里重复计一次同样的量。
func (b *billState) mergeResponsesUsage(u map[string]any) {
	seen := setNonZero(&b.tokens.Prompt, u["input_tokens"])
	seen = setNonZero(&b.tokens.Completion, u["output_tokens"]) || seen
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		seen = setNonZero(&b.tokens.CacheRead, d["cached_tokens"]) || seen
		seen = setNonZero(&b.tokens.CacheWrite, d["cache_write_tokens"]) || seen
	}
	b.usageSeen = b.usageSeen || seen
}

// mergeChatUsage 合并 OpenAI 形 usage（含 DeepSeek 方言），逐字段非零覆盖。
//
// chat 口径下 prompt_tokens **已含**缓存命中，CacheWrite 恒 0（缓存写在这一
// 口径下就是普通输入）——两家 cache 口径相反，剥离在计价时做（usage.Tokens
// 的口径说明），这里只如实落位。
func (b *billState) mergeChatUsage(u map[string]any) {
	seen := setNonZero(&b.tokens.Prompt, u["prompt_tokens"])
	seen = setNonZero(&b.tokens.Completion, u["completion_tokens"]) || seen
	// 缓存命中两种拼法：OpenAI 标准挂在 prompt_tokens_details.cached_tokens，
	// DeepSeek 平铺成 prompt_cache_hit_tokens。两者都归 CacheRead。
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		seen = setNonZero(&b.tokens.CacheRead, d["cached_tokens"]) || seen
	}
	seen = setNonZero(&b.tokens.CacheRead, u["prompt_cache_hit_tokens"]) || seen
	b.usageSeen = b.usageSeen || seen
}

// mergeMessagesUsage 合并 Anthropic 形 usage，逐字段非零覆盖。
// input_tokens **不含** cache 两项，三者相加才是真实输入量（计价时按 entry
// 分流处理，这里同样只如实落位）。
//
// final 说明这份 usage 的 output_tokens 是不是终值：非流式响应体与
// message_delta 是（true），message_start 不是（false——那里的 output_tokens
// 恒为占位的 1）。只有见过终值才置 outputFinal，否则收尾按估算补输出并打标。
func (b *billState) mergeMessagesUsage(u map[string]any, final bool) {
	seen := setNonZero(&b.tokens.Prompt, u["input_tokens"])
	out := setNonZero(&b.tokens.Completion, u["output_tokens"])
	seen = out || seen
	seen = setNonZero(&b.tokens.CacheRead, u["cache_read_input_tokens"]) || seen
	seen = setNonZero(&b.tokens.CacheWrite, u["cache_creation_input_tokens"]) || seen
	b.usageSeen = b.usageSeen || seen
	if out && final {
		b.outputFinal = true
	}
}

// tickProvisional 在长流进行中把「到目前为止的估算金额」并进预算计数器。
//
// 没有它，一条十分钟的 SSE 在结束前对准入完全隐身：并发长流能把日预算打穿
// 一个量级。有了它，超限的暴露窗口收敛到 30s。计数器上的这笔是**临时**金额，
// 请求收尾时先原额冲平再按真值记一次（recordUsage），所以中间估得高或低都
// 不会在账上留下痕迹。
//
// 只在 SSE 上跑：非流式只观测一次，"中间"无从谈起。节拍由观测事件驱动而不是
// 定时器——长流必然在出事件，而没有事件的流也没有新消费可结算，正好不必为
// 每条在途请求养一个 goroutine。
func (s *Server) tickProvisional(info *reqInfo) {
	b := &info.bill
	if s.meter == nil || !b.sse {
		return
	}
	now := time.Now()
	if b.provisionalAt.IsZero() {
		b.provisionalAt = now // 首帧只起表，不结算
		return
	}
	if now.Sub(b.provisionalAt) < provisionalInterval {
		return
	}
	b.provisionalAt = now
	cost := usage.Cost(b.parsedPricing(), usage.Measure{
		Kind:  store.ModelKindText,
		Entry: b.entry,
		Tokens: usage.Tokens{
			Prompt:     b.inputEstimate(),
			Completion: b.text.Tokens(),
		},
	})
	// 只做单调递增的补差：估算随流只增不减，真要倒退（不该发生）也不在这里
	// 做负向调整——冲正统一在收尾一次做完，少一条能把计数器压负的路径。
	if d := cost - b.provisional; d > 0 {
		win := s.meter.AddProvisional(info.keyID, d)
		// 窗口在这条流中间翻过去了：旧窗口里的那部分已随计数器清零蒸发，
		// 收尾只该冲正落在**新**窗口里的部分，所以对应的分窗口累计从头算起。
		// 日翻而周/月不翻是常态（周内、月内跨日），三个数各判各的。
		if win.Day != b.provWin.Day {
			b.provDayMicro = 0
		}
		if win.Week != b.provWin.Week {
			b.provWeekMicro = 0
		}
		if win.Month != b.provWin.Month {
			b.provMonthMicro = 0
		}
		b.provDayMicro += d
		b.provWeekMicro += d
		b.provMonthMicro += d
		b.provWin = win
		b.provisional = cost
	}
}

// ---- 载荷取值小工具（都只取数字与字符串，取不到就当没有） ----

// setNonZero 把载荷里的一个数字字段写进 dst，**零与取不出来都不写**，
// 返回是否写了。逐字段非零覆盖合并靠它：后到的帧只带自己那几个字段，
// 不该把先到的已知值清回 0。
func setNonZero(dst *int64, v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	i, err := n.Int64()
	if err != nil || i <= 0 {
		return false
	}
	*dst = i
	return true
}

// jsonString 取字符串字段（不是字符串就得空串）。
func jsonString(v any) string {
	s, _ := v.(string)
	return s
}

// jsonArray 取数组字段（不是数组就得 nil，range 天然跳过）。
func jsonArray(v any) []any {
	a, _ := v.([]any)
	return a
}

// jsonObjectText 把载荷里的一个子对象重新序列化成原文 JSON（厂商 usage 原文
// 要按形态原样留存：aigc_tasks.usage_json 与图片入口的内存点位都是这个口径）。
// 取不到对象或序列化失败得空串。
func jsonObjectText(v any) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	raw, err := encodeJSON(m) // UseNumber 解析过，数字字面量不失真
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(raw), "\n")
}
