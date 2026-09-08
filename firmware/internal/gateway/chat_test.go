// chat_test.go 验证 chat 入口的透传语义：请求侧 model 重写与未知字段保留
// （含大整数不失真）、include_usage 注入与客户端显式值优先、鉴权头重写、
// hop-by-hop 剥离与业务头透传；响应侧 model 字段改写回逻辑模型名
// （iteration-3 Phase 4，非流式与 SSE chunk，错误体/非 JSON 不改写）、
// SSE 转发与 [DONE]、网关自身错误形态、取消传播。
package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamCapture 记录 mock 上游收到的请求。
type upstreamCapture struct {
	mu     sync.Mutex
	header http.Header
	body   []byte
}

func (c *upstreamCapture) set(h http.Header, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.header, c.body = h, b
}

func (c *upstreamCapture) get() (http.Header, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.header, c.body
}

// startUpstream 起一个记录请求并回放 canned 响应的 mock 上游。
func startUpstream(t *testing.T, status int, header map[string]string, body string) (*httptest.Server, *upstreamCapture) {
	t.Helper()
	rec := &upstreamCapture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.set(r.Header.Clone(), b)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)
	return up, rec
}

// TestChatNonStreamPassthrough：非流式全链路——
// 请求侧：model 重写为上游 ID、未知字段（含 2^53+1 大整数）原样保留、
// 鉴权头重写为上游 Key、hop-by-hop 与 Connection 点名头剥离、anthropic-* 透传；
// 响应侧：model 改写回逻辑名（§11.1，Content-Length 重算），其余字段
// （未知字段、pause_turn、大整数）原样保留，状态码/自定义头原样回写。
func TestChatNonStreamPassthrough(t *testing.T) {
	upstreamBody := `{"id":"chatcmpl-1","model":"mock-gpt-4o-260801","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hi"},"finish_reason":"pause_turn"}],` +
		`"usage":{"total_tokens":42},"seed_echo":9007199254740993,"x_mock_extra":"passthrough"}`
	up, urec := startUpstream(t, http.StatusOK,
		map[string]string{"Content-Type": "application/json", "X-Upstream-Extra": "yes"},
		upstreamBody)

	h, _ := newTestHandler(t, up.URL+"/v1")
	w := do(h, "POST", "/v1/chat/completions", map[string]string{
		"Authorization":  "Bearer " + testKey,
		"Content-Type":   "application/json",
		"anthropic-beta": "tools-2024",
		"Connection":     "x-strip-me",
		"X-Strip-Me":     "1",
		"TE":             "trailers",
	}, `{"model":"chat-strong","messages":[{"role":"user","content":"hello"}],`+
		`"x_client_extra":{"nested":[1,2,3]},"seed":9007199254740993}`)

	// —— 响应侧：model 改回逻辑名，其余字段原样保留 ——
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	if got["model"] != "chat-strong" {
		t.Errorf(`响应 model = %v，期望改写回逻辑名 "chat-strong"`, got["model"])
	}
	if strings.Contains(w.Body.String(), "mock-gpt-4o") {
		t.Errorf("响应泄露上游真实模型 ID: %s", w.Body.String())
	}
	// 除 model 外逐字段与上游响应一致：未知字段 x_mock_extra 与非常规
	// finish_reason pause_turn 都保留。
	var want map[string]any
	if err := json.Unmarshal([]byte(upstreamBody), &want); err != nil {
		t.Fatalf("canned body 不是 JSON: %v", err)
	}
	want["model"] = "chat-strong"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("除 model 外应与上游响应逐字段一致:\ngot:  %v\nwant: %v", got, want)
	}
	// 响应侧大整数经 round-trip 后不失真（UseNumber 保字面量）。
	if !strings.Contains(w.Body.String(), "9007199254740993") {
		t.Errorf("响应大整数经改写后失真: %s", w.Body.String())
	}
	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(w.Body.Len()) {
		t.Errorf("Content-Length = %q，期望按改写后字节重算为 %d", cl, w.Body.Len())
	}
	if w.Header().Get("X-Upstream-Extra") != "yes" {
		t.Errorf("上游自定义响应头未透传: %v", w.Header())
	}

	// —— 请求侧：上游看到什么 ——
	upHeader, upBody := urec.get()
	if upHeader == nil {
		t.Fatal("上游未收到请求")
	}
	var sent map[string]any
	if err := json.Unmarshal(upBody, &sent); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %v\n%s", err, upBody)
	}
	if sent["model"] != "mock-gpt-4o" {
		t.Errorf("model 未重写为上游 ID: %v", sent["model"])
	}
	if _, has := sent["x_client_extra"]; !has {
		t.Errorf("未知请求字段丢失: %s", upBody)
	}
	if !strings.Contains(string(upBody), "9007199254740993") {
		t.Errorf("大整数经透传后失真（应保留 9007199254740993）: %s", upBody)
	}
	if got := upHeader.Get("Authorization"); got != "Bearer "+testUpstreamKey {
		t.Errorf("Authorization 未重写为上游 Key: %q", got)
	}
	if got := upHeader.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key 不应透传给上游: %q", got)
	}
	if got := upHeader.Get("anthropic-beta"); got != "tools-2024" {
		t.Errorf("anthropic-* 业务头应透传: %q", got)
	}
	for _, hh := range []string{"Connection", "TE", "X-Strip-Me"} {
		if got := upHeader.Get(hh); got != "" {
			t.Errorf("hop-by-hop 头 %s 应被剥离: %q", hh, got)
		}
	}
	// 客户端 Key 无论以何种形式都不得抵达上游。
	for k, vs := range upHeader {
		for _, v := range vs {
			if strings.Contains(v, testKey) {
				t.Errorf("客户端 Key 泄露到上游头 %s: %q", k, v)
			}
		}
	}
}

// TestChatUpstreamErrorPassthrough：上游 4xx/5xx 状态码与错误体原样透传——
// 错误体里即使带 model 字段也不改写（改写只作用于 2xx，保真优先）。
func TestChatUpstreamErrorPassthrough(t *testing.T) {
	upstreamBody := `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit"},` +
		`"model":"mock-gpt-4o-260801","x_extra":1}`
	up, _ := startUpstream(t, http.StatusTooManyRequests,
		map[string]string{"Content-Type": "application/json", "Retry-After": "7"}, upstreamBody)

	h, _ := newTestHandler(t, up.URL+"/v1")
	w := do(h, "POST", "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
		`{"model":"chat-fast","messages":[]}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429 原样透传", w.Code)
	}
	if w.Body.String() != upstreamBody {
		t.Errorf("上游错误体未原样透传: %s", w.Body.String())
	}
	if w.Header().Get("Retry-After") != "7" {
		t.Errorf("Retry-After 未透传: %v", w.Header())
	}
}

// TestChatNonStreamRewriteSkips：model 改写的保真边界——2xx 响应体不是
// JSON 对象、顶层无 model 字段、或 JSON 后有尾随内容时，原字节透传不改写，
// 绝不因改写尝试破坏响应。
func TestChatNonStreamRewriteSkips(t *testing.T) {
	cases := []struct{ name, body string }{
		{"非 JSON body", "plain text, not json"},
		{"无 model 字段", `{"id":"chatcmpl-1","choices":[],"usage":{"total_tokens":1}}`},
		{"JSON 后有尾随内容", `{"model":"mock-gpt-4o-260801"} trailing`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, _ := startUpstream(t, http.StatusOK,
				map[string]string{"Content-Type": "application/json"}, c.body)
			h, _ := newTestHandler(t, up.URL+"/v1")
			w := do(h, "POST", "/v1/chat/completions",
				map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
				`{"model":"chat-strong","messages":[]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
			}
			if w.Body.String() != c.body {
				t.Errorf("应原样透传:\ngot:  %q\nwant: %q", w.Body.String(), c.body)
			}
		})
	}
}

// TestChatStreamSSE：流式全链路——客户端未带 stream_options 时网关注入
// include_usage=true；带 model 的 chunk 逐事件改写回逻辑名（§11.1），
// 无 model/非 JSON 的 data 行与 [DONE] 原样转发不断流，事件不增不减。
func TestChatStreamSSE(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"mock-gpt-4o-mini-260801\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"model\":\"mock-gpt-4o-mini-260801\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}],\"x_mock_extra\":\"e\"}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"pause_turn\"}]}\n\n" +
		"data: not-json{{\n\n" +
		"data: {\"choices\":[],\"model\":\"mock-gpt-4o-mini-260801\",\"usage\":{\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	up, urec := startUpstream(t, http.StatusOK,
		map[string]string{"Content-Type": "text/event-stream"}, sse)

	h, _ := newTestHandler(t, up.URL+"/v1")
	w := do(h, "POST", "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
		`{"model":"chat-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}
	out := w.Body.String()
	// 带 model 的 3 个 chunk 全部改写为逻辑名，真实上游 ID 不落客户端。
	if strings.Contains(out, "mock-gpt-4o-mini") {
		t.Errorf("SSE 输出泄露上游真实模型 ID:\n%s", out)
	}
	if got := strings.Count(out, `"model":"chat-fast"`); got != 3 {
		t.Errorf(`带 model 的 chunk 应全部改写（期望 3 处 "model":"chat-fast"），实得 %d:\n%s`, got, out)
	}
	// 无 model 字段与非 JSON 的 data 行、[DONE] 行原样转发，不断流。
	for _, keep := range []string{
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"pause_turn\"}]}\n\n",
		"data: not-json{{\n\n",
		"data: [DONE]\n\n",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("SSE 行未原样保留 %q:\n%s", keep, out)
		}
	}
	// 改写后的 chunk 保留未知字段。
	if !strings.Contains(out, `"x_mock_extra":"e"`) {
		t.Errorf("改写后的 chunk 应保留未知字段 x_mock_extra:\n%s", out)
	}
	// 改写不得增删事件行。
	if got, want := strings.Count(out, "data: "), strings.Count(sse, "data: "); got != want {
		t.Errorf("data 行数 = %d，期望 %d（改写不得增删事件）", got, want)
	}
	if !w.Flushed {
		t.Error("SSE 响应应被逐块 Flush")
	}

	_, upBody := urec.get()
	var sent struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal(upBody, &sent); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %v", err)
	}
	if sent.StreamOptions == nil || sent.StreamOptions["include_usage"] != true {
		t.Errorf("客户端未带 stream_options 时网关应注入 include_usage=true，上游收到: %s", upBody)
	}
}

// TestChatStreamRespectsExplicitIncludeUsage：客户端显式 include_usage=false
// 时网关不改写（注入只补缺失，不覆盖）。
func TestChatStreamRespectsExplicitIncludeUsage(t *testing.T) {
	up, urec := startUpstream(t, http.StatusOK,
		map[string]string{"Content-Type": "text/event-stream"}, "data: [DONE]\n\n")

	h, _ := newTestHandler(t, up.URL+"/v1")
	do(h, "POST", "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
		`{"model":"chat-fast","stream":true,"stream_options":{"include_usage":false},"messages":[]}`)

	_, upBody := urec.get()
	var sent struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal(upBody, &sent); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %v", err)
	}
	if sent.StreamOptions["include_usage"] != false {
		t.Errorf("客户端显式 include_usage=false 被改写: %s", upBody)
	}
}

// TestChatGatewayErrors：网关自身错误返回 OpenAI 风格 error JSON + 明确 code，
// 且不泄露上游信息。
func TestChatGatewayErrors(t *testing.T) {
	// 上游不可达：base_url 指向必然拒绝连接的地址。
	h, logBuf := newTestHandler(t, "http://127.0.0.1:1/v1")
	auth := map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"}

	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"body 非 JSON", `{not-json`, http.StatusBadRequest, "invalid_request_body"},
		{"缺 model", `{"messages":[]}`, http.StatusBadRequest, "missing_model"},
		{"model 非字符串", `{"model":42,"messages":[]}`, http.StatusBadRequest, "missing_model"},
		{"未知模型", `{"model":"no-such-model","messages":[]}`, http.StatusNotFound, "model_not_found"},
		{"上游不可达", `{"model":"chat-fast","messages":[{"role":"user","content":"绝密提示词"}]}`,
			http.StatusBadGateway, "upstream_unreachable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(h, "POST", "/v1/chat/completions", auth, c.body)
			if w.Code != c.wantStatus {
				t.Fatalf("状态码 = %d，期望 %d；body: %s", w.Code, c.wantStatus, w.Body.String())
			}
			if _, code, msg := decodeError(t, w); code != c.wantCode {
				t.Errorf("error.code = %q，期望 %q（message: %s）", code, c.wantCode, msg)
			}
			// 错误响应不得泄露上游模型 ID 或上游 Key。
			for _, leak := range []string{"mock-gpt-4o", testUpstreamKey} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("错误响应泄露上游信息 %q: %s", leak, w.Body.String())
				}
			}
		})
	}
	// 上游不可达路径的日志不得含提示词与 Key（§15.1）。
	for _, leak := range []string{"绝密提示词", testKey, testUpstreamKey} {
		if strings.Contains(logBuf.String(), leak) {
			t.Errorf("日志泄露 %q:\n%s", leak, logBuf.String())
		}
	}
}

// TestChatClientCancelPropagates：客户端断开（context 取消）传播到上游——
// 上游侧请求 context 随之取消（§9.4 条 6）。
func TestChatClientCancelPropagates(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 必须先读完 body：net/http 服务端在 body 读尽后才开启后台读，
		// 靠它检测连接关闭并取消 r.Context()（真实上游都会读 body）。
		io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(up.Close)

	h, _ := newTestHandler(t, up.URL+"/v1")
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, "POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"chat-fast","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer "+testKey)
	r.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("上游未收到请求")
	}
	cancel() // 模拟客户端断开

	select {
	case <-canceled:
		// 取消已传播到上游
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后 5s 内取消未传播到上游")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler 未在取消后返回")
	}
}
