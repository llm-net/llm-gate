package claudeauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestManualLoginPKCEAndRedaction(t *testing.T) {
	login, err := NewLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(login.AuthorizeURL())
	q := u.Query()
	digest := sha256.Sum256([]byte(login.verifier))
	if u.Scheme != "https" || u.Host != "claude.com" || u.Path != "/cai/oauth/authorize" || q.Get("code") != "true" || q.Get("client_id") != OfficialClientID || q.Get("redirect_uri") != manualRedirectURL || q.Get("scope") != requiredScopes || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) || q.Get("state") != login.state {
		t.Fatal("wrong manual authorization parameters")
	}
	other, _ := NewLogin()
	if len(login.verifier) != 43 || len(login.state) != 43 || other.state == login.state || other.verifier == login.verifier {
		t.Fatal("PKCE randomness missing")
	}
	for _, value := range []any{login, *login} {
		var logs bytes.Buffer
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("login", "value", value)
		j, _ := json.Marshal(value)
		for _, formatted := range []string{fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value), logs.String(), string(j)} {
			if strings.Contains(formatted, login.verifier) || strings.Contains(formatted, login.state) {
				t.Fatal("login secret leaked")
			}
		}
	}
	var calls int
	client := NewClient(mockRouter(func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.URL.String() != tokenURL || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || body["code"] != "fake-login-code" || body["state"] != login.state || body["code_verifier"] != login.verifier || body["grant_type"] != "authorization_code" || body["client_id"] != OfficialClientID || body["redirect_uri"] != manualRedirectURL {
			t.Error("incorrect authorization grant")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"fake-login-access","refresh_token":"fake-login-refresh","expires_in":3600,"scope":"user:profile user:inference"}`))}, nil
	}))
	for _, invalid := range []string{"", "fake-login-code", "fake-login-code#", "fake-login-code#wrong-state", "fake\ncode#" + login.state, "fake-login-code#" + login.state + "#extra", strings.Repeat("x", 16<<10) + "#" + login.state} {
		if _, err := client.ExchangeCode(t.Context(), login, invalid); err == nil || strings.Contains(err.Error(), "fake-login-code") {
			t.Fatal("invalid code accepted or exposed")
		}
	}
	if calls != 0 {
		t.Fatal("invalid callback contacted issuer")
	}
	cred, err := client.ExchangeCode(t.Context(), login, "  fake-login-code#"+login.state+"\n")
	if err != nil || cred.AccessToken() != "fake-login-access" || !cred.IsOAuth() || calls != 1 {
		t.Fatalf("exchange failed: %v", err)
	}
	client.now = func() time.Time { return login.expires }
	if _, err := client.ExchangeCode(t.Context(), login, "fake-login-code#"+login.state); err == nil || calls != 1 {
		t.Fatal("expired flow exchanged")
	}
}

func TestManualLoginRequiresRefreshAndScopes(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"fake-login-access","expires_in":3600}`,
		`{"access_token":"fake-login-access","refresh_token":"fake-login-refresh","expires_in":3600,"scope":"user:inference"}`,
	} {
		client := NewClient(mockRouter(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		login, _ := NewLogin()
		if _, err := client.ExchangeCode(t.Context(), login, "fake-code#"+login.state); err == nil || strings.Contains(err.Error(), "fake-login") {
			t.Fatal("incomplete credentials accepted or exposed")
		}
	}
}
