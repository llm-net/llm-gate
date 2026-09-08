package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

func TestCodexMixedSelectedModelUsesCatalog(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatJSONReply(`{
		"id":"chatcmpl-mixed","choices":[{"message":{"role":"assistant","content":"catalog ok"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":4,"completion_tokens":2}}`))
	upstreamID := dbUpstream(t, e.st, "deepseek", config.UpstreamDeepseek, "sk-not-real", stub.url)
	modelID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, modelID, upstreamID, "deepseek-v4-flash", 100)
	if err := e.st.SetModelPricing(t.Context(), modelID, meterPricing); err != nil {
		t.Fatalf("SetModelPricing: %v", err)
	}
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	selectDevToolModels(t, e.st, modelID)
	w := do(e.h, http.MethodPost,
		"/agents/codex/v1/responses", chatAuth, `{"model":"deepseek-v4-flash","input":"hi","store":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if stub.count() != 1 {
		t.Fatalf("目录上游调用次数 = %d，期望 1", stub.count())
	}
	if sample := fm.only(t); sample.Entry != usage.EntryResponses || sample.ModelName != "deepseek-v4-flash" {
		t.Fatalf("勾选模型应按目录 Responses 记账：%+v", sample)
	}
}

func TestCodexMixedUnselectedModelUsesSubscription(t *testing.T) {
	e := newAgentEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(codexNonStreamBody))
	})
	w := do(e.h, http.MethodPost,
		"/agents/codex/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if e.backend.count() != 1 {
		t.Fatalf("订阅后端调用次数 = %d，期望 1", e.backend.count())
	}
}

func TestCodexMixedModelsMergesOnlySelectedCatalogModels(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	dbModel(t, e.st, codexModel)
	stub := newStub(t, chatJSONReply(`{}`))
	upstreamID := dbUpstream(t, e.st, "deepseek", config.UpstreamDeepseek, "sk-not-real", stub.url)
	catalogID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, catalogID, upstreamID, "deepseek-v4-flash", 100)
	selectDevToolModels(t, e.st, catalogID)
	e.srv.SetAgentModels(&fakeAgentModels{owners: map[string]string{
		codexModel: store.AgentProviderCodex,
	}})

	got := decodeModelList(t, do(e.h, http.MethodGet, "/agents/codex/v1/models", chatAuth, ""))
	if len(got) != 2 || got[0].ID != "deepseek-v4-flash" || got[1].ID != codexModel {
		t.Fatalf("混合目录 = %+v，期望订阅模型 + 仍可用的勾选模型", got)
	}
	if got[0].OwnedBy != "llmgate" || got[1].OwnedBy != store.AgentProviderCodex {
		t.Fatalf("owned_by 未区分订阅与目录：%+v", got)
	}

	selectDevToolModels(t, e.st)
	got = decodeModelList(t, do(e.h, http.MethodGet, "/agents/codex/v1/models", chatAuth, ""))
	if len(got) != 1 || got[0].ID != codexModel {
		t.Fatalf("未勾选时应为纯订阅目录：%+v", got)
	}
}

func TestCodexMixedRejectsInvalidSelectionAndRequiresKey(t *testing.T) {
	e := newRouteEnv(t)
	w := do(e.h, http.MethodGet, "/agents/codex/v1/models?llmgate_catalog=ok,,bad", chatAuth, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法选择状态 = %d", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "invalid_catalog_models" {
		t.Fatalf("错误 code = %q", code)
	}
	w = do(e.h, http.MethodGet, "/agents/codex/v1/models", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key 状态 = %d，期望 401", w.Code)
	}
}

func TestCodexLocalCatalogUsesModelCapabilityData(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatJSONReply(`{}`))
	upstreamID := dbUpstream(t, e.st, "deepseek", config.UpstreamDeepseek, "sk-not-real", stub.url)
	modelID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, modelID, upstreamID, "deepseek-v4-flash", 100)
	selectDevToolModels(t, e.st, modelID)

	w := do(e.h, http.MethodGet, "/agents/codex/v1/model-catalog", chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	var got struct {
		Models []struct {
			Slug          string `json:"slug"`
			DefaultEffort string `json:"default_reasoning_level"`
			Levels        []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("模型目录不是 JSON: %v", err)
	}
	if len(got.Models) != 1 || got.Models[0].Slug != "deepseek-v4-flash" ||
		got.Models[0].DefaultEffort != "high" || len(got.Models[0].Levels) != 2 ||
		got.Models[0].Levels[0].Effort != "high" || got.Models[0].Levels[1].Effort != "xhigh" {
		t.Fatalf("Codex effort 没有按能力数据生成：%+v", got.Models)
	}
}

// codexReplayReq 造一条 Codex 回放整段历史的请求体（Codex 恒 store:false）。
func codexReplayReq(input string) string {
	return fmt.Sprintf(`{"model":%q,"instructions":"You are a coding agent.","input":%s,"stream":false,"store":false}`,
		codexModel, input)
}

// sentInputItems 取第 i 次转发到订阅后端的请求体里的 input 数组。
func sentInputItems(t *testing.T, e *agentEnv, i int) []map[string]any {
	t.Helper()
	var sent struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(e.backend.sentBody(i)), &sent); err != nil {
		t.Fatalf("转发体不是 JSON：%v\n%s", err, e.backend.sentBody(i))
	}
	return sent.Input
}

func decodeInputItems(t *testing.T, input string) []map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatalf("用例 input 不是 JSON：%v", err)
	}
	return items
}

// TestCodexSubscriptionDropsCatalogReasoningItems：同一会话先由目录面作答、再切到
// 订阅模型时，Codex 回放的历史里带着目录面合成的 rs_resp_… reasoning item；订阅
// 代理转发前只摘掉它们，其余条目（带 msg_resp_/fc_resp_ id 的 message 与
// function_call、OpenAI 自己带 encrypted_content 的 reasoning）原样、原序过去。
func TestCodexSubscriptionDropsCatalogReasoningItems(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"第一轮"}]},
		{"id":"rs_resp_ac36008b147ad638_0","type":"reasoning","summary":[{"type":"summary_text","text":"目录模型的推理摘要"}]},
		{"id":"msg_resp_ac36008b147ad638_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"目录模型的回答"}]},
		{"id":"fc_resp_ac36008b147ad638_2","type":"function_call","status":"completed","call_id":"call_1","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"ok"},
		{"id":"rs_0123abcd","type":"reasoning","summary":[],"encrypted_content":"gAAAA-not-real"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"第二轮"}]}
	]`
	w := do(e.h, http.MethodPost, "/agents/codex/v1/responses", codexAuth, codexReplayReq(input))
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if e.backend.count() != 1 {
		t.Fatalf("订阅后端调用次数 = %d，期望 1", e.backend.count())
	}
	want := decodeInputItems(t, input)
	want = append(want[:1], want[2:]...) // 只有下标 1 那条目录面 reasoning 被摘掉
	if got := sentInputItems(t, e, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("转发到订阅后端的 input 不符：\n got = %v\nwant = %v", got, want)
	}
}

// TestCodexSubscriptionForwardsInputUntouched：没有目录面条目时 input 逐条原样
// 转发——OpenAI 自己的 reasoning item、带 encrypted_content 的 rs_resp_ 前缀条目
// （理论上不会出现，作为过滤器只认「设备自造且不可回放」的保险）与带 id 的
// message 一个都不动。
func TestCodexSubscriptionForwardsInputUntouched(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	input := `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"id":"rs_0123abcd","type":"reasoning","summary":[],"encrypted_content":"gAAAA-not-real"},
		{"id":"rs_resp_ffff_0","type":"reasoning","summary":[],"encrypted_content":"gAAAA-not-real"},
		{"id":"msg_0123","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]}
	]`
	w := do(e.h, http.MethodPost, "/agents/codex/v1/responses", codexAuth, codexReplayReq(input))
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if got, want := sentInputItems(t, e, 0), decodeInputItems(t, input); !reflect.DeepEqual(got, want) {
		t.Fatalf("没有目录面条目时 input 应原样转发：\n got = %v\nwant = %v", got, want)
	}
}
