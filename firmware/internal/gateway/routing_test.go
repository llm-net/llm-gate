// routing_test.go 是 iteration-5 Phase 3 的可执行验收：数据面动态选路
// （每请求三表点查、按优先级选路、请求体 model 改写为来源侧 ID）、启停即时
// 生效、按入口协议过滤候选、故障切换（未提交即切、每来源至多一次）、
// /v1/models 口径与字典序，以及 count_tokens 的逐来源尝试与本地粗估兜底。
//
// 编排方式：多个 httptest 上游 + 直接经 store 建上游/模型/来源行（等价管理面
// 操作），断言"客户端看到什么"与"每个上游被打了几次、收到什么"。
package gateway_test

import (
	"bytes"
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

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// deadURL 指向必然拒绝连接的地址：模拟"上游连不上"的候选来源。
const deadURL = "http://127.0.0.1:1/v1"

// routeEnv 是选路用例的环境：目录三表为空的网关 + 其 store，
// 调用方按用例需要建上游/模型/来源。srv 是同一个 Server（h 就是它的
// Handler）：计量用例据此在发请求前 EnableMetering 接一个假计量出口。
type routeEnv struct {
	h      http.Handler
	srv    *gateway.Server
	logBuf *bytes.Buffer
	st     *store.Store
	// dir 是数据目录（库文件在 dir/llmgate.db）：个别用例要以「第二条连接」直改
	// 库里 store 不提供写口的列，比如把任务观测时刻改早，见 taskprober_test.go。
	dir string
}

func newRouteEnv(t *testing.T) *routeEnv {
	t.Helper()
	cfg := &config.Config{Listen: "127.0.0.1:0", DataDir: t.TempDir(), LogLevel: "debug"}
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, slog.LevelDebug)
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, _, err := gateway.ImportConfigKeys(t.Context(), st,
		[]config.APIKey{{Key: testKey}}, logger); err != nil {
		t.Fatalf("ImportConfigKeys: %v", err)
	}
	s := gateway.New(cfg, logger, st, gateway.NewStoreKeyAuthorizer(st, logger), nil, nil)
	keys, err := st.ListAPIKeys(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v (%d)", err, len(keys))
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keys[0].ID, AllowCodexSubscription: true,
		AllowGrokSubscription: true, AllowClaudeSubscription: true,
		AllowCursorSubscription: true,
	}); err != nil {
		t.Fatalf("ReplaceDevToolConfig: %v", err)
	}
	s.SetDevToolPolicy(&devtoolpolicy.Resolver{
		Store:          st,
		AgentModels:    s.AgentSubscriptionModels,
		PlatformModels: func(context.Context) platformcatalog.Doc { return platformcatalog.Builtin() },
	})
	return &routeEnv{h: s.Handler(), srv: s, logBuf: &logBuf, st: st, dir: cfg.DataDir}
}

func selectDevToolModels(t *testing.T, st *store.Store, modelIDs ...int64) {
	t.Helper()
	keys, err := st.ListAPIKeys(t.Context())
	if err != nil || len(keys) == 0 {
		t.Fatalf("ListAPIKeys: %v (%d)", err, len(keys))
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keys[0].ID, AllowCodexSubscription: true,
		AllowGrokSubscription: true, AllowClaudeSubscription: true,
		AllowCursorSubscription: true,
		CatalogModelIDs:         modelIDs,
	}); err != nil {
		t.Fatalf("ReplaceDevToolConfig: %v", err)
	}
}

func devToolModelID(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	models, err := st.ListModelsWithSources(t.Context())
	if err != nil {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	for _, model := range models {
		if model.Name == name {
			return model.ID
		}
	}
	t.Fatalf("model %q not found", name)
	return 0
}

// chatAuth / messagesAuth 是两入口的常用请求头。
var (
	chatAuth     = map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"}
	messagesAuth = map[string]string{"x-api-key": testKey, "Content-Type": "application/json"}
)

// stubUpstream 是记录每次请求（头 + 体）的 mock 上游。
type stubUpstream struct {
	url     string
	mu      sync.Mutex
	headers []http.Header
	bodies  [][]byte
}

// newStub 起一个 mock 上游；respond 决定回放的响应。
func newStub(t *testing.T, respond http.HandlerFunc) *stubUpstream {
	t.Helper()
	s := &stubUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.bodies = append(s.bodies, b)
		s.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	s.url = srv.URL + "/v1"
	return s
}

func (s *stubUpstream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// sentModel 返回第 i 次请求体里的 model 字段（越界或非 JSON 返回空串）。
func (s *stubUpstream) sentModel(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(s.bodies[i], &m) != nil {
		return ""
	}
	v, _ := m["model"].(string)
	return v
}

// sentAuth 返回第 i 次请求的 Authorization 头。
func (s *stubUpstream) sentAuth(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.headers) {
		return ""
	}
	return s.headers[i].Get("Authorization")
}

// withHeader 在一份请求头表的副本上追加一个头（不动共享的 chatAuth / messagesAuth）。
func withHeader(base map[string]string, name, value string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out[name] = value
	return out
}

// sentBody 返回第 i 次请求体的原文（越界返回空串）。
func (s *stubUpstream) sentBody(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.bodies) {
		return ""
	}
	return string(s.bodies[i])
}

// jsonReply 回放固定状态码与 JSON 体。
func jsonReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

// chatReply 是一个带版本后缀 model 的 chat 成功响应（回写目标可证）。
func chatReply(upstreamModelID string) http.HandlerFunc {
	return jsonReply(http.StatusOK,
		fmt.Sprintf(`{"id":"chatcmpl-1","model":"%s-260801","choices":[],"usage":{"total_tokens":1}}`, upstreamModelID))
}

// bodyModel 解析客户端收到的响应体并取 model 字段。
func bodyModel(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	v, _ := m["model"].(string)
	return v
}

// —— 选路：优先级、来源侧 ID 改写、创建序 ——

// TestRoutePriorityAndModelRewrite：候选按 priority 小者先，请求体 model 改写为
// 该来源的 upstream_model_id（留空则取模型名），凭证取该来源自己的上游 Key。
func TestRoutePriorityAndModelRewrite(t *testing.T) {
	e := newRouteEnv(t)
	primary := newStub(t, chatReply("primary-side-id"))
	backup := newStub(t, chatReply("backup-side-id"))

	upPrimary := dbUpstream(t, e.st, "primary", config.UpstreamMock, "sk-primary-not-real", primary.url)
	upBackup := dbUpstream(t, e.st, "backup", config.UpstreamMock, "sk-backup-not-real", backup.url)
	m := dbModel(t, e.st, "shared-model")
	// 刻意先建高优先级数（后选）的来源：选路按 priority 而非建行序。
	dbSource(t, e.st, m, upBackup, "backup-side-id", 200)
	srcPrimary := dbSource(t, e.st, m, upPrimary, "primary-side-id", 100)

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"shared-model","messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if primary.count() != 1 || backup.count() != 0 {
		t.Fatalf("优先级选路失败：primary=%d backup=%d，期望 1/0", primary.count(), backup.count())
	}
	if got := primary.sentModel(0); got != "primary-side-id" {
		t.Errorf("上游收到的 model = %q，期望改写为来源侧 ID primary-side-id", got)
	}
	if got := primary.sentAuth(0); got != "Bearer sk-primary-not-real" {
		t.Errorf("上游收到的 Authorization = %q，期望该来源自己的上游 Key", got)
	}
	if got := bodyModel(t, w); got != "shared-model" {
		t.Errorf("响应 model = %q，期望恒为请求名 shared-model", got)
	}

	// 来源侧 ID 留空 = 与模型名相同（路由视图解析）。
	if err := e.st.UpdateModelSource(t.Context(), srcPrimary, "", 100); err != nil {
		t.Fatalf("UpdateModelSource: %v", err)
	}
	do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"shared-model","messages":[]}`)
	if got := primary.sentModel(1); got != "shared-model" {
		t.Errorf("留空来源侧 ID 时上游收到 model = %q，期望取模型名", got)
	}
}

// TestRouteSamePriorityByCreation：同优先级按创建序（id 小者先）。
func TestRouteSamePriorityByCreation(t *testing.T) {
	e := newRouteEnv(t)
	first := newStub(t, chatReply("x"))
	second := newStub(t, chatReply("x"))
	m := dbModel(t, e.st, "tie")
	dbSource(t, e.st, m, dbUpstream(t, e.st, "first", config.UpstreamMock, "k1", first.url), "", 100)
	dbSource(t, e.st, m, dbUpstream(t, e.st, "second", config.UpstreamMock, "k2", second.url), "", 100)

	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"tie","messages":[]}`); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if first.count() != 1 || second.count() != 0 {
		t.Errorf("同优先级应按创建序：first=%d second=%d，期望 1/0", first.count(), second.count())
	}
}

// TestRouteDisabledImmediate：模型/来源/上游任一停用对数据面即时生效
// （无缓存，决策 3），启用即时恢复；无可用来源时 404 model_not_found。
func TestRouteDisabledImmediate(t *testing.T) {
	e := newRouteEnv(t)
	up := newStub(t, chatReply("only"))
	upID := dbUpstream(t, e.st, "only", config.UpstreamMock, "k", up.url)
	modelID := dbModel(t, e.st, "toggle")
	srcID := dbSource(t, e.st, modelID, upID, "", 100)

	call := func() int {
		return do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"toggle","messages":[]}`).Code
	}
	if got := call(); got != http.StatusOK {
		t.Fatalf("初始状态码 = %d，期望 200", got)
	}
	for _, c := range []struct {
		name   string
		off    func() error
		on     func() error
		expect int
	}{
		{"停用来源", func() error { return e.st.SetModelSourceDisabled(t.Context(), srcID, true) },
			func() error { return e.st.SetModelSourceDisabled(t.Context(), srcID, false) }, http.StatusNotFound},
		{"停用上游", func() error { return e.st.SetUpstreamDisabled(t.Context(), upID, true) },
			func() error { return e.st.SetUpstreamDisabled(t.Context(), upID, false) }, http.StatusNotFound},
		{"停用模型", func() error { return e.st.SetModelDisabled(t.Context(), modelID, true) },
			func() error { return e.st.SetModelDisabled(t.Context(), modelID, false) }, http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.off(); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got := call(); got != c.expect {
				t.Errorf("%s后状态码 = %d，期望 %d（即时生效）", c.name, got, c.expect)
			}
			if err := c.on(); err != nil {
				t.Fatalf("恢复 %s: %v", c.name, err)
			}
			if got := call(); got != http.StatusOK {
				t.Errorf("恢复后状态码 = %d，期望 200（即时生效）", got)
			}
		})
	}

	// 模型存在但一条来源也没有：同样是 model_not_found（不暴露内部原因）。
	dbModel(t, e.st, "sourceless")
	w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"sourceless","messages":[]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("无来源模型状态码 = %d，期望 404", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "model_not_found" {
		t.Errorf("error.code = %q，期望 model_not_found", code)
	}
}

// TestRouteProtocolFilter：入口按 (上游 type, 入口协议) 内置端点表过滤候选
// （决策 4）——ark 型来源不进 /v1/messages 候选；同一模型双入口都可调；
// 无该协议来源 → 404 protocol_mismatch（消息指向另一入口）。
func TestRouteProtocolFilter(t *testing.T) {
	e := newRouteEnv(t)
	dual := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			jsonReply(http.StatusOK, `{"id":"msg_1","type":"message","model":"dual-260801","content":[]}`)(w, r)
			return
		}
		chatReply("dual")(w, r)
	})
	upMock := dbUpstream(t, e.st, "mock-dual", config.UpstreamMock, "k", dual.url)
	// ark（方舟按量）没有 anthropic 端点：内置端点表说了算，不拨号即可判定。
	upArk := dbUpstream(t, e.st, "ark-only", config.UpstreamArk, "sk-ark-not-real", "")

	dbSource(t, e.st, dbModel(t, e.st, "dual-model"), upMock, "", 100)
	dbSource(t, e.st, dbModel(t, e.st, "ark-model"), upArk, "", 100)
	mixed := dbModel(t, e.st, "mixed-model")
	dbSource(t, e.st, mixed, upArk, "", 100) // 高优先级但不服务 anthropic
	dbSource(t, e.st, mixed, upMock, "", 200)

	t.Run("同名模型双入口都可调", func(t *testing.T) {
		if w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
			`{"model":"dual-model","messages":[]}`); w.Code != http.StatusOK {
			t.Errorf("chat 入口状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		w := do(e.h, "POST", "/v1/messages", messagesAuth,
			`{"model":"dual-model","max_tokens":8,"messages":[]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("messages 入口状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		if got := bodyModel(t, w); got != "dual-model" {
			t.Errorf("messages 响应 model = %q，期望请求名 dual-model", got)
		}
	})

	t.Run("仅 ark 来源的模型进 messages 入口 404", func(t *testing.T) {
		w := do(e.h, "POST", "/v1/messages", messagesAuth,
			`{"model":"ark-model","max_tokens":8,"messages":[]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
		}
		errType, msg := decodeAnthropicError(t, w)
		if errType != "not_found_error" {
			t.Errorf("error.type = %q，期望 not_found_error", errType)
		}
		if !strings.Contains(msg, "/v1/chat/completions") {
			t.Errorf("protocol_mismatch 消息未指向另一入口: %s", msg)
		}
		// 错误体不泄露上游账户名与来源侧信息。
		for _, leak := range []string{"ark-only", "sk-ark-not-real", "volces"} {
			if strings.Contains(w.Body.String(), leak) {
				t.Errorf("错误响应泄露上游信息 %q: %s", leak, w.Body.String())
			}
		}
	})

	t.Run("混挂模型的 messages 入口只见支持的来源", func(t *testing.T) {
		before := dual.count()
		w := do(e.h, "POST", "/v1/messages", messagesAuth,
			`{"model":"mixed-model","max_tokens":8,"messages":[]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（ark 来源被过滤，落到 mock 来源）；body: %s", w.Code, w.Body.String())
		}
		if dual.count() != before+1 {
			t.Errorf("mock 来源未被使用：count %d → %d", before, dual.count())
		}
	})
}

// TestRouteOpenAICompat（2026-08-09）：openai_compat 型来源（通用 OpenAI 兼容
// 适配，base_url 自填）只进 chat 入口——chat 200 且凭证注入 / model 改写照常；
// messages 入口 404 protocol_mismatch（消息指向 chat）；/v1/models 照列。
func TestRouteOpenAICompat(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatReply("compat-side-id"))
	up := dbUpstream(t, e.st, "vllm-lab", config.UpstreamOpenAICompat, "sk-compat-not-real", stub.url)
	m := dbModel(t, e.st, "compat-model")
	dbSource(t, e.st, m, up, "compat-side-id", 100)

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"compat-model","messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("chat 入口状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if stub.count() != 1 || stub.sentModel(0) != "compat-side-id" {
		t.Errorf("上游收到 %d 次请求 / model=%q，期望 1 次且改写为来源侧 ID", stub.count(), stub.sentModel(0))
	}
	if got := stub.sentAuth(0); got != "Bearer sk-compat-not-real" {
		t.Errorf("上游收到的 Authorization = %q，期望注入该上游的 Key", got)
	}
	if got := bodyModel(t, w); got != "compat-model" {
		t.Errorf("响应 model = %q，期望请求名 compat-model", got)
	}

	// openai_compat 不服务 anthropic：messages 入口 protocol_mismatch 指向 chat。
	w = do(e.h, "POST", "/v1/messages", messagesAuth, `{"model":"compat-model","max_tokens":8,"messages":[]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("messages 入口状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
	}
	if errType, msg := decodeAnthropicError(t, w); errType != "not_found_error" || !strings.Contains(msg, "/v1/chat/completions") {
		t.Errorf("期望 protocol_mismatch 指向 chat 入口: type=%q msg=%q", errType, msg)
	}
	if stub.count() != 1 {
		t.Errorf("messages 入口不应打到 openai_compat 上游，请求数 = %d", stub.count())
	}

	// /v1/models 照列（text kind + 可用来源，与其他类型同口径）。
	w = do(e.h, "GET", "/v1/models", chatAuth, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "compat-model") {
		t.Errorf("/v1/models 未列出 openai_compat 服务的模型: %d %s", w.Code, w.Body.String())
	}
}

// TestRouteQwenPlan（2026-08-09）：qwen_plan 型来源（阿里云百炼 通义千问
// Token Plan 订阅套餐）进**文本双入口**——chat 与 messages 都落到它，凭证注入
// 与 model 改写照常。真实端点根两协议不同段（/compatible-mode/v1 与
// /apps/anthropic/v1，由 upstream_test 的端点表用例钉死）；这里用 base_url
// 覆盖指向同一个 stub，测的是「双协议都进候选」这一路由事实。
func TestRouteQwenPlan(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatReply("qwen3-max"))
	up := dbUpstream(t, e.st, "qwen-plan", config.UpstreamQwenPlan, "sk-qwen-not-real", stub.url)
	m := dbModel(t, e.st, "qwen-model")
	dbSource(t, e.st, m, up, "qwen3-max", 100)

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"qwen-model","messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("chat 入口状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	w = do(e.h, "POST", "/v1/messages", messagesAuth, `{"model":"qwen-model","max_tokens":8,"messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("messages 入口状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if stub.count() != 2 {
		t.Fatalf("上游收到 %d 次请求，期望 2（双入口各一次）", stub.count())
	}
	for i := range 2 {
		if stub.sentModel(i) != "qwen3-max" {
			t.Errorf("第 %d 次请求 model = %q，期望改写为来源侧 ID", i, stub.sentModel(i))
		}
		if got := stub.sentAuth(i); got != "Bearer sk-qwen-not-real" {
			t.Errorf("第 %d 次请求 Authorization = %q，期望注入该上游的 Key", i, got)
		}
	}
	if got := bodyModel(t, w); got != "qwen-model" {
		t.Errorf("响应 model = %q，期望回写为请求名", got)
	}
}

// TestRouteOpenCodeGo 钉住来源模型的协议过滤、分协议鉴权、会话透传与模型别名。
func TestRouteOpenCodeGo(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatReply("longcat-2.0"))
	up := dbUpstream(t, e.st, "opencode-go", config.UpstreamOpenCodeGo, "sk-ocg-not-real", stub.url)
	chat := dbModel(t, e.st, "ocg-chat")
	messages := dbModel(t, e.st, "ocg-messages")
	native := dbModel(t, e.st, "ocg-native-responses")
	dbSource(t, e.st, chat, up, "longcat-2.0", 100)
	dbSource(t, e.st, messages, up, "minimax-m3", 100)
	dbSource(t, e.st, native, up, "gpt-5.6-luna", 100)
	headers := withHeader(withHeader(chatAuth, "x-opencode-session", "ses_chat_0001"), "X-Session-Id", "ses_ignored")
	w := do(e.h, "POST", "/v1/chat/completions", headers, `{"model":"ocg-chat","messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Chat: %d %s", w.Code, w.Body.String())
	}
	w = do(e.h, "POST", "/v1/messages", withHeader(messagesAuth, "X-Session-Id", "ses_msg_0002"), `{"model":"ocg-messages","max_tokens":8,"messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("Messages: %d %s", w.Code, w.Body.String())
	}
	if stub.count() != 2 {
		t.Fatalf("上游请求数 %d", stub.count())
	}
	if stub.sentAuth(0) != "Bearer sk-ocg-not-real" || stub.sentHeaderValue(0, "x-api-key") != "" {
		t.Fatal("Chat 鉴权错误")
	}
	if stub.sentAuth(1) != "" || stub.sentHeaderValue(1, "x-api-key") != "sk-ocg-not-real" {
		t.Fatal("Messages 鉴权错误")
	}
	for i, want := range []string{"longcat-2.0", "minimax-m3"} {
		if stub.sentModel(i) != want {
			t.Errorf("来源模型映射错误: %q", stub.sentModel(i))
		}
	}
	for i, want := range []string{"ses_chat_0001", "ses_msg_0002"} {
		if stub.sentHeaderValue(i, "x-opencode-session") != want {
			t.Error("会话透传或派生错误")
		}
	}
	for _, tc := range []struct {
		path, body string
		headers    map[string]string
	}{
		{"/v1/messages", `{"model":"ocg-chat","max_tokens":8,"messages":[]}`, messagesAuth},
		{"/v1/chat/completions", `{"model":"ocg-messages","messages":[]}`, chatAuth},
		{"/v1/responses", `{"model":"ocg-native-responses","input":"ping"}`, chatAuth},
	} {
		w = do(e.h, "POST", tc.path, tc.headers, tc.body)
		if w.Code != http.StatusNotFound {
			t.Errorf("错误协议应在本机拒绝: %s %d", tc.path, w.Code)
		}
	}
	if stub.count() != 2 {
		t.Fatal("不支持的协议仍然打到了上游")
	}
	w = do(e.h, "GET", "/v1/models", chatAuth, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "ocg-native-responses") || !strings.Contains(w.Body.String(), "ocg-chat") || !strings.Contains(w.Body.String(), "ocg-messages") {
		t.Fatalf("模型清单未按能力筛选: %s", w.Body.String())
	}
	w = do(e.h, "POST", "/v1/responses", headers, `{"model":"ocg-chat","input":"ping","max_output_tokens":1}`)
	if w.Code != http.StatusOK || stub.count() != 3 || stub.sentModel(2) != "longcat-2.0" {
		t.Fatalf("Chat 上游的 Responses 转换不可用: %d %s", w.Code, w.Body.String())
	}
}

// TestRouteKindGate（迭代 8 Phase 2）：文本入口只收 kind=text——视频模型在
// chat/messages 一律 404 model_not_found（不是 protocol_mismatch，不解释内部
// 原因），且一个字节都不发往上游。陷阱构造：视频模型挂 ark 型上游并给
// base_url 指向活的 stub——ark 服务 openai_chat，没有 kind 闸门这条请求就会
// 200。/v1/models 同口径不列视频模型。
func TestRouteKindGate(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatReply("seedance-side-id"))
	// ark 型 + base_url 覆盖（store 直建等价 dev 路径；管理 API 对产品类型
	// 不开放 base_url，测试走库是既有惯例）。
	upArk := dbUpstream(t, e.st, "ark-video", config.UpstreamArk, "sk-ark-not-real", stub.url)
	video := dbKindModel(t, e.st, "doubao-seedance-2-0-mini", store.ModelKindVideo)
	dbSource(t, e.st, video, upArk, "doubao-seedance-2-0-mini-260615", 100)

	t.Run("chat 入口 404 model_not_found", func(t *testing.T) {
		w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
			`{"model":"doubao-seedance-2-0-mini","messages":[]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
		}
		if _, code, _ := decodeError(t, w); code != "model_not_found" {
			t.Errorf("error.code = %q，期望 model_not_found（kind 闸门与不存在同响应）", code)
		}
		if stub.count() != 0 {
			t.Errorf("kind 不符的请求打到了上游 %d 次，期望 0", stub.count())
		}
	})

	t.Run("messages 入口 404 model_not_found", func(t *testing.T) {
		w := do(e.h, "POST", "/v1/messages", messagesAuth,
			`{"model":"doubao-seedance-2-0-mini","max_tokens":8,"messages":[]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
		}
		errType, msg := decodeAnthropicError(t, w)
		if errType != "not_found_error" {
			t.Errorf("error.type = %q，期望 not_found_error", errType)
		}
		// kind 闸门不给 protocol_mismatch 的指路提示（那会泄露这个名字在别的
		// 入口活着）。
		if strings.Contains(msg, "/v1/chat/completions") {
			t.Errorf("kind 闸门的 404 不应指向另一入口: %s", msg)
		}
	})

	t.Run("/v1/models 不列视频模型", func(t *testing.T) {
		w := do(e.h, "GET", "/v1/models", chatAuth, "")
		if w.Code != http.StatusOK {
			t.Fatalf("/v1/models 状态码 = %d，期望 200", w.Code)
		}
		if strings.Contains(w.Body.String(), "doubao-seedance") {
			t.Errorf("/v1/models 列出了视频模型: %s", w.Body.String())
		}
	})
}

// —— 故障切换（决策 5）——

// TestFailoverBeforeCommit：未向客户端写出任何字节前，首来源连接失败/429/5xx
// 都自动切下一优先级来源，客户端无感（响应 model 仍是请求名），
// 访问日志记 attempts 与最终来源名。
func TestFailoverBeforeCommit(t *testing.T) {
	cases := []struct {
		name string
		// primary 为 nil 表示首来源指向死端口（连接失败）
		primary func(t *testing.T) *stubUpstream
	}{
		{"连接失败", nil},
		{"429", func(t *testing.T) *stubUpstream {
			return newStub(t, jsonReply(http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`))
		}},
		{"500", func(t *testing.T) *stubUpstream {
			return newStub(t, jsonReply(http.StatusInternalServerError, `{"error":{"message":"boom"}}`))
		}},
		{"503", func(t *testing.T) *stubUpstream {
			return newStub(t, jsonReply(http.StatusServiceUnavailable, `{"error":{"message":"down"}}`))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRouteEnv(t)
			backup := newStub(t, chatReply("backup-side-id"))
			primaryURL := deadURL
			var primary *stubUpstream
			if c.primary != nil {
				primary = c.primary(t)
				primaryURL = primary.url
			}
			m := dbModel(t, e.st, "failover-model")
			dbSource(t, e.st, m, dbUpstream(t, e.st, "primary", config.UpstreamMock, "k1", primaryURL), "primary-side-id", 100)
			dbSource(t, e.st, m, dbUpstream(t, e.st, "backup", config.UpstreamMock, "k2", backup.url), "backup-side-id", 200)

			w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"failover-model","messages":[]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（透明切换到备用来源）；body: %s", w.Code, w.Body.String())
			}
			if backup.count() != 1 {
				t.Fatalf("备用来源请求数 = %d，期望 1", backup.count())
			}
			if primary != nil && primary.count() != 1 {
				t.Errorf("首来源请求数 = %d，期望 1（每来源至多一次）", primary.count())
			}
			// 请求体按新来源重建：model 改写为备用来源的来源侧 ID。
			if got := backup.sentModel(0); got != "backup-side-id" {
				t.Errorf("备用来源收到 model = %q，期望 backup-side-id（按来源重建请求体）", got)
			}
			if got := bodyModel(t, w); got != "failover-model" {
				t.Errorf("响应 model = %q，期望请求名 failover-model（客户端无感）", got)
			}
			// 上游错误体不得漏给客户端。
			for _, leak := range []string{"rate limited", "boom", "down"} {
				if strings.Contains(w.Body.String(), leak) {
					t.Errorf("被切换掉的上游响应体漏给客户端 %q: %s", leak, w.Body.String())
				}
			}
			access := findAccessRecord(t, e.logBuf.String(), "/v1/chat/completions")
			if access["attempts"] != float64(2) {
				t.Errorf("访问日志 attempts = %v，期望 2", access["attempts"])
			}
			if access["upstream"] != "backup" {
				t.Errorf("访问日志 upstream = %v，期望最终来源名 backup", access["upstream"])
			}
			if strings.Contains(e.logBuf.String(), "k1") || strings.Contains(e.logBuf.String(), "k2") {
				t.Errorf("日志泄露上游凭证:\n%s", e.logBuf.String())
			}
		})
	}
}

// TestNoFailoverOnClientError：非 429 的 4xx 立即提交原样透传，不试下一来源
// （换来源也是同样结果，切换只会掩盖客户端自己的错误）。
func TestNoFailoverOnClientError(t *testing.T) {
	e := newRouteEnv(t)
	const upBody = `{"error":{"message":"invalid max_tokens","type":"invalid_request_error"}}`
	primary := newStub(t, jsonReply(http.StatusBadRequest, upBody))
	backup := newStub(t, chatReply("backup"))
	m := dbModel(t, e.st, "client-error")
	dbSource(t, e.st, m, dbUpstream(t, e.st, "primary", config.UpstreamMock, "k1", primary.url), "", 100)
	dbSource(t, e.st, m, dbUpstream(t, e.st, "backup", config.UpstreamMock, "k2", backup.url), "", 200)

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"client-error","messages":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400 原样透传", w.Code)
	}
	if w.Body.String() != upBody {
		t.Errorf("上游错误体未原样透传: %s", w.Body.String())
	}
	if backup.count() != 0 {
		t.Errorf("非 429 的 4xx 不应触发切换，备用来源请求数 = %d", backup.count())
	}
}

// TestFailoverExhausted：候选全部失败时透传最后一个来源的错误——末来源连接
// 失败 → 502 upstream_unreachable；末来源 5xx → 原样透传其状态码与错误体。
func TestFailoverExhausted(t *testing.T) {
	t.Run("末来源连接失败 → 502", func(t *testing.T) {
		e := newRouteEnv(t)
		m := dbModel(t, e.st, "all-dead")
		dbSource(t, e.st, m, dbUpstream(t, e.st, "d1", config.UpstreamMock, "k1", deadURL), "", 100)
		dbSource(t, e.st, m, dbUpstream(t, e.st, "d2", config.UpstreamMock, "k2", deadURL), "", 200)

		w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"all-dead","messages":[]}`)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("状态码 = %d，期望 502；body: %s", w.Code, w.Body.String())
		}
		if _, code, _ := decodeError(t, w); code != "upstream_unreachable" {
			t.Errorf("error.code = %q，期望 upstream_unreachable", code)
		}
		access := findAccessRecord(t, e.logBuf.String(), "/v1/chat/completions")
		if access["attempts"] != float64(2) {
			t.Errorf("访问日志 attempts = %v，期望 2（每来源各试一次）", access["attempts"])
		}
	})

	t.Run("末来源 5xx → 原样透传", func(t *testing.T) {
		e := newRouteEnv(t)
		const lastBody = `{"error":{"message":"upstream exploded","type":"api_error"}}`
		first := newStub(t, jsonReply(http.StatusBadGateway, `{"error":{"message":"first"}}`))
		last := newStub(t, jsonReply(http.StatusInternalServerError, lastBody))
		m := dbModel(t, e.st, "all-5xx")
		dbSource(t, e.st, m, dbUpstream(t, e.st, "first", config.UpstreamMock, "k1", first.url), "", 100)
		dbSource(t, e.st, m, dbUpstream(t, e.st, "last", config.UpstreamMock, "k2", last.url), "", 200)

		w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"all-5xx","messages":[]}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("状态码 = %d，期望 500（末来源原样透传）；body: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != lastBody {
			t.Errorf("末来源错误体未原样透传: %s", w.Body.String())
		}
		if first.count() != 1 || last.count() != 1 {
			t.Errorf("请求分布 first=%d last=%d，期望各 1 次", first.count(), last.count())
		}
	})
}

// TestNoFailoverAfterStreamStarted：SSE 一旦开始回写就绝不切换——上游中途断流
// 时客户端拿到已到达的部分，网关不重试也不改写状态码。
func TestNoFailoverAfterStreamStarted(t *testing.T) {
	e := newRouteEnv(t)
	const firstChunk = "data: {\"model\":\"primary-260801\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	primary := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(firstChunk))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // 中途断流（net/http 约定的静默中止）
	})
	backup := newStub(t, chatReply("backup"))
	m := dbModel(t, e.st, "stream-model")
	dbSource(t, e.st, m, dbUpstream(t, e.st, "primary", config.UpstreamMock, "k1", primary.url), "", 100)
	dbSource(t, e.st, m, dbUpstream(t, e.st, "backup", config.UpstreamMock, "k2", backup.url), "", 200)

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		`{"model":"stream-model","stream":true,"messages":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（流已开始）；body: %s", w.Code, w.Body.String())
	}
	if backup.count() != 0 {
		t.Errorf("流已开始回写后不得切换来源，备用来源请求数 = %d", backup.count())
	}
	if !strings.Contains(w.Body.String(), `"model":"stream-model"`) {
		t.Errorf("已到达的 chunk 应改写 model 为请求名: %s", w.Body.String())
	}
}

// —— /v1/models 口径 ——

// TestServableModelsListing：只列「启用、且至少有一条启用来源（其上游也启用）」
// 的模型，原始名字典序；created 取模型建行时刻，不泄露上游信息。
func TestServableModelsListing(t *testing.T) {
	e := newRouteEnv(t)
	live := newStub(t, chatReply("x"))
	upLive := dbUpstream(t, e.st, "live-upstream", config.UpstreamMock, "sk-live-not-real", live.url)
	upOff := dbUpstream(t, e.st, "off-upstream", config.UpstreamMock, "sk-off-not-real", live.url)
	if err := e.st.SetUpstreamDisabled(t.Context(), upOff, true); err != nil {
		t.Fatalf("SetUpstreamDisabled: %v", err)
	}

	zeta := dbModel(t, e.st, "zeta")
	dbSource(t, e.st, zeta, upLive, "", 100)
	alpha := dbModel(t, e.st, "alpha")
	dbSource(t, e.st, alpha, upLive, "", 100)
	beta := dbModel(t, e.st, "beta-disabled") // 模型停用
	dbSource(t, e.st, beta, upLive, "", 100)
	if err := e.st.SetModelDisabled(t.Context(), beta, true); err != nil {
		t.Fatalf("SetModelDisabled: %v", err)
	}
	dbModel(t, e.st, "gamma-no-source") // 无来源
	delta := dbModel(t, e.st, "delta-source-off")
	srcDelta := dbSource(t, e.st, delta, upLive, "", 100)
	if err := e.st.SetModelSourceDisabled(t.Context(), srcDelta, true); err != nil {
		t.Fatalf("SetModelSourceDisabled: %v", err)
	}
	epsilon := dbModel(t, e.st, "epsilon-upstream-off")
	dbSource(t, e.st, epsilon, upOff, "", 100)

	w := do(e.h, "GET", "/v1/models", chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
		if m.Object != "model" || m.OwnedBy != "llmgate" {
			t.Errorf("条目形态异常: %+v", m)
		}
		if m.Created <= 0 {
			t.Errorf("created 应取模型建行时刻: %+v", m)
		}
	}
	if want := []string{"alpha", "zeta"}; !equalStrings(ids, want) {
		t.Errorf("模型名 = %v，期望 %v（字典序，且只列可服务的）", ids, want)
	}
	// created 与库内 created_at 一致。
	models, err := e.st.ListServableModels(t.Context())
	if err != nil {
		t.Fatalf("ListServableModels: %v", err)
	}
	if len(models) != len(list.Data) || models[0].CreatedAt.Unix() != list.Data[0].Created {
		t.Errorf("created = %d，期望模型 created_at %d", list.Data[0].Created, models[0].CreatedAt.Unix())
	}
	for _, leak := range []string{"live-upstream", "off-upstream", "sk-live-not-real", "127.0.0.1"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("/v1/models 泄露上游信息 %q: %s", leak, w.Body.String())
		}
	}
}

// —— count_tokens 逐来源尝试 ——

// TestCountTokensSourceFallthrough：某来源 404/405 视为「该来源无此端点」，
// 继续下一个来源；全部无果才本地粗估兜底。
func TestCountTokensSourceFallthrough(t *testing.T) {
	body := `{"model":"ct-model","messages":[{"role":"user","content":"hello"}]}`

	t.Run("首来源 404 → 下一来源透传", func(t *testing.T) {
		e := newRouteEnv(t)
		first := newStub(t, jsonReply(http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error"}}`))
		second := newStub(t, jsonReply(http.StatusOK, `{"input_tokens":123}`))
		m := dbModel(t, e.st, "ct-model")
		dbSource(t, e.st, m, dbUpstream(t, e.st, "first", config.UpstreamMock, "k1", first.url), "", 100)
		dbSource(t, e.st, m, dbUpstream(t, e.st, "second", config.UpstreamMock, "k2", second.url), "", 200)

		w := do(e.h, "POST", "/v1/messages/count_tokens", messagesAuth, body)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != `{"input_tokens":123}` {
			t.Errorf("未透传第二来源的上游计数: %s", w.Body.String())
		}
		if first.count() != 1 || second.count() != 1 {
			t.Errorf("请求分布 first=%d second=%d，期望各 1 次", first.count(), second.count())
		}
		if strings.Contains(e.logBuf.String(), "fallback_heuristic") {
			t.Error("有来源给出计数时不应触发本地粗估")
		}
	})

	t.Run("全部来源无端点 → 本地粗估", func(t *testing.T) {
		e := newRouteEnv(t)
		first := newStub(t, jsonReply(http.StatusNotFound, `{"type":"error"}`))
		second := newStub(t, jsonReply(http.StatusMethodNotAllowed, `{"type":"error"}`))
		m := dbModel(t, e.st, "ct-model")
		dbSource(t, e.st, m, dbUpstream(t, e.st, "first", config.UpstreamMock, "k1", first.url), "", 100)
		dbSource(t, e.st, m, dbUpstream(t, e.st, "second", config.UpstreamMock, "k2", second.url), "", 200)

		w := do(e.h, "POST", "/v1/messages/count_tokens", messagesAuth, body)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（粗估兜底）；body: %s", w.Code, w.Body.String())
		}
		var got struct {
			InputTokens int `json:"input_tokens"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
		}
		// messages 序列化为 [{"content":"hello","role":"user"}]（35 字符）→ ceil(35/4) = 9。
		if got.InputTokens != 9 {
			t.Errorf("input_tokens = %d，期望 9", got.InputTokens)
		}
		if first.count() != 1 || second.count() != 1 {
			t.Errorf("请求分布 first=%d second=%d，期望各 1 次（逐来源试尽）", first.count(), second.count())
		}
		if !strings.Contains(e.logBuf.String(), `"count_tokens_source":"fallback_heuristic"`) {
			t.Errorf("缺 count_tokens_source=fallback_heuristic 日志:\n%s", e.logBuf.String())
		}
	})

	t.Run("全部来源连不上 → 502", func(t *testing.T) {
		e := newRouteEnv(t)
		m := dbModel(t, e.st, "ct-model")
		dbSource(t, e.st, m, dbUpstream(t, e.st, "d1", config.UpstreamMock, "k1", deadURL), "", 100)
		dbSource(t, e.st, m, dbUpstream(t, e.st, "d2", config.UpstreamMock, "k2", deadURL), "", 200)

		w := do(e.h, "POST", "/v1/messages/count_tokens", messagesAuth, body)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("状态码 = %d，期望 502（不拿粗估掩盖上游不可达）；body: %s", w.Code, w.Body.String())
		}
	})

	// 混合失败：只要有来源"本可能给出真计数却没答上"，就不能拿粗估糊弄——
	// 粗估只在**每个**来源都明确说"没有这个端点"时才成立。
	t.Run("404 + 连不上 → 502 而非粗估", func(t *testing.T) {
		e := newRouteEnv(t)
		first := newStub(t, jsonReply(http.StatusNotFound, `{"type":"error"}`))
		m := dbModel(t, e.st, "ct-model")
		dbSource(t, e.st, m, dbUpstream(t, e.st, "first", config.UpstreamMock, "k1", first.url), "", 100)
		dbSource(t, e.st, m, dbUpstream(t, e.st, "dead", config.UpstreamMock, "k2", deadURL), "", 200)

		w := do(e.h, "POST", "/v1/messages/count_tokens", messagesAuth, body)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("状态码 = %d，期望 502（有来源是连不上而不是无端点）；body: %s", w.Code, w.Body.String())
		}
		if strings.Contains(e.logBuf.String(), "fallback_heuristic") {
			t.Error("混合失败不应触发本地粗估")
		}
	})

	t.Run("5xx + 404 → 502 而非粗估", func(t *testing.T) {
		e := newRouteEnv(t)
		first := newStub(t, jsonReply(http.StatusInternalServerError, `{"type":"error"}`))
		second := newStub(t, jsonReply(http.StatusNotFound, `{"type":"error"}`))
		m := dbModel(t, e.st, "ct-model")
		dbSource(t, e.st, m, dbUpstream(t, e.st, "first", config.UpstreamMock, "k1", first.url), "", 100)
		dbSource(t, e.st, m, dbUpstream(t, e.st, "second", config.UpstreamMock, "k2", second.url), "", 200)

		w := do(e.h, "POST", "/v1/messages/count_tokens", messagesAuth, body)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("状态码 = %d，期望 502（首来源 5xx 本可能给出真计数）；body: %s", w.Code, w.Body.String())
		}
		if strings.Contains(e.logBuf.String(), "fallback_heuristic") {
			t.Error("混合失败不应触发本地粗估")
		}
	})
}

// TestRouteEntrySwitches（0013 调用入口开关）：关掉的入口对客户端 404——
// 另一入口开着且真有能服务它的启用来源才给 protocol_mismatch 指路，否则
// 一律 model_not_found，从不把客户端指向一个同样调不通的入口；双关模型
// 不进 /v1/models（谓词与选路同口径）。
func TestRouteEntrySwitches(t *testing.T) {
	e := newRouteEnv(t)
	dual := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			jsonReply(http.StatusOK, `{"id":"msg_1","type":"message","model":"chat-off-model","content":[]}`)(w, r)
			return
		}
		chatReply("chat-off-model")(w, r)
	})
	upMock := dbUpstream(t, e.st, "mock-dual", config.UpstreamMock, "k", dual.url)
	upArk := dbUpstream(t, e.st, "ark-only", config.UpstreamArk, "sk-ark-not-real", "")

	chatOff := dbModel(t, e.st, "chat-off-model")
	dbSource(t, e.st, chatOff, upMock, "", 100)
	if err := e.st.SetModelEntries(t.Context(), chatOff, false, false, true); err != nil {
		t.Fatalf("SetModelEntries: %v", err)
	}
	arkChatOff := dbModel(t, e.st, "ark-chat-off")
	dbSource(t, e.st, arkChatOff, upArk, "", 100)
	if err := e.st.SetModelEntries(t.Context(), arkChatOff, false, false, true); err != nil {
		t.Fatalf("SetModelEntries: %v", err)
	}

	t.Run("关掉的入口 404 并指向开着的另一入口", func(t *testing.T) {
		w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"chat-off-model","messages":[]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
		}
		if _, code, msg := decodeError(t, w); code != "protocol_mismatch" || !strings.Contains(msg, "/v1/messages") {
			t.Errorf("期望 protocol_mismatch 指向 /v1/messages，得到 code=%q msg=%q", code, msg)
		}
	})
	t.Run("开着的入口照常服务", func(t *testing.T) {
		w := do(e.h, "POST", "/v1/messages", messagesAuth, `{"model":"chat-off-model","max_tokens":8,"messages":[]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
	})
	t.Run("另一入口开着但无来源能服务时不指路", func(t *testing.T) {
		// ark 型只服务 chat 而 chat 开关关着；messages 开着却没有能服务它的
		// 来源——两个入口都调不通，双向都必须是 model_not_found。
		w := do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"ark-chat-off","messages":[]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("chat 状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
		}
		if _, code, _ := decodeError(t, w); code != "model_not_found" {
			t.Errorf("chat error.code = %q，期望 model_not_found（messages 无可用来源，不指路）", code)
		}
		w = do(e.h, "POST", "/v1/messages", messagesAuth, `{"model":"ark-chat-off","max_tokens":8,"messages":[]}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("messages 状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
		}
		if errType, _ := decodeAnthropicError(t, w); errType != "not_found_error" {
			t.Errorf("messages error.type = %q，期望 not_found_error（chat 开关关着，不互指）", errType)
		}
	})
	t.Run("双关模型不进 /v1/models，单关照列", func(t *testing.T) {
		bothOff := dbModel(t, e.st, "both-off-model")
		dbSource(t, e.st, bothOff, upMock, "", 100)
		if err := e.st.SetModelEntries(t.Context(), bothOff, false, false, false); err != nil {
			t.Fatalf("SetModelEntries: %v", err)
		}
		w := do(e.h, "GET", "/v1/models", chatAuth, "")
		if w.Code != http.StatusOK {
			t.Fatalf("/v1/models 状态码 = %d", w.Code)
		}
		if body := w.Body.String(); strings.Contains(body, "both-off-model") {
			t.Errorf("/v1/models 不该列出双关模型: %s", body)
		} else if !strings.Contains(body, "chat-off-model") {
			t.Errorf("/v1/models 应照列单关模型: %s", body)
		}
	})
}
