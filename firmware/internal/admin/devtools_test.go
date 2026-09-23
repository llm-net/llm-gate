package admin_test

// GET/PUT /admin/v1/keys/{id}/dev-tools（「可用订阅」）的可执行验收：端点只管
// 四种订阅各自钉死的账号行，开发工具可见的模型集合（CatalogModelIDs）由
// api-models 端点整份替换、本端点原样保留。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type devToolsBody struct {
	Revision      int64 `json:"revision"`
	Subscriptions struct {
		Codex  *int64 `json:"codex"`
		Grok   *int64 `json:"grok"`
		Claude *int64 `json:"claude"`
		Cursor *int64 `json:"cursor"`
	} `json:"subscriptions"`
	SubscriptionStatus []struct {
		Provider     string `json:"provider"`
		Configured   bool   `json:"configured"`
		Available    bool   `json:"available"`
		AccountID    *int64 `json:"account_id"`
		AccountLabel string `json:"account_label"`
	} `json:"subscription_status"`
}

func TestKeyDevToolsRequiresSessionAndReplacesAtomically(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	key, err := e.st.CreateAPIKey(t.Context(), "dev", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sk_test", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	up, err := e.st.CreateUpstream(t.Context(), "deepseek", config.UpstreamDeepseek, "sk-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	model, err := e.st.CreateModel(t.Context(), "deepseek-v4-flash", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateModelSource(t.Context(), model.ID, up.ID, "deepseek-v4-flash", 10); err != nil {
		t.Fatal(err)
	}
	// 同一种订阅两个账号，外加一个 Claude 账号（只配 OAuth，不可用于调用）。
	codexA, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: store.AgentProviderCodex, Label: "甲", AuthJSON: `{"tokens":{"access_token":"fake-a"}}`})
	if err != nil {
		t.Fatal(err)
	}
	codexB, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: store.AgentProviderCodex, Label: "乙", AuthJSON: `{"tokens":{"access_token":"fake-b"}}`, Status: store.AgentStatusDisabled})
	if err != nil {
		t.Fatal(err)
	}
	claude, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: store.AgentProviderClaude, Label: "Claude", AuthJSON: `{"oauth":{"access_token":"fake-quota","refresh_token":"fake-refresh","expires_at":2000000000000,"scopes":["user:profile"]}}`})
	if err != nil {
		t.Fatal(err)
	}
	// 既有的模型可见选择：订阅 PUT 前后必须原样保留。
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: key.ID, CatalogModelIDs: []int64{model.ID},
	}); err != nil {
		t.Fatal(err)
	}
	path := "/admin/v1/keys/" + jsonNumber(key.ID) + "/dev-tools"
	if got := e.do(http.MethodGet, path, "", ""); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET=%d", got.StatusCode)
	}
	body := `{"subscriptions":{"codex":` + jsonNumber(codexA.ID) + `,"grok":null,"claude":` + jsonNumber(claude.ID) + `,"cursor":null}}`
	resp := e.do(http.MethodPut, path, cookie, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	var got devToolsBody
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Revision != 2 || got.Subscriptions.Codex == nil || *got.Subscriptions.Codex != codexA.ID || got.Subscriptions.Grok != nil ||
		got.Subscriptions.Claude == nil || *got.Subscriptions.Claude != claude.ID || got.Subscriptions.Cursor != nil ||
		len(got.SubscriptionStatus) != 4 {
		t.Fatalf("response=%+v", got)
	}
	// subscription_status 逐订阅带钉死账号的 id/名称与可用性：codex 甲可用；
	// claude 只配了 OAuth，已授权但不可用；没钉的两家既未授权也没有账号信息。
	for _, s := range got.SubscriptionStatus {
		switch s.Provider {
		case "codex":
			if !s.Configured || !s.Available || s.AccountID == nil || *s.AccountID != codexA.ID || s.AccountLabel != "甲" {
				t.Fatalf("codex status=%+v", s)
			}
		case "claude":
			if !s.Configured || s.Available || s.AccountID == nil || *s.AccountID != claude.ID || s.AccountLabel != "Claude" {
				t.Fatalf("claude status=%+v", s)
			}
		default:
			if s.Configured || s.Available || s.AccountID != nil || s.AccountLabel != "" {
				t.Fatalf("%s status=%+v", s.Provider, s)
			}
		}
	}
	// 审计 detail 是策略全量快照（四个账号行 id 与保留的 model_ids），凭据不经
	// 这条路、无脱敏顾虑。
	wantDetail := "codex=" + jsonNumber(codexA.ID) + " grok=0 claude=" + jsonNumber(claude.ID) + " cursor=0 model_ids=[" + jsonNumber(model.ID) + "] revision=2"
	var audited bool
	for _, row := range auditRows(t, e.dir) {
		if row.event != "key.dev_tools_update" {
			continue
		}
		audited = true
		if row.detail != wantDetail {
			t.Fatalf("audit detail=%q want %q", row.detail, wantDetail)
		}
	}
	if !audited {
		t.Fatal("missing key.dev_tools_update audit row")
	}
	cfg, err := e.st.GetDevToolConfig(t.Context(), key.ID)
	if err != nil || len(cfg.CatalogModelIDs) != 1 || cfg.CatalogModelIDs[0] != model.ID {
		t.Fatalf("模型可见选择不该被订阅 PUT 改动: %+v err=%v", cfg, err)
	}
	// Identical replace is idempotent and does not manufacture a revision.
	resp = e.do(http.MethodPut, path, cookie, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.Revision != 2 {
		t.Fatalf("second revision=%d err=%v", got.Revision, err)
	}
	// 同一种订阅换到另一个账号（停用的乙）：已授权、当前不可用。
	resp = e.do(http.MethodPut, path, cookie, `{"subscriptions":{"codex":`+jsonNumber(codexB.ID)+`}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("switch PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.Revision != 3 || got.Subscriptions.Claude != nil ||
		got.Subscriptions.Codex == nil || *got.Subscriptions.Codex != codexB.ID {
		t.Fatalf("switch response=%+v err=%v", got, err)
	}
	for _, s := range got.SubscriptionStatus {
		if s.Provider == "codex" && (!s.Configured || s.Available || s.AccountLabel != "乙") {
			t.Fatalf("disabled pinned account status=%+v", s)
		}
	}
	// 非法账号整份拒：不存在的行、别家订阅的行、非正整数，都是 400 且不写盘。
	for _, bad := range []string{
		`{"subscriptions":{"codex":9999}}`,
		`{"subscriptions":{"grok":` + jsonNumber(codexA.ID) + `}}`,
		`{"subscriptions":{"cursor":0}}`,
		`{"subscriptions":{"cursor":-1}}`,
	} {
		resp = e.do(http.MethodPut, path, cookie, bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid %s PUT=%d", bad, resp.StatusCode)
		}
		if got := readAll(t, resp); !strings.Contains(got, "agent_account_invalid") {
			t.Fatalf("invalid %s body=%s", bad, got)
		}
	}
	// 旧契约的布尔与 catalog_model_ids 字段不再被本端点接受（未知字段/类型不符整体 400）。
	for _, stale := range []string{
		`{"subscriptions":{"codex":true}}`,
		`{"subscriptions":{"codex":null},"catalog_model_ids":[1]}`,
	} {
		if resp = e.do(http.MethodPut, path, cookie, stale); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("stale %s PUT=%d", stale, resp.StatusCode)
		}
	}
	cfg, err = e.st.GetDevToolConfig(t.Context(), key.ID)
	if err != nil || cfg.Revision != 3 || cfg.CodexAccountID != codexB.ID || cfg.ClaudeAccountID != 0 {
		t.Fatalf("partial write: %+v err=%v", cfg, err)
	}
	// 删掉被钉的账号：这把 Key 的钉解开、revision 递增，GET 读到 null。
	if resp = e.do(http.MethodDelete, "/admin/v1/agent-accounts/"+jsonNumber(codexB.ID), cookie, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE=%d", resp.StatusCode)
	}
	resp = e.do(http.MethodGet, path, cookie, "")
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.Revision != 4 || got.Subscriptions.Codex != nil {
		t.Fatalf("after delete=%+v err=%v", got, err)
	}
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
