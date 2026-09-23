// images_codex.go 是 Codex 接入面的画图门：
//
//	POST /agents/codex/v1/images/generations
//	POST /agents/codex/v1/images/edits
//
// Codex 客户端内置的 image_gen 工具打的是 {base_url}/images/generations 这扇
// 门；设备这一侧没有单独的画图后端，能画图的是 Codex 订阅后端的
// image_generation 工具。所以这里做一次翻译：把 images API 形态的请求改写成
// 一条带 image_generation 工具、tool_choice 钉死的 Responses 调用，交给同一条
// Codex 订阅代理（handleAgentResponsesFor），再把流里那一个
// image_generation_call 条目翻回 images API 的 JSON 形态。承载模型恒是这把
// Key 可见的订阅模型，绝不是目录模型（目录面没有这个工具）。
//
// 请求形态以 Codex 内置工具实际发出的 wire 为准（JSON 体，非 multipart）：
//
//	prompt        必填
//	model         可选；只有 gpt-image* 系列才作为工具的 model 转发
//	n/size/quality/background/output_format/output_compression
//	              可选，逐字放进工具对象。真机验证（docs-dev/gpt-image-25-model-selection.md）：
//	              订阅后端只可靠地采用 output_format（与 output_compression），size / quality
//	              静默忽略、transparent 背景答 400；这里照样逐字转发，由后端裁决。
//	              后端认提示词里的比例与透明背景指令：size 是 WxH 时另把化简后的长宽比
//	              折成提示词尾部的 "Aspect ratio: W:H."，background=transparent 不进工具
//	              对象（后端答 400）、改折成透明背景指令（codexPromptWithHints），像素
//	              总量仍由后端定
//	images[].image_url          仅 edits：作为 input_image 跟在文本之后
//
// 计量搭订阅面同一班车：handleAgentResponsesFor 已 beginEntry 到
// usage.EntryResponsesAgents，这里不再另起账。§15.1：提示词、图像 URL/字节
// 与 base64 结果一概不进日志，只记长度与条目数。
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// codexImagesBodyLimit 与方舟出图入口同一上限：edits 的参考图会以 data URI
// 内嵌，体积与视频提交同量级。
const codexImagesBodyLimit = videoSubmitBodyLimit

// codexImagePromptPrefix 是交给承载模型的指令：让它只做一件事——按提示词
// 调一次画图工具。tool_choice 已钉死工具，这句话只是让文本侧不另起话头。
const codexImagePromptPrefix = "Call the image generation tool to create exactly this image: "

func (s *Server) handleCodexImagesGenerations(w http.ResponseWriter, r *http.Request) {
	s.handleCodexImages(w, r, false)
}

func (s *Server) handleCodexImagesEdits(w http.ResponseWriter, r *http.Request) {
	s.handleCodexImages(w, r, true)
}

func (s *Server) handleCodexImages(w http.ResponseWriter, r *http.Request, edit bool) {
	if !rejectCodexCatalogQuery(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, codexImagesBodyLimit)
	req, ok := decodeEntryObject(w, r, openAIErrorStyle)
	if !ok {
		return
	}
	prompt, _ := req["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		openAIErrorStyle(w, http.StatusBadRequest, "invalid_request_body",
			"The prompt field is required and must be a non-empty string.")
		return
	}
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	if !snapshot.Subscription("codex").Configured {
		openAIErrorStyle(w, http.StatusForbidden, "subscription_not_allowed",
			"This API key is not allowed to use the Codex subscription.")
		return
	}
	carrier := codexImageCarrier(snapshot)
	if carrier == "" {
		// 订阅勾了但此刻没有任何可见的订阅模型：账号没连上、登录失效或被
		// 停用——都是设备状态，与 withSubscription 要求可用而不可用同一口径。
		writeAgentNotConfigured(w, store.AgentProviderCodex)
		return
	}
	transparent := false
	if bg, _ := req["background"].(string); strings.EqualFold(strings.TrimSpace(bg), "transparent") {
		transparent = true
		delete(req, "background")
	}
	payload := codexImagePayload(req, carrier, codexPromptWithHints(prompt, codexSizeAspect(req["size"]), transparent), edit)

	// 把订阅代理的整个应答收进内存：成功流里只需要那一个 image_generation_call
	// 条目；非 2xx 按原样转给客户端（错误体不改写）。
	rec := &bufferedResponse{header: http.Header{}}
	s.handleAgentResponsesFor(rec, r, store.AgentProviderCodex, payload, carrier)
	if r.Context().Err() != nil {
		return // 客户端已走，响应无处可写
	}
	if rec.status() != http.StatusOK {
		if ct := rec.header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(rec.status())
		_, _ = w.Write(rec.body.Bytes())
		return
	}
	item, failure := parseImageGenerationStream(rec.body.Bytes())
	if item == nil {
		info := infoFrom(r.Context())
		s.log.Warn("Codex 画图：订阅后端应答里没有图像条目",
			"request_id", info.id, "model", carrier, "bytes", rec.body.Len(), "failed", failure != "")
		msg := "The Codex subscription backend produced no image."
		if failure != "" {
			msg = failure
		}
		openAIErrorStyle(w, http.StatusBadGateway, "image_generation_failed", msg)
		return
	}
	out := map[string]any{
		"created": time.Now().Unix(),
		"data":    []any{map[string]any{"b64_json": item["result"]}},
	}
	for _, k := range []string{"background", "quality", "size", "output_format"} {
		if v, ok := item[k]; ok && v != nil {
			out[k] = v
		}
	}
	body, err := json.Marshal(out)
	if err != nil { // 只含字符串与整数，理论不可达
		openAIErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// codexImageCarrier 选承载这次工具调用的订阅模型：这把 Key 在 Codex 面的默认
// 模型若是订阅模型就用它，否则取可见清单里第一个订阅模型；一个都没有返回空串。
func codexImageCarrier(snapshot devtoolpolicy.Snapshot) string {
	tool := snapshot.Tools["codex"]
	var first string
	for _, m := range tool.Models {
		if m.Source != "subscription" {
			continue
		}
		if m.Name == tool.DefaultModel {
			return m.Name
		}
		if first == "" {
			first = m.Name
		}
	}
	return first
}

// codexImagePayload 把 images API 形态的请求翻成一条 Responses 调用。
func codexImagePayload(req map[string]any, carrier, prompt string, edit bool) map[string]any {
	tool := map[string]any{"type": "image_generation"}
	for _, k := range []string{"background", "n", "quality", "size", "output_format", "output_compression"} {
		if v, ok := req[k]; ok && v != nil {
			tool[k] = v
		}
	}
	if m, _ := req["model"].(string); strings.HasPrefix(m, "gpt-image") {
		tool["model"] = m
	}
	content := []any{map[string]any{"type": "input_text", "text": codexImagePromptPrefix + prompt}}
	if edit {
		for _, img := range jsonArray(req["images"]) {
			o, ok := img.(map[string]any)
			if !ok {
				continue
			}
			if u, _ := o["image_url"].(string); u != "" {
				content = append(content, map[string]any{"type": "input_image", "image_url": u})
			}
		}
	}
	return map[string]any{
		"model":       carrier,
		"store":       false,
		"stream":      true,
		"tool_choice": map[string]any{"type": "image_generation"},
		"tools":       []any{tool},
		"input": []any{map[string]any{
			"type": "message", "role": "user", "content": content,
		}},
	}
}

// codexTransparentHint 是透明背景的提示词指令：订阅后端不认 background=transparent 参数
// （答 400），却按这句话出带 alpha 通道的 PNG，并在条目里回报 background=transparent。
const codexTransparentHint = "Transparent background: the background must be fully transparent (alpha channel), not white."

// codexPromptWithHints 在提示词尾部追加比例指令（"Aspect ratio: 16:9."）与透明背景指令。
// 订阅后端的画图工具不认 size，却按提示词里的比例出图（真机验证 9 种比例全部生效，
// 像素总量恒约 157 万）；两项都为空原样返回。
func codexPromptWithHints(prompt, ratio string, transparent bool) string {
	if ratio == "" && !transparent {
		return prompt
	}
	prompt = strings.TrimRight(strings.TrimSpace(prompt), ".。") + "."
	if ratio != "" {
		prompt += " Aspect ratio: " + ratio + "."
	}
	if transparent {
		prompt += " " + codexTransparentHint
	}
	return prompt
}

// codexSizeAspect 把 images API 的 size（"1536x1024"）化简成比例（"3:2"）；auto、非
// WxH 形状、任一边为 0 或正方形（后端缺省即 1:1）返回空串（不追加指令）。
func codexSizeAspect(v any) string {
	size, _ := v.(string)
	w, h, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !ok {
		return ""
	}
	wi, err1 := strconv.Atoi(w)
	hi, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil || wi <= 0 || hi <= 0 {
		return ""
	}
	if wi == hi {
		return ""
	}
	g := gcd(wi, hi)
	return strconv.Itoa(wi/g) + ":" + strconv.Itoa(hi/g)
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// parseImageGenerationStream 在订阅代理回放的 SSE 里找最后一个带 result 的
// image_generation_call 条目；同时记下 response.failed / error 事件的可读原因
// （只取 message，不带事件原文）。
func parseImageGenerationStream(body []byte) (item map[string]any, failure string) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		rest, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		rest = bytes.TrimSpace(rest)
		if len(rest) == 0 || bytes.Equal(rest, []byte("[DONE]")) {
			continue
		}
		var ev map[string]any
		if json.Unmarshal(rest, &ev) != nil {
			continue
		}
		switch jsonString(ev["type"]) {
		case "response.output_item.done":
			it, ok := ev["item"].(map[string]any)
			if !ok || jsonString(it["type"]) != "image_generation_call" {
				continue
			}
			if jsonString(it["result"]) != "" {
				item = it
			}
		case "response.failed":
			if rsp, ok := ev["response"].(map[string]any); ok {
				if e, ok := rsp["error"].(map[string]any); ok {
					failure = jsonString(e["message"])
				}
			}
		case "error":
			failure = jsonString(ev["message"])
		}
	}
	return item, failure
}

// bufferedResponse 是一个把整份应答收进内存的 http.ResponseWriter：画图门用它
// 接住订阅代理的输出再翻译。实现 Flush 只为让 SSE 转发那段的即时刷出调用有处
// 可去；本身不需要流式语义。
type bufferedResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	return b.body.Write(p)
}

func (b *bufferedResponse) Flush() {}

func (b *bufferedResponse) status() int {
	if b.code == 0 {
		return http.StatusOK
	}
	return b.code
}
