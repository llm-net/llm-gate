package gateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// codexImageResult 是夹具里那张图的 base64（内容无所谓，只要能原样回到客户端）。
const codexImageResult = "aGVsbG8taW1hZ2U="

// codexImageStreamBody 是订阅后端对一次 image_generation 工具调用的 SSE 回放：
// 中间夹着工具的进行中事件与一个无关条目，最后一个 output_item.done 才是图。
var codexImageStreamBody = strings.Join([]string{
	"event: response.created",
	`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_img",` +
		`"object":"response","status":"in_progress","model":"` + codexBackendModel + `","output":[]}}`,
	"",
	"event: response.image_generation_call.in_progress",
	`data: {"type":"response.image_generation_call.in_progress","item_id":"ig_1","output_index":0}`,
	"",
	"event: response.output_item.done",
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
	"",
	"event: response.output_item.done",
	`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"image_generation_call",` +
		`"id":"ig_1","status":"completed","result":"` + codexImageResult + `","size":"1024x1024",` +
		`"quality":"low","background":"opaque","output_format":"png","revised_prompt":"a tiny red square"}}`,
	"",
	"event: response.completed",
	`data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_img",` +
		`"object":"response","status":"completed","model":"` + codexBackendModel + `","output":[],` +
		`"usage":{"input_tokens":50,"output_tokens":5,"total_tokens":55}}}`,
	"",
	"data: [DONE]",
	"",
}, "\n")

// codexImageFailedStreamBody 是后端拒绝画图的形态：只有 response.failed。
var codexImageFailedStreamBody = strings.Join([]string{
	"event: response.created",
	`data: {"type":"response.created","response":{"id":"resp_bad","status":"in_progress","model":"` + codexBackendModel + `","output":[]}}`,
	"",
	"event: response.failed",
	`data: {"type":"response.failed","response":{"id":"resp_bad","status":"failed","model":"` + codexBackendModel + `",` +
		`"error":{"code":"image_generation_user_error","message":"Your request was rejected by the safety system."}}}`,
	"",
	"data: [DONE]",
	"",
}, "\n")

const codexImagesPath = "/agents/codex/v1/images/generations"

// sentJSON 解析第 i 次上游请求体。
func sentJSON(t *testing.T, s *stubUpstream, i int) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s.sentBody(i)), &m); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v\n%s", err, s.sentBody(i))
	}
	return m
}

func TestCodexImagesGenerationsTranslatesToImageGenerationTool(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexImageStreamBody))
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	w := do(e.h, http.MethodPost, codexImagesPath, codexAuth,
		`{"prompt":"a tiny red square","model":"gpt-image-2-codex","quality":"low","size":"1024x1024","n":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q，期望 application/json", ct)
	}
	var got struct {
		Created int64 `json:"created"`
		Data    []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
		Size         string `json:"size"`
		Quality      string `json:"quality"`
		Background   string `json:"background"`
		OutputFormat string `json:"output_format"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("应答不是 JSON: %v\n%s", err, w.Body.String())
	}
	if got.Created == 0 || len(got.Data) != 1 || got.Data[0].B64 != codexImageResult {
		t.Fatalf("images 形态应答不对：%s", w.Body.String())
	}
	if got.Size != "1024x1024" || got.Quality != "low" || got.Background != "opaque" || got.OutputFormat != "png" {
		t.Fatalf("条目上的尺寸/质量没有带回：%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "revised_prompt") {
		t.Fatalf("不该把工具条目其余字段原样漏给客户端：%s", w.Body.String())
	}

	if e.backend.count() != 1 {
		t.Fatalf("订阅后端调用次数 = %d，期望 1", e.backend.count())
	}
	sent := sentJSON(t, e.backend, 0)
	if sent["model"] != codexModel {
		t.Fatalf("承载模型 = %v，期望订阅模型 %q（不是 gpt-image 名字）", sent["model"], codexModel)
	}
	if sent["stream"] != true || sent["store"] != false {
		t.Fatalf("stream/store 未钉死：%v", sent)
	}
	if tc, _ := sent["tool_choice"].(map[string]any); tc["type"] != "image_generation" {
		t.Fatalf("tool_choice = %v", sent["tool_choice"])
	}
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v，期望恰好一个 image_generation", sent["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "image_generation" || tool["model"] != "gpt-image-2-codex" ||
		tool["quality"] != "low" || tool["size"] != "1024x1024" || tool["n"] != float64(1) {
		t.Fatalf("工具对象没有逐字带上参数：%v", tool)
	}
	input, _ := sent["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", sent["input"])
	}
	msg := input[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if msg["role"] != "user" || len(content) != 1 {
		t.Fatalf("input 消息形态不对：%v", msg)
	}
	text := content[0].(map[string]any)
	if text["type"] != "input_text" || !strings.HasSuffix(text["text"].(string), "a tiny red square") {
		t.Fatalf("提示词没有进 input_text：%v", text)
	}
	// 订阅令牌与账号头照订阅代理的规矩注入。
	if e.backend.sentAuth(0) != "Bearer "+codexAccess1 {
		t.Fatalf("Authorization = %q", e.backend.sentAuth(0))
	}
	if sample := fm.only(t); sample.Entry != usage.EntryResponsesAgents || sample.ModelName != codexModel {
		t.Fatalf("画图应记在 Codex 订阅面：%+v", sample)
	}
}

func TestCodexImagesEditsCarryReferenceImages(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexImageStreamBody))
	w := do(e.h, http.MethodPost, "/agents/codex/v1/images/edits", codexAuth,
		`{"prompt":"make it blue","model":"gpt-image-2-codex","images":[{"image_url":"data:image/png;base64,QUJD"},{"image_url":""},"junk"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	sent := sentJSON(t, e.backend, 0)
	content := sent["input"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v，期望文本 + 一张参考图（空 URL 与非对象条目丢弃）", content)
	}
	img := content[1].(map[string]any)
	if img["type"] != "input_image" || img["image_url"] != "data:image/png;base64,QUJD" {
		t.Fatalf("参考图没有变成 input_image：%v", img)
	}
	tool := sent["tools"].([]any)[0].(map[string]any)
	if _, ok := tool["images"]; ok {
		t.Fatalf("images 不该进工具对象：%v", tool)
	}
}

// output_format 与 output_compression 逐字进工具对象；回给客户端的 output_format 取
// 后端回报值。
func TestCodexImagesForwardOutputFormat(t *testing.T) {
	e := newAgentEnv(t, sseReply(strings.Replace(codexImageStreamBody, `"output_format":"png"`, `"output_format":"jpeg"`, 1)))
	w := do(e.h, http.MethodPost, codexImagesPath, codexAuth,
		`{"prompt":"x","model":"gpt-image-2.5-flare","output_format":"jpeg","output_compression":20}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	tool := sentJSON(t, e.backend, 0)["tools"].([]any)[0].(map[string]any)
	if tool["model"] != "gpt-image-2.5-flare" || tool["output_format"] != "jpeg" || tool["output_compression"] != float64(20) {
		t.Fatalf("工具对象没有逐字带上输出格式：%v", tool)
	}
	var out struct {
		OutputFormat string `json:"output_format"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.OutputFormat != "jpeg" {
		t.Fatalf("响应 output_format = %q / %v", out.OutputFormat, err)
	}
}

// size 是 WxH 时化简成比例追加到提示词尾部（后端只认提示词里的比例）；auto 不追加。
func TestCodexImagesSizeBecomesAspectInstruction(t *testing.T) {
	for _, tc := range []struct{ size, want string }{
		{`"1536x1024"`, "x. Aspect ratio: 3:2."},
		{`"1024x1536"`, "x. Aspect ratio: 2:3."},
		{`"1024x1024"`, "x"},
		{`"auto"`, "x"},
		{`"bogus"`, "x"},
	} {
		e := newAgentEnv(t, sseReply(codexImageStreamBody))
		w := do(e.h, http.MethodPost, codexImagesPath, codexAuth, `{"prompt":"x","size":`+tc.size+`}`)
		if w.Code != http.StatusOK {
			t.Fatalf("size=%s 状态 = %d，body = %s", tc.size, w.Code, w.Body.String())
		}
		sent := sentJSON(t, e.backend, 0)
		text := sentText(sent["input"].([]any)[0])
		if !strings.HasSuffix(text, tc.want) {
			t.Fatalf("size=%s 提示词 = %q，期望以 %q 结尾", tc.size, text, tc.want)
		}
		if tc.size != `"bogus"` && tc.size != `"auto"` {
			if tool := sent["tools"].([]any)[0].(map[string]any); tool["size"] == nil {
				t.Fatalf("size 仍应逐字进工具对象：%v", tool)
			}
		}
	}
}

// background=transparent 不进工具对象（后端答 400），改成提示词尾部的透明背景指令；
// opaque 照旧逐字转发。
func TestCodexImagesTransparentBackgroundBecomesPromptHint(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexImageStreamBody))
	w := do(e.h, http.MethodPost, codexImagesPath, codexAuth, `{"prompt":"a logo","background":"transparent","size":"1536x1024"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	sent := sentJSON(t, e.backend, 0)
	tool := sent["tools"].([]any)[0].(map[string]any)
	if _, has := tool["background"]; has {
		t.Fatalf("transparent 不该进工具对象：%v", tool)
	}
	if text := sentText(sent["input"].([]any)[0]); !strings.HasSuffix(text, "a logo. Aspect ratio: 3:2. Transparent background: the background must be fully transparent (alpha channel), not white.") {
		t.Fatalf("提示词 = %q，期望带比例与透明背景指令", text)
	}
	e = newAgentEnv(t, sseReply(codexImageStreamBody))
	w = do(e.h, http.MethodPost, codexImagesPath, codexAuth, `{"prompt":"a logo","background":"opaque"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	sent = sentJSON(t, e.backend, 0)
	if tool := sent["tools"].([]any)[0].(map[string]any); tool["background"] != "opaque" {
		t.Fatalf("opaque 应逐字转发：%v", tool)
	}
	if text := sentText(sent["input"].([]any)[0]); !strings.HasSuffix(text, "a logo") {
		t.Fatalf("opaque 不该改提示词：%q", text)
	}
}

func TestCodexImagesIgnoresNonImageModelName(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexImageStreamBody))
	w := do(e.h, http.MethodPost, codexImagesPath, codexAuth, `{"prompt":"x","model":"dall-e-3"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	tool := sentJSON(t, e.backend, 0)["tools"].([]any)[0].(map[string]any)
	if _, ok := tool["model"]; ok {
		t.Fatalf("非 gpt-image 名字不该作为工具 model 转发：%v", tool)
	}
}

func TestCodexImagesFailedEventIs502(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexImageFailedStreamBody))
	w := do(e.h, http.MethodPost, codexImagesPath, codexAuth, `{"prompt":"x"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	typ, code, msg := decodeError(t, w)
	if typ != "api_error" || code != "image_generation_failed" ||
		msg != "Your request was rejected by the safety system." {
		t.Fatalf("错误体 = %s", w.Body.String())
	}
}

func TestCodexImagesNoImageItemIs502(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexStreamBody)) // 普通文本流，没有图像条目
	w := do(e.h, http.MethodPost, codexImagesPath, codexAuth, `{"prompt":"x"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if _, code, _ := decodeError(t, w); code != "image_generation_failed" {
		t.Fatalf("错误 code = %q", code)
	}
}

func TestCodexImagesRelaysUpstreamErrorVerbatim(t *testing.T) {
	const upstreamBody = `{"error":{"message":"image tools are disabled for this account","type":"invalid_request_error","code":null}}`
	e := newAgentEnv(t, jsonReply(http.StatusBadRequest, upstreamBody))
	w := do(e.h, http.MethodPost, codexImagesPath, codexAuth, `{"prompt":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if w.Body.String() != upstreamBody {
		t.Fatalf("错误体应原样透传：%s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestCodexImagesRejectsMissingPrompt(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexImageStreamBody))
	for _, body := range []string{`{"model":"gpt-image-2-codex"}`, `{"prompt":"   "}`, `{"prompt":3}`, `not json`} {
		w := do(e.h, http.MethodPost, codexImagesPath, codexAuth, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %s：状态 = %d，期望 400；%s", body, w.Code, w.Body.String())
		}
		if _, code, _ := decodeError(t, w); code != "invalid_request_body" {
			t.Fatalf("body %s：错误 code = %q", body, code)
		}
	}
	if e.backend.count() != 0 {
		t.Fatalf("坏请求不该打到订阅后端（%d 次）", e.backend.count())
	}
}

func TestCodexImagesRequireCodexSubscription(t *testing.T) {
	// 这把 Key 勾了一个目录模型（子树对它开放），但没勾 Codex 订阅。
	e := newRouteEnv(t)
	stub := newStub(t, chatJSONReply(`{}`))
	upstreamID := dbUpstream(t, e.st, "deepseek", config.UpstreamDeepseek, "sk-not-real", stub.url)
	modelID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, modelID, upstreamID, "deepseek-v4-flash", 100)
	keys, err := e.st.ListAPIKeys(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{
		KeyID: keys[0].ID, CatalogModelIDs: []int64{modelID},
	}); err != nil {
		t.Fatalf("ReplaceDevToolConfig: %v", err)
	}
	w := do(e.h, http.MethodPost, codexImagesPath, chatAuth, `{"prompt":"x"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if _, code, _ := decodeError(t, w); code != "subscription_not_allowed" {
		t.Fatalf("错误 code = %q", code)
	}
	if stub.count() != 0 {
		t.Fatalf("目录上游不该被画图门碰到（%d 次）", stub.count())
	}
}

func TestCodexImagesWithoutConnectedSubscriptionIs409(t *testing.T) {
	// 钉了 Codex 账号，但它被停用：没有任何可见的承载模型。
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	if err := e.st.SetAgentStatus(t.Context(), e.acctID, store.AgentStatusDisabled); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	w := do(e.h, http.MethodPost, codexImagesPath, chatAuth, `{"prompt":"x"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if _, code, _ := decodeError(t, w); code != "agent_not_configured" {
		t.Fatalf("错误 code = %q", code)
	}
}

func TestCodexImagesRoutesAndAuth(t *testing.T) {
	e := newAgentEnv(t, sseReply(codexImageStreamBody))
	if w := do(e.h, http.MethodPost, codexImagesPath, nil, `{"prompt":"x"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key 状态 = %d，期望 401", w.Code)
	}
	if w := do(e.h, http.MethodPost, "/agents/codex/v1/images/other", codexAuth, `{"prompt":"x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("未知画图路径状态 = %d，期望 404", w.Code)
	}
	if w := do(e.h, http.MethodGet, codexImagesPath, codexAuth, ""); w.Code != http.StatusNotFound {
		t.Fatalf("GET 画图门状态 = %d，期望 404", w.Code)
	}
	if w := do(e.h, http.MethodPost, codexImagesPath+"?llmgate_catalog=x", codexAuth, `{"prompt":"x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("旧 llmgate_catalog 参数状态 = %d，期望 400", w.Code)
	}
	w := do(e.h, http.MethodPost, "/codex/v1/images/generations", codexAuth, `{"prompt":"x"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("兼容别名状态 = %d，body = %s", w.Code, w.Body.String())
	}
	if e.backend.count() != 1 {
		t.Fatalf("只有别名那一次该到后端（%d 次）", e.backend.count())
	}
}

func TestCodexLocalCatalogCarriesSubscriptionOverlay(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	w := do(e.h, http.MethodGet, "/agents/codex/v1/model-catalog", chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body = %s", w.Code, w.Body.String())
	}
	var got struct {
		Models  []json.RawMessage `json:"models"`
		Overlay struct {
			Set      map[string]any `json:"set"`
			Default  map[string]any `json:"default"`
			ToolsAdd []string       `json:"experimental_supported_tools_add"`
		} `json:"subscription_overlay"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("模型目录不是 JSON: %v", err)
	}
	if len(got.Models) != 0 {
		t.Fatalf("没勾目录模型时 models 应为空：%s", w.Body.String())
	}
	if len(got.Overlay.Set) != 1 || got.Overlay.Set["use_responses_lite"] != false {
		t.Fatalf("set = %v，期望只有 use_responses_lite=false", got.Overlay.Set)
	}
	if len(got.Overlay.Default) != 2 || got.Overlay.Default["supports_reasoning_summaries"] != false ||
		got.Overlay.Default["supports_parallel_tool_calls"] != false {
		t.Fatalf("default = %v", got.Overlay.Default)
	}
	if len(got.Overlay.ToolsAdd) != 1 || got.Overlay.ToolsAdd[0] != "image_gen" {
		t.Fatalf("experimental_supported_tools_add = %v", got.Overlay.ToolsAdd)
	}
}
