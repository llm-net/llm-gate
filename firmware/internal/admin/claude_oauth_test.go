package admin_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestClaudeManualLoginRequiresCSRF(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	for _, path := range []string{"/admin/v1/agent-accounts/claude/login/start", "/admin/v1/agent-accounts/claude/login/callback"} {
		r := e.req("POST", path, cookie, `{}`)
		r.Header.Del("X-LlmGate-CSRF")
		wantStatus(t, e.send(r), http.StatusForbidden)
	}
}

// OAuth logins already sealed by the browser flow remain usable for self-check.
func TestClaudeOAuthSelfCheckAndNoFileImport(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	const fixture = `{"oauth":{"access_token":"fake-claude-access","refresh_token":"fake-claude-refresh","expires_at":2000000000000,"scopes":["user:profile","user:inference"]}}`
	a, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: store.AgentProviderClaude, AuthJSON: fixture})
	if err != nil {
		t.Fatal(err)
	}
	tokens := &fakeAgentTokens{}
	e.srv.SetAgentTokens(tokens)
	if err := e.st.SetAgentStatus(t.Context(), a.ID, store.AgentStatusAuthExpired); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.do("POST", fmt.Sprintf("/admin/v1/agent-accounts/%d/refresh", a.ID), e.cookie, `{}`), http.StatusOK)
	if tokens.count() != 1 {
		t.Fatal("OAuth self-check did not use sole token owner")
	}
	current, err := e.st.GetAgentAccount(t.Context(), a.ID)
	if err != nil || current.Status != store.AgentStatusAuthExpired {
		t.Fatal("quota self-check revived inference credential")
	}
	wantStatus(t, e.do("POST", "/admin/v1/agent-accounts/claude/oauth", e.cookie, `{"auth_json":"fake-file-credential"}`), http.StatusNotFound)
	_, still, err := e.st.GetAgentCredential(t.Context(), "claude")
	if err != nil || still != fixture {
		t.Fatal("removed file import changed the usable login")
	}
}
