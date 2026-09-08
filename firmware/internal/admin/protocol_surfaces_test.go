package admin_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestProtocolSurfaceSwitchesAndAccess(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, `{"name":"surface-upstream","type":"openai_compat","api_key":"sk-fake-test","base_url":"https://example.invalid/v1"}`)
	m := e.createModelBody(root, `{"name":"surface-model","entry_openai":false,"entry_responses":true,"entry_anthropic":false}`)
	e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	path := fmt.Sprintf("/admin/v1/models/%d", m.ID)
	check := func(want string) {
		t.Helper()
		models := e.access(root).Models
		if len(models) != 1 || strings.Join(models[0].Protocols, ",") != want {
			t.Fatalf("protocol surfaces: %+v, want %s", models, want)
		}
	}
	check("openai_responses")
	wantStatus(t, e.do("PATCH", path, root, `{"entry_openai":true}`), http.StatusOK)
	check("openai_chat,openai_responses")
	wantStatus(t, e.do("PATCH", path, root, `{"entry_responses":false}`), http.StatusOK)
	check("openai_chat")
	wantStatus(t, e.do("PATCH", path, root, `{"name":"must-not-change","entry_openai":false}`), http.StatusBadRequest)
	check("openai_chat")
	if got := e.access(root).Models[0].Name; got != m.Name {
		t.Fatalf("rejected patch renamed model: %s", got)
	}
	video := e.createModelKind(root, "video-surface", "video")
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", video.ID), root,
		`{"entry_responses":false}`), http.StatusBadRequest)
}
