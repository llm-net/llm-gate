package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type claudeLoginRouter func(*http.Request) (*http.Response, error)

func (f claudeLoginRouter) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (f claudeLoginRouter) Transport(scope egress.Scope, _ *http.Transport) http.RoundTripper {
	if scope != egress.ScopeAgentAuth {
		panic("wrong scope")
	}
	return f
}

func newClaudeFlowTest(t *testing.T, transport claudeLoginRouter) (*Server, *bytes.Buffer) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var logs bytes.Buffer
	s := &Server{st: st, log: logging.New(&logs, slog.LevelDebug)}
	s.claudeLogin.client = claudeauth.NewClient(transport)
	t.Cleanup(func() { s.claudeLogin.mu.Lock(); defer s.claudeLogin.mu.Unlock(); s.claudeLogin.clearLocked() })
	return s, &logs
}

func claudeFlowRequest(handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/admin/v1/agent-accounts/claude/login/callback", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func startClaudeFlow(t *testing.T, s *Server) string {
	t.Helper()
	w := claudeFlowRequest(s.handleClaudeLoginStart, struct{}{})
	var out agentLoginStartResponse
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ExpiresIn != 600 || out.Provider != "claude" {
		t.Fatal("start failed")
	}
	u, _ := url.Parse(out.AuthorizeURL)
	return "fake-manual-code#" + u.Query().Get("state")
}

func TestClaudeManualLoginSealsAndConsumesOnce(t *testing.T) {
	var calls atomic.Int32
	s, logs := newClaudeFlowTest(t, func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fake-manual-access","refresh_token":"fake-manual-refresh","expires_in":3600}`))}, nil
	})
	old, err := s.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: `{"setup_token":"fake-old-token"}`})
	if err != nil {
		t.Fatal(err)
	}
	code := startClaudeFlow(t, s)
	bad := claudeFlowRequest(s.handleClaudeLoginCallback, map[string]string{"code": "fake-manual-code#wrong"})
	if bad.Code != 400 || calls.Load() != 0 {
		t.Fatal("state validation failed")
	}
	w := claudeFlowRequest(s.handleClaudeLoginCallback, map[string]string{"code": code, "label": "Claude", "default_model": "claude-test"})
	if w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("callback failed: %d", w.Code)
	}
	current, raw, err := s.st.GetAgentCredential(t.Context(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := claudeauth.Parse(raw)
	if err != nil || !cred.IsOAuth() || cred.AccessToken() != "fake-manual-access" || cred.Token() != "fake-old-token" || current.ID != old.ID || current.Label != "Claude" || current.DefaultModel != "claude-test" {
		t.Fatal("OAuth connection not saved")
	}
	w2 := claudeFlowRequest(s.handleClaudeLoginCallback, map[string]string{"code": code})
	if w2.Code != 400 || calls.Load() != 1 {
		t.Fatal("authorization code replayed")
	}
	for _, text := range []string{w.Body.String(), w2.Body.String(), bad.Body.String(), logs.String()} {
		for _, secret := range []string{code, "fake-manual-code", "fake-manual-access", "fake-manual-refresh", "fake-old-token"} {
			if strings.Contains(text, secret) {
				t.Fatal("OAuth material leaked")
			}
		}
	}
}

func TestClaudeManualLoginLateExchangeCannotOverride(t *testing.T) {
	for _, action := range []string{"start", "setup", "delete"} {
		t.Run(action, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			s, _ := newClaudeFlowTest(t, func(*http.Request) (*http.Response, error) {
				close(entered)
				<-release
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fake-late-access","refresh_token":"fake-late-refresh","expires_in":3600}`))}, nil
			})
			old, err := s.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: `{"setup_token":"fake-old-token"}`})
			if err != nil {
				t.Fatal(err)
			}
			code := startClaudeFlow(t, s)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- claudeFlowRequest(s.handleClaudeLoginCallback, map[string]string{"code": code}) }()
			<-entered
			if w := claudeFlowRequest(s.handleClaudeLoginCallback, map[string]string{"code": code}); w.Code != 400 {
				t.Error("concurrent code replay accepted")
			}
			var newer string
			switch action {
			case "start":
				newer = startClaudeFlow(t, s)
			case "setup":
				w := claudeFlowRequest(s.handleConnectClaudeSetupToken, map[string]string{"setup_token": "fake-replacement"})
				if w.Code != 200 {
					t.Errorf("setup failed: %d", w.Code)
				}
			case "delete":
				r := httptest.NewRequest("DELETE", "/admin/v1/agent-accounts/1", nil)
				r.SetPathValue("id", fmt.Sprint(old.ID))
				w := httptest.NewRecorder()
				s.handleDeleteAgentAccount(w, r)
				if w.Code != 204 {
					t.Errorf("delete failed: %d", w.Code)
				}
			}
			close(release)
			wantStatus := 409
			if action == "setup" {
				wantStatus = 200
			}
			if w := <-done; w.Code != wantStatus {
				t.Fatalf("late callback status %d", w.Code)
			}
			_, blob, err := s.st.GetAgentCredential(t.Context(), "claude")
			if action == "delete" {
				if err == nil {
					t.Fatal("deleted connection resurrected")
				}
			} else if err != nil || action == "start" && strings.Contains(blob, "fake-late") || action == "setup" && (!strings.Contains(blob, "fake-replacement") || !strings.Contains(blob, "fake-late-refresh")) {
				t.Fatal("newer connection overwritten")
			}
			if action == "start" && s.claudeLogin.login.ValidateCode(newer, s.claudeLogin.client.Now()) != nil {
				t.Fatal("newer pending flow lost")
			}
		})
	}
}

func TestClaudeManualLoginIssuerFailureKeepsConnection(t *testing.T) {
	s, logs := newClaudeFlowTest(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"fake-manual-code"}`))}, nil
	})
	const original = `{"setup_token":"fake-old-token"}`
	_, err := s.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: original})
	if err != nil {
		t.Fatal(err)
	}
	w := claudeFlowRequest(s.handleClaudeLoginCallback, map[string]string{"code": startClaudeFlow(t, s)})
	_, blob, err := s.st.GetAgentCredential(t.Context(), "claude")
	if w.Code != 502 || err != nil || blob != original || strings.Contains(w.Body.String()+logs.String(), "fake-manual-code") {
		t.Fatal("failed exchange altered connection or exposed code")
	}
}

func TestClaudeCredentialUpdatesPreserveOtherComponentAndStatus(t *testing.T) {
	s, _ := newClaudeFlowTest(t, nil)
	const oauth = `{"oauth":{"access_token":"fake-quota","refresh_token":"fake-quota-refresh","expires_at":2000000000000,"scopes":["user:profile"]}}`
	a, err := s.saveClaudeCredential(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: oauth})
	if err != nil {
		t.Fatal(err)
	}
	out := s.agentAccountJSON(t.Context(), a)
	if out.SetupTokenConfigured || !out.QuotaOAuthConfigured {
		t.Fatal("OAuth-only metadata misrepresents inference")
	}
	// Setup updates retain both quota tokens, including their failure latch.
	_, err = s.st.MutateAgentAuth(t.Context(), a.ID, "claude", false, func(_ *store.AgentAccount, blob string) (string, error) {
		c, err := claudeauth.Parse(blob)
		if err != nil {
			return "", err
		}
		return c.ExpireOAuth().JSON()
	})
	if err != nil {
		t.Fatal(err)
	}
	w := claudeFlowRequest(s.handleConnectClaudeSetupToken, map[string]string{"setup_token": "fake-setup"})
	if w.Code != 200 {
		t.Fatalf("setup update failed: %d", w.Code)
	}
	current, blob, err := s.st.GetAgentCredential(t.Context(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := claudeauth.Parse(blob)
	if !c.OAuthExpired() || c.Token() != "fake-setup" || c.AccessToken() != "fake-quota" {
		t.Fatal("setup update modified quota authorization")
	}
	if err := s.st.SetAgentStatus(t.Context(), a.ID, store.AgentStatusAuthExpired); err != nil {
		t.Fatal(err)
	}
	// New quota authorization clears only the quota latch, not setup-token failure.
	current, err = s.saveClaudeCredential(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: oauth})
	if err != nil {
		t.Fatal(err)
	}
	out = s.agentAccountJSON(t.Context(), current)
	if current.Status != store.AgentStatusAuthExpired || !out.SetupTokenConfigured || !out.QuotaOAuthConfigured || out.QuotaOAuthExpired || out.CredentialKind != "setup_token+oauth" {
		t.Fatal("quota reconnect changed inference status")
	}
	w = claudeFlowRequest(s.handleConnectClaudeSetupToken, map[string]string{"setup_token": "fake-new-setup"})
	if w.Code != 200 {
		t.Fatal("replacement setup failed")
	}
	current, blob, err = s.st.GetAgentCredential(t.Context(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	c, _ = claudeauth.Parse(blob)
	if current.Status != "active" || c.Token() != "fake-new-setup" || c.AccessToken() != "fake-quota" {
		t.Fatal("setup reconnect did not repair inference independently")
	}
}
