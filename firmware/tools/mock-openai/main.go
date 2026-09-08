// mock-openai 是本地 OpenAI/Anthropic 双协议 mock 上游：无真实上游 Key 时供
// gatewayd 全量走查（非流式/SSE/usage/tool 调用/未知字段/取消传播/
// count_tokens fallback）。仅开发与冒烟用，独立 main 包，不编入 llmgate 产品二进制。
//
// 行为约定（走查脚本与人工 curl 参照）：
//
//   - GET /v1/models：返回 mock-gpt-4o 与 mock-gpt-4o-mini
//   - POST /v1/chat/completions（openai 协议）：按请求 stream 返回 JSON 或
//     SSE（[DONE] 结尾）；流式块序 role → 内容（或 tool_calls）增量 →
//     finish → usage 终块（仅收到 stream_options.include_usage=true 时）
//   - POST /v1/messages（anthropic 协议）：非流式返 message 结构（usage 含
//     cache 字段样例）；流式按 anthropic 事件序 message_start → ping →
//     content_block_start/delta/stop → message_delta → message_stop
//   - POST /v1/messages/count_tokens：按请求体字节长度粗估
//     {"input_tokens":N}；请求带 "x_mock_count_404": true → 404
//     （走查网关 count_tokens 的本地粗估 fallback 路径）
//   - 两协议响应的 model 字段返回带版本后缀的「真实上游 ID」形态：请求
//     mock-claude-mini → 返 mock-claude-mini-260801（模拟 iteration-2 观测
//     的上游行为，供走查网关把响应 model 改写回逻辑模型名）
//   - 响应总带未知字段 x_mock_extra（messages 流式带在 message_start 事件
//     上）；非流式还带 x_mock_request_keys（收到的顶层请求字段名，验证网关
//     对未知请求字段的透传）
//   - 最后一条 user 消息含 "pause" → chat 返未知 finish_reason pause_turn，
//     messages 返非常规 stop_reason x_mock_pause
//   - 请求带 tools 字段 → chat 返 tool_calls 样例（finish_reason=tool_calls），
//     messages 返 tool_use content block（stop_reason=tool_use；流式为
//     input_json_delta 序列）
//   - 块/事件间延迟 --sse-interval（缺省 100ms）；请求体未知字段
//     x_mock_delay_ms 可按请求覆盖（本身也演示未知字段透传）
//   - 客户端断开时打日志，供走查网关的取消传播
//   - 不校验鉴权，但日志记录收到的 Authorization 前缀（截断）、x-api-key
//     有无与 anthropic-* 头，供走查鉴权头重写与业务头透传
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18080",
		"监听地址（与 configs/dev.example.yaml 的 mock base_url 对齐）")
	sseInterval := flag.Duration("sse-interval", 100*time.Millisecond, "SSE 块间延迟")
	flag.Parse()

	s := &mockServer{interval: *sseInterval}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("Unknown request URL: %s %s", r.Method, r.URL.Path),
			"type":    "invalid_request_error",
			"code":    "not_found",
		}})
	})

	log.Printf("mock-openai 监听 http://%s（Ctrl-C 退出）", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

var mockModels = []string{"mock-gpt-4o", "mock-gpt-4o-mini"}

type mockServer struct {
	interval time.Duration
	seq      atomic.Int64
}

func (s *mockServer) handleModels(w http.ResponseWriter, r *http.Request) {
	log.Printf("models: auth=%s", redactHeader(r.Header.Get("Authorization")))
	data := make([]map[string]any, 0, len(mockModels))
	for _, m := range mockModels {
		data = append(data, map[string]any{
			"id": m, "object": "model", "created": time.Now().Unix(), "owned_by": "mock",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *mockServer) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("chat: 读请求体失败（客户端断开？）: %v", err)
		return
	}
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "request body is not valid JSON",
			"type":    "invalid_request_error",
			"code":    "invalid_json",
		}})
		return
	}

	model, _ := req["model"].(string)
	stream, _ := req["stream"].(bool)
	includeUsage := false
	if so, ok := req["stream_options"].(map[string]any); ok {
		includeUsage, _ = so["include_usage"].(bool)
	}
	delay := s.interval
	if n, ok := req["x_mock_delay_ms"].(json.Number); ok {
		if ms, err := n.Int64(); err == nil && ms >= 0 {
			delay = time.Duration(ms) * time.Millisecond
		}
	}
	_, hasTools := req["tools"]
	finish := "stop"
	if strings.Contains(lastUserContent(req), "pause") {
		finish = "pause_turn" // 未知 finish_reason：走查网关不解析、原样透传
	}
	if hasTools {
		finish = "tool_calls"
	}

	log.Printf("chat: model=%s stream=%v include_usage=%v finish=%s delay=%s auth=%s anthropic=%v",
		model, stream, includeUsage, finish, delay,
		redactHeader(r.Header.Get("Authorization")), anthropicHeaders(r.Header))

	id := fmt.Sprintf("chatcmpl-mock-%d", s.seq.Add(1))
	if stream {
		s.streamChat(w, r, id, model, finish, includeUsage, hasTools, delay)
		return
	}

	// 非流式：延迟同样生效（走查网关取消传播时把 x_mock_delay_ms 调大）。
	if !sleepCtx(r.Context(), delay) {
		log.Printf("chat %s: 客户端断开（非流式延迟期间），中止响应", id)
		return
	}
	msg := map[string]any{"role": "assistant"}
	if hasTools {
		msg["content"] = nil
		msg["tool_calls"] = []any{toolCallSample()}
	} else {
		msg["content"] = "mock response from " + model
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   upstreamModelID(model),
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
		"usage": usageSample(),
		// 未知字段：走查网关对未知响应字段的透传。
		"x_mock_extra": "unknown-field-passthrough",
		// 收到的顶层请求字段名：走查网关对未知请求字段的透传。
		"x_mock_request_keys": topKeys(req),
	})
	log.Printf("chat %s: 非流式完成", id)
}

// streamChat 输出 SSE：role → 增量 ×3 → finish → （可选）usage 终块 → [DONE]。
func (s *mockServer) streamChat(w http.ResponseWriter, r *http.Request,
	id, model, finish string, includeUsage, hasTools bool, delay time.Duration) {

	fl, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	created := time.Now().Unix()
	respModel := upstreamModelID(model)
	sent := 0
	emit := func(payload map[string]any) bool {
		if !sleepCtx(r.Context(), delay) {
			log.Printf("chat %s: 客户端断开，中止 SSE（已发 %d 块）", id, sent)
			return false
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
		sent++
		return true
	}
	chunk := func(delta map[string]any, finishReason any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": respModel,
			"choices": []any{map[string]any{
				"index": 0, "delta": delta, "finish_reason": finishReason,
			}},
			"x_mock_extra": "unknown-field-passthrough",
		}
	}

	deltas := []map[string]any{{"role": "assistant"}}
	if hasTools {
		deltas = append(deltas,
			map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_mock_001", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": ""},
			}}},
			map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": `{"city":`},
			}}},
			map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": `"Singapore"}`},
			}}},
		)
	} else {
		deltas = append(deltas,
			map[string]any{"content": "mock "},
			map[string]any{"content": "stream "},
			map[string]any{"content": "from " + model},
		)
	}
	for _, d := range deltas {
		if !emit(chunk(d, nil)) {
			return
		}
	}
	if !emit(chunk(map[string]any{}, finish)) {
		return
	}
	// usage 终块：OpenAI 语义下仅当客户端要求 include_usage 时出现，
	// choices 为空数组。网关会在客户端未带时代为注入，故正常走查总能看到。
	if includeUsage {
		if !emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": respModel,
			"choices": []any{}, "usage": usageSample(),
			"x_mock_extra": "unknown-field-passthrough",
		}) {
			return
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
	log.Printf("chat %s: SSE 完成（%d 数据块 + [DONE]）", id, sent)
}

// handleMessages 处理 anthropic 协议 POST /v1/messages（非流式与 SSE）。
func (s *mockServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("messages: 读请求体失败（客户端断开？）: %v", err)
		return
	}
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest,
			anthropicError("invalid_request_error", "request body is not valid JSON"))
		return
	}

	model, _ := req["model"].(string)
	stream, _ := req["stream"].(bool)
	delay := s.interval
	if n, ok := req["x_mock_delay_ms"].(json.Number); ok {
		if ms, err := n.Int64(); err == nil && ms >= 0 {
			delay = time.Duration(ms) * time.Millisecond
		}
	}
	_, hasTools := req["tools"]
	stopReason := "end_turn"
	if strings.Contains(lastUserContent(req), "pause") {
		stopReason = "x_mock_pause" // 非常规 stop_reason：走查网关不解析、原样透传
	}
	if hasTools {
		stopReason = "tool_use"
	}

	log.Printf("messages: model=%s stream=%v stop_reason=%s delay=%s auth=%s x_api_key=%s anthropic=%v",
		model, stream, stopReason, delay,
		redactHeader(r.Header.Get("Authorization")),
		redactHeader(r.Header.Get("x-api-key")), // 网关应已删除：此处恒 (none) 即证明
		anthropicHeaders(r.Header))

	id := fmt.Sprintf("msg_mock_%d", s.seq.Add(1))
	if stream {
		s.streamMessages(w, r, id, model, stopReason, hasTools, delay)
		return
	}

	// 非流式：延迟同样生效（走查网关取消传播时把 x_mock_delay_ms 调大）。
	if !sleepCtx(r.Context(), delay) {
		log.Printf("messages %s: 客户端断开（非流式延迟期间），中止响应", id)
		return
	}
	var content []any
	if hasTools {
		content = []any{toolUseBlock()}
	} else {
		content = []any{map[string]any{"type": "text", "text": "mock response from " + model}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         upstreamModelID(model),
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         anthropicUsageSample(),
		// 未知字段：走查网关对未知响应字段的透传。
		"x_mock_extra": "unknown-field-passthrough",
		// 收到的顶层请求字段名：走查网关对未知请求字段的透传。
		"x_mock_request_keys": topKeys(req),
	})
	log.Printf("messages %s: 非流式完成", id)
}

// streamMessages 按 anthropic 事件序输出 SSE：message_start → ping →
// content_block_start → content_block_delta（text_delta 或 input_json_delta）
// ×N → content_block_stop → message_delta → message_stop。
func (s *mockServer) streamMessages(w http.ResponseWriter, r *http.Request,
	id, model, stopReason string, hasTools bool, delay time.Duration) {

	fl, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	sent := 0
	emit := func(event string, payload map[string]any) bool {
		if !sleepCtx(r.Context(), delay) {
			log.Printf("messages %s: 客户端断开，中止 SSE（已发 %d 事件）", id, sent)
			return false
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
		sent++
		return true
	}

	if !emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         upstreamModelID(model), // Phase 4 改写目标：message_start 的 message.model
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                17,
				"output_tokens":               1,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     3,
			},
		},
		// 未知字段挂在被改写的事件上：走查改写后未知字段仍保留。
		"x_mock_extra": "unknown-field-passthrough",
	}) {
		return
	}
	if !emit("ping", map[string]any{"type": "ping"}) {
		return
	}

	var startBlock map[string]any
	var deltas []map[string]any
	if hasTools {
		startBlock = map[string]any{
			"type": "tool_use", "id": "toolu_mock_001",
			"name": "get_weather", "input": map[string]any{},
		}
		deltas = []map[string]any{
			{"type": "input_json_delta", "partial_json": `{"city":`},
			{"type": "input_json_delta", "partial_json": `"Singapore"}`},
		}
	} else {
		startBlock = map[string]any{"type": "text", "text": ""}
		deltas = []map[string]any{
			{"type": "text_delta", "text": "mock "},
			{"type": "text_delta", "text": "stream "},
			{"type": "text_delta", "text": "from " + model},
		}
	}
	if !emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0, "content_block": startBlock,
	}) {
		return
	}
	for _, d := range deltas {
		if !emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0, "delta": d,
		}) {
			return
		}
	}
	if !emit("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": 0,
	}) {
		return
	}
	if !emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 25},
	}) {
		return
	}
	if !emit("message_stop", map[string]any{"type": "message_stop"}) {
		return
	}
	log.Printf("messages %s: SSE 完成（%d 事件到 message_stop）", id, sent)
}

// handleCountTokens 处理 POST /v1/messages/count_tokens：按请求体字节长度
// 粗估 input_tokens；请求带 "x_mock_count_404": true 时返回 404，供走查
// 网关的本地粗估 fallback 路径。
func (s *mockServer) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("count_tokens: 读请求体失败（客户端断开？）: %v", err)
		return
	}
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest,
			anthropicError("invalid_request_error", "request body is not valid JSON"))
		return
	}
	model, _ := req["model"].(string)
	if v, _ := req["x_mock_count_404"].(bool); v {
		log.Printf("count_tokens: model=%s x_mock_count_404 触发 404（走查网关 fallback）", model)
		writeJSON(w, http.StatusNotFound,
			anthropicError("not_found_error", "count_tokens disabled by x_mock_count_404"))
		return
	}
	n := (len(body) + 3) / 4 // 字节长度 /4 向上取整的粗估，只求确定性
	if n < 1 {
		n = 1
	}
	log.Printf("count_tokens: model=%s body_len=%d input_tokens=%d auth=%s",
		model, len(body), n, redactHeader(r.Header.Get("Authorization")))
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
}

// upstreamModelID 给请求的 model 加版本后缀，模拟真实上游在响应里返回带
// 版本的完整 ID（iteration-2 实测：方舟 plan 返带版本后缀 ID、DeepSeek 返
// 内部真实 ID）。网关把响应 model 改写回逻辑名的走查依赖此差异。
func upstreamModelID(model string) string {
	if model == "" {
		return ""
	}
	return model + "-260801"
}

// anthropicError 构造 Anthropic 风格错误体 {"type":"error","error":{...}}。
func anthropicError(errType, msg string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": msg},
	}
}

func toolUseBlock() map[string]any {
	return map[string]any{
		"type": "tool_use", "id": "toolu_mock_001",
		"name": "get_weather", "input": map[string]any{"city": "Singapore"},
	}
}

// anthropicUsageSample 含 cache 字段样例：anthropic 口径 input_tokens 不含
// cache 命中（iteration-2 实测），cache_read 给非零值供后续计量迭代走查。
func anthropicUsageSample() map[string]any {
	return map[string]any{
		"input_tokens":                17,
		"output_tokens":               25,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     3,
	}
}

func toolCallSample() map[string]any {
	return map[string]any{
		"id": "call_mock_001", "type": "function",
		"function": map[string]any{"name": "get_weather", "arguments": `{"city":"Singapore"}`},
	}
}

func usageSample() map[string]any {
	return map[string]any{"prompt_tokens": 17, "completion_tokens": 25, "total_tokens": 42}
}

// sleepCtx 睡 d；期间 ctx 取消（客户端断开）返回 false。d<=0 时只探测取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// lastUserContent 取最后一条 role=user 消息的文本内容：string 直接返回；
// content block 数组（anthropic 风格，openai 多段 content 同构）拼接各块的
// text 字段；其余形态按空处理。
func lastUserContent(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			return c
		case []any:
			var b strings.Builder
			for _, blk := range c {
				if bm, ok := blk.(map[string]any); ok {
					if t, ok := bm["text"].(string); ok {
						b.WriteString(t)
					}
				}
			}
			return b.String()
		}
		return ""
	}
	return ""
}

// topKeys 返回 m 的顶层字段名（字典序，输出稳定）。
func topKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// redactHeader 截断头值：只保留前缀与长度。mock 只该收到占位 Key，
// 但保持不落全量凭证的习惯（走查鉴权头重写时看前缀即可分辨）。
func redactHeader(v string) string {
	if v == "" {
		return "(none)"
	}
	const keep = 18
	if len(v) > keep {
		return fmt.Sprintf("%s…(len=%d)", v[:keep], len(v))
	}
	return fmt.Sprintf("%s(len=%d)", v, len(v))
}

// anthropicHeaders 收集 anthropic-* 请求头（走查业务头透传；值非敏感）。
func anthropicHeaders(h http.Header) []string {
	var out []string
	for k, vs := range h {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-") {
			for _, v := range vs {
				out = append(out, k+"="+v)
			}
		}
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
