package agentquota

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/egress"
)

type Client struct {
	http      *http.Client
	endpoints map[string]string
}

func NewClient(router egress.Router) *Client {
	base := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 8, MaxIdleConnsPerHost: 2}
	return &Client{http: &http.Client{Transport: egress.TransportFor(router, egress.ScopeAgentAuth, base), Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoints: map[string]string{
		"codex":  "https://chatgpt.com/backend-api/wham/usage",
		"claude": "https://api.anthropic.com/api/oauth/usage",
		"cursor": "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage",
		"grok":   "https://cli-chat-proxy.grok.com/v1/billing?format=credits",
	}}
}

// Fetch receives an already valid access token from the gateway's sole token
// owner. A quota 401/403 never disables an otherwise usable subscription.
func (c *Client) Fetch(ctx context.Context, provider, token, accountID string) (Data, error) {
	target, ok := c.endpoints[provider]
	if !ok {
		return Data{}, &Error{Code: "unsupported"}
	}
	method, body := "GET", ""
	if provider == "cursor" {
		method, body = "POST", "{}"
	}
	r, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		return Data{}, &Error{Code: "unavailable"}
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Accept", "application/json")
	switch provider {
	case "codex":
		if accountID != "" {
			r.Header.Set("ChatGPT-Account-ID", accountID)
		}
	case "claude":
		r.Header.Set("anthropic-beta", "oauth-2025-04-20")
	case "cursor":
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Connect-Protocol-Version", "1")
	case "grok":
		r.Header.Set("x-grok-client-mode", "grok-cli")
		r.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return Data{}, &Error{Code: "network_error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		code := "upstream_error"
		switch resp.StatusCode {
		case 401:
			code = "unauthorized"
		case 403:
			code = "permission_required"
		case 429:
			code = "rate_limited"
		case 404:
			code = "unsupported"
		}
		return Data{}, &Error{Code: code, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	const max = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || len(raw) > max {
		return Data{}, errShape
	}
	return Parse(provider, raw)
}
