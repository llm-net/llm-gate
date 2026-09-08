// messages.go 实现 Anthropic 协议入口：POST /v1/messages（非流式 + SSE）与
// POST /v1/messages/count_tokens 的选路透传。
//
// 透传语义与 chat 入口一致（共享核心见 proxy.go）：请求体解析为通用 map，
// 只重写 model 为所选来源的 upstream_model_id，未知字段与非常规 stop_reason
// 双向原样保留；响应侧把 model（非流式顶层 / message_start 的 message.model）
// 改回客户端请求的模型名，其余事件内容不解析、逐行转发。模型的启用来源都不
// 服务 anthropic 协议时明确拒绝（404 protocol_mismatch），不做 openai↔anthropic
// 转换（MVP §6）。
//
// count_tokens 逐来源尝试：某来源 404/405 视为「该来源无此端点」，继续下一个
// 来源；全部来源都无端点才本地字符启发式粗估兜底（MVP §3.1）。
//
// 网关自身错误一律 Anthropic 风格 {"type":"error","error":{...}}。
// 日志遵守 §15.1：本文件绝不把请求/响应 body 或凭证传入 logger。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// messagesCall 是一次请求侧解析完成、待发往 anthropic 协议端点的调用。
type messagesCall struct {
	payload map[string]any // 解析后的请求体 map（每来源改写 model 后重序列化）
	model   string         // 客户端请求的模型名（响应回写目标，也用于脱敏日志）
	cands   []candidate    // 已按启停与 anthropic 协议过滤的有序候选来源
}

// body 按某个来源的 upstream_model_id 重建请求体。anthropic 协议无
// include_usage 一说：usage 随 message_delta 天然到达。
func (c messagesCall) body(upstreamModelID string) ([]byte, error) {
	c.payload["model"] = upstreamModelID
	return encodeJSON(c.payload)
}

// handleMessages 是 POST /v1/messages 的入口（已过认证中间件）：
// 非流式与 SSE 都经共享核心按响应 Content-Type 前缀透传。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	// admitBudget=true：生成入口是「新消费」，过预算准入。
	call, ok := s.prepareMessagesCall(w, r, true)
	if !ok {
		return
	}
	// 输入侧估算器只挂在生成入口上：count_tokens 走同一段准备代码，但它
	// 「只计一次请求、不计 token」（iteration-9 边界），不能跟着估算。
	info := infoFrom(r.Context())
	info.bill.estimateInput = func() int64 { return usage.EstimateInputTokens(call.payload) }
	s.forward(w, r, call.cands, forwardSpec{
		protocol: config.ProtocolAnthropicMessages,
		path:     "/messages",
		model:    call.model,
		body:     call.body,
		errStyle: anthropicErrorStyle,
		observe:  s.messagesObserver(info),
	})
}

// handleCountTokens 是 POST /v1/messages/count_tokens 的入口：逐个候选来源
// 尝试——404/405 记为「该来源无 count_tokens 端点」并试下一个来源，连接失败与
// 429/5xx 按故障切换规则试下一个来源，其余状态（200 与非 404/405 的 4xx/5xx）
// 立即原样透传。
//
// 只有**每个**候选都明确报了 404/405 才本地粗估兜底（记
// count_tokens_source="fallback_heuristic"）：只要有来源是连不上或 5xx，
// 它本可能给出真计数，此时回 502 而不是拿粗估掩盖上游故障。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	info := infoFrom(r.Context())
	// admitBudget=false：count_tokens 只计一次请求、不计 token 也不计金额
	// （iteration-9 边界），拿它去撞预算等于对着一笔零消费收紧额度。
	call, ok := s.prepareMessagesCall(w, r, false)
	if !ok {
		return
	}
	if len(call.cands) == 0 { // resolveRoute 保证 routeOK 时非空，防御分支
		s.log.Error("选路候选为空", "request_id", info.id, "model", call.model)
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", internalErrorMessage)
		return
	}

	spec := forwardSpec{
		protocol: config.ProtocolAnthropicMessages,
		path:     "/messages/count_tokens",
		model:    call.model,
		body:     call.body,
		errStyle: anthropicErrorStyle,
	}
	// allNoEndpoint 只在每个候选都回 404/405 时保持为真；任何连接失败或被
	// 切换掉的 429/5xx 都把它置假——那种来源不是"没有端点"，是"这次没答上"。
	allNoEndpoint := true
	lastStatus := 0
	for i, c := range call.cands {
		last := i == len(call.cands)-1
		info.attempts, info.upstream = i+1, c.account.Name

		attemptCtx, cancel := context.WithCancel(r.Context())
		req, ok := s.buildAttempt(attemptCtx, w, r, c, spec)
		if !ok {
			cancel()
			return // 内部错误已写出
		}
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			if r.Context().Err() != nil {
				s.log.Info("客户端断开，上游请求已取消", "request_id", info.id, "model", call.model)
				return
			}
			allNoEndpoint = false
			s.log.Warn("count_tokens 上游请求失败", "request_id", info.id, "model", call.model,
				"upstream", c.account.Name, "attempt", i+1, "err", err.Error())
			continue
		}
		// 404/405 视为该来源没有 count_tokens 端点：有界读尽错误体便于连接
		// 复用，内容不进日志（§15.1），继续下一个来源。
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
			discardBody(resp, cancel)
			lastStatus = resp.StatusCode
			continue
		}
		if !last && shouldFailover(resp.StatusCode) {
			discardBody(resp, cancel)
			allNoEndpoint = false
			lastStatus = resp.StatusCode
			s.log.Warn("count_tokens 上游返回可切换状态码，切换下一来源",
				"request_id", info.id, "model", call.model,
				"upstream", c.account.Name, "attempt", i+1, "upstream_status", resp.StatusCode)
			continue
		}
		// 其余状态码（200 与非 404/405 的 4xx/5xx）原样透传。
		defer cancel() // 回写完成后才取消
		s.passthroughResponse(w, r, resp, call.model)
		return
	}

	// 全部来源试尽。
	if !allNoEndpoint {
		writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"The gateway failed to reach the upstream provider.")
		return
	}
	n := heuristicInputTokens(call.payload)
	s.log.Info("count_tokens 上游无端点，本地粗估兜底",
		"request_id", info.id,
		"model", call.model,
		"attempts", info.attempts,
		"upstream_status", lastStatus,
		"count_tokens_source", "fallback_heuristic",
		"input_tokens", n,
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": n})
}

// passthroughResponse 把上游响应原样回写（状态码/响应头除逐跳头/响应体都不
// 改写）。用于没有 model 回写语义的响应：count_tokens 的计数体，以及视频任务
// 提交的未受理错误体（迭代 8——「厂商同步 4xx 原样透传，不包装」）。
func (s *Server) passthroughResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, model string) {
	defer resp.Body.Close()
	copyBackHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		// 与 commitResponse 同口径：客户端断开不算上游故障，别把它记成
		// 上游不稳定的信号。
		if r.Context().Err() != nil {
			s.log.Info("客户端断开，响应回写终止",
				"request_id", infoFrom(r.Context()).id, "model", model)
			return
		}
		s.log.Warn("上游响应中途断流",
			"request_id", infoFrom(r.Context()).id, "model", model, "err", err.Error())
	}
}

// prepareMessagesCall 完成 messages 系入口共同的请求侧处理：读 body、以
// UseNumber 解析为 map（未知字段保留）、取模型名、按 anthropic 协议选路。
// 任何失败都已写出 Anthropic 风格错误并返回 ok=false。
//
// admitBudget 决定这一路要不要过预算准入：两个入口共用本函数，而它们对预算
// 的关系相反——生成是新消费（要过），count_tokens 只计一次请求（不过）。
// 准入的位置由此固定在「模型名已知、尚未选路」之间。
func (s *Server) prepareMessagesCall(w http.ResponseWriter, r *http.Request, admitBudget bool) (messagesCall, bool) {
	// 读体 + 通用 map 解析 + model 校验：三入口共享的 decodeEntryPayload
	// （entry.go）；anthropicErrorStyle 按既有规则丢弃 code、只写 type+message。
	payload, model, ok := decodeEntryPayload(w, r, anthropicErrorStyle)
	if !ok {
		return messagesCall{}, false
	}
	// 自此本请求要入账（iteration-9）。count_tokens 与生成入口同属 messages
	// 入口：两者都计一次请求，token 与金额只有生成入口有。
	beginEntry(r, usage.EntryMessages, model)
	if admitBudget && !s.admit(w, r, anthropicErrorStyle) {
		return messagesCall{}, false
	}

	// 每请求点查三表选路（决策 3）：启停、优先级、换 Key 即时生效。
	// 文本入口只收 kind=text（迭代 8 闸门）：视频/图片模型在这里与不存在同响应。
	cands, status := s.resolveRoute(r.Context(), model, store.ModelKindText, config.ProtocolAnthropicMessages)
	switch status {
	case routeModelNotFound:
		// 只回显客户端提交的模型名，不提示任何上游信息。
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("The model %q does not exist or you do not have access to it.", model))
		return messagesCall{}, false
	case routeProtocolMismatch:
		// 该模型的启用来源都不服务 anthropic 协议（如只挂 ark 型来源）：
		// 明确拒绝并注明所属入口，不做跨协议转换（MVP §6）。
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			protocolMismatchMessage(model, config.ProtocolAnthropicMessages))
		return messagesCall{}, false
	case routeStoreError:
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", internalErrorMessage)
		return messagesCall{}, false
	}
	return messagesCall{payload: payload, model: model, cands: cands}, true
}

// heuristicInputTokens 是 count_tokens 的本地粗估：messages 与 system 字段
// JSON 序列化后的字符数（按 rune 计）之和 /4 向上取整，至少 1。只求量级与
// 确定性，不追求精确——任一来源有 count_tokens 端点时一律透传上游计数，
// 本粗估仅在全部来源都 404/405 时兜底（MVP §3.1）。
func heuristicInputTokens(payload map[string]any) int {
	chars := 0
	for _, field := range []string{"messages", "system"} {
		v, ok := payload[field]
		if !ok || v == nil {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil { // payload 源自合法 JSON，理论不可达
			continue
		}
		chars += utf8.RuneCount(b)
	}
	if n := (chars + 3) / 4; n > 0 {
		return n
	}
	return 1
}
