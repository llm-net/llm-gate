// mediagen.go 是媒体生成（internal/mediagen）的订阅后端与准入闸：设备自己作为调用方，以
// 任务归属的那把客户端 Key 的名义向订阅平台发起一次图像 / 视频生成。后端一律进程内调用，
// 不走本机回环 HTTP——页面这条路因此不经手密钥明文。
//
//	grok   Grok Imagine：图像同步出结果，视频由平台先受理（queued + 平台任务 ID）再查询；
//	codex  Codex 画图工具：订阅 Responses + image_generation 工具，同步出结果。
//
// 厂商面后端（ark_image / ark_video / minimax_video）在 mediagen_vendor.go。
//
// 计量：每一次上游调用记在任务归属的密钥名下，入口、模型维度与观察器都与数据面同名的门共用
// 一份（Grok 提交门 imagine_grok.go、Codex 订阅 Responses），所以媒体生成的用量与普通调用
// 出现在同一本账里；向平台查询不计模型消费。准入在内核受理时过一次（AdmitMediaJob，与
// 数据面 admit 同一份判据），后端不再重复过闸。
// §15.1：日志只记请求 id、后端、种类与 HTTP 状态；提示词、媒体与平台错误正文不进日志。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// MediaBackends 交出本进程的全部生成后端，装配期注册进 mediagen.Service。
func (s *Server) MediaBackends() []mediagen.Backend {
	return []mediagen.Backend{
		grokMediaBackend{s}, codexMediaBackend{s},
		arkImageMediaBackend{s},
		vendorVideoMediaBackend{s: s, name: mediagen.BackendArkVideo, adapter: arkVideoAdapter{}, facePrefix: "/ark/api/v3"},
		vendorVideoMediaBackend{s: s, name: mediagen.BackendMinimaxVideo, adapter: minimaxVideoAdapter{}, facePrefix: "/minimax"},
	}
}

// mediaEntry 是一个模型的那次上游调用在账本里的入口：订阅后端记 imagine_image /
// imagine_video / responses_agents，按量后端记对应厂商面的入口。
func mediaEntry(m mediagen.Model) string {
	switch m.Backend {
	case mediagen.BackendGrok:
		if m.Kind == store.ModelKindVideo {
			return usage.EntryImagineVideo
		}
		return usage.EntryImagineImage
	case mediagen.BackendCodex:
		return usage.EntryResponsesAgents
	case mediagen.BackendArkImage:
		return usage.EntryImage
	}
	return usage.EntryVideo
}

// AdmitMediaJob 实现 mediagen.Admitter：受理时过数据面同一道闸（RPM → 日 → 周 → 月预算 →
// 按量额度），限额快照与数据面鉴权后填进 reqInfo 的是同一份（按行 id 点查）。被拒的那一笔
// 同样进账本的 rejected 计数。计量未装配时一律放行。
func (s *Server) AdmitMediaJob(ctx context.Context, keyID int64, m mediagen.Model) error {
	if s.meter == nil || s.store == nil {
		return nil
	}
	ka, err := s.store.LookupKeyAuthByID(ctx, keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &mediagen.UnavailableError{Code: "not_found", Msg: "Key 不存在"}
		}
		return err
	}
	d := s.meter.Admit(*ka)
	if d.Allowed {
		return nil
	}
	info := &reqInfo{id: newRequestID(), keyID: keyID, limits: *ka}
	info.bill.keyDisplay = ka.KeyDisplay
	info.bill.entry, info.bill.model = mediaEntry(m), clipModelDim(m.ID)
	info.bill.rejected, info.bill.rejectReason = true, d.Reason
	s.recordUsage(info, http.StatusTooManyRequests, 0)
	if d.Reason == usage.RejectRPM {
		return &mediagen.RejectedError{Code: "rate_limited", Msg: "这把 API 密钥的请求过于频繁，请稍后再试", RetryAfterSec: d.RetryAfterSec}
	}
	return &mediagen.RejectedError{Code: "budget_exceeded", Msg: "这把 API 密钥的预算已用完", RetryAfterSec: d.RetryAfterSec}
}

// mediaRequest 造这次生成的合成请求：上下文里带 reqInfo，请求 id 进转发日志，归属密钥进账本
// 的密钥维度。设备后台没有真正的 HTTP 请求，这一份是转发层与计量层共同的载体。path 决定
// 网关自产错误的形状（厂商面后端走各自厂商段）。
func mediaRequest(ctx context.Context, method, path string, in mediagen.Request) *http.Request {
	info := &reqInfo{id: newRequestID(), keyID: in.KeyID}
	info.bill.keyDisplay = in.KeyDisplay
	r := httptest.NewRequest(method, "http://mediagen.local"+path, nil).
		WithContext(context.WithValue(ctx, reqInfoKey{}, info))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// beginMediaEntry 标记这次生成要入账，返回入账用的 reqInfo；任务没有归属密钥时返回 nil，
// 调用方据此不挂观察器也不借价——账本的密钥维度不收一个匹配不到任何行的 0。
func (s *Server) beginMediaEntry(r *http.Request, entry, model string) *reqInfo {
	if infoFrom(r.Context()).keyID == 0 {
		return nil
	}
	return beginEntry(r, entry, model)
}

// mediaAgentSession 取这次生成用的订阅账号与会话：accountID 非零是按 Key 的策略钉死的那一行
// （必须是这种订阅且仍可用）；为零取该订阅第一个可用账号。
func (s *Server) mediaAgentSession(ctx context.Context, provider string, accountID int64) (*store.AgentAccount, *agentSession, error) {
	if s.store == nil {
		return nil, nil, fmt.Errorf("生成服务不可用")
	}
	accounts, err := s.store.ListAgentAccounts(ctx)
	if err != nil {
		return nil, nil, err
	}
	var acct *store.AgentAccount
	for i := range accounts {
		a := &accounts[i]
		if a.Provider == provider && a.Status == store.AgentStatusActive && (accountID == 0 || a.ID == accountID) {
			acct = a
			break
		}
	}
	if acct == nil {
		return nil, nil, fmt.Errorf("订阅未连接或已失效")
	}
	_, authJSON, err := s.store.GetAgentCredential(ctx, acct.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("订阅凭据不可用")
	}
	sess, err := s.agentSessionFor(acct, authJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("订阅凭据不可用")
	}
	return acct, sess, nil
}

// ---- grok ----

type grokMediaBackend struct{ s *Server }

func (grokMediaBackend) Name() string { return mediagen.BackendGrok }

// Validate：Grok 的搭配约束能力表都表达得了（编辑 / 延长只挂在 classic 型号上、各操作的输入
// 与参数分开列），没有要补判的。
func (grokMediaBackend) Validate(mediagen.Request) error { return nil }

func (b grokMediaBackend) Generate(ctx context.Context, in mediagen.Request) (mediagen.Result, error) {
	s := b.s
	acct, sess, err := s.mediaAgentSession(ctx, store.AgentProviderGrok, in.AccountID)
	if err != nil {
		return mediagen.Result{}, err
	}
	r := mediaRequest(ctx, http.MethodPost, "", in)
	info := infoFrom(r.Context())
	rec := &bufferedResponse{header: http.Header{}}
	// 记的是**这一次上游调用**的状态：生成任务没有面向客户端的响应，平台答 200 却没给图时
	// 任务判失败，但调用是实实在在发生了的，那一笔照记。
	started := time.Now()
	defer func() { s.recordUsage(info, rec.status(), time.Since(started)) }()
	result := mediagen.Result{AccountID: acct.ID, Status: store.MediaStatusSucceeded}
	body, path := grokMediaBody(in)
	payload, _ := json.Marshal(body)
	if in.Model.Kind == store.ModelKindVideo {
		s.beginMediaEntry(r, usage.EntryImagineVideo, in.Model.ID)
		s.forwardAgent(rec, r, sess, "", http.MethodPost, s.grokImagineEndpoint()+path, payload, respRewrite{model: in.Model.ID, rewrite: keepModelField})
		if rec.status() >= 300 {
			result.Status = store.MediaStatusFailed
			result.Error = s.mediaUpstreamFailure(r, rec, in, "平台视频生成失败")
			return result, nil
		}
		var v struct {
			RequestID string `json:"request_id"`
			ID        string `json:"id"`
		}
		if json.Unmarshal(rec.body.Bytes(), &v) != nil {
			return result, fmt.Errorf("平台响应无法解析")
		}
		result.VendorID = v.RequestID
		if result.VendorID == "" {
			result.VendorID = v.ID
		}
		result.Status, result.MediaType = store.MediaStatusQueued, "video"
		if result.VendorID == "" {
			result.Status = store.MediaStatusFailed
			result.Error = "平台未返回任务 ID"
		}
		return result, nil
	}
	rw := respRewrite{model: in.Model.ID, rewrite: keepModelField}
	if entryInfo := s.beginMediaEntry(r, usage.EntryImagineImage, in.Model.ID); entryInfo != nil {
		// 画图门的名义价旋钮与张数观察器：与 imagine_grok.go 同一份口径。
		s.applyAgentModelPricingKind(r, entryInfo, in.Model.ID, store.ModelKindImage)
		rw.observe = imagineImageObserver(entryInfo)
	}
	s.forwardAgent(rec, r, sess, "", http.MethodPost, s.grokImagineEndpoint()+path, payload, rw)
	if rec.status() >= 300 {
		result.Status = store.MediaStatusFailed
		result.Error = s.mediaUpstreamFailure(r, rec, in, "平台图像生成失败")
		return result, nil
	}
	url, err := firstImageData(rec.body.Bytes())
	if err != nil {
		return result, err
	}
	result.MediaURL, result.MediaType = url, "image"
	return result, nil
}

// firstImageData 从 images 形响应（{data:[{url | b64_json}]}）取第一张图：平台地址原样，
// 内联 base64 折成 data URI。
func firstImageData(body []byte) (string, error) {
	var v struct {
		Data []struct {
			URL string `json:"url"`
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &v) != nil || len(v.Data) == 0 {
		return "", fmt.Errorf("平台未返回图像")
	}
	if v.Data[0].URL != "" {
		return v.Data[0].URL, nil
	}
	if v.Data[0].B64 != "" {
		return "data:image/png;base64," + v.Data[0].B64, nil
	}
	return "", fmt.Errorf("平台未返回图像")
}

// grokMediaBody 把一次生成翻成 Grok Imagine 官方请求体与路径（与数据面透传门同一段路径常量）。
// 没带的参数一律不带字段，由平台取缺省。
//
//	视频生成走 videos/generations：首帧进 image、尾帧进 last_frame、参考图进 reference_images
//	（各是官方示例的 {url} 对象），文生视频三者都不带；duration / aspect_ratio / resolution /
//	generate_audio 原名，音色进 reference_audios 的 {voice_id}；提示词为空时不带 prompt。
//	视频编辑走 videos/edits（model / prompt / video{url}），延长走 videos/extensions（再加
//	duration）。
//	图像没有参考图走 images/generations（aspect_ratio / resolution / quality），一张参考图走
//	images/edits 的 image，多张走 images（各是官方示例的 {type:"image_url", url} 对象；edits 带
//	aspect_ratio / resolution，不带 quality）。
//
// 媒体值（data URI / https 地址）只进请求体，不进日志（§15.1）。
func grokMediaBody(in mediagen.Request) (map[string]any, string) {
	body := map[string]any{"model": in.Model.ID}
	if in.Prompt != "" {
		body["prompt"] = in.Prompt
	}
	ref := func(v string) map[string]any { return map[string]any{"url": v} }
	set := func(k string) {
		if v := in.Params.String(k); v != "" {
			body[k] = v
		}
	}
	duration := func() {
		if n, ok := in.Params.Int("duration"); ok {
			body["duration"] = n
		}
	}
	if in.Model.Kind == store.ModelKindVideo {
		switch in.Operation {
		case store.MediaOpEdit:
			body["video"] = ref(in.Inputs.SourceVideo)
			return body, grokVideosEditsPath
		case store.MediaOpExtend:
			body["video"] = ref(in.Inputs.SourceVideo)
			duration()
			return body, grokVideosExtensionsPath
		}
		if in.Inputs.FirstFrame != "" {
			body["image"] = ref(in.Inputs.FirstFrame)
		}
		if in.Inputs.LastFrame != "" {
			body["last_frame"] = ref(in.Inputs.LastFrame)
		}
		if refs := in.Inputs.ReferenceImages; len(refs) > 0 {
			out := make([]any, 0, len(refs))
			for _, v := range refs {
				out = append(out, ref(v))
			}
			body["reference_images"] = out
		}
		if voices := in.Params.Strings("voices"); len(voices) > 0 {
			audios := make([]any, 0, len(voices))
			for _, v := range voices {
				audios = append(audios, map[string]any{"voice_id": v})
			}
			body["reference_audios"] = audios
		}
		duration()
		set("aspect_ratio")
		set("resolution")
		if b := in.Params.Bool("generate_audio"); b != nil {
			body["generate_audio"] = *b
		}
		return body, grokVideosGenerationsPath
	}
	set("aspect_ratio")
	set("resolution")
	src := func(v string) map[string]any { return map[string]any{"type": "image_url", "url": v} }
	switch refs := in.Inputs.ReferenceImages; len(refs) {
	case 0:
		set("quality")
		return body, grokImagesGenerationsPath
	case 1:
		body["image"] = src(refs[0])
	default:
		srcs := make([]any, 0, len(refs))
		for _, v := range refs {
			srcs = append(srcs, src(v))
		}
		body["images"] = srcs
	}
	return body, grokImagesEditsPath
}

// Refresh 查询一条平台排队中的 Grok 视频任务。
//
// 平台答非 2xx 时按状态码分三路：404 / 410 是任务已不存在（expired）；401 / 403 / 408 / 409 /
// 429 / 5xx 是查询这一侧的故障（凭据、限流、设备自产的 409 与 502），返回 error 让后台下一轮
// 再查；其余 4xx 是平台对这条任务的终审（xAI 用 400 + {"code":"imagine:content-moderated",
// "error":…} 报告生成结果被内容审核拒绝），任务判 failed 并透出平台文案与 code。
func (b grokMediaBackend) Refresh(ctx context.Context, t store.MediaJob) (mediagen.Result, error) {
	s := b.s
	if t.Kind != store.ModelKindVideo || t.VendorID == "" {
		return mediagen.Result{}, fmt.Errorf("该任务不支持刷新")
	}
	acct, sess, err := s.mediaAgentSession(ctx, store.AgentProviderGrok, t.AccountID)
	if err != nil {
		return mediagen.Result{}, err
	}
	r := httptest.NewRequest(http.MethodGet, "http://mediagen.local", nil).WithContext(ctx)
	rec := &bufferedResponse{header: http.Header{}}
	s.forwardAgent(rec, r, sess, "", http.MethodGet, s.grokImagineEndpoint()+grokVideosPath+url.PathEscape(t.VendorID), nil, respRewrite{rewrite: keepModelField})
	base := mediagen.Result{AccountID: acct.ID, VendorID: t.VendorID, MediaType: "video"}
	if code := rec.status(); code >= 300 {
		// 只记状态码，不记正文（§15.1）。
		s.log.Warn("查询平台生成任务被拒", "job", t.ID, "backend", t.Backend, "kind", t.Kind, "model", t.Model, "status", code)
		switch code {
		case http.StatusNotFound, http.StatusGone:
			// 平台已不认这个任务 ID：过期。
			base.Status = store.MediaStatusExpired
			base.Error = fmt.Sprintf("平台任务已过期或不可访问（HTTP %d）", code)
			return base, nil
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
			// 凭据、限流与设备自产的 409 / 502 都是查询这一侧的故障，不是任务的结局：报错让
			// 内核下一轮再查，直到 TaskTimeout 兜底。
			return mediagen.Result{}, fmt.Errorf("平台查询暂不可用（HTTP %d）", code)
		default:
			if code >= 500 {
				return mediagen.Result{}, fmt.Errorf("平台查询暂不可用（HTTP %d）", code)
			}
			// 其余 4xx 是平台对这条任务的裁决（如 400 imagine:content-moderated「生成的视频被
			// 内容审核拒绝」）：任务失败，原因透出平台文案与 code。
			base.Status = store.MediaStatusFailed
			base.Error = mediaUpstreamError(rec, "平台视频生成失败")
			return base, nil
		}
	}
	var v struct {
		Status string `json:"status"`
		Error  struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Video struct {
			URL string `json:"url"`
		} `json:"video"`
		Content struct {
			URL string `json:"url"`
		} `json:"content"`
		URL string `json:"url"`
	}
	if json.Unmarshal(rec.body.Bytes(), &v) != nil {
		return mediagen.Result{}, fmt.Errorf("平台响应无法解析")
	}
	media := v.URL
	if media == "" {
		media = v.Video.URL
	}
	if media == "" {
		media = v.Content.URL
	}
	// 平台状态词只折成三种：有结果即 succeeded；明确失败词为 failed；其余（pending /
	// processing 等中间状态、空串）都回 queued，由内核继续查。
	res := base
	res.Status = store.MediaStatusQueued
	switch status := strings.ToLower(strings.TrimSpace(v.Status)); {
	case media != "":
		res.Status = store.MediaStatusSucceeded
		res.MediaURL = media
	case status == "failed" || status == "error" || status == "errored" || status == "cancelled" || status == "canceled" || status == "rejected":
		res.Status = store.MediaStatusFailed
		res.Error = formatMediaPlatformError(v.Error.Code, v.Error.Message, "平台视频生成失败")
	}
	return res, nil
}

// ---- codex ----

type codexMediaBackend struct{ s *Server }

func (codexMediaBackend) Name() string { return mediagen.BackendCodex }

// Validate：透明背景只配 PNG（WebP 虽回报 transparent 实际无 alpha，JPEG 没有 alpha）。
func (codexMediaBackend) Validate(in mediagen.Request) error {
	if in.Params.String("background") == "transparent" {
		if f := in.Params.String("output_format"); f != "" && f != "png" {
			return &mediagen.InvalidError{Msg: "透明背景只支持 PNG 格式"}
		}
	}
	return nil
}

// Generate 走生产画图门同一段翻译与流解析，行为与 gate 的 image_gen 一致。承载的恒是订阅
// 文本模型；页面显示的预设名（gpt-image-2）是任务元数据，绝不当 Responses 的 model 发出去
// ——ChatGPT 后端对它答 "model is not supported"。
func (b codexMediaBackend) Generate(ctx context.Context, in mediagen.Request) (mediagen.Result, error) {
	s := b.s
	acct, sess, err := s.mediaAgentSession(ctx, store.AgentProviderCodex, in.AccountID)
	if err != nil {
		return mediagen.Result{}, err
	}
	r := mediaRequest(ctx, http.MethodPost, "", in)
	info := infoFrom(r.Context())
	rec := &bufferedResponse{header: http.Header{}}
	started := time.Now()
	defer func() { s.recordUsage(info, rec.status(), time.Since(started)) }()
	result := mediagen.Result{AccountID: acct.ID, Status: store.MediaStatusSucceeded}
	carrier := s.codexMediaCarrier(ctx, acct)
	if carrier == "" {
		result.Status = store.MediaStatusFailed
		result.Error = "Codex 订阅没有可用的文本模型，无法承载图像生成"
		return result, nil
	}
	// 参考图、比例、透明背景与输出格式走生产画图门同一段翻译（codexImagePayload）：参考图各成
	// 一条 input_image 跟在提示词后，比例与透明背景折成提示词尾部的指令行，output_format 进
	// 工具对象。订阅后端对 size / quality 静默忽略、background=transparent 参数答 400
	// （docs-dev/gpt-image-25-model-selection.md），能力表不收它们。
	payload := codexMediaPayload(in, carrier)
	rw := respRewrite{model: in.Model.ID, expectSSE: true, rewrite: rewriteResponsesModel}
	// 计量与订阅 Responses 门同一份口径：入口 responses_agents、模型维度是承载的订阅文本模型
	// （图像预设名只是任务元数据），token 由同一个观察器取真值。
	if entryInfo := s.beginMediaEntry(r, usage.EntryResponsesAgents, carrier); entryInfo != nil {
		entryInfo.bill.estimateInput = func() int64 { return usage.EstimateResponsesInput(payload) }
		s.applyAgentModelPricing(r, entryInfo, carrier)
		rw.observe = s.responsesObserver(entryInfo)
	}
	raw, _ := json.Marshal(payload)
	s.forwardResponses(rec, r, sess, acct.AccountID, raw, rw)
	if rec.status() >= 300 {
		result.Status = store.MediaStatusFailed
		result.Error = s.mediaUpstreamFailure(r, rec, in, "平台图像生成失败")
		return result, nil
	}
	item, failure := parseImageGenerationStream(rec.body.Bytes())
	if item == nil {
		result.Status = store.MediaStatusFailed
		result.Error = failure
		if result.Error == "" {
			result.Error = "平台未返回图像"
		}
		return result, nil
	}
	// 结果的 MIME 以后端回报的 output_format 为准（请求的格式经真机验证会被采用；后端没回报
	// 时按请求的格式，都没有按 PNG），落盘扩展名与页面解码都靠它。
	format := jsonString(item["output_format"])
	if format == "" {
		format = in.Params.String("output_format")
	}
	result.MediaURL = "data:" + codexImageMIME(format) + ";base64," + jsonString(item["result"])
	result.MediaType = "image"
	return result, nil
}

func (codexMediaBackend) Refresh(context.Context, store.MediaJob) (mediagen.Result, error) {
	return mediagen.Result{}, fmt.Errorf("该任务不支持刷新")
}

// codexMediaPayload 把一次生成翻成一条带画图工具的订阅 Responses 调用：提示词、参考图
// （input_image）、比例 / 透明背景指令与 output_format 经生产画图门的 codexImagePayload；
// 工具 model 只在 codexMediaToolModels 白名单内原样写入，其余预设不带 model。
func codexMediaPayload(in mediagen.Request, carrier string) map[string]any {
	req := map[string]any{"prompt": codexPromptWithHints(in.Prompt, in.Params.String("aspect_ratio"), in.Params.String("background") == "transparent")}
	if f := in.Params.String("output_format"); f != "" {
		req["output_format"] = f
	}
	refs := in.Inputs.ReferenceImages
	if len(refs) > 0 {
		images := make([]any, 0, len(refs))
		for _, ref := range refs {
			images = append(images, map[string]any{"image_url": ref})
		}
		req["images"] = images
	}
	payload := codexImagePayload(req, carrier, req["prompt"].(string), len(refs) > 0)
	if tools, ok := payload["tools"].([]any); ok && len(tools) == 1 {
		if tool, ok := tools[0].(map[string]any); ok {
			delete(tool, "model")
			if codexMediaToolModels[in.Model.ID] {
				tool["model"] = in.Model.ID
			}
		}
	}
	return payload
}

// codexImageMIME 把画图工具的 output_format 翻成 MIME；认不出按 PNG。
func codexImageMIME(format string) string {
	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	}
	return "image/png"
}

// codexMediaToolModels 是媒体生成允许原样写进画图工具 model 的 GPT Image 2.5
// 型号；其余预设（gpt-image-2）不带工具 model，由订阅后端自选。
var codexMediaToolModels = map[string]bool{
	"gpt-image-2.5-flare":    true,
	"gpt-image-2.5-sunburst": true,
}

// codexMediaCarrier 选承载媒体生成画图工具调用的订阅文本模型，与生产画图门
// codexImageCarrier 同口径：账号默认模型若是 Codex 订阅模型就用它，否则按
// 平台目录顺序取第一个订阅模型，目录之外的订阅模型按名字典序补在后面。
// 订阅模型读数不可用（目录读不到）时退回账号默认模型；仍没有则返回空串，
// 由调用方判失败——绝不拿图像预设名当 Responses 的 model 发出去。
func (s *Server) codexMediaCarrier(ctx context.Context, acct *store.AgentAccount) string {
	owners, err := s.AgentSubscriptionModels(ctx)
	if err != nil || len(owners) == 0 {
		return acct.DefaultModel
	}
	if owners[acct.DefaultModel] == store.AgentProviderCodex {
		return acct.DefaultModel
	}
	if agent, ok := s.effectivePlatformModels(ctx).Agent(store.AgentProviderCodex); ok {
		for _, m := range agent.Models {
			if m.Kind == store.ModelKindText && owners[m.Name] == store.AgentProviderCodex {
				return m.Name
			}
		}
	}
	var rest []string
	for name, owner := range owners {
		if owner == store.AgentProviderCodex {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	if len(rest) > 0 {
		return rest[0]
	}
	return ""
}

func formatMediaPlatformError(code, message, fallback string) string {
	message = strings.TrimSpace(message)
	message = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, message)
	if len([]rune(message)) > 240 {
		message = string([]rune(message)[:240]) + "…"
	}
	if message == "" {
		return fallback
	}
	if code = strings.TrimSpace(code); code != "" {
		return message + "（" + code + "）"
	}
	return message
}

// mediaUpstreamError 把上游拒绝翻成任务的失败原因。认三种错误体：OpenAI 形
// {"error":{"message","code"}}、Grok 视频形 {"message","code"} 与 xAI 通用形
// {"code","error":"<文案>"}（Imagine 的 invalid_image 等就是这一种）；不是 JSON 的
// 短文本体（xAI 对参数枚举越界答 422 纯文本）按原文取。都取不到时用缺省文案并带上
// HTTP 状态，管理员至少知道是平台拒绝而不是设备出错。文案只进任务行，不进日志（§15.1）。
func mediaUpstreamError(rec *bufferedResponse, fallback string) string {
	body := rec.body.Bytes()
	fallback = fmt.Sprintf("%s（HTTP %d）", fallback, rec.status())
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		var message, code string
		if len(envelope.Error) > 0 {
			var nested struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			}
			if json.Unmarshal(envelope.Error, &nested) == nil {
				message, code = strings.TrimSpace(nested.Message), strings.TrimSpace(nested.Code)
			} else if json.Unmarshal(envelope.Error, &message) == nil {
				message, code = strings.TrimSpace(message), strings.TrimSpace(envelope.Code)
			}
		}
		if message == "" {
			message = strings.TrimSpace(envelope.Message)
			code = strings.TrimSpace(envelope.Code)
		}
		if message == "" {
			message = strings.TrimSpace(envelope.Detail)
		}
		if message != "" {
			return formatMediaPlatformError(code, message, fallback)
		}
		return fallback
	}
	if text := strings.TrimSpace(string(body)); text != "" && len(text) <= 2048 && !strings.HasPrefix(text, "<") {
		return formatMediaPlatformError("", text, fallback)
	}
	return fallback
}

// mediaUpstreamFailure 是 后端各处「上游答非 2xx」的公共落点：记一条只含
// 请求 id、后端、种类与 HTTP 状态的告警（不含错误正文），再翻成任务失败原因。
func (s *Server) mediaUpstreamFailure(r *http.Request, rec *bufferedResponse, in mediagen.Request, fallback string) string {
	s.log.Warn("生成任务上游调用被拒", "request_id", infoFrom(r.Context()).id,
		"backend", in.Model.Backend, "kind", in.Model.Kind, "model", in.Model.ID, "status", rec.status())
	return mediaUpstreamError(rec, fallback)
}
