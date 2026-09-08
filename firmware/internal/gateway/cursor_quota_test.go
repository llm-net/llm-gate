package gateway

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	cursorQuotaAPIKey   = "fake-cursor-quota-api-key"
	cursorQuotaOldToken = "fake-cursor-quota-old-access"
	cursorQuotaNewToken = "fake-cursor-quota-new-access"
	cursorQuotaPath     = "/aiserver.v1.DashboardService/GetCurrentPeriodUsage"
	cursorQuotaBody     = `{"planUsage":{"autoPercentUsed":12}}`
)

func cursorQuotaResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func newCursorQuotaTestServer(t *testing.T, router quotaFuncRouter) (*Server, *store.AgentAccount, *cursorSession, *bytes.Buffer) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: store.AgentProviderCursor, AuthJSON: `{"api_key":"` + cursorQuotaAPIKey + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	logs := new(bytes.Buffer)
	s := New(&config.Config{}, logging.New(logs, slog.LevelDebug), st, nil, nil, router)
	sess := s.cursorSessionFor(a, cursorQuotaAPIKey)
	// Reproduce the cached access token used by both inference and quota sync.
	sess.token = cursorQuotaOldToken
	return s, a, sess, logs
}

func TestCursorQuotaReexchangesRejectedTokenOnce(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		firstStatus, retryStatus   int
		exchangeStatus             int
		exchangeNetworkError       bool
		wantCode                   string
		wantQueries, wantExchanges int32
	}{
		{name: "expired token recovers", firstStatus: 401, retryStatus: 200, exchangeStatus: 200, wantQueries: 2, wantExchanges: 1},
		{name: "second 401 stops", firstStatus: 401, retryStatus: 401, exchangeStatus: 200, wantCode: "unauthorized", wantQueries: 2, wantExchanges: 1},
		{name: "403 needs permission", firstStatus: 403, wantCode: "permission_required", wantQueries: 1},
		{name: "429 respects rate limit", firstStatus: 429, wantCode: "rate_limited", wantQueries: 1},
		{name: "exchange rejected", firstStatus: 401, exchangeStatus: 401, wantCode: "credential_unavailable", wantQueries: 1, wantExchanges: 1},
		{name: "exchange network failure", firstStatus: 401, exchangeNetworkError: true, wantCode: "credential_unavailable", wantQueries: 1, wantExchanges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queries, exchanges atomic.Int32
			router := quotaFuncRouter(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api2.cursor.sh" || r.Method != http.MethodPost {
					t.Error("unexpected Cursor quota destination or method")
				}
				switch r.URL.Path {
				case cursorExchangePath:
					exchanges.Add(1)
					if r.Header.Get("Authorization") != "Bearer "+cursorQuotaAPIKey {
						t.Error("exchange did not use the stored API key")
					}
					if tc.exchangeNetworkError {
						return nil, errors.New("fake-private-network-error-" + cursorQuotaAPIKey)
					}
					return cursorQuotaResponse(tc.exchangeStatus, `{"accessToken":"`+cursorQuotaNewToken+`"}`), nil
				case cursorQuotaPath:
					status, token := tc.firstStatus, cursorQuotaOldToken
					if queries.Add(1) > 1 {
						status, token = tc.retryStatus, cursorQuotaNewToken
					}
					if r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("quota did not use the expected access token")
					}
					body := cursorQuotaBody
					if status != 200 {
						body = `{"error":"fake-private-quota-body-` + cursorQuotaOldToken + `"}`
					}
					return cursorQuotaResponse(status, body), nil
				default:
					t.Error("unexpected Cursor endpoint")
					return cursorQuotaResponse(404, "{}"), nil
				}
			})
			s, a, sess, logs := newCursorQuotaTestServer(t, router)
			m := agentquota.NewManager(s.store, s.FetchAgentQuota)
			q := m.Sync(t.Context(), a, false)
			if q.ErrorCode != tc.wantCode || queries.Load() != tc.wantQueries || exchanges.Load() != tc.wantExchanges {
				t.Fatalf("code=%q queries=%d exchanges=%d", q.ErrorCode, queries.Load(), exchanges.Load())
			}
			if tc.wantCode == "" {
				if q.Status != "ok" || q.Stale || len(q.Windows) != 1 || q.Windows[0].UsedBPS == nil || *q.Windows[0].UsedBPS != 1200 {
					t.Fatalf("quota did not recover: %+v", q)
				}
				// Inference reuses the same refreshed token without another exchange.
				if token, err := s.cursorToken(t.Context(), sess); err != nil || token != cursorQuotaNewToken || exchanges.Load() != 1 {
					t.Fatal("quota refresh did not update the shared inference cache")
				}
			}
			current, err := s.store.GetAgentAccount(t.Context(), a.ID)
			if err != nil || current.Status != store.AgentStatusActive || !current.UpdatedAt.Equal(a.UpdatedAt) {
				t.Fatal("quota changed the subscription credential or status")
			}
			for _, secret := range []string{cursorQuotaAPIKey, cursorQuotaOldToken, cursorQuotaNewToken, "fake-private-quota-body", "fake-private-network-error"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("quota retry leaked private data to logs")
				}
			}
		})
	}
}

func TestCursorQuotaLate401KeepsNewToken(t *testing.T) {
	var oldQueries, exchanges atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	releaseRequest := sync.OnceFunc(func() { close(release) })
	defer releaseRequest()
	router := quotaFuncRouter(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == cursorExchangePath {
			exchanges.Add(1)
			return cursorQuotaResponse(200, `{"accessToken":"`+cursorQuotaNewToken+`"}`), nil
		}
		if r.URL.Path != cursorQuotaPath {
			t.Error("unexpected Cursor endpoint")
		}
		switch r.Header.Get("Authorization") {
		case "Bearer " + cursorQuotaOldToken:
			if oldQueries.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			return cursorQuotaResponse(401, "{}"), nil
		case "Bearer " + cursorQuotaNewToken:
			return cursorQuotaResponse(200, cursorQuotaBody), nil
		default:
			t.Error("unexpected access token")
			return cursorQuotaResponse(401, "{}"), nil
		}
	})
	s, a, sess, _ := newCursorQuotaTestServer(t, router)
	done := make(chan error, 1)
	go func() {
		_, err := s.FetchAgentQuota(t.Context(), a)
		done <- err
	}()
	<-started
	if _, err := s.FetchAgentQuota(t.Context(), a); err != nil {
		t.Fatalf("concurrent quota refresh: %v", err)
	}
	releaseRequest()
	if err := <-done; err != nil {
		t.Fatalf("late quota response: %v", err)
	}
	if token, err := s.cursorToken(t.Context(), sess); err != nil || token != cursorQuotaNewToken || exchanges.Load() != 1 {
		t.Fatal("late 401 invalidated the new token or caused duplicate exchange")
	}
}
