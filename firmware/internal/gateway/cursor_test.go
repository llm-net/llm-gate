// cursor_test.go 钉住 Cursor 订阅代理（/agents/cursor/）最容易出安全事故的边界：
// exchange 真验上游后签发设备本地 JWT（两侧凭据都不外泄、不计量）、策略勾选与
// 订阅可用性的 Connect 风格错误、aiserver.v1 整面逐字节转发（authorization 换发、
// 客户端 Key 不越过设备、trailer 与增量 flush）、agent.v1 对话面的真正双向流、
// 上游 401 的单次重试与二次 401 落 auth_expired、exchange 被上游拒绝落
// auth_expired、只计量 Run/RunSSE 对话且辅助 RPC 不过准入，以及日志对
// Key/上游 token/请求响应内容的全量脱敏。
package gateway_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/cursorauth"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
	"log/slog"
)

const (
	cursorAPIKeyFake   = "cursor-dashboard-api-key-never-real"
	cursorUpstreamTok  = "cursor-upstream-access-token-never-real"
	cursorUpstreamSub  = "cursor-upstream-user-never-real"
	cursorRPCPath      = "/agents/cursor/aiserver.v1.AiService/AvailableModels"
	cursorExchangePath = "/agents/cursor/auth/exchange_user_api_key"
	cursorReqMark      = "cursor-request-proto-private-marker"
	cursorRespMark     = "cursor-response-proto-private-marker"
)

func cursorHeaders() http.Header {
	h := make(http.Header)
	h.Set("Authorization", "Bearer "+testKey)
	h.Set("Content-Type", "application/proto")
	h.Set("Connect-Protocol-Version", "1")
	h.Set("x-cursor-client-type", "cli")
	return h
}

func fakeCursorUpstreamToken(n int64) string {
	payload, _ := json.Marshal(map[string]any{
		"sub": cursorUpstreamSub, "iat": int64(1_700_000_000),
		"exp": int64(4_102_444_800), "aud": "cursor-agent",
	})
	signature := base64.RawURLEncoding.EncodeToString([]byte("fake-signature-" + strconv.FormatInt(n, 10)))
	return "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		base64.RawURLEncoding.EncodeToString(payload) + "." + signature
}

func cursorTestJWTSubject(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token 不是 JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.Subject
}

func connectCursorCredential(t *testing.T, e *routeEnv, apiKey string) int64 {
	t.Helper()
	cred, err := cursorauth.FromAPIKey(apiKey)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := cred.JSON()
	if err != nil {
		t.Fatal(err)
	}
	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderCursor,
		Label:    "Cursor 测试订阅",
		AuthJSON: blob,
	})
	if err != nil {
		t.Fatal(err)
	}
	return acct.ID
}

// decodeConnectError 断言响应是 Connect 统一错误形 {"code","message"} 且不带
// 别家错误信封（error 包装、type 字段），返回两个字段。
func decodeConnectError(t *testing.T, w *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Connect 错误 Content-Type = %q，期望 application/json", ct)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("Connect 错误体不是 JSON: %v\n%s", err, w.Body.String())
	}
	if _, has := out["error"]; has {
		t.Fatalf("Connect 错误体不应套 error 信封: %s", w.Body.String())
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code == "" || body.Message == "" {
		t.Fatalf("Connect 错误体字段不全: %s", w.Body.String())
	}
	return body.Code, body.Message
}

func exchangeCursorClientToken(t *testing.T, e *routeEnv) string {
	t.Helper()
	w := serveClaude(e, http.MethodPost, cursorExchangePath, cursorHeaders(), "{}", false)
	if w.Code != http.StatusOK {
		t.Fatalf("exchange = %d %s", w.Code, w.Body.String())
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.AccessToken == "" {
		t.Fatalf("exchange 体无效: %s err=%v", w.Body.String(), err)
	}
	return out.AccessToken
}

// cursorDenyUpstream 起一个被命中即 Fail 的假上游：钉未知路径在设备本地 404。
func cursorDenyUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("不该出网的请求打到了上游: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCursorExchangeIssuesLocalJWTAndIsPolicyGated(t *testing.T) {
	upstream, exchangeHits, _, _ := newCursorUpstream(t, nil,
		func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "unused") })
	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(upstream.URL)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	const secondKey = "sk_second_user_key_never_real"
	if _, _, err := gateway.ImportConfigKeys(t.Context(), e.st,
		[]config.APIKey{{Key: secondKey}}, logging.New(io.Discard, slog.LevelDebug)); err != nil {
		t.Fatalf("导入第二个用户的测试 Key: %v", err)
	}

	t.Run("缺客户端 API密钥是 unauthenticated", func(t *testing.T) {
		w := serveClaude(e, http.MethodPost, cursorExchangePath,
			http.Header{"Content-Type": {"application/json"}}, "{}", false)
		if code, _ := decodeConnectError(t, w); w.Code != http.StatusUnauthorized || code != "unauthenticated" {
			t.Fatalf("缺 Key 响应 = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("未勾选的 Key 是 permission_denied", func(t *testing.T) {
		h := cursorHeaders()
		h.Set("Authorization", "Bearer "+secondKey)
		w := serveClaude(e, http.MethodPost, cursorExchangePath, h, "{}", false)
		if code, _ := decodeConnectError(t, w); w.Code != http.StatusForbidden || code != "permission_denied" {
			t.Fatalf("未勾选响应 = %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("勾选但订阅缺失或停用或已失效都是 failed_precondition", func(t *testing.T) {
		w := serveClaude(e, http.MethodPost, cursorExchangePath, cursorHeaders(), "{}", false)
		code, msg := decodeConnectError(t, w)
		if w.Code != http.StatusConflict || code != "failed_precondition" ||
			!strings.Contains(msg, "not currently available") {
			t.Fatalf("未连接响应 = %d %s", w.Code, w.Body.String())
		}
		id := connectCursorCredential(t, e, cursorAPIKeyFake)
		for _, status := range []string{store.AgentStatusDisabled, store.AgentStatusAuthExpired} {
			if err := e.st.SetAgentStatus(t.Context(), id, status); err != nil {
				t.Fatal(err)
			}
			w = serveClaude(e, http.MethodPost, cursorExchangePath, cursorHeaders(), "{}", false)
			if code, _ := decodeConnectError(t, w); w.Code != http.StatusConflict || code != "failed_precondition" {
				t.Fatalf("%s 响应 = %d %s", status, w.Code, w.Body.String())
			}
		}
		if err := e.st.SetAgentStatus(t.Context(), id, store.AgentStatusActive); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("可用时返回绑定客户端 Key 的本地 JWT", func(t *testing.T) {
		w := serveClaude(e, http.MethodPost, cursorExchangePath, cursorHeaders(), "{}", false)
		if w.Code != http.StatusOK {
			t.Fatalf("exchange = %d %s", w.Code, w.Body.String())
		}
		var out struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil ||
			out.AccessToken == testKey || out.AccessToken == fakeCursorUpstreamToken(1) ||
			out.RefreshToken != "llmgate" || cursorTestJWTSubject(t, out.AccessToken) != cursorUpstreamSub {
			t.Fatalf("exchange 体 = %s (err=%v)", w.Body.String(), err)
		}
		for _, forbidden := range []string{testKey, cursorAPIKeyFake, fakeCursorUpstreamToken(1)} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatalf("exchange 体泄露凭据 %q", forbidden)
			}
		}
	})

	// 探针真验一次上游订阅，但不产生用量账。
	if fm.count() != 0 {
		t.Fatalf("exchange 不该计量，样本数 = %d", fm.count())
	}
	if exchangeHits.Load() != 1 {
		t.Fatalf("可用探针应命中上游一次，得到 %d", exchangeHits.Load())
	}
	logs := e.logBuf.String()
	for _, forbidden := range []string{cursorAPIKeyFake, testKey, secondKey} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("日志泄露敏感标记 %q", forbidden)
		}
	}
}

// newCursorUpstream 起一个同根承载 exchange 与 RPC 的假 Cursor 上游。
// exchangeStatus 非 0 时 exchange 恒回该状态；rpc 是 RPC 面的应答器。
func newCursorUpstream(t *testing.T, exchangeStatus *atomic.Int64,
	rpc http.HandlerFunc) (srv *httptest.Server, exchangeHits, rpcHits *atomic.Int64, lastRPC *atomic.Value) {
	t.Helper()
	exchangeHits, rpcHits = &atomic.Int64{}, &atomic.Int64{}
	lastRPC = &atomic.Value{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/auth/exchange_user_api_key" {
			n := exchangeHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			if got := r.Header.Get("Authorization"); got != "Bearer "+cursorAPIKeyFake {
				t.Errorf("exchange Authorization = %q，期望板上封存的 API Key", got)
			}
			if string(body) != "{}" {
				t.Errorf("exchange 体 = %q，期望 {}", body)
			}
			if exchangeStatus != nil {
				if status := exchangeStatus.Load(); status != 0 {
					w.WriteHeader(int(status))
					io.WriteString(w, `{"error":"unauthorized"}`)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"accessToken":"`+fakeCursorUpstreamToken(n)+
				`","refreshToken":"cursor-upstream-refresh-never-real"}`)
			return
		}
		rpcHits.Add(1)
		b, _ := io.ReadAll(r.Body)
		lastRPC.Store(claudeCapturedRequest{
			method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery,
			header: r.Header.Clone(), body: string(b),
		})
		rpc(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, exchangeHits, rpcHits, lastRPC
}

func TestCursorAuxiliaryRPCForwardsWithoutMeteringOrAdmit(t *testing.T) {
	// 二进制体（非法 UTF-8、含 NUL）钉「不解析、不改写」。
	rawRequest := "\x00\x01\xff\x10" + cursorReqMark
	rawResponse := "\x02\x00\xfe" + cursorRespMark
	srv, exchangeHits, rpcHits, lastRPC := newCursorUpstream(t, nil,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/connect+proto")
			w.Header().Set("X-Cursor-Future", "preserved")
			w.Header().Set("Trailer", "X-Upstream-Trailer")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, rawResponse)
			w.Header().Set("X-Upstream-Trailer", "trailer-preserved")
		})
	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(srv.URL)
	connectCursorCredential(t, e, cursorAPIKeyFake)
	clientToken := exchangeCursorClientToken(t, e)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	// 辅助 RPC 不是新消费：即使计量出口拒绝一切新模型请求，
	// AvailableModels 也应正常转发，且不产生用量样本。
	denyAll(fm, usage.RejectRPM, 7)

	h := cursorHeaders()
	h.Set("Authorization", "Bearer "+clientToken)
	h.Set("x-ghost-mode", "true")
	h.Set("x-request-id", "11111111-2222-3333-4444-555555555555")
	h.Set("x-cursor-client-version", "2026.08.11-e8db854")
	h.Set("Cookie", "session=must-not-leave-device")
	h.Set("X-LlmGate-Private-Control", "must-not-leave-device")
	h.Set("Proxy-Authorization", "Bearer must-not-leave-device")
	h.Set("Connection", "X-Remove-Me")
	h.Set("X-Remove-Me", "must-not-leave-device")
	w := serveClaude(e, http.MethodPost, cursorRPCPath, h, rawRequest, false)
	if w.Code != http.StatusOK || w.Body.String() != rawResponse {
		t.Fatalf("RPC 响应未保真: status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Cursor-Future"); got != "preserved" {
		t.Errorf("未知响应头 = %q，期望 preserved", got)
	}
	if got := w.Result().Trailer.Get("X-Upstream-Trailer"); got != "trailer-preserved" {
		t.Errorf("上游 trailer 未转发，得到 %q", got)
	}

	req := lastRPC.Load().(claudeCapturedRequest)
	if req.method != http.MethodPost || req.path != "/aiserver.v1.AiService/AvailableModels" {
		t.Errorf("上游请求 = %s %s，设备前缀应剥掉", req.method, req.path)
	}
	if req.body != rawRequest {
		t.Fatalf("请求体被改写\n got: %q\nwant: %q", req.body, rawRequest)
	}
	for name, want := range map[string]string{
		"Authorization":            "Bearer " + fakeCursorUpstreamToken(1),
		"Connect-Protocol-Version": "1",
		"Content-Type":             "application/proto",
		"x-cursor-client-type":     "cli",
		"x-cursor-client-version":  "2026.08.11-e8db854",
		"x-ghost-mode":             "true",
		"x-request-id":             "11111111-2222-3333-4444-555555555555",
	} {
		if got := req.header.Get(name); got != want {
			t.Errorf("上游 %s = %q，期望 %q", name, got, want)
		}
	}
	for _, name := range []string{"Cookie", "X-LlmGate-Private-Control", "Proxy-Authorization", "x-api-key", "X-Remove-Me", "Connection"} {
		if got := req.header.Get(name); got != "" {
			t.Errorf("设备边界头 %s 被发往上游: %q", name, got)
		}
	}
	for name, values := range req.header {
		for _, value := range values {
			if strings.Contains(value, testKey) {
				t.Errorf("客户端 Key 出现在上游请求头 %s", name)
			}
		}
	}

	// 第二个 RPC 复用换发缓存：exchange 不再出网。
	w = serveClaude(e, http.MethodPost, cursorRPCPath, cursorHeaders(), rawRequest, false)
	if w.Code != http.StatusOK || exchangeHits.Load() != 1 || rpcHits.Load() != 2 {
		t.Fatalf("换发缓存未生效: status=%d exchange=%d rpc=%d", w.Code, exchangeHits.Load(), rpcHits.Load())
	}
	if fm.count() != 0 {
		t.Fatalf("辅助 RPC 不应进入模型用量，样本数 = %d", fm.count())
	}

	logs := e.logBuf.String()
	for _, forbidden := range []string{cursorAPIKeyFake, cursorUpstreamTok, testKey, cursorReqMark, cursorRespMark, "must-not-leave-device"} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("日志泄露敏感标记 %q", forbidden)
		}
	}
}

func TestCursorAgentRunIsMeteredAndAdmitted(t *testing.T) {
	srv, exchangeHits, rpcHits, _ := newCursorUpstream(t, nil,
		func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "must-not-run") })
	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(srv.URL)
	connectCursorCredential(t, e, cursorAPIKeyFake)
	clientToken := exchangeCursorClientToken(t, e)
	fm := &fakeMeter{}
	denyAll(fm, usage.RejectRPM, 7)
	e.srv.EnableMetering(fm)

	h := cursorHeaders()
	h.Set("Authorization", "Bearer "+clientToken)
	w := serveClaude(e, http.MethodPost,
		"/agents/cursor/"+usage.CursorAgentRunSSERPC, h, "\x00req", false)
	code, _ := decodeConnectError(t, w)
	if w.Code != http.StatusTooManyRequests || code != "resource_exhausted" {
		t.Fatalf("RunSSE 准入拒绝 = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q，期望 7", got)
	}
	if exchangeHits.Load() != 1 || rpcHits.Load() != 0 {
		t.Fatalf("被拒 RunSSE 不应转发: exchange=%d rpc=%d", exchangeHits.Load(), rpcHits.Load())
	}
	sample := fm.only(t)
	if sample.Entry != usage.EntryCursorAgent || sample.ModelName != usage.CursorAgentModelDimension ||
		!sample.Rejected || sample.RejectReason != usage.RejectRPM ||
		sample.Status != http.StatusTooManyRequests {
		t.Errorf("RunSSE 拒绝样本 = %+v", sample)
	}
}

func TestCursorLocalJWTRejectsTamperingAndTracksClientKeyState(t *testing.T) {
	srv, exchangeHits, rpcHits, _ := newCursorUpstream(t, nil,
		func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(srv.URL)
	connectCursorCredential(t, e, cursorAPIKeyFake)
	clientToken := exchangeCursorClientToken(t, e)

	h := cursorHeaders()
	h.Set("Authorization", "Bearer "+clientToken)
	w := serveClaude(e, http.MethodPost, cursorRPCPath, h, "\x00req", false)
	if w.Code != http.StatusOK || exchangeHits.Load() != 1 || rpcHits.Load() != 1 {
		t.Fatalf("本地 JWT 未放行: status=%d exchange=%d rpc=%d",
			w.Code, exchangeHits.Load(), rpcHits.Load())
	}

	// 篡改签名的**首**字符，不动末位：base64url 的末位只承载签名的最后几个有效
	// 位，其余是补齐位，翻它解出来可能还是同一串签名字节——那一步就等于什么也
	// 没改，用例每十来次运行随机变绿一次。首字符的六位全是有效位。
	sig := strings.LastIndexByte(clientToken, '.') + 1
	flip := byte('A')
	if clientToken[sig] == flip {
		flip = 'B'
	}
	tampered := clientToken[:sig] + string(flip) + clientToken[sig+1:]
	h.Set("Authorization", "Bearer "+tampered)
	w = serveClaude(e, http.MethodPost, cursorRPCPath, h, "\x00req", false)
	if code, _ := decodeConnectError(t, w); w.Code != http.StatusUnauthorized || code != "unauthenticated" {
		t.Fatalf("篡改 JWT 响应 = %d %s", w.Code, w.Body.String())
	}
	if rpcHits.Load() != 1 {
		t.Fatalf("篡改 JWT 不应出网，rpc=%d", rpcHits.Load())
	}

	keys, err := e.st.ListAPIKeys(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v (%d)", err, len(keys))
	}
	if err := e.st.SetAPIKeyDisabled(t.Context(), keys[0].ID, true); err != nil {
		t.Fatal(err)
	}
	h.Set("Authorization", "Bearer "+clientToken)
	w = serveClaude(e, http.MethodPost, cursorRPCPath, h, "\x00req", false)
	if code, _ := decodeConnectError(t, w); w.Code != http.StatusUnauthorized || code != "unauthenticated" {
		t.Fatalf("禁用 Key 后 JWT 响应 = %d %s", w.Code, w.Body.String())
	}
	if rpcHits.Load() != 1 {
		t.Fatalf("禁用 Key 后 JWT 不应出网，rpc=%d", rpcHits.Load())
	}
}

// TestCursorRPCStreamsIncrementally 用真监听验证流式响应逐块即时 flush：
// 上游发出首块后阻塞，客户端必须在上游发完之前就读到首块。
func TestCursorRPCStreamsIncrementally(t *testing.T) {
	const chunk1 = "\x00frame-one-"
	const chunk2 = "\x00frame-two-"
	release := make(chan struct{})
	srv, _, _, _ := newCursorUpstream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, chunk1)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, chunk2)
	})
	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(srv.URL)
	connectCursorCredential(t, e, cursorAPIKeyFake)

	front := httptest.NewServer(e.h)
	t.Cleanup(front.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+cursorRPCPath, strings.NewReader("\x00req"))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range cursorHeaders() {
		req.Header[name] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("RPC = %d %q", resp.StatusCode, b)
	}
	buf := make([]byte, len(chunk1))
	if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != chunk1 {
		t.Fatalf("上游未发完前应已读到首块: %q err=%v", buf, err)
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil || string(rest) != chunk2 {
		t.Fatalf("剩余块 = %q err=%v", rest, err)
	}
}

// TestCursorAgentRPCStreamsBidirectionally 钉 agent.v1 不能退化成先整读请求再
// 转发：客户端请求 pipe 保持打开时，上游已经读到首帧且客户端已经收到首个
// 响应帧；随后第二段请求与响应继续通过同一条 Connect 流。
func TestCursorAgentRPCStreamsBidirectionally(t *testing.T) {
	const (
		agentPath = "/agents/cursor/agent.v1.AgentService/Run"
		reqPart1  = "\x00agent-request-one"
		reqPart2  = "\x00agent-request-two"
		respPart1 = "\x00agent-response-one"
		respPart2 = "\x00agent-response-two"
	)
	firstRequestRead := make(chan struct{})
	restRequestRead := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/exchange_user_api_key":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"accessToken":"`+fakeCursorUpstreamToken(1)+`","refreshToken":"unused"}`)
		case "/agent.v1.AgentService/Run":
			if got := r.Header.Get("Authorization"); got != "Bearer "+fakeCursorUpstreamToken(1) {
				t.Errorf("Agent Authorization = %q", got)
			}
			if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
				t.Errorf("假上游启用全双工: %v", err)
				return
			}
			buf := make([]byte, len(reqPart1))
			if _, err := io.ReadFull(r.Body, buf); err != nil || string(buf) != reqPart1 {
				t.Errorf("Agent 首段请求 = %q err=%v", buf, err)
				return
			}
			close(firstRequestRead)
			w.Header().Set("Content-Type", "application/connect+proto")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, respPart1)
			w.(http.Flusher).Flush()
			rest, err := io.ReadAll(r.Body)
			if err != nil || string(rest) != reqPart2 {
				t.Errorf("Agent 剩余请求 = %q err=%v", rest, err)
				return
			}
			close(restRequestRead)
			io.WriteString(w, respPart2)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(upstream.URL)
	connectCursorCredential(t, e, cursorAPIKeyFake)
	clientToken := exchangeCursorClientToken(t, e)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	front := httptest.NewServer(e.h)
	t.Cleanup(front.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	requestReader, requestWriter := io.Pipe()
	t.Cleanup(func() { requestWriter.Close() })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+agentPath, requestReader)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range cursorHeaders() {
		req.Header[name] = values
	}
	req.Header.Set("Authorization", "Bearer "+clientToken)
	responseCh := make(chan *http.Response, 1)
	errorCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errorCh <- err
			return
		}
		responseCh <- resp
	}()

	if _, err := requestWriter.Write([]byte(reqPart1)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstRequestRead:
	case err := <-errorCh:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("上游未在请求结束前读到首段")
	}
	var resp *http.Response
	select {
	case resp = <-responseCh:
	case err := <-errorCh:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("客户端未在请求结束前收到响应头")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Agent RPC = %d %q", resp.StatusCode, body)
	}
	firstResponse := make([]byte, len(respPart1))
	if _, err := io.ReadFull(resp.Body, firstResponse); err != nil || string(firstResponse) != respPart1 {
		t.Fatalf("Agent 首段响应 = %q err=%v", firstResponse, err)
	}
	if _, err := requestWriter.Write([]byte(reqPart2)); err != nil {
		t.Fatal(err)
	}
	if err := requestWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restRequestRead:
	case <-ctx.Done():
		t.Fatal("上游未收到剩余请求段")
	}
	restResponse, err := io.ReadAll(resp.Body)
	if err != nil || string(restResponse) != respPart2 {
		t.Fatalf("Agent 剩余响应 = %q err=%v", restResponse, err)
	}

	sample := fm.only(t)
	if sample.Entry != usage.EntryCursorAgent || sample.ModelName != usage.CursorAgentModelDimension ||
		sample.Status != http.StatusOK || sample.Attempts != 1 {
		t.Errorf("Agent 计量维度不符: %+v", sample)
	}
	logs := e.logBuf.String()
	for _, forbidden := range []string{reqPart1, reqPart2, respPart1, respPart2, cursorAPIKeyFake, testKey} {
		if strings.Contains(logs, forbidden) {
			t.Errorf("Agent 日志泄露敏感标记 %q", forbidden)
		}
	}
}

func TestCursorUpstream401RetriesOnceThenLatches(t *testing.T) {
	const upstream401Body = `{"code":"unauthenticated","message":"upstream exact wording"}`

	t.Run("401 后作废缓存重新换发并单次重试", func(t *testing.T) {
		srv, exchangeHits, rpcHits, _ := newCursorUpstream(t, nil, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") == "Bearer "+fakeCursorUpstreamToken(1) {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, upstream401Body)
				return
			}
			io.WriteString(w, "ok-after-retry")
		})
		e := newRouteEnv(t)
		e.srv.SetCursorEndpoints(srv.URL)
		id := connectCursorCredential(t, e, cursorAPIKeyFake)

		w := serveClaude(e, http.MethodPost, cursorRPCPath, cursorHeaders(), "\x00req", false)
		if w.Code != http.StatusOK || w.Body.String() != "ok-after-retry" ||
			exchangeHits.Load() != 2 || rpcHits.Load() != 2 {
			t.Fatalf("重试未按预期: status=%d body=%q exchange=%d rpc=%d",
				w.Code, w.Body.String(), exchangeHits.Load(), rpcHits.Load())
		}
		acct, err := e.st.GetAgentAccount(t.Context(), id)
		if err != nil || acct.Status != store.AgentStatusActive {
			t.Fatalf("单次重试成功不该动状态: %v %v", err, acct)
		}
	})

	t.Run("二次 401 原样透传并落 auth_expired", func(t *testing.T) {
		srv, exchangeHits, rpcHits, _ := newCursorUpstream(t, nil, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, upstream401Body)
		})
		e := newRouteEnv(t)
		e.srv.SetCursorEndpoints(srv.URL)
		id := connectCursorCredential(t, e, cursorAPIKeyFake)
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)

		w := serveClaude(e, http.MethodPost, cursorRPCPath, cursorHeaders(), "\x00req", false)
		if w.Code != http.StatusUnauthorized || w.Body.String() != upstream401Body {
			t.Fatalf("二次 401 未原样透传: status=%d body=%q", w.Code, w.Body.String())
		}
		acct, err := e.st.GetAgentAccount(t.Context(), id)
		if err != nil || acct.Status != store.AgentStatusAuthExpired {
			t.Fatalf("二次 401 后状态 = %v (err=%v)", acct, err)
		}
		if fm.count() != 0 {
			t.Errorf("辅助 RPC 的上游 401 不应进模型用量，样本数 = %d", fm.count())
		}

		// 落闩后：下一个 RPC 不再出网，Connect 风格 failed_precondition。
		w = serveClaude(e, http.MethodPost, cursorRPCPath, cursorHeaders(), "\x00req", false)
		code, msg := decodeConnectError(t, w)
		if w.Code != http.StatusConflict || code != "failed_precondition" || !strings.Contains(msg, "reconnect") {
			t.Fatalf("落闩后响应 = %d %s", w.Code, w.Body.String())
		}
		if exchangeHits.Load() != 2 || rpcHits.Load() != 2 {
			t.Fatalf("落闩后仍出网: exchange=%d rpc=%d", exchangeHits.Load(), rpcHits.Load())
		}
	})
}

func TestCursorExchangeRejectedByUpstreamLatchesAuthExpired(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var exchangeStatus atomic.Int64
			exchangeStatus.Store(int64(status))
			srv, exchangeHits, rpcHits, _ := newCursorUpstream(t, &exchangeStatus,
				func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "never") })
			e := newRouteEnv(t)
			e.srv.SetCursorEndpoints(srv.URL)
			id := connectCursorCredential(t, e, cursorAPIKeyFake)

			w := serveClaude(e, http.MethodPost, cursorRPCPath, cursorHeaders(), "\x00req", false)
			code, _ := decodeConnectError(t, w)
			if w.Code != http.StatusConflict || code != "failed_precondition" {
				t.Fatalf("exchange 被拒响应 = %d %s", w.Code, w.Body.String())
			}
			acct, err := e.st.GetAgentAccount(t.Context(), id)
			if err != nil || acct.Status != store.AgentStatusAuthExpired {
				t.Fatalf("exchange 被拒后状态 = %v (err=%v)", acct, err)
			}
			if exchangeHits.Load() != 1 || rpcHits.Load() != 0 {
				t.Fatalf("exchange 被拒不该转发 RPC: exchange=%d rpc=%d", exchangeHits.Load(), rpcHits.Load())
			}
		})
	}
}

func TestCursorUnknownPathsStayLocal404(t *testing.T) {
	upstream := cursorDenyUpstream(t)
	e := newRouteEnv(t)
	e.srv.SetCursorEndpoints(upstream.URL)
	connectCursorCredential(t, e, cursorAPIKeyFake)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/agents/cursor/auth/poll"},
		{http.MethodPost, "/agents/cursor/repo42/anything"},
		{http.MethodPost, "/agents/cursor/aiserver.v1.AiService/AvailableModels/extra"},
		{http.MethodPost, "/agents/cursor/aiserver.v2.AiService/AvailableModels"},
		{http.MethodGet, cursorRPCPath},
		{http.MethodPost, "/agents/cursor/"},
	} {
		w := serveClaude(e, tc.method, tc.path, cursorHeaders(), "", false)
		code, _ := decodeConnectError(t, w)
		if w.Code != http.StatusNotFound || code != "not_found" {
			t.Errorf("%s %s = %d %s，期望本地 404", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestCursorRefreshAgentSelfCheck(t *testing.T) {
	t.Run("成功换发盖最近刷新", func(t *testing.T) {
		srv, exchangeHits, _, _ := newCursorUpstream(t, nil,
			func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
		e := newRouteEnv(t)
		e.srv.SetCursorEndpoints(srv.URL)
		id := connectCursorCredential(t, e, cursorAPIKeyFake)
		before, err := e.st.GetAgentAccount(t.Context(), id)
		if err != nil || !before.LastRefreshAt.IsZero() {
			t.Fatalf("初始 LastRefreshAt 应为零值: %v (err=%v)", before, err)
		}

		if err := e.srv.RefreshAgent(t.Context(), store.AgentProviderCursor); err != nil {
			t.Fatalf("RefreshAgent: %v", err)
		}
		after, err := e.st.GetAgentAccount(t.Context(), id)
		if err != nil || after.LastRefreshAt.IsZero() || after.Status != store.AgentStatusActive {
			t.Fatalf("自检成功后 = %v (err=%v)", after, err)
		}
		// 自检是强制往返：即使已有缓存 token 也要真打一次 exchange。
		if err := e.srv.RefreshAgent(t.Context(), store.AgentProviderCursor); err != nil {
			t.Fatalf("第二次 RefreshAgent: %v", err)
		}
		if exchangeHits.Load() != 2 {
			t.Fatalf("自检应逐次真打 exchange，命中 = %d", exchangeHits.Load())
		}
	})

	t.Run("被上游拒绝落 auth_expired", func(t *testing.T) {
		var exchangeStatus atomic.Int64
		exchangeStatus.Store(http.StatusUnauthorized)
		srv, _, _, _ := newCursorUpstream(t, &exchangeStatus,
			func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "never") })
		e := newRouteEnv(t)
		e.srv.SetCursorEndpoints(srv.URL)
		id := connectCursorCredential(t, e, cursorAPIKeyFake)

		err := e.srv.RefreshAgent(t.Context(), store.AgentProviderCursor)
		// 判据与管理面 refreshResult 相同：errors.Is(err, agentauth.ErrAuthExpired)。
		if err == nil || !errors.Is(err, agentauth.ErrAuthExpired) {
			t.Fatalf("自检被拒应报登录失效: %v", err)
		}
		acct, gerr := e.st.GetAgentAccount(t.Context(), id)
		if gerr != nil || acct.Status != store.AgentStatusAuthExpired {
			t.Fatalf("自检被拒后状态 = %v (err=%v)", acct, gerr)
		}
	})
}
