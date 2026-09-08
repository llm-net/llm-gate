package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
)

const tokenURL = "https://platform.claude.com/v1/oauth/token"

type Client struct {
	http *http.Client
	now  func() time.Time
}

func NewClient(router egress.Router) *Client {
	base := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConnsPerHost: 2}
	return &Client{http: &http.Client{Transport: egress.TransportFor(router, egress.ScopeAgentAuth, base), Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: time.Now}
}

func (c *Client) Now() time.Time { return c.now() }

// RefreshHandle only requests the two scopes needed by the gateway. Credentials
// never appear in errors, including network, malformed JSON and OAuth failures.
func (c *Client) RefreshHandle(ctx context.Context, handle agentauth.Handle) (agentauth.Handle, time.Duration, error) {
	cur, ok := handle.(*Credential)
	if !ok || !cur.IsOAuth() {
		return nil, 0, errors.New("Claude setup-token 不支持自动续期")
	}
	if cur.OAuthExpired() {
		return nil, 0, agentauth.ErrAuthExpired
	}
	if cur.oauth.RefreshExpiresAt > 0 && !time.UnixMilli(cur.oauth.RefreshExpiresAt).After(c.now()) {
		return nil, 0, &agentauth.OAuthError{Origin: "Anthropic", Phase: "刷新 Claude OAuth 凭据", Status: 400, Code: "invalid_grant"}
	}
	body, _ := json.Marshal(struct {
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
		Scope        string `json:"scope"`
	}{"refresh_token", cur.oauth.RefreshToken, cur.oauth.ClientID, requiredScopes})
	return c.exchange(ctx, body, *cur.oauth, "刷新 Claude OAuth 凭据")
}

// exchange shares bounded parsing and secret-free errors for both grants.
func (c *Client) exchange(ctx context.Context, body []byte, previous oauthWire, phase string) (*Credential, time.Duration, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, 0, errors.New("构造 Claude OAuth 请求失败")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, 0, errors.New("连接 Claude OAuth 签发服务失败")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCredentialBytes+1))
	if err != nil || len(raw) > maxCredentialBytes {
		return nil, 0, errors.New("读取 Claude OAuth 应答失败")
	}
	if resp.StatusCode != http.StatusOK {
		code := agentauth.ErrorCode(raw)
		switch code {
		case "invalid_grant", "invalid_client", "invalid_scope", "unauthorized_client", "access_denied", "token_expired":
		default:
			code = ""
		}
		return nil, 0, &agentauth.OAuthError{Origin: "Anthropic", Phase: phase, Status: resp.StatusCode, Code: code}
	}
	var out struct {
		Access         string  `json:"access_token"`
		Refresh        string  `json:"refresh_token"`
		Expires        int64   `json:"expires_in"`
		RefreshExpires *int64  `json:"refresh_token_expires_in"`
		Scope          *string `json:"scope"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Expires <= 0 || out.Expires > 366*86400 {
		return nil, 0, errors.New("Claude OAuth 应答格式不正确")
	}
	next := previous
	next.AccessToken = out.Access
	if out.Refresh != "" {
		next.RefreshToken = out.Refresh
	}
	duration := time.Duration(out.Expires) * time.Second
	next.ExpiresAt = c.now().Add(duration).UnixMilli()
	if out.RefreshExpires != nil {
		if *out.RefreshExpires <= 0 || *out.RefreshExpires > 10*366*86400 {
			return nil, 0, errors.New("Claude OAuth 应答格式不正确")
		}
		next.RefreshExpiresAt = c.now().Add(time.Duration(*out.RefreshExpires) * time.Second).UnixMilli()
	}
	next.Scopes = strings.Fields(requiredScopes)
	if out.Scope != nil {
		next.Scopes = strings.Fields(*out.Scope)
	}
	cred, err := fromOAuth(next)
	if err != nil {
		return nil, 0, errors.New("Claude OAuth 应答缺少有效凭据或必要权限")
	}
	return cred, duration, nil
}
