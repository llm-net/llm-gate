// responses_test.go 是 Codex 订阅代理的假后端全链路验收。用例经兼容别名
// POST /agents/v1/responses（按 model 前缀分流）驱动——别名与正式接入面
// /agents/codex/v1/responses 接同一段处理器（agents_faces_test.go 另钉正式面
// 与闸门），所以这里既验转发核心，也验别名仍在。
//
// 覆盖面（计划逐条）：SSE 逐块转发且即时 Flush、response.created /
// response.completed 的 model 回写、usage 三字段（input_tokens /
// output_tokens / input_tokens_details.cached_tokens）入账、llmgate Key 鉴权与
// 预算准入生效、未连接账号回 agent_not_configured、账号失效回
// agent_auth_expired（都非 5xx），以及后端 401 之后「作废这一代 → 刷新 →
// 重发一次」的两条分支（刷新成功 / 被确定性拒绝）。
//
// 编排方式沿 images_test.go：httptest 假后端 + 假签发方（auth.openai.com），
// 经 SetAgentEndpoints 指过去，断言「客户端看到什么」与「后端收到什么」。
// 用例里的令牌**缺省是假串、不是 JWT**——Provider 因此解不出到期时刻、按有效
// 处理，happy path 一次网络都不会打到签发方。要看刷新真的发生的那条用例
// （世代只进不退）必须改用真 JWT，理由见 [codexJWT]。
package gateway_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const (
	// codexModel 是客户端请求的模型名；codexBackendModel 是后端回显的那个
	// 带版本后缀的名字——回写目标可证（客户端绝不该看到后缀）。
	codexModel        = "gpt-5-codex"
	codexBackendModel = "gpt-5-codex-2026-01-01"
	codexAccountID    = "acct-codex-test"
	codexAccess1      = "codex-access-token-1"
	codexAccess2      = "codex-access-token-2"
	codexRefresh1     = "codex-refresh-token-1"
	codexRefresh2     = "codex-refresh-token-2"

	// codexBackendPath 是订阅后端 Responses 端点在假后端上的路径
	// （真地址是 https://chatgpt.com/backend-api/codex/responses）。
	codexBackendPath = "/backend-api/codex/responses"

	// codexAccountHeaderName 是订阅后端选账号的那个头（生产侧同名常量不导出，
	// 用例在包外，只能自带一份——两处写法要一致）。
	codexAccountHeaderName = "ChatGPT-Account-ID"
)

// codexAuthJSON 造一份形如 ~/.codex/auth.json 的句柄（带夹具的 account_id）。
func codexAuthJSON(access, refresh string) string {
	return codexAuthJSONFor(access, refresh, codexAccountID)
}

// codexAuthJSONFor 同上，但显式给定 account_id；**空 = tokens 段里根本没有这一
// 项**，而句柄也不带 id_token，于是 claim 那条路同样解不出值。
//
// 这不是一个假想形态：粘贴 auth.json 兜底那条路常常就是这个样子，而
// id_token 到底带不带 chatgpt_account_id 是决策 4 至今未验的那半段。
func codexAuthJSONFor(access, refresh, accountID string) string {
	tokens := fmt.Sprintf(`"access_token":%q,"refresh_token":%q`, access, refresh)
	if accountID != "" {
		tokens += fmt.Sprintf(`,"account_id":%q`, accountID)
	}
	return fmt.Sprintf(`{"OPENAI_API_KEY":null,"tokens":{%s},"last_refresh":"2026-08-11T00:00:00Z"}`, tokens)
}

// codexJWT 造一个只带 exp claim 的 JWT（不签名——codexauth 只解 claim 不验签）。
//
// **令牌是不是真 JWT 会改变行为**：解不出 exp 时 Provider 按「到期时刻未知 =
// 有效」处理，本该触发刷新的路径于是静默变成「直接拿来用」。本文件其余用例正是
// 靠这一点让 happy path 一次网络都不打；反过来，任何要观察「刷新真的发生了」的
// 用例都必须用真 JWT，否则断言看着在跑、其实一步都没走到。
func codexJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": exp.Unix()})
	if err != nil {
		t.Fatalf("造 JWT 载荷: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc(payload) + ".not-a-signature"
}

// codexStreamBody 是一段按 Responses 事件形态构造的 SSE：
// 带 response 快照的两帧（created / completed）各有一个要回写的 model，
// 增量帧、未知事件帧与非 JSON 的 data 行则必须逐字节原样过去。
var codexStreamBody = strings.Join([]string{
	"event: response.created",
	`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1",` +
		`"object":"response","status":"in_progress","model":"` + codexBackendModel + `","output":[]}}`,
	"",
	": ping",
	"",
	"event: response.output_text.delta",
	`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,` +
		`"content_index":0,"delta":"你好，世界"}`,
	"",
	"event: response.custom_unknown",
	`data: {"type":"response.custom_unknown","payload":{"weird":true}}`,
	"",
	"event: response.completed",
	`data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_1",` +
		`"object":"response","status":"completed","model":"` + codexBackendModel + `",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant",` +
		`"content":[{"type":"output_text","text":"你好，世界"}]}],` +
		`"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":900},` +
		`"output_tokens":42,"output_tokens_details":{"reasoning_tokens":10},"total_tokens":1242}}}`,
	"",
	"data: [DONE]",
	"",
}, "\n")

// codexNonStreamBody 是非流式应答：顶层就是 response 对象，model 在顶层。
const codexNonStreamBody = `{"id":"resp_2","object":"response","status":"completed",` +
	`"model":"` + codexBackendModel + `","output":[{"type":"message","id":"msg_2","role":"assistant",` +
	`"content":[{"type":"output_text","text":"ok"}]}],` +
	`"usage":{"input_tokens":30,"input_tokens_details":{"cached_tokens":10},` +
	`"output_tokens":7,"total_tokens":37}}`

// ---- 假签发方（auth.openai.com 的令牌端点） ----

type stubIssuer struct {
	url string
	mu  sync.Mutex
	// forms 是每次刷新提交的表单（断言 grant_type/refresh_token 用）。
	forms []url.Values
}

// newStubIssuer 起一个只认 POST /oauth/token 的假签发方；reply 决定应答。
func newStubIssuer(t *testing.T, reply http.HandlerFunc) *stubIssuer {
	t.Helper()
	s := &stubIssuer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.forms = append(s.forms, r.PostForm)
		s.mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *stubIssuer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.forms)
}

func (s *stubIssuer) form(i int) url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.forms) {
		return nil
	}
	return s.forms[i]
}

// issuerNever 是"这个用例根本不该发生刷新"的应答：真被打到就当场记一笔错。
func issuerNever(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("不该发生刷新：令牌未过期也未被拒绝")
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// issuerRotates 回一代新令牌。
func issuerRotates(access, refresh string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"id_token":"","expires_in":3600,`+
			`"token_type":"Bearer"}`, access, refresh)
	}
}

// issuerRotatingOnce 是**轮换型** refresh token 的假签发方：gens 说明「拿这一份
// 能换到哪一代」，而每一份**只换得动一次**，再拿它来换就是 invalid_grant
// （确定性拒绝）。真实签发方就是这么做的（决策 1 的整个论证都压在这上面），
// 也正是「世代倒退」之所以致命的原因——倒回去的那一代手上那份早已被消费掉。
func issuerRotatingOnce(gens map[string][2]string) http.HandlerFunc {
	var mu sync.Mutex
	spent := make(map[string]bool)
	return func(w http.ResponseWriter, r *http.Request) {
		rt := r.PostFormValue("refresh_token")
		mu.Lock()
		defer mu.Unlock()
		next, ok := gens[rt]
		if !ok || spent[rt] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token already used"}`))
			return
		}
		spent[rt] = true
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"id_token":"","expires_in":3600,`+
			`"token_type":"Bearer"}`, next[0], next[1])
	}
}

// ---- 用例环境 ----

type agentEnv struct {
	*routeEnv
	backend *stubUpstream
	issuer  *stubIssuer
	acctID  int64
}

// newAgentEnv 装配「一个已连接的 Codex 订阅 + 假后端 + 假签发方」。
func newAgentEnv(t *testing.T, respond http.HandlerFunc) *agentEnv {
	t.Helper()
	return newAgentEnvWithIssuer(t, respond, issuerNever(t))
}

func newAgentEnvWithIssuer(t *testing.T, respond, refresh http.HandlerFunc) *agentEnv {
	t.Helper()
	e := newRouteEnv(t)
	backend := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		// 路径钉死：网关应打 SetAgentEndpoints 给的那条整地址。
		if r.Method != http.MethodPost || r.URL.Path != codexBackendPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		respond(w, r)
	})
	issuer := newStubIssuer(t, refresh)
	e.srv.SetAgentEndpoints(issuer.url, stubRoot(backend)+codexBackendPath)

	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider:     store.AgentProviderCodex,
		Label:        "订阅账号",
		AccountID:    codexAccountID,
		DefaultModel: codexModel,
		AuthJSON:     codexAuthJSON(codexAccess1, codexRefresh1),
	})
	if err != nil {
		t.Fatalf("UpsertAgentAccount: %v", err)
	}
	e.srv.SetAgentModels(&fakeAgentModels{owners: map[string]string{
		codexModel: store.AgentProviderCodex,
	}})
	return &agentEnv{routeEnv: e, backend: backend, issuer: issuer, acctID: acct.ID}
}

// stubRoot 去掉 newStub 附加的 /v1 后缀：Agents 那条路要的是整条端点地址，
// 不是"端点根 + 相对路径"。
func stubRoot(s *stubUpstream) string { return strings.TrimSuffix(s.url, "/v1") }

// sentHeaderValue 返回第 i 次请求的某个头（stubUpstream.sentAuth 的通用版）。
func (s *stubUpstream) sentHeaderValue(i int, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.headers) {
		return ""
	}
	return s.headers[i].Get(name)
}

// sentHeaderValues 返回第 i 次请求某个头的**全部**取值。条数本身就是断言对象：
// 「设备的值顶掉了客户端那份」与「两个值并排送了过去」在 Get 眼里长得一样。
func (s *stubUpstream) sentHeaderValues(i int, name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.headers) {
		return nil
	}
	return s.headers[i].Values(name)
}

// codexReq 是一条客户端请求体（codex 以自定义 provider 模式发出的那种形态）。
func codexReq(stream bool) string {
	return fmt.Sprintf(`{"model":%q,"instructions":"You are a coding agent.",`+
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"写个快排"}]}],`+
		`"stream":%t,"store":false}`, codexModel, stream)
}

// codexAuth 是客户端头：sk_ API密钥 + codex 自带的身份头（必须原样到后端）
// + 一个网关必须**剥掉**的 Accept-Encoding（留着它，客户端协商出的 br/zstd
// 会让 model 回写静默失效）。
var codexAuth = map[string]string{
	"Authorization":   "Bearer " + testKey,
	"Content-Type":    "application/json",
	"originator":      "codex_cli_rs",
	"session_id":      "11111111-2222-3333-4444-555555555555",
	"OpenAI-Beta":     "responses=experimental",
	"Accept-Encoding": "br, zstd",
}

// sseData 取出 SSE 里全部 data: 行的载荷原文（按出现序）。
func sseData(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		rest, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		out = append(out, strings.TrimPrefix(rest, " "))
	}
	return out
}

// eventModel 解析一条 SSE 载荷并取 response.model（不是事件帧则返回空串）。
func eventModel(payload string) string {
	var m struct {
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(payload), &m) != nil {
		return ""
	}
	return m.Response.Model
}

// ---- SSE 全链路 ----

// TestResponsesStreamFullPath：流式全链路。请求侧 model 不改写、codex 身份头
// 原样过、客户端 Key 换成订阅令牌并带上账号头；响应侧 response.model 两帧都
// 回写成客户端请求名，其余行逐字节原样转发且逐块 Flush。
func TestResponsesStreamFullPath(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexStreamBody))

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(true))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}
	if !w.Flushed {
		t.Error("SSE 响应应被逐块 Flush")
	}

	body := w.Body.String()
	if strings.Contains(body, codexBackendModel) {
		t.Errorf("响应泄露了后端侧带版本后缀的模型名 %q:\n%s", codexBackendModel, body)
	}
	var rewritten int
	for _, payload := range sseData(body) {
		if m := eventModel(payload); m != "" {
			rewritten++
			if m != codexModel {
				t.Errorf("事件里的 response.model = %q，期望回写为 %q", m, codexModel)
			}
		}
	}
	if rewritten != 2 {
		t.Errorf("带 response.model 的事件 = %d 帧，期望 2（created + completed）", rewritten)
	}
	// 不需要改写的行必须逐字节原样过去（增量、未知事件、非 JSON 的 data 行、
	// 注释行与 event: 行）。
	for _, want := range []string{
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,` +
			`"content_index":0,"delta":"你好，世界"}`,
		`data: {"type":"response.custom_unknown","payload":{"weird":true}}`,
		"data: [DONE]",
		": ping",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("响应里没有原样转发的这一行：%s\n实际:\n%s", want, body)
		}
	}

	// 后端这一侧。
	if e.backend.count() != 1 {
		t.Fatalf("后端收到 %d 次请求，期望 1", e.backend.count())
	}
	if got := e.backend.sentAuth(0); got != "Bearer "+codexAccess1 {
		t.Errorf("后端收到 Authorization = %q，期望注入订阅令牌", got)
	}
	if got := e.backend.sentHeaderValue(0, "ChatGPT-Account-ID"); got != codexAccountID {
		t.Errorf("后端收到 ChatGPT-Account-ID = %q，期望 %q", got, codexAccountID)
	}
	for h, want := range map[string]string{
		"originator":  "codex_cli_rs",
		"session_id":  "11111111-2222-3333-4444-555555555555",
		"OpenAI-Beta": "responses=experimental",
	} {
		if got := e.backend.sentHeaderValue(0, h); got != want {
			t.Errorf("后端收到 %s = %q，期望 codex 身份头原样透传 %q", h, got, want)
		}
	}
	if got := e.backend.sentModel(0); got != codexModel {
		t.Errorf("后端收到 model = %q，期望原样透传 %q（这条路没有来源侧 ID）", got, codexModel)
	}
	if b := e.backend.sentBody(0); !strings.Contains(b, `"store":false`) ||
		!strings.Contains(b, `"instructions"`) {
		t.Errorf("请求体未保真透传: %s", b)
	}
	if strings.Contains(e.backend.sentAuth(0), testKey) {
		t.Error("客户端的 sk_ Key 绝不能外传给后端")
	}
	// Accept-Encoding 必须由网关自协商（Go Transport 会填 gzip 并透明解压），
	// 客户端那份不得外传——否则后端按 br/zstd 压缩，model 回写静默失效。
	if got := e.backend.sentHeaderValue(0, "Accept-Encoding"); strings.Contains(got, "br") {
		t.Errorf("后端收到 Accept-Encoding = %q，客户端那份应被剥掉", got)
	}
}

// TestResponsesStreamWithoutContentType 钉真机订阅后端的形态：请求明确
// stream:true，成功响应体是 SSE，但上游没有 Content-Type。网关仍须逐行
// 观测完成事件，否则输入会全按未缓存估算，输出与缓存则变成 0。
func TestResponsesStreamWithoutContentType(t *testing.T) {
	e := newAgentEnv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // 刻意不写 Content-Type
		_, _ = io.WriteString(w, codexStreamBody)
	})
	if err := e.st.SetModelPricing(t.Context(), dbModel(t, e.st, codexModel), meterPricing); err != nil {
		t.Fatalf("SetModelPricing: %v", err)
	}
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	w := do(e.h, "POST", "/codex/v1/responses", codexAuth, codexReq(true))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q，期望网关补成 text/event-stream", ct)
	}
	if !w.Flushed {
		t.Error("缺少 Content-Type 的 SSE 仍应逐行 Flush")
	}
	if strings.Contains(w.Body.String(), codexBackendModel) {
		t.Errorf("流式模型名未回写: %s", w.Body.String())
	}

	s := fm.only(t)
	wantTokens := usage.Tokens{Prompt: 1200, Completion: 42, CacheRead: 900}
	if s.Tokens != wantTokens {
		t.Errorf("tokens = %+v，期望完成事件真值 %+v", s.Tokens, wantTokens)
	}
	if s.Estimated {
		t.Error("完成事件带 usage，不应因响应头缺失落入估算")
	}
	p, err := usage.ParsePricing(s.Pricing)
	if err != nil {
		t.Fatalf("解析记账价失败: %v", err)
	}
	if got, want := usage.Cost(p, usage.Measure{
		Kind: store.ModelKindText, Entry: s.Entry, Tokens: s.Tokens,
	}), int64(1674); got != want {
		t.Errorf("名义金额 = %d 微元，期望 %d（未缓存输入 + 缓存命中 + 输出）", got, want)
	}
}

// TestResponsesNonStream：非流式的 model 回写在顶层。
func TestResponsesNonStream(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if got := bodyModel(t, w); got != codexModel {
		t.Errorf("响应 model = %q，期望回写为 %q", got, codexModel)
	}
	if strings.Contains(w.Body.String(), codexBackendModel) {
		t.Errorf("响应泄露了后端侧模型名: %s", w.Body.String())
	}
	// 其余字段原样透出。
	if !strings.Contains(w.Body.String(), `"resp_2"`) ||
		!strings.Contains(w.Body.String(), `"total_tokens":37`) {
		t.Errorf("响应未保真透出: %s", w.Body.String())
	}
}

// TestResponsesBackendErrorPassthrough：后端的错误体原样透传，不改写、不包装。
// 后端坏令牌的 401 是 {"detail":…} 形（Phase 2 实测），这里用 400 覆盖"非 401
// 的 4xx 立即提交"这一支——401 有它自己的重试用例。
func TestResponsesBackendErrorPassthrough(t *testing.T) {
	const vendorErr = `{"detail":"Invalid request: unsupported parameter"}`
	e := newAgentEnv(t, jsonReply(http.StatusBadRequest, vendorErr))

	// 即使客户端请求了流式，非 2xx 也仍是原样错误体，不按 SSE 处理。
	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(true))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望原样透传 400", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != vendorErr {
		t.Errorf("错误体 = %s，期望逐字节透传 %s", got, vendorErr)
	}
}

// TestResponsesBackendUnreachable：代理这一跳出不了网（受限现场）→ 502 且文案
// 说清是连不上，客户端可重试（决策 9 的第三条路，另两条是登录与刷新）。
// 这里**没有下一个来源**可切——订阅是单账户，连不上就是连不上。
func TestResponsesBackendUnreachable(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	e.srv.SetAgentEndpoints(e.issuer.url, deadURL)

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；body: %s", w.Code, w.Body.String())
	}
	_, code, msg := decodeError(t, w)
	if code != "upstream_unreachable" {
		t.Errorf("错误码 = %q，期望 upstream_unreachable", code)
	}
	if msg == "" {
		t.Error("错误文案为空")
	}
	if e.backend.count() != 0 {
		t.Errorf("地址已指向死端口，后端却收到 %d 次请求", e.backend.count())
	}
}

// ---- 账号头：只有设备说了算 ----

// TestResponsesAccountHeaderIsDeviceOnly：ChatGPT-Account-ID **只能由设备指定**。
//
// 这个头选的是「拿这份订阅令牌去访问哪个 ChatGPT 账号/工作区」，而令牌是管理员
// 的。客户端的业务头是整体复制到上游请求上的（copyForwardHeaders 只剥逐跳头与
// 客户端凭证），所以注入处必须**先无条件删掉它再按需注入**：设备侧 account_id
// 为空那条路完全可达（粘贴的 auth.json 不带 account_id、id_token 也没带 claim），
// 只做条件 Set 就等于让任何 sk_ Key 持有者自己挑账号。
func TestResponsesAccountHeaderIsDeviceOnly(t *testing.T) {
	const forged = "acct-attacker-workspace"
	clientHeader := func() map[string]string {
		h := make(map[string]string, len(codexAuth)+1)
		for k, v := range codexAuth {
			h[k] = v
		}
		h[codexAccountHeaderName] = forged
		return h
	}

	t.Run("设备侧为空：一个都不发", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		// 句柄里没有 account_id，库里那列也空。
		if _, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
			Provider: store.AgentProviderCodex,
			AuthJSON: codexAuthJSONFor(codexAccess1, codexRefresh1, ""),
		}); err != nil {
			t.Fatalf("UpsertAgentAccount: %v", err)
		}
		w := do(e.h, "POST", "/agents/v1/responses", clientHeader(), codexReq(false))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		if got := e.backend.sentHeaderValues(0, codexAccountHeaderName); len(got) != 0 {
			t.Errorf("后端收到 %s = %v，期望一个都没有："+
				"客户端不得为管理员的订阅令牌指定 ChatGPT 账号/工作区",
				codexAccountHeaderName, got)
		}
	})

	t.Run("设备侧有值：顶掉客户端那份", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		w := do(e.h, "POST", "/agents/v1/responses", clientHeader(), codexReq(false))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		got := e.backend.sentHeaderValues(0, codexAccountHeaderName)
		if len(got) != 1 || got[0] != codexAccountID {
			t.Errorf("后端收到 %s = %v，期望只有设备那一份 %q",
				codexAccountHeaderName, got, codexAccountID)
		}
	})
}

// ---- actor 头：只是客户端开关，绝不外传 ----

// TestResponsesActorAuthorizationHeaderNeverForwarded：x-openai-actor-authorization
// 是 Codex 内核判定 provider 能否挂出内置 image_gen 工具的开关（gate 写进派生与
// 共享 provider 的 http_headers），值对设备与后端都没有意义。它跟着客户端的全部
// 业务头一起被 copyForwardHeaders 抄上来，注入处必须把它剥掉：设备自己的订阅令牌
// 是唯一到达后端的凭据形态，一个占位 actor 头不许紧挨着它过去；其余身份头照旧。
func TestResponsesActorAuthorizationHeaderNeverForwarded(t *testing.T) {
	const actorHeader = "x-openai-actor-authorization"
	h := make(map[string]string, len(codexAuth)+1)
	for k, v := range codexAuth {
		h[k] = v
	}
	h[actorHeader] = "llmgate"
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	w := do(e.h, "POST", "/agents/v1/responses", h, codexReq(false))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if got := e.backend.sentHeaderValues(0, actorHeader); len(got) != 0 {
		t.Errorf("后端收到 %s = %v，期望一个都没有：它只是客户端内核的画图开关", actorHeader, got)
	}
	if got := e.backend.sentHeaderValue(0, "originator"); got != "codex_cli_rs" {
		t.Errorf("后端收到 originator = %q，剥 actor 头不该殃及其余身份头", got)
	}
}

// ---- 计量 ----

// TestResponsesMetering：一次流式调用记一笔，entry=responses、token 取
// Responses 三字段的真值、不打 estimated；没有模型目录行时未定价（金额 0 是
// 真值——订阅无边际成本）且 ModelKnown=false（决策 8 如实接受的两条推论）。
func TestResponsesMetering(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexStreamBody))
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(true))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	s := fm.only(t)
	if s.Entry != usage.EntryResponsesAgents {
		t.Errorf("entry = %q，期望 %q", s.Entry, usage.EntryResponsesAgents)
	}
	if s.ModelName != codexModel {
		t.Errorf("model = %q，期望 %q", s.ModelName, codexModel)
	}
	if s.Tokens.Prompt != 1200 || s.Tokens.Completion != 42 || s.Tokens.CacheRead != 900 {
		t.Errorf("token = %+v，期望 input=1200 output=42 cached=900", s.Tokens)
	}
	if s.Tokens.CacheWrite != 0 {
		t.Errorf("cache_write = %d，期望 0（OpenAI 口径下缓存写已含在输入里）", s.Tokens.CacheWrite)
	}
	if s.Estimated {
		t.Error("上游给了 usage，这一笔是真值，不该打 estimated（决策 8）")
	}
	if s.Pricing != "" || s.ModelKnown {
		t.Errorf("pricing=%q modelKnown=%v，期望未定价且不在目录里（决策 2 的推论）",
			s.Pricing, s.ModelKnown)
	}
	if cost := usage.Cost(usage.Pricing{}, usage.Measure{
		Kind: store.ModelKindText, Entry: s.Entry, Tokens: s.Tokens,
	}); cost != 0 {
		t.Errorf("金额 = %d 微元，期望 0（订阅无边际成本）", cost)
	}
	if s.KeyID <= 0 || s.KeyDisplay == "" {
		t.Errorf("归属未记全: %+v", s)
	}
	if s.Attempts != 1 {
		t.Errorf("attempts = %d，期望 1", s.Attempts)
	}
	if s.UpstreamName != "Codex 订阅 · 订阅账号" {
		t.Errorf("upstream = %q，期望带订阅平台与账号名称", s.UpstreamName)
	}
	if s.Status != http.StatusOK {
		t.Errorf("status = %d，期望 200", s.Status)
	}
}

// TestResponsesMeteringTruncatedStream：流在 response.completed 之前断掉——
// 上游一个 usage 字段都没给，按估算入账并打 estimated（三态语义照旧，
// "0 元是真值"说的是金额不是 token）。
func TestResponsesMeteringTruncatedStream(t *testing.T) {
	cut := strings.SplitN(codexStreamBody, "event: response.completed", 2)[0]
	e := newAgentEnv(t, sseReply(cut))
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	body := codexReq(true)
	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, body)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	s := fm.only(t)
	if !s.Estimated {
		t.Error("没有 usage 的流必须打 estimated")
	}
	// 输入侧估算读的是 Responses 的 input/instructions（不是 messages/system）。
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("用例请求体不是 JSON: %v", err)
	}
	if want := usage.EstimateResponsesInput(payload); s.Tokens.Prompt != want {
		t.Errorf("估算输入 = %d，期望 %d（走 EstimateResponsesInput）", s.Tokens.Prompt, want)
	}
	if s.Tokens.Completion <= 0 {
		t.Errorf("估算输出 = %d，期望 > 0（已收到的增量要计）", s.Tokens.Completion)
	}
}

// TestResponsesCatalogPriceKnob：决策 8 点名允许的旋钮——管理员手工在模型目录
// 建一行同名 text 模型并录价，codex 流量就按名取价记出**名义**金额、
// ModelKnown 随之为真。允许但不作为口径，所以这里只钉住行为存在。
func TestResponsesCatalogPriceKnob(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	if err := e.st.SetModelPricing(t.Context(), dbModel(t, e.st, codexModel), meterPricing); err != nil {
		t.Fatalf("SetModelPricing: %v", err)
	}
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false)); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	s := fm.only(t)
	if s.Pricing != meterPricing {
		t.Errorf("pricing = %q，期望取到同名目录行的价 %q", s.Pricing, meterPricing)
	}
	if !s.ModelKnown {
		t.Error("目录里有这一行时 ModelKnown 应为真（否则会被未知模型名合并帽约束）")
	}
}

// TestResponsesMeterEndToEnd：换上真的 usage.Meter 跑一遍——账本收得下
// entry=responses 这个新取值（kind 由 entry 推成 text），用量照记而金额恒 0，
// 于是**金额预算对这条流量天然不设防**（决策 8：唯一有效的闸门是密钥级 RPM）。
func TestResponsesMeterEndToEnd(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexStreamBody))
	m := usage.NewMeter(e.st, 31, time.UTC, nil, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)

	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(true)); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	if sp := m.Spend(testKeyID(t, e.st)); sp.DayMicro != 0 || sp.MonthMicro != 0 {
		t.Errorf("金额 = %+v，期望 0（订阅无边际成本，0 元是真值）", sp)
	}

	now := time.Now()
	rep, err := m.Report(t.Context(), now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	var found bool
	for _, row := range rep.ByEntry {
		if row.Key == usage.SubscriptionTrafficDimension {
			found = true
			if row.Requests != 1 || row.TotalTokens != 1242 {
				t.Errorf("responses 维度 = %+v，期望 1 次请求、1242 token", row)
			}
			if row.CostMicro != 0 {
				t.Errorf("responses 维度金额 = %d 微元，期望 0", row.CostMicro)
			}
		}
	}
	if !found {
		t.Errorf("报表里没有 entry=responses 这一维: %+v", rep.ByEntry)
	}
	if rep.Total.EstimatedRequests != 0 {
		t.Errorf("估算笔数 = %d，期望 0（真值 usage 不该打 estimated）", rep.Total.EstimatedRequests)
	}
}

// ---- 鉴权与准入 ----

// TestResponsesRequiresKey：无凭证与错凭证一律 401，且一个字节都不到后端。
func TestResponsesRequiresKey(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))

	for name, header := range map[string]map[string]string{
		"无凭证": {"Content-Type": "application/json"},
		"错凭证": {"Authorization": "Bearer sk_wrong", "Content-Type": "application/json"},
	} {
		w := do(e.h, "POST", "/agents/v1/responses", header, codexReq(false))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s：状态码 = %d，期望 401", name, w.Code)
		}
	}
	if e.backend.count() != 0 {
		t.Errorf("未通过鉴权的请求打到了后端 %d 次", e.backend.count())
	}
}

// TestResponsesAdmit：预算准入对本入口生效——被拒时 429 + Retry-After，
// 且不碰后端、不取令牌。
func TestResponsesAdmit(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	fm := &fakeMeter{deny: &usage.Decision{Reason: usage.RejectRPM, RetryAfterSec: 7}}
	e.srv.EnableMetering(fm)

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429；body: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q，期望 7", got)
	}
	if e.backend.count() != 0 {
		t.Errorf("被准入拒绝的请求打到了后端 %d 次", e.backend.count())
	}
	if s := fm.only(t); !s.Rejected || s.Entry != usage.EntryResponsesAgents {
		t.Errorf("被拒的一笔应带 rejected 标并记在 responses 入口上: %+v", s)
	}
}

// ---- 账号状态的两个机读原因 ----

// TestResponsesAgentNotConfigured：没连订阅 / 订阅被停用，一律
// 409 agent_not_configured（非 5xx——这是状态不是故障），且不碰后端。
func TestResponsesAgentNotConfigured(t *testing.T) {
	t.Run("没有账号", func(t *testing.T) {
		e := newRouteEnv(t)
		w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
		assertAgentRefusal(t, w, http.StatusConflict, "agent_not_configured")
	})
	t.Run("账号被停用", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		if err := e.st.SetAgentStatus(t.Context(), e.acctID, store.AgentStatusDisabled); err != nil {
			t.Fatalf("SetAgentStatus: %v", err)
		}
		w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
		assertAgentRefusal(t, w, http.StatusConflict, "agent_not_configured")
		if e.backend.count() != 0 {
			t.Errorf("停用后仍打到后端 %d 次", e.backend.count())
		}
	})
}

// TestResponsesAgentAuthExpired：库里那行已标失效 → 409 agent_auth_expired。
// 状态是每请求点查的，所以标失效在下一个请求就生效，没有缓存要失效。
func TestResponsesAgentAuthExpired(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false)); w.Code != http.StatusOK {
		t.Fatalf("前置请求状态码 = %d，期望 200", w.Code)
	}
	if err := e.st.SetAgentStatus(t.Context(), e.acctID, store.AgentStatusAuthExpired); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	assertAgentRefusal(t, w, http.StatusConflict, "agent_auth_expired")
	if e.backend.count() != 1 {
		t.Errorf("后端收到 %d 次请求，期望只有前置那 1 次", e.backend.count())
	}
}

// TestResponsesAgentRefusalUpstreamDimension：订阅侧的两种 409 在账本里分得开。
//
//   - 账号还在、只是登录失效/被停用 → 上游维度记这一个账号的名字。拒人的是它，
//     账就该出现在「按上游账号」的它那一行；掉进空名字那一格的话，管理员看到的
//     只有一行「— ¥0.00 1 次」，读不出是哪份订阅在拒。
//   - 账号压根不在了（管理员刚删掉、还没重连）→ 上游维度**留空**，那是唯一
//     无从归属的情形，界面按「未转发到上游」渲染。
//
// 两种情形都记 1 次请求、0 元、0 token，且都不该打到后端。
func TestResponsesAgentRefusalUpstreamDimension(t *testing.T) {
	t.Run("登录失效记在该账号名下", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		if err := e.st.SetAgentStatus(t.Context(), e.acctID, store.AgentStatusAuthExpired); err != nil {
			t.Fatalf("SetAgentStatus: %v", err)
		}

		w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
		assertAgentRefusal(t, w, http.StatusConflict, "agent_auth_expired")

		s := fm.only(t)
		if s.UpstreamName != "Codex 订阅 · 订阅账号" {
			t.Errorf("upstream = %q，期望记在拒人的那个订阅账号名下", s.UpstreamName)
		}
		if s.Status != http.StatusConflict || s.Attempts != 0 {
			t.Errorf("status = %d attempts = %d，期望 409/0（没发往后端）", s.Status, s.Attempts)
		}
		if (s.Tokens != usage.Tokens{}) || s.Entry != usage.EntryResponsesAgents {
			t.Errorf("样本 = %+v，期望 responses_agents 入口且 token 全 0", s)
		}
		if e.backend.count() != 0 {
			t.Errorf("后端收到 %d 次请求，期望 0", e.backend.count())
		}
	})

	t.Run("账号已删除时留空", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		if err := e.st.DeleteAgentAccount(t.Context(), e.acctID); err != nil {
			t.Fatalf("DeleteAgentAccount: %v", err)
		}

		w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
		assertAgentRefusal(t, w, http.StatusConflict, "agent_not_configured")

		s := fm.only(t)
		if s.UpstreamName != "" {
			t.Errorf("upstream = %q，期望留空：设备上已经没有这份订阅，无从归属", s.UpstreamName)
		}
		if s.Status != http.StatusConflict || s.Attempts != 0 {
			t.Errorf("status = %d attempts = %d，期望 409/0（没发往后端）", s.Status, s.Attempts)
		}
		if e.backend.count() != 0 {
			t.Errorf("后端收到 %d 次请求，期望 0", e.backend.count())
		}
	})
}

// assertAgentRefusal 断言一次拒绝的状态码与机读原因，并钉住"非 5xx"。
func assertAgentRefusal(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("状态码 = %d，期望 %d；body: %s", w.Code, status, w.Body.String())
	}
	if w.Code >= http.StatusInternalServerError {
		t.Error("「没连订阅 / 要重新登录」是状态不是故障，绝不能回 5xx")
	}
	_, gotCode, msg := decodeError(t, w)
	if gotCode != code {
		t.Errorf("错误码 = %q，期望 %q", gotCode, code)
	}
	if msg == "" {
		t.Error("错误文案为空：现场要按它知道该去找谁")
	}
}

// ---- 后端 401 之后的刷新与重试 ----

// TestResponsesRefreshOn401：后端拒了当前这一代令牌 → 作废它、刷新一次、
// 重发一次；新一代同步重新密封落库（不落库的话重启后拿的是已作废的
// refresh token，必 401）。**判据只看状态码**：这里的 401 体是
// {"detail":…} 形，按 {"error":{…}} 认必漏。
func TestResponsesRefreshOn401(t *testing.T) {
	var calls int
	var mu sync.Mutex
	e := newAgentEnvWithIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			// Phase 2 实测的那一形：坏令牌的 401 体是 {"detail":…}，
			// 不是 {"error":{…}}——判据只能是状态码。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"detail":"Your authentication token is not from a valid issuer."}`))
			return
		}
		jsonReply(http.StatusOK, codexNonStreamBody)(w, r)
	}, issuerRotates(codexAccess2, codexRefresh2))

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望刷新后重试成功 200；body: %s", w.Code, w.Body.String())
	}
	if e.backend.count() != 2 {
		t.Fatalf("后端收到 %d 次请求，期望 2（原发 + 重试）", e.backend.count())
	}
	if got := e.backend.sentAuth(0); got != "Bearer "+codexAccess1 {
		t.Errorf("第一次用的令牌 = %q，期望旧的那一代", got)
	}
	if got := e.backend.sentAuth(1); got != "Bearer "+codexAccess2 {
		t.Errorf("重试用的令牌 = %q，期望换成新一代", got)
	}
	if e.issuer.count() != 1 {
		t.Errorf("刷新了 %d 次，期望恰好 1（单飞）", e.issuer.count())
	}
	if form := e.issuer.form(0); form.Get("grant_type") != "refresh_token" ||
		form.Get("refresh_token") != codexRefresh1 {
		t.Errorf("刷新表单 = %v，期望 grant_type=refresh_token 且带旧 refresh token", form)
	}

	// 新一代必须已经重新密封落库。
	acct, authJSON, err := e.st.GetAgentCredential(t.Context(), store.AgentProviderCodex)
	if err != nil {
		t.Fatalf("GetAgentCredential: %v", err)
	}
	if !strings.Contains(authJSON, codexAccess2) || !strings.Contains(authJSON, codexRefresh2) {
		t.Error("库里仍是旧一代句柄：刷新结果没落库，重启后必 401")
	}
	if acct.LastRefreshAt.IsZero() {
		t.Error("last_refresh_at 没盖上")
	}
	if acct.Status != store.AgentStatusActive {
		t.Errorf("状态 = %q，期望仍是 active（刷新成功不该改状态）", acct.Status)
	}
}

// TestResponsesRelogin：管理员重新登录（同一行换一份句柄）之后，**下一个请求
// 就用新句柄**——进程内缓存的令牌世代跟着库里那一行的 updated_at 换代，不需要
// 重启，也没有一层要显式失效的缓存。Phase 4 的「重新连接」按钮就压在这上面。
func TestResponsesRelogin(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))

	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false)); w.Code != http.StatusOK {
		t.Fatalf("首次状态码 = %d，期望 200", w.Code)
	}
	if got := e.backend.sentAuth(0); got != "Bearer "+codexAccess1 {
		t.Fatalf("首次用的令牌 = %q，期望 %q", got, codexAccess1)
	}

	const relogin = "codex-access-token-relogin"
	acct, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider:  store.AgentProviderCodex,
		AccountID: codexAccountID,
		AuthJSON:  codexAuthJSON(relogin, "codex-refresh-relogin"),
		// 显式时刻：updated_at 必须与上一世代不同，用例才不依赖毫秒时序。
		CreatedAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("重新登录 Upsert: %v", err)
	}
	if acct.ID != e.acctID {
		t.Fatalf("重新登录建出了第二行（id %d → %d）：单账户语义被破坏", e.acctID, acct.ID)
	}

	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false)); w.Code != http.StatusOK {
		t.Fatalf("重新登录后状态码 = %d，期望 200", w.Code)
	}
	if got := e.backend.sentAuth(1); got != "Bearer "+relogin {
		t.Errorf("重新登录后用的令牌 = %q，期望换成新句柄的 %q", got, relogin)
	}
}

// TestResponsesStaleSnapshotNeverRegresses：**令牌世代只进不退**。
//
// 一份迟到的旧快照绝不能把内存里的世代倒回去。这不是洁癖：refresh token 是
// 轮换型的（决策 1），倒回去的那一代手上那份**已经被消费掉**，它的下一次刷新
// 必被确定性拒绝 → 整台设备落闩，此后每个请求都回 agent_auth_expired，而库里
// 明明还躺着一份好句柄，只有管理员重新登录才解得开。
//
// 生产里旧快照来自并发：点查（GetAgentCredential）发生在 agentMu 之外，两个
// 请求的快照可以乱序到达 agentSessionFor。那个交错从包外没法确定性复现，所以
// 这里把**同样的输入**摆到它面前——它比对的本来就只有 updated_at 一个量：先让
// 内存世代走到"新"，再把库里那行按更早的 updated_at 写回旧世代。
func TestResponsesStaleSnapshotNeverRegresses(t *testing.T) {
	const (
		gen0Refresh = "codex-refresh-gen0"
		gen1Refresh = "codex-refresh-gen1"
	)
	// 真 JWT（见 codexJWT）：旧世代的 access_token 已过期 → 取令牌会去刷新，
	// 而那一步正是"倒退"要撞上的地方（手里那份 refresh token 早已被消费）；
	// 新世代还有一小时 → 不刷新。用假串则两代都是"到期时刻未知 = 有效"，
	// 倒退之后照样直接把旧令牌发出去，这条回归就无从观察。
	gen0Access := codexJWT(t, time.Now().Add(-time.Hour))
	gen1Access := codexJWT(t, time.Now().Add(time.Hour))

	e := newAgentEnvWithIssuer(t, jsonReply(http.StatusOK, codexNonStreamBody),
		issuerRotatingOnce(map[string][2]string{gen0Refresh: {gen1Access, gen1Refresh}}))

	// putGen 把某一代句柄写进库并显式指定 updated_at（"更旧的快照"要的就是
	// 一个明确早于刷新落库时刻的值）。
	putGen := func(access, refresh string, at time.Time) {
		t.Helper()
		if _, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
			Provider:  store.AgentProviderCodex,
			AccountID: codexAccountID,
			AuthJSON:  codexAuthJSON(access, refresh),
			CreatedAt: at,
		}); err != nil {
			t.Fatalf("UpsertAgentAccount: %v", err)
		}
	}
	gen0At := time.Now().Add(-time.Hour)
	putGen(gen0Access, gen0Refresh, gen0At)

	// ① 首个请求：手上这代已过期 → 刷新一次换到 gen1，新世代同步落库
	//（updated_at 因此前进到"现在"）。
	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false)); w.Code != http.StatusOK {
		t.Fatalf("首个请求状态码 = %d，期望刷新后 200；body: %s", w.Code, w.Body.String())
	}
	if got := e.backend.sentAuth(0); got != "Bearer "+gen1Access {
		t.Fatalf("首个请求用的令牌 = %q，期望换代后的 gen1", got)
	}
	// ② 第二个请求：读到刚落库的 gen1（updated_at 已前进），内存里的 session
	// 随之记住这个更新的世代——第三步要越过的正是它。
	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false)); w.Code != http.StatusOK {
		t.Fatalf("第二个请求状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}

	// ③ 迟到的旧快照登场：同一行、更早的 updated_at、gen0 的内容。
	putGen(gen0Access, gen0Refresh, gen0At)

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusOK {
		t.Fatalf("旧快照把世代倒了回去：状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if got := e.backend.sentAuth(2); got != "Bearer "+gen1Access {
		t.Errorf("第三个请求用的令牌 = %q，期望仍是 gen1（旧快照不得倒退世代）", got)
	}
	if got := e.issuer.count(); got != 1 {
		t.Errorf("刷新了 %d 次，期望仍是 1——倒退一次就会拿已被消费的 refresh token 再刷一次", got)
	}
	acct, err := e.st.GetAgentAccount(t.Context(), e.acctID)
	if err != nil {
		t.Fatalf("GetAgentAccount: %v", err)
	}
	if acct.Status != store.AgentStatusActive {
		t.Errorf("状态 = %q，期望仍是 %q：倒退那一步的刷新会被确定性拒绝，"+
			"整台设备就此落闩，只有管理员重新登录才解得开", acct.Status, store.AgentStatusActive)
	}
}

// TestResponsesRefreshSingleFlight：N 个并发请求同时撞上一个被拒的令牌，
// **只换一代**。
//
// 这是决策 1「盒子是唯一刷新者」在进程内的最后一段：refresh token 是轮换型的，
// 每换一代都作废上一代，所以 N 个并发 401 换出 N 代 = 自己把自己的句柄链废掉。
// 挡住它的是两层——Provider 的单飞锁，加上 Invalidate 按世代去重
// （后到的那些交回的是早已换掉的那一代，不再触发刷新）。
func TestResponsesRefreshSingleFlight(t *testing.T) {
	e := newAgentEnvWithIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+codexAccess1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"detail":"expired"}`))
			return
		}
		jsonReply(http.StatusOK, codexNonStreamBody)(w, r)
	}, issuerRotates(codexAccess2, codexRefresh2))

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false)).Code
		}()
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("第 %d 个并发请求状态码 = %d，期望 200", i, code)
		}
	}
	if got := e.issuer.count(); got != 1 {
		t.Errorf("刷新了 %d 次，期望恰好 1（并发 401 不得连锁换代）", got)
	}
}

// TestResponsesRefreshRejected：刷新被**确定性拒绝**（invalid_grant 类 4xx）
// → 回 409 agent_auth_expired，库里那行同步标 auth_expired，自动重试到此为止
// （决策 5 的两类失败两种反应）。
func TestResponsesRefreshRejected(t *testing.T) {
	e := newAgentEnvWithIssuer(t,
		jsonReply(http.StatusUnauthorized, `{"detail":"token expired"}`),
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"…"}`))
		})

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	assertAgentRefusal(t, w, http.StatusConflict, "agent_auth_expired")
	if e.backend.count() != 1 {
		t.Errorf("后端收到 %d 次请求，期望 1（拿不到新令牌就不该再发）", e.backend.count())
	}

	acct, err := e.st.GetAgentAccount(t.Context(), e.acctID)
	if err != nil {
		t.Fatalf("GetAgentAccount: %v", err)
	}
	if acct.Status != store.AgentStatusAuthExpired {
		t.Errorf("状态 = %q，期望 %q", acct.Status, store.AgentStatusAuthExpired)
	}

	// 落闩之后不再打签发方：下一个请求直接回同一个机读原因。
	before := e.issuer.count()
	w2 := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	assertAgentRefusal(t, w2, http.StatusConflict, "agent_auth_expired")
	if e.issuer.count() != before {
		t.Errorf("落闩后又刷新了 %d 次，期望停手", e.issuer.count()-before)
	}
}

// TestResponsesRefreshUnreachable：出网到签发方失败（受限现场）→ 502 且文案
// 说清是连不上，客户端可重试；库里那行**不**被标失效（网络错不是确定性拒绝）。
func TestResponsesRefreshUnreachable(t *testing.T) {
	e := newAgentEnvWithIssuer(t,
		jsonReply(http.StatusUnauthorized, `{"detail":"token expired"}`),
		func(w http.ResponseWriter, r *http.Request) {
			// 5xx 是"我这边有事"，按可重试处理（凭据原样保留）。
			w.WriteHeader(http.StatusBadGateway)
		})

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；body: %s", w.Code, w.Body.String())
	}
	_, code, _ := decodeError(t, w)
	if code != "upstream_unreachable" {
		t.Errorf("错误码 = %q，期望 upstream_unreachable", code)
	}
	acct, err := e.st.GetAgentAccount(t.Context(), e.acctID)
	if err != nil {
		t.Fatalf("GetAgentAccount: %v", err)
	}
	if acct.Status != store.AgentStatusActive {
		t.Errorf("状态 = %q，期望仍是 active（网络错不该判成登录失效）", acct.Status)
	}
}

// TestResponsesCorruptHandle：密文解得开、内容却不是一份可用的 auth.json
// （手改过的库、旧格式）→ 同样只有"重新连接"一条路，回 agent_auth_expired；
// 而这条日志路径是 §15.1 的看点：它记的是那个**带着密文**的结构
// （GetAgentCredential 唯一填了密文的返回值），LogValue 必须把密文挡在外面。
func TestResponsesCorruptHandle(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	if _, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderCodex,
		AuthJSON: `{"tokens":{"id_token":"x"}}`, // 没有 access/refresh
	}); err != nil {
		t.Fatalf("UpsertAgentAccount: %v", err)
	}
	acct, _, err := e.st.GetAgentCredential(t.Context(), store.AgentProviderCodex)
	if err != nil {
		t.Fatalf("GetAgentCredential: %v", err)
	}

	w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
	assertAgentRefusal(t, w, http.StatusConflict, "agent_auth_expired")
	if e.backend.count() != 0 {
		t.Errorf("句柄不可用却打了后端 %d 次", e.backend.count())
	}
	logs := e.logBuf.String()
	if !strings.Contains(logs, store.AgentProviderCodex) {
		t.Error("这条失败没留下可排障的日志")
	}
	if acct.AuthJSONSealed == "" {
		t.Fatal("用例前提不成立：取令牌视图应带密文")
	}
	if strings.Contains(logs, acct.AuthJSONSealed) {
		t.Errorf("日志里出现了凭据密文（AgentAccount.LogValue 没挡住）:\n%s", logs)
	}
}

// TestResponsesNoLeakInLogs：§15.1——订阅令牌、账号头与请求/响应内容都不进日志。
func TestResponsesNoLeakInLogs(t *testing.T) {
	e := newAgentEnvWithIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		if e := r.Header.Get("Authorization"); e == "Bearer "+codexAccess1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"detail":"expired"}`))
			return
		}
		jsonReply(http.StatusOK, codexNonStreamBody)(w, r)
	}, issuerRotates(codexAccess2, codexRefresh2))

	if w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(true)); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	logs := e.logBuf.String()
	for _, secret := range []string{
		codexAccess1, codexAccess2, codexRefresh1, codexRefresh2, "写个快排", "You are a coding agent.",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("日志里出现了不该有的内容 %q:\n%s", secret, logs)
		}
	}
}

// TestResponsesAgentsRoot（2026-08-12 迁移）：/agents/v1/responses 是 Agents
// 唯一入口；旧路径 /v1/responses 已整体移除（同日产品决定，不留兼容别名），
// 落进 /v1/ 兜底 404 并保留给将来的标准 Responses 目录面；/agents/ 前缀归
// 数据面中间件链（无凭证 401、未知子路径先认证再 404，OpenAI 风格），
// 不落进管理面。裁决见 docs/firmware-gateway.md。
func TestResponsesAgentsRoot(t *testing.T) {
	t.Run("专用根整轮可用", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		w := do(e.h, "POST", "/agents/v1/responses", codexAuth, codexReq(false))
		if w.Code != http.StatusOK {
			t.Fatalf("专用根状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		if got := bodyModel(t, w); got != codexModel {
			t.Errorf("专用根响应 model = %q，期望回写为 %q", got, codexModel)
		}
	})
	t.Run("旧路径已移除：/v1/responses 一律 404", func(t *testing.T) {
		// 订阅在场也一样——旧路径不再指向 Agents 面，等它的是将来的标准
		// Responses 目录面。
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		w := do(e.h, "POST", "/v1/responses", codexAuth, codexReq(false))
		if w.Code != http.StatusNotFound {
			t.Fatalf("旧路径状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
		}
	})
	t.Run("agents 前缀走数据面纪律", func(t *testing.T) {
		e := newRouteEnv(t)
		if w := do(e.h, "POST", "/agents/v1/responses",
			map[string]string{"Content-Type": "application/json"}, codexReq(false)); w.Code != http.StatusUnauthorized {
			t.Errorf("无凭证状态码 = %d，期望 401（数据面认证链在场）", w.Code)
		}
		w := do(e.h, "GET", "/agents/v1/other", chatAuth, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("未知子路径状态码 = %d，期望数据面 404", w.Code)
		}
		// OpenAI 风格错误体（带 type 字段）＝数据面兜底在管；管理面 404 没有它。
		if !strings.Contains(w.Body.String(), `"type"`) {
			t.Errorf("未知子路径应是数据面 OpenAI 风格错误体: %s", w.Body.String())
		}
	})
}

// ---- GET /agents/v1/models（2026-08-19）----
//
// codex 的模型选择器打 <base_url>/models：此前落到 /agents/ 兜底、认证过也只
// 回 404，订阅用户的模型清单恒为空。现在它列「已连接订阅带来的文本模型」，
// 判据由管理面出（gateway.AgentModels），数据面只负责拼形态与降级。

// fakeAgentModels 是 gateway.AgentModels 的用例替身。
type fakeAgentModels struct {
	owners map[string]string
	err    error
	calls  int
}

func (f *fakeAgentModels) AgentSubscriptionModels(context.Context) (map[string]string, error) {
	f.calls++
	return f.owners, f.err
}

// decodeModelList 解析 OpenAI list 形的模型读数。
func decodeModelList(t *testing.T, w *httptest.ResponseRecorder) []struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
} {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析模型列表: %v\n%s", err, w.Body.String())
	}
	if body.Object != "list" {
		t.Errorf("object = %q，期望 list", body.Object)
	}
	if body.Data == nil {
		t.Error("data 必须是数组而不是 null——客户端按数组解，null 会让它当解析失败")
	}
	return body.Data
}

func connectModelListProvider(t *testing.T, e *routeEnv, provider string) {
	t.Helper()
	if _, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: provider, Label: "模型列表测试", AuthJSON: `{}`,
	}); err != nil {
		t.Fatalf("UpsertAgentAccount(%s): %v", provider, err)
	}
}

func TestAgentModelsListsSubscriptionModels(t *testing.T) {
	e := newRouteEnv(t)
	connectModelListProvider(t, e, store.AgentProviderCodex)
	connectModelListProvider(t, e, store.AgentProviderGrok)
	// 三行目录：两行属订阅（一份 codex 一份 grok），一行是管理员自己的 API 模型。
	dbModel(t, e.st, "gpt-5-codex")
	dbModel(t, e.st, "grok-4.6")
	dbModel(t, e.st, "deepseek-chat")
	fake := &fakeAgentModels{owners: map[string]string{
		"gpt-5-codex": store.AgentProviderCodex,
		"grok-4.6":    store.AgentProviderGrok,
	}}
	e.srv.SetAgentModels(fake)

	got := decodeModelList(t, do(e.h, http.MethodGet, "/agents/v1/models", chatAuth, ""))
	if len(got) != 2 {
		t.Fatalf("条数 = %d，期望 2（管理员的 API 模型不该出现在订阅清单里）：%+v", len(got), got)
	}
	// 字典序，与 /v1/models 同一口径。
	if got[0].ID != "gpt-5-codex" || got[1].ID != "grok-4.6" {
		t.Fatalf("未按名字典序：%+v", got)
	}
	// owned_by 是订阅 provider，不是 /v1/models 那个恒定的 llmgate：
	// 这条读数的意义就在于指出哪个名字背后是哪份订阅。
	if got[0].OwnedBy != store.AgentProviderCodex || got[1].OwnedBy != store.AgentProviderGrok {
		t.Fatalf("owned_by 应为订阅 provider：%+v", got)
	}
	for _, m := range got {
		if m.Object != "model" {
			t.Errorf("%s: object = %q", m.ID, m.Object)
		}
		if m.Created <= 0 {
			t.Errorf("%s: created = %d，应取模型行的建行时刻", m.ID, m.Created)
		}
	}
	keys, err := e.st.ListAPIKeys(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v (%d)", err, len(keys))
	}
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keys[0].ID, AllowCodexSubscription: true,
	}); err != nil {
		t.Fatal(err)
	}
	got = decodeModelList(t, do(e.h, http.MethodGet, "/agents/v1/models", chatAuth, ""))
	if len(got) != 1 || got[0].OwnedBy != store.AgentProviderCodex {
		t.Fatalf("停用 Grok 后直接订阅模型列表仍泄漏：%+v", got)
	}
}

// TestAgentModelsSkipsNamesWithoutRow 守的是「不编 created」：读数里有个名字
// 而目录里没有对应行（收敛器刚删掉、或两次读之间被改名），只能跳过——补一个
// 猜出来的 created 就是在响应里编事实。
func TestAgentModelsSkipsNamesWithoutRow(t *testing.T) {
	e := newRouteEnv(t)
	connectModelListProvider(t, e, store.AgentProviderCodex)
	dbModel(t, e.st, "gpt-5-codex")
	e.srv.SetAgentModels(&fakeAgentModels{owners: map[string]string{
		"gpt-5-codex": store.AgentProviderCodex,
		"已经没有的模型":     store.AgentProviderCodex,
	}})
	got := decodeModelList(t, do(e.h, http.MethodGet, "/agents/v1/models", chatAuth, ""))
	if len(got) != 1 || got[0].ID != "gpt-5-codex" {
		t.Fatalf("目录里没有行的名字应当跳过：%+v", got)
	}
}

// TestAgentModelsDegradesToEmptyList 守两条刻意的降级：没接线、读数出错，
// 都答空列表而不是 5xx。模型选择器空着不影响 /agents/v1/responses，
// 把它做成故障点不划算。
func TestAgentModelsDegradesToEmptyList(t *testing.T) {
	t.Run("未接线", func(t *testing.T) {
		e := newRouteEnv(t)
		if got := decodeModelList(t, do(e.h, http.MethodGet, "/agents/v1/models", chatAuth, "")); len(got) != 0 {
			t.Fatalf("未接线应答空列表：%+v", got)
		}
	})
	t.Run("读数出错", func(t *testing.T) {
		e := newRouteEnv(t)
		dbModel(t, e.st, "gpt-5-codex")
		e.srv.SetAgentModels(&fakeAgentModels{err: errors.New("目录读取失败")})
		if got := decodeModelList(t, do(e.h, http.MethodGet, "/agents/v1/models", chatAuth, "")); len(got) != 0 {
			t.Fatalf("读数出错应答空列表：%+v", got)
		}
	})
}

// TestAgentModelsRequiresKey 钉住这条端点仍在 withAuth 之后：它读的是这台设备
// 有哪些订阅模型，不是公开信息。
func TestAgentModelsRequiresKey(t *testing.T) {
	e := newRouteEnv(t)
	e.srv.SetAgentModels(&fakeAgentModels{owners: map[string]string{"gpt-5-codex": store.AgentProviderCodex}})
	w := do(e.h, http.MethodGet, "/agents/v1/models", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据时状态码 = %d，期望 401", w.Code)
	}
}
