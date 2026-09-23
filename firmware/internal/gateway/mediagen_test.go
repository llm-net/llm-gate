package gateway_test

// mediagen_test.go 是媒体生成订阅后端（grok / codex，mediagen.go）的可执行验收：各路径的
// 请求体形状、计量入账的入口与密钥归属、上游错误翻译、Refresh 的状态码分流，以及 Codex 的
// 承载模型、工具 model 白名单、比例与透明背景折成提示词、结果 MIME。后端经
// srv.MediaBackends() 按名字取，与生产装配注册进内核的是同一份对象。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// mediaBackend 按名字取网关交给内核的生成后端。
func mediaBackend(t *testing.T, srv *gateway.Server, name string) mediagen.Backend {
	t.Helper()
	for _, b := range srv.MediaBackends() {
		if b.Name() == name {
			return b
		}
	}
	t.Fatalf("网关没有交出后端 %q", name)
	return nil
}

// mediaPreset 取能力表里的订阅预设。
func mediaPreset(t *testing.T, id string) mediagen.Model {
	t.Helper()
	m, ok := mediagen.Preset(id)
	if !ok {
		t.Fatalf("能力表里没有预设 %q", id)
	}
	return m
}

// mediaCodexImage 以 Codex 后端画一张图，并返回结果与上游收到的请求体。defaultModel 是
// 账号默认模型（空 = 管理员没设）；owners 是订阅模型读数（nil = 目录读不到）。
func mediaCodexImage(t *testing.T, defaultModel string, owners map[string]string) (mediagen.Result, map[string]any, int) {
	t.Helper()
	return mediaCodexImageModel(t, mediaPreset(t, "gpt-image-2"), defaultModel, owners)
}

// mediaCodexImageModel 同上，但由调用方指定能力表项。
func mediaCodexImageModel(t *testing.T, model mediagen.Model, defaultModel string, owners map[string]string) (mediagen.Result, map[string]any, int) {
	t.Helper()
	e := newAgentEnv(t, sseReply(codexImageStreamBody))
	if err := e.st.UpdateAgentAccount(t.Context(), e.acctID, "订阅账号", defaultModel); err != nil {
		t.Fatalf("UpdateAgentAccount: %v", err)
	}
	e.srv.SetAgentModels(&fakeAgentModels{owners: owners})
	res, err := mediaBackend(t, e.srv, mediagen.BackendCodex).Generate(t.Context(), mediagen.Request{
		Model: model, Operation: store.MediaOpGenerate, Prompt: "一只小猫", AccountID: e.acctID,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if e.backend.count() == 0 {
		return res, nil, 0
	}
	return res, sentJSON(t, e.backend, 0), e.backend.count()
}

// 能力表的预设名 gpt-image-2 只是任务元数据：承载画图工具的 Responses model
// 必须是订阅文本模型，工具对象里也不带 model——ChatGPT 账号的 Codex 后端对
// 两处的 gpt-image 名字都答「model is not supported」。
func TestMediaCodexImageCarrierIsSubscriptionModel(t *testing.T) {
	codexOwners := map[string]string{codexModel: store.AgentProviderCodex}
	assertCarried := func(t *testing.T, res mediagen.Result, sent map[string]any, calls int, wantCarrier string) {
		t.Helper()
		if calls != 1 {
			t.Fatalf("上游调用次数 = %d，期望 1", calls)
		}
		if res.Status != "succeeded" || res.Error != "" {
			t.Fatalf("生成结果 = %q / %q，期望 succeeded", res.Status, res.Error)
		}
		if res.MediaType != "image" || res.AccountID == 0 {
			t.Fatalf("生成结果 = %+v，期望 image 且带订阅账号行", res)
		}
		if want := "data:image/png;base64," + codexImageResult; res.MediaURL != want {
			t.Fatalf("MediaURL = %q，期望 %q", res.MediaURL, want)
		}
		if sent["model"] != wantCarrier {
			t.Fatalf("承载模型 = %v，期望 %q", sent["model"], wantCarrier)
		}
		tools, _ := sent["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools = %v，期望恰一个画图工具", sent["tools"])
		}
		tool, _ := tools[0].(map[string]any)
		if tool["type"] != "image_generation" {
			t.Fatalf("工具 = %v，期望 image_generation", tool)
		}
		if _, has := tool["model"]; has {
			t.Fatalf("工具对象不该带 model：%v", tool)
		}
		choice, _ := sent["tool_choice"].(map[string]any)
		if choice["type"] != "image_generation" {
			t.Fatalf("tool_choice = %v，期望钉死画图工具", sent["tool_choice"])
		}
		body, _ := sent["input"].([]any)
		if len(body) != 1 || !strings.Contains(sentText(body[0]), "一只小猫") {
			t.Fatalf("input = %v，期望带提示词的一条用户消息", sent["input"])
		}
	}

	t.Run("账号没设默认模型", func(t *testing.T) {
		res, sent, calls := mediaCodexImage(t, "", codexOwners)
		assertCarried(t, res, sent, calls, codexModel)
	})
	t.Run("默认模型是订阅模型", func(t *testing.T) {
		res, sent, calls := mediaCodexImage(t, codexModel, codexOwners)
		assertCarried(t, res, sent, calls, codexModel)
	})
	t.Run("默认模型不是订阅模型", func(t *testing.T) {
		res, sent, calls := mediaCodexImage(t, "deepseek-chat", codexOwners)
		assertCarried(t, res, sent, calls, codexModel)
	})
	t.Run("多个订阅模型按平台目录顺序", func(t *testing.T) {
		agent, ok := platformcatalog.Builtin().Agent(store.AgentProviderCodex)
		if !ok || len(agent.Models) == 0 {
			t.Skip("内嵌平台目录没有 Codex 订阅模型")
		}
		catalogFirst := agent.Models[0].Name
		res, sent, calls := mediaCodexImage(t, "", map[string]string{
			"aaa-local-codex": store.AgentProviderCodex, // 字典序在前，但不在目录里
			catalogFirst:      store.AgentProviderCodex,
		})
		assertCarried(t, res, sent, calls, catalogFirst)
	})
	t.Run("订阅读数不可用时退回默认模型", func(t *testing.T) {
		res, sent, calls := mediaCodexImage(t, "gpt-5-custom", nil)
		assertCarried(t, res, sent, calls, "gpt-5-custom")
	})
	t.Run("既无订阅模型也无默认模型则不出门", func(t *testing.T) {
		res, _, calls := mediaCodexImage(t, "", nil)
		if calls != 0 {
			t.Fatalf("上游调用次数 = %d，期望 0（不能拿 gpt-image-2 当承载模型试）", calls)
		}
		if res.Status != "failed" || !strings.Contains(res.Error, "没有可用的文本模型") {
			t.Fatalf("生成结果 = %q / %q，期望 failed 并说明缺承载模型", res.Status, res.Error)
		}
	})
}

// GPT Image 2.5 的两个预设原样写进画图工具的 model；承载模型仍是订阅文本模型，
// 其他名字（含 gpt-image-2）不带工具 model。
func TestMediaCodexImage25PresetsForwardToolModel(t *testing.T) {
	codexOwners := map[string]string{codexModel: store.AgentProviderCodex}
	for _, preset := range []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"} {
		t.Run(preset, func(t *testing.T) {
			res, sent, calls := mediaCodexImageModel(t, mediaPreset(t, preset), "", codexOwners)
			if calls != 1 || res.Status != "succeeded" {
				t.Fatalf("calls=%d res=%+v", calls, res)
			}
			if sent["model"] != codexModel {
				t.Fatalf("承载模型 = %v，期望 %q", sent["model"], codexModel)
			}
			tools, _ := sent["tools"].([]any)
			tool, _ := tools[0].(map[string]any)
			if tool["type"] != "image_generation" || tool["model"] != preset {
				t.Fatalf("工具 = %v，期望 model=%q", tool, preset)
			}
		})
	}
	t.Run("未列入白名单的名字不带工具 model", func(t *testing.T) {
		nope := mediaPreset(t, "gpt-image-2")
		nope.ID = "gpt-image-2.5-nope"
		_, sent, _ := mediaCodexImageModel(t, nope, "", codexOwners)
		tools, _ := sent["tools"].([]any)
		tool, _ := tools[0].(map[string]any)
		if _, has := tool["model"]; has {
			t.Fatalf("工具对象不该带 model：%v", tool)
		}
	})
}

// Codex 图像生成的参考图与输出格式：参考图各成一条 input_image 跟在提示词后，
// output_format 进工具对象；结果 data URI 的 MIME 按后端回报的 output_format。
// 尺寸、质量与背景不进工具对象（订阅后端不认，见 docs-dev/gpt-image-25-model-selection.md）。
func TestMediaCodexImageCarriesReferenceImagesAndOutputFormat(t *testing.T) {
	codexOwners := map[string]string{codexModel: store.AgentProviderCodex}
	stream := strings.Replace(codexImageStreamBody, `"output_format":"png"`, `"output_format":"webp"`, 1)
	e := newAgentEnv(t, sseReply(stream))
	e.srv.SetAgentModels(&fakeAgentModels{owners: codexOwners})
	// quality 不在能力表里（受理校验会拒）；这里绕过内核直接塞给后端，钉住它即便收到也不外发。
	res, err := mediaBackend(t, e.srv, mediagen.BackendCodex).Generate(t.Context(), mediagen.Request{
		Model: mediaPreset(t, "gpt-image-2.5-flare"), Operation: store.MediaOpGenerate, Prompt: "一只小猫",
		Inputs: mediagen.Inputs{ReferenceImages: []string{"data:image/png;base64,QUJD", "https://example.invalid/b.png"}},
		Params: mediagen.Params{"output_format": "webp", "aspect_ratio": "16:9", "quality": "low", "background": "transparent"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != "succeeded" || res.MediaType != "image" {
		t.Fatalf("生成结果 = %+v", res)
	}
	if want := "data:image/webp;base64," + codexImageResult; res.MediaURL != want {
		t.Fatalf("MediaURL = %q，期望按后端回报的格式 %q", res.MediaURL, want)
	}
	sent := sentJSON(t, e.backend, 0)
	tool, _ := sent["tools"].([]any)[0].(map[string]any)
	if tool["model"] != "gpt-image-2.5-flare" || tool["output_format"] != "webp" {
		t.Fatalf("工具对象 = %v，期望带 model 与 output_format", tool)
	}
	for _, k := range []string{"size", "quality", "background", "aspect_ratio", "resolution"} {
		if _, has := tool[k]; has {
			t.Fatalf("工具对象不该带 %s：%v", k, tool)
		}
	}
	body, _ := sent["input"].([]any)
	msg, _ := body[0].(map[string]any)
	content := jsonArrayAny(msg["content"])
	if len(content) != 3 || !strings.Contains(sentText(body[0]), "一只小猫") {
		t.Fatalf("input = %v，期望提示词后跟两张 input_image", sent["input"])
	}
	if text := sentText(body[0]); !strings.HasSuffix(text, "一只小猫. Aspect ratio: 16:9. Transparent background: the background must be fully transparent (alpha channel), not white.") {
		t.Fatalf("提示词尾部应带比例与透明背景指令：%q", text)
	}
	for i, want := range []string{"data:image/png;base64,QUJD", "https://example.invalid/b.png"} {
		img, _ := content[i+1].(map[string]any)
		if img["type"] != "input_image" || img["image_url"] != want {
			t.Fatalf("第 %d 张参考图 = %v", i+1, img)
		}
	}
	if raw, _ := json.Marshal(sent); strings.Contains(string(raw), `"images"`) {
		t.Fatalf("images 不该出现在 Responses 请求里: %s", raw)
	}
}

// 后端没回报 output_format 时按请求的格式；请求也没带时按 PNG。
func TestMediaCodexImageMIMEFallsBackToRequestedFormat(t *testing.T) {
	codexOwners := map[string]string{codexModel: store.AgentProviderCodex}
	stream := strings.Replace(codexImageStreamBody, `"output_format":"png",`, ``, 1)
	for _, tc := range []struct{ format, want string }{{"jpeg", "image/jpeg"}, {"", "image/png"}} {
		e := newAgentEnv(t, sseReply(stream))
		e.srv.SetAgentModels(&fakeAgentModels{owners: codexOwners})
		params := mediagen.Params{}
		if tc.format != "" {
			params["output_format"] = tc.format
		}
		res, err := mediaBackend(t, e.srv, mediagen.BackendCodex).Generate(t.Context(), mediagen.Request{
			Model: mediaPreset(t, "gpt-image-2"), Operation: store.MediaOpGenerate, Prompt: "p", Params: params,
		})
		if err != nil || !strings.HasPrefix(res.MediaURL, "data:"+tc.want+";base64,") {
			t.Fatalf("format=%q: MediaURL = %q / %v，期望 %s", tc.format, res.MediaURL, err, tc.want)
		}
	}
}

// Codex 的搭配约束在后端的 Validate 钩子里：透明背景只配 PNG（WebP 回报透明但无 alpha，
// JPEG 没有 alpha）；没指定格式或指定 PNG 放行。能力表表达不了这一条。
func TestMediaCodexValidateTransparentNeedsPNG(t *testing.T) {
	e := newRouteEnv(t)
	b := mediaBackend(t, e.srv, mediagen.BackendCodex)
	for _, tc := range []struct {
		name   string
		params mediagen.Params
		ok     bool
	}{
		{"透明 + 未指定格式", mediagen.Params{"background": "transparent"}, true},
		{"透明 + PNG", mediagen.Params{"background": "transparent", "output_format": "png"}, true},
		{"透明 + WebP", mediagen.Params{"background": "transparent", "output_format": "webp"}, false},
		{"透明 + JPEG", mediagen.Params{"background": "transparent", "output_format": "jpeg"}, false},
		{"不透明 + JPEG", mediagen.Params{"output_format": "jpeg"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := b.Validate(mediagen.Request{Model: mediaPreset(t, "gpt-image-2"), Operation: store.MediaOpGenerate, Prompt: "p", Params: tc.params})
			if tc.ok {
				if err != nil {
					t.Fatalf("Validate = %v，期望放行", err)
				}
				return
			}
			var invalid *mediagen.InvalidError
			if !errors.As(err, &invalid) || !strings.Contains(invalid.Msg, "PNG") {
				t.Fatalf("Validate = %v，期望 *InvalidError 且说明只支持 PNG", err)
			}
		})
	}
	// Grok 的搭配能力表都表达得了，后端不补判。
	if err := mediaBackend(t, e.srv, mediagen.BackendGrok).Validate(mediagen.Request{Model: mediaPreset(t, imagineImageModel)}); err != nil {
		t.Fatalf("Grok Validate = %v，期望恒放行", err)
	}
}

// 生成以任务归属的那把密钥入账，与数据面同名的门同一套入口与模型维度：
// Codex 走订阅 Responses（模型维度是承载的订阅文本模型，图像预设名只是任务元数据），
// Grok 画图走 imagine_image 并数张数，Grok 视频提交记一次。没有归属密钥的历史任务
// 不入账——账本的密钥维度不收一个匹配不到任何行的 0。
func TestMediaGenerationMetersUnderOwningKey(t *testing.T) {
	const keyID = 42
	const keyDisplay = "sk_preview1…9999"
	t.Run("Codex 订阅 Responses", func(t *testing.T) {
		e := newAgentEnv(t, sseReply(codexImageStreamBody))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		e.srv.SetAgentModels(&fakeAgentModels{owners: map[string]string{codexModel: store.AgentProviderCodex}})
		if _, err := mediaBackend(t, e.srv, mediagen.BackendCodex).Generate(t.Context(), mediagen.Request{
			Model: mediaPreset(t, "gpt-image-2.5-flare"), Operation: store.MediaOpGenerate,
			Prompt: "p", KeyID: keyID, KeyDisplay: keyDisplay,
		}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		sample := fm.only(t)
		if sample.Entry != usage.EntryResponsesAgents || sample.KeyID != keyID || sample.KeyDisplay != keyDisplay {
			t.Fatalf("记账归属 = %+v", sample)
		}
		if sample.ModelName != codexModel || sample.Status != http.StatusOK {
			t.Fatalf("记账维度 = %q / %d，期望承载模型与 200", sample.ModelName, sample.Status)
		}
		if sample.Tokens.Prompt != 50 || sample.Tokens.Completion != 5 || sample.Estimated {
			t.Fatalf("记账用量 = %+v，期望上游真值", sample.Tokens)
		}
	})
	t.Run("Grok 画图", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		if _, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
			Model: mediaPreset(t, imagineImageModel), Operation: store.MediaOpGenerate,
			Prompt: "p", KeyID: keyID, KeyDisplay: keyDisplay,
		}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		sample := fm.only(t)
		if sample.Entry != usage.EntryImagineImage || sample.KeyID != keyID || sample.KeyDisplay != keyDisplay || sample.ModelName != imagineImageModel {
			t.Fatalf("记账 = %+v", sample)
		}
		if sample.TaskUsage.GeneratedImages != 2 {
			t.Fatalf("出图张数 = %d，期望按响应 data[] 数出 2", sample.TaskUsage.GeneratedImages)
		}
	})
	t.Run("Grok 视频提交", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
			Model: mediaPreset(t, imagineVideoModel), Operation: store.MediaOpGenerate,
			Prompt: "walk", KeyID: keyID, KeyDisplay: keyDisplay,
		})
		if err != nil || res.Status != store.MediaStatusQueued {
			t.Fatalf("Generate = %+v / %v", res, err)
		}
		if sample := fm.only(t); sample.Entry != usage.EntryImagineVideo || sample.KeyID != keyID || sample.ModelName != imagineVideoModel {
			t.Fatalf("记账 = %+v", sample)
		}
	})
	t.Run("上游拒绝的那一笔照记", func(t *testing.T) {
		e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}, issuerNever(t))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
			Model: mediaPreset(t, imagineImageModel), Operation: store.MediaOpGenerate,
			Prompt: "p", KeyID: keyID, KeyDisplay: keyDisplay,
		})
		if err != nil || res.Status != store.MediaStatusFailed {
			t.Fatalf("Generate = %+v / %v", res, err)
		}
		if sample := fm.only(t); sample.Entry != usage.EntryImagineImage || sample.KeyID != keyID || sample.Status != http.StatusUnprocessableEntity {
			t.Fatalf("记账 = %+v，期望记下这次调用的真实状态", sample)
		}
	})
	t.Run("无归属密钥不入账", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		if _, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
			Model: mediaPreset(t, imagineImageModel), Operation: store.MediaOpGenerate, Prompt: "p",
		}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if fm.count() != 0 {
			t.Fatalf("记账笔数 = %d，期望 0", fm.count())
		}
	})
	t.Run("查询平台任务不计量", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Refresh(t.Context(), store.MediaJob{
			ID: "01JOB", Backend: mediagen.BackendGrok, Provider: store.AgentProviderGrok, Kind: store.ModelKindVideo,
			Model: imagineVideoModel, AccountID: e.acctID, KeyID: keyID, KeyDisplay: keyDisplay, VendorID: "req_1", Status: store.MediaStatusQueued,
		})
		if err != nil || res.Status != store.MediaStatusSucceeded {
			t.Fatalf("Refresh = %+v / %v", res, err)
		}
		if fm.count() != 0 {
			t.Fatalf("记账笔数 = %d，期望 0（查询不计模型消费）", fm.count())
		}
	})
}

// sentText 拼出一条 Responses 输入消息里的全部 input_text。
func sentText(item any) string {
	msg, _ := item.(map[string]any)
	var b strings.Builder
	for _, part := range jsonArrayAny(msg["content"]) {
		p, _ := part.(map[string]any)
		if p["type"] == "input_text" {
			b.WriteString(jsonStringAny(p["text"]))
		}
	}
	return b.String()
}

func jsonArrayAny(v any) []any { a, _ := v.([]any); return a }

func jsonStringAny(v any) string { s, _ := v.(string); return s }

// 上游拒绝时任务记 failed，错误文案取上游 message（带 code）。
func TestMediaCodexImageUpstreamRejection(t *testing.T) {
	e := newAgentEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"The 'x' model is not supported when using Codex with a ChatGPT account.","code":"unsupported_model"}}`))
	})
	res, err := mediaBackend(t, e.srv, mediagen.BackendCodex).Generate(t.Context(), mediagen.Request{
		Model: mediaPreset(t, "gpt-image-2"), Operation: store.MediaOpGenerate, Prompt: "一只小猫",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "not supported") || !strings.Contains(res.Error, "unsupported_model") {
		t.Fatalf("生成结果 = %q / %q，期望 failed 且透出上游文案与 code", res.Status, res.Error)
	}
	// §15.1：平台错误正文只进任务行，不进日志。
	if strings.Contains(e.logBuf.String(), "not supported") {
		t.Fatalf("日志不该带平台错误正文: %s", e.logBuf.String())
	}
}

func TestMediaGrokVideoUpstreamRejection(t *testing.T) {
	e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"video extension is unavailable for this model","code":"failed_precondition"}`))
	}, issuerNever(t))
	res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
		Model: mediaPreset(t, mediagen.GrokVideoEditModel), Operation: store.MediaOpExtend, Prompt: "续写",
		Inputs: mediagen.Inputs{SourceVideo: "data:video/mp4;base64,AAAAIGZ0eXBpc29t"},
		Params: mediagen.Params{"duration": int64(6)},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "video extension") || !strings.Contains(res.Error, "failed_precondition") {
		t.Fatalf("生成结果 = %q / %q，期望 failed 且透出上游文案与 code", res.Status, res.Error)
	}
}

// xAI Imagine 的错误体是 {"code","error":"<文案>"}（error 是字符串而不是对象），任务的失败
// 原因要透出这段文案与 code，而不是退成笼统的「平台图像生成失败」。
func TestMediaGrokImageUpstreamRejectionXAIShape(t *testing.T) {
	e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_image","error":"Downloaded response does not contain a valid JPG, PNG, WebP, or ICO image."}`))
	}, issuerNever(t))
	res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
		Model: mediaPreset(t, imagineImageModel), Operation: store.MediaOpGenerate, Prompt: "改成水彩",
		Inputs: mediagen.Inputs{ReferenceImages: []string{"data:image/png;base64,iVBORw0KGgo="}},
		Params: mediagen.Params{"aspect_ratio": "3:4", "resolution": "2k"},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != "failed" || !strings.Contains(res.Error, "valid JPG, PNG, WebP") || !strings.Contains(res.Error, "invalid_image") {
		t.Fatalf("生成结果 = %q / %q，期望 failed 且透出 xAI 文案与 code", res.Status, res.Error)
	}
}

// 不是 JSON 的短文本错误体（xAI 对枚举越界答 422 纯文本）按原文透出；空体退成缺省文案并带
// HTTP 状态。
func TestMediaGrokImageUpstreamRejectionPlainText(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"纯文本", "Failed to deserialize the JSON body into the target type: aspect_ratio: unknown variant `7:5`", []string{"unknown variant"}},
		{"空体", "", []string{"平台图像生成失败", "HTTP 422"}},
		{"HTML 体", "<html><body>Bad Gateway</body></html>", []string{"平台图像生成失败", "HTTP 422"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(tc.body))
			}, issuerNever(t))
			res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
				Model: mediaPreset(t, imagineImageModel), Operation: store.MediaOpGenerate, Prompt: "一只小猫",
			})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if res.Status != "failed" {
				t.Fatalf("任务状态 = %q，期望 failed", res.Status)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Error, w) {
					t.Fatalf("失败原因 = %q，缺少 %q", res.Error, w)
				}
			}
		})
	}
}

// 平台答 200 却没给图 / 没给任务 ID：图像返回 error（内核判失败），视频判 failed。
func TestMediaGrokEmptySuccessBodies(t *testing.T) {
	e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}, issuerNever(t))
	b := mediaBackend(t, e.srv, mediagen.BackendGrok)
	if _, err := b.Generate(t.Context(), mediagen.Request{Model: mediaPreset(t, imagineImageModel), Operation: store.MediaOpGenerate, Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "未返回图像") {
		t.Fatalf("图像空结果 err = %v，期望「平台未返回图像」", err)
	}
	res, err := b.Generate(t.Context(), mediagen.Request{Model: mediaPreset(t, imagineVideoModel), Operation: store.MediaOpGenerate, Prompt: "p"})
	if err != nil || res.Status != store.MediaStatusFailed || !strings.Contains(res.Error, "任务 ID") {
		t.Fatalf("视频无任务 ID = %+v / %v，期望 failed", res, err)
	}
}

// 向平台查询视频任务答非 2xx 时按状态码分路：400 带 xAI 错误体是平台对任务的终审
// （内容审核拒绝）→ failed 并透出文案与 code；404 → expired；429 / 5xx / 设备自产 409
// 是查询侧故障 → 返回 error，任务保持排队由下一轮再查。
func TestMediaGrokRefreshUpstreamStatuses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		body       string
		wantStatus string
		wantErr    bool
		wantText   []string
	}{
		{"内容审核拒绝", http.StatusBadRequest, `{"code":"imagine:content-moderated","error":"Generated video rejected by content moderation."}`, "failed", false, []string{"content moderation", "imagine:content-moderated"}},
		{"任务不存在", http.StatusNotFound, `{"code":"not_found","error":"request not found"}`, "expired", false, []string{"已过期", "HTTP 404"}},
		{"任务已删除", http.StatusGone, ``, "expired", false, []string{"已过期", "HTTP 410"}},
		{"限流", http.StatusTooManyRequests, `{"code":"rate_limited","error":"slow down"}`, "", true, nil},
		{"平台故障", http.StatusServiceUnavailable, ``, "", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/videos/") {
					t.Errorf("意外的上游调用 %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}, issuerNever(t))
			res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Refresh(t.Context(), store.MediaJob{
				ID: "01TASK", Backend: mediagen.BackendGrok, Provider: store.AgentProviderGrok, Kind: store.ModelKindVideo, Model: imagineVideoModel,
				AccountID: e.acctID, VendorID: "vendor-1", Status: store.MediaStatusQueued,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望返回 error 让下一轮再查，实得 %+v", res)
				}
				if !strings.Contains(err.Error(), strconv.Itoa(tc.code)) {
					t.Fatalf("error = %v，期望带 HTTP 状态", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if res.Status != tc.wantStatus || res.VendorID != "vendor-1" || res.AccountID != e.acctID {
				t.Fatalf("结果 = %+v，期望 status %q 并带任务归属", res, tc.wantStatus)
			}
			for _, w := range tc.wantText {
				if !strings.Contains(res.Error, w) {
					t.Fatalf("失败原因 = %q，缺少 %q", res.Error, w)
				}
			}
			// §15.1：告警只记状态码，不记平台错误正文。
			if strings.Contains(e.logBuf.String(), "content moderation") {
				t.Fatalf("日志不该带平台错误正文: %s", e.logBuf.String())
			}
		})
	}
}

// 查询答 2xx 时平台状态词只折成三种：有结果即 succeeded，明确失败词为 failed（带平台
// code / message），其余中间状态一律 queued 由内核继续查。图像任务与没有平台任务 ID 的任务
// 不支持刷新；Codex 后端恒不支持。
func TestMediaGrokRefreshFoldsPlatformStates(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantStatus, wantURL string
		wantText                        []string
	}{
		{"已完成", imaginePollBody, store.MediaStatusSucceeded, "https://videos.x.ai/req_1.mp4", nil},
		{"处理中", `{"status":"pending"}`, store.MediaStatusQueued, "", nil},
		{"状态词缺失", `{}`, store.MediaStatusQueued, "", nil},
		{"平台判失败", `{"status":"failed","error":{"code":"internal","message":"render crashed"}}`, store.MediaStatusFailed, "", []string{"render crashed", "internal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/videos/req_1") {
					t.Errorf("意外的上游调用 %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}, issuerNever(t))
			res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Refresh(t.Context(), store.MediaJob{
				ID: "01TASK", Backend: mediagen.BackendGrok, Kind: store.ModelKindVideo, Model: imagineVideoModel,
				AccountID: e.acctID, VendorID: "req_1", Status: store.MediaStatusQueued,
			})
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if res.Status != tc.wantStatus || res.MediaURL != tc.wantURL || res.MediaType != "video" || res.VendorID != "req_1" {
				t.Fatalf("结果 = %+v，期望 %q / %q", res, tc.wantStatus, tc.wantURL)
			}
			for _, w := range tc.wantText {
				if !strings.Contains(res.Error, w) {
					t.Fatalf("失败原因 = %q，缺少 %q", res.Error, w)
				}
			}
		})
	}
	t.Run("不支持刷新的任务", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		grok := mediaBackend(t, e.srv, mediagen.BackendGrok)
		if _, err := grok.Refresh(t.Context(), store.MediaJob{ID: "01IMG", Kind: store.ModelKindImage, VendorID: "x", AccountID: e.acctID}); err == nil {
			t.Fatal("图像任务不该支持刷新")
		}
		if _, err := grok.Refresh(t.Context(), store.MediaJob{ID: "01VID", Kind: store.ModelKindVideo, AccountID: e.acctID}); err == nil {
			t.Fatal("没有平台任务 ID 的任务不该支持刷新")
		}
		if _, err := mediaBackend(t, e.srv, mediagen.BackendCodex).Refresh(t.Context(), store.MediaJob{ID: "01CDX", Kind: store.ModelKindImage, VendorID: "x"}); err == nil {
			t.Fatal("Codex 任务不该支持刷新")
		}
		if e.backend.count() != 0 {
			t.Fatalf("不支持刷新的任务不该打到平台：%d 次", e.backend.count())
		}
	})
}

// 钉死的订阅账号必须是这种订阅且仍可用：账号行对不上时后端返回 error、一个字节不出门。
func TestMediaBackendRequiresUsablePinnedAccount(t *testing.T) {
	e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
	_, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), mediagen.Request{
		Model: mediaPreset(t, imagineImageModel), Operation: store.MediaOpGenerate, Prompt: "p", AccountID: e.acctID + 100,
	})
	if err == nil || e.backend.count() != 0 {
		t.Fatalf("err = %v，上游调用 %d 次；期望报错且不出门", err, e.backend.count())
	}
	// 设备上没有 Codex 订阅：Codex 后端同样报错。
	if _, err := mediaBackend(t, e.srv, mediagen.BackendCodex).Generate(t.Context(), mediagen.Request{
		Model: mediaPreset(t, "gpt-image-2"), Operation: store.MediaOpGenerate, Prompt: "p",
	}); err == nil {
		t.Fatal("没有 Codex 订阅时应报错")
	}
}

// Grok 媒体输入按官方形状进请求体：视频的首帧 / 尾帧 / 参考图分别是
// videos/generations 的 image / last_frame / reference_images（各是 {url} 对象）；
// 图像带一张参考图走 images/edits 的 image，多张走 images，没有参考图仍是
// images/generations。
func TestMediaGrokInputsShapeUpstreamBody(t *testing.T) {
	const dataURI = "data:image/png;base64,iVBORw0KGgo="
	run := func(t *testing.T, in mediagen.Request) (mediagen.Result, map[string]any, string) {
		t.Helper()
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		in.Operation = store.MediaOpGenerate
		res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), in)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if e.backend.count() != 1 {
			t.Fatalf("上游调用次数 = %d，期望 1", e.backend.count())
		}
		return res, sentJSON(t, e.backend, 0), e.call(0)
	}
	urlOf := func(v any) string {
		m, _ := v.(map[string]any)
		return jsonStringAny(m["url"])
	}
	// typeOf 取 images/edits 官方对象形状里的 type（恒 image_url）。
	typeOf := func(v any) string {
		m, _ := v.(map[string]any)
		return jsonStringAny(m["type"])
	}
	video, image := mediaPreset(t, imagineVideoModel), mediaPreset(t, imagineImageModel)
	t.Run("视频：首帧 / 尾帧 / 参考图", func(t *testing.T) {
		res, sent, call := run(t, mediagen.Request{Model: video, Prompt: "walk",
			Inputs: mediagen.Inputs{FirstFrame: dataURI, LastFrame: "https://example.invalid/last.png", ReferenceImages: []string{"https://example.invalid/a.png", dataURI}}})
		if call != "POST /v1/videos/generations" {
			t.Fatalf("上游调用 = %q", call)
		}
		if res.Status != "queued" || res.VendorID != "req_1" || res.MediaType != "video" {
			t.Fatalf("结果 = %+v，期望 queued 且带平台任务 ID", res)
		}
		if sent["model"] != imagineVideoModel || sent["prompt"] != "walk" {
			t.Fatalf("model / prompt = %v / %v", sent["model"], sent["prompt"])
		}
		if urlOf(sent["image"]) != dataURI || urlOf(sent["last_frame"]) != "https://example.invalid/last.png" {
			t.Fatalf("image / last_frame = %v / %v", sent["image"], sent["last_frame"])
		}
		refs := jsonArrayAny(sent["reference_images"])
		if len(refs) != 2 || urlOf(refs[0]) != "https://example.invalid/a.png" || urlOf(refs[1]) != dataURI {
			t.Fatalf("reference_images = %v", sent["reference_images"])
		}
		// 角色名是设备自己的词汇，不进平台请求体。
		for _, k := range []string{"first_frame", "inputs", "source_video"} {
			if _, ok := sent[k]; ok {
				t.Fatalf("请求体不该带 %s: %v", k, sent[k])
			}
		}
	})
	t.Run("视频：文生不带媒体字段", func(t *testing.T) {
		_, sent, _ := run(t, mediagen.Request{Model: video, Prompt: "walk"})
		for _, k := range []string{"image", "last_frame", "reference_images"} {
			if _, ok := sent[k]; ok {
				t.Fatalf("文生视频不该带 %s: %v", k, sent[k])
			}
		}
	})
	t.Run("图像：一张参考图走 edits 的 image", func(t *testing.T) {
		res, sent, call := run(t, mediagen.Request{Model: image, Prompt: "blue",
			Inputs: mediagen.Inputs{ReferenceImages: []string{dataURI}}})
		if call != "POST /v1/images/edits" || urlOf(sent["image"]) != dataURI || typeOf(sent["image"]) != "image_url" || sent["images"] != nil {
			t.Fatalf("上游调用 = %q，body = %v", call, sent)
		}
		if res.Status != "succeeded" || res.MediaURL != "https://imgen.x.ai/one.png" || res.MediaType != "image" {
			t.Fatalf("结果 = %+v", res)
		}
	})
	t.Run("图像：多张参考图走 edits 的 images", func(t *testing.T) {
		_, sent, call := run(t, mediagen.Request{Model: image, Prompt: "blue",
			Inputs: mediagen.Inputs{ReferenceImages: []string{dataURI, "https://example.invalid/b.png"}}})
		imgs := jsonArrayAny(sent["images"])
		if call != "POST /v1/images/edits" || len(imgs) != 2 || urlOf(imgs[1]) != "https://example.invalid/b.png" || typeOf(imgs[0]) != "image_url" || typeOf(imgs[1]) != "image_url" || sent["image"] != nil {
			t.Fatalf("上游调用 = %q，body = %v", call, sent)
		}
	})
	t.Run("图像：没有参考图仍是 generations", func(t *testing.T) {
		_, sent, call := run(t, mediagen.Request{Model: image, Prompt: "blue"})
		if call != "POST /v1/images/generations" || sent["image"] != nil || sent["images"] != nil {
			t.Fatalf("上游调用 = %q，body = %v", call, sent)
		}
	})
}

// Grok 生成参数按官方字段进请求体：视频生成带 duration / aspect_ratio /
// resolution / generate_audio 与 reference_audios 的 {voice_id}，带首帧不带提示词时不带
// prompt；编辑走 videos/edits 只带 video，延长走 videos/extensions 带 video 与 duration；
// 图像带 aspect_ratio / resolution，quality 只在 images/generations。没带的参数一律不带字段。
func TestMediaGrokParamsShapeUpstreamBody(t *testing.T) {
	const dataURI = "data:image/png;base64,iVBORw0KGgo="
	const mp4URI = "data:video/mp4;base64,AAAAIGZ0eXBpc29t"
	run := func(t *testing.T, in mediagen.Request) (mediagen.Result, map[string]any, string) {
		t.Helper()
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		if in.Operation == "" {
			in.Operation = store.MediaOpGenerate
		}
		res, err := mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), in)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		return res, sentJSON(t, e.backend, 0), e.call(0)
	}
	video, classic, image := mediaPreset(t, imagineVideoModel), mediaPreset(t, mediagen.GrokVideoEditModel), mediaPreset(t, imagineImageModel)
	t.Run("视频生成：全部参数", func(t *testing.T) {
		_, sent, call := run(t, mediagen.Request{Model: video, Prompt: "walk",
			Inputs: mediagen.Inputs{FirstFrame: dataURI},
			Params: mediagen.Params{"duration": int64(12), "aspect_ratio": "9:16", "resolution": "1080p", "generate_audio": false, "voices": []string{"eve", "leo"}}})
		if call != "POST /v1/videos/generations" {
			t.Fatalf("上游调用 = %q", call)
		}
		if sent["duration"] != float64(12) || sent["aspect_ratio"] != "9:16" || sent["resolution"] != "1080p" || sent["generate_audio"] != false {
			t.Fatalf("参数 = %v", sent)
		}
		audios := jsonArrayAny(sent["reference_audios"])
		if len(audios) != 2 || jsonStringAny(audios[0].(map[string]any)["voice_id"]) != "eve" || jsonStringAny(audios[1].(map[string]any)["voice_id"]) != "leo" {
			t.Fatalf("reference_audios = %v", sent["reference_audios"])
		}
		for _, k := range []string{"video", "quality", "operation", "voices", "params"} {
			if _, ok := sent[k]; ok {
				t.Fatalf("不该带 %s: %v", k, sent[k])
			}
		}
	})
	t.Run("视频生成：只带首尾帧、无提示词、无参数", func(t *testing.T) {
		res, sent, _ := run(t, mediagen.Request{Model: video,
			Inputs: mediagen.Inputs{FirstFrame: dataURI, LastFrame: dataURI}})
		if res.Status != "queued" {
			t.Fatalf("结果 = %+v", res)
		}
		for _, k := range []string{"prompt", "duration", "aspect_ratio", "resolution", "generate_audio", "reference_audios", "reference_images"} {
			if _, ok := sent[k]; ok {
				t.Fatalf("不该带 %s: %v", k, sent[k])
			}
		}
	})
	t.Run("视频编辑", func(t *testing.T) {
		_, sent, call := run(t, mediagen.Request{Model: classic, Operation: store.MediaOpEdit, Prompt: "snow",
			Inputs: mediagen.Inputs{SourceVideo: mp4URI}})
		if call != "POST /v1/videos/edits" || sent["model"] != mediagen.GrokVideoEditModel || sent["prompt"] != "snow" || sent["video"].(map[string]any)["url"] != mp4URI {
			t.Fatalf("上游调用 = %q，body = %v", call, sent)
		}
		if _, ok := sent["duration"]; ok {
			t.Fatalf("编辑不该带 duration")
		}
	})
	t.Run("视频延长", func(t *testing.T) {
		_, sent, call := run(t, mediagen.Request{Model: classic, Operation: store.MediaOpExtend, Prompt: "pan",
			Inputs: mediagen.Inputs{SourceVideo: "https://example.invalid/in.mp4"}, Params: mediagen.Params{"duration": int64(6)}})
		if call != "POST /v1/videos/extensions" || sent["duration"] != float64(6) || sent["video"].(map[string]any)["url"] != "https://example.invalid/in.mp4" {
			t.Fatalf("上游调用 = %q，body = %v", call, sent)
		}
	})
	t.Run("图像生成：比例 / 分辨率 / 质量", func(t *testing.T) {
		_, sent, call := run(t, mediagen.Request{Model: image, Prompt: "blue",
			Params: mediagen.Params{"aspect_ratio": "21:9", "resolution": "2k", "quality": "medium"}})
		if call != "POST /v1/images/generations" || sent["aspect_ratio"] != "21:9" || sent["resolution"] != "2k" || sent["quality"] != "medium" {
			t.Fatalf("上游调用 = %q，body = %v", call, sent)
		}
	})
	t.Run("图像编辑：带比例与分辨率，不带质量", func(t *testing.T) {
		_, sent, call := run(t, mediagen.Request{Model: image, Prompt: "blue",
			Inputs: mediagen.Inputs{ReferenceImages: []string{dataURI}},
			Params: mediagen.Params{"aspect_ratio": "1:1", "resolution": "1k", "quality": "low"}})
		if call != "POST /v1/images/edits" || sent["aspect_ratio"] != "1:1" || sent["resolution"] != "1k" {
			t.Fatalf("上游调用 = %q，body = %v", call, sent)
		}
		if _, ok := sent["quality"]; ok {
			t.Fatalf("图像编辑不该带 quality")
		}
	})
}
