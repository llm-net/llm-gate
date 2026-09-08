// responses_grok_test.go：/v1/responses 的 Grok Build 半边——model 前缀分流、
// X-XAI-Token-Auth 注入纪律（设备值顶掉客户端值）、401 刷新重试与刷新请求形态。
//
// 透传/回写/计量的共用核心已由 codex 侧用例（responses_test.go）钉死，这里只
// 测 grok 的分岔点，不重复全链路。夹具全部是假凭据。
package gateway_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/grokauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// Cancel after authentication and beginEntry, before the subscription lookup.
// Grok's auxiliary calls can end here without sending anything upstream.
type cancelOnAdmitMeter struct {
	*fakeMeter
	cancel context.CancelFunc
}

func (m *cancelOnAdmitMeter) Admit(store.KeyAuth) usage.Decision {
	m.cancel()
	return usage.Decision{Allowed: true}
}

func TestResponsesGrokCanceledBeforeUpstreamDoesNotEstimate(t *testing.T) {
	e := newGrokEnv(t, func(http.ResponseWriter, *http.Request) {
		t.Error("canceled request reached upstream")
	}, func(http.ResponseWriter, *http.Request) {
		t.Error("canceled request refreshed credentials")
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := &cancelOnAdmitMeter{fakeMeter: &fakeMeter{}, cancel: cancel}
	e.srv.EnableMetering(m)
	r := httptest.NewRequest("POST", "/agents/grok/v1/responses", strings.NewReader(grokReq(grokModel))).WithContext(ctx)
	for name, value := range grokClientHeaders {
		r.Header.Set(name, value)
	}
	e.h.ServeHTTP(httptest.NewRecorder(), r)
	sample := m.only(t)
	if sample.Status != 499 || sample.Attempts != 0 || sample.UpstreamName != "" || sample.Estimated || sample.Tokens != (usage.Tokens{}) {
		t.Fatalf("canceled request must be an unbilled error: %+v", sample)
	}
	if sample.ModelName != grokModel || sample.Entry != usage.EntryResponsesAgents {
		t.Fatalf("request dimensions lost: %+v", sample)
	}
	if !strings.Contains(e.logBuf.String(), `"status":499`) {
		t.Fatal("access log did not record client cancellation")
	}
}

const (
	grokModel        = "grok-4.5"
	grokBackendModel = "grok-4.5-0709"
	grokAccess1      = "grok-access-token-1"
	grokAccess2      = "grok-access-token-2"
	grokRefresh1     = "grok-refresh-token-1"
	grokRefresh2     = "grok-refresh-token-2"

	// grokBackendPath 是订阅后端 Responses 端点在假后端上的路径
	// （真地址是 https://api.x.ai/v1/responses）。
	grokBackendPath = "/agents/v1/responses"

	// grokTokenAuthName 是 xAI 标记「用户 OAuth 令牌」的那个头（生产侧同名
	// 常量不导出，用例在包外自带一份——两处写法要一致）。
	grokTokenAuthName = "X-XAI-Token-Auth"
)

// grokAuthJSONFixture 造一份形如 ~/.grok/auth.json 的句柄（单条 OAuth 记录，
// 规范键；不带 expires_at——到期时刻未知按有效处理，happy path 不打刷新）。
func grokAuthJSONFixture(access, refresh string) string {
	return fmt.Sprintf(`{"https://auth.x.ai::%s":{"key":%q,"refresh_token":%q,`+
		`"auth_mode":"oidc","user_id":"user-grok-test","email":"admin@example.invalid"}}`,
		grokauth.ClientID, access, refresh)
}

// grokStubIssuer 是假 auth.x.ai：只认 POST /oauth2/token（路径与 codex 的
// /oauth/token 不同，所以不复用 stubIssuer）。
type grokStubIssuer struct {
	url string
	mu  sync.Mutex
	// forms 是每次刷新提交的表单（断言 grant_type/refresh_token/client_id 用）。
	forms []url.Values
}

func newGrokStubIssuer(t *testing.T, reply http.HandlerFunc) *grokStubIssuer {
	t.Helper()
	s := &grokStubIssuer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth2/token" {
			t.Errorf("打到了意料之外的签发方路径 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = r.ParseForm()
		s.mu.Lock()
		s.forms = append(s.forms, r.PostForm)
		s.mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *grokStubIssuer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.forms)
}

func (s *grokStubIssuer) form(i int) url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.forms) {
		return nil
	}
	return s.forms[i]
}

// grokEnv 装配「一个已连接的 Grok Build 订阅 + 假后端 + 假签发方」。
type grokEnv struct {
	*routeEnv
	backend *stubUpstream
	issuer  *grokStubIssuer
	acctID  int64
}

func newGrokEnv(t *testing.T, respond, refresh http.HandlerFunc) *grokEnv {
	t.Helper()
	e := newRouteEnv(t)
	backend := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != grokBackendPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		respond(w, r)
	})
	issuer := newGrokStubIssuer(t, refresh)
	e.srv.SetGrokEndpoints(issuer.url, stubRoot(backend)+grokBackendPath)

	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider:     store.AgentProviderGrok,
		Label:        "Grok 订阅",
		AccountID:    "admin@example.invalid",
		DefaultModel: grokModel,
		AuthJSON:     grokAuthJSONFixture(grokAccess1, grokRefresh1),
	})
	if err != nil {
		t.Fatalf("UpsertAgentAccount: %v", err)
	}
	return &grokEnv{routeEnv: e, backend: backend, issuer: issuer, acctID: acct.ID}
}

// grokReq 是一条 grok CLI（api_backend = "responses"）形态的请求体。
func grokReq(model string) string {
	return fmt.Sprintf(`{"model":%q,"instructions":"You are a coding agent.",`+
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"写个快排"}]}],`+
		`"stream":false,"store":false}`, model)
}

// grokClientHeaders 是客户端头：sk_ API密钥 + grok 自带的身份头（必须原样到
// 后端）+ 一个客户端**伪造的** X-XAI-Token-Auth（必须被设备的值顶掉）。
var grokClientHeaders = map[string]string{
	"Authorization":         "Bearer " + testKey,
	"Content-Type":          "application/json",
	"x-grok-client-version": "9.9.9-test",
	"X-XAI-Token-Auth":      "fake-client-forged-value",
	"Accept-Encoding":       "br, zstd",
}

const grokNonStreamBody = `{"id":"resp_g1","object":"response","status":"completed",` +
	`"model":"` + grokBackendModel + `","output":[{"type":"message","id":"msg_g1","role":"assistant",` +
	`"content":[{"type":"output_text","text":"ok"}]}],` +
	`"usage":{"input_tokens":30,"input_tokens_details":{"cached_tokens":10},` +
	`"output_tokens":7,"total_tokens":37}}`

// TestResponsesGrokFullPath：grok 半边的全链路（非流式）——注入订阅令牌 +
// X-XAI-Token-Auth（且**顶掉**客户端伪造的那份、只此一个值）、grok 身份头
// 原样过、sk_ Key 不外传、model 请求侧不改写 / 响应侧回写为请求名。
func TestResponsesGrokFullPath(t *testing.T) {
	e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))

	w := do(e.h, "POST", "/agents/v1/responses", grokClientHeaders, grokReq(grokModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if got := bodyModel(t, w); got != grokModel {
		t.Errorf("响应 model = %q，期望回写为 %q", got, grokModel)
	}
	if strings.Contains(w.Body.String(), grokBackendModel) {
		t.Errorf("响应泄露了后端侧模型名: %s", w.Body.String())
	}

	if e.backend.count() != 1 {
		t.Fatalf("后端收到 %d 次请求，期望 1", e.backend.count())
	}
	if got := e.backend.sentAuth(0); got != "Bearer "+grokAccess1 {
		t.Errorf("后端收到 Authorization = %q，期望注入订阅令牌", got)
	}
	// 设备值顶掉客户端伪造值，且**只有一个值**（Get 分不出「顶掉」与「并排」，
	// 必须数条数）。
	if got := e.backend.sentHeaderValues(0, grokTokenAuthName); len(got) != 1 || got[0] != "xai-grok-cli" {
		t.Errorf("后端收到 %s = %v，期望恰好一个设备值 xai-grok-cli", grokTokenAuthName, got)
	}
	// grok 没有账号头概念：codex 的账号头不得被本路注入。
	if got := e.backend.sentHeaderValue(0, codexAccountHeaderName); got != "" {
		t.Errorf("后端收到 ChatGPT-Account-ID = %q，grok 这条路不该注入它", got)
	}
	if got := e.backend.sentHeaderValue(0, "x-grok-client-version"); got != "9.9.9-test" {
		t.Errorf("后端收到 x-grok-client-version = %q，期望身份头原样透传", got)
	}
	if got := e.backend.sentModel(0); got != grokModel {
		t.Errorf("后端收到 model = %q，期望原样透传（这条路没有来源侧 ID）", got)
	}
	if strings.Contains(e.backend.sentAuth(0), testKey) {
		t.Error("客户端的 sk_ Key 绝不能外传给后端")
	}
	if got := e.backend.sentHeaderValue(0, "Accept-Encoding"); strings.Contains(got, "br") {
		t.Errorf("后端收到 Accept-Encoding = %q，客户端那份应被剥掉", got)
	}
	if e.issuer.count() != 0 {
		t.Errorf("happy path 不该发生刷新，实际 %d 次", e.issuer.count())
	}
}

// TestResponsesGrokDispatchByModel：分流契约（docs/firmware-agents-grok.md）。
// 只连了 grok 的设备：grok-* 通、gpt-* 回 409 且指名 Codex；大小写不敏感。
// 只连了 codex 的设备（复用 codex 环境）：grok-* 回 409 且指名 Grok Build。
func TestResponsesGrokDispatchByModel(t *testing.T) {
	e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))

	// 大小写不敏感：Grok-4.5 也归 grok。
	w := do(e.h, "POST", "/agents/v1/responses", grokClientHeaders, grokReq("Grok-4.5"))
	if w.Code != http.StatusOK {
		t.Fatalf("Grok-4.5 状态码 = %d，期望分流到 grok 并成功；body: %s", w.Code, w.Body.String())
	}

	w = do(e.h, "POST", "/agents/v1/responses", grokClientHeaders, grokReq(codexModel))
	if w.Code != http.StatusConflict {
		t.Fatalf("gpt-* 在只连 grok 的设备上状态码 = %d，期望 409", w.Code)
	}
	_, code, msg := decodeError(t, w)
	if code != "agent_not_configured" || !strings.Contains(msg, "Codex") {
		t.Errorf("错误 = %q %q，期望 agent_not_configured 且指名 Codex", code, msg)
	}

	// 反向：只连 codex 的设备上打 grok 模型。
	ce := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	w = do(ce.h, "POST", "/agents/v1/responses", codexAuth, grokReq(grokModel))
	if w.Code != http.StatusConflict {
		t.Fatalf("grok-* 在只连 codex 的设备上状态码 = %d，期望 409", w.Code)
	}
	_, code, msg = decodeError(t, w)
	if code != "agent_not_configured" || !strings.Contains(msg, "Grok Build") {
		t.Errorf("错误 = %q %q，期望 agent_not_configured 且指名 Grok Build", code, msg)
	}
}

// TestResponsesGrokRefreshOn401：后端拒了当前令牌 → 作废这一代、刷新一次、
// 带新令牌重发（只看状态码）。顺带钉死刷新请求形态：grant_type/refresh_token/
// client_id 三参、**不带 scope**（官方 CLI 的 refresh_tokens_once 同形）。
func TestResponsesGrokRefreshOn401(t *testing.T) {
	var backendCalls int
	var mu sync.Mutex
	e := newGrokEnv(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backendCalls++
		n := backendCalls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_token"}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+grokAccess2 {
			t.Errorf("重试请求 Authorization = %q，期望新一代令牌", got)
		}
		jsonReply(http.StatusOK, grokNonStreamBody)(w, r)
	}, issuerRotates(grokAccess2, grokRefresh2))

	w := do(e.h, "POST", "/agents/v1/responses", grokClientHeaders, grokReq(grokModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 401 后刷新重试成功；body: %s", w.Code, w.Body.String())
	}
	if e.issuer.count() != 1 {
		t.Fatalf("刷新 %d 次，期望 1", e.issuer.count())
	}
	form := e.issuer.form(0)
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != grokRefresh1 ||
		form.Get("client_id") != grokauth.ClientID {
		t.Errorf("刷新表单形态不对: %v", form)
	}
	if got := form.Get("scope"); got != "" {
		t.Errorf("grok 刷新不该带 scope, got %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if backendCalls != 2 {
		t.Errorf("后端收到 %d 次请求，期望 2（401 + 重试）", backendCalls)
	}
}
