// metering_test.go 是 iteration-9 Phase 3 的验收：数据面每一次消费都要在请求
// 收尾时变成一条 usage.Sample，而且 token 口径要对得上两家上游的方言。
//
// 覆盖面（计划逐条）：chat 非流式（标准 usage / DeepSeek prompt_cache_hit_tokens
// 方言 / 上游不报 usage 时的估算）、chat SSE（usage chunk / 显式
// include_usage:false 的估算打标 / 断流部分估算）、messages 非流式、messages SSE
// （message_start + message_delta 的逐字段非零覆盖合并）、最终状态 ≥400 计 0
// token、count_tokens 只计一次请求不计 token、观察器解析失败不断流不改流、
// 流式出图的厂商 usage（image_generation.* 事件），以及"金额与手算一致"的
// 真 Meter 端到端。
package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// ---- 假计量出口 ----

// provEntry 是一次临时金额调整（长流中间结算与收尾冲正）。
type provEntry struct{ keyID, delta int64 }

// fakeMeter 实现 gateway.Metering：把每一笔 Sample 与临时金额原样记下来。
// deny 非 nil 时准入一律按它拒绝（Phase 4 的 429 形态用例）。
type fakeMeter struct {
	mu      sync.Mutex
	samples []usage.Sample
	prov    []provEntry
	settled []string
	deny    *usage.Decision
}

func (f *fakeMeter) Record(s usage.Sample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, s)
}

func (f *fakeMeter) Admit(store.KeyAuth) usage.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deny != nil {
		return *f.deny
	}
	return usage.Decision{Allowed: true}
}

func (f *fakeMeter) AddProvisional(keyID, deltaMicro int64) usage.ProvisionalWindow {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prov = append(f.prov, provEntry{keyID, deltaMicro})
	return usage.ProvisionalWindow{Day: "2026-08-08", Week: "2026-08-03", Month: "2026-08"}
}

func (f *fakeMeter) ReverseProvisional(keyID, dayMicro, _, _ int64, _ usage.ProvisionalWindow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 冲正在这一层只按日金额记一条负增量：既有用例断言的是"原额冲平"，
	// 而日/周/月三个金额在同窗口内恒相等（分窗口的口径由包内用例直测）。
	f.prov = append(f.prov, provEntry{keyID, -dayMicro})
}

func (f *fakeMeter) SettleTask(_ context.Context, task store.AIGCTask) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = append(f.settled, task.ID)
}

// only 断言恰好记了一笔并返回它。
func (f *fakeMeter) only(t *testing.T) usage.Sample {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.samples) != 1 {
		t.Fatalf("记账笔数 = %d，期望恰好 1（%+v）", len(f.samples), f.samples)
	}
	return f.samples[0]
}

func (f *fakeMeter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.samples)
}

// ---- 用例环境 ----

const (
	meterModel    = "metered-model"
	meterUpModel  = "upstream-side-id"
	meterUpstream = "meter-up"
	// meterPricing 是一张文本三价目录价（微元 / 百万 token）：
	// 输入 3 元、输出 12 元、缓存命中 0.3 元每百万 token。
	meterPricing = `{"in":3000000,"out":12000000,"cache_read":300000}`
)

// newMeterEnv 装配「一个文本模型 + 一个假上游 + 一个假计量出口」。
func newMeterEnv(t *testing.T, respond http.HandlerFunc) (*routeEnv, *stubUpstream, *fakeMeter) {
	t.Helper()
	e := newRouteEnv(t)
	stub := newStub(t, respond)
	up := dbUpstream(t, e.st, meterUpstream, config.UpstreamMock, "sk-meter-not-real", stub.url)
	modelID := dbModel(t, e.st, meterModel)
	if err := e.st.SetModelPricing(t.Context(), modelID, meterPricing); err != nil {
		t.Fatalf("SetModelPricing: %v", err)
	}
	dbSource(t, e.st, modelID, up, meterUpModel, 100)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	return e, stub, fm
}

// sseReply 回放一段固定 SSE。
func sseReply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, body)
	}
}

// wantEstimateInput 按网关看到的同一张请求 map 算输入侧估算——用例不重抄公式，
// 只钉"网关用的就是 usage 包那一个函数"。
func wantEstimateInput(t *testing.T, body string) int64 {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("用例请求体不是 JSON: %v", err)
	}
	return usage.EstimateInputTokens(m)
}

// assertDims 断言一条样本的归属与维度（除 token 与状态外的一切）。
func assertDims(t *testing.T, s usage.Sample, entry, model string) {
	t.Helper()
	if s.Entry != entry {
		t.Errorf("entry = %q，期望 %q", s.Entry, entry)
	}
	if s.Kind != store.ModelKindText {
		t.Errorf("kind = %q，期望 %q", s.Kind, store.ModelKindText)
	}
	if s.ModelName != model {
		t.Errorf("model = %q，期望 %q", s.ModelName, model)
	}
	if s.KeyID <= 0 {
		t.Errorf("归属未记全: keyID=%d", s.KeyID)
	}
	if s.KeyDisplay == "" {
		t.Error("keyDisplay 为空：密钥维度快照没跟上（密钥删后历史账就读不出是谁花的）")
	}
	if s.Pricing != meterPricing {
		t.Errorf("pricing = %q，期望记账时点价原文 %q", s.Pricing, meterPricing)
	}
	if s.Rejected {
		t.Error("未过准入的请求不该带 rejected 标")
	}
}

// ---- chat 非流式 ----

func TestMeterChatNonStream(t *testing.T) {
	cases := []struct {
		name  string
		usage string
		want  usage.Tokens
	}{
		{
			// OpenAI 标准：缓存命中挂在 prompt_tokens_details.cached_tokens，
			// 且 prompt_tokens 已含它。
			name:  "标准usage",
			usage: `{"prompt_tokens":100,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":20},"total_tokens":150}`,
			want:  usage.Tokens{Prompt: 100, Completion: 50, CacheRead: 20},
		},
		{
			// DeepSeek 方言：命中/未命中平铺在 usage 顶层。
			name:  "DeepSeek方言",
			usage: `{"prompt_tokens":100,"completion_tokens":50,"prompt_cache_hit_tokens":30,"prompt_cache_miss_tokens":70}`,
			want:  usage.Tokens{Prompt: 100, Completion: 50, CacheRead: 30},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,`+
				`"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],"usage":%s}`,
				meterUpModel, c.usage)
			e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, body))

			w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
				fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
			}
			s := fm.only(t)
			assertDims(t, s, usage.EntryChat, meterModel)
			if s.Tokens != c.want {
				t.Errorf("tokens = %+v，期望 %+v", s.Tokens, c.want)
			}
			if s.Estimated {
				t.Error("上游报了 usage，不该打估算标")
			}
			if s.Status != http.StatusOK || s.Attempts != 1 || s.UpstreamName != meterUpstream ||
				s.UpstreamType != config.UpstreamMock {
				t.Errorf("请求事实未记全: %+v", s)
			}
			if s.DurationMs < 0 {
				t.Errorf("duration_ms = %d", s.DurationMs)
			}
		})
	}
}

// TestMeterChatNonStreamEstimated：上游一个 usage 字段都不给时按估算记账并打标
// （公式取 usage 包，用例不另抄一份）。
func TestMeterChatNonStreamEstimated(t *testing.T) {
	const content = "你好，这是一段中英混排的回答 with some ASCII words."
	body := fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,`+
		`"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, meterUpModel, content)
	e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, body))

	reqBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"请写一段话"}]}`, meterModel)
	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, reqBody); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	s := fm.only(t)
	if !s.Estimated {
		t.Fatal("上游没报 usage，应按估算记账并打标")
	}
	if want := wantEstimateInput(t, reqBody); s.Tokens.Prompt != want {
		t.Errorf("估算输入 = %d，期望 %d", s.Tokens.Prompt, want)
	}
	if want := usage.EstimateTokens(content); s.Tokens.Completion != want {
		t.Errorf("估算输出 = %d，期望 %d", s.Tokens.Completion, want)
	}
	if s.Tokens.CacheRead != 0 || s.Tokens.CacheWrite != 0 {
		t.Errorf("估算不该凭空造出 cache 分量: %+v", s.Tokens)
	}
}

// ---- chat 流式 ----

func TestMeterChatSSEUsageChunk(t *testing.T) {
	sse := "data: " + fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{"content":"你好"}}]}`, meterUpModel) + "\n\n" +
		"data: " + fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":%q,`+
		`"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`, meterUpModel) + "\n\n" +
		"data: [DONE]\n\n"
	e, _, fm := newMeterEnv(t, sseReply(sse))

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Errorf("流未完整透传: %s", w.Body.String())
	}
	s := fm.only(t)
	assertDims(t, s, usage.EntryChat, meterModel)
	if (s.Tokens != usage.Tokens{Prompt: 11, Completion: 7}) {
		t.Errorf("tokens = %+v，期望取末帧 usage {11,7}", s.Tokens)
	}
	if s.Estimated {
		t.Error("末帧带了 usage，不该打估算标")
	}
}

// TestMeterChatSSEIncludeUsageFalse：客户端显式关掉 usage（字节保真契约禁止
// 我们改它）——上游因此不报 usage，网关按估算记账并打标，绝不让这类请求隐身。
func TestMeterChatSSEIncludeUsageFalse(t *testing.T) {
	sse := "data: " + fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,"delta":{"content":"你好"}}]}`, meterUpModel) + "\n\n" +
		"data: " + fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,"delta":{"content":"世界"},"finish_reason":"stop"}]}`, meterUpModel) + "\n\n" +
		"data: [DONE]\n\n"
	e, stub, fm := newMeterEnv(t, sseReply(sse))

	reqBody := fmt.Sprintf(`{"model":%q,"stream":true,"stream_options":{"include_usage":false},`+
		`"messages":[{"role":"user","content":"hi"}]}`, meterModel)
	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, reqBody); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	// 客户端的显式 false 原样到达上游（注入逻辑不覆盖显式值）。
	if got := stub.sentBody(0); !strings.Contains(got, `"include_usage":false`) {
		t.Errorf("客户端显式 include_usage:false 应原样透传: %s", got)
	}
	s := fm.only(t)
	if !s.Estimated {
		t.Fatal("没有 usage 可用，应按估算记账并打标")
	}
	if want := usage.EstimateTokens("你好世界"); s.Tokens.Completion != want {
		t.Errorf("估算输出 = %d，期望按增量合计 %d", s.Tokens.Completion, want)
	}
	if want := wantEstimateInput(t, reqBody); s.Tokens.Prompt != want {
		t.Errorf("估算输入 = %d，期望 %d", s.Tokens.Prompt, want)
	}
}

// TestMeterChatSSEBroken：流中途断——usage 帧永远不会来了，已到达的增量照样
// 按估算入账（断流的那部分算力真的花掉了）。
func TestMeterChatSSEBroken(t *testing.T) {
	e, _, fm := newMeterEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: "+fmt.Sprintf(
			`{"id":"c1","model":%q,"choices":[{"index":0,"delta":{"content":"半截"}}]}`, meterUpModel)+"\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // 连接骤断：客户端已收到部分事件
	})

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（响应已提交）", w.Code)
	}
	s := fm.only(t)
	if !s.Estimated {
		t.Fatal("断流应按估算记账并打标")
	}
	if want := usage.EstimateTokens("半截"); s.Tokens.Completion != want {
		t.Errorf("估算输出 = %d，期望已到达增量的 %d", s.Tokens.Completion, want)
	}
}

// TestMeterSSEObserverParseFailure：观察器解析失败不断流也不改流——非 JSON
// 的 data 行、注释行、[DONE] 一概原样到客户端，账照记。
func TestMeterSSEObserverParseFailure(t *testing.T) {
	sse := ": keep-alive comment\n\n" +
		"data: not json at all\n\n" +
		"data: [DONE]\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"data: " + fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,"delta":{"content":"ok"}}],`+
		`"usage":{"prompt_tokens":5,"completion_tokens":2}}`, meterUpModel) + "\n\n"
	e, _, fm := newMeterEnv(t, sseReply(sse))

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	// 解析不出对象的行（注释、非 JSON 的 data、[DONE]）与没有 model 字段的
	// 行（ping）一律**逐字节**原样到客户端；只有末帧因 model 回写被重序列化。
	got := w.Body.String()
	const untouched = ": keep-alive comment\n\ndata: not json at all\n\ndata: [DONE]\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n"
	if !strings.HasPrefix(got, untouched) {
		t.Errorf("坏行/无需改写的行被动过:\ngot:  %q\nwant prefix: %q", got, untouched)
	}
	if !strings.Contains(got, `"model":"`+meterModel+`"`) || strings.Contains(got, meterUpModel) {
		t.Errorf("末帧 model 未回写: %q", got)
	}
	if !strings.Contains(got, `"prompt_tokens":5`) || !strings.Contains(got, `"completion_tokens":2`) {
		t.Errorf("末帧 usage 未原样透出: %q", got)
	}
	if s := fm.only(t); (s.Tokens != usage.Tokens{Prompt: 5, Completion: 2}) {
		t.Errorf("tokens = %+v，期望 {5,2}（坏行不影响好行）", s.Tokens)
	}
}

// ---- messages ----

func TestMeterMessagesNonStream(t *testing.T) {
	body := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":%q,`+
		`"content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn",`+
		`"usage":{"input_tokens":80,"output_tokens":40,"cache_read_input_tokens":10,`+
		`"cache_creation_input_tokens":5}}`, meterUpModel)
	e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, body))

	w := do(e.h, "POST", "/v1/messages", messagesAuth,
		fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	s := fm.only(t)
	assertDims(t, s, usage.EntryMessages, meterModel)
	want := usage.Tokens{Prompt: 80, Completion: 40, CacheRead: 10, CacheWrite: 5}
	if s.Tokens != want {
		t.Errorf("tokens = %+v，期望 %+v（anthropic 口径四分量各就各位）", s.Tokens, want)
	}
	if s.Estimated {
		t.Error("上游报了 usage，不该打估算标")
	}
}

// TestMeterMessagesSSEMerge：usage 分两帧到达——message_start 给输入与 cache
// （那时 output_tokens 恒为 1），message_delta 只给最终 output。逐字段非零覆盖
// 合并因此是必须的：整体覆盖会把输入抹成 0，不覆盖则 output 永远停在 1。
func TestMeterMessagesSSEMerge(t *testing.T) {
	sse := "event: message_start\ndata: " + fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_1","type":"message","model":%q,`+
			`"usage":{"input_tokens":80,"cache_read_input_tokens":10,`+
			`"cache_creation_input_tokens":5,"output_tokens":1}}}`, meterUpModel) + "\n\n" +
		"event: content_block_delta\ndata: " +
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}` + "\n\n" +
		"event: message_delta\ndata: " +
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":40}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	e, _, fm := newMeterEnv(t, sseReply(sse))

	w := do(e.h, "POST", "/v1/messages", messagesAuth,
		fmt.Sprintf(`{"model":%q,"max_tokens":16,"stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`, meterModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	s := fm.only(t)
	want := usage.Tokens{Prompt: 80, Completion: 40, CacheRead: 10, CacheWrite: 5}
	if s.Tokens != want {
		t.Errorf("tokens = %+v，期望 %+v", s.Tokens, want)
	}
	if s.Estimated {
		t.Error("两帧合起来给全了 usage，不该打估算标")
	}
}

// ---- 失败与不计量的路径 ----

// TestMeterErrorStatusZeroTokens：客户端看到的最终状态 ≥400 一律记 0 token
// （错误体里即便带了 usage 也不作数），但请求本身照记一笔——用量页要看得见
// 「这个模型今天错了多少次」。
func TestMeterErrorStatusZeroTokens(t *testing.T) {
	e, _, fm := newMeterEnv(t, jsonReply(http.StatusBadRequest,
		`{"error":{"message":"bad","type":"invalid_request_error"},"usage":{"prompt_tokens":99}}`))

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400 原样透传", w.Code)
	}
	s := fm.only(t)
	if (s.Tokens != usage.Tokens{}) {
		t.Errorf("tokens = %+v，期望全 0", s.Tokens)
	}
	if s.Estimated {
		t.Error("失败的请求不该打估算标（它没有产出可估）")
	}
	if s.Status != http.StatusBadRequest {
		t.Errorf("status = %d，期望 400", s.Status)
	}
}

// TestMeterModelNotFound：选路失败的请求也入账（模型名是客户端指定的，那一笔
// 404 该出现在这个模型的错误数里），上游维度留空。
func TestMeterModelNotFound(t *testing.T) {
	e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, `{}`))

	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		`{"model":"no-such-model","messages":[]}`); w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}
	s := fm.only(t)
	if s.ModelName != "no-such-model" || s.Entry != usage.EntryChat || s.Status != http.StatusNotFound {
		t.Errorf("样本 = %+v，期望 chat 入口的 404", s)
	}
	if s.UpstreamName != "" || s.Attempts != 0 || s.Pricing != "" {
		t.Errorf("没走到上游的请求不该有上游维度与价格: %+v", s)
	}
}

// TestMeterCountTokensRequestOnly：count_tokens 只计一次请求，不计 token
// ——它不是生成调用，估算兜底也不能挂到它头上。
func TestMeterCountTokensRequestOnly(t *testing.T) {
	e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, `{"input_tokens":123}`))

	w := do(e.h, "POST", "/v1/messages/count_tokens", messagesAuth,
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"一段挺长的中文提示词"}]}`, meterModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	s := fm.only(t)
	if s.Entry != usage.EntryMessages {
		t.Errorf("entry = %q，期望 %q", s.Entry, usage.EntryMessages)
	}
	if (s.Tokens != usage.Tokens{}) || s.Estimated {
		t.Errorf("count_tokens 只计请求：tokens = %+v estimated = %v", s.Tokens, s.Estimated)
	}
}

// TestMeterNonConsumingPathsSkipped：/v1/models 与 /healthz 不是消费，
// 不进账本（账本记的是花钱，不是流量）。
func TestMeterNonConsumingPathsSkipped(t *testing.T) {
	e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, `{}`))

	if w := do(e.h, "GET", "/v1/models", chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("/v1/models 状态码 = %d", w.Code)
	}
	if w := do(e.h, "GET", "/healthz", nil, ""); w.Code != http.StatusOK {
		t.Fatalf("/healthz 状态码 = %d", w.Code)
	}
	if n := fm.count(); n != 0 {
		t.Errorf("记账笔数 = %d，期望 0", n)
	}
}

// TestMeterUnauthorizedSkipped：连认证都没过的请求不入账（没有归属可记）。
func TestMeterUnauthorizedSkipped(t *testing.T) {
	e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, `{}`))
	if w := do(e.h, "POST", "/v1/chat/completions",
		map[string]string{"Content-Type": "application/json"},
		fmt.Sprintf(`{"model":%q,"messages":[]}`, meterModel)); w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", w.Code)
	}
	if n := fm.count(); n != 0 {
		t.Errorf("记账笔数 = %d，期望 0", n)
	}
}

// ---- 视频提交（异步任务面的第一半账） ----

// TestMeterVideoSubmitCountsRequestOnly：提交只计一次请求、金额 0——视频的钱
// 要等厂商 usage 出来才知道，由清算补记。settle.go 的「提交那一刻访问日志已经
// 记过一笔了、清算增量因此不再计 requests」就靠这一笔成立。
func TestMeterVideoSubmitCountsRequestOnly(t *testing.T) {
	e := newVideoEnv(t)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"一只猫"}]}`, videoModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	s := fm.only(t)
	if s.Entry != usage.EntryVideo || s.Kind != store.ModelKindVideo || s.ModelName != videoModel {
		t.Errorf("样本维度 = %+v，期望 video 入口的 %q", s, videoModel)
	}
	if (s.Tokens != usage.Tokens{}) || (s.TaskUsage != usage.TaskUsage{}) || s.Estimated {
		t.Errorf("提交这一笔不该有用量也不该打估算标: %+v", s)
	}
	if s.UpstreamName == "" || s.Attempts != 1 {
		t.Errorf("上游维度未记: upstream=%q attempts=%d", s.UpstreamName, s.Attempts)
	}
}

// TestMeterVideoQueryNotBilled：任务查询不是新消费，不入账（查已经花掉的钱
// 不该再记一笔）。
func TestMeterVideoQueryNotBilled(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)

	// 计量出口在提交之后才接上：这样环里只可能有查询那一笔（如果有的话）。
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	if w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if n := fm.count(); n != 0 {
		t.Errorf("任务查询记账笔数 = %d，期望 0", n)
	}
}

// TestMeterVideoSettledOnQuery（Phase 4）：客户端查询到任务终态时**就地清算**。
// 不这么做，这笔账要等懒对账那一轮才进账本（最长 15 分钟 = 10 分钟陈旧阈值 +
// 5 分钟轮次），而客户端刚刚才亲口告诉我们它完成了。
//
// 同时钉住一次性语义：再查一次不会重复入账（条件更新 cost_micro IS NULL）。
func TestMeterVideoSettledOnQuery(t *testing.T) {
	e := newVideoEnv(t)
	// H3 768P 档 0.5 元/秒（submitVideo 的请求 resolution=768P）。
	setModelPricing(t, e.routeEnv, videoModel, `{"minimax_video_sec_768p":500000}`)
	m := usage.NewMeter(e.st, 31, time.UTC, nil, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)

	id := submitVideo(t, e)
	uid := testKeyID(t, e.st)
	task, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), id, uid)
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if task.CostMicro != nil {
		t.Fatalf("刚提交的任务不该已清算: %v", *task.CostMicro)
	}

	e.mm.setQuery(http.StatusOK, fmt.Sprintf(
		`{"task":{"id":%q,"status":"succeeded","content":{"url":"https://cdn.example.net/clip.mp4"},"usage":{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}}}`,
		id))
	if w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d；body: %s", w.Code, w.Body.String())
	}

	// 5 输出秒 × 0.5 元/秒 = 2_500_000 微元。
	const want = 2_500_000
	task, err = e.st.GetAIGCTaskByVendorIDForKey(t.Context(), id, uid)
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if task.CostMicro == nil {
		t.Fatal("查询到终态后任务行应已清算（cost_micro 非 NULL）")
	}
	if *task.CostMicro != want {
		t.Errorf("清算金额 = %d 微元，期望 %d", *task.CostMicro, want)
	}
	if task.Estimated {
		t.Error("厂商 usage 拿到了，不该打估算标")
	}
	// 金额同时进了预算计数器（清算样本走 Meter.observe）。
	if sp := m.Spend(testKeyID(t, e.st)); sp.DayMicro != want {
		t.Errorf("预算计数器 = %+v，期望 %d 微元", sp, want)
	}

	// 再查一次：纯转发下终态行照样回查厂商（客户端要拿重签的产物 URL），
	// 但清算是一次性的（条件更新 cost_micro IS NULL），绝不重复入账。
	if w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("二次查询状态码 = %d", w.Code)
	}
	if sp := m.Spend(testKeyID(t, e.st)); sp.DayMicro != want {
		t.Errorf("二次查询后计数器 = %+v，期望仍是 %d（一次性清算）", sp, want)
	}
}

// TestMeterModelDimensionIsClipped（Phase 4 外审遗留）：模型名是客户端给的
// 字符串，而它随后成为内存 delta 表的键与一行 usage_hourly 的维度列。选不到路
// 的请求照样入账（那笔 404 该算在这个模型头上），所以一个随手写的超长名字会
// 原样落到 SD 卡上——入账前必须截断。目录里的真名恒 ≤128 字符，截断对能选到
// 路的请求是空操作。
func TestMeterModelDimensionIsClipped(t *testing.T) {
	e, _, fm := newMeterEnv(t, chatReply(meterUpModel))
	long := strings.Repeat("模", 5000) // 非 ASCII：按 rune 截断才对

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, long))
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（模型不存在）", w.Code)
	}
	s := fm.only(t)
	if n := len([]rune(s.ModelName)); n != 128 {
		t.Errorf("入账的模型维度长度 = %d rune，期望截断到 128", n)
	}
	if !strings.HasPrefix(long, s.ModelName) {
		t.Error("截断应保留前缀（管理员据此认出是谁在乱调）")
	}
}

// setModelPricing 按模型名给目录价（管理面等价操作）。
func setModelPricing(t *testing.T, e *routeEnv, name, pricing string) {
	t.Helper()
	models, err := e.st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	for _, m := range models {
		if m.Name == name {
			if err := e.st.SetModelPricing(t.Context(), m.ID, pricing); err != nil {
				t.Fatalf("SetModelPricing: %v", err)
			}
			return
		}
	}
	t.Fatalf("模型 %s 不在目录里", name)
}

// ---- 真 Meter 端到端：金额与手算一致 ----

// TestMeterMoneyEndToEnd 用真的 usage.Meter 跑一遍：管理员录了目录价之后，
// 一次调用的金额要与「厂商 usage × 单价」手算一致，并即时进预算计数器。
func TestMeterMoneyEndToEnd(t *testing.T) {
	body := fmt.Sprintf(`{"id":"c1","model":%q,"choices":[{"index":0,`+
		`"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":100,"completion_tokens":50,`+
		`"prompt_tokens_details":{"cached_tokens":20}}}`, meterUpModel)
	e, _, _ := newMeterEnv(t, jsonReply(http.StatusOK, body))
	m := usage.NewMeter(e.st, 31, time.UTC, nil, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)


	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel)); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}

	// 手算（chat 口径：prompt 已含 cache，先剥出来）：
	//   (100−20) × 3 元/M = 240 微元
	//        20  × 0.3 元/M = 6 微元
	//        50  × 12 元/M = 600 微元   合计 846 微元
	const want = 846
	sp := m.Spend(testKeyID(t, e.st))
	if sp.DayMicro != want {
		t.Errorf("预算计数器 = %+v，期望日额 %d 微元", sp, want)
	}
	if sp.MonthMicro != want {
		t.Errorf("月额 = %+v，期望 %d 微元", sp, want)
	}
}

// TestMeterUnpricedModelStillForwards：未定价的模型照常转发、金额记 0、
// 用量照记（「未定价」警示徽章是管理台的事，数据面一个字节都不该少）。
func TestMeterUnpricedModelStillForwards(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, jsonReply(http.StatusOK, fmt.Sprintf(
		`{"id":"c1","model":%q,"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, meterUpModel)))
	up := dbUpstream(t, e.st, meterUpstream, config.UpstreamMock, "sk-x", stub.url)
	dbSource(t, e.st, dbModel(t, e.st, meterModel), up, meterUpModel, 100)
	m := usage.NewMeter(e.st, 31, time.UTC, nil, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)

	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel)); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	if sp := m.Spend(testKeyID(t, e.st)); sp.DayMicro != 0 {
		t.Errorf("未定价模型应记 0 元，实际 %d 微元", sp.DayMicro)
	}
}

// TestMeterDisabledNoOp：没装配计量出口时，数据面行为一字不变（计量是旁路）。
func TestMeterDisabledNoOp(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, jsonReply(http.StatusOK, fmt.Sprintf(
		`{"id":"c1","model":%q,"choices":[],"usage":{"prompt_tokens":1}}`, meterUpModel)))
	up := dbUpstream(t, e.st, meterUpstream, config.UpstreamMock, "sk-x", stub.url)
	dbSource(t, e.st, dbModel(t, e.st, meterModel), up, meterUpModel, 100)

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model":"`+meterModel+`"`) {
		t.Errorf("响应 model 未回写: %s", w.Body.String())
	}
}

// 保证 httptest 的 recorder 支持 Flush（SSE 用例依赖它）。
var _ http.Flusher = (*httptest.ResponseRecorder)(nil)
