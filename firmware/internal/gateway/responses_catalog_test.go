// responses_catalog_test.go 验证 POST /v1/responses（标准 Responses 目录面）
// 的对外行为：非流式与 SSE 的双向转换、事件序、无状态子集 400、选路 404、
// 故障切换、以及计量（entry=responses_catalog、OpenAI 形 usage、目录价随行）。
// 纯函数映射矩阵在 responses_catalog_internal_test.go。
package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

type fixedPlatformModels struct{ doc platformcatalog.Doc }

func (f fixedPlatformModels) EffectivePlatformModels(context.Context) platformcatalog.Doc {
	return f.doc
}

// respCatalogBody 是一个贴近 codex 形态的最小请求（instructions + 消息 +
// 函数工具 + 丢弃项），模型固定 newMeterEnv 建的 metered-model。
const respCatalogBody = `{
	"model": "metered-model",
	"store": false,
	"instructions": "You are helpful.",
	"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
	"tools": [{"type":"function","name":"shell","description":"run","parameters":{"type":"object"},"strict":false}],
	"include": ["reasoning.encrypted_content"],
	"prompt_cache_key": "abc",
	"max_output_tokens": 128
}`

// chatJSONReply 回放一个非流式 chat completion。
func chatJSONReply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	}
}

func TestResponsesCatalogNonStream(t *testing.T) {
	e, stub, fm := newMeterEnv(t, chatJSONReply(`{"id":"chatcmpl-9","model":"upstream-side-id-v2",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi there","reasoning_content":"think"},`+
		`"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":25}}}`))
	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, respCatalogBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	if resp["object"] != "response" || resp["status"] != "completed" || resp["model"] != "metered-model" {
		t.Fatalf("response 对象不对: %v", resp)
	}
	if resp["store"] != false {
		t.Fatalf("response 应恒带 store:false: %v", resp["store"])
	}
	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output 应为 reasoning+message: %v", output)
	}
	msgItem := output[1].(map[string]any)
	if msgItem["type"] != "message" ||
		msgItem["content"].([]any)[0].(map[string]any)["text"] != "hi there" {
		t.Fatalf("message item 不对: %v", msgItem)
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(100) || u["output_tokens"] != float64(7) || u["total_tokens"] != float64(107) {
		t.Fatalf("usage 映射不对: %v", u)
	}
	if d := u["input_tokens_details"].(map[string]any); d["cached_tokens"] != float64(25) {
		t.Fatalf("缓存命中未映射: %v", d)
	}

	// 上游侧：model 改写为来源侧 ID，转换后的 chat 形字段在场、丢弃项不在场。
	var up map[string]any
	if err := json.Unmarshal([]byte(stub.sentBody(0)), &up); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	if up["model"] != meterUpModel {
		t.Fatalf("上游 model = %v，期望来源侧 ID %s", up["model"], meterUpModel)
	}
	msgs := up["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "hello" {
		t.Fatalf("messages 转换不对: %v", msgs)
	}
	if up["max_tokens"] != float64(128) {
		t.Fatalf("max_output_tokens 未映射: %v", up["max_tokens"])
	}
	fn := up["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Fatalf("工具未转换成嵌套形: %v", up["tools"])
	}
	for _, banned := range []string{"instructions", "input", "include", "prompt_cache_key", "store", "max_output_tokens"} {
		if _, has := up[banned]; has {
			t.Fatalf("字段 %q 不该到达上游", banned)
		}
	}

	// 计量：entry 独立、token 走 OpenAI 口径、目录价与上游快照随行。
	s := fm.only(t)
	if s.Entry != usage.EntryResponses {
		t.Fatalf("entry = %q，期望 %q", s.Entry, usage.EntryResponses)
	}
	if s.Tokens.Prompt != 100 || s.Tokens.Completion != 7 || s.Tokens.CacheRead != 25 {
		t.Fatalf("tokens = %+v", s.Tokens)
	}
	if s.Estimated || !s.ModelKnown || s.UpstreamName != meterUpstream || s.Pricing != meterPricing {
		t.Fatalf("样本维度不对: %+v", s)
	}
}

func TestResponsesCatalogDeepSeekV4ReplaysThinkingAcrossToolCall(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatJSONReply(`{
		"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]
	}`))
	upstreamID := dbUpstream(t, e.st, "deepseek-test", config.UpstreamDeepseek, "sk-not-real", stub.url)
	modelID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, modelID, upstreamID, "deepseek-v4-flash", 100)

	body := `{
		"model":"deepseek-v4-flash","store":false,
		"input":[
			{"type":"message","role":"user","content":"inspect"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"Need host facts."}]},
			{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"uname -a\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"Linux test"}
		],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"tool_choice":"auto","reasoning":{"effort":"xhigh"}
	}`
	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(stub.sentBody(0)), &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	msgs := sent["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	if assistant["reasoning_content"] != "Need host facts." || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("DeepSeek assistant 工具轮次没有完整回放: %v", assistant)
	}
	if sent["reasoning_effort"] != "max" || sent["thinking"].(map[string]any)["type"] != "enabled" {
		t.Fatalf("DeepSeek thinking 参数不对: effort=%v thinking=%v", sent["reasoning_effort"], sent["thinking"])
	}
	if _, has := sent["tool_choice"]; has {
		t.Fatal("DeepSeek thinking 上游请求不应携带 tool_choice")
	}
}

func TestResponsesCatalogUsesCapabilitiesFromDataUpgrade(t *testing.T) {
	e := newRouteEnv(t)
	doc, err := platformcatalog.Parse([]byte(`{
		"schema":"llmgate.platform-models/v2","version":9999,"updated_at":"2026-08-24",
		"platforms":[{"id":"example_ai","type":"openai_compat","vendor":"Example AI",
			"base_url":"https://api.example.invalid/v1","billing_mode":"usage","models":[{
				"name":"example-reasoner","kind":"text","capabilities":{"responses_chat":{
					"profile":"reasoning_replay_v1","replay_reasoning_content":true,
					"thinking_type":"enabled","drop_tool_choice":true,"effort_map":{"xhigh":"turbo"}
				},"codex":{"default_reasoning_level":"xhigh","supported_reasoning_levels":[
					{"effort":"xhigh","description":"Maximum reasoning"}]}}
			}]}],"agents":[]}`))
	if err != nil {
		t.Fatalf("Parse dynamic platform: %v", err)
	}
	e.srv.SetPlatformModels(fixedPlatformModels{doc: doc})
	stub := newStub(t, chatJSONReply(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	up, err := e.st.CreateCatalogUpstream(t.Context(), "example", config.UpstreamOpenAICompat,
		"example_ai", "usage", "sk-not-real", stub.url)
	if err != nil {
		t.Fatalf("CreateCatalogUpstream: %v", err)
	}
	modelID := dbModel(t, e.st, "example-reasoner")
	dbSource(t, e.st, modelID, up.ID, "example-reasoner", 100)

	body := `{"model":"example-reasoner","store":false,"input":[
		{"type":"message","role":"user","content":"inspect"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"Need facts."}]},
		{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"}],
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],
		"tool_choice":"auto","reasoning":{"effort":"xhigh"}}`
	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(stub.sentBody(0)), &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v", err)
	}
	assistant := sent["messages"].([]any)[1].(map[string]any)
	if assistant["reasoning_content"] != "Need facts." || sent["reasoning_effort"] != "turbo" {
		t.Fatalf("升级数据中的模型能力没有生效: assistant=%v effort=%v", assistant, sent["reasoning_effort"])
	}
	if _, has := sent["tool_choice"]; has {
		t.Fatal("升级数据要求丢弃 tool_choice，但上游请求仍携带该字段")
	}
}

// synthEvent 是解析出的一个合成事件帧。
type synthEvent struct {
	event string
	data  map[string]any
}

// parseSynthEvents 把合成的 Responses SSE 拆成事件序列，并顺带断言帧内
// data.type 与 event: 行一致、sequence_number 单调递增。
func parseSynthEvents(t *testing.T, body string) []synthEvent {
	t.Helper()
	var events []synthEvent
	lastSeq := int64(-1)
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		lines := strings.SplitN(frame, "\n", 2)
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("事件帧形态不对: %q", frame)
		}
		ev := strings.TrimPrefix(lines[0], "event: ")
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
			t.Fatalf("事件载荷不是 JSON: %v\n%s", err, frame)
		}
		if data["type"] != ev {
			t.Fatalf("data.type=%v 与 event 行 %q 不一致", data["type"], ev)
		}
		seq := int64(data["sequence_number"].(float64))
		if seq != lastSeq+1 {
			t.Fatalf("sequence_number 不连续: %d 之后是 %d", lastSeq, seq)
		}
		lastSeq = seq
		events = append(events, synthEvent{event: ev, data: data})
	}
	return events
}

func eventTypes(events []synthEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.event
	}
	return out
}

func TestResponsesCatalogStreamSynthesis(t *testing.T) {
	e, _, fm := newMeterEnv(t, sseReply(
		"data: {\"id\":\"c\",\"model\":\"upstream-side-id-v2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"th\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"He\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n"+
			"data: [DONE]\n\n"))
	body := `{"model":"metered-model","stream":true,"store":false,"input":"hi"}`
	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	raw := w.Body.String()
	if strings.Contains(raw, "[DONE]") {
		t.Fatal("Responses SSE 不该有 [DONE] 终止符")
	}
	events := parseSynthEvents(t, raw)
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	got := eventTypes(events)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("事件序不对:\n got %v\nwant %v", got, want)
	}
	// created 与 completed 的 response.model 都是客户端请求名。
	first := events[0].data["response"].(map[string]any)
	if first["model"] != "metered-model" {
		t.Fatalf("response.created 的 model = %v", first["model"])
	}
	final := events[len(events)-1].data["response"].(map[string]any)
	if final["status"] != "completed" || final["model"] != "metered-model" {
		t.Fatalf("response.completed 不对: %v", final)
	}
	output := final["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed.output 应为 reasoning+message: %v", output)
	}
	if txt := output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; txt != "Hello" {
		t.Fatalf("正文全文 = %v", txt)
	}
	u := final["usage"].(map[string]any)
	if u["input_tokens"] != float64(10) || u["output_tokens"] != float64(2) {
		t.Fatalf("completed.usage 不对: %v", u)
	}
	s := fm.only(t)
	if s.Entry != usage.EntryResponses || s.Tokens.Prompt != 10 || s.Tokens.Completion != 2 || s.Estimated {
		t.Fatalf("流式计量不对: %+v", s)
	}
}

func TestResponsesCatalogStreamToolCalls(t *testing.T) {
	e, _, _ := newMeterEnv(t, sseReply(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
			"data: [DONE]\n\n"))
	body := `{"model":"metered-model","stream":true,"store":false,"input":"run ls",
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`
	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	events := parseSynthEvents(t, w.Body.String())
	var added, argDeltas, done []synthEvent
	for _, ev := range events {
		switch ev.event {
		case "response.output_item.added":
			added = append(added, ev)
		case "response.function_call_arguments.delta":
			argDeltas = append(argDeltas, ev)
		case "response.output_item.done":
			done = append(done, ev)
		}
	}
	if len(added) != 1 || len(done) != 1 || len(argDeltas) != 2 {
		t.Fatalf("函数调用事件计数不对: added=%d deltas=%d done=%d", len(added), len(argDeltas), len(done))
	}
	item := added[0].data["item"].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_9" || item["name"] != "shell" {
		t.Fatalf("output_item.added 不对: %v", item)
	}
	final := done[0].data["item"].(map[string]any)
	if final["arguments"] != `{"cmd":"ls"}` || final["status"] != "completed" {
		t.Fatalf("output_item.done 不对: %v", final)
	}
}

func TestResponsesCatalogContractErrors(t *testing.T) {
	e, stub, _ := newMeterEnv(t, chatJSONReply(`{}`))
	cases := []struct {
		name, body, code string
	}{
		{"store真", `{"model":"metered-model","store":true,"input":"hi"}`, "responses_stateful_unsupported"},
		{"续聊", `{"model":"metered-model","previous_response_id":"resp_1","input":"hi"}`, "responses_stateful_unsupported"},
		{"强制内置工具", `{"model":"metered-model","input":"hi","tool_choice":{"type":"web_search"}}`, "responses_tool_unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if _, code, _ := decodeError(t, w); code != tc.code {
				t.Fatalf("code = %q，期望 %q", code, tc.code)
			}
		})
	}
	if stub.count() != 0 {
		t.Fatalf("契约拒绝不该触达上游（count=%d）", stub.count())
	}
	// 未知模型：与 chat 同口径 404。
	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, `{"model":"no-such","store":false,"input":"hi"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("未知模型 status = %d", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "model_not_found" {
		t.Fatalf("code = %q", code)
	}
}

func TestResponsesCatalogFailover(t *testing.T) {
	e := newRouteEnv(t)
	good := newStub(t, chatJSONReply(
		`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	// 更高优先级（数字更小）的来源指向恒 500 的上游：未提交即切换。
	bad := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	})
	modelID := dbModel(t, e.st, meterModel)
	dbSource(t, e.st, modelID, dbUpstream(t, e.st, "meter-bad", config.UpstreamMock, "sk-bad-not-real", bad.url), "bad-side-id", 50)
	dbSource(t, e.st, modelID, dbUpstream(t, e.st, meterUpstream, config.UpstreamMock, "sk-meter-not-real", good.url), meterUpModel, 100)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	stub := good

	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth,
		`{"model":"metered-model","store":false,"input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if bad.count() != 1 || stub.count() != 1 {
		t.Fatalf("切换计数不对: bad=%d good=%d", bad.count(), stub.count())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "completed" || resp["model"] != "metered-model" {
		t.Fatalf("切换后的响应不对: %v", resp)
	}
	s := fm.only(t)
	if s.Attempts != 2 || s.UpstreamName != meterUpstream {
		t.Fatalf("样本应记 2 次尝试与最终来源: %+v", s)
	}
}

// TestResponsesCatalogStreamAggregatesForJSONClient 覆盖错位形态：客户端不要
// 流（stream 缺省）而上游只会答 SSE——网关聚合成单个 response 对象。
func TestResponsesCatalogStreamAggregatesForJSONClient(t *testing.T) {
	e, _, _ := newMeterEnv(t, sseReply(
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n"+
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n"+
			"data: [DONE]\n\n"))
	w := do(e.h, http.MethodPost, "/v1/responses", chatAuth,
		`{"model":"metered-model","store":false,"input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	txt := resp["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if txt != "Hello" {
		t.Fatalf("聚合正文 = %v", txt)
	}
	if u := resp["usage"].(map[string]any); u["input_tokens"] != float64(4) {
		t.Fatalf("聚合 usage 不对: %v", u)
	}
}
