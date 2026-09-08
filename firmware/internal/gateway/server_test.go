// server_test.go 验证网关对外承诺的 HTTP 行为：
// 双头认证与 401 错误形态（chat 系 OpenAI 风格 / messages 系 Anthropic 风格）、
// /v1/models 只暴露模型名（不泄露上游账户与来源侧 ID）、/healthz 免鉴权、
// /v1/messages 与 count_tokens 透传（含粗估兜底与响应 model 改写回请求名），
// 以及访问日志在 HTTP 层同样遵守 §15.1 脱敏硬规则。
// 动态选路/故障切换/协议过滤的用例在 routing_test.go。
package gateway_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// TestRunRejectsInvalidTLSMaterialBeforeListening 钉住启动顺序：静态证书有误时
// 必须在打开 HTTP/TLS 监听器之前失败，不能留下只启动了半边的进程。
func TestRunRejectsInvalidTLSMaterialBeforeListening(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Listen: "127.0.0.1:1",
		TLS: config.TLS{
			Listen:   "127.0.0.1:2",
			CertFile: filepath.Join(dir, "missing-cert.pem"),
			KeyFile:  filepath.Join(dir, "missing-key.pem"),
		},
	}
	s := gateway.New(cfg, logging.New(io.Discard, slog.LevelDebug), nil, nil, nil, nil)
	err := s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "加载 TLS 证书或私钥") {
		t.Fatalf("坏证书应在监听前失败，得到 %v", err)
	}
	if strings.Contains(err.Error(), "监听 127.0.0.1") {
		t.Fatalf("错误已进入监听阶段，启动顺序不对: %v", err)
	}
}

const (
	testKey = "sk_test_0123456789abcdef"
	// testKeyDisplay 是 testKey 的展示串（displayParts 取前 12 位 + 末 4 位）：
	// 访问日志的调用方标识就是它，明文永不出现。
	testKeyDisplay  = "sk_test_0123…cdef"
	testUpstreamKey = "sk-upstream-secret-fedcba98"
)

// newTestHandler 装配一个挂着单个 mock 上游的 handler，日志写入返回的 buffer
// （debug 级别：脱敏在最宽松级别也必须成立）。upstreamURL 是 mock 上游端点
// （转发用例传 httptest server 的 URL；不触发转发的用例传空串，落到一个不会被
// 拨号的占位地址）。
func newTestHandler(t *testing.T, upstreamURL string) (http.Handler, *bytes.Buffer) {
	h, logBuf, _ := newTestEnv(t, upstreamURL)
	return h, logBuf
}

// newTestEnv 同 newTestHandler，另返回底层 store——数据面的 Key 鉴权与模型
// 选路都查 SQLite，测试可直接操作 Key/用户/模型/来源行。
func newTestEnv(t *testing.T, upstreamURL string) (http.Handler, *bytes.Buffer, *store.Store) {
	return newTestEnvWithKeys(t, upstreamURL, []config.APIKey{{Key: testKey}})
}

// newTestEnvWithKeys 是装配核心：打开临时 SQLite、幂等导入 apiKeys、建一个
// mock 上游与三个模型，再以 StoreKeyAuthorizer 装配 handler（与 cmd/llmgate
// runGatewayd 同构）。三个模型的来源都显式给了与模型名不同的 upstream_model_id
// （chat-strong→mock-gpt-4o 等），请求侧改写与响应侧回写因此都可证。
func newTestEnvWithKeys(t *testing.T, upstreamURL string, apiKeys []config.APIKey) (http.Handler, *bytes.Buffer, *store.Store) {
	t.Helper()
	if upstreamURL == "" {
		upstreamURL = "http://127.0.0.1:1/v1" // 占位；用到即测试出错
	}
	cfg := &config.Config{
		Listen:   "127.0.0.1:0",
		DataDir:  t.TempDir(),
		LogLevel: "debug",
		APIKeys:  apiKeys,
	}
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, slog.LevelDebug)
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if _, _, err := gateway.ImportConfigKeys(t.Context(), st, cfg.APIKeys, logger); err != nil {
		t.Fatalf("ImportConfigKeys: %v", err)
	}

	up := dbUpstream(t, st, "mock-main", config.UpstreamMock, testUpstreamKey, upstreamURL)
	for name, upstreamModelID := range map[string]string{
		"chat-strong": "mock-gpt-4o",
		"chat-fast":   "mock-gpt-4o-mini",
		"cc-fast":     "mock-claude-mini",
	} {
		dbSource(t, st, dbModel(t, st, name), up, upstreamModelID, 100)
	}

	s := gateway.New(cfg, logger, st, gateway.NewStoreKeyAuthorizer(st, logger), nil, nil)
	return s.Handler(), &logBuf, st
}

// dbUpstream / dbModel / dbSource 是目录三表的建行捷径（等价管理面创建）。
func dbUpstream(t *testing.T, st *store.Store, name, typ, apiKey, baseURL string) int64 {
	t.Helper()
	u, err := st.CreateUpstream(t.Context(), name, typ, apiKey, baseURL)
	if err != nil {
		t.Fatalf("CreateUpstream %s: %v", name, err)
	}
	return u.ID
}

func dbModel(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	return dbKindModel(t, st, name, store.ModelKindText)
}

func dbKindModel(t *testing.T, st *store.Store, name, kind string) int64 {
	t.Helper()
	m, err := st.CreateModel(t.Context(), name, kind, "")
	if err != nil {
		t.Fatalf("CreateModel %s: %v", name, err)
	}
	return m.ID
}

func dbSource(t *testing.T, st *store.Store, modelID, upstreamID int64, upstreamModelID string, priority int64) int64 {
	t.Helper()
	src, err := st.CreateModelSource(t.Context(), modelID, upstreamID, upstreamModelID, priority)
	if err != nil {
		t.Fatalf("CreateModelSource: %v", err)
	}
	return src.ID
}

func do(h http.Handler, method, path string, header map[string]string, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// decodeError 解析 OpenAI 风格错误体并断言三字段齐全。
func decodeError(t *testing.T, w *httptest.ResponseRecorder) (typ, code, msg string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("错误响应 Content-Type = %q，期望 application/json", ct)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 JSON: %v\n%s", err, w.Body.String())
	}
	if body.Error.Message == "" || body.Error.Type == "" || body.Error.Code == "" {
		t.Errorf("OpenAI 风格错误体字段不全: %s", w.Body.String())
	}
	return body.Error.Type, body.Error.Code, body.Error.Message
}

// TestAuthAccepted：Authorization: Bearer 与 x-api-key 两种头都被接受。
func TestAuthAccepted(t *testing.T) {
	h, _ := newTestHandler(t, "")
	for name, header := range map[string]map[string]string{
		"bearer":    {"Authorization": "Bearer " + testKey},
		"x-api-key": {"x-api-key": testKey},
	} {
		t.Run(name, func(t *testing.T) {
			if w := do(h, "GET", "/v1/models", header, ""); w.Code != http.StatusOK {
				t.Errorf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAuthRejected：无 Key / 错 Key 得 401 + OpenAI 风格错误体，且响应带 request-id。
func TestAuthRejected(t *testing.T) {
	h, logBuf := newTestHandler(t, "")
	wrongKey := "sk_wrong_key_000000000000"
	cases := []struct {
		name     string
		header   map[string]string
		wantCode string
	}{
		{"无凭证", nil, "missing_api_key"},
		{"错误 Bearer Key", map[string]string{"Authorization": "Bearer " + wrongKey}, "invalid_api_key"},
		{"错误 x-api-key", map[string]string{"x-api-key": wrongKey}, "invalid_api_key"},
		{"非 Bearer 授权头", map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, "missing_api_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(h, "GET", "/v1/models", c.header, "")
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，期望 401", w.Code)
			}
			typ, code, _ := decodeError(t, w)
			if typ != "invalid_request_error" || code != c.wantCode {
				t.Errorf("error.type/code = %q/%q，期望 invalid_request_error/%q", typ, code, c.wantCode)
			}
			if id := w.Header().Get("X-Request-Id"); len(id) != 16 {
				t.Errorf("X-Request-Id = %q，期望 16 位十六进制", id)
			}
		})
	}
	// 认证失败路径的日志（含 debug「认证失败」记录）不得出现错误 Key 明文。
	if out := logBuf.String(); strings.Contains(out, wrongKey) {
		t.Errorf("日志泄露了客户端提交的 Key 明文:\n%s", out)
	}
}

// TestModels：OpenAI list 格式，只含逻辑模型名（字典序），
// 不泄露上游账户名、真实模型 ID 与上游 Key（§11.1）。
func TestModels(t *testing.T) {
	h, _ := newTestHandler(t, "")
	w := do(h, "GET", "/v1/models", map[string]string{"Authorization": "Bearer " + testKey}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if list.Object != "list" {
		t.Errorf("object = %q，期望 list", list.Object)
	}
	var ids []string
	for _, m := range list.Data {
		if m.Object != "model" {
			t.Errorf("data[].object = %q，期望 model", m.Object)
		}
		ids = append(ids, m.ID)
	}
	// anthropic 协议模型（cc-fast）与 openai 协议模型同列。
	if want := []string{"cc-fast", "chat-fast", "chat-strong"}; !equalStrings(ids, want) {
		t.Errorf("模型名 = %v，期望 %v（字典序）", ids, want)
	}
	for _, leak := range []string{"mock-main", "mock-gpt-4o", "mock-claude", testUpstreamKey, "18080"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("/v1/models 泄露上游信息 %q:\n%s", leak, w.Body.String())
		}
	}
}

// TestHealthz：存活探针免鉴权。
func TestHealthz(t *testing.T) {
	h, _ := newTestHandler(t, "")
	if w := do(h, "GET", "/healthz", nil, ""); w.Code != http.StatusOK {
		t.Errorf("/healthz 状态码 = %d，期望 200", w.Code)
	}
}

// TestUnknownV1Path：/v1 下未实现路径先认证再 404（OpenAI 风格 error body）。
func TestUnknownV1Path(t *testing.T) {
	h, _ := newTestHandler(t, "")
	w := do(h, "POST", "/v1/embeddings",
		map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
		`{"model":"chat-fast"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（embeddings 未实装）", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "not_found" {
		t.Errorf("error.code = %q，期望 not_found", code)
	}
	// 未认证时同一路径必须 401 而不是 404。
	if w := do(h, "POST", "/v1/embeddings", nil, "{}"); w.Code != http.StatusUnauthorized {
		t.Errorf("无凭证状态码 = %d，期望 401", w.Code)
	}
}

// TestAdminMountDispatch：单端口决策（2026-08-06）——管理面 handler 挂载进
// 网关根路由后按前缀分发：/v1/* 与 /healthz 留在数据面（中间件链不变），
// 其余路径（/、/admin/*、未知路径）整链交给管理面——数据面的 Key 认证与
// 404 风格对这些路径不再生效。
func TestAdminMountDispatch(t *testing.T) {
	cfg := &config.Config{Listen: "127.0.0.1:0", DataDir: t.TempDir(), LogLevel: "debug"}
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, slog.LevelDebug)
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stub-Admin", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	h := gateway.New(cfg, logger, st, gateway.NewStoreKeyAuthorizer(st, logger), stub, nil).Handler()

	// 管理面侧：无数据面凭证也直达管理面 handler（此处以 418 佐证），根路径、
	// 管理 API、管理界面与任意未知路径都归它。
	for _, path := range []string{"/", "/admin/", "/admin/v1/login", "/unknown-path"} {
		w := do(h, "GET", path, nil, "")
		if w.Code != http.StatusTeapot || w.Header().Get("X-Stub-Admin") != path {
			t.Errorf("GET %s = %d（X-Stub-Admin=%q），期望整链交给管理面", path, w.Code, w.Header().Get("X-Stub-Admin"))
		}
	}
	// 根分发同样不按 Host 筛选：客户自己的域名经路由器或反代转进来时，
	// 原样进入管理面 handler，不要求设备预先登记这个域名。
	r := httptest.NewRequest(http.MethodGet, "http://customer-gateway.example/ui/", nil)
	r.Host = "customer-gateway.example"
	wHost := httptest.NewRecorder()
	h.ServeHTTP(wHost, r)
	if wHost.Code != http.StatusTeapot || wHost.Header().Get("X-Stub-Admin") != "/ui/" {
		t.Errorf("自定义 Host 未进入管理面：status=%d X-Stub-Admin=%q", wHost.Code, wHost.Header().Get("X-Stub-Admin"))
	}
	// 数据面侧不受挂载影响：/healthz 免鉴权 200；/v1/* 与 Codex
	// 混合面无凭证 401 且保持 OpenAI 风格错误体（不落进管理面的 JSON 404）。
	if w := do(h, "GET", "/healthz", nil, ""); w.Code != http.StatusOK {
		t.Errorf("/healthz 状态码 = %d，期望 200", w.Code)
	}
	w := do(h, "GET", "/v1/models", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/models 无凭证状态码 = %d，期望 401", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "missing_api_key" {
		t.Errorf("error.code = %q，期望 missing_api_key", code)
	}
	w = do(h, "GET", "/agents/codex/v1/models", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("/agents/codex/v1/models 无凭证状态码 = %d，期望 401", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "missing_api_key" {
		t.Errorf("Codex 混合面 error.code = %q，期望 missing_api_key", code)
	}
}

// TestAccessLogRedaction：访问日志记录方法/路径/状态/时长/两侧 body 元数据，
// 但绝不含 body 内容与 Key 明文——debug 级别也一样（§15.1）。chat 透传路径
// （Phase 4）请求侧计整读长度、响应侧计上游响应长度；未读 body 的路径计 0。
func TestAccessLogRedaction(t *testing.T) {
	const upstreamMarker = "上游绝密响应内容"
	upstreamBody := `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"` +
		upstreamMarker + `"},"finish_reason":"stop"}]}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(upstreamBody))
	}))
	defer up.Close()

	h, logBuf := newTestHandler(t, up.URL+"/v1")
	secretBody := `{"model":"chat-fast","messages":[{"role":"user","content":"绝密提示词内容"}]}`
	if w := do(h, "POST", "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
		secretBody); w.Code != http.StatusOK {
		t.Fatalf("chat 状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	// 未实现路径：handler 不读 body，请求侧应计 0 字节。
	do(h, "POST", "/v1/embeddings",
		map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
		secretBody)

	out := logBuf.String()
	for label, s := range map[string]string{
		"请求体全文":     secretBody,
		"提示词片段":     "绝密提示词",
		"上游响应内容":    upstreamMarker,
		"API密钥 明文":  testKey,
		"上游 Key 明文": testUpstreamKey,
	} {
		if strings.Contains(out, s) {
			t.Errorf("访问日志泄露%s %q:\n%s", label, s, out)
		}
	}

	access := findAccessRecord(t, out, "/v1/chat/completions")
	if access["method"] != "POST" {
		t.Errorf("access 记录 method 异常: %v", access)
	}
	if access["status"] != float64(http.StatusOK) {
		t.Errorf("access 记录 status = %v，期望 200", access["status"])
	}
	if access["key"] != testKeyDisplay {
		t.Errorf("access 记录 key = %v，期望 %q", access["key"], testKeyDisplay)
	}
	if _, ok := access["duration_ms"].(float64); !ok {
		t.Errorf("access 记录缺 duration_ms: %v", access)
	}
	req, ok := access["req"].(map[string]any)
	if !ok {
		t.Fatalf("access 记录缺 req 元数据组: %v", access)
	}
	// chat 处理程序整读 body：请求侧长度等于请求体长度。
	if req["body_len"] != float64(len(secretBody)) {
		t.Errorf("req.body_len = %v，期望 %d", req["body_len"], len(secretBody))
	}
	if req["content_type"] != "application/json" {
		t.Errorf("req.content_type = %v", req["content_type"])
	}
	resp, ok := access["resp"].(map[string]any)
	if !ok || resp["body_len"] != float64(len(upstreamBody)) {
		t.Errorf("resp.body_len 应等于上游响应长度 %d: %v", len(upstreamBody), access)
	}

	notRead := findAccessRecord(t, out, "/v1/embeddings")
	if req2, ok := notRead["req"].(map[string]any); !ok || req2["body_len"] != float64(0) {
		t.Errorf("未读 body 的路径 req.body_len 应为 0: %v", notRead)
	}
}

// findAccessRecord 从日志输出中找出 msg=="access" 且 path 匹配的 JSON 记录。
func findAccessRecord(t *testing.T, out, path string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("日志行不是 JSON: %v\n%s", err, line)
		}
		if rec["msg"] == "access" && rec["path"] == path {
			return rec
		}
	}
	t.Fatal("未找到 path=" + path + " 的 access 日志记录:\n" + out)
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// —— iteration-3 Phase 3：/v1/messages 与 count_tokens ——

// decodeAnthropicError 解析 Anthropic 风格错误体
// {"type":"error","error":{"type","message"}} 并断言形态完整。
func decodeAnthropicError(t *testing.T, w *httptest.ResponseRecorder) (errType, msg string) {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("错误响应 Content-Type = %q，期望 application/json", ct)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误体不是 JSON: %v\n%s", err, w.Body.String())
	}
	if body.Type != "error" || body.Error.Type == "" || body.Error.Message == "" {
		t.Errorf("Anthropic 风格错误体字段不全: %s", w.Body.String())
	}
	return body.Error.Type, body.Error.Message
}

// TestMessagesAuthAccepted：Authorization: Bearer 与 x-api-key 两种客户端
// 鉴权头都能过 messages 入口并触达上游。
func TestMessagesAuthAccepted(t *testing.T) {
	upstreamBody := `{"id":"msg_1","type":"message","role":"assistant","model":"mock-claude-mini-260801",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	up, _ := startUpstream(t, http.StatusOK,
		map[string]string{"Content-Type": "application/json"}, upstreamBody)
	h, _ := newTestHandler(t, up.URL+"/v1")
	for name, header := range map[string]map[string]string{
		"bearer":    {"Authorization": "Bearer " + testKey},
		"x-api-key": {"x-api-key": testKey},
	} {
		t.Run(name, func(t *testing.T) {
			header["Content-Type"] = "application/json"
			w := do(h, "POST", "/v1/messages", header,
				`{"model":"cc-fast","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusOK {
				t.Errorf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestMessagesAuthRejected：messages 系入口（messages 与 count_tokens）的
// 401 是 Anthropic 风格 authentication_error（chat 系仍是 OpenAI 风格，由
// TestAuthRejected 覆盖）。
func TestMessagesAuthRejected(t *testing.T) {
	h, logBuf := newTestHandler(t, "")
	wrongKey := "sk_wrong_key_000000000000"
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for name, header := range map[string]map[string]string{
			"无凭证":    nil,
			"错误 Key": {"x-api-key": wrongKey},
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				w := do(h, "POST", path, header, `{"model":"cc-fast","messages":[]}`)
				if w.Code != http.StatusUnauthorized {
					t.Fatalf("状态码 = %d，期望 401；body: %s", w.Code, w.Body.String())
				}
				if errType, _ := decodeAnthropicError(t, w); errType != "authentication_error" {
					t.Errorf("error.type = %q，期望 authentication_error", errType)
				}
			})
		}
	}
	if out := logBuf.String(); strings.Contains(out, wrongKey) {
		t.Errorf("日志泄露了客户端提交的 Key 明文:\n%s", out)
	}
}

// TestMessagesNonStreamPassthrough：messages 非流式全链路——请求侧 model
// 重写为上游 ID、未知字段与大整数原样抵达上游、客户端 x-api-key 换成上游
// Bearer、anthropic-* 业务头透传；响应侧 model 改写回逻辑名（§11.1），其余
// 字段（未知字段 x_mock_extra 与非常规 stop_reason x_mock_pause）原样保留。
func TestMessagesNonStreamPassthrough(t *testing.T) {
	upstreamBody := `{"id":"msg_1","type":"message","role":"assistant","model":"mock-claude-mini-260801",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"x_mock_pause","stop_sequence":null,` +
		`"usage":{"input_tokens":17,"output_tokens":25,"cache_read_input_tokens":3},"x_mock_extra":"passthrough"}`
	up, urec := startUpstream(t, http.StatusOK,
		map[string]string{"Content-Type": "application/json", "X-Upstream-Extra": "yes"}, upstreamBody)

	h, _ := newTestHandler(t, up.URL+"/v1")
	w := do(h, "POST", "/v1/messages", map[string]string{
		"x-api-key":         testKey, // anthropic 客户端习惯的鉴权头
		"Content-Type":      "application/json",
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "token-counting-2024-11-01",
	}, `{"model":"cc-fast","max_tokens":32,"messages":[{"role":"user","content":"hello"}],`+
		`"x_client_extra":{"nested":[1,2]},"x_big":9007199254740993}`)

	// —— 响应侧：model 改回逻辑名，其余字段原样保留 ——
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	if got["model"] != "cc-fast" {
		t.Errorf(`响应 model = %v，期望改写回逻辑名 "cc-fast"`, got["model"])
	}
	if strings.Contains(w.Body.String(), "mock-claude-mini") {
		t.Errorf("响应泄露上游真实模型 ID: %s", w.Body.String())
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(upstreamBody), &want); err != nil {
		t.Fatalf("canned body 不是 JSON: %v", err)
	}
	want["model"] = "cc-fast"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("除 model 外应与上游响应逐字段一致（未知字段与非常规 stop_reason 保留）:\ngot:  %v\nwant: %v", got, want)
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
	if sent["model"] != "mock-claude-mini" {
		t.Errorf("model 未重写为上游 ID: %v", sent["model"])
	}
	if _, has := sent["x_client_extra"]; !has {
		t.Errorf("未知请求字段丢失: %s", upBody)
	}
	if !strings.Contains(string(upBody), "9007199254740993") {
		t.Errorf("大整数经透传后失真: %s", upBody)
	}
	if got := upHeader.Get("Authorization"); got != "Bearer "+testUpstreamKey {
		t.Errorf("Authorization 未重写为上游 Key（anthropic 上游统一 Bearer）: %q", got)
	}
	if got := upHeader.Get("x-api-key"); got != "" {
		t.Errorf("客户端 x-api-key 不应透传给上游: %q", got)
	}
	if got := upHeader.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version 业务头应透传: %q", got)
	}
	if got := upHeader.Get("anthropic-beta"); got != "token-counting-2024-11-01" {
		t.Errorf("anthropic-beta 业务头应透传: %q", got)
	}
}

// TestMessagesStreamSSE：messages SSE 全链路——带 event: 名行的 anthropic
// 事件帧完整到 message_stop 且顺序不变；message_start 的 message.model 改写
// 回逻辑名（§11.1）且未知字段保留，其余事件行逐字节原样转发，逐块 Flush。
func TestMessagesStreamSSE(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"mock-claude-mini-260801\"},\"x_mock_extra\":\"e\"}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	up, _ := startUpstream(t, http.StatusOK,
		map[string]string{"Content-Type": "text/event-stream"}, sse)

	h, _ := newTestHandler(t, up.URL+"/v1")
	w := do(h, "POST", "/v1/messages",
		map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
		`{"model":"cc-fast","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q，期望 text/event-stream", ct)
	}
	out := w.Body.String()
	if strings.Contains(out, "mock-claude-mini") {
		t.Errorf("SSE 输出泄露上游真实模型 ID:\n%s", out)
	}
	// 事件序列完整且顺序不变（event: 行原样转发）。
	idx := 0
	for _, ev := range []string{"message_start", "ping", "content_block_start",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		p := strings.Index(out[idx:], "event: "+ev+"\n")
		if p < 0 {
			t.Fatalf("事件 %s 缺失或顺序错误:\n%s", ev, out)
		}
		idx += p
	}
	// message_start 的 message.model 改写为逻辑名，id 与未知字段保留。
	var start map[string]any
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "message_start") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &start); err != nil {
				t.Fatalf("message_start 载荷不是 JSON: %v\n%s", err, line)
			}
			break
		}
	}
	if start == nil {
		t.Fatalf("未找到 message_start 数据行:\n%s", out)
	}
	msg, _ := start["message"].(map[string]any)
	if msg == nil || msg["model"] != "cc-fast" {
		t.Errorf(`message_start 的 message.model = %v，期望 "cc-fast"`, start["message"])
	}
	if msg != nil && msg["id"] != "msg_1" {
		t.Errorf("message_start 的 message.id 应保留: %v", msg)
	}
	if start["x_mock_extra"] != "e" {
		t.Errorf("message_start 未知字段 x_mock_extra 丢失: %v", start)
	}
	// 无 model 的事件行逐字节原样转发（ping 与 message_delta 抽查）。
	for _, keep := range []string{
		"event: ping\ndata: {\"type\":\"ping\"}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("事件未原样保留 %q:\n%s", keep, out)
		}
	}
	if !w.Flushed {
		t.Error("SSE 响应应被逐块 Flush")
	}
}

// TestCountTokensPassthrough：上游有 count_tokens 端点时原样透传（请求侧
// model 同样重写为上游 ID）；非 404/405 的上游错误也原样透传、不触发粗估。
func TestCountTokensPassthrough(t *testing.T) {
	t.Run("200 透传", func(t *testing.T) {
		upstreamBody := `{"input_tokens":123}`
		up, urec := startUpstream(t, http.StatusOK,
			map[string]string{"Content-Type": "application/json"}, upstreamBody)
		h, _ := newTestHandler(t, up.URL+"/v1")
		w := do(h, "POST", "/v1/messages/count_tokens",
			map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
			`{"model":"cc-fast","messages":[{"role":"user","content":"hello"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != upstreamBody {
			t.Errorf("count_tokens 响应未原样透传: %s", w.Body.String())
		}
		_, upBody := urec.get()
		var sent map[string]any
		if err := json.Unmarshal(upBody, &sent); err != nil {
			t.Fatalf("上游收到的 body 不是 JSON: %v", err)
		}
		if sent["model"] != "mock-claude-mini" {
			t.Errorf("count_tokens 请求 model 未重写为上游 ID: %v", sent["model"])
		}
	})
	t.Run("500 原样透传不触发粗估", func(t *testing.T) {
		upstreamBody := `{"type":"error","error":{"type":"api_error","message":"upstream exploded"}}`
		up, _ := startUpstream(t, http.StatusInternalServerError,
			map[string]string{"Content-Type": "application/json"}, upstreamBody)
		h, logBuf := newTestHandler(t, up.URL+"/v1")
		w := do(h, "POST", "/v1/messages/count_tokens",
			map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
			`{"model":"cc-fast","messages":[{"role":"user","content":"hello"}]}`)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("状态码 = %d，期望 500 原样透传；body: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != upstreamBody {
			t.Errorf("上游错误体未原样透传: %s", w.Body.String())
		}
		if strings.Contains(logBuf.String(), "fallback_heuristic") {
			t.Error("非 404/405 不应触发本地粗估")
		}
	})
}

// TestCountTokensFallback：上游 404/405 视为无 count_tokens 端点——网关本地
// 粗估兜底返回 200 {"input_tokens":N}（messages+system JSON 字符数/4 向上
// 取整），上游错误体不透传，日志标注 count_tokens_source="fallback_heuristic"。
func TestCountTokensFallback(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(fmt.Sprintf("上游 %d", status), func(t *testing.T) {
			up, _ := startUpstream(t, status,
				map[string]string{"Content-Type": "application/json"},
				`{"type":"error","error":{"type":"not_found_error","message":"no such endpoint"}}`)
			h, logBuf := newTestHandler(t, up.URL+"/v1")
			// messages 序列化为 [{"content":"hello","role":"user"}]（35 字符）、
			// 无 system → ceil(35/4) = 9。
			w := do(h, "POST", "/v1/messages/count_tokens",
				map[string]string{"Authorization": "Bearer " + testKey, "Content-Type": "application/json"},
				`{"model":"cc-fast","messages":[{"role":"user","content":"hello"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（粗估兜底）；body: %s", w.Code, w.Body.String())
			}
			var got struct {
				InputTokens int `json:"input_tokens"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
			}
			if got.InputTokens != 9 {
				t.Errorf("input_tokens = %d，期望 9（messages JSON 35 字符 /4 向上取整）", got.InputTokens)
			}
			if strings.Contains(w.Body.String(), "no such endpoint") {
				t.Errorf("上游 404/405 错误体不应透传: %s", w.Body.String())
			}
			if !strings.Contains(logBuf.String(), `"count_tokens_source":"fallback_heuristic"`) {
				t.Errorf("缺 count_tokens_source=fallback_heuristic 日志:\n%s", logBuf.String())
			}
		})
	}
}

// —— iteration-4 Phase 4：数据面 Key 鉴权切换 SQLite ——

// digestOf 是 Key 明文的 SHA-256 十六进制摘要（库中 key_digest 的唯一形态，
// 与管理面签发同一算法）。
func digestOf(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// dbIssueKey 直接经 store 为用户签发一把 Key（等价管理面签发：库存摘要，
// 明文只在测试内存在），用户不存在时先建；返回 Key 行。
func dbIssueKey(t *testing.T, st *store.Store, label, plaintext string) *store.APIKey {
	t.Helper()
	k, err := st.CreateAPIKey(t.Context(), label, digestOf(plaintext),
		plaintext[:12], plaintext[len(plaintext)-4:], plaintext)
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return k
}

// lastUsedOf 读取 Key 当前的 last_used_at（零值 = 从未使用）。
func lastUsedOf(t *testing.T, st *store.Store, keyID int64) time.Time {
	t.Helper()
	keys, err := st.ListAPIKeys(t.Context())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	for _, k := range keys {
		if k.ID == keyID {
			return k.LastUsedAt
		}
	}
	t.Fatalf("Key %d 不存在", keyID)
	return time.Time{}
}

// TestDBKeyAuthorized：库签发的 Key 命中放行，访问日志 key 为展示串
// （前缀…末4位），且日志任何级别不出现 Key 明文（§15.1）。
func TestDBKeyAuthorized(t *testing.T) {
	h, logBuf, st := newTestEnv(t, "")
	const plaintext = "sk_dbissued_0123456789abcdefXYZ"
	dbIssueKey(t, st, "alice-key", plaintext)

	w := do(h, "GET", "/v1/models", map[string]string{"Authorization": "Bearer " + plaintext}, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	access := findAccessRecord(t, logBuf.String(), "/v1/models")
	if access["key"] != "sk_dbissued_…fXYZ" {
		t.Errorf("access 记录 key = %v，期望展示串", access["key"])
	}
	if strings.Contains(logBuf.String(), plaintext) {
		t.Errorf("日志泄露库签发 Key 明文:\n%s", logBuf.String())
	}
}

// TestDBKeyDisabledImmediate401：禁用 Key 对数据面即时 401（决策 5 无缓存层），
// 启用即时恢复；错误形态与静态表时期一致。
func TestDBKeyDisabledImmediate401(t *testing.T) {
	h, _, st := newTestEnv(t, "")
	const plaintext = "sk_dbissued_disable_0123456789ab"
	k := dbIssueKey(t, st, "bob-key", plaintext)
	auth := map[string]string{"Authorization": "Bearer " + plaintext}

	if w := do(h, "GET", "/v1/models", auth, ""); w.Code != http.StatusOK {
		t.Fatalf("初始状态码 = %d，期望 200", w.Code)
	}
	// 禁用 Key → 即时 401 invalid_api_key。
	if err := st.SetAPIKeyDisabled(t.Context(), k.ID, true); err != nil {
		t.Fatalf("SetAPIKeyDisabled: %v", err)
	}
	w := do(h, "GET", "/v1/models", auth, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("禁用 Key 后状态码 = %d，期望 401", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "invalid_api_key" {
		t.Errorf("error.code = %q，期望 invalid_api_key", code)
	}
	// 启用回来 → 即时放行。
	if err := st.SetAPIKeyDisabled(t.Context(), k.ID, false); err != nil {
		t.Fatalf("SetAPIKeyDisabled: %v", err)
	}
	if w := do(h, "GET", "/v1/models", auth, ""); w.Code != http.StatusOK {
		t.Errorf("启用后状态码 = %d，期望 200（禁用启用均即时生效）", w.Code)
	}
	// 删除 → 摘要点查落空，即时 401。
	if err := st.DeleteAPIKey(t.Context(), k.ID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if w := do(h, "GET", "/v1/models", auth, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("删除后状态码 = %d，期望 401", w.Code)
	}
}

// TestImportConfigKeysIdempotent：YAML 导入幂等——重复导入不重复建行、
// 已禁用摘要不复活；日志只记条数不含 Key 物料。
func TestImportConfigKeysIdempotent(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	var logBuf bytes.Buffer
	logger := logging.New(&logBuf, slog.LevelDebug)

	yamlKeys := []config.APIKey{
		{Key: "sk_import_aaaa0000bbbb1111"},
		{Key: "sk_import_cccc2222dddd3333"},
	}
	imported, skipped, err := gateway.ImportConfigKeys(t.Context(), st, yamlKeys, logger)
	if err != nil {
		t.Fatalf("首次导入: %v", err)
	}
	if imported != 2 || skipped != 0 {
		t.Errorf("首次导入 imported/skipped = %d/%d，期望 2/0", imported, skipped)
	}
	keys, err := st.ListAPIKeys(t.Context())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("导入后 Key 行数 = %d，期望 2", len(keys))
	}
	for _, k := range keys {
		if k.Label != "imported-from-config" {
			t.Errorf("导入行 label = %q，期望 imported-from-config", k.Label)
		}
	}

	// 重复导入（模拟重复启动）：整条跳过，不重复建行。
	imported, skipped, err = gateway.ImportConfigKeys(t.Context(), st, yamlKeys, logger)
	if err != nil {
		t.Fatalf("重复导入: %v", err)
	}
	if imported != 0 || skipped != 2 {
		t.Errorf("重复导入 imported/skipped = %d/%d，期望 0/2", imported, skipped)
	}

	// 禁用第一条后再导入：已存在摘要整条跳过，禁用状态不被复活。
	if err := st.SetAPIKeyDisabled(t.Context(), keys[0].ID, true); err != nil {
		t.Fatalf("SetAPIKeyDisabled: %v", err)
	}
	if _, _, err := gateway.ImportConfigKeys(t.Context(), st, yamlKeys, logger); err != nil {
		t.Fatalf("禁用后导入: %v", err)
	}
	after, err := st.ListAPIKeys(t.Context())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("禁用后导入 Key 行数 = %d，期望仍为 2", len(after))
	}
	if !after[0].Disabled {
		t.Error("已禁用摘要被导入复活")
	}

	// 新增一条：照常建行，已有的两条不受影响。
	imported, _, err = gateway.ImportConfigKeys(t.Context(), st,
		[]config.APIKey{{Key: "sk_import_eeee4444ffff5555"}}, logger)
	if err != nil || imported != 1 {
		t.Fatalf("新条目导入 imported = %d, err = %v，期望 1/nil", imported, err)
	}
	if all, err := st.ListAPIKeys(t.Context()); err != nil || len(all) != 3 {
		t.Errorf("导入后 Key 行数 = %d (err=%v)，期望 3", len(all), err)
	}

	// 导入日志只记条数：任何 Key 明文不落日志（§15.1）。
	out := logBuf.String()
	for _, k := range append(yamlKeys, config.APIKey{Key: "sk_import_eeee4444ffff5555"}) {
		if strings.Contains(out, k.Key) {
			t.Errorf("导入日志泄露 Key 明文 %s:\n%s", logging.RedactKey(k.Key), out)
		}
	}
}

// TestNoAPIKeysConfig：无 api_keys 段（空导入表）也能装配与服务——
// /healthz 正常、任意 Key 401（config.Load 侧的放宽由 config 包测试覆盖）。
func TestNoAPIKeysConfig(t *testing.T) {
	h, _, _ := newTestEnvWithKeys(t, "", nil)
	if w := do(h, "GET", "/healthz", nil, ""); w.Code != http.StatusOK {
		t.Errorf("/healthz 状态码 = %d，期望 200", w.Code)
	}
	w := do(h, "GET", "/v1/models", map[string]string{"Authorization": "Bearer " + testKey}, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("空 Key 表下任意 Key 状态码 = %d，期望 401", w.Code)
	}
}

// TestLastUsedThrottle：last_used_at 节流——60s 窗口内重复请求只落库一次
// （>60s 再写的补测在包内 keyauth_internal_test.go 用注入时钟覆盖）。
func TestLastUsedThrottle(t *testing.T) {
	h, _, st := newTestEnv(t, "")
	const plaintext = "sk_dbissued_throttle_0123456789"
	k := dbIssueKey(t, st, "carol-key", plaintext)
	auth := map[string]string{"Authorization": "Bearer " + plaintext}

	if w := do(h, "GET", "/v1/models", auth, ""); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	t1 := lastUsedOf(t, st, k.ID)
	if t1.IsZero() {
		t.Fatal("首次请求应写 last_used_at")
	}
	// 拉开毫秒级时间差：若节流失效，第二次写会产生不同的时间戳。
	time.Sleep(5 * time.Millisecond)
	if w := do(h, "GET", "/v1/models", auth, ""); w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	if t2 := lastUsedOf(t, st, k.ID); !t2.Equal(t1) {
		t.Errorf("60s 内重复请求重复落库: %v → %v", t1, t2)
	}
}
