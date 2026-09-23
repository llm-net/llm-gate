package admin_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/apidebug"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type debugBridge struct{ keyID int64 }

func (b *debugBridge) DebugAPI(w http.ResponseWriter, r *http.Request, id int64) {
	b.keyID = id
	w.WriteHeader(204)
}

func TestAPIDebugSubscriptionVisibility(t *testing.T) {
	e := newEnv(t)
	key, _ := e.createKey(e.rootSession(), "subscription debug")
	codex := seedAgentSubscription(t, e, store.AgentProviderCodex)
	grok := seedAgentSubscription(t, e, store.AgentProviderGrok)
	if _, _, err := e.st.ReplaceKeyAPIModelConfig(t.Context(), store.KeyAPIModelConfig{KeyID: key.ID, Restricted: true}); err != nil {
		t.Fatal(err)
	}
	read := func() []apidebug.Model {
		t.Helper()
		models, err := e.srv.APIDebugModels(t.Context(), key.ID)
		if err != nil {
			t.Fatal(err)
		}
		return models
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("unassigned subscription visible: %+v", got)
	}
	cfg := store.DevToolConfig{KeyID: key.ID, CodexAccountID: codex.ID, GrokAccountID: grok.ID}
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	models := read()
	want := 0
	for _, provider := range []string{"codex", "grok"} {
		agent, _ := platformcatalog.Builtin().Agent(provider)
		want += len(agent.Models)
		for _, model := range agent.Models {
			if !slices.ContainsFunc(models, func(m apidebug.Model) bool {
				return m.Name == model.Name && m.Provider == provider && slices.Equal(m.Protocols, []string{"openai_responses"}) && len(m.FileTypes["openai_responses"]) == 0
			}) {
				t.Fatalf("missing subscription model %s/%s", provider, model.Name)
			}
		}
	}
	if len(models) != want {
		t.Fatalf("unexpected model count=%d want=%d", len(models), want)
	}
	if err := e.st.SetAgentStatus(t.Context(), codex.ID, store.AgentStatusDisabled); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) == 0 || slices.ContainsFunc(got, func(m apidebug.Model) bool { return m.Provider != "grok" }) {
		t.Fatalf("disabled account remained visible: %+v", got)
	}
	cfg.GrokAccountID = 0
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("revoked subscription remained visible: %+v", got)
	}
}

func TestAPIDebugScopeAndCapabilities(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	key, _ := e.createKey(root, "debug")
	up := e.createUpstream(root, `{"name":"vision","type":"deepseek","api_key":"sk-fake-vision"}`)
	model := e.createModel(root, "deepseek-v4-flash-vision-exp")
	e.createSource(root, model.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	path := fmt.Sprintf("/admin/v1/api-debug/models?key_id=%d", key.ID)
	wantStatus(t, e.do("GET", path, "", ""), 401)
	read := func() []apidebug.Model {
		var body struct {
			Models []apidebug.Model `json:"models"`
		}
		response := e.do("GET", path, root, "")
		if response.StatusCode != 200 {
			t.Fatalf("models: %d %s", response.StatusCode, readAll(t, response))
		}
		decodeInto(t, response, &body)
		return body.Models
	}
	models := read()
	if len(models) != 1 || !slices.Contains(models[0].FileTypes["openai_chat"], "image/png") {
		t.Fatalf("missing catalog capability: %+v", models)
	}
	if len(models[0].FileTypes["openai_responses"]) != 0 {
		t.Fatal("advertises media on text-only conversion")
	}
	unknown := e.createUpstream(root, `{"name":"text-source","type":"deepseek","api_key":"sk-fake-text"}`)
	e.createSource(root, model.ID, fmt.Sprintf(`{"upstream_id":%d,"upstream_model_id":"deepseek-v4-pro"}`, unknown.ID))
	if got := read(); len(got[0].FileTypes["openai_chat"]) != 0 {
		t.Fatalf("did not intersect failover capabilities: %+v", got)
	}
	second := e.createModel(root, "hidden-text")
	e.createSource(root, second.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	wantStatus(t, e.do("PUT", fmt.Sprintf("/admin/v1/keys/%d/api-models", key.ID), root, fmt.Sprintf(`{"restricted":true,"model_ids":[%d],"dev_tool_model_ids":[]}`, model.ID)), 200)
	if got := read(); len(got) != 1 || got[0].Name != "deepseek-v4-flash-vision-exp" {
		t.Fatalf("scope leaked: %+v", got)
	}
	bridge := &debugBridge{}
	e.srv.SetAPIDebugGateway(bridge)
	submit := fmt.Sprintf("/admin/v1/api-debug/%d", key.ID)
	wantStatus(t, e.do("POST", submit, "", `{}`), 401)
	req := e.req("POST", submit, root, `{}`)
	req.Header.Del("X-LlmGate-CSRF")
	wantStatus(t, e.send(req), 403)
	wantStatus(t, e.do("POST", submit, root, `{}`), 204)
	if bridge.keyID != key.ID {
		t.Fatal("wrong selected key")
	}
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/keys/%d", key.ID), root, `{"disabled":true}`), 200)
	wantStatus(t, e.do("GET", path, root, ""), 409)
	wantStatus(t, e.do("POST", submit, root, `{}`), 409)
	wantStatus(t, e.do("POST", fmt.Sprintf("/admin/v1/keys/%d/archive", key.ID), root, `{}`), 200)
	wantStatus(t, e.do("GET", path, root, ""), 409)
	wantStatus(t, e.do("POST", submit, root, `{}`), 409)
}
