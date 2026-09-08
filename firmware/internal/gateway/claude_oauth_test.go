package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func claudeOAuthBlob(t *testing.T, access, refresh string, expiry time.Time) string {
	t.Helper()
	c, err := claudeauth.Parse(fmt.Sprintf(`{"oauth":{"access_token":%q,"refresh_token":%q,"expires_at":%d,"scopes":["user:profile","user:inference"]}}`, access, refresh, expiry.UnixMilli()))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func TestClaudeQuotaRefreshDoesNotBlockSetupInference(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: claudeCombinedBlob(t, "fake-old", "fake-refresh", time.Now().Add(-time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	router := quotaFuncRouter(func(r *http.Request) (*http.Response, error) {
		body := `{"five_hour":{"utilization":42,"resets_at":"2033-05-18T03:33:20Z"},"seven_day":{"utilization":15}}`
		if r.URL.Path == "/v1/oauth/token" {
			if refreshes.Add(1) == 1 {
				close(started)
			}
			<-release
			body = `{"access_token":"fake-new","refresh_token":"fake-rotated","expires_in":3600,"scope":"user:profile user:inference"}`
		} else if r.URL.Path == "/api/oauth/usage" {
			if r.Header.Get("Authorization") != "Bearer fake-new" {
				t.Error("quota used inference credential")
			}
		} else {
			if r.Header.Get("Authorization") != "Bearer fake-setup" {
				t.Error("inference used OAuth credential")
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	var logs bytes.Buffer
	s := New(&config.Config{}, logging.New(&logs, slog.LevelDebug), st, nil, nil, router)
	m := agentquota.NewManager(st, s.FetchAgentQuota)
	a, _ := st.GetAgentAccount(t.Context(), 1)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); m.Sync(context.Background(), a, false) }()
	}
	<-started
	dataDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.forwardClaude(w, httptest.NewRequest("POST", "/agents/claude/v1/messages", nil), "/v1/messages", []byte(`{"model":"fake-model","messages":[]}`), "fake-model", nil, nil)
		dataDone <- w.Code
	}()
	select {
	case code := <-dataDone:
		if code != 200 {
			t.Error("inference failed during quota refresh")
		}
	case <-time.After(time.Second):
		t.Error("inference blocked on quota refresh")
	}
	// Updating setup-token while refresh is in flight must survive token rotation.
	_, err = st.MutateAgentAuth(t.Context(), a.ID, "claude", false, func(_ *store.AgentAccount, blob string) (string, error) {
		c, err := claudeauth.Parse(blob)
		if err != nil {
			return "", err
		}
		next, _ := claudeauth.FromSetupToken("fake-replaced-setup")
		return c.WithSetupToken(next).JSON()
	})
	if err != nil {
		t.Error(err)
	}
	// A quota request loading the setup-only update must reuse the current owner
	// immediately, even while that owner is blocked on its upstream refresh.
	updated, updatedBlob, err := st.GetAgentCredential(t.Context(), "claude")
	if err != nil {
		t.Error(err)
	}
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		if _, err := s.agentSessionFor(updated, updatedBlob); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ownerDone:
	case <-time.After(time.Second):
		t.Error("setup-only update tried to reset a rotating quota owner")
	}
	close(release)
	<-ownerDone
	wg.Wait()

	if refreshes.Load() != 1 {
		t.Fatalf("multiple refresh owners: %d", refreshes.Load())
	}
	current, blob, err := st.GetAgentCredential(t.Context(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blob, "fake-replaced-setup") || !strings.Contains(blob, "fake-rotated") || current.LastRefreshAt.IsZero() || m.Read(current).Status != "ok" {
		t.Fatal("rotation not persisted or quota missing")
	}
	// Restart reuses the sealed generation instead of refreshing the old token.
	s2 := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, router)
	if _, err := s2.FetchAgentQuota(t.Context(), current); err != nil || refreshes.Load() != 1 {
		t.Fatal("restart lost current generation")
	}
	for _, secret := range []string{"fake-old", "fake-refresh", "fake-new", "fake-rotated"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credential logged")
		}
	}
}

func TestClaudeQuota401RefreshesOnce(t *testing.T) {
	for _, quota := range []bool{true} {
		t.Run(fmt.Sprint(quota), func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			a, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: claudeOAuthBlob(t, "fake-old", "fake-refresh", time.Now().Add(time.Hour))})
			if err != nil {
				t.Fatal(err)
			}
			var refreshes, calls int
			const body = " {\"five_hour\":{\"utilization\":42}} \n"
			router := quotaFuncRouter(func(r *http.Request) (*http.Response, error) {
				status, response := 200, body
				if r.URL.Path == "/v1/oauth/token" {
					refreshes++
					response = `{"access_token":"fake-new","refresh_token":"fake-next","expires_in":3600}`
				} else {
					calls++
					if r.Header.Get("Authorization") == "Bearer fake-old" {
						status = 401
						response = `{"type":"error"}`
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response))}, nil
			})
			s := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, router)
			if _, err := s.FetchAgentQuota(t.Context(), a); err != nil {
				t.Fatal(err)
			}

			if refreshes != 1 || calls != 2 {
				t.Fatalf("unexpected retry count: refresh=%d data=%d", refreshes, calls)
			}
			current, _ := st.GetAgentAccount(t.Context(), a.ID)
			if current.Status != "active" {
				t.Fatal("401 expired refreshable OAuth login")
			}
		})
	}
}

func TestClaudeOAuthLateRefreshCannotOverwriteReconnect(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprint(rejected), func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			_, err = st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: claudeOAuthBlob(t, "fake-old", "fake-refresh", time.Now().Add(-time.Hour))})
			if err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			router := quotaFuncRouter(func(*http.Request) (*http.Response, error) {
				close(started)
				<-release
				status, body := 200, `{"access_token":"fake-late","refresh_token":"fake-late-refresh","expires_in":3600}`
				if rejected {
					status = 400
					body = `{"error":"invalid_grant"}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			s := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, router)
			done := make(chan struct{})
			go func() { defer close(done); _ = s.RefreshAgent(context.Background(), "claude") }()
			<-started
			want := claudeOAuthBlob(t, "fake-reconnected", "fake-reconnected-refresh", time.Now().Add(time.Hour))
			_, err = st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: want})
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			<-done
			a, got, err := st.GetAgentCredential(t.Context(), "claude")
			if err != nil || got != want || a.Status != "active" {
				t.Fatal("late old refresh replaced or disabled new login")
			}
		})
	}
}

func TestClaudeSetupQuotaRequiresOAuthWithoutNetwork(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: `{"setup_token":"fake-setup"}`})
	if err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, quotaFuncRouter(func(*http.Request) (*http.Response, error) {
		t.Fatal("setup-token quota query hit network")
		return nil, nil
	}))
	_, err = s.FetchAgentQuota(t.Context(), a)
	if err == nil || err.Error() != "subscription quota: permission_required" {
		t.Fatal("missing OAuth guidance")
	}
}

func claudeCombinedBlob(t *testing.T, access, refresh string, expiry time.Time) string {
	t.Helper()
	c, err := claudeauth.Parse(claudeOAuthBlob(t, access, refresh, expiry))
	if err != nil {
		t.Fatal(err)
	}
	setup, _ := claudeauth.FromSetupToken("fake-setup")
	blob, err := c.WithSetupToken(setup).JSON()
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func TestClaudeQuotaFailureNeverDisablesInference(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: claudeCombinedBlob(t, "fake-old", "fake-refresh", time.Now().Add(-time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	var refreshes int
	const response = " {\"fake\": true} \n"
	router := quotaFuncRouter(func(r *http.Request) (*http.Response, error) {
		status, body := 200, response
		if r.URL.Path == "/v1/oauth/token" {
			refreshes++
			status, body = 400, `{"error":"invalid_grant"}`
		} else if r.Header.Get("Authorization") != "Bearer fake-setup" {
			t.Error("model request used OAuth")
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	s := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, router)
	if _, err := s.FetchAgentQuota(t.Context(), a); err == nil {
		t.Fatal("rejected quota refresh accepted")
	}
	current, blob, err := st.GetAgentCredential(t.Context(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := claudeauth.Parse(blob)
	if current.Status != "active" || !c.OAuthExpired() || c.Token() != "fake-setup" {
		t.Fatal("quota failure changed inference state")
	}
	// The failure latch survives a process restart, without another refresh attempt.
	restarted := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, router)
	if _, err := restarted.FetchAgentQuota(t.Context(), current); err == nil || refreshes != 1 {
		t.Fatal("expired quota authorization retried")
	}
	w := httptest.NewRecorder()
	restarted.forwardClaude(w, httptest.NewRequest("POST", "/agents/claude/v1/messages", nil), "/v1/messages", []byte(`{}`), "fake-model", nil, nil)
	if w.Code != 200 || w.Body.String() != response || refreshes != 1 {
		t.Fatal("quota failure affected inference bytes or availability")
	}
}

func TestClaudeOAuthOnlyCannotInvokeModel(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: claudeOAuthBlob(t, "fake-old", "fake-refresh", time.Now().Add(-time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, quotaFuncRouter(func(*http.Request) (*http.Response, error) {
		t.Error("OAuth-only inference hit network")
		return nil, fmt.Errorf("unexpected network")
	}))
	w := httptest.NewRecorder()
	s.forwardClaude(w, httptest.NewRequest("POST", "/agents/claude/v1/messages", nil), "/v1/messages", []byte(`{}`), "fake-model", nil, nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "requires setup-token") {
		t.Fatal("missing setup-token guidance")
	}
}

func TestClaudeSetup401CannotDisableReplacementOrQuota(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint(replace), func(t *testing.T) {
			st, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			a, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: claudeCombinedBlob(t, "fake-oauth", "fake-refresh", time.Now().Add(time.Hour))})
			if err != nil {
				t.Fatal(err)
			}
			var dataCalls int
			s := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, quotaFuncRouter(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v1/oauth/token" {
					t.Error("setup-token triggered OAuth refresh")
				}
				if r.URL.Path == "/api/oauth/usage" {
					if r.Header.Get("Authorization") != "Bearer fake-oauth" {
						t.Error("quota used setup-token")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":12}}`))}, nil
				}
				dataCalls++
				if replace {
					_, err := st.MutateAgentAuth(t.Context(), a.ID, "claude", false, func(_ *store.AgentAccount, blob string) (string, error) {
						c, err := claudeauth.Parse(blob)
						if err != nil {
							return "", err
						}
						next, _ := claudeauth.FromSetupToken("fake-new-setup")
						return c.WithSetupToken(next).JSON()
					})
					if err != nil {
						t.Error(err)
					}
				}
				return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"fake-denied"}`))}, nil
			}))
			w := httptest.NewRecorder()
			s.forwardClaude(w, httptest.NewRequest("POST", "/agents/claude/v1/messages", nil), "/v1/messages", []byte(`{}`), "fake-model", nil, nil)
			current, blob, err := st.GetAgentCredential(t.Context(), "claude")
			if err != nil {
				t.Fatal(err)
			}
			c, _ := claudeauth.Parse(blob)
			wantStatus := "auth_expired"
			if replace {
				wantStatus = "active"
			}
			if w.Code != 401 || w.Body.String() != `{"error":"fake-denied"}` || dataCalls != 1 || current.Status != wantStatus || c.OAuthExpired() {
				t.Fatal("setup 401 damaged independent credentials")
			}
			m := agentquota.NewManager(st, s.FetchAgentQuota)
			if q := m.Sync(t.Context(), current, true); q.Status != "ok" {
				t.Fatal("failed setup-token paused quota queries")
			}
		})
	}
}
