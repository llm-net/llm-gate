package devtoolpolicy

import (
	"context"
	"slices"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// CLI 投影与实际来源协议相同，不能把 Chat-only 型号放进 Claude 或把
// 原生 Responses-only 型号当成支持 Chat/目录 Responses。
func TestOpenCodeGoModelProtocolProjection(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"mimo-v2.5", []string{"opencode"}},
		{"longcat-2.0", []string{"opencode"}},
		{"minimax-m3", []string{"claude"}},
		{"gpt-5.6-luna", []string{}},
	} {
		m := &store.ModelWithSources{
			Model:   store.Model{Name: "alias", Kind: store.ModelKindText, EntryOpenAI: true, EntryResponses: true, EntryAnthropic: true},
			Sources: []store.ModelSourceDetail{{ModelSource: store.ModelSource{UpstreamModelID: tc.model}, UpstreamType: "opencode_go", UpstreamCatalogID: "opencode_go"}},
		}
		if got := compatibleTools(platformcatalog.Builtin(), m); !slices.Equal(got, tc.want) {
			t.Fatalf("%s 工具投影: %v want %v", tc.model, got, tc.want)
		}
	}
}

func openPolicyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func createPolicyKey(t *testing.T, st *store.Store, suffix string) int64 {
	t.Helper()
	k, err := st.CreateAPIKey(t.Context(), suffix, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abc"+suffix, "sk_test", suffix, "")
	if err != nil {
		t.Fatal(err)
	}
	return k.ID
}

func TestSnapshotIsPerKeyAndCatalogProjectsByProtocol(t *testing.T) {
	st := openPolicyStore(t)
	keyA := createPolicyKey(t, st, "1")
	keyB := createPolicyKey(t, st, "2")
	deepseek, err := st.CreateUpstream(t.Context(), "deepseek", config.UpstreamDeepseek, "sk-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	anthropic, err := st.CreateUpstream(t.Context(), "anthropic", config.UpstreamAnthropicCompat, "sk-fake", "https://anthropic.invalid/v1")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.CreateModel(t.Context(), "deepseek-v4-flash", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModelSource(t.Context(), model.ID, deepseek.ID, "deepseek-v4-flash", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModelSource(t.Context(), model.ID, anthropic.ID, "deepseek-v4-flash", 20); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keyA, AllowCodexSubscription: true, CatalogModelIDs: []int64{model.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderCodex, Label: "codex", AccountID: "test",
		DefaultModel: "gpt-5.6-sol", AuthJSON: `{"tokens":{"access_token":"fake"}}`,
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{
		Store: st,
		AgentModels: func(context.Context) (map[string]string, error) {
			return map[string]string{"gpt-5.6-sol": store.AgentProviderCodex}, nil
		},
		PlatformModels: func(context.Context) platformcatalog.Doc { return platformcatalog.Builtin() },
	}
	a, err := r.Snapshot(t.Context(), keyA)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Subscription("codex").Available || a.Subscription("claude").Configured {
		t.Fatalf("subscriptions=%+v", a.Subscriptions)
	}
	if !a.IsCatalogModel("codex", model.Name) || !a.IsCatalogModel("claude", model.Name) ||
		!a.IsCatalogModel("opencode", model.Name) {
		t.Fatalf("catalog projection=%+v", a.Tools)
	}
	if a.Tools["codex"].DefaultModel != "gpt-5.6-sol" || a.Tools["grok"].DefaultModel != "" {
		t.Fatalf("defaults=%+v", a.Tools)
	}
	b, err := r.Snapshot(t.Context(), keyB)
	if err != nil {
		t.Fatal(err)
	}
	if b.Revision != 0 || len(b.Tools["codex"].Models) != 0 || len(b.Tools["opencode"].Models) != 0 ||
		b.Subscription("codex").Configured {
		t.Fatalf("unconfigured key leaked policy: %+v", b)
	}
	// Each tool follows its own protocol surface on the next snapshot.
	for _, tc := range []struct {
		chat, responses, anthropic bool
	}{
		{false, true, false},
		{true, false, false},
		{false, false, true},
	} {
		if err := st.SetModelEntries(t.Context(), model.ID, tc.chat, tc.responses, tc.anthropic); err != nil {
			t.Fatal(err)
		}
		snapshot, err := r.Snapshot(t.Context(), keyA)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.IsCatalogModel("codex", model.Name) != tc.responses ||
			snapshot.IsCatalogModel("opencode", model.Name) != tc.chat ||
			snapshot.IsCatalogModel("claude", model.Name) != tc.anthropic {
			t.Fatalf("independent surface projection: %+v, tools=%+v", tc, snapshot.Tools)
		}
		if !snapshot.Subscription("codex").Available {
			t.Fatal("catalog surface switch affected the subscription")
		}
	}
}

func TestOpenCodeAcceptsOpenAIChatWithoutCodexCapabilities(t *testing.T) {
	st := openPolicyStore(t)
	keyID := createPolicyKey(t, st, "6")
	up, err := st.CreateUpstream(t.Context(), "openai", config.UpstreamOpenAICompat, "sk-fake", "https://openai.invalid/v1")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.CreateModel(t.Context(), "plain-chat-model", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModelSource(t.Context(), model.ID, up.ID, model.Name, 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keyID, CatalogModelIDs: []int64{model.ID},
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := (&Resolver{Store: st}).Snapshot(t.Context(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Tools["opencode"]; got.DefaultModel != model.Name || len(got.Models) != 1 ||
		got.Models[0] != (Model{Name: model.Name, Source: "catalog"}) {
		t.Fatalf("OpenCode projection=%+v", got)
	}
	if got := snap.Tools["codex"].Models; len(got) != 0 {
		t.Fatalf("plain OpenAI Chat model leaked into Codex: %+v", got)
	}
	if options, err := (&Resolver{Store: st}).ModelOptions(t.Context(), keyID); err != nil ||
		len(options) != 1 || !slices.Equal(options[0].Tools, []string{"opencode"}) {
		t.Fatalf("OpenCode model options=%+v err=%v", options, err)
	}
}

func TestArkPlanCurrentModelsProjectToCodex(t *testing.T) {
	st := openPolicyStore(t)
	keyID := createPolicyKey(t, st, "4")
	arkPlan, err := st.CreateUpstream(t.Context(), "ark-plan", config.UpstreamArkPlan, "sk-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	var modelIDs []int64
	for _, name := range []string{"glm-5.3", "kimi-k3"} {
		model, err := st.CreateModel(t.Context(), name, store.ModelKindText, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateModelSource(t.Context(), model.ID, arkPlan.ID, name, 10); err != nil {
			t.Fatal(err)
		}
		modelIDs = append(modelIDs, model.ID)
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keyID, CatalogModelIDs: modelIDs,
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := (&Resolver{Store: st}).Snapshot(t.Context(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	got := snap.Tools["codex"].Models
	if len(got) != 2 || got[0].Name != "glm-5.3" || got[1].Name != "kimi-k3" {
		t.Fatalf("火山方舟订阅模型没有投影到 Codex：%+v", got)
	}
}

func TestClaudeVisibleModelNarrowsSubscriptionButKeepsCatalog(t *testing.T) {
	st := openPolicyStore(t)
	keyID := createPolicyKey(t, st, "5")
	up, err := st.CreateUpstream(t.Context(), "anthropic", config.UpstreamAnthropicCompat, "sk-fake", "https://anthropic.invalid/v1")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := st.CreateModel(t.Context(), "deepseek-v4-flash", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModelSource(t.Context(), catalog.ID, up.ID, catalog.Name, 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keyID, AllowClaudeSubscription: true, CatalogModelIDs: []int64{catalog.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderClaude, Label: "claude", DefaultModel: "claude-fable-5",
		AuthJSON: `{"setup_token":"fake"}`,
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Store: st, AgentModels: func(context.Context) (map[string]string, error) {
		return map[string]string{
			"claude-fable-5":  store.AgentProviderClaude,
			"claude-opus-5":   store.AgentProviderClaude,
			"claude-sonnet-5": store.AgentProviderClaude,
		}, nil
	}}

	snap, err := r.Snapshot(t.Context(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	got := snap.Tools["claude"]
	if got.DefaultModel != "claude-fable-5" || len(got.Models) != 2 ||
		got.Models[0] != (Model{Name: "claude-fable-5", Source: "subscription"}) ||
		got.Models[1] != (Model{Name: catalog.Name, Source: "catalog"}) {
		t.Fatalf("Claude 可见模型没有收窄订阅清单并保留目录模型：%+v", got)
	}
}

// TestCursorProjectsNoModels 钉住 cursor 的透明代理规则：订阅授权只给可用性，
// 目录模型和订阅型号都不投影，默认模型恒为空；模型由 cursor-agent 自行发现。
func TestCursorProjectsNoModels(t *testing.T) {
	st := openPolicyStore(t)
	keyID := createPolicyKey(t, st, "7")
	up, err := st.CreateUpstream(t.Context(), "openai", config.UpstreamOpenAICompat, "sk-fake", "https://openai.invalid/v1")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := st.CreateModel(t.Context(), "plain-chat-model", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModelSource(t.Context(), catalog.ID, up.ID, catalog.Name, 10); err != nil {
		t.Fatal(err)
	}
	cfg := store.DevToolConfig{
		KeyID: keyID, AllowCursorSubscription: true, CatalogModelIDs: []int64{catalog.ID},
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderCursor, Label: "cursor", DefaultModel: "cursor-fake-model",
		AuthJSON: `{"api_key":"fake"}`,
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Store: st, AgentModels: func(context.Context) (map[string]string, error) {
		return map[string]string{
			"cursor-fake-model": store.AgentProviderCursor,
		}, nil
	}}

	snap, err := r.Snapshot(t.Context(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	sub := snap.Subscription("cursor")
	if !sub.Configured || !sub.Available || sub.DefaultModel != "" {
		t.Fatalf("cursor subscription=%+v", snap.Subscriptions)
	}
	got := snap.Tools["cursor"]
	if got.DefaultModel != "" || len(got.Models) != 0 {
		t.Fatalf("cursor 不应投影模型：%+v", got)
	}
	// 勾选的目录模型照常投给别的工具，但绝不投给 cursor。
	if snap.IsCatalogModel("cursor", catalog.Name) || !snap.IsCatalogModel("opencode", catalog.Name) {
		t.Fatalf("catalog projection=%+v", snap.Tools)
	}
	options, err := r.ModelOptions(t.Context(), keyID)
	if err != nil || len(options) != 1 {
		t.Fatalf("options=%+v err=%v", options, err)
	}
	if slices.Contains(options[0].Tools, "cursor") {
		t.Fatalf("目录模型不该宣称兼容 cursor：%+v", options[0])
	}
	cfg.AllowCodexSubscription, cfg.AllowGrokSubscription, cfg.AllowClaudeSubscription = true, true, true
	if got := AllowedSubscriptions(cfg); !slices.Equal(got, []string{"codex", "grok", "claude", "cursor"}) {
		t.Fatalf("AllowedSubscriptions=%v", got)
	}
}

func TestSelectedCatalogWinsSameNameAndUnavailableSelectionIsRetained(t *testing.T) {
	st := openPolicyStore(t)
	keyID := createPolicyKey(t, st, "3")
	up, err := st.CreateUpstream(t.Context(), "deepseek", config.UpstreamDeepseek, "sk-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.CreateModel(t.Context(), "gpt-5.6-sol", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.CreateModelSource(t.Context(), model.ID, up.ID, "deepseek-v4-flash", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keyID, AllowCodexSubscription: true, CatalogModelIDs: []int64{model.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider: store.AgentProviderCodex, Label: "codex", AccountID: "test",
		DefaultModel: model.Name, AuthJSON: `{"tokens":{"access_token":"fake"}}`,
	}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Store: st, AgentModels: func(context.Context) (map[string]string, error) {
		return map[string]string{model.Name: store.AgentProviderCodex}, nil
	}}
	snap, err := r.Snapshot(t.Context(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Tools["codex"].Models; len(got) != 1 || got[0].Source != "catalog" {
		t.Fatalf("same-name priority=%+v", got)
	}
	if err := st.SetModelSourceDisabled(t.Context(), source.ID, true); err != nil {
		t.Fatal(err)
	}
	options, err := r.ModelOptions(t.Context(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || !options[0].Selected || options[0].Available || options[0].Reason != "source_unavailable" {
		t.Fatalf("retained option=%+v", options)
	}
}

func TestToolEnabledFollowsSubscriptionOrCatalogProjection(t *testing.T) {
	st := openPolicyStore(t)
	key := createPolicyKey(t, st, "7")
	deepseek, err := st.CreateUpstream(t.Context(), "deepseek", config.UpstreamDeepseek, "sk-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.CreateModel(t.Context(), "deepseek-v4-flash", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateModelSource(t.Context(), model.ID, deepseek.ID, "deepseek-v4-flash", 10); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Store: st, PlatformModels: func(context.Context) platformcatalog.Doc { return platformcatalog.Builtin() }}

	// 什么都没勾：四个工具都关着。
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{KeyID: key}); err != nil {
		t.Fatal(err)
	}
	snap, err := r.Snapshot(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"codex", "grok", "claude", "cursor", "opencode"} {
		if snap.ToolEnabled(tool) {
			t.Errorf("空策略下 %s 不该开放", tool)
		}
	}

	// 只勾 Grok 订阅（账号没连也算勾了）：只有 grok 开放。
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{KeyID: key, AllowGrokSubscription: true}); err != nil {
		t.Fatal(err)
	}
	if snap, err = r.Snapshot(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if !snap.ToolEnabled("grok") || snap.ToolEnabled("codex") || snap.ToolEnabled("claude") || snap.ToolEnabled("cursor") {
		t.Errorf("只勾 grok 订阅时的开放集 = codex:%v grok:%v claude:%v cursor:%v",
			snap.ToolEnabled("codex"), snap.ToolEnabled("grok"), snap.ToolEnabled("claude"), snap.ToolEnabled("cursor"))
	}

	// 只勾目录模型：投影到哪个工具，哪个工具就开放；grok / cursor 永远不靠目录开放。
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{KeyID: key, CatalogModelIDs: []int64{model.ID}}); err != nil {
		t.Fatal(err)
	}
	if snap, err = r.Snapshot(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if len(snap.Tools["codex"].Models) == 0 || len(snap.Tools["opencode"].Models) == 0 {
		t.Fatalf("前置：deepseek-v4-flash 应投影到 codex 与 opencode，得到 %+v", snap.Tools)
	}
	if !snap.ToolEnabled("codex") || !snap.ToolEnabled("opencode") {
		t.Errorf("有目录投影的工具应开放")
	}
	if snap.ToolEnabled("grok") || snap.ToolEnabled("cursor") {
		t.Errorf("grok / cursor 不该因目录模型开放")
	}
}

func TestClaudeQuotaOAuthDoesNotMakeSubscriptionAvailable(t *testing.T) {
	st := openPolicyStore(t)
	key := createPolicyKey(t, st, "7")
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{KeyID: key, AllowClaudeSubscription: true}); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{Store: st, AgentModels: func(context.Context) (map[string]string, error) {
		return map[string]string{"claude-test": "claude"}, nil
	}}
	for _, tc := range []struct {
		blob      string
		available bool
	}{
		{`{"oauth":{"access_token":"fake-quota","refresh_token":"fake-refresh","expires_at":2000000000000,"scopes":["user:profile"]}}`, false},
		{`{"setup_token":"fake-setup","quota_auth_expired":true,"oauth":{"access_token":"fake-quota","refresh_token":"fake-refresh","expires_at":2000000000000,"scopes":["user:profile"]}}`, true},
	} {
		if _, err := st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{Provider: "claude", AuthJSON: tc.blob}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := r.Snapshot(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshot.Subscription("claude").Configured || snapshot.Subscription("claude").Available != tc.available || snapshot.HasModel("claude", "claude-test") != tc.available {
			t.Fatal("Claude availability did not follow setup-token")
		}
	}
}
