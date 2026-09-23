package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/apidebug"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

type debugAccess struct {
	keyID  int64
	models []apidebug.Model
}

func (d *debugAccess) KeyAccessSnapshot(*http.Request, int64) (any, error) { return nil, nil }
func (d *debugAccess) APIDebugModels(_ context.Context, keyID int64) ([]apidebug.Model, error) {
	d.keyID = keyID
	if d.models != nil {
		return d.models, nil
	}
	return []apidebug.Model{{Name: "debug-model", Protocols: []string{"openai_chat"}, FileTypes: map[string][]string{"openai_chat": {"image/png"}}}}, nil
}

func TestAPIDebugSubscriptions(t *testing.T) {
	for _, provider := range []string{"codex", "grok"} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", provider, admin), func(t *testing.T) {
				var e *routeEnv
				var backend *stubUpstream
				var accountID int64
				model := codexModel
				if provider == "codex" {
					env := newAgentEnv(t, sseReply(codexStreamBody))
					e, backend, accountID = env.routeEnv, env.backend, env.acctID
				} else {
					env := newGrokEnv(t, jsonReply(200, grokNonStreamBody), issuerNever(t))
					e, backend, accountID, model = env.routeEnv, env.backend, env.acctID, grokModel
				}
				e.srv.SetAgentModels(&fakeAgentModels{owners: map[string]string{model: provider}})
				e.srv.SetKeyAccess(&debugAccess{models: []apidebug.Model{{Name: model, Provider: provider, Protocols: []string{"openai_responses"}}}})
				keyID := testKeyDevToolConfig(t, e.st).KeyID
				// API-model scope is independent of an explicitly assigned subscription.
				if _, _, err := e.st.ReplaceKeyAPIModelConfig(t.Context(), store.KeyAPIModelConfig{KeyID: keyID, Restricted: true}); err != nil {
					t.Fatal(err)
				}
				meter := &fakeMeter{}
				e.srv.EnableMetering(meter)
				body := fmt.Sprintf(`{"model":%q,"provider":%q,"protocol":"openai_responses","prompt":"private-subscription-debug"}`, model, provider)
				send := func() *httptest.ResponseRecorder {
					if !admin {
						return do(e.h, "POST", "/gate-helper/v1/api-debug", chatAuth, body)
					}
					r := httptest.NewRequest("POST", "/admin/v1/api-debug/1", strings.NewReader(body))
					r.Header.Set("Cookie", "fake-debug-session")
					r.Header.Set("X-LlmGate-CSRF", "1")
					w := httptest.NewRecorder()
					e.srv.DebugAPI(w, r, keyID)
					return w
				}
				w := send()
				if w.Code != 200 || backend.count() != 1 {
					t.Fatalf("subscription routing: %d %s calls=%d", w.Code, w.Body.String(), backend.count())
				}
				if provider == "codex" && !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatal("Codex stream lost")
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(backend.sentBody(0)), &payload); err != nil {
					t.Fatal(err)
				}
				if payload["model"] != model || payload["store"] != false || payload["stream"] != (provider == "codex") || len(payload["input"].([]any)) != 1 {
					t.Fatal("incorrect subscription request")
				}
				if sample := meter.only(t); sample.KeyID != keyID || sample.Entry != usage.EntryResponsesAgents {
					t.Fatalf("wrong billing identity or entry: %+v", sample)
				}
				if backend.sentHeaderValue(0, "Cookie") != "" || backend.sentHeaderValue(0, "X-LlmGate-CSRF") != "" || backend.sentAuth(0) == "Bearer "+testKey {
					t.Fatal("client credentials leaked upstream")
				}
				for _, secret := range []string{"private-subscription-debug", "fake-debug-session", testKey, codexAccess1, grokAccess1} {
					if strings.Contains(e.logBuf.String(), secret) {
						t.Fatal("subscription debug content or credentials logged")
					}
				}
				e.srv.EnableMetering(&fakeMeter{deny: &usage.Decision{Allowed: false}})
				if w := send(); w.Code != 429 || backend.count() != 1 {
					t.Fatalf("subscription admission bypass: %d calls=%d", w.Code, backend.count())
				}
				if err := e.st.SetAgentStatus(t.Context(), accountID, store.AgentStatusDisabled); err != nil {
					t.Fatal(err)
				}
				if w := send(); w.Code != 409 {
					t.Fatalf("disabled subscription accepted: %d", w.Code)
				}
				pinSubscription(t, e.st, provider, 0)
				if w := send(); w.Code != 403 || backend.count() != 1 {
					t.Fatalf("revoked subscription accepted: %d calls=%d", w.Code, backend.count())
				}
			})
		}
	}
}

func TestAPIDebugCodexCatalogCollision(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexStreamBody))
	e.srv.SetKeyAccess(&debugAccess{models: []apidebug.Model{{Name: codexModel, Provider: "codex", Protocols: []string{"openai_responses"}}}})
	upstream := newStub(t, jsonReply(200, codexNonStreamBody))
	up := dbUpstream(t, e.st, "catalog", config.UpstreamDeepseek, "sk-fake-catalog", upstream.url)
	model := dbModel(t, e.st, codexModel)
	dbSource(t, e.st, model, up, "deepseek-v4-flash", 100)
	selectDevToolModels(t, e.st, model)
	body := fmt.Sprintf(`{"model":%q,"provider":"codex","protocol":"openai_responses","prompt":"test"}`, codexModel)
	w := do(e.h, "POST", "/gate-helper/v1/api-debug", chatAuth, body)
	if w.Code != 404 || upstream.count() != 0 || e.backend.count() != 0 {
		t.Fatalf("subscription silently redirected: status=%d catalog=%d subscription=%d", w.Code, upstream.count(), e.backend.count())
	}
}

const debugBody = `{"model":"debug-model","protocol":"openai_chat","prompt":"private-debug-prompt","max_tokens":123,"files":[{"name":"private-image.png","type":"image/png","data":"cHJpdmF0ZS1pbWFnZS1ieXRlcw=="}]}`

func TestAPIDebugRoutingAuthAndMetering(t *testing.T) {
	for _, admin := range []bool{false, true} {
		name := "holder"
		if admin {
			name = "admin"
		}
		t.Run(name, func(t *testing.T) {
			e := newRouteEnv(t)
			stub := newStub(t, jsonReply(200, `{"choices":[{"message":{"content":"private-debug-response"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
			up := dbUpstream(t, e.st, "debug-up", config.UpstreamMock, "sk-fake-debug-upstream", stub.url)
			model := dbModel(t, e.st, "debug-model")
			dbSource(t, e.st, model, up, "upstream-model", 100)
			keys, _ := e.st.ListAPIKeys(t.Context())
			keyID := keys[0].ID
			access := &debugAccess{}
			e.srv.SetKeyAccess(access)
			meter := &fakeMeter{}
			e.srv.EnableMetering(meter)
			send := func(body string) *httptest.ResponseRecorder {
				if !admin {
					return do(e.h, "POST", "/gate-helper/v1/api-debug", chatAuth, body)
				}
				req := httptest.NewRequest("POST", "/admin/v1/api-debug/1", strings.NewReader(body))
				req.Header.Set("Cookie", "fake-session-sensitive")
				req.Header.Set("X-LlmGate-CSRF", "1")
				w := httptest.NewRecorder()
				e.srv.DebugAPI(w, req, keyID)
				return w
			}
			w := send(debugBody)
			if w.Code != 200 {
				t.Fatalf("status=%d %s", w.Code, w.Body.String())
			}
			if access.keyID != keyID || stub.count() != 1 {
				t.Fatal("wrong key or duplicate call")
			}
			sample := meter.only(t)
			if sample.KeyID != keyID || sample.Entry != usage.EntryChat {
				t.Fatalf("wrong billing identity: %+v", sample)
			}
			var payload map[string]any
			json.Unmarshal([]byte(stub.sentBody(0)), &payload)
			if payload["model"] != "upstream-model" || len(payload["messages"].([]any)) != 1 {
				t.Fatal("not routed single turn")
			}
			if stub.headers[0].Get("Cookie") != "" || stub.headers[0].Get("X-LlmGate-CSRF") != "" {
				t.Fatal("admin headers escaped")
			}
			for _, secret := range []string{"private-debug-prompt", "private-debug-response", "private-image.png", "cHJpdmF0ZS1pbWFnZS1ieXRlcw==", "sk-fake-debug-upstream", "fake-session-sensitive", testKey} {
				if strings.Contains(e.logBuf.String(), secret) {
					t.Fatalf("sensitive content logged: %q", secret)
				}
			}
			for _, body := range []string{strings.TrimSuffix(debugBody, "}") + `,"key_id":999}`, strings.TrimSuffix(debugBody, "}") + `,"messages":[]}`, debugBody + `{}`} {
				if got := send(body); got.Code != 400 {
					t.Fatalf("accepted identity/history/trailing JSON: %d", got.Code)
				}
			}
			if stub.count() != 1 {
				t.Fatal("invalid input reached upstream")
			}
			if _, _, err := e.st.ReplaceKeyAPIModelConfig(t.Context(), store.KeyAPIModelConfig{KeyID: keyID, Restricted: true}); err != nil {
				t.Fatal(err)
			}
			if got := send(debugBody); got.Code != 404 {
				t.Fatalf("scope bypass: %d %s", got.Code, got.Body.String())
			}
			if stub.count() != 1 {
				t.Fatal("restricted request reached upstream")
			}
			if err := e.st.SetAPIKeyDisabled(t.Context(), keyID, true); err != nil {
				t.Fatal(err)
			}
			if got := send(debugBody); got.Code != 401 {
				t.Fatalf("disabled key accepted: %d", got.Code)
			}
		})
	}
}

func TestAPIDebugAuthAndAdmission(t *testing.T) {
	e := newRouteEnv(t)
	e.srv.SetKeyAccess(&debugAccess{})
	for _, path := range []string{"/gate-helper/v1/api-debug", "/gate-helper/v1/api-debug/models"} {
		method := "POST"
		if strings.HasSuffix(path, "models") {
			method = "GET"
		}
		if w := do(e.h, method, path, nil, debugBody); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	e.srv.EnableMetering(&fakeMeter{deny: &usage.Decision{Allowed: false}})
	if w := do(e.h, "POST", "/gate-helper/v1/api-debug", chatAuth, debugBody); w.Code != 429 {
		t.Fatalf("admission bypass: %d %s", w.Code, w.Body.String())
	}
}

func TestAPIDebugCancellation(t *testing.T) {
	e := newRouteEnv(t)
	started, cancelled := make(chan struct{}), make(chan struct{})
	stub := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	up := dbUpstream(t, e.st, "cancel-up", config.UpstreamMock, "sk-fake-cancel", stub.url)
	model := dbModel(t, e.st, "debug-model")
	dbSource(t, e.st, model, up, "cancel-model", 100)
	e.srv.SetKeyAccess(&debugAccess{})
	meter := &fakeMeter{}
	e.srv.EnableMetering(meter)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest("POST", "/gate-helper/v1/api-debug", strings.NewReader(debugBody)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testKey)
	done := make(chan struct{})
	go func() { e.h.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request not cancelled")
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
	if sample := meter.only(t); sample.Status != 499 {
		t.Fatalf("cancelled status=%d", sample.Status)
	}
}

func TestAPIDebugBodyLimit(t *testing.T) {
	e := newRouteEnv(t)
	body := `{"model":"debug-model","prompt":"` + strings.Repeat("x", apidebug.BodyLimit) + `"}`
	if w := do(e.h, "POST", "/gate-helper/v1/api-debug", chatAuth, body); w.Code != 413 {
		t.Fatalf("body limit: %d", w.Code)
	}
}
