package gateway_test

import (
	"net/http"
	"testing"
)

func TestTextProtocolSurfacesRouteIndependently(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		chat, responses, anthropic bool
	}{
		{"chat", true, false, false},
		{"responses", false, true, false},
		{"anthropic", false, false, true},
		{"all", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRouteEnv(t)
			stub := newStub(t, chatJSONReply(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			up := dbUpstream(t, e.st, "surface-upstream", "mock", "sk-fake", stub.url)
			id := dbModel(t, e.st, "surface-model")
			dbSource(t, e.st, id, up, "upstream-model", 100)
			if err := e.st.SetModelEntries(t.Context(), id, tc.chat, tc.responses, tc.anthropic); err != nil {
				t.Fatal(err)
			}
			for _, request := range []struct {
				path, body string
				allowed    bool
			}{
				{"/v1/chat/completions", `{"model":"surface-model","messages":[{"role":"user","content":"ping"}]}`, tc.chat},
				{"/v1/responses", `{"model":"surface-model","store":false,"input":"ping"}`, tc.responses},
				{"/v1/messages", `{"model":"surface-model","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`, tc.anthropic},
			} {
				before := stub.count()
				w := do(e.h, http.MethodPost, request.path, chatAuth, request.body)
				want := http.StatusNotFound
				if request.allowed {
					want = http.StatusOK
				}
				if w.Code != want {
					t.Fatalf("%s: status=%d, want %d: %s", request.path, w.Code, want, w.Body.String())
				}
				if !request.allowed && stub.count() != before {
					t.Fatalf("disabled surface reached upstream: %s", request.path)
				}
			}
		})
	}
}
