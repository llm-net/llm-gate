// claude_test.go 钉住 Claude Code 集中订阅代理最容易出安全事故的边界：
// 客户端 sk_ 头不越过设备、设备封存的 setup-token 才能出站、代理可明文回源、
// 原始请求/响应与 SSE 保真、上游错误不包装，以及计量归到发起调用的 API密钥。
package gateway_test

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const (
	claudeTestModel    = "claude-test-model"
	claudeOAuthFake    = "oauth-setup-credential-never-real"
	claudeAPIKeyFake   = "client-supplied-credential-never-real"
	claudePromptMark   = "prompt-private-test-marker"
	claudeResponseMark = "response-private-test-marker"
)

type claudeCapturedRequest struct {
	method   string
	path     string
	rawQuery string
	header   http.Header
	body     string
}

func serveClaude(e *routeEnv, method, target string, headers http.Header, body string, secure bool) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, reader)
	if secure {
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	}
	for name, values := range headers {
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func claudeHeaders() http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+testKey)
	h.Set("Content-Type", "application/json")
	h.Set("Anthropic-Beta", "claude-code-test-beta")
	h.Set("Anthropic-Version", "2023-06-01")
	return h
}

func connectClaudeCredential(t *testing.T, e *routeEnv, token string) int64 {
	t.Helper()
	cred, err := claudeauth.FromSetupToken(token)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := cred.JSON()
	if err != nil {
		t.Fatal(err)
	}
	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderClaude,
		Label:    "Claude 测试订阅",
		AuthJSON: blob,
	})
	if err != nil {
		t.Fatal(err)
	}
	exposeClaudeModels(e, claudeTestModel)
	return acct.ID
}

// exposeClaudeModels 把这些名字登记成 Claude 订阅带来的可见模型（开发工具策略
// 的订阅投影靠它）：/agents/claude/ 接入面对可用订阅只放行这把 Key 可见的模型名。
func exposeClaudeModels(e *routeEnv, names ...string) {
	owners := make(map[string]string, len(names))
	for _, name := range names {
		owners[name] = store.AgentProviderClaude
	}
	e.srv.SetAgentModels(&fakeAgentModels{owners: owners})
}

func decodeAnthropicType(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("Anthropic 错误体不是 JSON: %v\n%s", err, w.Body.String())
	}
	if out.Type != "error" || out.Error.Type == "" {
		t.Fatalf("Anthropic 错误体字段不全: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"code"`) {
		t.Fatalf("Anthropic 错误体不应带 code: %s", w.Body.String())
	}
	return out.Error.Type
}

func TestClaudeAcceptsProxyHTTPOriginAndUsesOnlyDeviceKeyFromClient(t *testing.T) {
	var hits atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"claude-test-model","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(backend.Close)
	e := newRouteEnv(t)
	e.srv.SetClaudeEndpoint(backend.URL)
	connectClaudeCredential(t, e, claudeOAuthFake)
	const secondKey = "sk_second_user_key_never_real"
	if _, _, err := gateway.ImportConfigKeys(t.Context(), e.st,
		[]config.APIKey{{Key: secondKey}}, logging.New(io.Discard, slog.LevelDebug)); err != nil {
		t.Fatalf("导入第二个用户的测试 Key: %v", err)
	}
	body := `{"model":"claude-test-model","max_tokens":1,"messages":[]}`

	t.Run("反向代理明文回源可用", func(t *testing.T) {
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, false)
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP 回源状态 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("缺客户端 API密钥拒绝", func(t *testing.T) {
		h := http.Header{"Content-Type": {"application/json"}}
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", h, body, true)
		if w.Code != http.StatusUnauthorized || decodeAnthropicType(t, w) != "authentication_error" {
			t.Fatalf("缺 API密钥响应 = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("无效 Bearer 不会被当成上游凭据透传", func(t *testing.T) {
		h := http.Header{"Authorization": {"Bearer " + claudeAPIKeyFake}, "Content-Type": {"application/json"}}
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", h, body, true)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("无效 Bearer 状态 = %d，期望 401", w.Code)
		}
	})

	t.Run("官方网关 Bearer 形态可用", func(t *testing.T) {
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusOK {
			t.Fatalf("Bearer 状态 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("标准 x-api-key 兼容形态可用", func(t *testing.T) {
		h := http.Header{"x-api-key": {testKey}, "Content-Type": {"application/json"}}
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", h, body, true)
		if w.Code != http.StatusOK {
			t.Fatalf("x-api-key 状态 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("另一把未配置的设备 Key 不能借用订阅", func(t *testing.T) {
		h := claudeHeaders()
		h.Set("Authorization", "Bearer "+secondKey)
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", h, body, true)
		if w.Code != http.StatusForbidden {
			t.Fatalf("第二把 Key 状态 = %d，期望 403；body: %s", w.Code, w.Body.String())
		}
	})

	if got := hits.Load(); got != 3 {
		t.Fatalf("假上游命中 = %d，期望只收到已授权 Key 的三次请求", got)
	}
}

func TestClaudeAccountStateAndUpstream401(t *testing.T) {
	body := `{"model":"` + claudeTestModel + `","max_tokens":1,"messages":[]}`

	t.Run("未连接、停用与已失效都不触上游", func(t *testing.T) {
		var hits atomic.Int64
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(backend.Close)
		e := newRouteEnv(t)
		e.srv.SetClaudeEndpoint(backend.URL)

		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "No active Claude Code subscription") {
			t.Fatalf("未连接响应=%d %s", w.Code, w.Body.String())
		}
		id := connectClaudeCredential(t, e, claudeOAuthFake)
		if err := e.st.SetAgentStatus(t.Context(), id, store.AgentStatusDisabled); err != nil {
			t.Fatal(err)
		}
		w = serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "No active Claude Code subscription") {
			t.Fatalf("停用响应=%d %s", w.Code, w.Body.String())
		}
		if err := e.st.SetAgentStatus(t.Context(), id, store.AgentStatusAuthExpired); err != nil {
			t.Fatal(err)
		}
		w = serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "needs a new setup-token") {
			t.Fatalf("失效响应=%d %s", w.Code, w.Body.String())
		}
		if hits.Load() != 0 {
			t.Fatalf("不可用状态仍命中上游 %d 次", hits.Load())
		}
	})

	t.Run("上游 401 原样返回并闩住该订阅", func(t *testing.T) {
		const upstreamBody = `{"type":"error","error":{"type":"authentication_error","message":"fake setup credential rejected"}}`
		var hits atomic.Int64
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, upstreamBody)
		}))
		t.Cleanup(backend.Close)
		e := newRouteEnv(t)
		e.srv.SetClaudeEndpoint(backend.URL)
		id := connectClaudeCredential(t, e, claudeOAuthFake)

		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusUnauthorized || w.Body.String() != upstreamBody {
			t.Fatalf("401 未保真: status=%d body=%q", w.Code, w.Body.String())
		}
		acct, err := e.st.GetAgentAccount(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if acct.Status != store.AgentStatusAuthExpired {
			t.Fatalf("401 后状态=%v", acct.Status)
		}
		w = serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusConflict || hits.Load() != 1 {
			t.Fatalf("闩住后 status=%d upstream_hits=%d", w.Code, hits.Load())
		}
	})

	t.Run("形态损坏的密文不会出站并标失效", func(t *testing.T) {
		e := newRouteEnv(t)
		acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
			Provider: store.AgentProviderClaude,
			AuthJSON: `{"unexpected":"fake-secret-never-real"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		exposeClaudeModels(e, claudeTestModel)
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusConflict {
			t.Fatalf("损坏凭据响应=%d %s", w.Code, w.Body.String())
		}
		got, err := e.st.GetAgentAccount(t.Context(), acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.AgentStatusAuthExpired {
			t.Fatalf("损坏凭据状态=%v", got.Status)
		}
	})
}

func TestClaudeMessagesByteFaithfulHeadersMeteringAndRedaction(t *testing.T) {
	const rawRequest = " {\n  \"model\": \"" + claudeTestModel + "\", \"max_tokens\": 1.00,\n" +
		"  \"messages\": [{\"role\":\"user\",\"content\":\"" + claudePromptMark + "\"}],\n" +
		"  \"future_field\": {\"keep\":true}\n}\n"
	const rawResponse = `{"id":"msg_test","type":"message","role":"assistant","model":"claude-upstream-version",` +
		`"content":[{"type":"text","text":"` + claudeResponseMark + `"}],"stop_reason":"future_reason",` +
		`"usage":{"input_tokens":11,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":7},` +
		`"x_future":{"n":1}}`
	const rawQuery = "beta=true&private_query_marker=must-not-be-logged"

	seen := make(chan claudeCapturedRequest, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- claudeCapturedRequest{
			method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery,
			header: r.Header.Clone(), body: string(b),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Anthropic-Future", "preserved")
		io.WriteString(w, rawResponse)
	}))
	t.Cleanup(backend.Close)

	e := newRouteEnv(t)
	e.srv.SetClaudeEndpoint(backend.URL)
	connectClaudeCredential(t, e, claudeOAuthFake)
	modelID := dbModel(t, e.st, claudeTestModel)
	if err := e.st.SetModelPricing(t.Context(), modelID, meterPricing); err != nil {
		t.Fatalf("SetModelPricing: %v", err)
	}
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	h := claudeHeaders()
	h.Set("x-api-key", claudeAPIKeyFake)
	h.Set("Proxy-Authorization", "Bearer "+claudeAPIKeyFake)
	h.Set("X-Claude-Code-Version", "test-version")
	h.Set("X-LlmGate-Private-Control", "must-not-leave-device")
	h.Set("Cookie", "session=must-not-leave-device")
	h.Set("Connection", "X-Remove-Me")
	h.Set("X-Remove-Me", "must-not-leave-device")
	w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages?"+rawQuery, h, rawRequest, true)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != rawResponse {
		t.Fatalf("响应体被改写\n got: %q\nwant: %q", got, rawResponse)
	}
	if got := w.Header().Get("X-Anthropic-Future"); got != "preserved" {
		t.Errorf("未知响应头 = %q，期望 preserved", got)
	}

	req := <-seen
	if req.method != http.MethodPost || req.path != "/v1/messages" || req.rawQuery != rawQuery {
		t.Errorf("上游请求 = %s %s?%s", req.method, req.path, req.rawQuery)
	}
	if req.body != rawRequest {
		t.Fatalf("请求体被重编码\n got: %q\nwant: %q", req.body, rawRequest)
	}
	for name, want := range map[string]string{
		"Authorization":         "Bearer " + claudeOAuthFake,
		"Anthropic-Beta":        "claude-code-test-beta,oauth-2025-04-20",
		"Anthropic-Version":     "2023-06-01",
		"X-Claude-Code-Version": "test-version",
	} {
		if got := req.header.Get(name); got != want {
			t.Errorf("上游 %s = %q，期望 %q", name, got, want)
		}
	}
	for _, name := range []string{"x-api-key", "Proxy-Authorization", "X-LlmGate-API-Key", "X-LlmGate-Private-Control", "Cookie", "X-Remove-Me", "Connection"} {
		if got := req.header.Get(name); got != "" {
			t.Errorf("设备边界头 %s 被发往上游: %q", name, got)
		}
	}

	sample := fm.only(t)
	if sample.Entry != usage.EntryClaudeCode || sample.ModelName != claudeTestModel ||
		!sample.ModelKnown || sample.Pricing != meterPricing {
		t.Errorf("Claude 计量维度不符: %+v", sample)
	}
	if sample.UpstreamName != "Claude Code 订阅 · Claude 测试订阅" || sample.Attempts != 1 {
		t.Errorf("固定上游维度 = upstream %q attempts %d，期望订阅账号名称/1", sample.UpstreamName, sample.Attempts)
	}
	if got := sample.Tokens; got.Prompt != 11 || got.Completion != 7 || got.CacheRead != 3 || got.CacheWrite != 2 {
		t.Errorf("Claude Messages token = %+v", got)
	}
	if sample.Estimated {
		t.Error("有完整 Anthropic usage 的请求不应标 estimated")
	}

	logs := e.logBuf.String()
	for _, forbidden := range []string{
		claudeOAuthFake, claudeAPIKeyFake, testKey, claudePromptMark, claudeResponseMark,
		"must-not-be-logged", "must-not-leave-device",
	} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("日志泄露敏感标记 %q: %s", forbidden, logs)
		}
	}
}

func TestClaudeSSEAndUpstreamErrorAreUnchanged(t *testing.T) {
	t.Run("SSE 保留 ping、未知事件与上游 model", func(t *testing.T) {
		const stream = ": keepalive\n\n" +
			"event: message_start\n" +
			"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-upstream-version\",\"usage\":{\"input_tokens\":9,\"cache_read_input_tokens\":2,\"cache_creation_input_tokens\":1,\"output_tokens\":1}}}\n\n" +
			"event: ping\ndata: {\"type\":\"ping\",\"future\":true}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, stream)
		}))
		t.Cleanup(backend.Close)
		e := newRouteEnv(t)
		e.srv.SetClaudeEndpoint(backend.URL)
		connectClaudeCredential(t, e, claudeOAuthFake)
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		body := `{"model":"` + claudeTestModel + `","stream":true,"messages":[]}`
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages?beta=true", claudeHeaders(), body, true)
		if w.Code != http.StatusOK || w.Body.String() != stream {
			t.Fatalf("SSE 未保真：status=%d\n got: %q\nwant: %q", w.Code, w.Body.String(), stream)
		}
		sample := fm.only(t)
		if sample.Entry != usage.EntryClaudeCode || sample.Estimated ||
			sample.Tokens.Prompt != 9 || sample.Tokens.Completion != 5 ||
			sample.Tokens.CacheRead != 2 || sample.Tokens.CacheWrite != 1 {
			t.Errorf("SSE 计量不符: %+v", sample)
		}
	})

	t.Run("上游错误状态、头和 body 不包装", func(t *testing.T) {
		const upstreamBody = `{"type":"error","error":{"type":"rate_limit_error","message":"exact upstream wording"},"future":1}`
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "23")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, upstreamBody)
		}))
		t.Cleanup(backend.Close)
		e := newRouteEnv(t)
		e.srv.SetClaudeEndpoint(backend.URL)
		connectClaudeCredential(t, e, claudeOAuthFake)
		body := `{"model":"` + claudeTestModel + `","messages":[]}`
		w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
		if w.Code != http.StatusTooManyRequests || w.Body.String() != upstreamBody || w.Header().Get("Retry-After") != "23" {
			t.Fatalf("错误未原样透传：status=%d retry=%q body=%q", w.Code, w.Header().Get("Retry-After"), w.Body.String())
		}
	})
}

func TestClaudeUtilityEndpointsAndAdmissionBoundary(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages/count_tokens":
			io.WriteString(w, `{"input_tokens":19}`)
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"claude-test-model"}],"has_more":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)
	e := newRouteEnv(t)
	e.srv.SetClaudeEndpoint(backend.URL)
	connectClaudeCredential(t, e, claudeOAuthFake)
	fm := &fakeMeter{}
	denyAll(fm, usage.RejectRPM, 11)
	e.srv.EnableMetering(fm)

	body := `{"model":"` + claudeTestModel + `","messages":[]}`
	w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages/count_tokens", claudeHeaders(), body, true)
	if w.Code != http.StatusOK || w.Body.String() != `{"input_tokens":19}` {
		t.Fatalf("count_tokens = %d %q", w.Code, w.Body.String())
	}
	sample := fm.only(t)
	if sample.Entry != usage.EntryClaudeCode || sample.Rejected || sample.Tokens.Total(sample.Entry) != 0 {
		t.Errorf("count_tokens 计量 = %+v", sample)
	}

	w = serveClaude(e, http.MethodGet, "/agents/claude/v1/models?limit=1000", claudeHeaders(), "", true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), claudeTestModel) {
		t.Fatalf("models = %d %q", w.Code, w.Body.String())
	}
	w = serveClaude(e, http.MethodHead, "/agents/claude/api/hello", claudeHeaders(), "", true)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("hello = %d body=%q", w.Code, w.Body.String())
	}
	if fm.count() != 1 {
		t.Fatalf("模型发现/hello 不应记账，样本数 = %d", fm.count())
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if got := strings.Join(gotPaths, " | "); got != "POST /v1/messages/count_tokens | GET /v1/models?limit=1000" {
		t.Fatalf("上游路径 = %q", got)
	}

	// 生成请求要过准入；同一台假上游不应再被命中。
	w = serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
	if w.Code != http.StatusTooManyRequests || decodeAnthropicType(t, w) != "rate_limit_error" ||
		w.Header().Get("Retry-After") != "11" {
		t.Fatalf("生成准入响应 = %d retry=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	mu.Lock()
	hits := len(paths)
	mu.Unlock()
	if hits != 2 {
		t.Fatalf("被拒生成仍命中上游，命中数 = %d", hits)
	}
}

func TestClaudeNeverFollowsUpstreamRedirect(t *testing.T) {
	var redirectedHits atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(redirected.Close)
	const redirectBody = `{"type":"error","error":{"type":"redirect_rejected","message":"do not follow"}}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirected.URL+"/credential-catcher")
		w.WriteHeader(http.StatusTemporaryRedirect)
		io.WriteString(w, redirectBody)
	}))
	t.Cleanup(backend.Close)

	e := newRouteEnv(t)
	e.srv.SetClaudeEndpoint(backend.URL)
	connectClaudeCredential(t, e, claudeOAuthFake)
	body := `{"model":"` + claudeTestModel + `","messages":[]}`
	w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages?beta=true", claudeHeaders(), body, true)
	if w.Code != http.StatusTemporaryRedirect || w.Body.String() != redirectBody {
		t.Fatalf("重定向响应 = %d %q", w.Code, w.Body.String())
	}
	if redirectedHits.Load() != 0 {
		t.Fatal("网关跟随了上游重定向，可能把 Claude 登录凭据送到另一目的地")
	}
}

func TestClaudeFallbackAndTransportFailureStayRedacted(t *testing.T) {
	e := newRouteEnv(t)
	e.srv.SetClaudeEndpoint("http://127.0.0.1:1")
	connectClaudeCredential(t, e, claudeOAuthFake)

	w := serveClaude(e, http.MethodGet, "/agents/claude/not-supported", claudeHeaders(), "", true)
	if w.Code != http.StatusNotFound || decodeAnthropicType(t, w) != "not_found_error" {
		t.Fatalf("Claude fallback = %d %s", w.Code, w.Body.String())
	}

	const queryMark = "transport-private-query-marker"
	body := `{"model":"` + claudeTestModel + `","messages":[{"role":"user","content":"` + claudePromptMark + `"}]}`
	w = serveClaude(e, http.MethodPost, "/agents/claude/v1/messages?beta=true&secret="+queryMark,
		claudeHeaders(), body, true)
	if w.Code != http.StatusBadGateway || decodeAnthropicType(t, w) != "api_error" {
		t.Fatalf("不可达响应 = %d %s", w.Code, w.Body.String())
	}
	logs := e.logBuf.String()
	for _, forbidden := range []string{queryMark, claudeOAuthFake, testKey, claudePromptMark} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("传输失败日志泄露 %q: %s", forbidden, logs)
		}
	}
}

// TestClaudeModelDiscoveryNarrowsToVisibleModel 钉住「对成员可见的模型」在接入面
// 上的两层效果：模型发现只列这把 Key 可见的订阅模型（订阅账号设了可见模型就只剩
// 同名一项、游标跟着收窄；上游没有那个名字时回空清单、游标归 null、不硬编造），
// 生成请求对可见集之外的模型名答 404——策略在数据面裁决，不只是收窄选择器；
// 模型发现自己不记账。
func TestClaudeModelDiscoveryNarrowsToVisibleModel(t *testing.T) {
	const fullList = `{"data":[{"id":"claude-a","type":"model"},{"id":"claude-fable-5","type":"model","display_name":"Fable 5","capabilities":{"future_field":true}},` +
		`{"id":"claude-b","type":"model"}],"has_more":true,"first_id":"claude-a","last_id":"claude-b"}`
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, fullList)
		case "/v1/messages":
			io.WriteString(w, `{"id":"msg_1","model":"claude-fable-5","usage":{"input_tokens":1,"output_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backend.Close)
	e := newRouteEnv(t)
	e.srv.SetClaudeEndpoint(backend.URL)
	id := connectClaudeCredential(t, e, claudeOAuthFake)
	exposeClaudeModels(e, "claude-a", "claude-fable-5", "claude-b")
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	discover := func() *httptest.ResponseRecorder {
		return serveClaude(e, http.MethodGet, "/agents/claude/v1/models?limit=1000", claudeHeaders(), "", true)
	}
	setVisible := func(model string) {
		t.Helper()
		if err := e.st.UpdateAgentAccount(t.Context(), id, "Claude 测试订阅", model); err != nil {
			t.Fatal(err)
		}
	}
	type modelList struct {
		Data []struct {
			ID           string          `json:"id"`
			Type         string          `json:"type"`
			DisplayName  string          `json:"display_name"`
			Capabilities json.RawMessage `json:"capabilities"`
		} `json:"data"`
		HasMore bool    `json:"has_more"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
	}
	decodeList := func(w *httptest.ResponseRecorder) modelList {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("模型发现 = %d %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Content-Length"); got != "" && got != strconv.Itoa(w.Body.Len()) {
			t.Errorf("Content-Length=%s 与实际 %d 不符", got, w.Body.Len())
		}
		var out modelList
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("模型清单不是 JSON: %v\n%s", err, w.Body.String())
		}
		return out
	}

	// 未设可见模型：订阅带来的三项都可见，清单收敛为单页。
	got := decodeList(discover())
	if len(got.Data) != 3 || got.HasMore {
		t.Fatalf("未收窄时应列出全部可见订阅模型: %+v", got)
	}
	for _, model := range got.Data {
		want := "LLM Gate · " + model.ID
		if model.ID == "claude-fable-5" {
			want = "LLM Gate · Fable 5"
		}
		if model.DisplayName != want {
			t.Fatalf("模型显示名未按上游名称或模型 ID 生成: %+v", model)
		}
	}

	// 设了：只剩同名一项，游标与 has_more 跟着收窄走。
	setVisible("claude-fable-5")
	got = decodeList(discover())
	if len(got.Data) != 1 || got.Data[0].ID != "claude-fable-5" || got.Data[0].Type != "model" ||
		got.Data[0].DisplayName != "LLM Gate · Fable 5" || string(got.Data[0].Capabilities) != `{"future_field":true}` {
		t.Fatalf("收窄后的清单 = %+v", got.Data)
	}
	if got.HasMore || got.FirstID == nil || *got.FirstID != "claude-fable-5" ||
		got.LastID == nil || *got.LastID != "claude-fable-5" {
		t.Fatalf("分页字段未跟随收窄: %+v", got)
	}

	// 可见模型上游没有：回空清单，游标归 null，不硬编造一个模型。
	setVisible("claude-not-exposed")
	got = decodeList(discover())
	if len(got.Data) != 0 || got.FirstID != nil || got.LastID != nil {
		t.Fatalf("上游没有该模型时应回空清单: %+v", got)
	}
	if fm.count() != 0 {
		t.Fatalf("模型发现不应记账，样本数 = %d", fm.count())
	}

	// 可见集之外的模型名在数据面被拦：404，不出网、不记账。
	setVisible("claude-fable-5")
	w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(),
		`{"model":"claude-b","messages":[]}`, true)
	if w.Code != http.StatusNotFound || decodeAnthropicType(t, w) != "not_found_error" {
		t.Fatalf("可见集之外的模型名应 404: %d %s", w.Code, w.Body.String())
	}
	if fm.count() != 0 {
		t.Fatalf("被拦的请求不应记账，样本数 = %d", fm.count())
	}
	// 可见模型照常转发并记一笔账。
	w = serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(),
		`{"model":"claude-fable-5","messages":[]}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("可见模型应照常转发: %d %s", w.Code, w.Body.String())
	}
	if fm.count() != 1 {
		t.Fatalf("生成应记一笔账，样本数 = %d", fm.count())
	}
}
