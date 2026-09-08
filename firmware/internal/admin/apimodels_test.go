package admin_test

// GET/PUT /admin/v1/keys/{id}/api-models（「可用模型」策略）的可执行验收：
//
//   - 无会话 401（与 /admin/v1/* 其余端点同规）。
//   - 缺省不限制：restricted=false、两份选择为空，候选是 API密钥接入 的模型，
//     每行叠加开发工具投影读数（dev_tools 兼容子集与 dev_tool_selected）。
//   - 订阅接入模型不进候选、也不接受选中；非开发工具模型不接受设为可见。
//   - 收窄时强制「开发工具可见 ⊆ 可用」，越界整份拒绝。
//   - PUT 同时整份替换调用范围与开发工具可见集合；订阅四开关原样保留。
//   - 整份替换幂等：等值 PUT 不递增 revision；模型非法整份拒绝、旧策略留存。
//   - 审计只记开关、模型 id 与 revision（不含任何凭据）。

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type apiModelsBody struct {
	Revision        int64   `json:"revision"`
	Restricted      bool    `json:"restricted"`
	ModelIDs        []int64 `json:"model_ids"`
	DevToolModelIDs []int64 `json:"dev_tool_model_ids"`
	Models          []struct {
		ID              int64    `json:"id"`
		Name            string   `json:"name"`
		Kind            string   `json:"kind"`
		Platforms       []string `json:"platforms"`
		Selected        bool     `json:"selected"`
		Selectable      bool     `json:"selectable"`
		Servable        bool     `json:"servable"`
		Reason          string   `json:"reason"`
		DevTools        []string `json:"dev_tools"`
		DevToolSelected bool     `json:"dev_tool_selected"`
		DevToolReason   string   `json:"dev_tool_reason"`
	} `json:"models"`
}

func TestKeyAPIModelsScopeAndSubscriptionGuard(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	key, err := e.st.CreateAPIKey(t.Context(), "api-models", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "sk_test", "test", "")
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
	// 订阅接入的文本计价行：名字在内置目录 agents 段里且一条来源都没挂。
	agentModel, err := e.st.CreateModel(t.Context(), "gpt-5.6-sol", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}

	path := "/admin/v1/keys/" + jsonNumber(key.ID) + "/api-models"
	if got := e.do(http.MethodGet, path, "", ""); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET=%d", got.StatusCode)
	}

	// 缺省：不限制，候选只有 API密钥接入 那一行，附带开发工具投影读数。
	var got apiModelsBody
	resp := e.do(http.MethodGet, path, cookie, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET=%d %s", resp.StatusCode, readAll(t, resp))
	}
	decodeInto(t, resp, &got)
	if got.Restricted || got.Revision != 0 || len(got.ModelIDs) != 0 || len(got.DevToolModelIDs) != 0 {
		t.Fatalf("缺省应不限制: %+v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != model.ID || got.Models[0].Kind != store.ModelKindText ||
		!got.Models[0].Servable || !got.Models[0].Selectable || len(got.Models[0].Platforms) != 1 {
		t.Fatalf("候选清单不符（订阅行不该在内）: %+v", got.Models)
	}
	if !slices.Equal(got.Models[0].DevTools, []string{"codex", "opencode", "claude"}) || got.Models[0].DevToolSelected {
		t.Fatalf("开发工具投影读数不符: %+v", got.Models[0])
	}

	// 订阅接入模型不接受选中：整份拒绝，策略保持缺省。
	bad := `{"restricted":true,"model_ids":[` + jsonNumber(agentModel.ID) + `]}`
	resp = e.do(http.MethodPut, path, cookie, bad)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("subscription model PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	cfg, err := e.st.GetKeyAPIModelConfig(t.Context(), key.ID)
	if err != nil || cfg.Restricted || cfg.Revision != 0 {
		t.Fatalf("拒绝后不该留下策略: %+v err=%v", cfg, err)
	}
	// 非开发工具模型（订阅行没挂来源）不接受设为可见。
	bad = `{"restricted":false,"model_ids":[],"dev_tool_model_ids":[` + jsonNumber(agentModel.ID) + `]}`
	if resp = e.do(http.MethodPut, path, cookie, bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-devtool dev id PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}

	// 收窄到一个模型并设为开发工具可见；重复 id 折成一条。
	body := `{"restricted":true,"model_ids":[` + jsonNumber(model.ID) + `,` + jsonNumber(model.ID) + `],"dev_tool_model_ids":[` + jsonNumber(model.ID) + `]}`
	resp = e.do(http.MethodPut, path, cookie, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	decodeInto(t, resp, &got)
	if got.Revision != 1 || !got.Restricted || len(got.ModelIDs) != 1 || got.ModelIDs[0] != model.ID ||
		len(got.DevToolModelIDs) != 1 || got.DevToolModelIDs[0] != model.ID ||
		len(got.Models) != 1 || !got.Models[0].Selected || !got.Models[0].DevToolSelected {
		t.Fatalf("收窄未生效: %+v", got)
	}
	devCfg, err := e.st.GetDevToolConfig(t.Context(), key.ID)
	if err != nil || len(devCfg.CatalogModelIDs) != 1 || devCfg.CatalogModelIDs[0] != model.ID {
		t.Fatalf("开发工具可见集合未落库: %+v err=%v", devCfg, err)
	}

	wantDetail := "restricted=true model_ids=[" + jsonNumber(model.ID) + "] revision=1"
	wantDevDetail := "codex=false grok=false claude=false cursor=false model_ids=[" + jsonNumber(model.ID) + "] revision=1"
	var audited, devAudited bool
	for _, row := range auditRows(t, e.dir) {
		switch row.event {
		case "key.api_models_update":
			audited = true
			if row.detail != wantDetail {
				t.Fatalf("audit detail=%q want %q", row.detail, wantDetail)
			}
		case "key.dev_tools_update":
			devAudited = true
			if row.detail != wantDevDetail {
				t.Fatalf("dev audit detail=%q want %q", row.detail, wantDevDetail)
			}
		}
	}
	if !audited || !devAudited {
		t.Fatalf("missing audit rows: api=%t dev=%t", audited, devAudited)
	}

	// 等值替换幂等：两份 revision 都不虚增。
	resp = e.do(http.MethodPut, path, cookie, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	decodeInto(t, resp, &got)
	if got.Revision != 1 {
		t.Fatalf("second revision=%d", got.Revision)
	}
	if devCfg, err = e.st.GetDevToolConfig(t.Context(), key.ID); err != nil || devCfg.Revision != 1 {
		t.Fatalf("dev revision=%d err=%v", devCfg.Revision, err)
	}

	// 收窄时可见集合必须落在可用集合内，越界整份拒绝、旧策略留存。
	bad = `{"restricted":true,"model_ids":[],"dev_tool_model_ids":[` + jsonNumber(model.ID) + `]}`
	if resp = e.do(http.MethodPut, path, cookie, bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("outside scope PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	cfg, err = e.st.GetKeyAPIModelConfig(t.Context(), key.ID)
	if err != nil || cfg.Revision != 1 || !cfg.Restricted || len(cfg.ModelIDs) != 1 {
		t.Fatalf("越界拒绝后旧策略应留存: %+v err=%v", cfg, err)
	}

	// 不存在的模型整份拒绝，旧策略留存。
	if resp = e.do(http.MethodPut, path, cookie, `{"restricted":true,"model_ids":[999999]}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing model PUT=%d", resp.StatusCode)
	}
	if resp = e.do(http.MethodPut, path, cookie, `{"restricted":true,"model_ids":[`+jsonNumber(model.ID)+`],"dev_tool_model_ids":[999999]}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing dev model PUT=%d", resp.StatusCode)
	}
	cfg, err = e.st.GetKeyAPIModelConfig(t.Context(), key.ID)
	if err != nil || cfg.Revision != 1 || !cfg.Restricted || len(cfg.ModelIDs) != 1 {
		t.Fatalf("partial write: %+v err=%v", cfg, err)
	}

	// 空选择是合法表达（一个 API 模型都不能调）；可见集合随之必须为空。
	resp = e.do(http.MethodPut, path, cookie, `{"restricted":true,"model_ids":[],"dev_tool_model_ids":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	decodeInto(t, resp, &got)
	if !got.Restricted || len(got.ModelIDs) != 0 || len(got.DevToolModelIDs) != 0 || got.Revision != 2 {
		t.Fatalf("空选择未落库: %+v", got)
	}

	// 不限制时可见集合不受可用集合约束（全部模型本来就可用）。
	resp = e.do(http.MethodPut, path, cookie, `{"restricted":false,"model_ids":[],"dev_tool_model_ids":[`+jsonNumber(model.ID)+`]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unrestricted dev PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	decodeInto(t, resp, &got)
	if got.Restricted || len(got.DevToolModelIDs) != 1 || !got.Models[0].DevToolSelected {
		t.Fatalf("不限制下的可见集合未生效: %+v", got)
	}

	// Key 不存在：读写两侧都是 404。
	missing := "/admin/v1/keys/" + jsonNumber(key.ID+999) + "/api-models"
	if resp = e.do(http.MethodGet, missing, cookie, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing key GET=%d", resp.StatusCode)
	}
	if resp = e.do(http.MethodPut, missing, cookie, `{"restricted":false,"model_ids":[]}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing key PUT=%d", resp.StatusCode)
	}
}

// PUT 只整份替换调用范围与开发工具可见集合；「可用订阅」四开关不经本端点，
// 必须原样保留。
func TestKeyAPIModelsPreservesSubscriptions(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	key, err := e.st.CreateAPIKey(t.Context(), "api-models", "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", "sk_test", "test", "")
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
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: key.ID, AllowCodexSubscription: true, AllowCursorSubscription: true,
		CatalogModelIDs: []int64{model.ID},
	}); err != nil {
		t.Fatal(err)
	}
	path := "/admin/v1/keys/" + jsonNumber(key.ID) + "/api-models"

	var got apiModelsBody
	decodeInto(t, e.do(http.MethodGet, path, cookie, ""), &got)
	if len(got.DevToolModelIDs) != 1 || len(got.Models) != 1 || !got.Models[0].DevToolSelected {
		t.Fatalf("既有开发工具选择应读得到: %+v", got)
	}
	// 清掉可见集合：订阅开关原样保留。
	if resp := e.do(http.MethodPut, path, cookie, `{"restricted":true,"model_ids":[`+jsonNumber(model.ID)+`],"dev_tool_model_ids":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT=%d", resp.StatusCode)
	}
	devCfg, err := e.st.GetDevToolConfig(t.Context(), key.ID)
	if err != nil || len(devCfg.CatalogModelIDs) != 0 || !devCfg.AllowCodexSubscription ||
		devCfg.AllowGrokSubscription || devCfg.AllowClaudeSubscription || !devCfg.AllowCursorSubscription {
		t.Fatalf("订阅开关不该被 api-models PUT 改动: %+v err=%v", devCfg, err)
	}
}

// 选中之后模型改归订阅接入（挂着的来源被删）：它仍留在候选里且带原因，
// 整份替换照收——否则那一行取消不掉。仅因开发工具可见挂在清单里的行同理，
// 但 selectable=false，不能新勾为可用。
func TestKeyAPIModelsKeepsSelectionThatLeftAPIDomain(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()
	key, err := e.st.CreateAPIKey(t.Context(), "api-models", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "sk_test", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	up, err := e.st.CreateUpstream(t.Context(), "openai", config.UpstreamOpenAICompat, "sk-fake", "https://example.invalid/v1")
	if err != nil {
		t.Fatal(err)
	}
	model, err := e.st.CreateModel(t.Context(), "gpt-5.6-sol", store.ModelKindText, "")
	if err != nil {
		t.Fatal(err)
	}
	src, err := e.st.CreateModelSource(t.Context(), model.ID, up.ID, "gpt-5.6-sol", 10)
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/v1/keys/" + jsonNumber(key.ID) + "/api-models"
	body := `{"restricted":true,"model_ids":[` + jsonNumber(model.ID) + `],"dev_tool_model_ids":[` + jsonNumber(model.ID) + `]}`
	if resp := e.do(http.MethodPut, path, cookie, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT=%d %s", resp.StatusCode, readAll(t, resp))
	}
	// 删掉唯一来源：这一行从此归订阅接入侧，也失去开发工具兼容性。
	if err := e.st.DeleteModelSource(t.Context(), src.ID); err != nil {
		t.Fatal(err)
	}
	var got apiModelsBody
	resp := e.do(http.MethodGet, path, cookie, "")
	decodeInto(t, resp, &got)
	if len(got.Models) != 1 || !got.Models[0].Selected || !got.Models[0].Selectable ||
		got.Models[0].Servable || got.Models[0].Reason == "" {
		t.Fatalf("离开 API 侧的选择应保留并带原因: %+v", got.Models)
	}
	if !got.Models[0].DevToolSelected || got.Models[0].DevToolReason == "" {
		t.Fatalf("失去兼容性的可见选择应保留并带机器原因: %+v", got.Models[0])
	}
	if resp := e.do(http.MethodPut, path, cookie, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("旧选择应照收: %d %s", resp.StatusCode, readAll(t, resp))
	}
	// 取消它之后就不再是合法选择了。
	if resp := e.do(http.MethodPut, path, cookie, `{"restricted":true,"model_ids":[],"dev_tool_model_ids":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("清空=%d", resp.StatusCode)
	}
	if resp := e.do(http.MethodPut, path, cookie, body); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("重新选中订阅行应被拒: %d", resp.StatusCode)
	}
	var probe struct {
		Models []json.RawMessage `json:"models"`
	}
	decodeInto(t, e.do(http.MethodGet, path, cookie, ""), &probe)
	if len(probe.Models) != 0 {
		t.Fatalf("取消后订阅行不该再进候选: %s", probe.Models)
	}
}
