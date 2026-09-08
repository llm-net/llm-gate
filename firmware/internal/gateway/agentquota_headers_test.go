package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestClaudeQuotaHeadersObservedWithoutChangingResponse(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			const body = `{"type":"message","content":[{"type":"text","text":"quota-response-private-marker"}]}`
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
				w.Header().Set("anthropic-ratelimit-unified-7d-utilization", "0.15")
				w.Header().Set("anthropic-ratelimit-unified-5h-reset", "2000000000")
				w.Header().Set("anthropic-ratelimit-unified-7d-reset", "2000604800")
				w.WriteHeader(status)
				io.WriteString(w, body)
			}))
			t.Cleanup(backend.Close)
			e := newRouteEnv(t)
			e.srv.SetClaudeEndpoint(backend.URL)
			id := connectClaudeCredential(t, e, claudeOAuthFake)
			m := agentquota.NewManager(e.st, func(context.Context, *store.AgentAccount) (agentquota.Data, error) {
				t.Error("observing a normal response triggered an extra quota request")
				return agentquota.Data{}, nil
			})
			e.srv.SetAgentQuota(m)
			w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), `{"model":"`+claudeTestModel+`","messages":[]}`, true)
			if w.Code != status || w.Body.String() != body || w.Header().Get("anthropic-ratelimit-unified-5h-utilization") != "0.42" {
				t.Fatal("quota observation changed the upstream response")
			}
			a, err := e.st.GetAgentAccount(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			q := m.Read(a)
			if q.Source != "response_headers" || len(q.Windows) != 2 || q.LastSuccessAt == nil || q.Stale || *q.Windows[0].UsedBPS != 4200 || *q.Windows[1].UsedBPS != 1500 {
				t.Fatalf("normal response did not update the quota: %+v", q)
			}
			for _, secret := range []string{claudeOAuthFake, testKey, "quota-response-private-marker"} {
				if strings.Contains(e.logBuf.String(), secret) {
					t.Fatal("quota observation leaked a credential or response body")
				}
			}
		})
	}
}
