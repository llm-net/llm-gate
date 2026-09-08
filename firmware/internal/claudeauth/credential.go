// Package claudeauth owns device-sealed Claude Code credentials. Administrators
// use an official CLI setup-token for inference and independent browser OAuth
// credentials for quota reads. Refreshing quota authorization cannot change the
// inference token.
//
// The plaintext is a high-value credential. Callers must never log a
// Credential with general formatting or include its JSON in an API response.
package claudeauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxSetupTokenBytes = 16 << 10

// Credential is the canonical plaintext object sealed in
// agent_accounts.auth_json_sealed for provider=claude. The field is private so
// accidental json.Marshal or fmt formatting cannot disclose the token.
type Credential struct {
	setupToken       string
	oauth            *oauthWire
	quotaAuthExpired bool
}

type wireCredential struct {
	SetupToken       string     `json:"setup_token,omitempty"`
	OAuth            *oauthWire `json:"oauth,omitempty"`
	QuotaAuthExpired bool       `json:"quota_auth_expired,omitempty"`
}

// FromSetupToken validates one output value copied from `claude setup-token`.
// It intentionally does not pin a prefix: the prefix is not a documented
// compatibility contract and may change independently of firmware.
func FromSetupToken(raw string) (*Credential, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return nil, errors.New("Claude setup-token 不能为空")
	}
	if len(token) > maxSetupTokenBytes {
		return nil, fmt.Errorf("Claude setup-token 过长（最多 %d 字节）", maxSetupTokenBytes)
	}
	if !utf8.ValidString(token) {
		return nil, errors.New("Claude setup-token 不是有效文本")
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return nil, errors.New("Claude setup-token 中间不得包含空白或控制字符")
		}
	}
	return &Credential{setupToken: token}, nil
}

// Parse decodes the canonical plaintext object after the store has unsealed
// it. Unknown fields are rejected so a corrupt or manually edited row cannot
// silently acquire a second source of truth.
func Parse(blob string) (*Credential, error) {
	if len(blob) > maxCredentialBytes {
		return nil, errors.New("Claude 订阅凭据过长")
	}
	dec := json.NewDecoder(strings.NewReader(blob))
	dec.DisallowUnknownFields()
	var wire wireCredential
	if err := dec.Decode(&wire); err != nil {
		return nil, errors.New("Claude 订阅凭据格式不正确")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("Claude 订阅凭据含多余内容")
	}
	cred := &Credential{}
	if wire.OAuth != nil {
		var err error
		cred, err = fromOAuth(*wire.OAuth)
		if err != nil {
			return nil, err
		}
		cred.quotaAuthExpired = wire.QuotaAuthExpired
	}
	if wire.SetupToken != "" {
		setup, err := FromSetupToken(wire.SetupToken)
		if err != nil {
			return nil, err
		}
		cred.setupToken = setup.setupToken
	}
	if !cred.HasSetupToken() && !cred.IsOAuth() || wire.QuotaAuthExpired && !cred.IsOAuth() {
		return nil, errors.New("Claude 订阅凭据格式不正确")
	}
	return cred, nil
}

// JSON returns the canonical plaintext representation that the store seals.
func (c *Credential) JSON() (string, error) {
	if c == nil || (c.setupToken == "" && c.oauth == nil) {
		return "", errors.New("Claude setup-token 不能为空")
	}
	b, err := json.Marshal(wireCredential{SetupToken: c.setupToken, OAuth: c.oauth, QuotaAuthExpired: c.quotaAuthExpired})
	if err != nil {
		return "", errors.New("编码 Claude 订阅凭据失败")
	}
	return string(b), nil
}

// Token exposes the plaintext only to the narrow outbound request builder.
func (c *Credential) Token() string {
	if c == nil {
		return ""
	}
	return c.setupToken
}

func (c *Credential) IsOAuth() bool       { return c != nil && c.oauth != nil }
func (c *Credential) HasSetupToken() bool { return c != nil && c.setupToken != "" }
func (c *Credential) OAuthExpired() bool  { return c != nil && c.quotaAuthExpired }

// AccessToken implements agentauth.Handle only for the quota refresh owner.
func (c *Credential) AccessToken() string {
	if !c.IsOAuth() {
		return ""
	}
	return c.oauth.AccessToken
}
func (c *Credential) OAuthCredential() *Credential {
	if !c.IsOAuth() {
		return nil
	}
	return &Credential{oauth: c.oauth, quotaAuthExpired: c.quotaAuthExpired}
}
func (c *Credential) WithSetupToken(next *Credential) *Credential {
	out := *c
	out.setupToken = next.Token()
	return &out
}
func (c *Credential) WithOAuth(next *Credential) *Credential {
	out := *c
	out.oauth = next.oauth
	out.quotaAuthExpired = next.quotaAuthExpired
	return &out
}
func (c *Credential) ExpireOAuth() *Credential {
	out := *c
	out.quotaAuthExpired = out.IsOAuth()
	return &out
}

// SameOAuth compares just the quota credential, preserving concurrent setup-token changes.
func (c *Credential) SameOAuth(other *Credential) bool {
	if !c.IsOAuth() || !other.IsOAuth() {
		return false
	}
	a, _ := c.OAuthCredential().JSON()
	b, _ := other.OAuthCredential().JSON()
	return a == b
}
func (c *Credential) Expiry() time.Time {
	if !c.IsOAuth() {
		return time.Time{}
	}
	return time.UnixMilli(c.oauth.ExpiresAt).UTC()
}

// String, GoString and LogValue keep accidental diagnostics from exposing
// plaintext. GoString exists separately because %#v resolves through
// fmt.GoStringer rather than Stringer; without it %#v prints the unexported
// field in Go syntax. Value receivers are deliberate: they put the methods in
// the method sets of both Credential and *Credential, so a dereferenced or
// by-value embedded credential is redacted too — fmt cannot reach
// pointer-receiver methods on a plain value.
func (Credential) String() string { return "ClaudeCredential([REDACTED])" }

func (Credential) GoString() string { return "ClaudeCredential([REDACTED])" }

func (Credential) LogValue() slog.Value {
	return slog.GroupValue(slog.String("credential", "[REDACTED]"))
}
