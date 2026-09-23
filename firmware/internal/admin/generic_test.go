package admin_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGenericAccountProtocolsAndRepointing(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	// Selection and address are mandatory; unsupported protocols cannot be injected.
	for _, urls := range []string{`{}`, `{"openai_chat":""}`, `{"ark_image":"https://example.test/v1"}`, `{"systemone":"https://user:pass@example.test"}`, `{"openai_chat":"https://example.test/v1?key=fake"}`, `{"openai_responses":"https://example.test/v1?"}`, `{"systemone":"https://example.test/#"}`} {
		res := e.do("POST", "/admin/v1/upstreams", root, `{"name":"invalid","type":"generic","api_key":"fake-shared-key","protocol_urls":`+urls+`}`)
		wantStatus(t, res, 400)
		res.Body.Close()
	}
	urls := `{"openai_chat":"http://chat.test/v1","openai_responses":"http://responses.test/v1","anthropic_messages":"http://messages.test/v1","systemone":"http://judge.test"}`
	up := e.createUpstream(root, `{"name":"generic","type":"generic","api_key":"fake-shared-key","protocol_urls":`+urls+`}`)
	got, err := e.st.GetUpstreamByID(t.Context(), up.ID)
	if err != nil || got.ProtocolURLs == "" || got.BaseURL != "" {
		t.Fatalf("missing protocol settings: %v", err)
	}
	res := e.do("GET", fmt.Sprintf("/admin/v1/upstreams/%d/models", up.ID), root, "")
	wantStatus(t, res, 200)
	var models struct {
		Kinds []struct {
			Kind      string
			Protocols []string
		}
	}
	decodeInto(t, res, &models)
	if len(models.Kinds) != 2 || models.Kinds[0].Kind != "text" || models.Kinds[1].Kind != "systemone" {
		t.Fatalf("kinds: %+v", models)
	}
	m := e.createModel(root, "generic-text")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	if len(src.Protocols) != 3 {
		t.Fatalf("text protocols: %v", src.Protocols)
	}
	judge := e.createModelBody(root, `{"name":"judge","kind":"systemone"}`)
	src = e.createSource(root, judge.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	if len(src.Protocols) != 1 || src.Protocols[0] != "systemone" {
		t.Fatalf("judge protocols: %v", src.Protocols)
	}
	path := fmt.Sprintf("/admin/v1/upstreams/%d", up.ID)
	res = e.do("PATCH", path, root, `{"protocol_urls":{"openai_responses":"http://new.test/v1"}}`)
	wantStatus(t, res, 400)
	if errCode(t, res) != "base_url_requires_key" {
		t.Fatal("repointing accepted saved key")
	}
	unchanged, _ := e.st.GetUpstreamByID(t.Context(), up.ID)
	if unchanged.ProtocolURLs != got.ProtocolURLs {
		t.Fatal("rejected patch changed URLs")
	}
	res = e.do("PATCH", path, root, `{"protocol_urls":{"openai_responses":"http://new.test/v1"},"api_key":"fake-replacement-key"}`)
	wantStatus(t, res, 200)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(body), "fake-replacement-key") {
		t.Fatal("response leaked key")
	}
	route, err := e.st.ResolveModelRoute(t.Context(), m.Name)
	if err != nil || route.Candidates[0].Upstream.APIKey != "fake-replacement-key" || strings.Contains(route.Candidates[0].Upstream.ProtocolURLs, "chat.test") {
		t.Fatal("route did not update URLs and key atomically")
	}
}

func TestGenericDraftProtocolTests(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var p map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			t.Error("invalid probe body")
		}
		if string(p["model"]) != `"test-model"` {
			t.Error("test model not forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/messages" {
			if r.Header.Get("X-Api-Key") != "fake-shared-key" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") == "" {
				t.Error("Messages auth")
			}
		} else if r.Header.Get("Authorization") != "Bearer fake-shared-key" {
			t.Error("Bearer auth")
		}
		switch r.URL.Path {
		case "/chat/completions":
			if p["messages"] == nil {
				t.Error("Chat shape")
			}
			io.WriteString(w, `{"choices":[]}`)
		case "/responses":
			if p["input"] == nil || p["messages"] != nil {
				t.Error("Responses shape")
			}
			io.WriteString(w, `{"output":[]}`)
		case "/messages":
			io.WriteString(w, `{"content":[]}`)
		case "/v1/systemone":
			if p["state"] == nil || p["questions"] == nil {
				t.Error("System One shape")
			}
			io.WriteString(w, systemOneMixedProbeResponse)
		case "/bad/responses":
			io.WriteString(w, `<html>not a protocol response</html>`)
		case "/error/responses":
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"fake-shared-key"}}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	probe := func(protocol, url, key string, id int64) (int, string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"protocol": protocol, "base_url": url, "api_key": key, "model": "test-model", "upstream_id": id})
		res := e.do("POST", "/admin/v1/upstreams/test", root, string(b))
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "fake-shared-key") {
			t.Fatal("probe leaked credential")
		}
		return res.StatusCode, string(body)
	}
	for _, p := range []string{"openai_chat", "openai_responses", "anthropic_messages", "systemone"} {
		before := calls.Load()
		status, body := probe(p, server.URL, "fake-shared-key", 0)
		if status != 200 || !strings.Contains(body, `"ok":true`) || calls.Load() != before+1 {
			t.Fatalf("single protocol %s: %d %s", p, status, body)
		}
	}
	for _, path := range []string{"/bad", "/error"} {
		_, body := probe("openai_responses", server.URL+path, "fake-shared-key", 0)
		if !strings.Contains(body, `"ok":false`) {
			t.Fatal(body)
		}
	}
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"saved","type":"generic","api_key":"fake-shared-key","protocol_urls":{"openai_responses":%q}}`, server.URL))
	if status, body := probe("openai_responses", server.URL, "", up.ID); status != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatal(status, body)
	}
	before := calls.Load()
	if status, _ := probe("openai_responses", server.URL+"/different", "", up.ID); status != 400 || calls.Load() != before {
		t.Fatal("stored credential sent to new URL")
	}
}
