package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

func genericAccount(t *testing.T, e *routeEnv, name string, urls map[string]string) int64 {
	t.Helper()
	b, _ := json.Marshal(urls)
	up, err := e.st.CreateCatalogUpstream(t.Context(), name, config.UpstreamGeneric, config.UpstreamGeneric, "none", "fake-shared-key", "", string(b))
	if err != nil {
		t.Fatal(err)
	}
	return up.ID
}

func TestGenericIndependentProtocols(t *testing.T) {
	e := newRouteEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/messages/") {
			if r.Header.Get("X-Api-Key") != "fake-shared-key" || r.Header.Get("Authorization") != "" {
				t.Error("wrong Messages auth")
			}
		} else if r.Header.Get("Authorization") != "Bearer fake-shared-key" {
			t.Error("wrong Bearer auth")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/chat/v1/chat/completions":
			io.WriteString(w, `{"model":"wire","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
		case "/responses/v1/responses":
			if body["messages"] != nil || body["input"] == nil || body["previous_response_id"] != "resp_previous" || body["store"] != true {
				t.Error("native Responses was converted")
			}
			io.WriteString(w, `{"id":"resp_native","object":"response","model":"wire","output":[],"usage":{"input_tokens":12,"output_tokens":3}}`)
		case "/messages/v1/messages":
			io.WriteString(w, `{"type":"message","model":"wire","content":[],"usage":{"input_tokens":2,"output_tokens":1}}`)
		case "/judge/v1/systemone":
			io.WriteString(w, systemOneResponse)
		default:
			t.Errorf("wrong independent URL: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	urls := map[string]string{config.ProtocolOpenAIChat: server.URL + "/chat/v1", config.ProtocolOpenAIResponses: server.URL + "/responses/v1", config.ProtocolAnthropicMessages: server.URL + "/messages/v1", config.ProtocolSystemOne: server.URL + "/judge"}
	up := genericAccount(t, e, "generic", urls)
	text := dbKindModel(t, e.st, "shared-text", store.ModelKindText)
	judge := dbKindModel(t, e.st, "judge", store.ModelKindSystemOne)
	dbSource(t, e.st, text, up, "wire", 100)
	dbSource(t, e.st, judge, up, "jev-latest", 100)
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"shared-text","messages":[{"role":"user","content":"ping"}]}`},
		{"/v1/messages", `{"model":"shared-text","messages":[{"role":"user","content":"ping"}],"max_tokens":1}`},
		{systemOnePath, systemOneRequest},
	} {
		w := do(e.h, "POST", tc.path, chatAuth, tc.body)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	meter := &fakeMeter{}
	e.srv.EnableMetering(meter)
	w := do(e.h, "POST", "/v1/responses", chatAuth, `{"model":"shared-text","input":"ping","previous_response_id":"resp_previous","store":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"model":"shared-text"`) {
		t.Fatalf("native response: %d %s", w.Code, w.Body)
	}
	sample := meter.only(t)
	if sample.Entry != usage.EntryResponses || sample.Tokens.Prompt != 12 || sample.Tokens.Completion != 3 || sample.Estimated {
		t.Fatalf("native usage: %+v", sample)
	}
	// Deselecting a protocol immediately removes that route, while native Responses stays discoverable.
	onlyResponses, _ := json.Marshal(map[string]string{config.ProtocolOpenAIResponses: urls[config.ProtocolOpenAIResponses]})
	if err := e.st.UpdateGenericUpstream(t.Context(), up, "generic", string(onlyResponses), "fake-shared-key"); err != nil {
		t.Fatal(err)
	}
	w = do(e.h, "POST", "/v1/chat/completions", chatAuth, `{"model":"shared-text","messages":[]}`)
	if w.Code != 404 {
		t.Fatalf("unselected protocol served: %d", w.Code)
	}
	w = do(e.h, "GET", "/v1/models", chatAuth, "")
	if !strings.Contains(w.Body.String(), "shared-text") {
		t.Fatal("native Responses model absent")
	}
	selectDevToolModels(t, e.st, text)
	w = do(e.h, "GET", "/agents/codex/v1/models", chatAuth, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "shared-text") {
		t.Fatalf("native Codex model absent: %d %s", w.Code, w.Body)
	}
}

func TestGenericNativeResponsesStreamingAndFailover(t *testing.T) {
	for _, nativeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "native_to_chat", false: "chat_to_native"}[nativeFirst], func(t *testing.T) {
			e := newRouteEnv(t)
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				if len(calls) == 1 {
					w.WriteHeader(503)
					return
				}
				if r.URL.Path == "/responses" {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"wire\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":2}}}\n\n")
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"id\":\"chatcmpl_test\",\"model\":\"wire\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			native := genericAccount(t, e, "native", map[string]string{config.ProtocolOpenAIResponses: server.URL})
			legacy := dbUpstream(t, e.st, "legacy", config.UpstreamOpenAICompat, "fake-legacy-key", server.URL)
			model := dbKindModel(t, e.st, "stream-model", store.ModelKindText)
			first, second := native, legacy
			if !nativeFirst {
				first, second = legacy, native
			}
			dbSource(t, e.st, model, first, "wire", 100)
			dbSource(t, e.st, model, second, "wire", 200)
			meter := &fakeMeter{}
			e.srv.EnableMetering(meter)
			w := do(e.h, "POST", "/v1/responses", chatAuth, `{"model":"stream-model","input":"ping","stream":true}`)
			if w.Code != 200 || len(calls) != 2 || !strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), `"model":"stream-model"`) {
				t.Fatalf("failover: %d %v %s", w.Code, calls, w.Body)
			}
			sample := meter.only(t)
			if sample.Tokens.Prompt != 7 || sample.Tokens.Completion != 2 {
				t.Fatalf("stream usage: %+v", sample)
			}
		})
	}
}
