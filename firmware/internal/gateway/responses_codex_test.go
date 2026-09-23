package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
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
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
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

func TestCodexSubscriptionRecoversInvalidToolImage(t *testing.T) {
	for _, endpoint := range []string{"/agents/codex/v1/responses", "/agents/v1/responses"} {
		t.Run(endpoint, func(t *testing.T) {
			e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
			input := `[
				{"type":"function_call","call_id":"call_image","name":"view_image","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_image","output":[
					{"type":"input_text","text":"media/original.png"},
					{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U=","detail":"high"},
					{"type":"input_image","image_url":"data:image/png;base64,[truncated]","detail":"high"}]},
				{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%%%"}]}
			]`
			w := do(e.h, http.MethodPost, endpoint, codexAuth, codexReplayReq(input))
			if w.Code != http.StatusOK || e.backend.count() != 1 {
				t.Fatalf("status = %d, upstream requests = %d", w.Code, e.backend.count())
			}
			got := sentInputItems(t, e, 0)
			want := decodeInputItems(t, input)
			parts := got[1]["output"].([]any)
			notice := parts[2].(map[string]any)
			if notice["type"] != "input_text" || !strings.Contains(notice["text"].(string), "Read the original image again") {
				t.Fatal("upstream received an invalid tool image instead of a reread notice")
			}
			want[1]["output"].([]any)[2] = notice
			if !reflect.DeepEqual(got, want) {
				t.Fatal("tool metadata, valid image or user attachment changed")
			}
		})
	}
}

// viewImageReplay 造一段 Agent 引擎回放的会话历史：n 次 view_image，每次工具结果带
// 一张约 size 字节的内联 JPEG（data URL，Base64 长度取 4 的整数倍，是有效编码）。
func viewImageReplay(n, size int) string {
	image := "data:image/jpeg;base64," + strings.Repeat("AAAA", (size-len("data:image/jpeg;base64,"))/4)
	var b strings.Builder
	b.WriteString(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"做一段 MV"}]}`)
	for i := range n {
		fmt.Fprintf(&b, `,{"type":"function_call","call_id":"call_%d","name":"view_image","arguments":"{}"}`, i)
		fmt.Fprintf(&b, `,{"type":"function_call_output","call_id":"call_%d","output":[{"type":"input_text","text":"media/s%02d-frame.png"},{"type":"input_image","image_url":%q,"detail":"high"}]}`, i, i, image)
	}
	b.WriteString("]")
	return b.String()
}

// TestAgentResponsesOmitEarlyToolImages：Agent 面的会话历史超过 8 MiB 转发上限时，
// 设备从最早的工具图片起换成省略说明、把请求收进上限再转发，最新的图片与其余条目
// 原样；标准 /v1/responses 协议面仍按 8 MiB 读体。
func TestAgentResponsesOmitEarlyToolImages(t *testing.T) {
	const images, imageSize, limit = 30, 330 << 10, 8 << 20
	input := viewImageReplay(images, imageSize)
	check := func(t *testing.T, sent []map[string]any, body string) {
		t.Helper()
		if len(body) > limit {
			t.Fatalf("forwarded body = %d bytes, want <= %d", len(body), limit)
		}
		want := decodeInputItems(t, input)
		if len(sent) != len(want) {
			t.Fatalf("forwarded %d items, want %d", len(sent), len(want))
		}
		omitted := 0
		for i, item := range sent {
			if item["type"] != "function_call_output" {
				if !reflect.DeepEqual(item, want[i]) {
					t.Fatalf("item %d changed", i)
				}
				continue
			}
			part := item["output"].([]any)[1].(map[string]any)
			if part["type"] == "input_text" {
				if !strings.Contains(part["text"].(string), "omitted from this request") || omitted != (i-1)/2 {
					t.Fatalf("item %d: unexpected replacement or omission out of order", i)
				}
				omitted++
				continue
			}
			if !reflect.DeepEqual(item, want[i]) {
				t.Fatalf("item %d: kept tool output changed", i)
			}
		}
		if omitted == 0 || omitted == images {
			t.Fatalf("omitted %d of %d images", omitted, images)
		}
	}
	for _, endpoint := range []string{"/agents/codex/v1/responses", "/agents/v1/responses"} {
		t.Run(endpoint, func(t *testing.T) {
			e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
			body := codexReplayReq(input)
			if len(body) <= limit {
				t.Fatalf("fixture body = %d bytes, want > %d", len(body), limit)
			}
			w := do(e.h, http.MethodPost, endpoint, codexAuth, body)
			if w.Code != http.StatusOK || e.backend.count() != 1 {
				t.Fatalf("status = %d, upstream requests = %d, body = %s", w.Code, e.backend.count(), w.Body.String())
			}
			check(t, sentInputItems(t, e, 0), e.backend.sentBody(0))
		})
	}
	t.Run("/agents/grok/v1/responses", func(t *testing.T) {
		e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
		body := fmt.Sprintf(`{"model":%q,"input":%s,"stream":false,"store":false}`, grokModel, input)
		w := do(e.h, http.MethodPost, "/agents/grok/v1/responses", grokClientHeaders, body)
		if w.Code != http.StatusOK || e.backend.count() != 1 {
			t.Fatalf("status = %d, upstream requests = %d, body = %s", w.Code, e.backend.count(), w.Body.String())
		}
		var sent struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal([]byte(e.backend.sentBody(0)), &sent); err != nil {
			t.Fatal(err)
		}
		check(t, sent.Input, e.backend.sentBody(0))
	})
	t.Run("standard /v1/responses keeps the 8 MiB read limit", func(t *testing.T) {
		e := newRouteEnv(t)
		w := do(e.h, http.MethodPost, "/v1/responses", chatAuth, codexReplayReq(input))
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})
	t.Run("read limit", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		w := do(e.h, http.MethodPost, "/agents/codex/v1/responses", codexAuth, codexReplayReq(viewImageReplay(100, imageSize)))
		if w.Code != http.StatusRequestEntityTooLarge || e.backend.count() != 0 {
			t.Fatalf("status = %d, upstream requests = %d", w.Code, e.backend.count())
		}
		if _, code, msg := decodeError(t, w); code != "request_too_large" || !strings.Contains(msg, "32 MB") {
			t.Fatalf("error = %s / %s", code, msg)
		}
	})
}
