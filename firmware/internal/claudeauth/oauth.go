package claudeauth

import (
	"errors"
	"slices"
	"strings"
)

const maxCredentialBytes = 128 << 10

// OfficialClientID is public application metadata from the official CLI.
const OfficialClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
const requiredScopes = "user:profile"

// oauthWire never escapes Credential except through its explicit sealing JSON.
type oauthWire struct {
	AccessToken      string   `json:"access_token"`
	RefreshToken     string   `json:"refresh_token"`
	ExpiresAt        int64    `json:"expires_at"` // Unix milliseconds, as in the official CLI.
	RefreshExpiresAt int64    `json:"refresh_expires_at,omitempty"`
	Scopes           []string `json:"scopes"`
	ClientID         string   `json:"client_id"`
}

func fromOAuth(o oauthWire) (*Credential, error) {
	for _, token := range []string{o.AccessToken, o.RefreshToken} {
		if strings.TrimSpace(token) != token {
			return nil, errors.New("Claude OAuth 令牌格式不正确")
		}
		if _, err := FromSetupToken(token); err != nil {
			return nil, errors.New("Claude OAuth 凭据必须包含有效的 accessToken 和 refreshToken")
		}
	}
	const maxMillis = int64(253402300799999)
	if o.ExpiresAt <= 0 || o.ExpiresAt > maxMillis || o.RefreshExpiresAt < 0 || o.RefreshExpiresAt > maxMillis {
		return nil, errors.New("Claude OAuth 凭据缺少有效的到期时间")
	}
	if o.ClientID == "" {
		o.ClientID = OfficialClientID
	}
	if o.ClientID != OfficialClientID {
		return nil, errors.New("Claude OAuth 客户端标识不正确，请重新授权")
	}
	if len(o.Scopes) > 32 {
		return nil, errors.New("Claude OAuth 权限范围格式不正确")
	}
	for _, scope := range o.Scopes {
		if len(scope) == 0 || len(scope) > 128 {
			return nil, errors.New("Claude OAuth 权限范围格式不正确")
		}
		for _, ch := range scope {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == ':' || ch == '_' || ch == '-' || ch == '.') {
				return nil, errors.New("Claude OAuth 权限范围格式不正确")
			}
		}
	}
	if !slices.Contains(o.Scopes, "user:profile") {
		return nil, errors.New("Claude OAuth 凭据缺少 user:profile 权限，请重新授权额度查询")
	}
	o.Scopes = slices.Clone(o.Scopes)
	return &Credential{oauth: &o}, nil
}
