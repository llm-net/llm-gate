package gateway

import (
	"context"
	"errors"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/codexauth"
	"github.com/llm-net/llm-gate/firmware/internal/cursorauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func (s *Server) SetAgentQuota(m *agentquota.Manager) { s.agentQuota = m }

// FetchAgentQuota shares the existing single OAuth refresh owner and Cursor
// exchange cache. A permission error on the quota API never expires the account.
func (s *Server) FetchAgentQuota(ctx context.Context, a *store.AgentAccount) (agentquota.Data, error) {
	acct, blob, err := s.store.GetAgentCredential(ctx, a.Provider)
	if err != nil {
		return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
	}
	if acct.ID != a.ID || !agentquota.SyncEnabled(acct) || !acct.UpdatedAt.Equal(a.UpdatedAt) {
		return agentquota.Data{}, &agentquota.Error{Code: "account_changed"}
	}
	var token, accountID string
	var claudeTokens *agentauth.Provider
	var cursorTokens *cursorSession
	switch a.Provider {
	case store.AgentProviderClaude:
		cred, e := claudeauth.Parse(blob)
		if e != nil {
			return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
		}
		if cred.OAuthExpired() {
			return agentquota.Data{}, &agentquota.Error{Code: "authorization_expired"}
		}
		if !cred.IsOAuth() {
			return agentquota.Data{}, &agentquota.Error{Code: "permission_required"}
		}
		{
			sess, e := s.agentSessionFor(acct, blob)
			if e != nil {
				return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
			}
			token, err = sess.tokens.Current(ctx)
			claudeTokens = sess.tokens
		}
	case store.AgentProviderCursor:
		cred, e := cursorauth.Parse(blob)
		if e != nil {
			return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
		}
		cursorTokens = s.cursorSessionFor(acct, cred.Token())
		token, err = s.cursorToken(ctx, cursorTokens)
	case store.AgentProviderCodex, store.AgentProviderGrok:
		sess, e := s.agentSessionFor(acct, blob)
		if e != nil {
			return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
		}
		token, err = sess.tokens.Current(ctx)
		if a.Provider == store.AgentProviderCodex {
			accountID = codexauth.ProviderAccountID(sess.tokens)
			if accountID == "" {
				accountID = acct.AccountID
			}
		}
	default:
		return agentquota.Data{}, &agentquota.Error{Code: "unsupported"}
	}
	if err != nil {
		return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
	}
	data, err := s.quotaClient.Fetch(ctx, a.Provider, token, accountID)
	var qe *agentquota.Error
	if claudeTokens != nil && errors.As(err, &qe) && qe.Code == "unauthorized" {
		claudeTokens.Invalidate(token)
		token, refreshErr := claudeTokens.Current(ctx)
		if refreshErr != nil {
			return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
		}
		return s.quotaClient.Fetch(ctx, a.Provider, token, accountID)
	}
	if cursorTokens != nil && errors.As(err, &qe) && qe.Code == "unauthorized" {
		// Match inference's single retry, sharing its exchange cache. Invalidating
		// only the rejected token preserves a concurrent refresh; quota failure
		// must never latch the subscription's auth_expired state.
		cursorTokens.invalidate(token)
		token, refreshErr := s.cursorToken(ctx, cursorTokens)
		if refreshErr != nil {
			return agentquota.Data{}, &agentquota.Error{Code: "credential_unavailable"}
		}
		return s.quotaClient.Fetch(ctx, a.Provider, token, accountID)
	}
	return data, err
}
