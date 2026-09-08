package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func TestAgentQuotaAdminOnlyAndNoCredentials(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	a, err := e.st.UpsertAgentAccount(context.Background(), store.NewAgentAccount{Provider: "claude", AuthJSON: `{"setup_token":"fake-quota-secret"}`})
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/v1/agent-accounts/" + strconv.FormatInt(a.ID, 10) + "/quota/sync"
	wantStatus(t, e.do("POST", path, "", `{}`), http.StatusUnauthorized)
	wantStatus(t, e.do("POST", path, root, `{}`), http.StatusServiceUnavailable)
	var calls int
	m := agentquota.NewManager(e.st, func(context.Context, *store.AgentAccount) (agentquota.Data, error) {
		calls++
		v := int64(0)
		return agentquota.Data{Source: "api", Windows: []agentquota.Window{{ID: "week", Period: "week", UsedBPS: &v}}}, nil
	})
	e.srv.SetAgentQuota(m)
	resp := e.do("POST", path, root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("GET", "/admin/v1/agent-accounts", root, "")
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Accounts []struct {
			Quota agentquota.Snapshot `json:"quota"`
		} `json:"accounts"`
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(out.Accounts) != 1 || out.Accounts[0].Quota.Status != "ok" || *out.Accounts[0].Quota.Windows[0].UsedBPS != 0 {
		t.Fatalf("missing quota: %+v", out)
	}
	if strings.Contains(string(raw), "fake-quota-secret") || strings.Contains(e.buf.String(), "fake-quota-secret") {
		t.Fatal("credential leaked")
	}
	if err := e.st.DeleteAgentAccount(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.do("POST", path, root, `{}`), http.StatusNotFound)
}
