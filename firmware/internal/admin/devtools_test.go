package admin_test

// GET/PUT /admin/v1/keys/{id}/dev-tools（「可用订阅」）的可执行验收：端点只管
// 四个订阅开关，开发工具可见的模型集合（CatalogModelIDs）由 api-models 端点
// 整份替换、本端点原样保留。

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

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
	body := `{"subscriptions":{"codex":true,"grok":false,"claude":true,"cursor":true}}`
	resp := e.do(http.MethodPut, path, cookie, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	var got struct {
		Revision      int64 `json:"revision"`
		Subscriptions struct {
			Codex  bool `json:"codex"`
			Grok   bool `json:"grok"`
			Claude bool `json:"claude"`
			Cursor bool `json:"cursor"`
		} `json:"subscriptions"`
		SubscriptionStatus []struct {
			Provider string `json:"provider"`
		} `json:"subscription_status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// subscription_status 是 devtoolpolicy.Snapshot 的原样投影，四种订阅各一条。
	if got.Revision != 2 || !got.Subscriptions.Codex || got.Subscriptions.Grok || !got.Subscriptions.Claude ||
		!got.Subscriptions.Cursor || len(got.SubscriptionStatus) != 4 {
		t.Fatalf("response=%+v", got)
	}
	// 审计 detail 是策略全量快照（含 cursor 与保留的 model_ids），凭据不经这条
	// 路、无脱敏顾虑。
	wantDetail := "codex=true grok=false claude=true cursor=true model_ids=[" + jsonNumber(model.ID) + "] revision=2"
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
	// 旧契约的 catalog_model_ids 字段不再被本端点接受（未知字段整体 400）。
	stale := `{"subscriptions":{"codex":false,"grok":true,"claude":false,"cursor":false},"catalog_model_ids":[1]}`
	if resp = e.do(http.MethodPut, path, cookie, stale); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("stale field PUT=%d", resp.StatusCode)
	}
	cfg, err = e.st.GetDevToolConfig(t.Context(), key.ID)
	if err != nil || cfg.Revision != 2 || !cfg.AllowCodexSubscription || cfg.AllowGrokSubscription ||
		!cfg.AllowCursorSubscription {
		t.Fatalf("partial write: %+v err=%v", cfg, err)
	}
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
