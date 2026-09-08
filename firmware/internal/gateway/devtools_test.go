package gateway_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

func TestGateConfigIsAuthenticatedPerKeyAndNoStore(t *testing.T) {
	e := newRouteEnv(t)
	w := do(e.h, http.MethodGet, "/gate-helper/v1/config", chatAuth, "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("config=%d cache=%q body=%s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	var got struct {
		SchemaVersion int   `json:"schema_version"`
		Revision      int64 `json:"revision"`
		Tools         map[string]struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"tools"`
		Subscriptions []struct {
			Provider   string `json:"provider"`
			Configured bool   `json:"configured"`
		} `json:"subscriptions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.SchemaVersion != 1 || got.Revision != 1 || len(got.Subscriptions) != 4 {
		t.Fatalf("config=%+v err=%v", got, err)
	}
	if _, ok := got.Tools["opencode"]; !ok {
		t.Fatalf("config missing OpenCode tool: %+v", got.Tools)
	}
	if w := do(e.h, http.MethodGet, "/gate-helper/v1/config", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key=%d", w.Code)
	}
}

// keyAccessStub 是注入给 SetKeyAccess 的执行体替身：记下数据面交来的 keyID，
// 回一份可辨认的读数。
type keyAccessStub struct {
	keyID int64
	calls int
}

func (k *keyAccessStub) KeyAccessSnapshot(_ *http.Request, keyID int64) (any, error) {
	k.keyID = keyID
	k.calls++
	return map[string]any{"endpoints": map[string]any{"addresses": []any{}, "http_port": 8080}}, nil
}

// GET /gate-helper/v1/endpoints 与 config 同一纪律：Key 鉴权、按鉴权结果的 keyID
// 取读数、no-store；无 Key 401；未接线（没注入 KeyAccess）答 503 而不是空读数。
func TestGateEndpointsIsAuthenticatedPerKeyAndNoStore(t *testing.T) {
	e := newRouteEnv(t)
	if w := do(e.h, http.MethodGet, "/gate-helper/v1/endpoints", chatAuth, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired=%d body=%s", w.Code, w.Body.String())
	}
	stub := &keyAccessStub{}
	e.srv.SetKeyAccess(stub)
	w := do(e.h, http.MethodGet, "/gate-helper/v1/endpoints", chatAuth, "")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("endpoints=%d cache=%q body=%s", w.Code, w.Header().Get("Cache-Control"), w.Body.String())
	}
	var got struct {
		Endpoints struct {
			HTTPPort int `json:"http_port"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Endpoints.HTTPPort != 8080 {
		t.Fatalf("endpoints=%+v err=%v", got, err)
	}
	if stub.calls != 1 || stub.keyID != testKeyID(t, e.st) {
		t.Fatalf("calls=%d keyID=%d want key %d", stub.calls, stub.keyID, testKeyID(t, e.st))
	}
	if w := do(e.h, http.MethodGet, "/gate-helper/v1/endpoints", nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing key=%d", w.Code)
	}
	if stub.calls != 1 {
		t.Fatalf("unauthenticated request reached KeyAccess: calls=%d", stub.calls)
	}
}

func TestClaudeMixedCatalogDispatchAndHello(t *testing.T) {
	e := newRouteEnv(t)
	backend := newStub(t, jsonReply(http.StatusOK, `{"model":"deepseek-v4-flash","usage":{"input_tokens":1,"output_tokens":1}}`))
	upstreamID := dbUpstream(t, e.st, "deepseek", config.UpstreamDeepseek, "sk-not-real", backend.url)
	modelID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, modelID, upstreamID, "deepseek-v4-flash", 10)
	selectDevToolModels(t, e.st, modelID)
	models := serveClaude(e, http.MethodGet, "/agents/claude/v1/models", claudeHeaders(), "", true)
	if models.Code != http.StatusOK {
		t.Fatalf("models=%d body=%s", models.Code, models.Body.String())
	}
	var discovered struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(models.Body.Bytes(), &discovered); err != nil || len(discovered.Data) != 1 ||
		discovered.Data[0].DisplayName != "LLM Gate · deepseek-v4-flash" ||
		len(discovered.Data[0].ID) < len("anthropic/llmgate/") || discovered.Data[0].ID[:len("anthropic/llmgate/")] != "anthropic/llmgate/" {
		t.Fatalf("catalog discovery=%+v err=%v", discovered, err)
	}
	bodyBytes, err := json.Marshal(map[string]any{
		"model": discovered.Data[0].ID, "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := string(bodyBytes)
	w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), body, true)
	if w.Code != http.StatusOK || backend.count() != 1 || backend.sentModel(0) != "deepseek-v4-flash" {
		t.Fatalf("catalog dispatch=%d hits=%d body=%s", w.Code, backend.count(), w.Body.String())
	}
	w = serveClaude(e, http.MethodHead, "/agents/claude/api/hello", claudeHeaders(), "", true)
	if w.Code != http.StatusNoContent {
		t.Fatalf("hello=%d body=%s", w.Code, w.Body.String())
	}
	connectClaudeCredential(t, e, claudeOAuthFake)
	w = serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(),
		`{"model":"not-selected","max_tokens":1,"messages":[]}`, true)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown model=%d body=%s", w.Code, w.Body.String())
	}
}
