// responses_catalog.go 实现 POST /v1/responses——标准 Responses 目录面
// （2026-08-13 按 2026-08-12 裁决挂载，触发条件②：官方 codex 只有
// wire_api="responses" 一种形态，而用户要它够到模型目录里的按量模型。
// 契约整节见 docs/firmware-gateway.md「Responses 目录面」）。
//
// 它与 /agents/{codex,grok}/v1/responses（订阅代理，responses.go）是两回事：那边解析订阅
// 账号、不选路、0 元记账；这边只认客户端 API 密钥与模型目录——选路、准入、
// 计量、故障切换全套复用 chat 的既有机制（resolveRoute + forward），差异收在
// 两端的**形态转换**：
//
//	请求侧  Responses → chat（responsesToChatPayload）：无状态子集校验
//	        （store:true / previous_response_id 明确 400）、input items 折成
//	        messages、工具与标量映射；无 chat 等价物的旋钮按契约丢弃。
//	响应侧  chat → Responses：非流式折成单个 response 对象（chatToResponse），
//	        SSE 按 Responses 事件序合成（responsesSynth：response.created →
//	        output_item/content_part/delta… → response.completed，无 [DONE]）。
//
// 字节保真契约对本入口**不适用**（这是转换面），唯一例外是上游 4xx/5xx
// 错误体——OpenAI 错误信封两面同形，原样透传（复用 commitResponse）。
//
// §15.1：本文件绝不把请求/响应 body 传入 logger；转换中的文本只在内存里活到
// 重编码结束，计量只取数字与 rune 计数（chatObserver 的既有纪律）。
package gateway

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// handleResponsesCatalog 是 POST /v1/responses 的入口（已过认证中间件）。
func (s *Server) handleResponsesCatalog(w http.ResponseWriter, r *http.Request) {
	payload, model, ok := decodeEntryPayload(w, r, openAIErrorStyle)
	if !ok {
		return
	}
	s.handleResponsesCatalogPayload(w, r, payload, model)
}

// handleResponsesCatalogPayload 承接一份已经解码的目录 Responses 请求。
// 标准 /v1/responses 与 Codex 混合入口选中的普通模型共用这一段。
func (s *Server) handleResponsesCatalogPayload(w http.ResponseWriter, r *http.Request, payload map[string]any, model string) {
	// 自此本请求要入账：entry 与订阅代理分开（这边 responses、金额是真钱；
	// 那边 responses_agents、0 元真值），估算兜底按 Responses 形读
	// input/instructions。
	info := beginEntry(r, usage.EntryResponses, model)
	info.bill.estimateInput = func() int64 { return usage.EstimateResponsesInput(payload) }
	// 预算准入：与其他消费入口同位同序（鉴权之后、任何点查之前）。
	if !s.admit(w, r, openAIErrorStyle) {
		return
	}
	// 形态转换 + 无状态子集校验：准入之后（契约不合的请求也是一次真实的调用
	// 尝试，照样入账占 RPM）、选路之前（不该为注定 400 的请求消耗三表点查）。
	chatPayload, convErr := responsesToChatPayload(payload)
	if convErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", convErr.code, convErr.message)
		return
	}
	// Responses 独立检查模型协议面开关；候选来源使用 Chat 上游承载转换。
	cands, status := s.resolveRoute(r.Context(), model, store.ModelKindText, config.ProtocolOpenAIResponses)
	switch status {
	case routeModelNotFound:
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("The model %q does not exist or you do not have access to it.", model))
		return
	case routeProtocolMismatch:
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "protocol_mismatch",
			protocolMismatchMessage(model, config.ProtocolOpenAIResponses))
		return
	case routeStoreError:
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error",
			internalErrorMessage)
		return
	}
	clientStream, _ := chatPayload["stream"].(bool)
	if clientStream {
		injectIncludeUsage(chatPayload)
	}
	capabilityDoc := s.effectivePlatformModels(r.Context())
	s.forward(w, r, cands, forwardSpec{
		protocol: config.ProtocolOpenAIChat,
		path:     "/chat/completions",
		model:    model,
		bodyFor: func(c candidate) ([]byte, error) {
			caps, _ := capabilityDoc.ModelCapabilitiesFor(c.catalogID, c.account.Type, model, c.modelID)
			attemptPayload := chatPayload
			if caps.ResponsesChat.Profile != "" {
				// 能力按候选来源解析并重建，故障切换到另一平台时不会携带前一家
				// 的专属字段。目录只组合固件认识的有界 profile。
				attemptPayload, _ = responsesToChatPayloadWithCompat(payload, responsesChatCompat{capability: caps.ResponsesChat})
				if clientStream {
					injectIncludeUsage(attemptPayload)
				}
			}
			attemptPayload["model"] = c.modelID
			return encodeJSON(attemptPayload)
		},
		errStyle: openAIErrorStyle,
		commit:   s.commitResponsesCatalog(info, model, clientStream),
	})
}

// ---- 请求侧：Responses → chat ----

// convError 是转换面的契约拒绝：code 是机读原因，message 给客户端。
type convError struct{ code, message string }

// responsesChatCompat 只收会改变上游请求语义的有界能力。普通目录模型保持
// Responses 无状态子集的通用转换；声明 profile 的具体平台模型才启用扩展。
type responsesChatCompat struct {
	capability platformcatalog.ResponsesChatCapabilities
}

// responsesToChatPayload 把一个 Responses 请求折成 chat 请求。
//
// 契约（docs/firmware-gateway.md）：尽力映射——硬拒只有无状态子集越界、媒体
// 输入、未知 input item、非 function 工具这几条点名项；其余没有 chat 等价物的
// 字段（reasoning/include/prompt_cache_key/metadata/truncation 等）一律丢弃，
// 逐项硬拒等于拒掉 codex 这个客户端（它每个请求都带其中几项）。
func responsesToChatPayload(p map[string]any) (map[string]any, *convError) {
	return responsesToChatPayloadWithCompat(p, responsesChatCompat{})
}

func responsesToChatPayloadWithCompat(p map[string]any, compat responsesChatCompat) (map[string]any, *convError) {
	if v, ok := p["store"].(bool); ok && v {
		return nil, &convError{"responses_stateful_unsupported",
			"This device serves the stateless subset of the Responses API: set store to false (responses are never persisted here)."}
	}
	if v, ok := p["previous_response_id"].(string); ok && v != "" {
		return nil, &convError{"responses_stateful_unsupported",
			"This device serves the stateless subset of the Responses API: previous_response_id is not supported, resend the full conversation in input."}
	}

	messages := make([]any, 0, 8)
	if instr, ok := p["instructions"].(string); ok && instr != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instr})
	}
	switch in := p["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": in})
	case []any:
		converted, cerr := convertInputItems(in, compat.capability.ReplayReasoningContent)
		if cerr != nil {
			return nil, cerr
		}
		messages = append(messages, converted...)
	case nil: // 缺失：只有 instructions（或什么都没有）——让上游按它的规则答复
	default:
		return nil, &convError{"responses_input_unsupported",
			"The input field must be a string or an array of input items."}
	}
	out := map[string]any{"messages": messages}

	if rawTools, ok := p["tools"].([]any); ok && len(rawTools) > 0 {
		tools := make([]any, 0, len(rawTools))
		for _, rt := range rawTools {
			tool, ok := rt.(map[string]any)
			if !ok {
				return nil, &convError{"responses_tool_unsupported", "Each tool must be a JSON object."}
			}
			typ, _ := tool["type"].(string)
			if typ != "function" {
				// **丢弃而不是 400**（2026-08-13 真 codex 实测改判）：codex 0.147
				// 缺省工具清单就带 namespace（multi_agent_v1 分组）与 web_search
				// 两种非 function 类型，硬拒等于拒掉整个客户端。丢弃的语义是
				// 「这条路上没有这个工具」——模型不会调用它，CLI 对「模型没用
				// 某工具」本就容忍（多 agent、联网在目录模型路上如实不可用）。
				// 决不能做的是改名平铺 namespace 内层函数：模型会照平铺名发起
				// 调用，而 CLI 侧按什么名字派发未经实证，凭空造一个「模型会调、
				// CLI 不认」的名字比没有更糟。tool_choice 强制指向被丢弃的工具
				// 仍是 400（convertToolChoice）——强制要求满足不了就不假装。
				continue
			}
			fn := map[string]any{"name": tool["name"]}
			if d, ok := tool["description"]; ok && d != nil {
				fn["description"] = d
			}
			if params, ok := tool["parameters"]; ok && params != nil {
				fn["parameters"] = params
			}
			// strict 有意丢弃：严格 schema 执行在各家目录上游上支持不一，带上
			// 反而让部分上游 400。
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 { // 全被丢弃时整个键不带：空 tools 数组会让部分上游 400
			out["tools"] = tools
		}
	}
	if tc, ok := p["tool_choice"]; ok && tc != nil && !compat.capability.DropToolChoice {
		conv, cerr := convertToolChoice(tc)
		if cerr != nil {
			return nil, cerr
		}
		if conv != nil {
			out["tool_choice"] = conv
		}
	}
	for _, key := range [...]string{"temperature", "top_p", "parallel_tool_calls", "user", "stream"} {
		if v, ok := p[key]; ok && v != nil {
			out[key] = v
		}
	}
	if v, ok := p["max_output_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	}
	if rf := convertTextFormat(p["text"]); rf != nil {
		out["response_format"] = rf
	}
	if compat.capability.Profile != "" {
		if compat.capability.ThinkingType != "" {
			out["thinking"] = map[string]any{"type": compat.capability.ThinkingType}
		}
		if reasoning, ok := p["reasoning"].(map[string]any); ok {
			if mapped := compat.capability.EffortMap[jsonString(reasoning["effort"])]; mapped != "" {
				out["reasoning_effort"] = mapped
			}
		}
	}
	return out, nil
}

// convertInputItems 把有序 Responses items 折成 chat messages。普通模型逐项
// 转换；DeepSeek V4 还要把同一轮的 reasoning/message/function_call 合成一条
// assistant 消息，否则工具结果回传时上游无法继续 thinking 链。
func convertInputItems(items []any, replayDeepSeekReasoning bool) ([]any, *convError) {
	if !replayDeepSeekReasoning {
		out := make([]any, 0, len(items))
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				return nil, &convError{"responses_input_unsupported", "Each input item must be a JSON object."}
			}
			converted, cerr := convertInputItem(item)
			if cerr != nil {
				return nil, cerr
			}
			out = append(out, converted...)
		}
		return out, nil
	}

	out := make([]any, 0, len(items))
	var pendingAssistant map[string]any
	pendingReasoning := ""
	flushAssistant := func() {
		if pendingAssistant != nil {
			if pendingReasoning != "" {
				pendingAssistant["reasoning_content"] = pendingReasoning
			}
			out = append(out, pendingAssistant)
		}
		pendingAssistant = nil
		pendingReasoning = ""
	}

	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, &convError{"responses_input_unsupported", "Each input item must be a JSON object."}
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "reasoning":
			flushAssistant()
			pendingReasoning = reasoningSummaryText(item)
		case "function_call":
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID, _ = item["id"].(string)
			}
			if pendingAssistant == nil {
				pendingAssistant = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{}}
			}
			calls, _ := pendingAssistant["tool_calls"].([]any)
			pendingAssistant["tool_calls"] = append(calls, map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{"name": jsonString(item["name"]), "arguments": jsonString(item["arguments"])},
			})
		case "function_call_output":
			flushAssistant()
			converted, cerr := convertInputItem(item)
			if cerr != nil {
				return nil, cerr
			}
			out = append(out, converted...)
		case "message", "":
			role, _ := item["role"].(string)
			if role != "assistant" {
				flushAssistant()
				converted, cerr := convertInputItem(item)
				if cerr != nil {
					return nil, cerr
				}
				out = append(out, converted...)
				continue
			}
			text, cerr := messageText(item["content"])
			if cerr != nil {
				return nil, cerr
			}
			if pendingAssistant == nil {
				pendingAssistant = map[string]any{"role": "assistant", "content": text}
			} else {
				pendingAssistant["content"] = text
			}
		default:
			flushAssistant()
			converted, cerr := convertInputItem(item)
			if cerr != nil {
				return nil, cerr
			}
			out = append(out, converted...)
		}
	}
	flushAssistant()
	return out, nil
}

func reasoningSummaryText(item map[string]any) string {
	parts := make([]string, 0, 1)
	for _, raw := range jsonArray(item["summary"]) {
		if part, ok := raw.(map[string]any); ok {
			if text := jsonString(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// convertInputItem 把一个 input item 折成零或多条 chat 消息。
func convertInputItem(item map[string]any) ([]any, *convError) {
	typ, _ := item["type"].(string)
	switch typ {
	case "message", "": // 官方也接受省略 type 的 {role, content} 简写
		role, _ := item["role"].(string)
		if role == "" {
			return nil, &convError{"responses_input_unsupported", "An input message item needs a role."}
		}
		if role == "developer" { // chat 侧没有 developer 语域，目录上游只认 system
			role = "system"
		}
		text, cerr := messageText(item["content"])
		if cerr != nil {
			return nil, cerr
		}
		return []any{map[string]any{"role": role, "content": text}}, nil
	case "function_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		// content 给空串而不是 null：null content 在部分自建推理服务上被拒，
		// 空串各家都收。
		return []any{map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}, nil
	case "function_call_output":
		callID, _ := item["call_id"].(string)
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      outputText(item["output"]),
		}}, nil
	case "reasoning":
		// 静默丢弃（契约点名）：推理密文是发起方厂商的私有物，跨厂商无法回放；
		// codex 每轮都会带上一轮的 reasoning item，硬拒会废掉多轮会话。
		return nil, nil
	default:
		return nil, &convError{"responses_input_unsupported",
			fmt.Sprintf("Input item type %q is not supported by this device.", typ)}
	}
}

// messageText 把 message item 的 content 折成纯文本。字符串直接用；块数组只收
// 文本块（input_text/output_text/summary_text 与 refusal），媒体块明确 400——
// 文本目录面不假装收媒体。
func messageText(content any) (string, *convError) {
	switch c := content.(type) {
	case string:
		return c, nil
	case []any:
		var parts []string
		for _, raw := range c {
			blk, ok := raw.(map[string]any)
			if !ok {
				return "", &convError{"responses_input_unsupported", "Each content part must be a JSON object."}
			}
			typ, _ := blk["type"].(string)
			switch typ {
			case "input_text", "output_text", "summary_text", "text":
				parts = append(parts, jsonString(blk["text"]))
			case "refusal":
				parts = append(parts, jsonString(blk["refusal"]))
			default:
				return "", &convError{"responses_input_unsupported", fmt.Sprintf(
					"Content part type %q is not supported by this device (text-only catalog entry).", typ)}
			}
		}
		return strings.Join(parts, "\n"), nil
	case nil:
		return "", nil
	default:
		return "", &convError{"responses_input_unsupported",
			"A message content must be a string or an array of content parts."}
	}
}

// outputText 把 function_call_output 的 output 折成字符串：字符串直接用，块
// 数组取文本块拼接，其余形态原样 JSON 序列化（尽力而为——工具输出是模型要读
// 的上下文，不值得为形态花样拒绝整个请求）。
func outputText(v any) string {
	switch o := v.(type) {
	case string:
		return o
	case []any:
		var parts []string
		for _, raw := range o {
			if blk, ok := raw.(map[string]any); ok {
				if t := jsonString(blk["text"]); t != "" {
					parts = append(parts, t)
					continue
				}
			}
			raw := raw
			if b, err := encodeJSON(raw); err == nil {
				parts = append(parts, strings.TrimSuffix(string(b), "\n"))
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		b, err := encodeJSON(o)
		if err != nil {
			return ""
		}
		return strings.TrimSuffix(string(b), "\n")
	}
}

// convertToolChoice 映射 tool_choice：三个字符串档照传，具名函数换嵌套形，
// 其余对象形（hosted tool 等）明确拒绝。
func convertToolChoice(v any) (any, *convError) {
	switch tc := v.(type) {
	case string:
		return tc, nil
	case map[string]any:
		if typ, _ := tc["type"].(string); typ == "function" {
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": tc["name"]},
			}, nil
		}
		return nil, &convError{"responses_tool_unsupported",
			"Only string tool_choice values and {\"type\":\"function\",\"name\":…} are supported by this device."}
	default:
		return nil, nil // null 等无意义形态：丢弃，让上游按缺省行为走
	}
}

// convertTextFormat 把 text.format 映射成 chat 的 response_format。
// type=text 与认不出的形态一律丢弃（尽力映射口径：硬拒只有契约点名那几条）。
func convertTextFormat(v any) any {
	txt, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	format, ok := txt["format"].(map[string]any)
	if !ok {
		return nil
	}
	switch typ, _ := format["type"].(string); typ {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := map[string]any{"name": format["name"]}
		if s, ok := format["schema"]; ok && s != nil {
			js["schema"] = s
		}
		if s, ok := format["strict"]; ok && s != nil {
			js["strict"] = s
		}
		if d, ok := format["description"]; ok && d != nil {
			js["description"] = d
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	default:
		return nil
	}
}

// ---- 响应侧：chat → Responses ----

// commitResponsesCatalog 构造本入口的提交段（forwardSpec.commit）：2xx 按客户
// 端要求的形态合成 Responses 响应；非 2xx 错误体原样透传（OpenAI 错误信封两面
// 同形，复用 commitResponse 的既有处置，含 SSE 形错误与逐跳头剥离）。
//
// 四种形态组合都能走（客户端要不要流 × 上游答没答流）：常态是同构直转，
// 错位的两种（上游没按 stream 答）经聚合/整体合成兜住——上游违约不该变成
// 客户端侧的解析失败。
func (s *Server) commitResponsesCatalog(info *reqInfo, model string, clientStream bool) func(http.ResponseWriter, *http.Request, *http.Response) {
	observe := s.chatObserver(info)
	return func(w http.ResponseWriter, r *http.Request, resp *http.Response) {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			// 错误体不改写不合成（status≥400 收尾也不记 token），交给透传核心。
			s.commitResponse(w, r, resp, respRewrite{model: model}, openAIErrorStyle)
			return
		}
		defer resp.Body.Close()
		respID := "resp_" + info.id
		upstreamSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
		info.bill.sse = upstreamSSE // 长流中间结算只在真的流上跑

		if !upstreamSSE {
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				if r.Context().Err() != nil {
					s.log.Info("客户端断开，上游请求已取消", "request_id", info.id, "model", model)
					return
				}
				s.log.Warn("读取上游响应失败", "request_id", info.id, "model", model, "err", err.Error())
				openAIErrorStyle(w, http.StatusBadGateway, "upstream_read_error",
					"The gateway failed to read the upstream response.")
				return
			}
			chatObj, ok := decodeJSONObject(respBody)
			if !ok {
				s.log.Warn("上游 2xx 响应不是 JSON 对象，无法合成 Responses 形",
					"request_id", info.id, "model", model)
				openAIErrorStyle(w, http.StatusBadGateway, "upstream_protocol_error",
					"The upstream answered a response this gateway could not convert to the Responses API.")
				return
			}
			observe(chatObj)
			if !clientStream {
				writeJSONResponse(w, chatToResponse(chatObj, respID, model))
				return
			}
			// 客户端要流而上游整体作答：按同一事件序把完整产出一次性放流。
			beginSSE(w)
			synth := newResponsesSynth(w, respID, model)
			synth.start()
			synth.feedChatObject(chatObj)
			synth.finish()
			return
		}

		// 上游是 SSE。客户端不要流时聚合成整体，要流时逐块合成事件。
		if !clientStream {
			accum := &chatStreamAccum{}
			err := scanChatSSE(resp.Body, func(m map[string]any) {
				observe(m)
				accum.feed(m)
			})
			if err != nil {
				if r.Context().Err() != nil {
					s.log.Info("客户端断开，上游请求已取消", "request_id", info.id, "model", model)
					return
				}
				s.log.Warn("读取上游流失败", "request_id", info.id, "model", model, "err", err.Error())
				openAIErrorStyle(w, http.StatusBadGateway, "upstream_read_error",
					"The gateway failed to read the upstream response.")
				return
			}
			writeJSONResponse(w, chatToResponse(accum.result(), respID, model))
			return
		}

		beginSSE(w)
		synth := newResponsesSynth(w, respID, model)
		synth.start()
		err := scanChatSSE(resp.Body, func(m map[string]any) {
			observe(m)
			synth.feedChatChunk(m)
		})
		if err != nil {
			// 响应已开始：错误进不了状态码，尽力发一个 response.failed 事件。
			if r.Context().Err() != nil {
				s.log.Info("客户端断开，转发终止（上游请求已取消）",
					"request_id", info.id, "model", model, "sse", true)
			} else {
				s.log.Warn("上游响应中途断流", "request_id", info.id, "model", model,
					"sse", true, "err", err.Error())
				synth.fail("upstream_error", "The upstream stream ended unexpectedly.")
			}
			return
		}
		synth.finish()
	}
}

// writeJSONResponse 写出一个合成的 JSON 响应（只带设备自己的头——chat 侧的
// 上游响应头对 Responses 客户端没有意义，契约明确不搬）。
func writeJSONResponse(w http.ResponseWriter, obj map[string]any) {
	buf, err := encodeJSON(obj)
	if err != nil { // 合成对象必可序列化；防御分支
		openAIErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

// scanChatSSE 逐行读一条 chat SSE 流：每个解析成对象的 data: 行回调一次，
// [DONE] 结束；其余行（event:/注释/空行/解析不出的载荷）跳过。返回读错误
// （EOF 不算——没等到 [DONE] 的 EOF 由调用方按「已尽力」处理，聚合结果里
// usage 缺失自然落估算路径）。
func scanChatSSE(src io.Reader, onChunk func(map[string]any)) error {
	br := bufio.NewReader(src)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if payload, ok := sseDataPayload(line); ok {
				if bytes.Equal(payload, []byte("[DONE]")) {
					return nil
				}
				if m, ok := decodeJSONObject(payload); ok {
					onChunk(m)
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// sseDataPayload 取一个 SSE 行的 data: 载荷（去掉前缀、分隔空格与行尾），
// 非 data: 行返回 ok=false。
func sseDataPayload(line []byte) ([]byte, bool) {
	const prefix = "data:"
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil, false
	}
	rest := bytes.TrimRight(line[len(prefix):], "\r\n")
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return rest, true
}

// chatStreamAccum 把一条 chat SSE 流聚合成等价的非流式 chat 对象（客户端不要
// 流而上游只会流时的兜底）。只聚合 choices[0]——本入口契约只取第一个 choice。
type chatStreamAccum struct {
	content   strings.Builder
	reasoning strings.Builder
	toolCalls []map[string]any // 按到达顺序；idx 映射见 tcIndex
	// tcArgs 与 toolCalls 平行：arguments 增量进 Builder，result 时一次成串。
	// 直接往 map 里的字符串上拼是 O(n²)——工具入参可达几十 KB、数百帧。
	tcArgs  []*strings.Builder
	tcIndex map[string]int // chat delta 的 index 字面量 → toolCalls 下标
	finish  string
	usage   map[string]any
}

func (a *chatStreamAccum) feed(m map[string]any) {
	if u, ok := m["usage"].(map[string]any); ok {
		a.usage = u
	}
	choices := jsonArray(m["choices"])
	if len(choices) == 0 {
		return
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	if f := jsonString(ch["finish_reason"]); f != "" {
		a.finish = f
	}
	delta, ok := ch["delta"].(map[string]any)
	if !ok {
		return
	}
	a.content.WriteString(jsonString(delta["content"]))
	a.reasoning.WriteString(jsonString(delta["reasoning_content"]))
	for _, raw := range jsonArray(delta["tool_calls"]) {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key := jsonScalarLiteral(tc["index"])
		if a.tcIndex == nil {
			a.tcIndex = map[string]int{}
		}
		pos, seen := a.tcIndex[key]
		if !seen {
			pos = len(a.toolCalls)
			a.tcIndex[key] = pos
			a.toolCalls = append(a.toolCalls, map[string]any{
				"id": "", "type": "function",
				"function": map[string]any{"name": "", "arguments": ""},
			})
			a.tcArgs = append(a.tcArgs, &strings.Builder{})
		}
		cur := a.toolCalls[pos]
		if id := jsonString(tc["id"]); id != "" {
			cur["id"] = id
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			cf := cur["function"].(map[string]any)
			if name := jsonString(fn["name"]); name != "" {
				cf["name"] = name
			}
			a.tcArgs[pos].WriteString(jsonString(fn["arguments"]))
		}
	}
}

// result 摊出聚合结果（非流式 chat 对象形）。
func (a *chatStreamAccum) result() map[string]any {
	msg := map[string]any{"role": "assistant", "content": a.content.String()}
	if a.reasoning.Len() > 0 {
		msg["reasoning_content"] = a.reasoning.String()
	}
	if len(a.toolCalls) > 0 {
		tcs := make([]any, 0, len(a.toolCalls))
		for i, tc := range a.toolCalls {
			tc["function"].(map[string]any)["arguments"] = a.tcArgs[i].String()
			tcs = append(tcs, tc)
		}
		msg["tool_calls"] = tcs
	}
	out := map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": a.finish}},
	}
	if a.usage != nil {
		out["usage"] = a.usage
	}
	return out
}

// chatToResponse 把一个非流式 chat 对象折成 Responses 的 response 对象。
func chatToResponse(chatObj map[string]any, respID, model string) map[string]any {
	var output []any
	finish := ""
	itemN := 0
	if choices := jsonArray(chatObj["choices"]); len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			finish = jsonString(ch["finish_reason"])
			if msg, ok := ch["message"].(map[string]any); ok {
				if rc := jsonString(msg["reasoning_content"]); rc != "" {
					output = append(output, reasoningItem(itemID(respID, "rs", itemN), rc))
					itemN++
				}
				if content := jsonString(msg["content"]); content != "" {
					output = append(output, messageItem(itemID(respID, "msg", itemN), content, "completed"))
					itemN++
				}
				for _, raw := range jsonArray(msg["tool_calls"]) {
					tc, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					name, args := "", ""
					if fn, ok := tc["function"].(map[string]any); ok {
						name, args = jsonString(fn["name"]), jsonString(fn["arguments"])
					}
					output = append(output,
						functionCallItem(itemID(respID, "fc", itemN), jsonString(tc["id"]), name, args, "completed"))
					itemN++
				}
			}
		}
	}
	if output == nil {
		output = []any{}
	}
	status, incomplete := responseStatus(finish)
	resp := map[string]any{
		"id":                 respID,
		"object":             "response",
		"created_at":         time.Now().Unix(),
		"status":             status,
		"model":              model, // 恒为客户端请求名：合成时直接写，来源侧 ID 无泄露面
		"output":             output,
		"store":              false,
		"error":              nil,
		"incomplete_details": incomplete,
	}
	if u, ok := chatObj["usage"].(map[string]any); ok {
		resp["usage"] = responsesUsageFromChat(u)
	}
	return resp
}

// responseStatus 把 chat 的 finish_reason 折成 response 的 status 与
// incomplete_details（无则 nil——字段照 OpenAI 形恒在场）。
func responseStatus(finish string) (string, any) {
	switch finish {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

// responsesUsageFromChat 把 OpenAI 形 chat usage 映射成 Responses 形。
// 口径同一（prompt 已含缓存命中），只换字段名；缓存命中两种拼法都收
// （标准 prompt_tokens_details.cached_tokens 与 DeepSeek 平铺的
// prompt_cache_hit_tokens），与计量侧 mergeChatUsage 同一份认定。
func responsesUsageFromChat(u map[string]any) map[string]any {
	prompt := usageNumber(u["prompt_tokens"])
	completion := usageNumber(u["completion_tokens"])
	cached := int64(0)
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = usageNumber(d["cached_tokens"])
	}
	if cached == 0 {
		cached = usageNumber(u["prompt_cache_hit_tokens"])
	}
	reasoning := int64(0)
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoning = usageNumber(d["reasoning_tokens"])
	}
	total := usageNumber(u["total_tokens"])
	if total == 0 {
		total = prompt + completion
	}
	return map[string]any{
		"input_tokens":          prompt,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         completion,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          total,
	}
}

// usageNumber 取一个 usage 数字字段（取不出按 0——合成 usage 的字段恒在场，
// 0 比缺字段对客户端更友好）。
func usageNumber(v any) int64 {
	var n int64
	setNonZero(&n, v)
	return n
}

// itemID 造一个 output item 的 id：respID 已含本请求的 request-id，序号保证
// 同响应内唯一。形如 msg_resp_<reqid>_0。合成流里每个事件都要它，
// 直接拼接（fmt 反射在这条逐帧路径上是纯浪费）。
func itemID(respID, kind string, n int) string {
	return kind + "_" + respID + "_" + strconv.Itoa(n)
}

// catalogReasoningIDPrefix 是本面合成的 reasoning item id 的固定前缀：
// itemID("resp_<reqid>", "rs", n) → "rs_resp_<reqid>_n"。Codex 每轮把上一轮的
// reasoning item 原样（带 id）回放，同一会话从目录模型切到订阅模型时这些 id 会
// 跟着进订阅后端；OpenAI 认不出它们（既没有 encrypted_content、也不在它的
// 存储里）就整个请求 404。Codex 订阅代理转发前按这个前缀把它们摘掉
// （responses_codex.go dropCatalogReasoningItems）。改 itemID 的形状必须连
// 这里一起改，内部测试钉住两者一致。
const catalogReasoningIDPrefix = "rs_resp_"

// beginSSE 在首个事件写出之前敲定合成 SSE 的响应头。两个放流入口（上游本身
// 是 SSE / 上游整体作答而客户端要流）都必须先过这里：不设 Content-Type 的话，
// 首次 Write 会触发 net/http 的内容嗅探，把事件流当 text/plain 发出去——
// 按 Content-Type 分发的 Responses 客户端会直接拒收。
func beginSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// messageItem / functionCallItem / reasoningItem 是三种 output item 的成形。
func messageItem(id, text, status string) map[string]any {
	return map[string]any{
		"id": id, "type": "message", "status": status, "role": "assistant",
		"content": []any{outputTextPart(text)},
	}
}

func outputTextPart(text string) map[string]any {
	return map[string]any{"type": "output_text", "annotations": []any{}, "text": text}
}

func functionCallItem(id, callID, name, args, status string) map[string]any {
	return map[string]any{
		"id": id, "type": "function_call", "status": status,
		"call_id": callID, "name": name, "arguments": args,
	}
}

func reasoningItem(id, summary string) map[string]any {
	return map[string]any{
		"id": id, "type": "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": summary}},
	}
}

// ---- SSE 合成器 ----

// responsesSynth 把 chat 流合成 Responses 事件流。事件带自增 sequence_number；
// 以 response.completed 结束，**没有 [DONE]**（Responses SSE 的既定形态）。
// 首个写失败后闩住（客户端断开时不再空转合成）。
type responsesSynth struct {
	w       io.Writer
	respID  string
	model   string
	created int64
	seq     int64
	failed  bool // 写失败闩

	// 当前敞开的 item（一次只开一个：chat 流的产出天然按段到达，段切换即
	// 关旧开新；并行工具调用按 chat 的 index 切段）。
	itemType string // "" | "message" | "reasoning" | "function_call"
	itemIdx  int    // 当前 item 的 output_index
	nextIdx  int    // 下一个 item 的 output_index
	buf      strings.Builder
	fcCallID string
	fcName   string
	fcKey    string // 当前 function_call 对应的 chat index 字面量

	finishReason string
	usage        map[string]any
	output       []any // 已收口的 item（response.completed 的 output）
}

func newResponsesSynth(w http.ResponseWriter, respID, model string) *responsesSynth {
	return &responsesSynth{w: &flushWriter{w: w}, respID: respID, model: model, created: time.Now().Unix()}
}

// emit 写出一个事件帧（event: 行 + data: 行）。写失败闩住后续输出。
func (y *responsesSynth) emit(event string, data map[string]any) {
	if y.failed {
		return
	}
	data["type"] = event
	data["sequence_number"] = y.seq
	y.seq++
	encoded, err := encodeJSON(data)
	if err != nil { // 合成载荷必可序列化；防御分支
		return
	}
	encoded = bytes.TrimSuffix(encoded, []byte("\n"))
	frame := make([]byte, 0, len(event)+len(encoded)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, encoded...)
	frame = append(frame, "\n\n"...)
	if _, err := y.w.Write(frame); err != nil {
		y.failed = true
	}
}

// snapshot 是事件里携带的 response 快照。
func (y *responsesSynth) snapshot(status string, output []any, withUsage bool) map[string]any {
	if output == nil {
		output = []any{}
	}
	resp := map[string]any{
		"id":         y.respID,
		"object":     "response",
		"created_at": y.created,
		"status":     status,
		"model":      y.model,
		"output":     output,
		"store":      false,
		"error":      nil,
	}
	if withUsage && y.usage != nil {
		resp["usage"] = y.usage
	}
	return resp
}

func (y *responsesSynth) start() {
	y.emit("response.created", map[string]any{"response": y.snapshot("in_progress", nil, false)})
	y.emit("response.in_progress", map[string]any{"response": y.snapshot("in_progress", nil, false)})
}

// feedChatChunk 消化一个 chat SSE chunk。
func (y *responsesSynth) feedChatChunk(m map[string]any) {
	if u, ok := m["usage"].(map[string]any); ok {
		y.usage = responsesUsageFromChat(u)
	}
	choices := jsonArray(m["choices"])
	if len(choices) == 0 {
		return
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	if f := jsonString(ch["finish_reason"]); f != "" {
		y.finishReason = f
	}
	if delta, ok := ch["delta"].(map[string]any); ok {
		y.feedDelta(delta)
	}
}

// feedChatObject 消化一个完整的非流式 chat 对象（客户端要流而上游整体作答的
// 兜底：message 的全文按同一条 item 管线一次性放流）。
func (y *responsesSynth) feedChatObject(m map[string]any) {
	if u, ok := m["usage"].(map[string]any); ok {
		y.usage = responsesUsageFromChat(u)
	}
	choices := jsonArray(m["choices"])
	if len(choices) == 0 {
		return
	}
	ch, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	if f := jsonString(ch["finish_reason"]); f != "" {
		y.finishReason = f
	}
	if msg, ok := ch["message"].(map[string]any); ok {
		y.feedDelta(msg) // message 与 delta 的产出字段同名，管线共用
	}
}

// feedDelta 消化一段产出增量：reasoning_content / content / tool_calls 三类，
// 类别切换即关旧 item 开新 item。
func (y *responsesSynth) feedDelta(delta map[string]any) {
	if rc := jsonString(delta["reasoning_content"]); rc != "" {
		y.ensureItem("reasoning", "", "", "")
		y.buf.WriteString(rc)
		y.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id": y.curItemID(), "output_index": y.itemIdx, "summary_index": 0, "delta": rc,
		})
	}
	if c := jsonString(delta["content"]); c != "" {
		y.ensureItem("message", "", "", "")
		y.buf.WriteString(c)
		y.emit("response.output_text.delta", map[string]any{
			"item_id": y.curItemID(), "output_index": y.itemIdx, "content_index": 0, "delta": c,
		})
	}
	for _, raw := range jsonArray(delta["tool_calls"]) {
		tc, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key := jsonScalarLiteral(tc["index"])
		name, args := "", ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, args = jsonString(fn["name"]), jsonString(fn["arguments"])
		}
		y.ensureItem("function_call", key, jsonString(tc["id"]), name)
		if args != "" {
			y.buf.WriteString(args)
			y.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": y.curItemID(), "output_index": y.itemIdx, "delta": args,
			})
		}
	}
}

// ensureItem 保证当前敞开的 item 是所需类别（function_call 还要同一条 chat
// index）；不是就关旧开新。fcID/fcName 只在开新 function_call 时使用（chat
// 流在首个增量里给 id 与 name，后续增量只带 arguments）。
func (y *responsesSynth) ensureItem(typ, fcKey, fcID, fcName string) {
	if y.itemType == typ && (typ != "function_call" || y.fcKey == fcKey) {
		// 迟到的 name/id（有的上游分帧给）：补记到收口用的字段上。
		if typ == "function_call" {
			if fcID != "" {
				y.fcCallID = fcID
			}
			if fcName != "" {
				y.fcName = fcName
			}
		}
		return
	}
	y.closeItem()
	y.itemType, y.itemIdx = typ, y.nextIdx
	y.nextIdx++
	y.buf.Reset()
	switch typ {
	case "message":
		item := map[string]any{
			"id": y.curItemID(), "type": "message", "status": "in_progress",
			"role": "assistant", "content": []any{},
		}
		y.emit("response.output_item.added", map[string]any{"output_index": y.itemIdx, "item": item})
		y.emit("response.content_part.added", map[string]any{
			"item_id": y.curItemID(), "output_index": y.itemIdx, "content_index": 0,
			"part": outputTextPart(""),
		})
	case "reasoning":
		item := map[string]any{"id": y.curItemID(), "type": "reasoning", "summary": []any{}}
		y.emit("response.output_item.added", map[string]any{"output_index": y.itemIdx, "item": item})
		y.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": y.curItemID(), "output_index": y.itemIdx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	case "function_call":
		y.fcKey, y.fcCallID, y.fcName = fcKey, fcID, fcName
		item := map[string]any{
			"id": y.curItemID(), "type": "function_call", "status": "in_progress",
			"call_id": y.fcCallID, "name": y.fcName, "arguments": "",
		}
		y.emit("response.output_item.added", map[string]any{"output_index": y.itemIdx, "item": item})
	}
}

// curItemID 当前敞开 item 的 id（类别 + respID + output_index，响应内唯一）。
// 逐 delta 调用多次：switch 而不是每次现造一张 map。
func (y *responsesSynth) curItemID() string {
	var kind string
	switch y.itemType {
	case "message":
		kind = "msg"
	case "reasoning":
		kind = "rs"
	case "function_call":
		kind = "fc"
	}
	return itemID(y.respID, kind, y.itemIdx)
}

// closeItem 收口当前敞开的 item：补发 *.done 事件并把完成态 item 计入 output。
func (y *responsesSynth) closeItem() {
	if y.itemType == "" {
		return
	}
	id, text := y.curItemID(), y.buf.String()
	switch y.itemType {
	case "message":
		y.emit("response.output_text.done", map[string]any{
			"item_id": id, "output_index": y.itemIdx, "content_index": 0, "text": text,
		})
		y.emit("response.content_part.done", map[string]any{
			"item_id": id, "output_index": y.itemIdx, "content_index": 0,
			"part": outputTextPart(text),
		})
		done := messageItem(id, text, "completed")
		y.emit("response.output_item.done", map[string]any{"output_index": y.itemIdx, "item": done})
		y.output = append(y.output, done)
	case "reasoning":
		y.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": id, "output_index": y.itemIdx, "summary_index": 0, "text": text,
		})
		y.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": id, "output_index": y.itemIdx, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": text},
		})
		done := reasoningItem(id, text)
		y.emit("response.output_item.done", map[string]any{"output_index": y.itemIdx, "item": done})
		y.output = append(y.output, done)
	case "function_call":
		y.emit("response.function_call_arguments.done", map[string]any{
			"item_id": id, "output_index": y.itemIdx, "arguments": text,
		})
		done := functionCallItem(id, y.fcCallID, y.fcName, text, "completed")
		y.emit("response.output_item.done", map[string]any{"output_index": y.itemIdx, "item": done})
		y.output = append(y.output, done)
	}
	y.itemType = ""
	y.buf.Reset()
}

// finish 收口整条流：关掉敞开的 item，发 response.completed（带 usage 与全部
// 完成态 item）。没有 [DONE]。
func (y *responsesSynth) finish() {
	y.closeItem()
	status, incomplete := responseStatus(y.finishReason)
	resp := y.snapshot(status, y.output, true)
	resp["incomplete_details"] = incomplete
	y.emit("response.completed", map[string]any{"response": resp})
}

// fail 尽力向已在流上的客户端宣告失败（response.failed 事件）后停笔。
func (y *responsesSynth) fail(code, message string) {
	y.closeItem()
	resp := y.snapshot("failed", y.output, true)
	resp["error"] = map[string]any{"code": code, "message": message}
	y.emit("response.failed", map[string]any{"response": resp})
}
