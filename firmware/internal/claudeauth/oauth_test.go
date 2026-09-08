package claudeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
)

const oauthFixture = `{"oauth":{"access_token":"fake-oauth-access","refresh_token":"fake-oauth-refresh","expires_at":2000000000000,"scopes":["user:profile","user:inference"]}}`

type mockRouter func(*http.Request) (*http.Response, error)

func (f mockRouter) Transport(scope egress.Scope, _ *http.Transport) http.RoundTripper {
	if scope != egress.ScopeAgentAuth {
		panic("wrong egress scope")
	}
	return mockTransport(f)
}

type mockTransport func(*http.Request) (*http.Response, error)

func (f mockTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthCredentialValidationAndSelfRedaction(t *testing.T) {
	c, err := Parse(oauthFixture)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(blob)
	if err != nil || !got.IsOAuth() || got.AccessToken() != "fake-oauth-access" || got.Expiry().UnixMilli() != 2000000000000 {
		t.Fatal("canonical OAuth round trip failed")
	}
	for _, v := range []any{c, *c} {
		var logs bytes.Buffer
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("credential", "value", v)
		raw, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		for _, s := range []string{fmt.Sprintf("%v", v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v), logs.String(), string(raw)} {
			for _, secret := range []string{"fake-oauth-access", "fake-oauth-refresh"} {
				if strings.Contains(s, secret) {
					t.Fatal("credential exposed by generic formatting")
				}
			}
		}
	}
	for _, bad := range []string{
		strings.Replace(oauthFixture, "user:profile", "user:other", 1),
		strings.Replace(oauthFixture, "2000000000000", "0", 1),
		strings.Replace(oauthFixture, "fake-oauth-refresh", "", 1),
		strings.Replace(oauthFixture, "fake-oauth-access", "fake token", 1),
		oauthFixture + `{}`,
		`{"claudeAiOauth":{"accessToken":"fake-oauth-access"}}`,
		strings.Replace(oauthFixture, `"oauth":`, `"unknown":"fake-extra","oauth":`, 1),
	} {
		if _, err := Parse(bad); err == nil {
			t.Fatal("invalid OAuth credential accepted")
		}
	}
	if got.Token() != "" {
		t.Fatal("OAuth credential exposed an inference token")
	}
	combined, err := Parse(strings.Replace(blob, `{"oauth":`, `{"setup_token":"fake-setup","oauth":`, 1))
	if err != nil || combined.Token() != "fake-setup" || combined.AccessToken() != "fake-oauth-access" {
		t.Fatal("independent credentials not preserved")
	}
	for _, v := range []any{combined, *combined, combined.ExpireOAuth(), *combined.ExpireOAuth()} {
		var logs bytes.Buffer
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("credential", "value", v)
		raw, _ := json.Marshal(v)
		rendered := fmt.Sprintf("%v %+v %#v", v, v, v) + logs.String() + string(raw)
		if strings.Contains(rendered, "fake-") {
			t.Fatal("combined credential leaked")
		}
	}
	profileOnly := strings.Replace(oauthFixture, `,"user:inference"`, "", 1)
	if _, err := Parse(profileOnly); err != nil {
		t.Fatal("quota-only scope rejected")
	}

}

func TestOAuthRefreshRotationAndSafeErrors(t *testing.T) {
	cur, _ := Parse(oauthFixture)
	now := time.Unix(1900000000, 0)
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		failure       bool
		deterministic bool
	}{
		{"rotated", 200, `{"access_token":"fake-new-access","refresh_token":"fake-new-refresh","expires_in":3600,"scope":"user:profile user:inference"}`, false, false},
		{"retained_refresh", 200, `{"access_token":"fake-new-access","expires_in":3600}`, false, false},
		{"expired", 400, `{"error":"invalid_grant","error_description":"fake-oauth-refresh"}`, true, true},
		{"throttled", 429, `{"error":"invalid_grant"}`, true, false},
		{"opaque_error", 403, `{"error":"fake-oauth-refresh"}`, true, false},
		{"malformed", 200, `{"access_token":"fake-new-access"}`, true, false},
		{"lost_scope", 200, `{"access_token":"fake-new-access","expires_in":3600,"scope":"user:inference"}`, true, false},
		{"huge", 200, strings.Repeat("x", maxCredentialBytes+1), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(mockRouter(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tokenURL || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("wrong refresh request")
				}
				var req map[string]string
				if json.NewDecoder(r.Body).Decode(&req) != nil || req["refresh_token"] != "fake-oauth-refresh" || req["scope"] != requiredScopes || req["client_id"] != OfficialClientID {
					t.Error("wrong refresh body")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			client.now = func() time.Time { return now }
			next, ttl, err := client.RefreshHandle(context.Background(), cur)
			if (err != nil) != tc.failure || agentauth.Deterministic(err) != tc.deterministic {
				t.Fatalf("unexpected refresh result: %v", err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "fake-") {
					t.Fatal("upstream text leaked")
				}
				return
			}
			if ttl != time.Hour || !next.Expiry().Equal(now.Add(time.Hour)) || next.AccessToken() != "fake-new-access" {
				t.Fatal("incorrect refreshed lifetime or access")
			}
			blob, _ := next.JSON()
			want := "fake-new-refresh"
			if tc.name == "retained_refresh" {
				want = "fake-oauth-refresh"
			}
			if !strings.Contains(blob, want) {
				t.Fatal("refresh generation not retained")
			}
			if cur.AccessToken() != "fake-oauth-access" {
				t.Fatal("prior generation mutated")
			}
		})
	}
	client := NewClient(mockRouter(func(*http.Request) (*http.Response, error) { return nil, errors.New("fake-oauth-refresh") }))
	_, _, err := client.RefreshHandle(context.Background(), cur)
	if err == nil || strings.Contains(err.Error(), "fake-") {
		t.Fatal("transport error leaked")
	}
}

func TestOAuthRefreshNeverFollowsRedirect(t *testing.T) {
	var follow int
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { follow++ }))
	defer destination.Close()
	client := NewClient(mockRouter(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {destination.URL}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	cur, _ := Parse(oauthFixture)
	_, _, err := client.RefreshHandle(context.Background(), cur)
	if err == nil || follow != 0 {
		t.Fatal("OAuth request followed redirect")
	}
}
