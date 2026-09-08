package claudeauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

const LoginTTL = 10 * time.Minute
const authorizeURL = "https://claude.com/cai/oauth/authorize"
const manualRedirectURL = "https://platform.claude.com/oauth/code/callback"

// Login holds one manual authorization-code flow. Only AuthorizeURL is sent
// to the administrator; the PKCE verifier stays in device memory until expiry.
type Login struct {
	verifier string
	state    string
	expires  time.Time
}

func (Login) String() string         { return "claudeauth.Login([REDACTED])" }
func (l Login) GoString() string     { return l.String() }
func (l Login) LogValue() slog.Value { return slog.StringValue(l.String()) }

func NewLogin() (*Login, error) {
	var random [64]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("生成 Claude 授权链接失败")
	}
	return &Login{verifier: base64.RawURLEncoding.EncodeToString(random[:32]), state: base64.RawURLEncoding.EncodeToString(random[32:]), expires: time.Now().Add(LoginTTL)}, nil
}

func (l *Login) AuthorizeURL() string {
	challenge := sha256.Sum256([]byte(l.verifier))
	q := url.Values{
		"code": {"true"}, "client_id": {OfficialClientID}, "response_type": {"code"},
		"redirect_uri": {manualRedirectURL}, "scope": {requiredScopes}, "state": {l.state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}
	return authorizeURL + "?" + q.Encode()
}

// ValidateCode requires the full code#state shown by Claude's manual callback.
// It does not consume the flow: a mistyped or unrelated code can be corrected.
func (l *Login) ValidateCode(pasted string, now time.Time) error {
	if l == nil || !now.Before(l.expires) {
		return errors.New("Claude 授权链接已过期，请重新生成")
	}
	pasted = strings.TrimSpace(pasted)
	code, state, found := strings.Cut(pasted, "#")
	if !found || len(code) == 0 || len(pasted) > 16<<10 {
		return errors.New("请完整粘贴 Claude 页面显示的授权码（包含 # 后的内容）")
	}
	for _, ch := range code {
		if ch < 33 || ch > 126 {
			return errors.New("Claude 授权码格式不正确，请重新复制")
		}
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(l.state)) != 1 {
		return errors.New("授权码与当前 Claude 授权链接不匹配，请使用本次链接重新授权")
	}
	return nil
}

// ExchangeCode uses the official CLI's manual redirect and JSON grant shape.
// The caller owns single-use consumption and guards against a newer login.
func (c *Client) ExchangeCode(ctx context.Context, login *Login, pasted string) (*Credential, error) {
	if err := login.ValidateCode(pasted, c.now()); err != nil {
		return nil, err
	}
	code, _, _ := strings.Cut(strings.TrimSpace(pasted), "#")
	body, _ := json.Marshal(map[string]string{
		"grant_type": "authorization_code", "code": code, "state": login.state,
		"client_id": OfficialClientID, "redirect_uri": manualRedirectURL, "code_verifier": login.verifier,
	})
	cred, _, err := c.exchange(ctx, body, oauthWire{ClientID: OfficialClientID}, "换取 Claude OAuth 凭据")
	return cred, err
}
