package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// New OAuth logins and deletion supersede in-flight authorizations; setup-token
// updates are independent and use this lock only for local creation/merging.
// The lock covers local commits only; token exchange never blocks a new login.
type claudeLoginState struct {
	mu         sync.Mutex
	login      *claudeauth.Login
	generation uint64
	client     *claudeauth.Client
	timer      *time.Timer
}

func (a *claudeLoginState) clearLocked() {
	a.generation++
	a.login = nil
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

func (s *Server) handleClaudeLoginStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	login, err := claudeauth.NewLogin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "claude_login_failed", err.Error())
		return
	}
	a := &s.claudeLogin
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clearLocked()
	a.login = login
	a.timer = time.AfterFunc(claudeauth.LoginTTL, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.login == login {
			a.clearLocked()
		}
	})
	writeJSON(w, http.StatusOK, agentLoginStartResponse{Provider: store.AgentProviderClaude, ExpiresIn: int(claudeauth.LoginTTL.Seconds()), AuthorizeURL: login.AuthorizeURL()})
}

func (s *Server) handleClaudeLoginCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Code         string `json:"code"`
		Label        string `json:"label"`
		DefaultModel string `json:"default_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	label, model, ok := s.readAgentProfile(w, req.Label, req.DefaultModel)
	if !ok {
		return
	}
	a := &s.claudeLogin
	a.mu.Lock()
	login, generation := a.login, a.generation
	if err := login.ValidateCode(req.Code, time.Now()); err != nil {
		a.mu.Unlock()
		writeError(w, http.StatusBadRequest, "invalid_claude_code", err.Error())
		return
	}
	// Consume before the network call: the same code can never be exchanged twice.
	a.login = nil
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	if a.client == nil {
		a.client = claudeauth.NewClient(s.agents.egress)
	}
	client := a.client
	a.mu.Unlock()
	cred, err := client.ExchangeCode(r.Context(), login, req.Code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "claude_exchange_failed", "Claude 授权码换取凭据失败，请重新生成链接并授权："+err.Error())
		return
	}
	blob, err := cred.JSON()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.generation != generation {
		writeError(w, http.StatusConflict, "claude_login_superseded", "Claude 连接已更新，本次授权已作废，请使用最新的连接")
		return
	}
	a.clearLocked()
	s.saveAgentAccount(w, r, store.AgentProviderClaude, "", blob, label, model, "login")
}

// Only the non-secret credential kind reaches the UI, never tokens or scopes.
func (s *Server) agentAccountJSON(ctx context.Context, a *store.AgentAccount) agentAccountJSON {
	out := toAgentAccountJSON(a)
	if a.Provider != store.AgentProviderClaude {
		return out
	}
	current, blob, err := s.st.GetAgentCredential(ctx, a.Provider)
	if err != nil || current.ID != a.ID || !current.UpdatedAt.Equal(a.UpdatedAt) {
		return out
	}
	cred, err := claudeauth.Parse(blob)
	if err != nil {
		return out
	}
	out.SetupTokenConfigured = cred.HasSetupToken()
	out.QuotaOAuthConfigured = cred.IsOAuth()
	out.QuotaOAuthExpired = cred.OAuthExpired()
	out.CredentialKind = "setup_token"
	if cred.IsOAuth() {
		out.CredentialKind = "oauth"
	}
	if cred.IsOAuth() && cred.HasSetupToken() {
		out.CredentialKind = "setup_token+oauth"
	}
	return out
}

// Caller holds claudeLogin.mu. Each submission replaces only its own component.
func (s *Server) saveClaudeCredential(ctx context.Context, in store.NewAgentAccount) (*store.AgentAccount, error) {
	next, err := claudeauth.Parse(in.AuthJSON)
	if err != nil {
		return nil, err
	}
	current, _, err := s.st.GetAgentCredential(ctx, store.AgentProviderClaude)
	if errors.Is(err, store.ErrNotFound) {
		return s.st.UpsertAgentAccount(ctx, in)
	}
	if err != nil {
		return nil, err
	}
	return s.st.MutateAgentAuth(ctx, current.ID, store.AgentProviderClaude, false, func(a *store.AgentAccount, blob string) (string, error) {
		cred, err := claudeauth.Parse(blob)
		if err != nil {
			return "", err
		}
		if next.HasSetupToken() {
			cred = cred.WithSetupToken(next)
			if a.Status != store.AgentStatusDisabled {
				a.Status = store.AgentStatusActive
			}
		} else {
			cred = cred.WithOAuth(next)
		}
		if in.Label != "" {
			a.Label = in.Label
		}
		if in.DefaultModel != "" {
			a.DefaultModel = in.DefaultModel
		}
		return cred.JSON()
	})
}
