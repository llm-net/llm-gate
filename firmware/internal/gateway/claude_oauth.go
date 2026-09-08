package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Claude OAuth has a quota-only refresh owner. Component-level comparison
// protects reconnects and preserves independently updated inference credentials.
type claudeAccountRefresher struct {
	server *Server
	client *claudeauth.Client
	rowID  int64
}

func (c *claudeAccountRefresher) Now() time.Time { return c.client.Now() }
func (c *claudeAccountRefresher) RefreshHandle(ctx context.Context, cur agentauth.Handle) (agentauth.Handle, time.Duration, error) {
	previous, ok := cur.(*claudeauth.Credential)
	if !ok || !previous.IsOAuth() || previous.OAuthExpired() {
		return nil, 0, agentauth.ErrAuthExpired
	}
	next, ttl, err := c.client.RefreshHandle(ctx, cur)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentauth.CallbackTimeout)
	defer cancel()
	if err != nil {
		if agentauth.Deterministic(err) {
			if e := c.persist(writeCtx, previous, previous.ExpireOAuth(), false); e != nil {
				c.server.log.Error("标记 Claude OAuth 登录失效失败", "account_row", c.rowID)
			}
		}
		return nil, 0, err
	}
	e := c.persist(writeCtx, previous, next.(*claudeauth.Credential), true)
	if e != nil {
		// The upstream has already rotated. Keep the new generation in memory
		// rather than retrying the consumed refresh token after a storage error.
		c.server.log.Error("Claude OAuth 新一代凭据落库失败，重启后可能需要重新连接", "account_row", c.rowID)
	}
	return next, ttl, nil
}

func (c *claudeAccountRefresher) persist(ctx context.Context, previous, next *claudeauth.Credential, refreshed bool) error {
	_, err := c.server.store.MutateAgentAuth(ctx, c.rowID, store.AgentProviderClaude, refreshed, func(a *store.AgentAccount, blob string) (string, error) {
		current, err := claudeauth.Parse(blob)
		if err != nil {
			return "", err
		}
		if !current.SameOAuth(previous) {
			return "", nil
		}
		return current.WithOAuth(next).JSON()
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
