// entry.go 是数据面入口共同的请求体解码（Phase 3 外审遗留：chat/messages/
// video 三处各抄一份「读体 + UseNumber 解析 + model 必填校验」，Phase 5 的
// 图像入口是第四处——抽到这里归一）。
//
// 解码直接从 r.Body 流式进行，不先 ReadAll 攒原始切片：文本入口的体是 KB
// 级、无所谓；视频/图像入口的体可到数十 MB（base64 素材内嵌），同时持有原始
// 字节与解码后的 map 会把峰值内存翻倍。错误契约与原三处逐字节一致：读失败
// 400 invalid_request_body（"Failed to read…"）、非法 JSON 400
// invalid_request_body（"…not valid JSON."）、缺 model 400 missing_model；
// 超过 http.MaxBytesReader 上限时 413 request_too_large。错误形态由
// errStyle 决定（chat/video 是 OpenAI 风格带 code，messages 是 Anthropic
// 风格只有 type+message——code 在该风格下按既有规则不外显）。
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// videoSubmitBodyLimit 是视频提交体的字节上限（Phase 3 外审遗留：数据面
// 此前全无请求体上限，视频体又能合法地大到几十 MB，压力最大）。取值对齐
// 厂商契约加余量：minimax 明列「整个 JSON 请求体 ≤64 MB」，方舟未列总量
// 上限但素材同为 base64 内嵌——80 MiB 让合法请求恒碰不到，拦的是无界读内存。
// 视频/图像入口使用此上限（video.go 与 images.go 各挂一次）；Responses 文本入口使用
// 下方更小的专用上限。调整时同步 video_test.go 的 TestVideoSubmitBodyLimit 与
// images_test.go 的 TestImagesBodyLimit。
const videoSubmitBodyLimit = 80 << 20

// agentsResponsesBodyLimit 是 Responses 文本入口转发给上游的 JSON 体上限。Responses
// 可以承载图片，但把视频或图片 base64 放进会话历史会在解码、重编码和重试时产生
// 多份副本；8 MiB 足够正常长文本请求。标准 /v1/responses 直接按它读体；Agent 面
// 按下方读入上限读体，再收进这个上限（readAgentResponses）。
const agentsResponsesBodyLimit = 8 << 20

// agentsResponsesReadLimit 是 Agent 面 Responses 入口（/agents/codex、/agents/grok 与
// 兼容别名 /agents/v1）读入的原始体上限。Codex / Grok 每次请求都重发整段会话历史，
// 工具返回的图片以 base64 留在历史里，长任务一轮看几十张图就会超过转发上限；读入放宽到
// 这里，超出转发上限的部分由较早的工具图片让出。取值在视频入口的 80 MiB 之内。
const agentsResponsesReadLimit = 32 << 20

// decodeEntryPayload 读取并解析一个数据面入口的 JSON 请求体：通用 map 保留
// 未知字段（透传语义），UseNumber 保数字原字面量，取出必填的 model。任何
// 失败都已按 errStyle 写出错误响应并返回 ok=false。
func decodeEntryPayload(w http.ResponseWriter, r *http.Request, errStyle errorStyle) (map[string]any, string, bool) {
	payload, model, _, ok := decodeEntryPayloadSized(w, r, errStyle)
	return payload, model, ok
}

// decodeEntryPayloadSized 同 decodeEntryPayload，另回这个 JSON 值在请求体里占的字节数。
func decodeEntryPayloadSized(w http.ResponseWriter, r *http.Request, errStyle errorStyle) (map[string]any, string, int64, bool) {
	payload, size, ok := decodeEntryObjectSized(w, r, errStyle)
	if !ok {
		return nil, "", 0, false
	}
	model, _ := payload["model"].(string)
	if model == "" {
		errStyle(w, http.StatusBadRequest, "missing_model",
			"The model field is required and must be a string.")
		return nil, "", 0, false
	}
	return payload, model, size, true
}

// decodeEntryObject 是 decodeEntryPayload 去掉「必填 model」那一步的形态：
// 只把请求体解析成通用 map（UseNumber 保数字原字面量）。给 model 不是必填
// 字段的入口用（Codex 画图门 images_codex.go 的 model 是图像模型名，可缺省）。
// 任何失败都已按 errStyle 写出错误响应并返回 ok=false。
func decodeEntryObject(w http.ResponseWriter, r *http.Request, errStyle errorStyle) (map[string]any, bool) {
	payload, _, ok := decodeEntryObjectSized(w, r, errStyle)
	return payload, ok
}

// decodeEntryObjectSized 同 decodeEntryObject，另回这个 JSON 值在请求体里占的字节数
// （json.Decoder.InputOffset，不含尾随空白）。
func decodeEntryObjectSized(w http.ResponseWriter, r *http.Request, errStyle errorStyle) (map[string]any, int64, bool) {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		var maxErr *http.MaxBytesError
		var synErr *json.SyntaxError
		var typErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &maxErr):
			writeBodyTooLarge(w, errStyle, maxErr.Limit)
		case errors.As(err, &synErr), errors.As(err, &typErr),
			errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			// 语法/形态错误与截断（含空体）：体到了但不是合法 JSON 对象。
			errStyle(w, http.StatusBadRequest, "invalid_request_body",
				"The request body is not valid JSON.")
		default:
			// 传输层读失败通常是客户端中途断开；响应多半也写不出去，尽力而为。
			errStyle(w, http.StatusBadRequest, "invalid_request_body",
				"Failed to read the request body.")
		}
		return nil, 0, false
	}
	return payload, dec.InputOffset(), true
}

// writeBodyTooLarge 写出请求体超限的 413；读体超限与收不进转发上限共用这一句。
func writeBodyTooLarge(w http.ResponseWriter, errStyle errorStyle, limit int64) {
	errStyle(w, http.StatusRequestEntityTooLarge, "request_too_large",
		fmt.Sprintf("The request body must not exceed %d MB for this endpoint.", limit>>20))
}
