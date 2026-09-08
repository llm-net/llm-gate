package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type quotaRouter struct {
	t        *testing.T
	requests int
}

type quotaFuncRouter func(*http.Request) (*http.Response, error)

func (f quotaFuncRouter) Transport(_ egress.Scope, _ *http.Transport) http.RoundTripper {
	return quotaRoundTrip(f)
}

func TestQuotaUsesExistingOAuthRefreshOwner(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.UpsertAgentAccount(context.Background(), store.NewAgentAccount{Provider: "codex", AuthJSON: `{"tokens":{"access_token":"fake-old-access","refresh_token":"fake-refresh","account_id":"fake-account"}}`})
	if err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	router := quotaFuncRouter(func(r *http.Request) (*http.Response, error) {
		body := `{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":604800}}}`
		if r.URL.Path == "/oauth/token" {
			if refreshes.Add(1) == 1 {
				close(started)
			}
			<-release
			body = `{"access_token":"fake-new-access","refresh_token":"fake-new-refresh","expires_in":3600}`
		} else {
			if r.URL.Host != "chatgpt.com" || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("Authorization") != "Bearer fake-new-access" || r.Header.Get("ChatGPT-Account-ID") != "fake-account" {
				t.Error("quota did not use the refreshed account credential")
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	s := New(&config.Config{}, logging.New(io.Discard, slog.LevelDebug), st, nil, nil, router)
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- s.RefreshAgent(context.Background(), "codex") }()
	<-started
	m := agentquota.NewManager(st, s.FetchAgentQuota)
	syncDone := make(chan agentquota.Snapshot, 1)
	go func() { syncDone <- m.Sync(context.Background(), a, false) }()
	close(release)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	q := <-syncDone
	if q.Status != "ok" || refreshes.Load() != 1 {
		t.Fatalf("refresh owners=%d quota=%+v", refreshes.Load(), q)
	}
}

type quotaRoundTrip func(*http.Request) (*http.Response, error)

func (f quotaRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func (q *quotaRouter) Transport(scope egress.Scope, _ *http.Transport) http.RoundTripper {
	return quotaRoundTrip(func(r *http.Request) (*http.Response, error) {
		q.requests++
		if scope != egress.ScopeAgentAuth {
			q.t.Error("quota bypassed agent_auth policy")
		}
		if r.URL.Host != "api.anthropic.com" || r.URL.Path != "/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer fake-claude-quota-secret" {
			q.t.Error("incorrect quota request")
		}
		return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fake-claude-quota-secret"}}`))}, nil
	})
}
func TestQuotaPermissionDoesNotExpireSubscription(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.UpsertAgentAccount(context.Background(), store.NewAgentAccount{Provider: "claude", AuthJSON: claudeOAuthBlob(t, "fake-claude-quota-secret", "fake-claude-refresh", time.Now().Add(time.Hour))})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	router := &quotaRouter{t: t}
	s := New(&config.Config{}, logging.New(&logs, slog.LevelDebug), st, nil, nil, router)
	m := agentquota.NewManager(st, s.FetchAgentQuota)
	s.SetAgentQuota(m)
	q := m.Sync(context.Background(), a, false)
	if q.ErrorCode != "permission_required" || router.requests != 1 {
		t.Fatalf("quota status: %+v", q)
	}
	a, err = st.GetAgentAccount(context.Background(), a.ID)
	if err != nil || a.Status != "active" {
		t.Fatal("quota permission disabled inference")
	}
	if strings.Contains(logs.String(), "fake-claude-quota-secret") {
		t.Fatal("secret logged")
	}
}
